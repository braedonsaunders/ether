package plugins

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"strings"
	"time"
)

// Data-service limits are host enforced, independent of guest memory. All
// operations are on demand and use the existing plugin-owned cache namespace.
const (
	MaxDataFetchBytes       = 256 << 20
	MaxDataRangeBytes       = 1 << 20
	maxArchiveMembers       = 4096
	maxArchiveDirectory     = 4 << 20
	maxArchiveMemberBytes   = 64 << 20
	maxArchiveExpandedBytes = 256 << 20
	maxArchiveRatio         = 1000
)

type DataRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type FetchCacheRequest struct {
	FetchRequest
	LibraryKey string `json:"libraryKey"`
	Name       string `json:"name"`
}

type FetchCacheResult struct {
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	CRC32  string `json:"crc32"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	Status int    `json:"status"`
}

// boundedDataReader propagates cancellation and rejects overflow instead of
// turning a truncated file into a successful content-addressed cache entry.
type boundedDataReader struct {
	ctx  context.Context
	r    io.Reader
	left int64
	size int64
}

func (r *boundedDataReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left < 0 {
		return 0, fmt.Errorf("plugins: data exceeds size limit")
	}
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	r.size += int64(n)
	if r.left < 0 {
		return n, fmt.Errorf("plugins: data exceeds size limit")
	}
	return n, err
}

func (h *host) HTTPFetchToCache(ctx context.Context, req FetchCacheRequest) (FetchCacheResult, error) {
	if req.Method != "" && req.Method != "GET" {
		return FetchCacheResult{}, fmt.Errorf("plugins: fetchToCache supports GET only")
	}
	if err := h.requireWritableToolContext(ctx); err != nil {
		return FetchCacheResult{}, err
	}
	if _, err := h.cacheWriteCheck(ctx, req.LibraryKey, req.Name); err != nil {
		return FetchCacheResult{}, err
	}
	resp, err := h.openFetch(ctx, req.FetchRequest)
	if err != nil {
		return FetchCacheResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchCacheResult{}, fmt.Errorf("plugins: fetchToCache HTTP %d", resp.StatusCode)
	}
	crc, m, s := crc32.NewIEEE(), md5.New(), sha1.New()
	reader := &boundedDataReader{ctx: ctx, r: resp.Body, left: MaxDataFetchBytes}
	ref, err := h.CachePut(ctx, req.LibraryKey, req.Name, io.TeeReader(reader, io.MultiWriter(crc, m, s)))
	h.noteFetchTraffic(ctx, req.FetchRequest, reader.size)
	if err != nil {
		return FetchCacheResult{}, err
	}
	return FetchCacheResult{Blob: ref, Size: reader.size, CRC32: fmt.Sprintf("%08x", crc.Sum32()), MD5: fmt.Sprintf("%x", m.Sum(nil)), SHA1: fmt.Sprintf("%x", s.Sum(nil)), Status: resp.StatusCode}, nil
}

func validDataRange(r DataRange) error {
	if r.Offset < 0 || r.Length <= 0 || r.Length > MaxDataRangeBytes || r.Offset > MaxDataFetchBytes {
		return fmt.Errorf("plugins: range needs nonnegative offset and length 1..%d", MaxDataRangeBytes)
	}
	return nil
}

// Always return base64: arbitrary text/control bytes then have a predictable
// JSON envelope well below the 4 MiB host-result limit.
func readDataRange(ctx context.Context, r io.Reader, opt DataRange) (map[string]any, error) {
	if err := validDataRange(opt); err != nil {
		return nil, err
	}
	cr := &boundedDataReader{ctx: ctx, r: r, left: MaxDataFetchBytes}
	if _, err := io.CopyN(io.Discard, cr, opt.Offset); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(cr, opt.Length))
	if err != nil {
		return nil, err
	}
	return map[string]any{"bodyBase64": base64.StdEncoding.EncodeToString(data), "size": len(data)}, nil
}

// seekReaderAt gives zip.Reader random access without materializing an archive.
// It is used by one synchronous host call, never shared between goroutines.
type seekReaderAt struct {
	ctx context.Context
	r   io.ReadSeeker
}

func (r seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := r.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

type ArchiveMember struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
}

func openDataZIP(ctx context.Context, rc io.ReadSeeker) (*zip.Reader, error) {
	size, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size < 22 || size > MaxDataFetchBytes {
		return nil, fmt.Errorf("plugins: ZIP size outside limit")
	}
	ra := seekReaderAt{ctx, rc}
	tailSize := min(size, int64(65557))
	tail := make([]byte, tailSize)
	if _, err := ra.ReadAt(tail, size-tailSize); err != nil {
		return nil, err
	}
	// Inspect EOCD before zip.NewReader allocates the central directory. ZIP64
	// and split archives are deliberately refused by this bounded ZIP service.
	found := false
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) != len(tail) {
			continue
		}
		e := tail[i:]
		count := binary.LittleEndian.Uint16(e[10:])
		directory := binary.LittleEndian.Uint32(e[12:])
		if binary.LittleEndian.Uint16(e[4:]) != 0 || binary.LittleEndian.Uint16(e[6:]) != 0 || binary.LittleEndian.Uint16(e[8:]) != count || count > maxArchiveMembers || directory > maxArchiveDirectory || binary.LittleEndian.Uint32(e[16:]) == 0xffffffff {
			return nil, fmt.Errorf("plugins: ZIP directory exceeds limits or uses unsupported ZIP64/split format")
		}
		// Require a contiguous, bounded directory ending at this EOCD.
		offset := int64(binary.LittleEndian.Uint32(e[16:]))
		if offset+int64(directory) != size-tailSize+int64(i) {
			return nil, fmt.Errorf("plugins: unsupported ZIP directory layout")
		}
		raw := make([]byte, int(directory))
		if _, err := ra.ReadAt(raw, offset); err != nil {
			return nil, err
		}
		pos := 0
		for n := 0; n < int(count); n++ {
			if pos+46 > len(raw) || binary.LittleEndian.Uint32(raw[pos:]) != 0x02014b50 {
				return nil, fmt.Errorf("plugins: invalid ZIP directory")
			}
			header := raw[pos:]
			pos += 46 + int(binary.LittleEndian.Uint16(header[28:])) + int(binary.LittleEndian.Uint16(header[30:])) + int(binary.LittleEndian.Uint16(header[32:]))
			if pos > len(raw) {
				return nil, fmt.Errorf("plugins: invalid ZIP directory length")
			}
		}
		if pos != len(raw) {
			return nil, fmt.Errorf("plugins: ZIP directory count mismatch")
		}
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("plugins: ZIP directory not found")
	}
	z, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, err
	}
	if len(z.File) > maxArchiveMembers {
		return nil, fmt.Errorf("plugins: ZIP has too many members")
	}
	var total uint64
	names := 0
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		names += len(f.Name)
		if name == "" || len(f.Name) > 1024 || names > MaxDataRangeBytes/2 || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("plugins: unsafe ZIP member name")
		}
		if !f.FileInfo().IsDir() && !f.Mode().IsRegular() {
			return nil, fmt.Errorf("plugins: unsupported ZIP member type")
		}
		if f.Flags&1 != 0 || (f.Method != zip.Store && f.Method != zip.Deflate) {
			return nil, fmt.Errorf("plugins: encrypted or unsupported ZIP member")
		}
		if f.CompressedSize64 > uint64(size) || f.UncompressedSize64 > maxArchiveMemberBytes || (f.UncompressedSize64 > 0 && (f.CompressedSize64 == 0 || f.UncompressedSize64 > f.CompressedSize64*maxArchiveRatio)) {
			return nil, fmt.Errorf("plugins: ZIP member exceeds size or compression-ratio limit")
		}
		total += f.UncompressedSize64
		if total > maxArchiveExpandedBytes {
			return nil, fmt.Errorf("plugins: ZIP expanded size exceeds limit")
		}
	}
	return z, nil
}

// CallDataHost is the shared hosted/remote data-service funnel. Only the four
// named services are accepted; cache access rechecks plugin ownership each call.
func CallDataHost(ctx context.Context, h Host, name string, raw json.RawMessage) (json.RawMessage, error) {
	// Remote callers receive the same maximum I/O deadline as hosted calls.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var args []json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	var value any
	var err error
	if name == "http.fetchToCache" {
		var req struct {
			Method     string            `json:"method"`
			URL        string            `json:"url"`
			Headers    map[string]string `json:"headers"`
			LibraryKey string            `json:"libraryKey"`
			Name       string            `json:"name"`
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("plugins: fetchToCache takes one request")
		}
		if err = json.Unmarshal(args[0], &req); err != nil {
			return nil, err
		}
		if req.Method != "" && req.Method != "GET" {
			return nil, fmt.Errorf("plugins: fetchToCache supports GET only")
		}
		service, ok := h.(interface {
			HTTPFetchToCache(context.Context, FetchCacheRequest) (FetchCacheResult, error)
		})
		if !ok {
			return nil, ErrUnavailable
		}
		value, err = service.HTTPFetchToCache(ctx, FetchCacheRequest{FetchRequest: FetchRequest{Method: req.Method, URL: req.URL, Headers: req.Headers}, LibraryKey: req.LibraryKey, Name: req.Name})
	} else {
		if name != "cache.read" && name != "archive.list" && name != "archive.open" {
			return nil, ErrPermissionDenied
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("plugins: data service needs a blob reference")
		}
		var ref string
		if err = json.Unmarshal(args[0], &ref); err != nil {
			return nil, err
		}
		var opt DataRange
		var member int
		switch name {
		case "archive.list":
			if len(args) != 1 {
				return nil, fmt.Errorf("plugins: archive.list takes one blob reference")
			}
		case "cache.read":
			if len(args) != 2 {
				return nil, fmt.Errorf("plugins: cache.read takes (blob, range)")
			}
			err = json.Unmarshal(args[1], &opt)
		case "archive.open":
			if len(args) != 3 {
				return nil, fmt.Errorf("plugins: archive.open takes (blob, memberIndex, range)")
			}
			if err = json.Unmarshal(args[1], &member); err == nil {
				err = json.Unmarshal(args[2], &opt)
			}
		}
		if err != nil {
			return nil, err
		}
		if name != "archive.list" {
			if err = validDataRange(opt); err != nil {
				return nil, err
			}
		}
		rc, e := h.CacheOpen(ctx, ref)
		if e != nil {
			return nil, e
		}
		defer rc.Close()
		if name == "cache.read" {
			if _, err = rc.Seek(opt.Offset, io.SeekStart); err != nil {
				return nil, err
			}
			opt.Offset = 0
			value, err = readDataRange(ctx, rc, opt)
		} else {
			z, e := openDataZIP(ctx, rc)
			if e != nil {
				return nil, e
			}
			if name == "archive.list" {
				members := make([]ArchiveMember, 0, len(z.File))
				for i, f := range z.File {
					members = append(members, ArchiveMember{i, f.Name, f.UncompressedSize64})
				}
				value = members
			} else {
				if member < 0 || member >= len(z.File) {
					return nil, fmt.Errorf("plugins: ZIP member index outside range")
				}
				f := z.File[member]
				if uint64(opt.Offset) > f.UncompressedSize64 {
					return nil, fmt.Errorf("plugins: ZIP member offset outside range")
				}
				stream, e := f.Open()
				if e != nil {
					return nil, e
				}
				defer stream.Close()
				value, err = readDataRange(ctx, io.LimitReader(stream, int64(f.UncompressedSize64)+1), opt)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
