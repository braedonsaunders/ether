package etherplugin

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
)

// BlobHost provides bounded data services. RemoteHostClient implements it;
// obtain it with ctx.Host.(etherplugin.BlobHost) when starting from Host.
// The host enforces network grants and the plugin's library cache quota.
type BlobHost interface {
	FetchToCache(context.Context, FetchCacheRequest) (FetchCacheResult, error)
	CacheRead(context.Context, string, DataRange) ([]byte, error)
	ArchiveList(context.Context, string) ([]ArchiveMember, error)
	ArchiveOpen(context.Context, string, int, DataRange) ([]byte, error)
}

type DataRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}
type FetchCacheRequest struct {
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	LibraryKey string            `json:"libraryKey"`
	Name       string            `json:"name"`
}
type FetchCacheResult struct {
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	CRC32  string `json:"crc32"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	Status int    `json:"status"`
}
type ArchiveMember struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
}

func (c *RemoteHostClient) dataCall(ctx context.Context, name string, args []any, out any) error {
	_, err := c.callJSON(ctx, http.MethodPost, "/ether/plugin-host/data/"+name, args, out, false)
	return err
}
func (c *RemoteHostClient) FetchToCache(ctx context.Context, r FetchCacheRequest) (FetchCacheResult, error) {
	var out FetchCacheResult
	err := c.dataCall(ctx, "http.fetchToCache", []any{r}, &out)
	return out, err
}
func (c *RemoteHostClient) ArchiveList(ctx context.Context, blob string) ([]ArchiveMember, error) {
	var out []ArchiveMember
	err := c.dataCall(ctx, "archive.list", []any{blob}, &out)
	return out, err
}
func (c *RemoteHostClient) dataRead(ctx context.Context, name string, args []any) ([]byte, error) {
	var out struct {
		BodyBase64 string `json:"bodyBase64"`
	}
	if err := c.dataCall(ctx, name, args, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.BodyBase64)
}
func checkDataRange(r DataRange) error {
	if r.Offset < 0 || r.Offset > 256<<20 || r.Length <= 0 || r.Length > 1<<20 {
		return fmt.Errorf("data range needs nonnegative offset and length 1..1048576")
	}
	return nil
}
func (c *RemoteHostClient) CacheRead(ctx context.Context, blob string, r DataRange) ([]byte, error) {
	if err := checkDataRange(r); err != nil {
		return nil, err
	}
	return c.dataRead(ctx, "cache.read", []any{blob, r})
}

// ArchiveOpen reads a bounded range of an uncompressed ZIP member. Deflate
// members are inflated from the start per call; use sequential large chunks.
func (c *RemoteHostClient) ArchiveOpen(ctx context.Context, blob string, member int, r DataRange) ([]byte, error) {
	if err := checkDataRange(r); err != nil {
		return nil, err
	}
	return c.dataRead(ctx, "archive.open", []any{blob, member, r})
}
