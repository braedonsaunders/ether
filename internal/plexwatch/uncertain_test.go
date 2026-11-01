package plexwatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// ETHE-25's accepted safety intent: remote acceptance without a reliable
// acknowledgement must never cause another POST, including after lease expiry.
// The wire shape is the source-backed /state contract documented in doc.go
// and encoded by Client.postState, not a claimed idempotence contract.
func TestUncertainSendNoSecondPOST(t *testing.T) {
	for _, mode := range []string{"lost_reply", "accepted_500", "response_read", "accepted_429"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, 42)
			h.connect(t, 2, 42)
			ctx := context.Background()
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			guid := "plex://movie/5d7768265af944001f1f6976"
			h.guid.forward[key] = guid
			var mu sync.Mutex
			var requests [][]byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/state" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, b)
				mu.Unlock()
				if mode == "response_read" {
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(201)
					_, _ = w.Write([]byte("{}"))
					return
				}
				if mode == "accepted_429" {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					return
				}
				if mode == "accepted_500" {
					w.WriteHeader(500)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}))
			defer server.Close()
			c, err := newClient(server.URL, "offline-check", nil, h.now.Now)
			if err != nil {
				t.Fatal(err)
			}
			h.svc.client = c
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			original := h.now.Now().UnixMilli()
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.now.Add(3 * time.Minute)
			restarted, err := New(Options{DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: server.URL, NodeID: "node-b", Now: h.now.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if _, err := restarted.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, b := range requests {
				var got struct {
					User  int64      `json:"user"`
					Items []postItem `json:"items"`
				}
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatal(err)
				}
				if got.User != 42 || len(got.Items) != 1 || got.Items[0].GUID != guid || got.Items[0].ViewState != "complete" || got.Items[0].ViewedAt != original {
					t.Fatalf("wrong wire payload: %s", b)
				}
				t.Logf("saved request: %s", b)
			}
			t.Logf("remote accepted requests after restart/lease expiry: %d", len(requests))
			if len(requests) != 1 {
				t.Fatalf("uncertain effect replayed: got %d POSTs, want 1", len(requests))
			}
			var payload, storedGUID, aspect string
			var account, originalNS int64
			if err := h.svc.db.QueryRow(`SELECT payload,plex_id,guid,aspect,occurred_at FROM plexwatch_attempt`).Scan(&payload, &account, &storedGUID, &aspect, &originalNS); err != nil {
				t.Fatal(err)
			}
			if payload != string(requests[0]) || account != 42 || storedGUID != guid || aspect != "watch" || time.Unix(0, originalNS).UnixMilli() != original {
				t.Fatalf("admission did not bind the exact wire effect: account=%d guid=%s aspect=%s at=%d payload=%s", account, storedGUID, aspect, originalNS, payload)
			}
		})
	}
}

// ETHE-25: durable admission and atomic completion survive every local
// crash boundary, including a commit that succeeds but loses its reply.
func TestUncertainSendCrashAndCommitBoundaries(t *testing.T) {
	for _, boundary := range []string{"admitted", "accepted", "echo", "before_commit", "admission_failed", "admission_unknown", "receipt_failed", "receipt_unknown"} {
		t.Run(boundary, func(t *testing.T) {
			h := newHarness(t, 42)
			ctx := context.Background()
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			h.guid.forward[key] = "plex://movie/5d7768265af944001f1f6976"
			fault := errors.New("injected crash or lost commit acknowledgement")
			switch boundary {
			case "admitted":
				h.svc.afterAdmission = func() error { return fault }
			case "accepted":
				h.svc.afterPost = func() error { return fault }
			case "echo", "before_commit":
				h.svc.receiptFault = func(stage string) error {
					if (boundary == "echo" && stage == "echo") || (boundary == "before_commit" && stage == "commit") {
						return fault
					}
					return nil
				}
			case "admission_failed":
				h.svc.admissionCommit = func(tx *sql.Tx) error { _ = tx.Rollback(); return fault }
			case "admission_unknown":
				h.svc.admissionCommit = func(tx *sql.Tx) error {
					if err := tx.Commit(); err != nil {
						return err
					}
					return fault
				}
			case "receipt_failed":
				h.svc.receiptCommit = func(tx *sql.Tx) error { _ = tx.Rollback(); return fault }
			case "receipt_unknown":
				h.svc.receiptCommit = func(tx *sql.Tx) error {
					if err := tx.Commit(); err != nil {
						return err
					}
					return fault
				}
			}
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); !errors.Is(err, fault) {
				t.Fatalf("fault not reached: %v", err)
			}
			h.now.Add(3 * time.Minute)
			restarted, err := New(Options{DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), NodeID: "node-b", Now: h.now.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if _, err := restarted.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			want := 1
			if boundary == "admitted" || boundary == "admission_unknown" {
				want = 0
			}
			if n != want {
				t.Fatalf("POSTs %d want %d", n, want)
			}
			var attempts, receipts, echoes, acks int
			for q, p := range map[string]*int{
				`SELECT COUNT(*) FROM plexwatch_attempt`:                        &attempts,
				`SELECT COUNT(*) FROM plexwatch_attempt WHERE receipt_at<>0`:    &receipts,
				`SELECT COUNT(*) FROM plexwatch_sent`:                           &echoes,
				`SELECT COUNT(*) FROM state_sync_journal WHERE delivered_at<>0`: &acks,
			} {
				if err := h.svc.db.QueryRow(q).Scan(p); err != nil {
					t.Fatal(err)
				}
			}
			if attempts != 1 {
				t.Fatalf("admission count %d", attempts)
			}
			committed := 0
			if boundary == "receipt_unknown" || boundary == "admission_failed" {
				committed = 1
			}
			if receipts != committed || echoes != committed || acks != committed {
				t.Fatalf("non-atomic receipt/echo/ack: %d/%d/%d want %d", receipts, echoes, acks, committed)
			}
			t.Logf("boundary=%s posts=%d durable admissions=%d receipt/echo/ack=%d/%d/%d", boundary, n, attempts, receipts, echoes, acks)
		})
	}
}

// ETHE-25: changing queue, consent or identity cannot erase an unknown effect.
func TestUncertainSendSurvivesLifecycle(t *testing.T) {
	for _, change := range []string{"supersede", "retry", "reconnect", "consent", "remap", "compact", "export"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t, 42)
			ctx := context.Background()
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			guid := "plex://movie/5d7768265af944001f1f6976"
			h.guid.forward[key] = guid
			h.fake.postStatus = 500
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			at := h.now.Now().UnixNano()
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "retry":
				if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
			case "reconnect":
				if _, err := h.svc.Disconnect(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
				h.connect(t, 2, 42)
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
			case "consent":
				if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
					t.Fatal(err)
				}
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
			case "remap":
				h.guid.forward[key] = "plex://movie/5d776b8ead5437001f7a3218"
			case "compact":
				h.now.Add(15 * 24 * time.Hour)
				if err := h.st.CompactSync(ctx, h.now.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if change == "export" {
				if _, err := h.svc.ExportCurrent(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := h.st.Unscrobble(ctx, 2, key); err != nil {
					t.Fatal(err)
				}
			}
			h.now.Add(3 * time.Minute)
			h.fake.postStatus = 201
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			// The same remote GUID under another local key cannot evade the hold.
			alias := core.MovieKey(551)
			h.guid.forward[alias] = guid
			if err := h.st.Scrobble(ctx, 2, alias); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			if n != 1 {
				t.Fatalf("%s bypassed hold: %d POSTs", change, n)
			}
			var gotGUID, payload string
			var account, generation, original int64
			if err := h.svc.db.QueryRow(`SELECT plex_id,generation,guid,occurred_at,payload FROM plexwatch_attempt WHERE user_id=2`).Scan(&account, &generation, &gotGUID, &original, &payload); err != nil {
				t.Fatal(err)
			}
			if account != 42 || generation == 0 || gotGUID != guid || original != at || !strings.Contains(payload, guid) {
				t.Fatalf("immutable admission changed: %d/%d/%s/%d/%s", account, generation, gotGUID, original, payload)
			}
			st, err := h.svc.Status(ctx, 2, 42)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(st)
			if st.Held != 1 {
				t.Fatalf("dishonest hold status %+v", st)
			}
			if strings.Contains(payload, viewerToken) || bytes.Contains(raw, []byte(viewerToken)) || bytes.Contains(h.logs.Bytes(), []byte(viewerToken)) {
				t.Fatal("credential exposed")
			}
			t.Logf("%s: one original POST; immutable admission held across new intent and GUID alias", change)
		})
	}
}

// ETHE-25: an expired claim must not let a second node POST while the first
// request might still be running. Other titles and aspects remain available.
func TestUncertainSendTwoNodesAndUnaffectedWork(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(550)
	h.guid.forward[key] = "plex://movie/5d7768265af944001f1f6976"
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	h.fake.blocked = make(chan struct{})
	h.fake.entered = make(chan struct{})
	h.fake.postStatus = 500
	done := make(chan error, 1)
	go func() { _, err := h.svc.DeliverOnce(ctx); done <- err }()
	<-h.fake.entered
	h.now.Add(3 * time.Minute)
	second, err := New(Options{DB: h.svc.db, State: h.st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), NodeID: "node-b", Now: h.now.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := h.st.Unscrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	if _, err := second.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	close(h.fake.blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	n := len(h.fake.posts)
	h.fake.postStatus = 201
	h.fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("second node repeated in-flight effect: %d", n)
	}
	other := core.MovieKey(7)
	h.guid.forward[other] = "plex://movie/5d776b8ead5437001f7a3218"
	if err := h.st.Scrobble(ctx, 2, other); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Rate(ctx, 2, key, 10); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := second.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h.fake.mu.Lock()
	n = len(h.fake.posts)
	h.fake.accountID = 84
	h.fake.authToken = "offline-viewer-84-token"
	h.fake.mu.Unlock()
	if n != 3 {
		t.Fatalf("unrelated title or aspect blocked: %d posts", n)
	}
	h.connect(t, 8, 84)
	if _, err := h.svc.SetEnabled(ctx, 8, 84, true); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Scrobble(ctx, 8, key); err != nil {
		t.Fatal(err)
	}
	if _, err := second.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 4 || h.fake.postUsers[3] != 84 {
		t.Fatalf("independent viewer blocked: %+v", h.fake.postUsers)
	}
}

// ETHE-25: a real process exit discards open transactions and worker leases,
// not the immutable admission in SQLite files or disposable PostgreSQL.
func TestUncertainSendProcessCrash(t *testing.T) {
	if path := os.Getenv("ETHER_UNCERTAIN_CRASH_DB"); path != "" {
		db, err := openCrashDB(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		st := state.New(db, state.Options{})
		guid := &memGUID{forward: map[core.RatingKey]string{core.MovieKey(550): "plex://movie/5d7768265af944001f1f6976"}}
		svc, err := New(Options{DB: db, State: st, GUID: guid, BaseURL: os.Getenv("ETHER_UNCERTAIN_CRASH_ORIGIN"), Now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }})
		if err != nil {
			t.Fatal(err)
		}
		crash := func() error { os.Exit(72); return nil }
		switch os.Getenv("ETHER_UNCERTAIN_CRASH_STAGE") {
		case "admitted":
			svc.afterAdmission = crash
		case "accepted":
			svc.afterPost = crash
		case "echo":
			svc.receiptFault = func(stage string) error {
				if stage == "echo" {
					return crash()
				}
				return nil
			}
		case "commit":
			svc.receiptFault = func(stage string) error {
				if stage == "commit" {
					return crash()
				}
				return nil
			}
		}
		_, err = svc.DeliverOnce(context.Background())
		t.Fatalf("child did not crash: %v", err)
	}
	for _, stage := range []string{"admitted", "accepted", "echo", "commit"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "crash.db")
			var db *sql.DB
			var err error
			postgres := os.Getenv(store.TestPostgresEnv) != ""
			if postgres {
				// Keep the test-schema owner alive; child/restarted connections use
				// the exact same disposable schema without owning its drop callback.
				db = store.OpenTest(t)
				var schema string
				if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(os.Getenv(store.TestPostgresEnv))
				if err != nil {
					t.Fatal(err)
				}
				q := u.Query()
				q.Set("search_path", schema)
				u.RawQuery = q.Encode()
				path = u.String()
			} else {
				db, err = openCrashDB(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			h := newHarnessDB(t, 42, db)
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			h.guid.forward[key] = "plex://movie/5d7768265af944001f1f6976"
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if !postgres {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			child := exec.Command(os.Args[0], "-test.run=^TestUncertainSendProcessCrash$")
			child.Env = append(os.Environ(), "ETHER_UNCERTAIN_CRASH_DB="+path, "ETHER_UNCERTAIN_CRASH_ORIGIN="+h.svc.client.plex.String(), "ETHER_UNCERTAIN_CRASH_STAGE="+stage)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 72 {
				t.Fatalf("crash child: %v %s", err, output)
			}
			db, err = openCrashDB(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			st := state.New(db, state.Options{Now: h.now.Now})
			h.now.Add(3 * time.Minute)
			svc, err := New(Options{DB: db, State: st, GUID: h.guid, BaseURL: h.svc.client.plex.String(), Now: h.now.Now})
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			if _, err := svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			want := 1
			if stage == "admitted" {
				want = 0
			}
			if n != want {
				t.Fatalf("process restart replayed effect: %d want %d", n, want)
			}
			var held int
			if err := db.QueryRow(`SELECT COUNT(*) FROM plexwatch_attempt WHERE receipt_at=0`).Scan(&held); err != nil || held != 1 {
				t.Fatalf("durable hold %d %v", held, err)
			}
			t.Logf("process exit at %s: reopened database retains one hold; POSTs=%d", stage, n)
		})
	}
}

// Upgrade must keep never-attempted queued rows (attempts=0, unclaimed, no
// error) as normal queued work while converting previously attempted or
// claimed undelivered rows into legacy holds.
func TestUncertainSendLegacyUpgrade(t *testing.T) {
	t.Run("unattempted_stays_queued", func(t *testing.T) {
		h := newHarness(t, 42)
		ctx := context.Background()
		h.connect(t, 2, 42)
		if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
			t.Fatal(err)
		}
		key := core.MovieKey(550)
		guid := "plex://movie/5d7768265af944001f1f6976"
		h.guid.forward[key] = guid
		if err := h.st.Scrobble(ctx, 2, key); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`DROP TABLE plexwatch_attempt`, `ALTER TABLE state_sync_journal DROP COLUMN never_sent`, `DELETE FROM schema_migrations WHERE component='watchsend' OR (component='state_plexwatch' AND version=4)`} {
			if _, err := h.svc.db.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Migrate(ctx, h.svc.db); err != nil {
			t.Fatal(err)
		}
		st, err := h.svc.Status(ctx, 2, 42)
		if err != nil || st.Held != 0 || st.HeldLegacy != 0 || st.Pending != 1 {
			t.Fatalf("unattempted upgrade status %+v %v", st, err)
		}
		if _, err := h.svc.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.fake.mu.Lock()
		n := len(h.fake.posts)
		h.fake.mu.Unlock()
		if n != 1 {
			t.Fatalf("unattempted upgraded row was not delivered: %d posts", n)
		}
	})

	for _, disconnected := range []bool{false, true} {
		t.Run(fmt.Sprintf("attempted_held/disconnected=%t", disconnected), func(t *testing.T) {
			h := newHarness(t, 42)
			ctx := context.Background()
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			guid := "plex://movie/5d7768265af944001f1f6976"
			h.guid.forward[key] = guid
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if err := h.svc.client.postState(ctx, viewerToken, 42, postItem{GUID: guid, ViewedAt: h.now.Now().UnixMilli(), ViewState: "complete"}); err != nil {
				t.Fatal(err)
			}
			// Simulate a row that had already been claimed or attempted before upgrade.
			if _, err := h.svc.db.ExecContext(ctx, `UPDATE state_sync_journal SET attempts=1, last_error='plexwatch: status 500'`); err != nil {
				t.Fatal(err)
			}
			if disconnected {
				if _, err := h.svc.Disconnect(ctx, 2, 42); err != nil {
					t.Fatal(err)
				}
			}
			for _, q := range []string{`DROP TABLE plexwatch_attempt`, `ALTER TABLE state_sync_journal DROP COLUMN never_sent`, `DELETE FROM schema_migrations WHERE component='watchsend' OR (component='state_plexwatch' AND version=4)`} {
				if _, err := h.svc.db.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Migrate(ctx, h.svc.db); err != nil {
				t.Fatal(err)
			}
			if disconnected {
				h.connect(t, 2, 42)
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
				t.Fatal(err)
			}
			h.now.Add(15 * 24 * time.Hour)
			if err := h.st.CompactSync(ctx, h.now.Now()); err != nil {
				t.Fatal(err)
			}
			if err := h.st.Unscrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			// Without the original GUID, conservatively fence this account/aspect.
			other := core.MovieKey(7)
			h.guid.forward[other] = "plex://movie/5d776b8ead5437001f7a3218"
			if err := h.st.Scrobble(ctx, 2, other); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			n := len(h.fake.posts)
			h.fake.mu.Unlock()
			if n != 1 {
				t.Fatalf("legacy send replayed: %d", n)
			}
			st, err := h.svc.Status(ctx, 2, 42)
			if err != nil || st.Held != 1 || st.HeldLegacy != 1 {
				t.Fatalf("legacy status %+v %v", st, err)
			}
			t.Logf("legacy attempted row: original POST retained, migration/retry/supersession/compaction do not reopen delivery")
		})
	}
}

// ETHE-25: a legacy writer can leave an unproven row after schema upgrade;
// merely marking that queue row dead would let compaction erase the hold.
func TestUncertainSendLegacyRowAfterUpgrade(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(550)
	guid := "plex://movie/5d7768265af944001f1f6976"
	h.guid.forward[key] = guid
	// Released writers omit never_sent, retaining its conservative default.
	if _, err := h.svc.db.Exec(`INSERT INTO state_sync_journal(user_id,rating_key,aspect,kind,occurred_at,consent_generation) SELECT 2,?,'watch','complete',?,generation FROM plexwatch_fence WHERE user_id=2`, int64(key), h.now.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.client.postState(ctx, viewerToken, 42, postItem{GUID: guid, ViewedAt: h.now.Now().UnixMilli(), ViewState: "complete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.now.Add(15 * 24 * time.Hour)
	if err := h.st.CompactSync(ctx, h.now.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Unscrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 {
		t.Fatalf("compaction erased legacy hold: %d POSTs", len(h.fake.posts))
	}
}

func TestUncertainSendReconcileReceiptAndRelease(t *testing.T) {
	t.Run("receipt_when_plex_has_state", func(t *testing.T) {
		h := newHarness(t, 42)
		ctx := context.Background()
		h.connect(t, 2, 42)
		if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
			t.Fatal(err)
		}
		key := core.MovieKey(550)
		guid := "plex://movie/5d7768265af944001f1f6976"
		h.guid.forward[key] = guid
		h.fake.postStatus = 500
		if err := h.st.Scrobble(ctx, 2, key); err != nil {
			t.Fatal(err)
		}
		viewedAt := h.now.Now().UnixMilli()
		if _, err := h.svc.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
		st, err := h.svc.Status(ctx, 2, 42)
		if err != nil || st.Held != 1 {
			t.Fatalf("expected hold after 500: %+v %v", st, err)
		}
		// Upstream actually applied the state before returning 500.
		h.fake.stateBody = fmt.Sprintf(`{"items":[{"guid":%q,"viewedAt":%d,"viewState":"complete"}],"pageInfo":{"endCursor":"c1","hasNextPage":false}}`, guid, viewedAt)
		resolved, err := h.svc.Resolve(ctx, 2, 42)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Held != 0 || resolved.Dead != 0 || resolved.Pending != 0 {
			t.Fatalf("expected hold receipted and cleared: %+v", resolved)
		}
		h.fake.mu.Lock()
		postsAfterResolve := len(h.fake.posts)
		h.fake.postStatus = 201
		h.fake.mu.Unlock()
		if postsAfterResolve != 1 {
			t.Fatalf("Resolve re-POSTed: %d posts", postsAfterResolve)
		}
		var receipts, echoes, acks int
		if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM plexwatch_attempt WHERE receipt_at<>0`).Scan(&receipts); err != nil || receipts != 1 {
			t.Fatalf("receipts=%d %v", receipts, err)
		}
		if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM plexwatch_sent WHERE user_id=2 AND guid=? AND kind='complete'`, guid).Scan(&echoes); err != nil || echoes != 1 {
			t.Fatalf("echoes=%d %v", echoes, err)
		}
		if err := h.svc.db.QueryRow(`SELECT COUNT(*) FROM state_sync_journal WHERE delivered_at<>0`).Scan(&acks); err != nil || acks != 1 {
			t.Fatalf("acks=%d %v", acks, err)
		}
		// Subsequent change to the same title is now admitted and sent.
		if err := h.st.Unscrobble(ctx, 2, key); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		if len(h.fake.posts) != 2 || h.fake.posts[1].ViewState != "unplayed" {
			t.Fatalf("subsequent change after receipt not delivered: %+v", h.fake.posts)
		}
	})

	t.Run("release_when_absent_and_no_live_permit", func(t *testing.T) {
		for _, trigger := range []string{"schedule", "resolve", "reconnect", "consent", "remap"} {
			t.Run(trigger, func(t *testing.T) {
				h := newHarness(t, 42)
				ctx := context.Background()
				h.connect(t, 2, 42)
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
				key := core.MovieKey(550)
				guid := "plex://movie/5d7768265af944001f1f6976"
				h.guid.forward[key] = guid
				h.fake.postStatus = 500
				if err := h.st.Scrobble(ctx, 2, key); err != nil {
					t.Fatal(err)
				}
				if _, err := h.svc.DeliverOnce(ctx); err != nil {
					t.Fatal(err)
				}
				// While a permit is live in plexwatch_permit, absence must not release the hold.
				if _, err := h.svc.db.ExecContext(ctx, `INSERT INTO plexwatch_permit(permit_id, user_id, generation, until_at) SELECT attempt_id, user_id, generation, ? FROM plexwatch_attempt WHERE user_id=2`, h.now.Now().Add(time.Minute).UnixNano()); err != nil {
					t.Fatal(err)
				}
				if st, err := h.svc.Resolve(ctx, 2, 42); err != nil || st.Held != 1 {
					t.Fatalf("released while permit still live: %+v %v", st, err)
				}
				// Advance past the permit lease.
				h.now.Add(3 * time.Minute)
				h.fake.postStatus = 201
				switch trigger {
				case "schedule":
					h.svc.deliverAvailable(ctx)
				case "resolve":
					st, err := h.svc.Resolve(ctx, 2, 42)
					if err != nil || st.Held != 0 {
						t.Fatalf("Resolve did not release absent hold: %+v %v", st, err)
					}
					if _, err := h.svc.DeliverOnce(ctx); err != nil {
						t.Fatal(err)
					}
				case "reconnect":
					if _, err := h.svc.BeginPIN(ctx, 2, 42); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.FinishPIN(ctx, 2, 42); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
						t.Fatal(err)
					}
					if err := h.st.Scrobble(ctx, 2, key); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.DeliverOnce(ctx); err != nil {
						t.Fatal(err)
					}
				case "consent":
					if _, err := h.svc.SetEnabled(ctx, 2, 42, false); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
						t.Fatal(err)
					}
					if err := h.st.Scrobble(ctx, 2, key); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.DeliverOnce(ctx); err != nil {
						t.Fatal(err)
					}
				case "remap":
					remapped := "plex://movie/5d776b8ead5437001f7a3218"
					h.guid.forward[key] = remapped
					if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
						t.Fatal(err)
					}
					if _, err := h.svc.DeliverOnce(ctx); err != nil {
						t.Fatal(err)
					}
				}
				st, err := h.svc.Status(ctx, 2, 42)
				if err != nil || st.Held != 0 {
					t.Fatalf("%s left hold up: %+v %v", trigger, st, err)
				}
				h.fake.mu.Lock()
				n := len(h.fake.posts)
				h.fake.mu.Unlock()
				if n != 2 {
					t.Fatalf("%s expected 1 fresh admission after release, got %d total POSTs", trigger, n)
				}
			})
		}
	})
}

func TestDefinitiveRefusalClearsAdmissionFence(t *testing.T) {
	for _, code := range []int{400, 401, 403, 422} {
		t.Run(itoa(int64(code)), func(t *testing.T) {
			h := newHarness(t, 42)
			ctx := context.Background()
			h.connect(t, 2, 42)
			if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
				t.Fatal(err)
			}
			key := core.MovieKey(550)
			guid := "plex://movie/5d7768265af944001f1f6976"
			h.guid.forward[key] = guid
			h.fake.postStatus = code
			if err := h.st.Scrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			st, err := h.svc.Status(ctx, 2, 42)
			if err != nil || st.Held != 0 {
				t.Fatalf("status %d left hold fenced: %+v %v", code, st, err)
			}
			h.fake.postStatus = 201
			if code == 401 {
				h.connect(t, 2, 42)
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
			} else if code == 403 {
				if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.st.Unscrobble(ctx, 2, key); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.DeliverOnce(ctx); err != nil {
				t.Fatal(err)
			}
			h.fake.mu.Lock()
			defer h.fake.mu.Unlock()
			if len(h.fake.posts) != 2 || h.fake.posts[1].ViewState != "unplayed" {
				t.Fatalf("status %d blocked later change for title: %+v", code, h.fake.posts)
			}
		})
	}
}

func TestEligibilityDeferAndRetryRevives(t *testing.T) {
	h := newHarness(t, 42)
	ctx := context.Background()
	h.connect(t, 2, 42)
	if _, err := h.svc.SetEnabled(ctx, 2, 42, true); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(550)
	guid := "plex://movie/5d7768265af944001f1f6976"
	h.guid.forward[key] = guid
	if err := h.st.Scrobble(ctx, 2, key); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.db.ExecContext(ctx, `UPDATE plexwatch_account SET eligibility='refused' WHERE user_id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var dead int
	var lastErr string
	var nextAttempt int64
	if err := h.svc.db.QueryRow(`SELECT dead, last_error, next_attempt FROM state_sync_journal WHERE user_id=2`).Scan(&dead, &lastErr, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if dead != 0 || lastErr != "not_eligible" || nextAttempt <= h.now.Now().UnixNano() {
		t.Fatalf("expected 15m defer for not_eligible, got dead=%d lastErr=%q nextAttempt=%d", dead, lastErr, nextAttempt)
	}
	// Once eligibility is allowed again, Retry revives the deferred row immediately without waiting 15 minutes.
	h.fake.eligible = http.StatusOK
	if _, err := h.svc.Retry(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.posts) != 1 || h.fake.posts[0].GUID != guid {
		t.Fatalf("Retry did not revive not_eligible row: %+v", h.fake.posts)
	}
}

// OpenPath gives SQLite files; OpenWith preserves an explicitly scoped
// disposable PostgreSQL schema across independent worker processes.
func openCrashDB(ctx context.Context, path string) (*sql.DB, error) {
	if strings.HasPrefix(path, "postgres://") || strings.HasPrefix(path, "postgresql://") {
		db, _, err := store.OpenWith(ctx, store.Options{URL: path, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		return db, err
	}
	return store.OpenPath(ctx, path)
}
