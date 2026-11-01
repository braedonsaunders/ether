package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestViewerTimestampsReflectStoredValues(t *testing.T) {
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(9,9,10000,11000)`); err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.ActiveUser(context.Background(), 9)
	if err != nil || v.CreatedAt.Unix() != 10000 || v.UpdatedAt.Unix() != 11000 {
		t.Fatalf("viewer %+v %v", v, err)
	}
	b, err := json.Marshal(v)
	if err != nil || strings.Contains(string(b), "CreatedAt") || strings.Contains(string(b), "createdAt") {
		t.Fatal("timestamp changed existing JSON", string(b), err)
	}
}
