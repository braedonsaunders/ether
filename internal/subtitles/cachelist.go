package subtitles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CachedSubtitle is one downloaded sidecar as listed for the owner UI.
//
// What exists on disk, and what each field is derived from — nothing here
// is invented:
//
//   - The per-file meta (<id>.json, written by fetchAndCache) stores only
//     fileId, language, release and fetchedAt. The cache id is a hash of
//     title key + language + fileId, so the title's name cannot be
//     recovered from the cache files themselves.
//   - Key is the title's Plex ratingKey, recovered from the selection
//     index (index/<ratingKey>.<lang>.<releaseSig>.json, written by
//     saveSelection) whose stored selection id matches this cache id.
//     Empty when no index entry points at this file (e.g. a subtitle
//     fetched before any selection was remembered for it).
//   - FileName is the sidecar file on disk (<id>.srt); FetchedAt falls
//     back to that file's mtime when the meta carries no timestamp.
type CachedSubtitle struct {
	ID        string    `json:"id"`
	Language  string    `json:"language"`
	Release   string    `json:"release"`
	FetchedAt time.Time `json:"fetchedAt"`
	Key       string    `json:"key,omitempty"`
	FileName  string    `json:"fileName"`
}

func (s *Service) CachedSubtitles() ([]CachedSubtitle, error) {
	out := []CachedSubtitle{}
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	keys := s.indexKeysByID()
	for _, entry := range entries {
		if len(out) >= 1000 {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".sync.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !s.cached(id, "srt") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil || len(raw) > 64<<10 {
			continue
		}
		var c CachedSubtitle
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		c.ID = id
		c.FileName = id + ".srt"
		c.Key = keys[id]
		if c.FetchedAt.IsZero() {
			if info, err := os.Stat(filepath.Join(s.dir, c.FileName)); err == nil {
				c.FetchedAt = info.ModTime().UTC()
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FetchedAt.After(out[j].FetchedAt) })
	return out, nil
}

// indexKeysByID maps cache id -> title ratingKey using the selection index
// (see indexPath). When several index entries point at the same file, the
// most recently remembered selection (selection.At) wins; ties keep the
// first entry in filename order so the result is deterministic.
func (s *Service) indexKeysByID() map[string]string {
	out := map[string]string{}
	if s.dir == "" {
		return out
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "index"))
	if err != nil {
		return out
	}
	newest := map[string]time.Time{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		key, _, ok := strings.Cut(name, ".")
		if !ok {
			continue
		}
		if _, err := strconv.ParseInt(key, 10, 64); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "index", name))
		if err != nil || len(raw) > 64<<10 {
			continue
		}
		var sel selection
		if json.Unmarshal(raw, &sel) != nil || sel.ID == "" {
			continue
		}
		if at, seen := newest[sel.ID]; seen && !sel.At.After(at) {
			continue
		}
		newest[sel.ID] = sel.At
		out[sel.ID] = key
	}
	return out
}
