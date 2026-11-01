package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

type pausedSharingValidator struct {
	checking chan struct{}
	resume   chan struct{}
}

func (v pausedSharingValidator) Account(context.Context, string) (*plextv.Account, error) {
	return &plextv.Account{ID: 444}, nil
}
func (v pausedSharingValidator) HasServer(ctx context.Context, _ string) (bool, error) {
	close(v.checking)
	select {
	case <-v.resume:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// With ETHER_TEST_POSTGRES this uses a live server and independent Service
// instances, so a process-local mutex cannot make the assertion pass.
func TestPlexLinkConcurrentSharingEpoch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,origin,created_at,updated_at) VALUES(20,NULL,'local',1,1)`); err != nil {
		t.Fatal(err)
	}
	validator := pausedSharingValidator{make(chan struct{}), make(chan struct{})}
	linking, err := New(ctx, db, Options{Validator: validator})
	if err != nil {
		t.Fatal(err)
	}
	sharing, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := db.QueryRowContext(ctx, `SELECT epoch FROM auth_membership_epoch WHERE id=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); result <- linking.LinkPlex(ctx, 20, "fixture-token") }()
	defer func() { cancel(); <-finished }()

	select {
	case <-validator.checking:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := sharing.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT epoch FROM auth_membership_epoch WHERE id=1`).Scan(&after); err != nil || after != before+1 {
		t.Fatalf("epoch %d -> %d: %v", before, after, err)
	}
	close(validator.resume)
	select {
	case err := <-result:
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("stale sharing check: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var links int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity_links WHERE subject='444'`).Scan(&links); err != nil || links != 0 {
		t.Fatalf("stale link rows %d: %v", links, err)
	}
}
