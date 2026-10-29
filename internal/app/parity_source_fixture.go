//go:build plexparity

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
func (paritySource) Search(_ context.Context, q core.SearchRequest) ([]core.Release, error) {
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
	return []core.Release{{ID: fmt.Sprintf("fixture-%d-%d-%d", q.IDs.TMDB, q.Season, q.Episode), GUID: "fixture", Indexer: "parity fixture", Name: name, Size: 4 << 30, TitleIDs: q.IDs, Protocol: protocol, PubDate: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Info: info}}, nil
}
func (paritySource) FetchNZB(context.Context, core.Release) ([]byte, error) {
	return []byte("parity-media"), nil
}
func (paritySource) Check(context.Context, []byte) (bool, error) { return true, nil }
func (s paritySource) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	filename := "parity.mp4"
	if strings.HasPrefix(id, "fixture-10004-") {
		filename = "parity-tracks.mp4"
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

func (p *parityMedia) Size() int64                       { return p.size }
func (p *parityMedia) Name() string                      { return "fixture.mp4" }
func (p *parityMedia) ReleaseID() string                 { return p.id }
func parityIndexer(core.Indexer) core.Indexer            { return paritySource{} }
func parityHealth(core.HealthChecker) core.HealthChecker { return paritySource{} }
func parityEngine(_ core.Engine, dir string) core.Engine { return paritySource{dir: dir} }
