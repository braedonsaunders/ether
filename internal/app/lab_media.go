//go:build plexparity && !scorecard

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// labMediaFile is one probed file from the reference library. Paths stay in
// the data directory and are never written into client responses.
type labMediaFile struct {
	TMDB    int64           `json:"tmdb"`
	Season  int             `json:"season"`
	Episode int             `json:"episode"`
	Path    string          `json:"path"`
	SHA256  string          `json:"sha256"`
	Media   *core.MediaInfo `json:"media,omitempty"`
}

func labMediaPath(dir string) string { return filepath.Join(dir, "lab-media.json") }

type labMediaDocument struct {
	Version int            `json:"version"`
	Files   []labMediaFile `json:"files"`
}

func writeLabMedia(dir string, files []labMediaFile) error {
	payload, err := json.Marshal(labMediaDocument{Version: core.ProbeFactsVersion, Files: files})
	if err != nil {
		return err
	}
	return os.WriteFile(labMediaPath(dir), payload, 0600)
}

func decodeLabMedia(raw []byte) ([]labMediaFile, bool) {
	var doc labMediaDocument
	if json.Unmarshal(raw, &doc) != nil || doc.Version != core.ProbeFactsVersion || len(doc.Files) == 0 {
		return nil, false
	}
	return doc.Files, true
}

var labMediaReadCache = struct {
	mu    sync.Mutex
	dir   string
	files []labMediaFile
}{}

func readLabMedia(dir string) ([]labMediaFile, error) {
	labMediaReadCache.mu.Lock()
	defer labMediaReadCache.mu.Unlock()
	if labMediaReadCache.dir == dir && labMediaReadCache.files != nil {
		return labMediaReadCache.files, nil
	}
	b, err := os.ReadFile(labMediaPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	files, ok := decodeLabMedia(b)
	if !ok {
		return nil, nil
	}
	labMediaReadCache.dir = dir
	labMediaReadCache.files = files
	return files, nil
}

func labReleaseID(f labMediaFile) string {
	return fmt.Sprintf("lab-%d-%d-%d-%s", f.TMDB, f.Season, f.Episode, strings.ToLower(f.SHA256))
}

func labMediaVariants(dir string, t *core.Title) ([]core.ReleaseVariant, bool) {
	if t == nil {
		return nil, false
	}
	files, err := readLabMedia(dir)
	if err != nil || len(files) == 0 {
		return nil, false
	}
	tmdb, season, episode := t.IDs.TMDB, t.Season, t.Episode
	if t.Type == core.TypeEpisode {
		tmdb = t.ShowIDs.TMDB
		if tmdb == 0 {
			tmdb = t.ShowKey.TMDB()
		}
	}
	var out []core.ReleaseVariant
	for _, f := range files {
		if f.TMDB != tmdb || f.Season != season || f.Episode != episode {
			continue
		}
		if t.Type == core.TypeMovie && f.Episode != 0 {
			continue
		}
		if t.Type == core.TypeEpisode && f.Episode == 0 {
			continue
		}
		info := core.MediaInfo{Size: 0}
		if f.Media != nil {
			info = *f.Media
		}
		id := labReleaseID(f)
		out = append(out, core.ReleaseVariant{
			ID:      id,
			Label:   filepath.Base(f.Path),
			Release: core.Release{ID: id, GUID: id, Size: info.Size, TitleIDs: t.IDs, PubDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
			Info:    info,
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	// The lab harness plays the first version. Prefer an H.264 file the
	// phone profiles can open, shortest picture first.
	sort.SliceStable(out, func(i, j int) bool {
		vi, vj := out[i].Info.Video(), out[j].Info.Video()
		hi, hj := 0, 0
		ci, cj := "", ""
		if vi != nil {
			hi, ci = vi.Height, vi.Codec
		}
		if vj != nil {
			hj, cj = vj.Height, vj.Codec
		}
		if (ci == "h264") != (cj == "h264") {
			return ci == "h264"
		}
		return hi < hj
	})
	return out, true
}

func localEditionVariants(dir string, t *core.Title) ([]core.ReleaseVariant, error) {
	vs, _ := labMediaVariants(dir, t)
	return vs, nil
}

func localEditionVariantsMany(dir string, titles []core.Title) (map[core.RatingKey][]core.ReleaseVariant, error) {
	out := map[core.RatingKey][]core.ReleaseVariant{}
	for i := range titles {
		variants, err := localEditionVariants(dir, &titles[i])
		if err != nil {
			return nil, err
		}
		if len(variants) > 0 {
			out[titles[i].Key] = variants
		}
	}
	return out, nil
}
