// Package artwork serves and makes Ether's artwork: a caching proxy for
// TMDB images (memory LRU over a disk cache) and a pure-Go poster
// generator for collections (collage and backdrop treatments with the
// title set in the Go fonts).
package artwork

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

var (
	// ErrInvalidPath rejects anything that is not a TMDB image path.
	ErrInvalidPath = errors.New("artwork: not a TMDB image path")
	// ErrNotFound means the image does not exist upstream.
	ErrNotFound = errors.New("artwork: image not found")
)

const maxImageBytes = 15 << 20

var pathRe = regexp.MustCompile(`^/[A-Za-z0-9_\-]{1,128}\.(jpg|jpeg|png|webp|svg)$`)

// ValidPath reports whether p is a TMDB image path such as "/abc123.jpg":
// a single file name, so it can never traverse or address another host.
func ValidPath(p string) bool { return pathRe.MatchString(p) }

// tmdbWidths are the widths the TMDB CDN serves.
var tmdbWidths = []int{92, 154, 185, 300, 342, 500, 780, 1280}

// Size picks the smallest TMDB size at least w wide ("original" for 0,
// capped at w1280 otherwise).
func Size(w int) string {
	if w <= 0 {
		return "original"
	}
	for _, s := range tmdbWidths {
		if s >= w {
			return "w" + strconv.Itoa(s)
		}
	}
	return "w1280"
}

// ImagesOptions configure an image store.
type ImagesOptions struct {
	// Posters returns the current owner settings; nil preserves TMDB.
	Posters func() config.Posters

	// Dir is the disk cache directory ("" = memory only).
	Dir string
	// MemoryBytes bounds the in-memory LRU (default 64 MiB).
	MemoryBytes int64
	// DiskBytes bounds the disk cache (default 512 MiB).
	DiskBytes int64
	// HTTPClient fetches images (default: 20 s timeout).
	HTTPClient *http.Client
	// ImageURL builds TMDB URLs (default image.tmdb.org).
	ImageURL core.ImageURLFunc
	Logger   *slog.Logger
}

// Images is a caching TMDB image store. Concurrent requests for the same
// image share one fetch. Safe for concurrent use.
type Images struct {
	o             ImagesOptions
	log           *slog.Logger
	guardedClient *http.Client

	mu       sync.Mutex
	items    *lru.Cache[string, *memImage]
	inflight flight.Group[string, *memImage]

	posterSlots chan struct{}
	diskMu      sync.Mutex
	diskUsed    int64
	diskKnown   bool
}

type memImage struct {
	key  string
	data []byte
	ct   string
}

// NewImages creates an image store.
func NewImages(o ImagesOptions) *Images {
	if o.MemoryBytes <= 0 {
		o.MemoryBytes = 64 << 20
	}
	if o.DiskBytes <= 0 {
		o.DiskBytes = 512 << 20
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if o.ImageURL == nil {
		o.ImageURL = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Dir != "" {
		_ = os.MkdirAll(o.Dir, 0o755)
	}
	return &Images{posterSlots: make(chan struct{}, 2), o: o, log: o.Logger, guardedClient: (&safefetch.Guard{}).Client(o.HTTPClient), items: lru.New(lru.Options[string, *memImage]{MaxCost: o.MemoryBytes, Cost: func(_ string, img *memImage) int64 {
		return int64(len(img.data))
	}})}
}

// Get returns a TMDB image at the smallest size covering width w.
func (im *Images) Get(ctx context.Context, path string, w int) ([]byte, string, error) {
	if !ValidPath(path) {
		return nil, "", ErrInvalidPath
	}
	size := Size(w)
	return im.get(ctx, size+path, func(ctx context.Context) ([]byte, string, error) {
		return im.download(ctx, im.o.ImageURL(path, size))
	})
}

// GetURL fetches an owner-supplied image URL (custom collection art). Only
// http(s) URLs to images are accepted.
func (im *Images) GetURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, "", ErrInvalidPath
	}
	sum := sha1.Sum([]byte(rawURL))
	return im.get(ctx, "url/"+hex.EncodeToString(sum[:]), func(ctx context.Context) ([]byte, string, error) {
		return im.download(ctx, rawURL)
	})
}

// GetUntrustedURL fetches a URL supplied by a remote feed, preventing it from
// reaching private addresses or rebinding across redirects. Owner-configured
// collection URLs continue to use GetURL and retain their existing behavior.
func (im *Images) GetUntrustedURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return nil, "", ErrInvalidPath
	}
	sum := sha1.Sum([]byte(rawURL))
	return im.get(ctx, "untrusted-url/"+hex.EncodeToString(sum[:]), func(ctx context.Context) ([]byte, string, error) {
		return im.downloadWithClient(ctx, im.guardedClient, rawURL)
	})
}

func (im *Images) get(ctx context.Context, key string, load func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	im.mu.Lock()
	if img, ok := im.items.Get(key); ok {
		im.mu.Unlock()
		return img.data, img.ct, nil
	}
	im.mu.Unlock()
	// The fetch is shared, so it must not die with the first requester.
	img, err := im.inflight.DoDetached(ctx, key, func(ctx context.Context) (*memImage, error) {
		img, err := im.loadShared(ctx, key, load)
		if err == nil {
			im.mu.Lock()
			im.addLocked(img)
			im.mu.Unlock()
		}
		return img, err
	})
	if err != nil {
		return nil, "", err
	}
	return img.data, img.ct, nil
}

func (im *Images) loadShared(ctx context.Context, key string, load func(context.Context) ([]byte, string, error)) (*memImage, error) {
	if data, ct, ok := im.readDisk(key); ok {
		return &memImage{key: key, data: data, ct: ct}, nil
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, ct, err := load(fctx)
	if err != nil {
		return nil, err
	}
	im.writeDisk(key, data)
	return &memImage{key: key, data: data, ct: ct}, nil
}

func (im *Images) download(ctx context.Context, u string) ([]byte, string, error) {
	return im.downloadWithClient(ctx, im.o.HTTPClient, u)
}

func (im *Images) downloadWithClient(ctx context.Context, client *http.Client, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Ether (+https://github.com/braedonsaunders/ether)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("artwork: fetch %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("artwork: %s is too large", u)
	}
	ct := sniff(data)
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("artwork: %s is not an image", u)
	}
	return data, ct, nil
}

func sniff(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "text/xml") || strings.HasPrefix(ct, "text/plain") {
		if strings.Contains(string(data[:min(len(data), 512)]), "<svg") {
			return "image/svg+xml"
		}
	}
	return ct
}

func (im *Images) addLocked(img *memImage) {
	// One image may take at most a quarter of the memory budget.
	if int64(len(img.data)) > im.o.MemoryBytes/4 {
		return
	}
	im.items.Add(img.key, img)
}

// Disk layout: <dir>/<sha1(key)[:2]>/<sha1(key)>, content type sniffed on read.
func (im *Images) diskPath(key string) string {
	sum := sha1.Sum([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(im.o.Dir, h[:2], h)
}

func (im *Images) readDisk(key string) ([]byte, string, bool) {
	if im.o.Dir == "" {
		return nil, "", false
	}
	p := im.diskPath(key)
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now) // LRU by modification time
	return data, sniff(data), true
}

func (im *Images) writeDisk(key string, data []byte) {
	if im.o.Dir == "" {
		return
	}
	p := im.diskPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	im.diskMu.Lock()
	defer im.diskMu.Unlock()
	if !im.diskKnown {
		im.diskUsed = dirSize(im.o.Dir)
		im.diskKnown = true
	} else {
		im.diskUsed += int64(len(data))
	}
	if im.diskUsed > im.o.DiskBytes {
		im.diskUsed = trimDir(im.o.Dir, im.o.DiskBytes*9/10)
	}
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// trimDir deletes the least recently used files until the directory is
// under target bytes, and returns the new size.
func trimDir(dir string, target int64) int64 {
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				files = append(files, file{p, fi.Size(), fi.ModTime()})
				total += fi.Size()
			}
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
	return total
}
