package subtitles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CachedSubtitle identifies a sidecar from its own metadata. Key is internal
// only; the app resolves it through the catalog. Legacy metadata without a key
// stays unknown, with no selection-index scan on the list request path.
type CachedSubtitle struct {
	ID        string    `json:"id"`
	Language  string    `json:"language"`
	Release   string    `json:"release"`
	FetchedAt time.Time `json:"fetchedAt"`
	Key       string    `json:"-"`
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
	for _, entry := range entries {
		if len(out) >= 1000 {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
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
		var meta struct {
			CachedSubtitle
			TitleKey string `json:"titleKey"`
		}
		c := CachedSubtitle{}
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		c = meta.CachedSubtitle
		c.Key = meta.TitleKey
		c.ID = id
		if c.FetchedAt.IsZero() {
			if info, err := os.Stat(filepath.Join(s.dir, id+".srt")); err == nil {
				c.FetchedAt = info.ModTime().UTC()
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FetchedAt.After(out[j].FetchedAt) })
	return out, nil
}
