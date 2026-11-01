package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFrontendPreferencesIsolationLimitsAndRemoval(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,101,0,0),(2,102,0,0)`); err != nil {
		t.Fatal(err)
	}
	a, b := &core.Viewer{ID: 1}, &core.Viewer{ID: 2}
	jelly := *identity.NewPreferences(db, "jellyfin")
	emby := *identity.NewPreferences(db, "emby")
	if err := jelly.Put(ctx, a, "tv", "home", json.RawMessage(`{"view":"grid"}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := jelly.Get(ctx, a, "tv", "home"); err != nil || string(got) != `{"view":"grid"}` {
		t.Fatal(string(got), err)
	}
	for _, tc := range []struct {
		s           identity.Preferences
		v           *core.Viewer
		client, key string
	}{{emby, a, "tv", "home"}, {jelly, b, "tv", "home"}, {jelly, a, "phone", "home"}} {
		if _, err := tc.s.Get(ctx, tc.v, tc.client, tc.key); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("leaked preference", err)
		}
	}
	if err := jelly.Put(ctx, a, "", "home", json.RawMessage(`{}`)); !errors.Is(err, frontend.ErrInvalidPreferences) {
		t.Fatal(err)
	}
	if err := jelly.Put(ctx, a, strings.Repeat("x", 129), "home", json.RawMessage(`{}`)); !errors.Is(err, frontend.ErrInvalidPreferences) {
		t.Fatal(err)
	}
	if err := jelly.Put(ctx, a, "tv", "home", json.RawMessage(`bad`)); !errors.Is(err, frontend.ErrInvalidPreferences) {
		t.Fatal(err)
	}
	if err := jelly.Put(ctx, a, "tv", "home", json.RawMessage(`"`+strings.Repeat("x", 64*1024)+`"`)); !errors.Is(err, frontend.ErrPreferencesLimit) {
		t.Fatal(err)
	}
	for i := 1; i < 256; i++ {
		if err := emby.Put(ctx, a, "tv", fmt.Sprintf("key-%d", i), json.RawMessage(`{}`)); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := jelly.Put(ctx, a, "tv", "overflow", json.RawMessage(`{}`)); !errors.Is(err, frontend.ErrPreferencesLimit) {
		t.Fatal(err)
	}
	if err := jelly.Put(ctx, a, "tv", "home", json.RawMessage(`{"view":"list"}`)); err != nil {
		t.Fatal("update at cap", err)
	}
	if err := jelly.Delete(ctx, a, "tv", "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := jelly.Get(ctx, a, "tv", "home"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := jelly.Put(ctx, a, "tv", "home", json.RawMessage(`{}`)); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM auth_users WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_preferences WHERE viewer_id=1`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestFrontendPreferencesConcurrentQuota(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,101,0,0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256-1; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO frontend_preferences(viewer_id,surface,client,pref_key,value) VALUES(1,'jellyfin','tv',?, '{}')`, fmt.Sprintf("key-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"final-a", "final-b"} {
		go func(key string) {
			<-start
			results <- (*identity.NewPreferences(db, "jellyfin")).Put(ctx, &core.Viewer{ID: 1}, "tv", key, json.RawMessage(`{}`))
		}(key)
	}
	close(start)
	succeeded := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			succeeded++
		} else if !errors.Is(err, frontend.ErrPreferencesLimit) {
			t.Fatal(err)
		}
	}
	if succeeded != 1 {
		t.Fatal("quota accepted", succeeded)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_preferences WHERE viewer_id=1`).Scan(&count); err != nil || count != 256 {
		t.Fatal(count, err)
	}
}
