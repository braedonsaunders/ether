package webclient

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// buildDeb assembles a minimal ar archive from named members, padding
// odd-sized bodies the way GNU ar does.
func buildDeb(t *testing.T, members []struct {
	name string
	body []byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	for _, m := range members {
		name := fmt.Sprintf("%-16s", m.name)
		hdr := fmt.Sprintf("%s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(m.body))
		if len(hdr) != 60 {
			t.Fatalf("header length %d", len(hdr))
		}
		buf.WriteString(hdr)
		buf.Write(m.body)
		if len(m.body)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// buildTarXZ packs the given files into an xz-compressed tar. Entries whose
// name ends in "/" become directories.
func buildTarXZ(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if len(name) > 0 && name[len(name)-1] == '/' {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	xw, err := xz.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func syntheticDeb(t *testing.T, web, copyright []byte) []byte {
	t.Helper()
	data := buildTarXZ(t, map[string][]byte{
		"./usr/":                                 nil,
		"./usr/share/":                           nil,
		"./usr/share/jellyfin/":                  nil,
		"./usr/share/jellyfin/web/":              nil,
		"./usr/share/jellyfin/web/index.html":    web,
		"./usr/share/jellyfin/web/main.js":       []byte("console.log('hi')"),
		"./usr/share/jellyfin/web/../evil.js":    web, // must never land outside the prefix
		"./usr/share/doc/jellyfin-web/copyright": copyright,
	})
	return buildDeb(t, []struct {
		name string
		body []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.xz", []byte("ignored")},
		{"data.tar.xz", data},
	})
}

func TestExtractSyntheticDeb(t *testing.T) {
	deb := syntheticDeb(t, []byte("<html>jf</html>"), []byte("GPL-2.0"))
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(deb), dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "index.html")); err != nil || string(got) != "<html>jf</html>" {
		t.Fatalf("index.html: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "COPYRIGHT")); err != nil || string(got) != "GPL-2.0" {
		t.Fatalf("COPYRIGHT: %q %v", got, err)
	}
	// The "../evil.js" member normalises back inside the prefix and is written
	// there, never above dst; nothing may appear outside dst.
	if _, err := os.Stat(filepath.Join(dst, "..", "evil.js")); err == nil {
		t.Fatal("traversal wrote outside the destination")
	}
}

func TestExtractRejectsTraversalEntry(t *testing.T) {
	// A member whose cleaned path escapes the web prefix must be skipped.
	data := buildTarXZ(t, map[string][]byte{
		"./usr/share/jellyfin/web/ok.js":  []byte("ok"),
		"./etc/passwd":                    []byte("nope"),
		"./usr/share/jellyfin/other/x.js": []byte("nope"),
	})
	deb := buildDeb(t, []struct {
		name string
		body []byte
	}{{"data.tar.xz", data}})
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(deb), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.js")); err != nil {
		t.Fatalf("expected ok.js: %v", err)
	}
	for _, bad := range []string{"passwd", filepath.Join("other", "x.js")} {
		if _, err := os.Stat(filepath.Join(dst, bad)); err == nil {
			t.Fatalf("%s should not be extracted", bad)
		}
	}
}

func TestExtractRejectsLinkEntry(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "./usr/share/jellyfin/web/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	var out bytes.Buffer
	xw, _ := xz.NewWriter(&out)
	xw.Write(tarBuf.Bytes())
	xw.Close()
	deb := buildDeb(t, []struct {
		name string
		body []byte
	}{{"data.tar.xz", out.Bytes()}})
	if err := Extract(bytes.NewReader(deb), t.TempDir()); err == nil {
		t.Fatal("symlink member should be refused")
	}
}

// installServer serves a synthetic deb and returns its sha256.
func installServer(t *testing.T, deb []byte) (*httptest.Server, string, int64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(deb)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(deb)
	return srv, hex.EncodeToString(sum[:]), int64(len(deb))
}

func newTestInstaller(t *testing.T, deb []byte) (*Installer, string) {
	t.Helper()
	srv, sha, size := installServer(t, deb)
	in := New(t.TempDir(), srv.Client())
	in.URL = srv.URL
	in.SHA256 = sha
	in.Size = size
	return in, sha
}

func TestInstallerDownloadsAndReuses(t *testing.T) {
	deb := syntheticDeb(t, []byte("v1"), []byte("GPL-2.0"))
	in, _ := newTestInstaller(t, deb)
	dir, err := in.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := in.State(); st != StatusReady {
		t.Fatalf("state %d", st)
	}
	// A second installer over the same dir must not touch the network.
	srv2 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("network used on reuse")
	}))
	defer srv2.Close()
	reuse := New(in.Dir, srv2.Client())
	reuse.URL = srv2.URL
	reuse.SHA256 = in.SHA256
	reuse.Size = in.Size
	again, err := reuse.Ensure(t.Context())
	if err != nil || again != dir {
		t.Fatalf("reuse: %q %v (want %q)", again, err, dir)
	}
}

func TestInstallerRefusesShaMismatch(t *testing.T) {
	deb := syntheticDeb(t, []byte("v1"), []byte("GPL-2.0"))
	in, _ := newTestInstaller(t, deb)
	in.SHA256 = repeat("00", 32)
	if _, err := in.Ensure(t.Context()); err == nil {
		t.Fatal("expected sha mismatch")
	}
	if st, err := in.State(); st != StatusFailed || err == nil {
		t.Fatalf("state %d err %v", st, err)
	}
}

func TestInstallerBacksOffAfterFailure(t *testing.T) {
	deb := syntheticDeb(t, []byte("v1"), []byte("GPL-2.0"))
	in, _ := newTestInstaller(t, deb)
	in.SHA256 = repeat("00", 32)
	now := in.now()
	in.now = func() time.Time { return now }
	if _, err := in.Ensure(t.Context()); err == nil {
		t.Fatal("expected failure")
	}
	// A fresh installer would retry; this one remembers the failure.
	if _, err := in.Ensure(t.Context()); err == nil {
		t.Fatal("expected backoff to reuse the remembered failure")
	}
	in.now = func() time.Time { return now.Add(backoffDelay + time.Second) }
	if _, err := in.Ensure(t.Context()); err == nil {
		t.Fatal("expected retry past backoff to still fail on the stale sha")
	}
}

func repeat(s string, n int) string {
	var b bytes.Buffer
	for range n {
		b.WriteString(s)
	}
	return b.String()
}

func TestMarkerRecordsVersionAndSha(t *testing.T) {
	deb := syntheticDeb(t, []byte("v1"), []byte("GPL-2.0"))
	in, sha := newTestInstaller(t, deb)
	if _, err := in.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(in.markerPath())
	if err != nil {
		t.Fatal(err)
	}
	fields := bytes.Fields(body)
	if len(fields) != 3 || string(fields[0]) != Version || string(fields[1]) != sha {
		t.Fatalf("marker %q", body)
	}
	if _, err := strconv.ParseInt(string(fields[2]), 10, 64); err != nil {
		t.Fatalf("size field: %v", err)
	}
}
