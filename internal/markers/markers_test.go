package markers

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSkipSwitchesHideIntroAndCredits(t *testing.T) {
	info := core.MediaInfo{Duration: 20 * time.Minute, Chapters: []core.Chapter{
		{Title: "Intro", Start: time.Second, End: 30 * time.Second},
		{Title: "Recap", Start: 0, End: time.Second},
		{Title: "Credits", Start: 18 * time.Minute, End: 20 * time.Minute},
	}}
	key := core.EpisodeKey(1, 1, 1)
	introOff := New(Options{Config: func() Lookup { return Lookup{IntroOff: true} }})
	got, err := introOff.Markers(context.Background(), key, info)
	if err != nil || len(got) != 1 || got[0].Type != "credits" || got[0].Source != "chapter" {
		t.Fatalf("intro off: %+v %v", got, err)
	}
	creditsOff := New(Options{Config: func() Lookup { return Lookup{CreditsOff: true} }})
	got, err = creditsOff.Markers(context.Background(), key, info)
	if err != nil || len(got) != 2 || got[0].Type != "recap" || got[1].Type != "intro" {
		t.Fatalf("credits off: %+v %v", got, err)
	}
}

func TestRemoteDurationCacheAndChapters(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("tmdb_id") != "1396" || q.Get("season") != "1" || (q.Get("duration_ms") != "1200000" && q.Get("duration_ms") != "1201000") || q.Get("merge_unknown") != "false" {
			t.Error(q)
		}
		io.WriteString(w, `{"intro":[{"start_ms":null,"end_ms":60000}],"credits":[{"start_ms":1100000,"end_ms":null}],"recap":[{"start_ms":-1,"end_ms":9000}]}`)
	}))
	defer srv.Close()
	s := New(Options{DB: db, Enabled: true, BaseURL: srv.URL})
	info := core.MediaInfo{Duration: 20 * time.Minute}
	key := core.EpisodeKey(1396, 1, 1)
	m, err := s.Markers(ctx, key, info)
	if err != nil || len(m) != 2 || !m[1].Final {
		t.Fatalf("markers %+v %v", m, err)
	}
	_, err = s.Markers(ctx, key, info)
	if err != nil || calls != 1 {
		t.Fatalf("cache calls %d %v", calls, err)
	}
	info.Duration += time.Second
	_, err = s.Markers(ctx, key, info)
	if err != nil || calls != 2 {
		t.Fatal("runtime cache collision")
	}
	info.Chapters = []core.Chapter{{Title: "Intro", Start: time.Second, End: 30 * time.Second}}
	m = FromChapters(info)
	if len(m) != 1 || m[0].StartMS != 1000 {
		t.Fatal(m)
	}
}

type markerCountingTransport struct{ calls int }

func (t *markerCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected network request")
}

func TestCachedMarkersNeverFetchesOnMissOrHit(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	transport := &markerCountingTransport{}
	s := New(Options{DB: db, Enabled: true, HTTPClient: &http.Client{Transport: transport}})
	key := core.EpisodeKey(42, 1, 2)
	info := core.MediaInfo{Duration: 10 * time.Minute, Chapters: []core.Chapter{{Title: "Intro", Start: time.Second, End: 20 * time.Second}}}
	got, err := s.CachedMarkers(ctx, key, info)
	if err != nil || len(got) != 1 || got[0].Type != "intro" || transport.calls != 0 {
		t.Fatalf("cache miss: %+v, %v, calls=%d", got, err, transport.calls)
	}
	if err := s.Put(ctx, key, info.Duration, []Marker{{Type: "credits", StartMS: 500000, EndMS: 590000}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.CachedMarkers(ctx, key, info)
	if err != nil || len(got) != 2 || got[1].Type != "credits" || transport.calls != 0 {
		t.Fatalf("cache hit: %+v, %v, calls=%d", got, err, transport.calls)
	}
}

type blockingRunner struct{ started chan struct{} }

func (r blockingRunner) Run(ctx context.Context, _ string, _ []string, _ io.Writer, _ io.Writer) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}
func TestMatrixStudioCardIsNotAnIntro(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tmdb_id") != "603" || r.URL.Query().Get("season") != "" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"group_duration_ms":8180662,"intro":[{"start_ms":null,"end_ms":40000,"confidence":0.1,"submission_count":1}],"credits":[{"start_ms":7726802,"end_ms":null}]}`)
	}))
	defer srv.Close()
	s := New(Options{Enabled: true, BaseURL: srv.URL})
	info := core.MediaInfo{Duration: 8180662 * time.Millisecond}
	got, err := s.Markers(ctx, core.MovieKey(603), info)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.Type == "intro" {
			t.Fatalf("studio card offered as an intro: %+v", m)
		}
	}
	if len(got) != 1 || got[0].Type != "credits" || got[0].Source != "theintrodb" {
		t.Fatalf("credits %+v", got)
	}
	// A title sequence that starts after the cards is still an intro.
	later := []Marker{{Type: "intro", StartMS: 120_000, EndMS: 180_000, Source: "theintrodb"}}
	if filtered := dropMovieBumpers(core.MovieKey(603), later); len(filtered) != 1 {
		t.Fatalf("later movie intro dropped: %+v", filtered)
	}
	// The same head range stays on an episode.
	episode := dropMovieBumpers(core.EpisodeKey(1, 1, 1), []Marker{{Type: "intro", StartMS: 0, EndMS: 40_000}})
	if len(episode) != 1 {
		t.Fatalf("episode intro dropped: %+v", episode)
	}
	chapters := core.MediaInfo{Duration: info.Duration, Chapters: []core.Chapter{
		{Title: "Intro", Start: 0, End: 23 * time.Second},
		{Title: "Credits", Start: 7726802 * time.Millisecond, End: info.Duration},
	}}
	got, err = s.CachedMarkers(ctx, core.MovieKey(603), chapters)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.Type == "intro" {
			t.Fatalf("chapter studio card offered as an intro: %+v", m)
		}
	}
}

func TestMergePreservesMultipleRemoteRanges(t *testing.T) {
	remote := []Marker{{Type: "intro", StartMS: 1, EndMS: 2}, {Type: "intro", StartMS: 3, EndMS: 4}, {Type: "credits", StartMS: 5, EndMS: 6}}
	if got := merge(nil, remote); len(got) != 3 {
		t.Fatalf("lost ranges: %+v", got)
	}
	chapter := []Marker{{Type: "intro", StartMS: 9, EndMS: 10}}
	got := merge(chapter, remote)
	if len(got) != 2 || got[0].StartMS != 9 || got[1].Type != "credits" {
		t.Fatalf("source priority: %+v", got)
	}
}
