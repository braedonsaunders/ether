package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"time"

	_ "golang.org/x/image/webp"
)

// RasterFormat caches explicit output formats alongside fetched artwork, within
// the same byte budget. Callers must authorize the source before calling it.
// The content digest identifies the actual source variant (including its pixel
// size), so refreshed bytes and different sizes cannot reuse stale conversions.
// Returning to a warm poster requires a hash/lookup, never a pixel decode.
func (c *RemoteCache) RasterFormat(ctx context.Context, data []byte, contentType, format string, now time.Time) ([]byte, string, error) {
	if (format != "jpeg" && format != "png") || contentType == "image/"+format {
		return data, contentType, nil
	}
	key := fmt.Sprintf("raster-format-v1:%x:%s", sha256.Sum256(data), format)
	cached := func() *RemoteImage {
		c.mu.Lock()
		defer c.mu.Unlock()
		if img, ok := c.items.Get(key); ok {
			if now.Sub(img.FetchedAt) < imageTTL {
				return img
			}
			c.items.Remove(key)
		}
		return nil
	}
	if img := cached(); img != nil {
		return img.Data, img.ContentType, nil
	}
	img, err := c.inflight.DoDetached(ctx, key, func(ctx context.Context) (*RemoteImage, error) {
		// A prior flight may have populated the cache between lookup and joining.
		if img := cached(); img != nil {
			return img, nil
		}
		converted, ct, err := encodeRasterFormat(data, format)
		if err != nil {
			return nil, err
		}
		img := &RemoteImage{URL: key, Data: converted, ContentType: ct, FetchedAt: now}
		c.mu.Lock()
		c.addLocked(img)
		c.mu.Unlock()
		return img, nil
	})
	if err != nil {
		return nil, "", err
	}
	return img.Data, img.ContentType, nil
}

func encodeRasterFormat(data []byte, format string) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 16000000 {
		return nil, "", fmt.Errorf("artwork dimensions exceed raster limit")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	if format == "png" {
		err = png.Encode(&out, decoded)
	} else {
		err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 90})
	}
	return out.Bytes(), "image/" + format, err
}
