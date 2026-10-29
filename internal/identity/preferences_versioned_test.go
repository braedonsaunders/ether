package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVersionedPreferencesConcurrentWrites(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	nodes := []*identity.Preferences{identity.NewPreferences(db, "silo"), identity.NewPreferences(db, "silo")}
	viewer := &core.Viewer{ID: 1}
	ctx := context.Background()
	var wg sync.WaitGroup
	revisions := make(chan int64, 20)
	errs := make(chan error, 20)
	for j := 0; j < 20; j++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev, _, err := nodes[i%2].PutVersioned(ctx, viewer, "settings", "theme", json.RawMessage(`"dark"`))
			if err != nil {
				errs <- err
				return
			}
			revisions <- rev
		}(j)
	}
	wg.Wait()
	close(errs)
	close(revisions)
	for err := range errs {
		t.Error(err)
	}
	seen := map[int64]bool{}
	for revision := range revisions {
		if revision < 1 || seen[revision] {
			t.Errorf("duplicate revision %d", revision)
		}
		seen[revision] = true
	}
	_, rev, _, err := nodes[1].GetVersioned(ctx, viewer, "settings", "theme")
	if err != nil || rev != 20 || len(seen) != 20 {
		t.Fatalf("revision %d (%d writes): %v", rev, len(seen), err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET active=0 WHERE id=1`); err != nil { t.Fatal(err) }
	if _, _, err = nodes[0].PutVersioned(ctx, viewer, "settings", "theme", json.RawMessage(`"light"`)); !errors.Is(err,core.ErrUnauthorized) { t.Fatalf("inactive write: %v",err) }
}
