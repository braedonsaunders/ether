package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/identity"
	"testing"
)

func TestCreateDLNAGuestIsRestrictedAndPasswordless(t *testing.T) {
	a := setupTestApp(t, true)
	viewer, err := a.createDLNAGuest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	policy := a.safety.Get(viewer.ID)
	if policy.MaxContentRating != "PG" || len(policy.BlockedKeywords) == 0 {
		t.Fatalf("policy: %+v", policy)
	}
	local, _ := identity.NewLocal(a.db)
	hasPassword, err := local.HasPassword(context.Background(), viewer.ID)
	if err != nil || hasPassword {
		t.Fatalf("guest password: %v %v", hasPassword, err)
	}
}
