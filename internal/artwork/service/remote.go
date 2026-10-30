package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/lru"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxImageBytes = 15 << 20
	imageTTL      = 24 * time.Hour
)

type RemoteImage struct {
	URL         string
	Data        []byte
	ContentType string
	FetchedAt   time.Time
}

type RemoteCache struct {
	client *http.Client
	budget int64

	mu       sync.Mutex
	items    *lru.Cache[string, *RemoteImage]
	inflight flight.Group[string, *RemoteImage]
}

func NewRemoteCache(budget int64, client *http.Client) *RemoteCache {
	return &RemoteCache{
		client: client,
		budget: budget,
		items: lru.New(lru.Options[string, *RemoteImage]{MaxCost: budget, Cost: func(_ string, img *RemoteImage) int64 {
			return int64(len(img.Data))
		}}),
	}
}

var ErrRemoteImageNotFound = errors.New("image not found")

// get returns the image at u, fetching it once for concurrent callers.
func (c *RemoteCache) Get(ctx context.Context, u string, now time.Time) (*RemoteImage, error) {
	c.mu.Lock()
	if img, ok := c.items.Get(u); ok {
		if now.Sub(img.FetchedAt) < imageTTL {
			c.mu.Unlock()
			return img, nil
		}
		c.items.Remove(u)
	}
	c.mu.Unlock()
	// The fetch is shared, so it must not die with the first requester.
	return c.inflight.DoDetached(ctx, u, func(ctx context.Context) (*RemoteImage, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		img, err := c.fetch(ctx, u, now)
		if err == nil {
			c.mu.Lock()
			c.addLocked(img)
			c.mu.Unlock()
		}
		return img, err
	})
}

func (c *RemoteCache) fetch(ctx context.Context, u string, now time.Time) (*RemoteImage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrRemoteImageNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("image fetch %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("image %s too large", u)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(data)
	}
	return &RemoteImage{URL: u, Data: data, ContentType: ct, FetchedAt: now}, nil
}

func (c *RemoteCache) addLocked(img *RemoteImage) {
	if int64(len(img.Data)) > c.budget {
		return
	}
	c.items.Add(img.URL, img)
}

// ---------------------------------------------------------------------------
// TMDB size selection

// tmdbWidths are the width variants the TMDB image CDN serves.
var tmdbWidths = []int{92, 154, 185, 300, 342, 500, 780, 1280}

// TMDBSize picks the smallest variant at least w wide, or "original".
func TMDBSize(w int) string {
	if w <= 0 {
		return "original"
	}
	for _, s := range tmdbWidths {
		if s >= w {
			return "w" + strconv.Itoa(s)
		}
	}
	return "original"
}
