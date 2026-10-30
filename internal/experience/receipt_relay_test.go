package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// Gate, from this package directory:
//
//	go test ./internal/experience/ -count=1 -timeout 180s -run '^(TestReceipt|TestEntryReceipt)'
//
// PostgreSQL uses the same gate with ETHER_TEST_POSTGRES set to a reviewer-supplied
// server. OpenTest selects that dialect. Do not point it at production.
// This process does not switch MirrorHeaders on for callers that leave it unset.

func relayOptions(db *sql.DB, now *time.Time, max int, mirror bool, calls *int) ReceiptOptions {
	return ReceiptOptions{
		DB: db, TTL: time.Minute, MaxPerDevice: max, MirrorHeaders: mirror,
		Now: func() time.Time { return *now },
		Active: func(_ context.Context, id int64) (*core.User, error) {
			return &core.User{ID: id}, nil
		},
		Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool {
			return true
		},
		AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool {
			return true
		},
		Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
			if calls != nil {
				*calls++
			}
			return nil
		},
	}
}

func TestReceiptRelayLegacyAuthorityGate(t *testing.T) {
	switch store.DialectOf(store.OpenTest(t)) {
	case store.SQLite, store.Postgres:
	default:
		t.Fatal("dialect gate not sqlite or postgres")
	}
	t.Run("scope", func(t *testing.T) {
		ctx := context.Background()
		db := store.OpenTest(t)
		now := time.Unix(1_700_000_000, 0)
		calls := 0
		generation := int64(7)
		allowed := true
		opts := relayOptions(db, &now, 4, true, &calls)
		opts.Generation = func(context.Context, *core.User, core.Device, core.Protocol) (int64, error) {
			return generation, nil
		}
		opts.Allowed = func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool {
			return allowed
		}
		mirrored, err := NewReceipts(opts)
		if err != nil {
			t.Fatal(err)
		}
		plainOpts := opts
		plainOpts.MirrorHeaders = false
		plain, err := NewReceipts(plainOpts)
		if err != nil {
			t.Fatal(err)
		}
		viewer := &core.User{ID: 1}
		device := core.Device{ID: "tv"}
		subject := ReceiptSubject{Kind: "title", ID: "movie", Purpose: "recommendation", Rank: 2}
		token, err := mirrored.Issue(ctx, viewer, device, "plex", subject)
		if err != nil {
			t.Fatal(err)
		}
		var digest, surface, kind, id, purpose string
		var viewerID, expires, storedGen int64
		var rank int
		if err = db.QueryRowContext(ctx, `SELECT hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,generation FROM experience_receipts`).Scan(&digest, &viewerID, new(string), &surface, &expires, &kind, &id, &purpose, &rank, &storedGen); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
		if err != nil || len(raw) != 32 {
			t.Fatal("token bytes")
		}
		sum := sha256.Sum256(raw)
		wire := sha256.Sum256([]byte(token))
		if hex.EncodeToString(sum[:]) != digest || hex.EncodeToString(wire[:]) == digest || digest == token {
			t.Fatal("digest is not the raw-token hash")
		}
		if viewerID != 1 || surface != "plex" || kind != "title" || id != "movie" || purpose != "recommendation" || rank != 2 || storedGen != 7 {
			t.Fatal("authority scope changed")
		}
		var headerViewer, headerExpires int64
		var headerSurface, headerDevice string
		if err = db.QueryRowContext(ctx, `SELECT viewer_id,surface,device_id,expires FROM capability_headers WHERE namespace=? AND digest=?`, ReceiptCapabilityNamespace, digest).Scan(&headerViewer, &headerSurface, &headerDevice, &headerExpires); err != nil {
			t.Fatal(err)
		}
		if headerViewer != 1 || headerSurface != "plex" || headerDevice != "tv" || headerExpires != expires*1000 {
			t.Fatal("header bookkeeping diverged")
		}
		event := frontend.RowInteraction{Viewer: viewer, Device: device, Protocol: "plex", Handle: token, Kind: frontend.RowImpression}
		if err = plain.Report(ctx, event); err != nil || calls != 1 {
			t.Fatalf("legacy report of mirrored issue: calls=%d err=%v", calls, err)
		}
		if err = mirrored.Report(ctx, event); err != nil || calls != 1 {
			t.Fatalf("replay: calls=%d err=%v", calls, err)
		}
		event.Protocol = "jellyfin"
		if !errors.Is(plain.Report(ctx, event), ErrReceipt) {
			t.Fatal("wrong surface")
		}
		event.Protocol = "plex"
		event.Device.ID = "other"
		if !errors.Is(plain.Report(ctx, event), ErrReceipt) {
			t.Fatal("wrong device")
		}
		event.Device = device
		event.Viewer = &core.User{ID: 2}
		if !errors.Is(plain.Report(ctx, event), ErrReceipt) {
			t.Fatal("wrong viewer")
		}
		event.Viewer = viewer
		generation = 9
		event.Kind = frontend.RowClick
		if !errors.Is(mirrored.Report(ctx, event), ErrReceipt) || calls != 1 {
			t.Fatal("stale generation accepted")
		}
		generation = 7
		allowed = false
		if !errors.Is(mirrored.Report(ctx, event), ErrReceipt) || calls != 1 {
			t.Fatal("current policy ignored")
		}
		allowed = true
		if err = mirrored.Report(ctx, event); err != nil || calls != 2 {
			t.Fatalf("restored policy: calls=%d err=%v", calls, err)
		}
		if _, err = db.ExecContext(ctx, `DELETE FROM experience_receipts WHERE hash=?`, digest); err != nil {
			t.Fatal(err)
		}
		event.Kind = frontend.RowPlay
		if !errors.Is(mirrored.Report(ctx, event), ErrReceipt) || calls != 2 {
			t.Fatal("header authorized a missing receipt")
		}
		var headers int
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, ReceiptCapabilityNamespace, digest).Scan(&headers); err != nil || headers != 1 {
			t.Fatalf("orphan header count=%d err=%v", headers, err)
		}
	})
	t.Run("quota", func(t *testing.T) {
		ctx := context.Background()
		db := store.OpenTest(t)
		now := time.Unix(1_700_000_000, 0)
		s, err := NewReceipts(relayOptions(db, &now, 1, true, nil))
		if err != nil {
			t.Fatal(err)
		}
		viewer := &core.User{ID: 1}
		device := core.Device{ID: "tv"}
		subject := ReceiptSubject{Kind: "row", ID: "r"}
		if _, err = s.Issue(ctx, viewer, device, "plex", subject); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Issue(ctx, viewer, device, "plex", subject); err == nil || !strings.Contains(err.Error(), "receipt limit reached") {
			t.Fatalf("quota: %v", err)
		}
		if _, err = db.ExecContext(ctx, `DELETE FROM experience_receipts`); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Issue(ctx, viewer, device, "plex", subject); err != nil {
			t.Fatal("orphan header consumed quota")
		}
		var n int
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("authority rows=%d err=%v", n, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, ReceiptCapabilityNamespace).Scan(&n); err != nil || n != 2 {
			t.Fatalf("headers=%d err=%v", n, err)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		ctx := context.Background()
		db := store.OpenTest(t)
		now := time.Unix(1_700_000_000, 0)
		s, err := NewReceipts(relayOptions(db, &now, 4, true, nil))
		if err != nil {
			t.Fatal(err)
		}
		viewer := &core.User{ID: 1}
		device := core.Device{ID: "tv"}
		token, err := s.Issue(ctx, viewer, device, "plex", ReceiptSubject{Kind: "row", ID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		var digest string
		if err = db.QueryRowContext(ctx, `SELECT hash FROM experience_receipts`).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES(?,?,?,?,?,?,?)`, "image.grant", digest, int64(1), int64(1), "plex", "tv", now.Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err = s.Issue(ctx, viewer, device, "jellyfin", ReceiptSubject{Kind: "row", ID: "next"}); err != nil {
			t.Fatal(err)
		}
		var left int
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND digest=?`, ReceiptCapabilityNamespace, digest).Scan(&left); err != nil || left != 0 {
			t.Fatalf("expired header left=%d err=%v", left, err)
		}
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='image.grant' AND digest=?`, digest).Scan(&left); err != nil || left != 1 {
			t.Fatalf("foreign header left=%d err=%v", left, err)
		}
		if !errors.Is(s.Report(ctx, frontend.RowInteraction{Viewer: viewer, Device: device, Protocol: "plex", Handle: token, Kind: frontend.RowImpression}), ErrReceipt) {
			t.Fatal("expired receipt accepted")
		}
		var liveSurface string
		if err = db.QueryRowContext(ctx, `SELECT protocol FROM experience_receipts`).Scan(&liveSurface); err != nil || liveSurface != "jellyfin" {
			t.Fatalf("live authority=%s err=%v", liveSurface, err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		ctx := context.Background()
		db := store.OpenTest(t)
		now := time.Unix(1_700_000_000, 0)
		errBind := errors.New("bind failed")
		opts := relayOptions(db, &now, 1, true, nil)
		opts.bindHeaders = func(ctx context.Context, tx *sql.Tx, headers []capability.Header) error {
			if len(headers) != 1 || headers[0].Namespace != ReceiptCapabilityNamespace {
				t.Errorf("bind shape %+v", headers)
			}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE hash=?`, headers[0].Digest).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("bind ran without the authority row")
			}
			return errBind
		}
		s, err := NewReceipts(opts)
		if err != nil {
			t.Fatal(err)
		}
		viewer := &core.User{ID: 1}
		device := core.Device{ID: "tv"}
		if _, err = s.Issue(ctx, viewer, device, "plex", ReceiptSubject{Kind: "row", ID: "r"}); !errors.Is(err, errBind) {
			t.Fatal(err)
		}
		var n int
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rolled back rows=%d err=%v", n, err)
		}
		opts.bindHeaders = nil
		s, err = NewReceipts(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Issue(ctx, viewer, device, "plex", ReceiptSubject{Kind: "row", ID: "r"}); err != nil {
			t.Fatal("failed bind charged quota")
		}
	})
}

func TestReceiptRelayConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := store.OpenTest(t)
	now := time.Unix(1_700_000_000, 0)
	var calls int
	var mu sync.Mutex
	opts := relayOptions(db, &now, 1, true, nil)
	opts.Record = func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}
	s, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 1}
	device := core.Device{ID: "tv"}
	subject := ReceiptSubject{Kind: "row", ID: "r"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Issue(ctx, viewer, device, "plex", subject)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	ok, limited := 0, 0
	for err := range errs {
		if err == nil {
			ok++
			continue
		}
		if strings.Contains(err.Error(), "receipt limit reached") {
			limited++
			continue
		}
		t.Fatal(err)
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("issues ok=%d limited=%d", ok, limited)
	}
	var token string
	// The successful issue returned its token only inside the goroutine.
	// Report uses a fresh receipt on another device so the cap is not shared.
	other := device
	other.ID = "player"
	opts.MaxPerDevice = 2
	reporter, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	token, err = reporter.Issue(ctx, viewer, other, "plex", subject)
	if err != nil {
		t.Fatal(err)
	}
	event := frontend.RowInteraction{Viewer: viewer, Device: other, Protocol: "plex", Handle: token, Kind: frontend.RowImpression}
	errs = make(chan error, 2)
	start = make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- reporter.Report(ctx, event)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("recorded %d", got)
	}
	var headers int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND device_id=?`, ReceiptCapabilityNamespace, other.ID).Scan(&headers); err != nil || headers != 1 {
		t.Fatalf("headers=%d err=%v", headers, err)
	}
}
