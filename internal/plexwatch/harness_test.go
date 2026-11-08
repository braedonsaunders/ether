package plexwatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

const (
	viewerToken = "plex-token-viewer-aaa"
	ownerToken  = "plex-token-owner-must-not-leak"
)

// memGUID is an in-memory verified identity table.
type memGUID struct {
	mu      sync.Mutex
	forward map[core.RatingKey]string
	reverse map[string]core.RatingKey
	block   chan struct{}
	entered chan struct{}
}

func (m *memGUID) Resolve(ctx context.Context, key core.RatingKey) (string, error) {
	m.mu.Lock()
	block, entered := m.block, m.entered
	m.mu.Unlock()
	if block != nil {
		if entered != nil {
			close(entered)
		}
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.forward[key]
	if !ok || g == "" {
		return "", core.ErrNotFound
	}
	return g, nil
}

func (m *memGUID) ExactKey(_ context.Context, guid string) (core.RatingKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.reverse[guid]
	if !ok {
		return 0, core.ErrNotFound
	}
	return k, nil
}

func (m *memGUID) VerifiedRange(_ context.Context, after, below core.RatingKey, limit int) ([]plexguid.Verified, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []core.RatingKey
	for k := range m.forward {
		if k > after && k < below {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]plexguid.Verified, 0, len(keys))
	for _, k := range keys {
		out = append(out, plexguid.Verified{Key: k, GUID: m.forward[k]})
	}
	return out, nil
}

func (m *memGUID) add(key core.RatingKey, hex string) {
	kind := map[core.KeyKind]string{core.KindMovie: "movie", core.KindShow: "show", core.KindEpisode: "episode"}[key.Kind()]
	g := "plex://" + kind + "/" + hex
	m.mu.Lock()
	m.forward[key] = g
	m.reverse[g] = key
	m.mu.Unlock()
}

type lockedTime struct {
	mu sync.Mutex
	t  time.Time
}

func (l *lockedTime) Now() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.t
}

func (l *lockedTime) Add(d time.Duration) {
	l.mu.Lock()
	l.t = l.t.Add(d)
	l.mu.Unlock()
}

// remoteTitle is one title's state on the fake Discover account.
type remoteTitle struct {
	typ          string
	viewCount    int
	lastViewedAt int64
	viewOffset   int64
	viewState    string
	rating       int
	leaves       int
}

// fakePlex serves plex.tv PIN sign-in and Discover userState and actions.
type fakePlex struct {
	mu        sync.Mutex
	clock     func() time.Time
	accountID int64
	titles    map[string]*remoteTitle
	// failNext answers the next action with this status after applying it
	// (applyThenFail) or without applying it.
	failNext      int
	applyThenFail bool
	retryAfter    string
	rejectAll     int
	rejected      int
	writes        []string
	reads         [][]string
	tokens        map[string]bool
	// consentOff is the account's "Sync My Watch State and Ratings" set
	// off; consentReads counts reads of it.
	consentOff   bool
	consentReads int
}

func newFakePlex(account int64, clock func() time.Time) *fakePlex {
	return &fakePlex{accountID: account, clock: clock, titles: map[string]*remoteTitle{}, tokens: map[string]bool{}}
}

// play records a stopped position as a Plex app would.
func (f *fakePlex) play(key, typ string, offsetMS int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.title(key, typ)
	t.viewOffset = offsetMS
	t.viewState = "progress"
	t.lastViewedAt = at.Unix()
}

// dismiss removes key from Continue Watching as a Plex app would.
func (f *fakePlex) dismiss(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles[key].viewState = "ignore"
}

func (f *fakePlex) state(key string) remoteTitle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.titles[key]
}

func (f *fakePlex) setConsent(on bool) {
	f.mu.Lock()
	f.consentOff = !on
	f.mu.Unlock()
}

func (f *fakePlex) title(key, typ string) *remoteTitle {
	t := f.titles[key]
	if t == nil {
		t = &remoteTitle{typ: typ}
		f.titles[key] = t
	}
	return t
}

// watch, unwatch and rate change state as if done in Plex Web.
func (f *fakePlex) watch(key, typ string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.title(key, typ)
	t.viewCount++
	t.lastViewedAt = at.Unix()
}

func (f *fakePlex) unwatch(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles[key].viewCount = 0
}

func (f *fakePlex) rate(key, typ string, r int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title(key, typ).rating = r
}

func (f *fakePlex) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.writes
	f.writes = nil
	return w
}

func (f *fakePlex) takeReads() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reads
	f.reads = nil
	return r
}

func (f *fakePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("X-Plex-Token") != "" {
		http.Error(w, "token in query", http.StatusBadRequest)
		return
	}
	tok := r.Header.Get("X-Plex-Token")
	f.mu.Lock()
	defer f.mu.Unlock()
	if tok != "" {
		f.tokens[tok] = true
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/api/v2/pins":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"code":"CODE123","expiresIn":600}`))
		return
	case r.Method == http.MethodGet && path == "/api/v2/pins/42":
		_, _ = w.Write([]byte(`{"id":42,"code":"CODE123","authToken":"` + viewerToken + `"}`))
		return
	}
	if tok != viewerToken || f.rejectAll != 0 {
		code := f.rejectAll
		if code == 0 {
			code = http.StatusUnauthorized
		}
		f.rejected++
		w.WriteHeader(code)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/api/v2/user":
		_, _ = w.Write([]byte(`{"id":` + strconv.FormatInt(f.accountID, 10) + `,"username":"viewer"}`))
	case r.Method == http.MethodGet && path == "/api/v2/user/view_state_sync":
		f.consentReads++
		_, _ = w.Write([]byte(`{"consent":` + strconv.FormatBool(!f.consentOff) + `,"updatedAt":"2022-07-29T04:49:30Z","deletionRequestedAt":null}`))
	case r.Method == http.MethodGet && path == "/timeline":
		// Discover takes positions only as a GET; it never marks watched.
		q := r.URL.Query()
		key := q.Get("ratingKey")
		ms, err1 := strconv.ParseInt(q.Get("time"), 10, 64)
		dur, err2 := strconv.ParseInt(q.Get("duration"), 10, 64)
		if key == "" || q.Get("key") != "/library/metadata/"+key || q.Get("state") != "stopped" || err1 != nil || err2 != nil || dur <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.writes = append(f.writes, "timeline "+key+" "+q.Get("time"))
		t := f.title(key, "movie")
		t.viewOffset = ms
		t.viewState = "progress"
		t.lastViewedAt = f.clock().Unix()
		_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{
			"playbackState": "progress", "viewCount": t.viewCount, "viewOffset": t.viewOffset}})
	case r.Method != http.MethodGet && (path == "/timeline" || path == "/:/timeline"):
		http.NotFound(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/library/metadata/") && strings.HasSuffix(path, "/userState"):
		keys := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/library/metadata/"), "/userState"), ",")
		f.reads = append(f.reads, keys)
		var items []map[string]any
		for _, k := range keys {
			t := f.titles[k]
			if t == nil {
				continue
			}
			item := map[string]any{"ratingKey": k, "type": t.typ, "viewCount": t.viewCount}
			if t.lastViewedAt > 0 {
				item["lastViewedAt"] = t.lastViewedAt
			}
			if t.rating > 0 {
				item["userRating"] = float64(t.rating)
			}
			if t.viewOffset > 0 {
				item["viewOffset"] = t.viewOffset
			}
			if t.viewState != "" {
				item["viewState"] = t.viewState
			}
			if t.typ == "show" {
				item["viewedLeafCount"] = t.leaves
			}
			items = append(items, item)
		}
		mc := map[string]any{"size": len(items)}
		if len(keys) == 1 && len(items) == 1 {
			mc["UserState"] = items[0]
		} else if len(items) > 0 {
			mc["UserState"] = items
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": mc})
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/actions/"):
		q := r.URL.Query()
		key := q.Get("key")
		if q.Get("identifier") != discoverIdentifier || key == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		action := strings.TrimPrefix(path, "/actions/")
		f.writes = append(f.writes, action+" "+key+" "+q.Get("rating"))
		fail := f.failNext
		if fail != 0 {
			f.failNext = 0
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			if !f.applyThenFail {
				w.WriteHeader(fail)
				return
			}
		}
		t := f.title(key, "movie")
		switch action {
		case "scrobble":
			t.viewCount++
			t.lastViewedAt = f.clock().Unix()
			t.viewOffset, t.viewState = 0, ""
		case "unscrobble":
			t.viewCount = 0
			t.viewOffset, t.viewState = 0, ""
		case "removeFromContinueWatching":
			t.viewState = "ignore"
		case "rate":
			n, _ := strconv.Atoi(q.Get("rating"))
			if n < 0 {
				n = 0
			}
			t.rating = n
		}
		if fail != 0 {
			w.WriteHeader(fail)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

type harness struct {
	svc   *Service
	st    *state.State
	plex  *fakePlex
	guid  *memGUID
	logs  *bytes.Buffer
	clock *lockedTime
	db    *sql.DB
}

func newHarness(t *testing.T, account int64) *harness {
	t.Helper()
	db := store.OpenTest(t)
	clock := &lockedTime{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	st := state.New(db, state.Options{Now: clock.Now})
	st.SetJournalGate(func(ctx context.Context, tx *sql.Tx, userID int64) (int64, bool, error) {
		var enabled, gen int64
		err := tx.QueryRowContext(ctx, `SELECT a.direction, f.generation
			FROM plexwatch_account a JOIN plexwatch_fence f ON f.user_id=a.user_id
			WHERE a.user_id=? AND a.revoked=0 AND a.token<>'' AND a.token_generation=f.generation`, userID).Scan(&enabled, &gen)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return gen, enabled&1 != 0, nil
	})
	guid := &memGUID{forward: map[core.RatingKey]string{}, reverse: map[string]core.RatingKey{}}
	plex := newFakePlex(account, clock.Now)
	srv := httptest.NewServer(plex)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	svc, err := New(Options{
		DB: db, State: st, GUID: guid, BaseURL: srv.URL, NodeID: "node-a",
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.pace = 0
	t.Cleanup(func() { _ = svc.Close() })
	return &harness{svc: svc, st: st, plex: plex, guid: guid, logs: &logs, clock: clock, db: db}
}

func (h *harness) connect(t *testing.T, user, plex int64, direction int) Status {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.BeginPIN(ctx, user, plex); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.FinishPIN(ctx, user, plex)
	if err != nil {
		t.Fatal(err)
	}
	if direction != 0 {
		if st, err = h.svc.SetDirection(ctx, user, plex, direction, 10); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// deliverAll runs the sender until nothing is claimable now.
func (h *harness) deliverAll(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		more, err := h.svc.DeliverOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("delivery did not settle")
}

func (h *harness) importNow(t *testing.T, user, plex int64) Status {
	t.Helper()
	st, err := h.svc.ImportCurrent(context.Background(), user, plex)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (h *harness) local(t *testing.T, user int64, key core.RatingKey) core.WatchStatus {
	t.Helper()
	ws, err := h.st.Get(context.Background(), user, key)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
