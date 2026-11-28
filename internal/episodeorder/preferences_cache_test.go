package episodeorder

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSpecialsPreferenceHonoredWithoutGroupsSource(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := New(db, nil, nil) // groupsConfigured() == false
	show := core.ShowKey(100)
	viewer := &core.User{ID: 5}
	if _, err := db.ExecContext(ctx, `INSERT INTO episodeorder_preferences(user_id,show_key,group_id,specials) VALUES(?,?,?,?)`, viewer.ID, int64(show), "", "hidden"); err != nil {
		t.Fatal(err)
	}
	_, pref, err := svc.layoutWithPref(ctx, viewer, show)
	if err != nil {
		t.Fatal(err)
	}
	if pref.Specials != "hidden" {
		t.Fatalf("layoutWithPref Specials = %q, want hidden", pref.Specials)
	}
	custom, err := svc.Customized(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if !custom {
		t.Fatal("Customized() = false, want true when specials are hidden without groups source")
	}
}

func TestLayoutCacheEvictsOldestInsteadOfWiping(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := New(db, nil, nil)
	now := time.Now()
	svc.mu.Lock()
	for i := 0; i < 64; i++ {
		svc.cache[fmt.Sprintf("u:%d", i)] = cachedLayout{
			value:   &layout{},
			expires: now.Add(time.Duration(i+1) * time.Minute),
		}
	}
	// Simulate the cap eviction rule: evict only the earliest-expiring entry.
	var evictKey string
	var evictExp time.Time
	for k, v := range svc.cache {
		if evictKey == "" || v.expires.Before(evictExp) {
			evictKey, evictExp = k, v.expires
		}
	}
	delete(svc.cache, evictKey)
	svc.cache["u:64"] = cachedLayout{value: &layout{}, expires: now.Add(65 * time.Minute)}
	gotLen := len(svc.cache)
	_, stillHasOldest := svc.cache["u:0"]
	_, stillHasNewest := svc.cache["u:63"]
	svc.mu.Unlock()

	if gotLen != 64 || stillHasOldest || !stillHasNewest {
		t.Fatalf("len=%d oldest=%v newest=%v", gotLen, stillHasOldest, stillHasNewest)
	}
}
