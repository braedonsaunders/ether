//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ScorecardFile describes a verified file inside the read-only lab mount.
type ScorecardFile struct {
	TMDB    int64            `json:"tmdb"`
	Season  int              `json:"season"`
	Episode int              `json:"episode"`
	Path    string           `json:"path"`
	SHA256  string           `json:"sha256"`
	Info    core.ReleaseInfo `json:"info"`
	Media   *core.MediaInfo  `json:"media_info,omitempty"`
}

func scorecardReleaseID(f ScorecardFile) string {
	return fmt.Sprintf("scorecard-%d-%d-%d-%s", f.TMDB, f.Season, f.Episode, strings.ToLower(f.SHA256))
}

type scorecardSource struct{ dir string }

func (s scorecardSource) files() ([]ScorecardFile, error) {
	b, e := os.ReadFile(filepath.Join(s.dir, "scorecard-files.json"))
	if e != nil {
		return nil, e
	}
	var files []ScorecardFile
	e = json.Unmarshal(b, &files)
	return files, e
}
func (s scorecardSource) Name() string { return "scorecard local files" }
func (s scorecardSource) Search(_ context.Context, q core.SearchRequest) ([]core.Release, error) {
	files, e := s.files()
	if e != nil {
		return nil, e
	}
	var out []core.Release
	for _, f := range files {
		if f.TMDB != q.IDs.TMDB || f.Season != q.Season || f.Episode != q.Episode || (q.Type == core.TypeMovie && f.Episode != 0) || (q.Type == core.TypeEpisode && f.Episode == 0) || (q.Type != core.TypeMovie && q.Type != core.TypeEpisode) {
			continue
		}
		stat, e := os.Stat(f.Path)
		if e != nil {
			return nil, e
		}
		info := f.Info
		info.Title = q.Title
		info.Year = q.Year
		if q.Type == core.TypeEpisode {
			info.Seasons = []int{q.Season}
			info.Episodes = []int{q.Episode}
		}
		key := scorecardReleaseID(f)
		name := fmt.Sprintf("%s %d %dp WEB-DL %s %s", q.Title, q.Year, info.Height, info.VideoCodec, info.AudioCodec)
		if q.Type == core.TypeEpisode {
			name = fmt.Sprintf("%s S%02dE%02d %dp WEB-DL %s %s", q.Title, q.Season, q.Episode, info.Height, info.VideoCodec, info.AudioCodec)
		}
		out = append(out, core.Release{ID: key, GUID: key, Indexer: s.Name(), Name: name, Size: stat.Size(), TitleIDs: q.IDs, Info: info, PubDate: time.Now()})
	}
	return out, nil
}
func (s scorecardSource) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	return []byte(r.ID), nil
}
func (s scorecardSource) Check(_ context.Context, b []byte) (bool, error) {
	_, e := s.lookup(string(b))
	return e == nil, e
}
func (s scorecardSource) lookup(id string) (ScorecardFile, error) {
	files, e := s.files()
	if e != nil {
		return ScorecardFile{}, e
	}
	for _, f := range files {
		if scorecardReleaseID(f) == id {
			return f, nil
		}
	}
	return ScorecardFile{}, fmt.Errorf("unknown scorecard release")
}
func (s scorecardSource) Open(_ context.Context, id string, b []byte) (core.MediaSource, error) {
	if string(b) != id {
		return nil, fmt.Errorf("release mismatch")
	}
	entry, e := s.lookup(id)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(entry.Path)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	return &scorecardMedia{File: f, id: id, size: st.Size()}, nil
}
func (scorecardSource) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

type scorecardMedia struct {
	*os.File
	id   string
	size int64
}

func (m *scorecardMedia) Size() int64                                  { return m.size }
func (m *scorecardMedia) Name() string                                 { return filepath.Base(m.File.Name()) }
func (m *scorecardMedia) ReleaseID() string                            { return m.id }
func parityIndexer(_ core.Indexer, dir string) core.Indexer            { return scorecardSource{dir} }
func parityHealth(_ core.HealthChecker, dir string) core.HealthChecker { return scorecardSource{dir} }
func parityEngine(_ core.Engine, dir string) core.Engine               { return scorecardSource{dir} }

// VerifyScorecardFiles rejects missing, duplicated, or changed lab files before serving.
func VerifyScorecardFiles(files []ScorecardFile) error {
	if len(files) == 0 {
		return fmt.Errorf("no scorecard files")
	}
	seen := map[string]bool{}
	for _, entry := range files {
		key := scorecardReleaseID(entry)
		if entry.TMDB <= 0 || seen[key] || !filepath.IsAbs(entry.Path) || len(entry.SHA256) != 64 {
			return fmt.Errorf("invalid or duplicate scorecard entry %s", key)
		}
		seen[key] = true
		f, e := os.Open(entry.Path)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return e
		}
		if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), entry.SHA256) {
			return fmt.Errorf("sha256 mismatch for %s", entry.Path)
		}
	}
	return nil
}
