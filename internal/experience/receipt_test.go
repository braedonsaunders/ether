package experience

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestReceiptsSharedScopeAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Unix(1000, 0)
	active := true
	allowed := true
	calls := 0
	opts := ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 2, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active {
			return nil, errors.New("revoked")
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return allowed }, Record: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, s ReceiptSubject, _ frontend.RowInteractionKind) error {
		calls++
		if s.ID != "movie" {
			t.Fatal("subject changed")
		}
		return nil
	}}
	a, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewReceipts(opts)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 1}
	dev := core.Device{ID: "tv"}
	h, err := a.Issue(ctx, viewer, dev, "plex", ReceiptSubject{Kind: "title", ID: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	i := frontend.RowInteraction{Viewer: viewer, Device: dev, Protocol: "plex", Handle: h, Kind: frontend.RowImpression}
	if err = b.Report(ctx, i); err != nil {
		t.Fatal(err)
	}
	if err = a.Report(ctx, i); err != nil || calls != 1 {
		t.Fatalf("duplicate: calls=%d err=%v", calls, err)
	}
	i.Kind = frontend.RowClick
	if err = b.Report(ctx, i); err != nil || calls != 2 {
		t.Fatalf("separate action: %d %v", calls, err)
	}
	i.Protocol = "jellyfin"
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("wrong surface accepted")
	}
	i.Protocol = "plex"
	i.Device.ID = "other"
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("wrong device accepted")
	}
	i.Device = dev
	i.Viewer = &core.User{ID: 2}
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("wrong viewer accepted")
	}
	i.Viewer = viewer
	i.Handle = "invalid"
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("forged receipt accepted")
	}
	i.Handle = h
	allowed = false
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("revoked policy accepted")
	}
	allowed = true
	active = false
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("inactive viewer accepted")
	}
	active = true
	now = now.Add(time.Minute)
	if !errors.Is(b.Report(ctx, i), ErrReceipt) {
		t.Fatal("expired receipt accepted")
	}
}
func TestReceiptCapAndHashStorage(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Unix(1000, 0)
	s, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 1, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	d := core.Device{ID: "tv"}
	subject := ReceiptSubject{Kind: "row", ID: "r"}
	h, err := s.Issue(ctx, u, d, "plex", subject)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = db.QueryRowContext(ctx, `SELECT hash FROM experience_receipts`).Scan(&stored); err != nil || stored == h {
		t.Fatal("raw receipt persisted")
	}
	if _, err = s.Issue(ctx, u, d, "plex", subject); err == nil {
		t.Fatal("cap not enforced")
	}
	now = now.Add(time.Minute)
	if _, err = s.Issue(ctx, u, d, "plex", subject); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptConcurrentDuplicate(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	var calls atomic.Int64
	o := ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 8, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool { return true }, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		calls.Add(1)
		return nil
	}}
	a, err := NewReceipts(o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewReceipts(o)
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	d := core.Device{ID: "tv"}
	h, err := a.Issue(ctx, u, d, "plex", ReceiptSubject{Kind: "row", ID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	i := frontend.RowInteraction{Viewer: u, Device: d, Protocol: "plex", Handle: h, Kind: frontend.RowImpression}
	errs := make(chan error, 2)
	go func() { errs <- a.Report(ctx, i) }()
	go func() { errs <- b.Report(ctx, i) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("recorded %d times", calls.Load())
	}
}

func TestReceiptAuthorizationCanReadSameDB(t *testing.T) {
	db := store.OpenTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	check := func() {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&n); err != nil {
			t.Errorf("authorization read: %v", err)
		}
	}
	s, err := NewReceipts(ReceiptOptions{DB: db, TTL: time.Minute, MaxPerDevice: 2, Active: func(_ context.Context, id int64) (*core.User, error) { check(); return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool {
		check()
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	d := core.Device{ID: "tv"}
	h, err := s.Issue(ctx, u, d, "plex", ReceiptSubject{Kind: "row", ID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Report(ctx, frontend.RowInteraction{Viewer: u, Device: d, Protocol: "plex", Handle: h, Kind: frontend.RowClick}); err != nil {
		t.Fatal(err)
	}
}
