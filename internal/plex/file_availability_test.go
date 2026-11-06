package plex

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/playback"
)

func (p *fakePlayback) CheckFileAvailability(_ context.Context, _ *core.User, key core.RatingKey, _ core.ClientCaps, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.failures[key]
	return err == nil, err
}

type fileProbeResolver struct{}

func (fileProbeResolver) Resolve(_ context.Context, r core.ResolveRequest) (*core.Resolution, error) {
	return &core.Resolution{Title: r.Title.Key, Release: core.Release{ID: "checked", Name: "Fjord.Harbor.1999.1080p.mkv"}, NZB: []byte("checked")}, nil
}
func (fileProbeResolver) MarkBad(context.Context, string, string) error          { return nil }
func (fileProbeResolver) Available(context.Context, core.RatingKey) (bool, bool) { return false, false }

type fileProbeEngine struct {
	dead          bool
	opens, closes atomic.Int32
}

func (e *fileProbeEngine) Open(context.Context, string, []byte) (core.MediaSource, error) {
	e.opens.Add(1)
	if e.dead {
		return nil, core.ErrNotAvailable
	}
	return &fileProbeSource{issue94Source: issue94Source{Reader: bytes.NewReader([]byte("live-file")), id: "checked"}, closed: &e.closes}, nil
}
func (*fileProbeEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }

type fileProbeSource struct {
	issue94Source
	closed *atomic.Int32
}

func (s *fileProbeSource) Close() error { s.closed.Add(1); return nil }

func TestMetadataFileChecksUseSourceWithoutPlaybackAdmission(t *testing.T) {
	for _, dead := range []bool{false, true} {
		f := newFixture(func(d *Deps) { d.DisableEnrichment = true })
		cfg := config.Defaults(t.TempDir())
		cfg.Cache.DisableSpeculation = true
		engine := &fileProbeEngine{dead: dead}
		m, err := playback.New(playback.Deps{Catalog: f.catalog, Resolver: fileProbeResolver{}, Engine: engine, Transcoder: f.trans, SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return core.QualityProfile{} }}, cfg, playback.Options{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		f.srv.d.Playback = m
		func() {
			defer f.srv.Close()
			defer m.Close()
			for _, option := range []string{"", "checkFiles=1", "asyncCheckFiles=1", "checkFileAvailability=1"} {
				r := f.get(t, "/library/metadata/1000000000000550?"+option, jsonAccept...)
				if r.Code != 200 {
					t.Fatalf("%s: %d %s", option, r.Code, r.Body.String())
				}
				mc := decodeJSON(t, r)
				part := mc["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)
				for _, name := range []string{"exists", "accessible"} {
					v, present := part[name]
					if option == "" {
						if present {
							t.Fatalf("ordinary read has %s", name)
						}
					} else if !present || v != !dead {
						t.Fatalf("dead=%v option=%s %s=%v", dead, option, name, v)
					}
				}
				if option == "" && engine.opens.Load() != 0 {
					t.Fatal("ordinary read opened source")
				}
				if len(m.Sessions()) != 0 {
					t.Fatal("file check admitted playback")
				}
			}
			r := f.get(t, "/status/sessions", jsonAccept...)
			if decodeJSON(t, r)["size"] != float64(0) {
				t.Fatal("file check created Now Playing")
			}
			if !dead && engine.closes.Load() != engine.opens.Load() {
				t.Fatal("probe source not closed")
			}
		}()
	}
}
