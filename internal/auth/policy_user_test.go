package auth

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestPolicyUserReadsDurableRestrictions(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,active,plex_sections) VALUES(71,71,1,'[1]')`); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db}
	// A remote writer need not have published a process-cache invalidation yet.
	u, err := s.PolicyUser(ctx, 71)
	if err != nil || len(u.AllowedSections) != 1 || u.AllowedSections[0] != 1 {
		t.Fatalf("user=%+v %v", u, err)
	}
	if _, err := db.Exec(`UPDATE auth_users SET max_content_rating=2,allowed_sections='[2]' WHERE id=71`); err != nil {
		t.Fatal(err)
	}
	u, err = s.PolicyUser(ctx, 71)
	if err != nil || u.MaxContentRating != 2 || len(u.AllowedSections) != 1 || u.AllowedSections[0] != 2 {
		t.Fatalf("updated=%+v %v", u, err)
	}
	if _, err := db.Exec(`UPDATE auth_users SET active=0 WHERE id=71`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PolicyUser(ctx, 71); err == nil {
		t.Fatal("deactivated viewer accepted")
	}
}
