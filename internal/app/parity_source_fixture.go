//go:build plexparity && !scorecard

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Fixture sources feed the real resolver and playback manager without network.
type paritySource struct{ dir string }

func (paritySource) Name() string { return "parity fixture" }
func (s paritySource) Search(_ context.Context, q core.SearchRequest) ([]core.Release, error) {
	if rels := s.labReleases(q); len(rels) > 0 {
		return rels, nil
	}
	return s.synthetic(q), nil
}

func (s paritySource) labReleases(q core.SearchRequest) []core.Release {
	files, err := readLabMedia(s.dir)
	if err != nil || len(files) == 0 {
		return nil
	}
	var out []core.Release
	for _, f := range files {
		if f.TMDB != q.IDs.TMDB || f.Season != q.Season || f.Episode != q.Episode {
			continue
		}
		if q.Type == core.TypeMovie && f.Episode != 0 {
			continue
		}
		if q.Type == core.TypeEpisode && f.Episode == 0 {
			continue
		}
		size := int64(0)
		info := core.ReleaseInfo{Title: q.Title, Year: q.Year, Source: core.SourceWEBDL, Container: "mkv"}
		if f.Media != nil {
			size = f.Media.Size
			if f.Media.Container != "" {
				info.Container = f.Media.Container
			}
			if v := f.Media.Video(); v != nil {
				info.Height = v.Height
				info.VideoCodec = v.Codec
			}
		}
		if q.Type == core.TypeEpisode {
			info.Seasons = []int{q.Season}
			info.Episodes = []int{q.Episode}
		}
		id := labReleaseID(f)
		out = append(out, core.Release{ID: id, GUID: id, Indexer: "lab media", Name: filepath.Base(f.Path), Size: size, TitleIDs: q.IDs, Info: info, PubDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	}
	return out
}

func (paritySource) synthetic(q core.SearchRequest) []core.Release {
	name := fmt.Sprintf("%s %d 1080p WEB-DL H264 AAC", q.Title, q.Year)
	if q.Type == core.TypeEpisode {
		name = fmt.Sprintf("%s S%02dE%02d 1080p WEB-DL H264 AAC", q.Title, q.Season, q.Episode)
	}
	info := core.ReleaseInfo{Title: q.Title, Year: q.Year, Height: 1080, Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "aac", Container: "mp4"}
	if q.Type == core.TypeEpisode {
		info.Seasons = []int{q.Season}
		info.Episodes = []int{q.Episode}
	}
	protocol := ""
	if q.IDs.TMDB == 10002 {
		// The fake debrid HTTP source serves local bytes; its release carries
		// the real protocol through the resolver and playback decision.
		protocol = "http"
	}
	return []core.Release{{ID: fmt.Sprintf("fixture-%d-%d-%d", q.IDs.TMDB, q.Season, q.Episode), GUID: "fixture", Indexer: "parity fixture", Name: name, Size: 4 << 30, TitleIDs: q.IDs, Protocol: protocol, PubDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Info: info}}
}
func (paritySource) FetchNZB(context.Context, core.Release) ([]byte, error) {
	return []byte("parity-media"), nil
}
func (paritySource) Check(context.Context, []byte) (bool, error) { return true, nil }
func (s paritySource) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	if strings.HasPrefix(id, "lab-") {
		files, err := readLabMedia(s.dir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if labReleaseID(f) != id {
				continue
			}
			file, err := os.Open(f.Path)
			if err != nil {
				return nil, err
			}
			stat, err := file.Stat()
			if err != nil {
				file.Close()
				return nil, err
			}
			return &parityMedia{File: file, size: stat.Size(), id: id}, nil
		}
		return nil, fmt.Errorf("unknown lab media")
	}
	filename := "parity.mp4"
	if strings.HasPrefix(id, "fixture-10004-") {
		filename = "parity-tracks.mp4"
	}
	if os.Getenv("ETHER_PARITY_EXPIRY_ONLY") == "1" {
		filename = "parity-expiry.mp4"
	}
	f, err := os.Open(filepath.Join(s.dir, filename))
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &parityMedia{File: f, size: stat.Size(), id: id}, nil
}
func (paritySource) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

type parityMedia struct {
	*os.File
	size int64
	id   string
}

// The expiry-only source is deliberately slow after its initial bytes so the
// real FFmpeg process remains observable while the idle reaper runs.
func (p *parityMedia) ReadAt(b []byte, off int64) (int, error) {
	if os.Getenv("ETHER_PARITY_EXPIRY_ONLY") != "1" {
		return p.File.ReadAt(b, off)
	}
	// ReadAt callers may request the whole source in one buffer, starting at
	// zero. Pace each chunk rather than only calls with a nonzero offset.
	const chunk = 64 * 1024
	n := 0
	for n < len(b) {
		end := n + chunk
		if end > len(b) {
			end = len(b)
		}
		if off+int64(n) >= 128*1024 {
			time.Sleep(50 * time.Millisecond)
		}
		read, err := p.File.ReadAt(b[n:end], off+int64(n))
		n += read
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (p *parityMedia) Size() int64                               { return p.size }
func (p *parityMedia) Name() string                              { return "fixture.mp4" }
func (p *parityMedia) ReleaseID() string                         { return p.id }
func (p *parityMedia) LocalFile() *os.File                       { return p.File }
func parityIndexer(core.Indexer, string) core.Indexer            { return paritySource{} }
func parityHealth(core.HealthChecker, string) core.HealthChecker { return paritySource{} }
func parityEngine(_ core.Engine, dir string) core.Engine         { return paritySource{dir: dir} }
