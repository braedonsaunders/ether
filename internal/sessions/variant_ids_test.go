package sessions

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVariantRegistryPointLookups(t *testing.T) {
	db := store.OpenTest(t)
	key := core.MovieKey(603)
	first := NewVariantIDs(db, time.Now)
	wanted := first.IDFor(key, "selected")
	for i := int64(1); i <= 8; i++ {
		first.IDFor(core.MovieKey(i), "unrelated")
	}
	restarted := NewVariantIDs(db, time.Now)
	if len(restarted.byID) != 0 {
		t.Fatal("eager registry load")
	}
	ref, ok := restarted.Lookup(wanted)
	if !ok || ref.Key != key || ref.ReleaseID != "selected" {
		t.Fatalf("persisted lookup: %+v %v", ref, ok)
	}
	if len(restarted.byID) != 1 {
		t.Fatalf("lookup loaded %d mappings, want one", len(restarted.byID))
	}
	if id := restarted.IDFor(key, "selected"); id != wanted {
		t.Fatal("restart changed id")
	}
	// Occupied hash values are checked using a point read too.
	collision := VariantHash(key, "collision")
	if _, err := db.Exec(`INSERT INTO variants_ids(media_id,rating_key,release_id,last_seen) VALUES(?,?,?,?)`, collision, int64(core.MovieKey(99)), "occupied", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if got := restarted.IDFor(key, "collision"); got == collision {
		t.Fatal("persisted collision was overwritten")
	}
	if len(restarted.byID) > 3 {
		t.Fatal("collision scanned registry")
	}
}
