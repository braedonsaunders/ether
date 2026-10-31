package sessions

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSourceOffersSharedExpiryRevocationAndNoABA(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now().UTC()
	denied := false
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if denied {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	broker := &Broker{deps: BrokerDeps{DB: db, Now: func() time.Time { return now }, ActiveViewer: active, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		return []core.ReleaseVariant{{ID: "private-release"}}, nil
	}}}
	first, err := NewSourceOffers(db, broker, "silo")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSourceOffers(db, broker, "silo")
	if err != nil {
		t.Fatal(err)
	}
	scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "silo"}
	item := core.MovieKey(1)
	ps := &core.PlaySession{Resolution: &core.Resolution{Release: core.Release{ID: "private-release"}}}
	req := frontend.PlanRequest{Viewer: scope.Viewer, Device: scope.Device, Protocol: scope.Protocol, Item: item}
	detail, err := broker.issueDetailSource(ctx, req, ps, "revision-one")
	if err != nil {
		t.Fatal(err)
	}
	// An offer never outlives a shorter actual underlying selector lifetime.
	underlying := now.Add(2 * time.Minute)
	if _, err = db.Exec(`UPDATE sessions_detail_sources SET expires=? WHERE handle=?`, underlying.UnixMilli(), detail); err != nil {
		t.Fatal(err)
	}
	target := frontend.SourceOfferTarget{Item: item, DetailSource: detail, ETag: "revision-one"}
	offer, err := first.Issue(ctx, scope, target)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(offer.ID, 10, 64)
	if err != nil || n <= 1<<31-1 || n > maxSourceOfferID || !offer.ExpiresAt.Equal(underlying.Truncate(time.Millisecond)) {
		t.Fatalf("offer %+v %v", offer, err)
	}
	got, err := second.Resolve(ctx, scope, offer.ID)
	if err != nil || got.Target != target {
		t.Fatalf("second node %+v %v", got, err)
	}
	wrong := scope
	wrong.Device.ID = "foreign"
	if _, err = second.Resolve(ctx, wrong, offer.ID); err == nil {
		t.Fatal("foreign device resolved")
	}
	wrong = scope
	wrong.Viewer = &core.Viewer{ID: 2}
	if _, err = second.Resolve(ctx, wrong, offer.ID); err == nil {
		t.Fatal("foreign viewer resolved")
	}
	wrong = scope
	wrong.Protocol = "jellyfin"
	if _, err = second.Resolve(ctx, wrong, offer.ID); err == nil {
		t.Fatal("foreign surface resolved")
	}
	if _, err = second.Resolve(ctx, scope, "1"); err == nil {
		t.Fatal("legacy compact ID resolved")
	}
	denied = true
	if _, err = second.Resolve(ctx, scope, offer.ID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	denied = false
	first.(*sourceOffers).limits = capability.Limits{PerScope: 1, PerShard: 1}
	// Renewal at a full quota returns the same numeric identity.
	again, err := first.Issue(ctx, scope, target)
	if err != nil || again.ID != offer.ID {
		t.Fatalf("renew %+v %v", again, err)
	}
	if err = second.Revoke(ctx, scope, offer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Resolve(ctx, scope, offer.ID); err == nil {
		t.Fatal("revoked header resolved")
	}
	replacement, err := first.Issue(ctx, scope, target)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == offer.ID {
		t.Fatal("deleted ID rebound")
	}
	// Expired header rows and their typed payload are pruned atomically.
	now = now.Add(3 * time.Minute)
	if _, err = second.Resolve(ctx, scope, replacement.ID); err == nil {
		t.Fatal("expired offer resolved")
	}
	detail, err = broker.issueDetailSource(ctx, req, ps, "revision-one")
	if err != nil {
		t.Fatal(err)
	}
	target.DetailSource = detail
	replacement2, err := first.Issue(ctx, scope, target)
	if err != nil {
		t.Fatal(err)
	}
	if replacement2.ID == replacement.ID || replacement2.ID == offer.ID {
		t.Fatal("expired ID rebound")
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_source_offers`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expired payload not removed %d %v", count, err)
	}
	// Underlying revocation is independently authoritative.
	if _, err = db.Exec(`DELETE FROM sessions_detail_sources WHERE handle=?`, detail); err != nil {
		t.Fatal(err)
	}
	if _, err = second.Resolve(ctx, scope, replacement2.ID); err == nil {
		t.Fatal("revoked underlying selector resolved")
	}
}

func TestSourceOffersBatchQuotaRollbackAndConcurrentIdentity(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now().UTC()
	b := &Broker{deps: BrokerDeps{DB: db, Now: func() time.Time { return now }, ActiveViewer: func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		return []core.ReleaseVariant{{ID: "one"}, {ID: "two"}}, nil
	}}}
	versionAPI, err := NewVersionHandles(db, b, "silo")
	if err != nil {
		t.Fatal(err)
	}
	scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "silo"}
	item := core.MovieKey(1)
	handles, err := versionAPI.(frontend.VersionHandleBatch).IssueBatch(ctx, scope, item, []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewSourceOffers(db, b, "silo")
	if err != nil {
		t.Fatal(err)
	}
	service.(*sourceOffers).limits = capability.Limits{PerScope: 1, PerShard: 1}
	targets := []frontend.SourceOfferTarget{{Item: item, VersionHandle: handles["one"]}, {Item: item, VersionHandle: handles["two"]}}
	var capacity *capability.CapacityError
	if _, err = service.IssueBatch(ctx, scope, targets); !errors.As(err, &capacity) {
		t.Fatalf("quota %v", err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_source_offers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial payload %d %v", count, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, sourceOfferNamespace).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial header %d %v", count, err)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			offer, err := service.Issue(ctx, scope, targets[0])
			if err != nil {
				t.Error(err)
				return
			}
			ids <- offer.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for candidate := range ids {
		if id != "" && id != candidate {
			t.Fatal("duplicate live identities")
		}
		id = candidate
	}
	if id == "" {
		t.Fatal("no offers")
	}
	service.(*sourceOffers).limits = capability.Limits{PerScope: 2, PerShard: 2}
	if store.DialectOf(db) == store.Postgres {
		if _, err = db.Exec(`SELECT setval(pg_get_serial_sequence('sessions_source_offers','id'),9007199254740991,true)`); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err = db.Exec(`UPDATE sqlite_sequence SET seq=9007199254740991 WHERE name='sessions_source_offers'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = service.Issue(ctx, scope, targets[1]); err == nil {
		t.Fatal("exhausted numeric ID range accepted")
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_source_offers`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("exhaustion payload rollback %d %v", count, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, sourceOfferNamespace).Scan(&count); err != nil || count != 1 {
		t.Fatalf("exhaustion header rollback %d %v", count, err)
	}

}
