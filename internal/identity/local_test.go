package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLocal(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO auth_users (id,plex_id,username,title,email,created_at,updated_at) VALUES (1, 42, 'different', 'Display', 'mail@example.com', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Create(ctx, 1, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if err = l.Create(ctx, 1, "alice", "another"); err == nil {
		t.Fatal("duplicate username accepted")
	}
	if err = l.Create(ctx, 1, "empty", ""); err == nil {
		t.Fatal("empty password accepted")
	}
	for _, name := range []string{"Display", "mail@example.com", "different", "ALICE"} {
		if _, _, err := l.Authenticate(ctx, name, "secret"); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if _, _, err := l.Authenticate(ctx, "alice", ""); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if id, link, err := l.Authenticate(ctx, "alice", "secret"); err != nil || id != 1 || link == 0 {
		t.Fatalf("%d %d %v", id, link, err)
	}
	if err := l.SetPassword(ctx, 1, "alice", "new secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Authenticate(ctx, "alice", "secret"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, _, err := l.Authenticate(ctx, "alice", "new secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Authenticate(ctx, "alice", "new secret"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestLegacyArgon2idStillVerifiesAndRewrites(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users (id,plex_id,username,title,email,created_at,updated_at) VALUES (1, 42, 'alice', 'Alice', 'a@example.com', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	legacy, err := hashPasswordWith(argonLegacy, "secret")
	if err != nil || !strings.HasPrefix(legacy, argonLegacy.prefix()) {
		t.Fatal(legacy, err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO identity_links (viewer_id,provider,subject,display,verified_at,password_hash) VALUES (1,'local','alice','alice',1,?)`, legacy); err != nil {
		t.Fatal(err)
	}
	l, err := NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = l.Authenticate(ctx, "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var stored string
		if err = db.QueryRowContext(ctx, `SELECT password_hash FROM identity_links WHERE subject='alice'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(stored, argonCurrent.prefix()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(stored)
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := hashPassword("secret")
	if err != nil || !strings.HasPrefix(current, argonCurrent.prefix()) || verifyPassword(current, "nope") {
		t.Fatal(current, err)
	}
}

func FuzzLocalHashParsing(f *testing.F) {
	f.Add("", "")
	f.Add("$argon2id$v=19$m=999999999,t=3,p=1$bad$bad", "password")
	f.Fuzz(func(t *testing.T, hash, password string) { verifyPassword(hash, password) })
}
