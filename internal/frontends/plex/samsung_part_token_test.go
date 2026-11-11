package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

// samsungAuth builds a real auth.Service with the owner and the given remote
// shared friends, each restricted to library 1 (Movies).
func samsungAuth(t *testing.T, friends ...plextv.Friend) *auth.Service {
	t.Helper()
	db, err := store.OpenPath(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := auth.New(context.Background(), db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.BootstrapOwner(ctx, config.Plex{OwnerPlexID: 42, OwnerUsername: "owner", Token: "server-token"}); err != nil {
		t.Fatal(err)
	}
	all := append([]plextv.Friend{{Owned: true, Token: "owner-access", PlexID: 42}}, friends...)
	if err := s.SyncFriends(ctx, all); err != nil {
		t.Fatal(err)
	}
	return s
}

// sharedFriend is a remote viewer shared only Movies (library 1).
func sharedFriend(plexID int64, username, token string) plextv.Friend {
	return plextv.Friend{PlexID: plexID, Username: username, Title: username, Token: token, Sections: []string{"1"}}
}

func samsungIdentity(a *auth.Service) func(*testServerDeps) {
	return func(d *testServerDeps) { d.Host.Identity = frontend.BindAuth(a) }
}

const (
	samsungFriendToken  = "friend-access"
	samsungSecondToken  = "second-access"
	samsungSecondPlexID = 200
)

// samsungDelegation fetches a transient delegation token the way Plex TV apps
// do before putting it on media URLs.
func samsungDelegation(t *testing.T, f *fixture, account string) string {
	t.Helper()
	rec := f.request(t, http.MethodGet, "/security/token", map[string]string{
		"X-Plex-Token": account,
		"Accept":       "application/json",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("security/token: %d %s", rec.Code, rec.Body.String())
	}
	tok := containerAttrs(t, rec.Body.Bytes())["token"]
	if tok == "" {
		t.Fatalf("no delegation token: %s", rec.Body.String())
	}
	return tok
}

// samsungPartPath is the part URL Ether renders for the movie (fightClub).
func samsungPartPath() string { return "/library/parts/1000000000000550/1759276800/file.mkv" }

// samsungDecision makes the TV's decision with Plex client headers, which opens
// a broker session under that client identity.
func samsungDecision(t *testing.T, f *fixture, token, clientID, session string) {
	t.Helper()
	dec := f.request(t, http.MethodGet, "/video/:/transcode/universal/decision?"+issue94Query(session, 2000), map[string]string{
		"X-Plex-Token":              token,
		"X-Plex-Client-Identifier":  clientID,
		"X-Plex-Product":            "Plex for Samsung",
		"X-Plex-Session-Identifier": session,
		"Accept":                    "application/json",
	})
	if dec.Code != http.StatusOK {
		t.Fatalf("decision: %d %s", dec.Code, dec.Body.String())
	}
}

// samsungStop ends the session the decision opened (the recorded sequence shows
// the TV stopping before it falls back to headerless part reads).
func samsungStop(t *testing.T, f *fixture, token, clientID, session string) {
	t.Helper()
	rec := f.request(t, http.MethodGet, "/video/:/transcode/universal/stop?session="+session, map[string]string{
		"X-Plex-Token":              token,
		"X-Plex-Client-Identifier":  clientID,
		"X-Plex-Session-Identifier": session,
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body.String())
	}
}

// samsungHeaderless does a clientless media read carrying only the token.
func samsungHeaderless(t *testing.T, f *fixture, method, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	return samsungHeaderlessFrom(t, f, method, token, path, "203.0.113.9:52000")
}

// samsungHeaderlessFrom is samsungHeaderless from a chosen network peer; the
// synthetic device identity is keyed on the viewer, token and peer.
func samsungHeaderlessFrom(t *testing.T, f *fixture, method, token, path, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path+"?X-Plex-Token="+token, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// TestSamsungRecordedSequence reproduces the Tizen 10 AVPlay flow end to end:
// a shared remote viewer obtains a delegation token, makes the decision with
// its Plex client headers, the TV stops that session and falls back to direct
// reads, and then re-fetches the part with ONLY ?X-Plex-Token and no client
// headers. PMS serves those reads; Ether must serve them too, accounting for
// exactly one session no matter how many range reads arrive. This is the
// v0.3.9 #424 client-compatibility invariant.
func TestSamsungRecordedSequence(t *testing.T) {
	a := samsungAuth(t, sharedFriend(100, "friend", samsungFriendToken))
	f, m := issue94Fixture(t, true, "open", samsungIdentity(a))
	defer f.srv.Close()
	partURL := samsungPartPath()
	delegation := samsungDelegation(t, f, samsungFriendToken)

	samsungDecision(t, f, samsungFriendToken, "samsung-tv", "samsung-sess")
	samsungStop(t, f, samsungFriendToken, "samsung-tv", "samsung-sess")
	if got := len(m.Sessions()); got != 0 {
		t.Fatalf("session survived stop: %d", got)
	}

	// The player re-fetches the part with ONLY the token and no headers. GET
	// and HEAD serve, for the delegation token and the account token alike,
	// and repeated reads reuse the one accounted session they open.
	for _, token := range []string{delegation, samsungFriendToken} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := samsungHeaderless(t, f, method, token, partURL)
			if rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
				t.Fatalf("%s part fetch with %q: %d %s", method, token, rec.Code, rec.Body.String())
			}
		}
	}
	if got := len(m.Sessions()); got != 1 {
		t.Fatalf("headerless reads left %d sessions, want exactly 1", got)
	}
}

// TestSamsungHeaderlessWithoutLiveSessionIsAccounted asserts a clientless read
// with no live session still opens a real, accounted session (rather than
// streaming outside the broker), and that a distinct viewer or credential is a
// distinct session.
func TestSamsungHeaderlessWithoutLiveSessionIsAccounted(t *testing.T) {
	a := samsungAuth(t,
		sharedFriend(100, "friend", samsungFriendToken),
		sharedFriend(samsungSecondPlexID, "second", samsungSecondToken),
	)
	f, m := issue94Fixture(t, true, "open", samsungIdentity(a))
	defer f.srv.Close()
	partURL := samsungPartPath()

	friendDelegation := samsungDelegation(t, f, samsungFriendToken)
	secondDelegation := samsungDelegation(t, f, samsungSecondToken)

	// One viewer, one token, one session -- however many reads arrive.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if rec := samsungHeaderless(t, f, method, friendDelegation, partURL); rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
			t.Fatalf("%s reads: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	if got := len(m.Sessions()); got != 1 {
		t.Fatalf("first viewer left %d sessions, want 1", got)
	}

	// A second viewer with its own delegation token is a separate session.
	if rec := samsungHeaderless(t, f, http.MethodGet, secondDelegation, partURL); rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		t.Fatalf("second viewer read: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(m.Sessions()); got != 2 {
		t.Fatalf("second viewer did not count separately: %d sessions", got)
	}

	// Protection is preserved: no token is still refused.
	req := httptest.NewRequest(http.MethodGet, partURL, nil)
	req.RemoteAddr = "203.0.113.9:52000"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless part fetch: %d", rec.Code)
	}
}

// titleIndexer returns one release named after the requested title, so any
// catalogue title with a resolvable source is playable in the fixture.
type titleIndexer struct{}

func (titleIndexer) Name() string { return "offline" }

func (titleIndexer) Search(_ context.Context, req core.SearchRequest) ([]core.Release, error) {
	year := req.Year
	if year == 0 {
		year = 2020
	}
	return []core.Release{{
		ID: "src", GUID: "src", Indexer: "offline",
		Name:    strings.TrimSpace(req.Title) + "." + strconv.Itoa(year) + ".1080p.WEB-DL.H264-GRP",
		Size:    8000 << 20,
		PubDate: time.Now().Add(-24 * time.Hour),
		NZBURL:  "offline",
	}}, nil
}
func (titleIndexer) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	return []byte(r.ID), nil
}

// anyTitleFixture is issue94Fixture with an indexer that can satisfy any title,
// so a viewer can hold two distinct playable sessions at once.
func anyTitleFixture(t *testing.T, opts ...func(*testServerDeps)) (*fixture, *playback.Manager) {
	t.Helper()
	cfg := config.Defaults(t.TempDir())
	cfg.Quality.Default = "test"
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "test", MaxHeight: 1080}}
	cfg.Quality.Rules = nil
	cfg.Quality.Remote = ""
	rs := resolver.New(titleIndexer{}, nil, nil, cfg, resolver.Options{})
	t.Cleanup(func() { rs.Close() })
	fixtureOpts := append([]func(*testServerDeps){func(d *testServerDeps) { d.DisableEnrichment = true }}, opts...)
	f := newFixture(fixtureOpts...)
	tr := &issue94Transcoder{newFakeTranscoder(core.Decision{})}
	mgr, err := playback.New(playback.Deps{
		Catalog: f.catalog, Resolver: rs, Engine: issue94Engine{}, Transcoder: tr,
		SelectProfile: func(c core.ClientCaps, u *core.User, _ string) core.QualityProfile { return rs.ProfileFor(c, u) },
		NativeProbe: func(_ context.Context, _ core.MediaSource) (*core.MediaInfo, error) {
			info := sampleInfo
			info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
			return &info, nil
		},
	}, cfg, playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.setPlayback(mgr)
	f.srv.setTranscoder(tr)
	t.Cleanup(func() { f.srv.Close(); mgr.Close() })
	return f, mgr
}

// TestSamsungHeaderlessRespectsStreamLimit asserts the synthetic device does
// not bypass the viewer's stream limit. The limit counts (viewer, device), so a
// second independent native player -- a different address, hence a different
// synthetic device -- wanting a second title is refused at the cap.
func TestSamsungHeaderlessRespectsStreamLimit(t *testing.T) {
	a := samsungAuth(t, sharedFriend(100, "friend", samsungFriendToken))
	ctx := context.Background()
	viewer, err := a.Authenticate(ctx, samsungFriendToken)
	if err != nil {
		t.Fatal(err)
	}
	limited := *viewer
	limited.StreamLimit = 1
	if err := a.UpdateUser(ctx, limited); err != nil {
		t.Fatal(err)
	}

	f, m := anyTitleFixture(t, samsungIdentity(a))
	defer f.srv.Close()
	delegation := samsungDelegation(t, f, samsungFriendToken)

	if rec := samsungHeaderlessFrom(t, f, http.MethodGet, delegation, samsungPartPath(), "203.0.113.9:52000"); rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		t.Fatalf("first title: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(m.Sessions()); got != 1 {
		t.Fatalf("first title left %d sessions, want 1", got)
	}

	// A second title from a different player wants a second stream: refused.
	alienPart := "/library/parts/1000000000000348/1759276800/file.mkv"
	rec := samsungHeaderlessFrom(t, f, http.MethodGet, delegation, alienPart, "198.51.100.7:52000")
	if rec.Code == http.StatusOK || rec.Code == http.StatusPartialContent {
		t.Fatalf("second title bypassed the stream limit: %d", rec.Code)
	}
	if got := len(m.Sessions()); got != 1 {
		t.Fatalf("refused title opened a session: %d", got)
	}
}
