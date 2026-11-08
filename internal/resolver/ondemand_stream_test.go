package resolver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
)

// fakeAddon is a Stremio add-on whose stream links prepare the release on
// request (AltMount queues and imports an NZB on every /play): it records
// every link opened, so a test can count the releases Ether made it fetch.
type fakeAddon struct {
	srv    *httptest.Server
	mu     sync.Mutex
	opened []string
	fail   map[string]bool // answer 503, as an add-on whose import failed
	delay  time.Duration   // time the add-on takes to prepare a release
}

func newFakeAddon(t *testing.T) *fakeAddon {
	t.Helper()
	a := &fakeAddon{fail: map[string]bool{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/play/"):]
		a.mu.Lock()
		if !slices.Contains(a.opened, name) {
			a.opened = append(a.opened, name)
		}
		fail, delay := a.fail[name], a.delay
		a.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			http.Error(w, "no playable release found", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-0/8388608000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAddon) links() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.opened)
}

func (a *fakeAddon) release(id, group string) core.Release {
	return withParsed(core.Release{ID: id, GUID: "http:addon:" + id, Indexer: "Add-on", Name: "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-" + group,
		Size: 8000 << 20, PubDate: time.Now().Add(-30 * 24 * time.Hour), Protocol: core.ProtocolHTTP,
		StreamURL: a.srv.URL + "/play/" + id, StreamOrigin: a.srv.URL, StreamOnDemand: true})
}

// streamingFake answers like the indexer manager: results arrive through
// SearchWith, so the resolver's early discovery checks run as in production.
type streamingFake struct{ fakeIndexer }

func (f *streamingFake) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	r, err := f.Search(ctx, req)
	if err == nil && arrived != nil {
		arrived(r)
	}
	return r, err
}

func addonResolver(t *testing.T, a *fakeAddon, edit func(*config.Config)) *Service {
	t.Helper()
	inner := &streamingFake{fakeIndexer{releases: []core.Release{a.release("a", "AAA"), a.release("b", "BBB"), a.release("c", "CCC")}}}
	svc := debrid.NewService(config.Debrid{}, debrid.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	cfg := testCfg()
	cfg.Resolver.MaxCandidates = 3
	if edit != nil {
		edit(&cfg)
	}
	s := New(debrid.NewIndexer(inner, svc), fakeHealth{}, nil, cfg, Options{})
	t.Cleanup(func() { s.Close() })
	return s
}

// One Play opens exactly one add-on stream link while it is healthy, and
// the next only once that one has failed. Warm-ups and availability
// checks open none. Opening such a link makes the add-on fetch the release.
func TestOnDemandAddonStreamsOpenOneLinkPerPlay(t *testing.T) {
	play := func(s *Service, ctx context.Context) (*core.Resolution, error) {
		return s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	}
	lead := ""
	for _, race := range []int{0, 3} {
		a := newFakeAddon(t)
		s := addonResolver(t, a, func(c *config.Config) { c.Resolver.HealthRace = race })
		res, err := play(s, context.Background())
		if err != nil || res == nil {
			t.Fatalf("race %d: play: %v", race, err)
		}
		if got := a.links(); len(got) != 1 || got[0] != res.Release.ID {
			t.Fatalf("race %d: healthy play opened %v, want only %s", race, got, res.Release.ID)
		}
		lead = res.Release.ID
	}

	a := newFakeAddon(t)
	a.fail[lead] = true
	s := addonResolver(t, a, nil)
	res, err := play(s, context.Background())
	if err != nil || res == nil || res.Release.ID == lead {
		t.Fatalf("failover play: res=%+v err=%v", res, err)
	}
	if got := a.links(); !slices.Equal(got, []string{lead, res.Release.ID}) {
		t.Fatalf("failover opened %v, want %s then %s", got, lead, res.Release.ID)
	}

	for name, ctx := range map[string]context.Context{
		"warm-up":      func() context.Context { c, _ := core.WithSpeculative(context.Background()); return c }(),
		"prefetch":     core.WithUnattendedSearch(core.WithIndexerPurpose(context.Background(), core.IndexerPurposeWarmup)),
		"availability": core.WithBackgroundBudget(context.Background(), func(context.Context, string, string, int64) error { return nil }),
		"checkFiles":   core.WithoutOnDemandStreams(core.WithIndexerPurpose(context.Background(), core.IndexerPurposeWarmup)),
	} {
		a := newFakeAddon(t)
		s := addonResolver(t, a, nil)
		if _, err := play(s, ctx); err == nil {
			t.Fatalf("%s resolved an add-on stream it may not open", name)
		}
		if got := a.links(); len(got) != 0 {
			t.Fatalf("%s opened add-on links %v", name, got)
		}
		// A Play joining or following that work opens the link itself.
		if res, err := play(s, context.Background()); err != nil || res == nil {
			t.Fatalf("%s: play after it: %v", name, err)
		}
		if got := a.links(); len(got) != 1 {
			t.Fatalf("%s: play after it opened %v", name, got)
		}
	}
}
