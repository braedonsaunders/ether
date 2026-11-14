package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"fmt"
	"hash/crc32"
	"net/http"

	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"strings"
	"testing"
)

type dataRoundTripper func(*http.Request) (*http.Response, error)

func (f dataRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type dataTestHost struct {
	Host
	data   []byte
	opens  int
	closes int
}
type dataTestFile struct {
	*bytes.Reader
	close func()
}

func (f dataTestFile) Close() error { f.close(); return nil }
func (h *dataTestHost) CacheOpen(context.Context, string) (io.ReadSeekCloser, error) {
	h.opens++
	return dataTestFile{bytes.NewReader(h.data), func() { h.closes++ }}, nil
}
func dataZIP(t *testing.T, name string, method uint16, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// These are host-service invariants: bounded envelopes, random access without
// whole-archive guest copies, pre-allocation directory validation and containment.
func TestDataHostInvariants(t *testing.T) {
	ctx := context.Background()

	t.Run("streaming acquisition and grants", func(t *testing.T) {
		env := newTestEnv(t)
		rec := installContent(t, env)
		rec.Manifest.Ether.Permissions.Network = []string{"data.example.test"}
		var marshalErr error
		rec.ManifestRaw, marshalErr = json.Marshal(rec.Manifest)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := env.m.store.upsert(ctx, rec); err != nil {
			t.Fatal(err)
		}
		body := []byte("a streamed dataset")
		requests := 0
		writes := 0
		env.m.client = &http.Client{Transport: dataRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})}
		env.m.SetKindBackends(KindBackends{CachePut: func(_ context.Context, plugin, library, name string, r io.Reader) (string, error) {
			writes++
			data, err := io.ReadAll(r)
			if plugin != rec.ID || library != core.PluginLibraryKey(rec.ID, "recording") || name != "data.zip" || !bytes.Equal(data, body) {
				t.Fatal("cache received wrong bytes or namespace")
			}
			return "blob:test", err
		}})
		h := env.m.HostFor(rec.ID).(*host)
		req := FetchCacheRequest{FetchRequest: FetchRequest{URL: "https://data.example.test/data.zip"}, LibraryKey: core.PluginLibraryKey(rec.ID, "recording"), Name: "data.zip"}
		out, err := h.HTTPFetchToCache(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if out.Size != int64(len(body)) || out.CRC32 != fmt.Sprintf("%08x", crc32.ChecksumIEEE(body)) || out.MD5 != fmt.Sprintf("%x", md5.Sum(body)) || out.SHA1 != fmt.Sprintf("%x", sha1.Sum(body)) {
			t.Fatalf("digests: %+v", out)
		}
		bad := req
		bad.LibraryKey = "another-plugin-library"
		if _, err = h.HTTPFetchToCache(ctx, bad); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("foreign cache namespace: %v", err)
		}
		if requests != 1 || writes != 1 {
			t.Fatal("foreign cache namespace caused I/O")
		}
		if err = env.m.SetGrants(ctx, rec.ID, Grants{Revoked: []string{"network"}}); err != nil {
			t.Fatal(err)
		}
		if _, err = h.HTTPFetchToCache(ctx, req); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("revoked network: %v", err)
		}
		if requests != 1 || writes != 1 {
			t.Fatal("revoked network caused I/O")
		}
	})
	t.Run("ranged blob and ZIP member", func(t *testing.T) {
		body := bytes.Repeat([]byte{0, 255, 1, 2, 3, 4, 5, 6}, (5<<20)/8)
		h := &dataTestHost{data: body}
		raw, err := CallDataHost(ctx, h, "cache.read", json.RawMessage(`["blob:test",{"offset":4194300,"length":100}]`))
		if err != nil {
			t.Fatal(err)
		}
		var answer struct {
			BodyBase64 string `json:"bodyBase64"`
		}
		if err = json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}
		got, err := base64.StdEncoding.DecodeString(answer.BodyBase64)
		if err != nil || !bytes.Equal(got, body[4194300:4194400]) {
			t.Fatalf("range mismatch: %v", err)
		}
		h.data = dataZIP(t, "data/member.bin", zip.Store, body)
		raw, err = CallDataHost(ctx, h, "archive.list", json.RawMessage(`["blob:test"]`))
		if err != nil {
			t.Fatal(err)
		}
		var members []ArchiveMember
		if err = json.Unmarshal(raw, &members); err != nil || len(members) != 1 || members[0].Size != uint64(len(body)) {
			t.Fatalf("list %s: %v", raw, err)
		}
		raw, err = CallDataHost(ctx, h, "archive.open", json.RawMessage(`["blob:test",0,{"offset":4194300,"length":1048576}]`))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= 4<<20 {
			t.Fatalf("range envelope too large: %d", len(raw))
		}
		if err = json.Unmarshal(raw, &answer); err != nil {
			t.Fatal(err)
		}
		got, err = base64.StdEncoding.DecodeString(answer.BodyBase64)
		if err != nil || !bytes.Equal(got, body[4194300:4194300+(1<<20)]) {
			t.Fatalf("member mismatch: %v", err)
		}

		h.data = dataZIP(t, "data/member.bin", zip.Deflate, body)
		if _, err = CallDataHost(ctx, h, "archive.open", json.RawMessage(`["blob:test",0,{"offset":4194300,"length":100}]`)); err != nil {
			t.Fatalf("deflate range: %v", err)
		}
		if h.opens != h.closes {
			t.Fatalf("leaked readers: %d opens/%d closes", h.opens, h.closes)
		}
		before := h.opens
		for _, args := range []string{`["blob:test",{"offset":-1,"length":1}]`, `["blob:test",{"offset":0,"length":1048577}]`} {
			if _, err = CallDataHost(ctx, h, "cache.read", json.RawMessage(args)); err == nil {
				t.Fatal("invalid range accepted")
			}
		}
		if before != h.opens {
			t.Fatal("invalid range opened cache")
		}
	})
	t.Run("directory and expansion limits", func(t *testing.T) {
		for _, name := range []string{"../escape", "/absolute", "C:/windows", "dir/../escape", "dir\\escape"} {
			if _, err := openDataZIP(ctx, bytes.NewReader(dataZIP(t, name, zip.Store, []byte("ok")))); err == nil {
				t.Fatalf("accepted unsafe name %q", name)
			}
		}
		bomb := dataZIP(t, "bomb", zip.Deflate, bytes.Repeat([]byte("a"), 2<<20))
		central := bytes.Index(bomb, []byte{0x50, 0x4b, 0x01, 0x02})
		binary.LittleEndian.PutUint32(bomb[central+24:], 64<<20)
		if _, err := openDataZIP(ctx, bytes.NewReader(bomb)); err == nil {
			t.Fatal("compression bomb accepted")
		}
		b := dataZIP(t, "ok", zip.Store, []byte("ok"))
		end := len(b) - 22
		binary.LittleEndian.PutUint16(b[end+8:], 0)
		binary.LittleEndian.PutUint16(b[end+10:], 0)
		if _, err := openDataZIP(ctx, bytes.NewReader(b)); err == nil {
			t.Fatal("directory count mismatch accepted")
		}
		binary.LittleEndian.PutUint16(b[end+8:], 1)
		binary.LittleEndian.PutUint16(b[end+10:], 1)
		binary.LittleEndian.PutUint32(b[end+12:], maxArchiveDirectory+1)
		if _, err := openDataZIP(ctx, bytes.NewReader(b)); err == nil {
			t.Fatal("oversized directory accepted")
		}
	})
	t.Run("overflow and cancellation propagate", func(t *testing.T) {
		r := &boundedDataReader{ctx: ctx, r: strings.NewReader("12345"), left: 4}
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("overflow became successful EOF")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		r = &boundedDataReader{ctx: canceled, r: strings.NewReader("a"), left: 4}
		if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	})
}
