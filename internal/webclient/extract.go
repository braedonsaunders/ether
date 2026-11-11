package webclient

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// debs are ar(5) archives whose member names may end in a single '/' when
// GNU ar wrote a long-name table. The members Ether needs are read in order.
const arMagic = "!<arch>\n"

// Extract unpacks the Jellyfin Web files from a .deb read at the start of r
// into dstDir. It reads only the "data.tar.xz" member and refuses anything
// outside the web prefix. dstDir must already exist.
func Extract(r io.ReaderAt, dstDir string) error {
	data, err := findArMember(r, "data.tar.xz")
	if err != nil {
		return err
	}
	return extractData(data, dstDir)
}

// findArMember returns the reader for the named ar member. r must be seekable;
// the .deb layout lets members be addressed by offset.
func findArMember(r io.ReaderAt, name string) (*io.SectionReader, error) {
	header := make([]byte, len(arMagic))
	if _, err := r.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("ar: read magic: %w", err)
	}
	if string(header) != arMagic {
		return nil, errors.New("ar: bad magic")
	}
	off := int64(len(arMagic))
	var longNames []byte
	for {
		hdr := make([]byte, 60)
		if _, err := r.ReadAt(hdr, off); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("ar: member %q not found", name)
			}
			return nil, fmt.Errorf("ar: read header: %w", err)
		}
		if string(hdr[58:60]) != "`\n" {
			return nil, errors.New("ar: bad member header")
		}
		rawName := strings.TrimRight(string(hdr[:16]), " ")
		size, err := parseArSize(hdr[48:58])
		if err != nil {
			return nil, err
		}
		// 2-byte alignment padding after odd-sized members.
		dataOff := off + 60
		member := strings.TrimSuffix(rawName, "/")
		next := dataOff + size
		if size%2 == 1 {
			next++
		}
		switch {
		case rawName == "//":
			longNames = make([]byte, size)
			if _, err := r.ReadAt(longNames, dataOff); err != nil {
				return nil, fmt.Errorf("ar: read name table: %w", err)
			}
		case strings.HasPrefix(rawName, "/") && len(rawName) > 1 && isDigits(rawName[1:]):
			n, _ := parseDecimal(rawName[1:])
			idx := int(n)
			if idx >= 0 && idx < len(longNames) {
				end := bytes.IndexByte(longNames[idx:], '\n')
				if end >= 0 {
					member = strings.TrimRight(string(longNames[idx:idx+end]), "/")
				}
			}
		}
		if member == name {
			return io.NewSectionReader(r, dataOff, size), nil
		}
		off = next
	}
}

func parseArSize(b []byte) (int64, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	return parseDecimal(s)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseDecimal(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("ar: bad number %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// extractData reads the xz-compressed tar and writes regular files under the
// Jellyfin Web prefix, plus the package copyright notice. Absolute paths,
// parent traversal and links are refused (zip-slip safe). The total size and
// file count are capped.
func extractData(data io.Reader, dstDir string) error {
	xzr, err := xz.NewReader(data)
	if err != nil {
		return fmt.Errorf("xz: %w", err)
	}
	tr := tar.NewReader(xzr)
	var total int64
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		rel, ok := memberPath(hdr.Name)
		if !ok {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(dstDir, filepath.FromSlash(rel)), 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			files++
			if total > maxExtractedBytes {
				return fmt.Errorf("web client exceeds %d bytes", maxExtractedBytes)
			}
			if files > maxExtractedFiles {
				return fmt.Errorf("web client exceeds %d files", maxExtractedFiles)
			}
			if err := writeFile(dstDir, rel, tr, hdr.Size); err != nil {
				return err
			}
		default:
			// Links, devices and fifos are not part of the web build.
			return fmt.Errorf("tar: refusing %q entry %q", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

// memberPath maps a tar member to its path under dstDir, or ok=false when the
// member is outside the web prefix or is unsafe.
func memberPath(name string) (string, bool) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "./"))
	if clean == "/usr/share/doc/jellyfin-web/copyright" {
		return "COPYRIGHT", true
	}
	prefix := "/" + strings.TrimPrefix(webPrefix, "./")
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(clean, prefix)
	if rel == "" || rel == "." {
		return "", false
	}
	// path.Clean already removed any "..", but re-check for defence.
	if strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", false
	}
	return rel, true
}

func writeFile(dstDir, rel string, r io.Reader, size int64) error {
	dest := filepath.Join(dstDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(r, size)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
