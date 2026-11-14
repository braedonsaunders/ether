package extras

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrCachePreparing = errors.New("trailer cache busy")

// CachedTrailer identifies immutable, locally downloaded trailer bytes.
// OpenCachedTrailer must be used to open it; eviction may have removed it.
type CachedTrailer struct {
	Path, MIMEType, SourceGeneration string
	extra                            Extra
}

func (s *Service) trailerGeneration(e Extra) string {
	c := s.cfg()
	identity, _ := json.Marshal([]any{e.ParentKey, e.Site, e.SiteKey, e.PublishedAt, c.ResolverCommand, c.ResolverArgs})
	h := sha256.Sum256(identity)
	return hex.EncodeToString(h[:])
}

// CacheTrailer runs only on focus/open or explicit playback. One download at a
// time bounds network and staging bytes; other callers poll with preparing.
// The disk index is the directory itself, capped at 512 complete trailers.
// No timers, sweeps, provider calls or filesystem work run while unused.
func (s *Service) CacheTrailer(ctx context.Context, e Extra) (CachedTrailer, error) {
	if !s.cfg().Enabled || s.cacheDir == "" || s.cfg().TrailerCacheMaxBytes < 0 || !e.IsTrailer() {
		return CachedTrailer{}, errors.New("trailer cache disabled")
	}
	if !s.cacheDownloadMu.TryLock() {
		return CachedTrailer{}, ErrCachePreparing
	}
	defer s.cacheDownloadMu.Unlock()
	cap := s.cfg().TrailerCacheMaxBytes
	if cap == 0 {
		cap = 512 << 20
	}
	generation := s.trailerGeneration(e)
	cached := CachedTrailer{Path: filepath.Join(s.cacheDir, generation+".mp4"), MIMEType: "video/mp4", SourceGeneration: generation, extra: e}
	if err := os.MkdirAll(s.cacheDir, 0700); err != nil {
		return CachedTrailer{}, err
	}
	// Retire an interrupted staging download before accounting for a new one.
	if err := os.Remove(filepath.Join(s.cacheDir, "download.part")); err != nil && !os.IsNotExist(err) {
		return CachedTrailer{}, err
	}
	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return CachedTrailer{}, err
	}
	var files []os.FileInfo
	var used int64
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".mp4") {
			info, err := entry.Info()
			if err != nil {
				return CachedTrailer{}, err
			}
			files = append(files, info)
			used += info.Size()
		}
	}

	// Enforce cap reductions even if resolving the requested source later fails.
	if used > cap || len(files) > 512 {
		if info, err := os.Stat(cached.Path); err == nil && info.Size() <= cap {
			now := s.now()
			_ = os.Chtimes(cached.Path, now, now)
			for i, f := range files {
				if f.Name() == filepath.Base(cached.Path) {
					if fresh, err := os.Stat(cached.Path); err == nil {
						files[i] = fresh
					}
				}
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
		for len(files) > 0 && (used > cap || len(files) > 512) {
			oldest := files[0]
			files = files[1:]
			s.cacheMu.Lock()
			err := os.Remove(filepath.Join(s.cacheDir, oldest.Name()))
			s.cacheMu.Unlock()
			if err != nil {
				return CachedTrailer{}, err
			}
			used -= oldest.Size()
		}
	}
	var response *http.Response
	reserve := int64(0)
	hit, statErr := os.Stat(cached.Path)
	if statErr != nil || hit.Size() <= 0 || hit.Size() > cap {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		resolved, err := s.Resolve(ctx, e)
		if err != nil {
			return CachedTrailer{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved.URL, nil)
		if err != nil {
			return CachedTrailer{}, err
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return CachedTrailer{}, errors.New("unsupported trailer URL")
		}
		for name, value := range resolved.Headers {
			req.Header.Set(name, value)
		}
		client := s.cacheHTTP
		if client == nil {
			client = &http.Client{Timeout: 90 * time.Second}
		}
		response, err = client.Do(req)
		if err != nil {
			return CachedTrailer{}, err
		}
		defer response.Body.Close()
		reserve = min(cap, int64(64<<20))
		if response.StatusCode != http.StatusOK || response.ContentLength > reserve {
			return CachedTrailer{}, errors.New("trailer unavailable or exceeds cache entry bound")
		}
		if response.ContentLength > 0 {
			reserve = response.ContentLength
		}
	} else {
		// A hit is the most recently used file, including after a cap reduction.
		now := s.now()
		_ = os.Chtimes(cached.Path, now, now)
		for i, f := range files {
			if f.Name() == filepath.Base(cached.Path) {
				if info, err := os.Stat(cached.Path); err == nil {
					files[i] = info
				}
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
	for len(files) > 0 && (used > cap-reserve || len(files) >= 512 && reserve > 0) {
		oldest := files[0]
		files = files[1:]
		s.cacheMu.Lock()
		err := os.Remove(filepath.Join(s.cacheDir, oldest.Name()))
		s.cacheMu.Unlock()
		if err != nil {
			return CachedTrailer{}, err
		}
		used -= oldest.Size()
	}
	if response == nil {
		return cached, nil
	}
	stage := filepath.Join(s.cacheDir, "download.part")
	f, err := os.OpenFile(stage, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return CachedTrailer{}, err
	}
	defer os.Remove(stage)
	n, copyErr := io.Copy(f, io.LimitReader(response.Body, reserve))
	var extra [1]byte
	extraBytes, tailErr := response.Body.Read(extra[:])
	if copyErr == nil && tailErr != nil && tailErr != io.EOF {
		copyErr = tailErr
	}
	var header [12]byte
	_, readErr := f.ReadAt(header[:], 0)
	closeErr := f.Close()
	if copyErr != nil {
		return CachedTrailer{}, copyErr
	}
	if closeErr != nil {
		return CachedTrailer{}, closeErr
	}
	if extraBytes != 0 || (response.ContentLength >= 0 && n != response.ContentLength) || readErr != nil || string(header[4:8]) != "ftyp" {
		return CachedTrailer{}, errors.New("trailer is not a bounded MP4 file")
	}
	if err := os.Rename(stage, cached.Path); err != nil {
		return CachedTrailer{}, err
	}
	return cached, nil
}

// OpenCachedTrailer serializes opening against eviction (including Windows).
func (s *Service) OpenCachedTrailer(c CachedTrailer) (*os.File, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.cfg().Enabled || s.cfg().TrailerCacheMaxBytes < 0 || c.Path != filepath.Join(s.cacheDir, c.SourceGeneration+".mp4") || len(c.SourceGeneration) != 64 || s.trailerGeneration(c.extra) != c.SourceGeneration {
		return nil, errors.New("invalid cached trailer")
	}
	f, err := os.Open(c.Path)
	if err == nil {
		now := s.now()
		_ = os.Chtimes(c.Path, now, now)
	}
	return f, err
}
