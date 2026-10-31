package plexwatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

type memGUID struct {
	mu          sync.Mutex
	forward     map[core.RatingKey]string
	reverse     map[string]core.RatingKey
	ambiguous   bool
	block       chan struct{}
	entered     chan struct{}
	blockOnce   bool
	passedBlock bool
	resolveErr  error
	exactErr    error
}

func (m *memGUID) Resolve(ctx context.Context, key core.RatingKey) (string, error) {
	if m.block != nil {
		m.mu.Lock()
		wait := !m.blockOnce || !m.passedBlock
		if m.blockOnce {
			m.passedBlock = true
		}
		m.mu.Unlock()
		if wait {
			if m.entered != nil {
				select {
				case <-m.entered:
				default:
					close(m.entered)
				}
			}
			select {
			case <-m.block:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	g, ok := m.forward[key]
	if !ok || g == "" {
		return "", core.ErrNotFound
	}
	return g, nil
}

func (m *memGUID) ExactKey(_ context.Context, guid string) (core.RatingKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exactErr != nil {
		return 0, m.exactErr
	}
	if m.ambiguous {
		return 0, plexguid.ErrAmbiguous
	}
	k, ok := m.reverse[guid]
	if !ok {
		return 0, core.ErrNotFound
	}
	return k, nil
}

type fakeUpstream struct {
	mu          sync.Mutex
	accountID   int64
	eligible    int
	postStatus  int
	retryAfter  string
	posts       []postItem
	postUsers   []int64
	tokens      []string
	blocked     chan struct{}
	entered     chan struct{}
	redirect    bool
	authToken   string
	rejectToken string
	refuseToken string
	stateBody   string
	paths       []string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.URL.Query().Get("X-Plex-Token") != "" || r.URL.Query().Get("auth_token") != "" {
		http.Error(w, "token in query", http.StatusBadRequest)
		return
	}
	if f.redirect {
		http.Redirect(w, r, "https://evil.example/collect?X-Plex-Token=stolen", http.StatusFound)
		return
	}
	tok := r.Header.Get("X-Plex-Token")
	f.mu.Lock()
	if tok != "" {
		f.tokens = append(f.tokens, tok)
	}
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/pins":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"code":"CODE123","expiresIn":60}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/pins/42":
		_, _ = w.Write([]byte(`{"id":42,"code":"CODE123","authToken":"` + f.issuedToken() + `"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/user":
		if !f.tokenOK(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id := f.accountID
		_, _ = w.Write([]byte(`{"id":` + itoa(id) + `,"username":"viewer","restricted":false,"authToken":"` + f.issuedToken() + `"}`))
	case r.Method == http.MethodGet && len(r.URL.Path) > 20 && r.URL.Path[len(r.URL.Path)-16:] == "/view_state_sync":
		f.mu.Lock()
		refuse := f.refuseToken
		f.mu.Unlock()
		if refuse != "" && tok == refuse {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !f.tokenOK(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		code := f.eligible
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	case r.Method == http.MethodPost && r.URL.Path == "/state":
		if f.entered != nil {
			select {
			case <-f.entered:
			default:
				close(f.entered)
			}
		}
		if f.blocked != nil {
			<-f.blocked
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
		var payload struct {
			Items []postItem `json:"items"`
			User  int64      `json:"user"`
		}
		_ = json.Unmarshal(body, &payload)
		f.mu.Lock()
		f.postUsers = append(f.postUsers, payload.User)
		if len(payload.Items) == 1 {
			f.posts = append(f.posts, payload.Items[0])
		} else {
			f.posts = append(f.posts, postItem{})
		}
		status := f.postStatus
		retry := f.retryAfter
		reject := f.rejectToken
		f.mu.Unlock()
		if reject != "" && tok == reject {
			status = http.StatusUnauthorized
		}
		if status == 0 {
			status = http.StatusCreated
		}
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(status)
	case r.Method == http.MethodGet && len(r.URL.Path) > 6 && r.URL.Path[:7] == "/state/":
		if f.stateBody != "" {
			_, _ = w.Write([]byte(f.stateBody))
			return
		}
		_, _ = w.Write([]byte(`{"items":[],"pageInfo":{"endCursor":"cursor-1","hasNextPage":false}}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeUpstream) issuedToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authToken == "" {
		return viewerToken
	}
	return f.authToken
}

func (f *fakeUpstream) tokenOK(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tok == viewerToken || (f.authToken != "" && tok == f.authToken)
}

func itoa(n int64) string {
	return jsonNumber(n)
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type harness struct {
	svc       *Service
	st        *state.State
	fake      *fakeUpstream
	guid      *memGUID
	logs      *bytes.Buffer
	now       *lockedTime
	afterGate func()
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

func newHarness(t *testing.T, account int64) *harness {
	t.Helper()
	return newHarnessDB(t, account, store.OpenTest(t))
}

func newHarnessDB(t *testing.T, account int64, db *sql.DB) *harness {
	t.Helper()
	clock := &lockedTime{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	st := state.New(db, state.Options{Now: clock.Now})
	h := &harness{st: st, now: clock}
	st.SetJournalGate(func(ctx context.Context, tx *sql.Tx, userID int64) (int64, bool, error) {
		var enabled, gen int64
		err := tx.QueryRowContext(ctx, `SELECT a.enabled, f.generation
			FROM plexwatch_account a JOIN plexwatch_fence f ON f.user_id=a.user_id
			WHERE a.user_id=? AND a.revoked=0 AND a.token<>'' AND a.token_generation=f.generation`, userID).Scan(&enabled, &gen)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if h.afterGate != nil {
			h.afterGate()
		}
		return gen, enabled != 0, nil
	})
	guid := &memGUID{forward: map[core.RatingKey]string{}, reverse: map[string]core.RatingKey{}}
	fake := &fakeUpstream{accountID: account, eligible: http.StatusOK}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	svc, err := New(Options{
		DB: db, State: st, GUID: guid, BaseURL: srv.URL, NodeID: "node-a",
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	h.svc = svc
	h.fake = fake
	h.guid = guid
	h.logs = &logs
	return h
}

func (h *harness) claimedIntent(t *testing.T, user int64) (state.SyncRow, string) {
	t.Helper()
	var gen int64
	if err := h.svc.db.QueryRow(`SELECT generation FROM plexwatch_fence WHERE user_id=?`, user).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	attempt := h.svc.node + ":" + newID()
	var seq int64
	err := h.svc.db.QueryRow(`INSERT INTO state_sync_journal(user_id, rating_key, aspect, kind, occurred_at, consent_generation, claimed_by, claimed_at, never_sent)
		VALUES(?,?,?,?,?,?,?,?,1) RETURNING seq`,
		user, int64(1), "watch", "complete", h.now.Now().UnixNano(), gen, attempt, h.now.Now().UnixNano()).Scan(&seq)
	if err != nil {
		t.Fatal(err)
	}
	return state.SyncRow{Seq: seq, UserID: user, ConsentGen: gen, At: h.now.Now()}, attempt
}

func (h *harness) connect(t *testing.T, user, plex int64) Status {
	t.Helper()
	if _, err := h.svc.BeginPIN(context.Background(), user, plex); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.FinishPIN(context.Background(), user, plex)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestConnectRejectsOtherAccount(t *testing.T) {
	h := newHarness(t, 999)
	if _, err := h.svc.BeginPIN(context.Background(), 2, 42); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.FinishPIN(context.Background(), 2, 42)
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("err %v", err)
	}
	var n int
	if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM plexwatch_account`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored %d %v", n, err)
	}
	if bytes.Contains(h.logs.Bytes(), []byte(viewerToken)) || bytes.Contains(h.logs.Bytes(), []byte(ownerToken)) {
		t.Fatalf("token logged: %s", h.logs.String())
	}
}

func TestDeliveryIsolatesViewersAndCoalesces(t *testing.T) {
	h := newHarness(t, 42)
	st := h.connect(t, 2, 42)
	if st.PlexID != 42 || st.Enabled || !st.Connected || st.Eligibility != "allowed" {
		t.Fatalf("status %+v", st)
	}
	raw, _ := json.Marshal(st)
	if bytes.Contains(raw, []byte(viewerToken)) {
		t.Fatal("status included the token")
	}
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	movie := core.MovieKey(550)
	ep := core.EpisodeKey(1396, 1, 2)
	h.guid.forward[movie] = "plex://movie/5d7768265af944001f1f6976"
	h.guid.forward[ep] = "plex://episode/602e5e2294f08e002d003344"
	if err := h.st.ScrobbleMany(context.Background(), 2, []core.RatingKey{ep, core.EpisodeKey(1396, 1, 3)}); err != nil {
		t.Fatal(err)
	}
	h.guid.forward[core.EpisodeKey(1396, 1, 3)] = "plex://episode/602e5e2294f08e002d005566"
	// The second episode was journaled before its guid existed. First delivery
	// of episode 2 should still be bulk. Deliver both.
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	posts := append([]postItem(nil), h.fake.posts...)
	users := append([]int64(nil), h.fake.postUsers...)
	tokens := append([]string(nil), h.fake.tokens...)
	h.fake.mu.Unlock()
	if len(posts) != 2 {
		t.Fatalf("posts %+v", posts)
	}
	for _, p := range posts {
		if p.ViewState != "complete" || !p.IsBulk || p.Rating != nil {
			t.Fatalf("post %+v", p)
		}
	}
	for _, id := range users {
		if id != 42 {
			t.Fatalf("user field %d", id)
		}
	}
	for _, tok := range tokens {
		if tok == ownerToken {
			t.Fatal("owner token was sent")
		}
	}
	// A viewer who has not turned sync on is not journaled, so nothing is sent.
	if err := h.st.Scrobble(context.Background(), 8, movie); err != nil {
		t.Fatal(err)
	}
	if more, err := h.svc.DeliverOnce(context.Background()); err != nil || more {
		t.Fatalf("disconnected viewer was queued %v %v", more, err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 2 {
		t.Fatalf("unconnected viewer was uploaded, posts %d", n)
	}
}

func TestNewerIntentWinsOverBlockedRetry(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(7)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	h.fake.entered = make(chan struct{})
	h.fake.blocked = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(context.Background())
		done <- err
	}()
	<-h.fake.entered
	if err := h.st.Unscrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) == 0 || h.fake.posts[len(h.fake.posts)-1].ViewState != "unplayed" {
		t.Fatalf("last post %+v", h.fake.posts)
	}
}

func TestRestartDeliversOnce(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(1)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	_ = h.svc.Close()
	svc, err := New(Options{
		DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), NodeID: "node-b",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: h.now.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 || h.fake.posts[0].ViewState != "complete" {
		t.Fatalf("posts %+v", h.fake.posts)
	}
}

// ETHE-25: Retry-After and server errors are not proof of no effect.
func TestHTTPFailuresAndDisable(t *testing.T) {
	for _, code := range []int{400, 401, 403, 422, 429, 500} {
		t.Run(itoa(int64(code)), func(t *testing.T) {
			h := newHarness(t, 42)
			ctx := context.Background()
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(3)
			h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
			// Preserve the accepted whole-star/coalescing rule: the latest clear
			// remains local, even if an earlier queued value was a whole-star rating.
			for _, value := range []float64{8, 7, 0} {
				if err := h.st.Rate(ctx, 2, key, value); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			before := len(h.fake.posts)
			h.fake.mu.Unlock()
			if before != 0 {
				t.Fatalf("unsupported latest rating sent: %d", before)
			}

			if err := h.st.Rate(ctx, 2, key, 10); err != nil {
				t.Fatal(err)
			}
			h.fake.postStatus = code
			h.fake.retryAfter = "4"
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.now.Add(3 * time.Minute)
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			if n != 1 {
				t.Fatalf("refused or uncertain send repeated: %d", n)
			}
			st, err := h.svc.Status(ctx, 2, 42)
			if err != nil {
				t.Fatal(err)
			}
			if code == 401 && !st.Revoked {
				t.Fatal("authentication refusal did not revoke")
			}
			if code == 403 && st.Enabled {
				t.Fatal("eligibility refusal left sync on")
			}
			if st.Held != 1 {
				t.Fatalf("hold lost: %+v", st)
			}
			if bytes.Contains(h.logs.Bytes(), []byte(viewerToken)) {
				t.Fatal("token logged")
			}
		})
	}
}

func TestMissingAndAmbiguousGUID(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.EpisodeKey(1, 1, 1)
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < missingGUIDLimit; i++ {
		if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.now.Add(backoff(i) + time.Second)
	}
	if more, err := h.svc.DeliverOnce(context.Background()); err != nil || more {
		t.Fatalf("missing guid still live %v %v", more, err)
	}
	h.guid.forward[key] = "plex://show/5d9c086c46115600200aa2fe"
	if err := h.svc.state.RetryDeadSync(context.Background(), 2, []string{"missing_guid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 0 {
		t.Fatal("show guid sent for an episode")
	}
}

func TestImportDoesNotEchoOrGuess(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	movie := core.MovieKey(550)
	guid := "plex://movie/5d7768265af944001f1f6976"
	h.guid.forward[movie] = guid
	h.guid.reverse[guid] = movie
	epGUID := "plex://episode/602e5e2294f08e002d003344"
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Scrobble(context.Background(), 2, movie); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	sentAt := h.fake.posts[0].ViewedAt
	h.fake.mu.Unlock()
	// Replace the empty import handler by serving the echo plus an episode
	// and a show. The test server's GET /state is empty; call apply directly.
	if err := h.svc.applyRemoteItem(context.Background(), 2, remoteItem{GUID: guid, ViewedAt: sentAt, ViewState: "complete"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.st.ClaimSync(context.Background(), "x", h.now.Now(), time.Minute); err != nil || ok {
		t.Fatalf("echo requeued %v %v", ok, err)
	}
	if err := h.svc.applyRemoteItem(context.Background(), 2, remoteItem{GUID: epGUID, ViewedAt: sentAt, ViewState: "complete"}); err != nil && !errors.Is(err, errUnmappedStored) {
		t.Fatal(err)
	}
	if err := h.svc.applyRemoteItem(context.Background(), 2, remoteItem{GUID: "plex://show/5d9c086c46115600200aa2fe", ViewedAt: sentAt, ViewState: "complete"}); err != nil {
		t.Fatal(err)
	}
	ws, err := h.st.Get(context.Background(), 2, core.EpisodeKey(1396, 1, 1))
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("episode guessed %+v %v", ws, err)
	}
}

func TestDisableDropsPending(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(9)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, false); err != nil {
		t.Fatal(err)
	}
	if more, err := h.svc.DeliverOnce(context.Background()); err != nil || more {
		t.Fatalf("disabled delivery %v %v", more, err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 0 {
		t.Fatalf("posted while disabled %+v", h.fake.posts)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t, 42)
	h.fake.redirect = true
	c := h.svc.client
	_, err := c.account(context.Background(), viewerToken)
	if err == nil {
		t.Fatal("redirect followed")
	}
	if bytes.Contains([]byte(err.Error()), []byte(viewerToken)) || bytes.Contains([]byte(err.Error()), []byte("stolen")) {
		t.Fatalf("redirect leaked: %v", err)
	}
}

func TestRefusedEligibilityDoesNotEnable(t *testing.T) {
	h := newHarness(t, 42)
	h.fake.eligible = http.StatusForbidden
	st := h.connect(t, 2, 42)
	if st.Eligibility != "refused" || st.Enabled {
		t.Fatalf("%+v", st)
	}
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("err %v", err)
	}
}

func TestOverlappingWorkersSendOnce(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(4)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{
		DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), NodeID: "node-b",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: h.now.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = h.svc.DeliverOnce(context.Background()) }()
	go func() { defer wg.Done(); _, _ = other.DeliverOnce(context.Background()) }()
	wg.Wait()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 {
		t.Fatalf("posts %d", len(h.fake.posts))
	}
}

func TestDisableDuringLookupDoesNotSend(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(11)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	h.guid.block = make(chan struct{})
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(context.Background())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, false); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.Status(context.Background(), 2, 42)
	if err != nil || st.Enabled || st.InFlight {
		t.Fatalf("status after disable %+v %v", st, err)
	}
	close(h.guid.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("sent after disable acknowledgement: %d", n)
	}
}

func TestInFlightSendIsReportedAndNotRecalled(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(12)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	h.fake.blocked = make(chan struct{})
	h.fake.entered = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(context.Background())
		done <- err
	}()
	<-h.fake.entered
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, false); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.Status(context.Background(), 2, 42)
	if err != nil || !st.InFlight || st.Enabled {
		t.Fatalf("in flight %+v %v", st, err)
	}
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err = h.svc.Status(context.Background(), 2, 42)
	if err != nil || st.InFlight {
		t.Fatalf("after send %+v %v", st, err)
	}
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("posts %d", n)
	}
}

func TestOldToken401DoesNotRevokeReplacement(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(13)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(context.Background(), 2, key); err != nil {
		t.Fatal(err)
	}
	h.fake.blocked = make(chan struct{})
	h.fake.entered = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(context.Background())
		done <- err
	}()
	<-h.fake.entered
	const next = "plex-token-viewer-bbb"
	h.fake.mu.Lock()
	h.fake.authToken = next
	h.fake.rejectToken = viewerToken
	h.fake.mu.Unlock()
	if _, err := h.svc.BeginPIN(context.Background(), 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.FinishPIN(context.Background(), 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(context.Background(), 2, 42, true); err != nil {
		t.Fatal(err)
	}
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var revoked int
	var sealed string
	if err := h.svc.db.QueryRow(`SELECT token, revoked FROM plexwatch_account WHERE user_id=2`).Scan(&sealed, &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 || sealed == "" {
		t.Fatalf("replacement revoked=%d token empty=%v", revoked, sealed == "")
	}
	if err := h.st.Scrobble(context.Background(), 2, core.MovieKey(14)); err != nil {
		t.Fatal(err)
	}
	h.guid.forward[core.MovieKey(14)] = "plex://movie/5d776b8ead5437001f7a3219"
	h.fake.mu.Lock()
	h.fake.rejectToken = ""
	h.fake.blocked = nil
	before := len(h.fake.posts)
	h.fake.mu.Unlock()
	if _, err := h.svc.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != before+1 {
		t.Fatalf("new generation posts %d before %d", len(h.fake.posts), before)
	}
	if h.fake.tokens[len(h.fake.tokens)-1] != next {
		t.Fatalf("sent token %q", h.fake.tokens[len(h.fake.tokens)-1])
	}
}

func TestImportKeepsUnmappedAndStopsOnStoreError(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	const missing = "plex://movie/5d776b8ead5437001f7a9999"
	h.fake.stateBody = `{"items":[{"guid":"` + missing + `","viewedAt":1710000000000,"viewState":"complete"}],"pageInfo":{"endCursor":"page-2","hasNextPage":false}}`
	if _, err := h.svc.ImportCurrent(context.Background(), 2, 42); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.Status(context.Background(), 2, 42)
	if err != nil || st.Skipped != 1 {
		t.Fatalf("skipped %+v %v", st, err)
	}
	var cursor string
	if err := h.svc.db.QueryRow(`SELECT import_cursor FROM plexwatch_account WHERE user_id=2`).Scan(&cursor); err != nil || cursor != "page-2" {
		t.Fatalf("cursor %q %v", cursor, err)
	}
	h.guid.exactErr = errors.New("database is locked")
	h.fake.stateBody = `{"items":[{"guid":"plex://movie/5d776b8ead5437001f7a8888","viewedAt":1710000001000,"viewState":"complete"}],"pageInfo":{"endCursor":"page-3","hasNextPage":false}}`
	if _, err := h.svc.ImportCurrent(context.Background(), 2, 42); err == nil {
		t.Fatal("transient identity error was ignored")
	}
	if err := h.svc.db.QueryRow(`SELECT import_cursor FROM plexwatch_account WHERE user_id=2`).Scan(&cursor); err != nil || cursor != "page-2" {
		t.Fatalf("cursor advanced past a store error: %q %v", cursor, err)
	}
	h.guid.exactErr = nil
	h.guid.reverse[missing] = core.MovieKey(77)
	if _, err := h.svc.Retry(context.Background(), 2, 42); err != nil {
		t.Fatal(err)
	}
	ws, err := h.st.Get(context.Background(), 2, core.MovieKey(77))
	if err != nil || ws.ViewCount != 1 {
		t.Fatalf("retried import %+v %v", ws, err)
	}
	if _, ok, err := h.st.ClaimSync(context.Background(), "n", h.now.Now(), time.Minute); err != nil || ok {
		t.Fatalf("import re-exported %v %v", ok, err)
	}
	st, err = h.svc.Status(context.Background(), 2, 42)
	if err != nil || st.Skipped != 0 {
		t.Fatalf("still skipped %+v %v", st, err)
	}
}

func TestEchoCompactKeepsNewest(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42)
	guid := "plex://movie/5d776b8ead5437001f7a3218"
	key := core.MovieKey(21)
	h.guid.reverse[guid] = key
	for i, at := range []int64{1, 2, 3} {
		if _, err := h.svc.db.Exec(`INSERT INTO plexwatch_sent(user_id, guid, kind, at_ms, rating, sent_at) VALUES(?,?,?,?,0,?)`,
			2, guid, "complete", at, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.svc.compactSent(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM plexwatch_sent WHERE user_id=2`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("kept %d %v", n, err)
	}
	if err := h.svc.applyRemoteItem(context.Background(), 2, remoteItem{GUID: guid, ViewedAt: 3, ViewState: "complete"}); err != nil {
		t.Fatal(err)
	}
	ws, err := h.st.Get(context.Background(), 2, key)
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("newest echo was imported %+v %v", ws, err)
	}
}

func TestConsentStaysOnTheCheckedGeneration(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	before := len(h.fake.paths)
	_, err := h.svc.client.call(ctx, http.MethodPut, h.svc.client.plex, "/api/v2/server/users/42/view_state_sync", viewerToken, []byte(`{"allow":true}`), nil, http.StatusOK)
	if err == nil {
		t.Fatal("consent write was accepted")
	}
	_, err = h.svc.client.call(ctx, http.MethodGet, h.svc.client.plex, "/api/v2/server/users/services", viewerToken, nil, nil, http.StatusOK)
	if err == nil {
		t.Fatal("services route was accepted")
	}
	_, err = h.svc.client.call(ctx, http.MethodPost, h.svc.client.plex, "/api/home/users/42/switch", viewerToken, nil, nil, http.StatusOK)
	if err == nil {
		t.Fatal("home switch was accepted")
	}
	h.fake.mu.Lock()
	sent := append([]string(nil), h.fake.paths[before:]...)
	h.fake.mu.Unlock()
	if len(sent) != 0 {
		t.Fatalf("forbidden routes reached the network: %v", sent)
	}

	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	old, ok, err := h.svc.loadAccount(ctx, 2)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	const next = "plex-token-viewer-bbb"
	h.fake.mu.Lock()
	h.fake.authToken = next
	h.fake.mu.Unlock()
	if _, err := h.svc.BeginPIN(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.FinishPIN(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	h.fake.refuseToken = viewerToken
	h.fake.mu.Unlock()
	if err := h.svc.refreshEligibility(ctx, 2, old); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil || !st.Enabled || st.Eligibility != "allowed" || st.Revoked {
		t.Fatalf("stale refusal changed the replacement: %+v %v", st, err)
	}

	row, ok, err := h.svc.loadAccount(ctx, 2)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if err := h.svc.revoke(ctx, 2, row.tokenGeneration); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	h.fake.refuseToken = ""
	h.fake.mu.Unlock()
	if err := h.svc.refreshEligibility(ctx, 2, row); !errors.Is(err, errStaleGeneration) {
		t.Fatalf("stale allow err %v", err)
	}
	var revoked int
	var sealed string
	if err := h.svc.db.QueryRow(`SELECT token, revoked FROM plexwatch_account WHERE user_id=2`).Scan(&sealed, &revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 || sealed != "" {
		t.Fatalf("stale allow restored revoked=%d empty=%v", revoked, sealed == "")
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, p := range h.fake.paths {
		if strings.Contains(p, "services") || strings.Contains(p, "/api/home/") || strings.HasPrefix(p, http.MethodPut) || strings.HasPrefix(p, http.MethodPatch) {
			t.Fatalf("consent boundary crossed: %s", p)
		}
	}
}

func TestContentionDoesNotStarveOtherTitles(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	older := core.MovieKey(31)
	newer := core.MovieKey(32)
	h.guid.forward[older] = "plex://movie/5d776b8ead5437001f7a0031"
	h.guid.forward[newer] = "plex://movie/5d776b8ead5437001f7a0032"
	if err := h.st.Scrobble(ctx, 2, older); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Scrobble(ctx, 2, newer); err != nil {
		t.Fatal(err)
	}
	now := h.now.Now()
	ok, err := h.st.LockSync(ctx, 2, older, state.SyncWatch, "other-node", now, time.Hour)
	if err != nil || !ok {
		t.Fatalf("lock %v %v", ok, err)
	}
	steps := 0
	for steps < 8 {
		more, err := h.svc.DeliverOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		steps++
		if !more {
			break
		}
	}
	if steps > 3 {
		t.Fatalf("contended claim spun %d times", steps)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 || h.fake.posts[0].GUID != h.guid.forward[newer] {
		t.Fatalf("posts %+v", h.fake.posts)
	}
}

func TestClusterFenceExpiresAndStaysShared(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	other, err := New(Options{
		DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), NodeID: "node-b",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: h.now.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	past := h.now.Now().Add(-time.Minute).UnixNano()
	if _, err := h.svc.db.Exec(`UPDATE plexwatch_fence SET inflight=4, inflight_until=? WHERE user_id=2`, past); err != nil {
		t.Fatal(err)
	}
	st, err := other.Status(ctx, 2, 42)
	if err != nil || st.InFlight {
		t.Fatalf("expired lease %+v %v", st, err)
	}
	key := core.MovieKey(33)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0033"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	if _, err := other.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = h.svc.Status(ctx, 2, 42)
	if err != nil || st.InFlight {
		t.Fatalf("lease stuck after send %+v %v", st, err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("posts %d", n)
	}

	next := core.MovieKey(34)
	h.guid.forward[next] = "plex://movie/5d776b8ead5437001f7a0034"
	if err := h.st.Scrobble(ctx, 2, next); err != nil {
		t.Fatal(err)
	}
	h.fake.blocked = make(chan struct{})
	h.fake.entered = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(ctx)
		done <- err
	}()
	<-h.fake.entered
	if _, err := other.SetEnabled(ctx, 2, 42, false); err != nil {
		t.Fatal(err)
	}
	st, err = other.Status(ctx, 2, 42)
	if err != nil || !st.InFlight || st.Enabled {
		t.Fatalf("other node missed the in-flight send %+v %v", st, err)
	}
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := other.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 2 {
		t.Fatalf("posts %d", len(h.fake.posts))
	}
}

func TestExportAndImportUseSeparateClocks(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	key := core.MovieKey(41)
	guid := "plex://movie/5d776b8ead5437001f7a0041"
	h.guid.forward[key] = guid
	h.guid.reverse[guid] = key
	if err := h.st.Rate(ctx, 2, key, 10); err != nil {
		t.Fatal(err)
	}
	rated := h.now.Now()
	h.now.Add(2 * time.Hour)
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	watched := h.now.Now()
	if _, err := h.svc.ExportCurrent(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	rows, err := h.svc.db.Query(`SELECT aspect, occurred_at FROM state_sync_journal WHERE user_id=2 AND superseded=0 AND dead=0 AND delivered_at=0`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for rows.Next() {
		var aspect string
		var at int64
		if err := rows.Scan(&aspect, &at); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[aspect] = at
	}
	rows.Close()
	if got["watch"] != watched.UnixNano() || got["rating"] != rated.UnixNano() {
		t.Fatalf("export clocks %+v want watch %d rating %d", got, watched.UnixNano(), rated.UnixNano())
	}
	two := 2
	if err := h.svc.applyRemoteItem(ctx, 2, remoteItem{
		GUID: guid, ViewState: "complete", ViewedAt: watched.Add(time.Hour).UnixMilli(),
		RatedAt: rated.Add(-time.Hour).UnixMilli(), Rating: &two,
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := h.st.Get(ctx, 2, key)
	if err != nil || ws.UserRating != 10 {
		t.Fatalf("import let the watch time replace the rating %+v %v", ws, err)
	}
}

func TestKindMismatchStaysUnmapped(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	const guid = "plex://movie/5d776b8ead5437001f7a0077"
	ep := core.EpisodeKey(8, 1, 1)
	h.guid.reverse[guid] = ep
	if err := h.svc.applyRemoteItem(ctx, 2, remoteItem{GUID: guid, ViewedAt: 1710000000000, ViewState: "complete"}); !errors.Is(err, errUnmappedStored) {
		t.Fatal(err)
	}
	ws, err := h.st.Get(ctx, 2, ep)
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("movie guid wrote an episode %+v %v", ws, err)
	}
	if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil || st.Skipped != 1 {
		t.Fatalf("retry dropped the mismatch %+v %v", st, err)
	}
	h.guid.reverse[guid] = core.MovieKey(70)
	if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	ws, err = h.st.Get(ctx, 2, core.MovieKey(70))
	if err != nil || ws.ViewCount != 1 {
		t.Fatalf("retry after the map was fixed %+v %v", ws, err)
	}
	ws, err = h.st.Get(ctx, 2, ep)
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("episode changed %+v %v", ws, err)
	}
	st, err = h.svc.Status(ctx, 2, 42)
	if err != nil || st.Skipped != 0 {
		t.Fatalf("still skipped %+v %v", st, err)
	}
}

func TestEchoCompactKeepsEachTitle(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	first := "plex://movie/5d776b8ead5437001f7a0001"
	second := "plex://movie/5d776b8ead5437001f7a0002"
	h.guid.reverse[first] = core.MovieKey(51)
	h.guid.reverse[second] = core.MovieKey(52)
	for i, guid := range []string{first, second} {
		if _, err := h.svc.db.Exec(`INSERT INTO plexwatch_sent(user_id, guid, kind, at_ms, rating, sent_at) VALUES(?,?,?,?,0,?)`,
			2, guid, "complete", int64(10+i), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.svc.compactSent(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM plexwatch_sent WHERE user_id=2`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("kept %d %v", n, err)
	}
	if err := h.svc.applyRemoteItem(ctx, 2, remoteItem{GUID: first, ViewedAt: 10, ViewState: "complete"}); err != nil {
		t.Fatal(err)
	}
	ws, err := h.st.Get(ctx, 2, core.MovieKey(51))
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("older title echo was imported %+v %v", ws, err)
	}
}

func TestStalePermitReleaseDoesNotClearNewer(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	row, attempt := h.claimedIntent(t, 2)
	permitA, err := h.svc.acquireSend(ctx, row, attempt, postItem{GUID: "plex://movie/5d7768265af944001f1f6976", ViewState: "complete", ViewedAt: h.now.Now().UnixMilli()})
	if err != nil || !permitA.OK || permitA.ID == "" {
		t.Fatal(err, permitA)
	}
	past := h.now.Now().Add(-time.Minute).UnixNano()
	if _, err := h.svc.db.Exec(`UPDATE plexwatch_permit SET until_at=? WHERE permit_id=?`, past, permitA.ID); err != nil {
		t.Fatal(err)
	}
	// ETHE-25: the same effect cannot acquire another permit after expiry.
	blocked, err := h.svc.acquireSend(ctx, row, attempt, postItem{GUID: "plex://movie/5d7768265af944001f1f6976", ViewState: "complete"})
	if err != nil || blocked.OK || blocked.Terminal != "uncertain_send" {
		t.Fatal(err, blocked)
	}
	row, attempt = h.claimedIntent(t, 2)
	row.Key = core.RatingKey(2)
	if _, err := h.svc.db.Exec(`UPDATE state_sync_journal SET rating_key=? WHERE seq=?`, int64(row.Key), row.Seq); err != nil {
		t.Fatal(err)
	}
	permitB, err := h.svc.acquireSend(ctx, row, attempt, postItem{GUID: "plex://movie/5d776b8ead5437001f7a3218", ViewState: "complete", ViewedAt: h.now.Now().UnixMilli()})
	if err != nil || !permitB.OK || permitB.ID == "" || permitB.ID == permitA.ID {
		t.Fatal(err, permitB)
	}
	h.svc.releaseSend(ctx, permitA.ID)
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil || !st.InFlight {
		t.Fatalf("newer permit cleared %+v %v", st, err)
	}
	var live string
	if err := h.svc.db.QueryRow(`SELECT permit_id FROM plexwatch_permit WHERE user_id=2 AND until_at>?`, h.now.Now().UnixNano()).Scan(&live); err != nil || live != permitB.ID {
		t.Fatalf("live %q %v", live, err)
	}
	h.svc.releaseSend(ctx, permitB.ID)
	st, err = h.svc.Status(ctx, 2, 42)
	if err != nil || st.InFlight {
		t.Fatalf("after release %+v %v", st, err)
	}
}

// ETHE-25: concurrent workers may admit only one uncertain effect.
func TestConcurrentAcquireKeepsEachPermit(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	row, attempt := h.claimedIntent(t, 2)
	var wg sync.WaitGroup
	outcomes := make(chan sendPermit, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := h.svc.acquireSend(ctx, row, attempt, postItem{GUID: "plex://movie/5d7768265af944001f1f6976", ViewState: "complete", ViewedAt: h.now.Now().UnixMilli()})
			outcomes <- p
			errs <- err
		}()
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	admitted, held := 0, 0
	for p := range outcomes {
		if p.OK {
			admitted++
		} else if p.Terminal == "uncertain_send" {
			held++
		}
	}
	if admitted != 1 || held != 1 {
		t.Fatalf("admitted=%d held=%d", admitted, held)
	}
}

func TestStaleEnableDoesNotUndoLaterTransition(t *testing.T) {
	ctx := context.Background()
	t.Run("disable", func(t *testing.T) {
		h := newHarness(t, 42)
		h.connect(t, 2, 42)
		started, release := make(chan struct{}), make(chan struct{})
		h.svc.beforeEnable = func() {
			close(started)
			<-release
		}
		errc := make(chan error, 1)
		go func() {
			_, err := h.svc.SetEnabled(ctx, 2, 42, true)
			errc <- err
		}()
		<-started
		if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-errc; !errors.Is(err, errStaleGeneration) {
			t.Fatal(err)
		}
		st, err := h.svc.Status(ctx, 2, 42)
		if err != nil || st.Enabled {
			t.Fatalf("stale enable undid disable %+v %v", st, err)
		}
	})
	t.Run("replace", func(t *testing.T) {
		h := newHarness(t, 42)
		h.connect(t, 2, 42)
		started, release := make(chan struct{}), make(chan struct{})
		h.svc.beforeEnable = func() {
			close(started)
			<-release
		}
		errc := make(chan error, 1)
		go func() {
			_, err := h.svc.SetEnabled(ctx, 2, 42, true)
			errc <- err
		}()
		<-started
		h.fake.mu.Lock()
		h.fake.authToken = "plex-token-viewer-bbb"
		h.fake.mu.Unlock()
		if _, err := h.svc.BeginPIN(ctx, 2, 42); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.FinishPIN(ctx, 2, 42); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-errc; !errors.Is(err, errStaleGeneration) {
			t.Fatal(err)
		}
		st, err := h.svc.Status(ctx, 2, 42)
		if err != nil || st.Enabled || !st.Connected {
			t.Fatalf("stale enable armed the replacement %+v %v", st, err)
		}
		h.svc.beforeEnable = nil
		if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
			t.Fatal(err)
		}
		st, err = h.svc.Status(ctx, 2, 42)
		if err != nil || !st.Enabled {
			t.Fatalf("fresh enable %+v %v", st, err)
		}
	})
}

func TestStaleDeliveryDoesNotReleaseReplacement(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(81)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0081"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	h.fake.blocked = make(chan struct{})
	h.fake.entered = make(chan struct{})
	holdSecond := make(chan struct{})
	sawSecond := make(chan string, 1)
	var attempts []string
	h.svc.attemptStart = func(attempt string) {
		attempts = append(attempts, attempt)
		if len(attempts) == 2 {
			sawSecond <- attempt
			<-holdSecond
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(ctx)
		done <- err
	}()
	<-h.fake.entered
	h.now.Add(3 * time.Minute)
	done2 := make(chan struct{})
	go func() {
		_, _ = h.svc.DeliverOnce(ctx)
		close(done2)
	}()
	second := <-sawSecond
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// ETHE-25: a committed receipt acknowledges the original intent even
	// after claim expiry; it must not release another worker's title lock.
	var holder string
	var delivered int64
	if err := h.svc.db.QueryRow(`SELECT delivered_at FROM state_sync_journal WHERE user_id=2`).Scan(&delivered); err != nil || delivered == 0 {
		t.Fatalf("receipt not acknowledged: %d %v", delivered, err)
	}

	if err := h.svc.db.QueryRow(`SELECT holder FROM state_sync_lock WHERE user_id=2 AND rating_key=?`, int64(key)).Scan(&holder); err != nil || holder != second {
		t.Fatalf("lock %q %v", holder, err)
	}
	if len(attempts) < 2 || attempts[0] == attempts[1] || attempts[0] == h.svc.node || attempts[1] == h.svc.node {
		t.Fatalf("attempts %q node %s", attempts, h.svc.node)
	}
	close(holdSecond)
	<-done2
}

func TestIntentAfterDisableIsNotSentOnEnable(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	var observed int64
	if err := h.svc.db.QueryRow(`SELECT generation FROM plexwatch_fence WHERE user_id=2`).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
		t.Fatal(err)
	}
	late := core.MovieKey(82)
	if _, err := h.svc.db.Exec(`INSERT INTO state_sync_journal(user_id, rating_key, aspect, kind, occurred_at, consent_generation)
		VALUES(?,?,?,?,?,?)`, 2, int64(late), "watch", "complete", h.now.Now().UnixNano(), observed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	h.guid.forward[late] = "plex://movie/5d776b8ead5437001f7a0082"
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("late intent was sent: %d", n)
	}
	var dead int
	var gen int64
	if err := h.svc.db.QueryRow(`SELECT dead, consent_generation FROM state_sync_journal WHERE rating_key=?`, int64(late)).Scan(&dead, &gen); err != nil || dead != 1 || gen != observed {
		t.Fatalf("dead %d gen %d observed %d %v", dead, gen, observed, err)
	}
	fresh := core.MovieKey(83)
	h.guid.forward[fresh] = "plex://movie/5d776b8ead5437001f7a0083"
	if err := h.st.Scrobble(ctx, 2, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 || h.fake.posts[0].GUID != h.guid.forward[fresh] {
		t.Fatalf("posts %+v", h.fake.posts)
	}
}

func TestIntentCommittedAfterDisableIsNotSent(t *testing.T) {
	h := newHarness(t, 42)
	if h.svc.db.Stats().MaxOpenConnections < 2 {
		t.Skip("overlapping consent admission needs PostgreSQL (ETHER_TEST_POSTGRES)")
	}
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	var observed int64
	if err := h.svc.db.QueryRow(`SELECT generation FROM plexwatch_fence WHERE user_id=2`).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	h.afterGate = func() {
		close(started)
		<-release
	}
	late := core.MovieKey(84)
	h.guid.forward[late] = "plex://movie/5d776b8ead5437001f7a0084"
	errc := make(chan error, 1)
	go func() {
		errc <- h.st.Scrobble(ctx, 2, late)
	}()
	<-started
	if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.afterGate = nil
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("intent committed after disable was sent: %d", n)
	}
	var dead int
	var gen int64
	if err := h.svc.db.QueryRow(`SELECT dead, consent_generation FROM state_sync_journal WHERE rating_key=?`, int64(late)).Scan(&dead, &gen); err != nil || dead != 1 || gen != observed {
		t.Fatalf("dead %d gen %d observed %d %v", dead, gen, observed, err)
	}
}

func TestLookupThenReenableDoesNotSendOldIntent(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"disable", "reconnect"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 42)
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(90)
			h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0090"
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			h.guid.block = make(chan struct{})
			h.guid.entered = make(chan struct{})
			h.guid.blockOnce = true
			done := make(chan error, 1)
			go func() {
				_, err := h.svc.DeliverOnce(ctx)
				done <- err
			}()
			<-h.guid.entered
			if name == "reconnect" {
				h.fake.mu.Lock()
				h.fake.authToken = "plex-token-viewer-bbb"
				h.fake.mu.Unlock()
				if _, err := h.svc.BeginPIN(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
				if _, err := h.svc.FinishPIN(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
			} else if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			close(h.guid.block)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			if n != 0 {
				t.Fatalf("old intent posted after %s: %d", name, n)
			}
		})
	}
}

func TestLookupSupersessionDoesNotSendStaleAttempt(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(91)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0091"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	h.guid.block = make(chan struct{})
	h.guid.entered = make(chan struct{})
	h.guid.blockOnce = true
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(ctx)
		done <- err
	}()
	<-h.guid.entered
	h.now.Add(3 * time.Minute)
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	close(h.guid.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 || h.fake.posts[0].GUID != h.guid.forward[key] {
		t.Fatalf("posts %+v", h.fake.posts)
	}
}

func TestTransientFenceReadStaysRetryable(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(92)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0092"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	h.svc.admitFault = func() error { return errors.New("database is locked") }
	if _, err := h.svc.DeliverOnce(ctx); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("err %v", err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("transient read posted %d", n)
	}
	var dead int
	var reason string
	if err := h.svc.db.QueryRow(`SELECT dead, last_error FROM state_sync_journal WHERE user_id=2 AND rating_key=?`, int64(key)).Scan(&dead, &reason); err != nil {
		t.Fatal(err)
	}
	if dead != 0 || reason == "consent" {
		t.Fatalf("dead %d reason %q", dead, reason)
	}
	h.svc.admitFault = nil
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 {
		t.Fatalf("retry posts %+v", h.fake.posts)
	}
}

func TestAdmissionHoldsFenceUntilPermit(t *testing.T) {
	h := newHarness(t, 42)
	if h.svc.db.Stats().MaxOpenConnections < 2 {
		t.Skip("fence admission overlap needs PostgreSQL (ETHER_TEST_POSTGRES)")
	}
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(93)
	h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a0093"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	h.svc.afterFenceRead = func() {
		close(held)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.svc.DeliverOnce(ctx)
		done <- err
	}()
	<-held
	dctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	_, err := h.svc.SetEnabled(dctx, 2, 42, false)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("disable during admission: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
		t.Fatal(err)
	}
}

func TestProbeTokenIsReadOnly(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.fake.mu.Lock()
	before := len(h.fake.paths)
	h.fake.mu.Unlock()
	acct, err := h.svc.ProbeToken(ctx, viewerToken, 42)
	if err != nil || acct.ID != 42 {
		t.Fatalf("%+v %v", acct, err)
	}
	if _, err := h.svc.ProbeToken(ctx, viewerToken, 99); !errors.Is(err, ErrAccountMismatch) {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	var sawUser, sawEligibility bool
	for _, p := range h.fake.paths[before:] {
		if strings.Contains(p, "/pins") || strings.Contains(p, "POST /state") || strings.Contains(p, "/services") || strings.Contains(p, "/api/home/") {
			t.Fatalf("probe left the read-only pair: %s", p)
		}
		if p == "GET /api/v2/user" {
			sawUser = true
		}
		if strings.HasSuffix(p, "/view_state_sync") && strings.HasPrefix(p, "GET ") {
			sawEligibility = true
		}
	}
	if !sawUser || !sawEligibility {
		t.Fatalf("paths %v", h.fake.paths[before:])
	}
}
