package admin

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexwatch"
)

func TestFindMatchingViewer(t *testing.T) {
	viewers := []ViewerRecord{
		{User: core.User{ID: 1, IsOwner: true, Username: "owner", Title: "Server Owner"}},
		{User: core.User{ID: 2, PlexID: 2097560, Username: "deharryson1", Title: "deharryson1", Email: "deharryson@aol.com"}},
		{User: core.User{ID: 3, Title: "Em NY", Username: "emny"}},
		{User: core.User{ID: 4, Title: "Dronde"}},
	}

	// 1. Match owner
	m := findMatchingViewer(viewers, plexwatch.PMSAccount{ID: 1, Name: "owner", Owner: true})
	if m == nil || m.ID != 1 {
		t.Fatalf("expected owner match ID 1, got %+v", m)
	}

	// 2. Match by PlexID directly
	m = findMatchingViewer(viewers, plexwatch.PMSAccount{ID: 2097560, Name: "Someone Else", Owner: false})
	if m == nil || m.ID != 2 {
		t.Fatalf("expected match by PlexID ID 2, got %+v", m)
	}

	// 3. Match with space trimming / casing
	m = findMatchingViewer(viewers, plexwatch.PMSAccount{ID: 999, Name: " em ny ", Owner: false})
	if m == nil || m.ID != 3 {
		t.Fatalf("expected match for Em NY ID 3, got %+v", m)
	}

	// 4. Match Dronde
	m = findMatchingViewer(viewers, plexwatch.PMSAccount{ID: 888, Name: "dronde", Owner: false})
	if m == nil || m.ID != 4 {
		t.Fatalf("expected match for Dronde ID 4, got %+v", m)
	}

	// 5. No match
	m = findMatchingViewer(viewers, plexwatch.PMSAccount{ID: 777, Name: "NonExistent", Owner: false})
	if m != nil {
		t.Fatalf("expected nil match, got %+v", m)
	}
}
