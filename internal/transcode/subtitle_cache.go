package transcode

import (
	"bytes"
	"container/list"
	"context"
	"strconv"
	"sync"
)

// Extracting an embedded text subtitle reads the whole input, since its
// packets are interleaved through the file. A client asks for the same
// track again on a seek, a retry or the next session, so the WebVTT is kept
// per source and track and a concurrent request joins the one extraction.

type subtitleSourceKey struct{}

// WithSubtitleSource names the source an extraction reads, the same for
// every session of that source (its release). ExtractSubtitle caches the
// track's WebVTT under it; an empty name extracts uncached.
func WithSubtitleSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, subtitleSourceKey{}, source)
}

const (
	// subtitleCacheBytes bounds the WebVTT kept across sources.
	subtitleCacheBytes = 64 << 20
	// subtitleEntryMax is the largest track kept.
	subtitleEntryMax = 8 << 20
)

type subtitleEntry struct {
	key  string
	data []byte
	done chan struct{}
	err  error
	elem *list.Element
}

type subtitleCache struct {
	mu    sync.Mutex
	m     map[string]*subtitleEntry
	lru   list.List // of *subtitleEntry, most recent first
	bytes int
}

// extract returns the track's WebVTT, extracting it once per key.
func (c *subtitleCache) extract(ctx context.Context, key string, run func(context.Context, *bytes.Buffer) error) ([]byte, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*subtitleEntry{}
	}
	if e, ok := c.m[key]; ok {
		if e.elem != nil {
			c.lru.MoveToFront(e.elem)
		}
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err == nil {
			return e.data, nil
		}
		// The extraction this request joined failed (its client may have
		// gone); run one of its own.
		return c.extract(ctx, key, run)
	}
	e := &subtitleEntry{key: key, done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	var buf bytes.Buffer
	err := run(ctx, &buf)
	c.mu.Lock()
	e.err = err
	if err != nil || buf.Len() == 0 || buf.Len() > subtitleEntryMax {
		delete(c.m, key)
		if err == nil {
			e.data = buf.Bytes()
		}
	} else {
		e.data = buf.Bytes()
		e.elem = c.lru.PushFront(e)
		c.bytes += len(e.data)
		for c.bytes > subtitleCacheBytes && c.lru.Len() > 1 {
			old := c.lru.Remove(c.lru.Back()).(*subtitleEntry)
			delete(c.m, old.key)
			c.bytes -= len(old.data)
		}
	}
	close(e.done)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return e.data, nil
}

func subtitleCacheKey(source, inputCodec string, index int) string {
	return source + "\x00" + strconv.Itoa(index) + "\x00" + inputCodec
}
