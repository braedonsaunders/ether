package admin

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexwatch"
)

func TestFindExactViewer(t *testing.T) {
	viewers := []ViewerRecord{
		{User: core.User{ID: 1, IsOwner: true, Username: "owner", Title: "Server Owner"}},
		{User: core.User{ID: 2, PlexID: 2097560, Username: "deharryson1", Title: "deharryson1", Email: "deharryson@aol.com"}},
		{User: core.User{ID: 3, Title: "Mike", Username: "mike1"}},
		{User: core.User{ID: 4, Title: "Mike", Username: "mike2"}},
	}

	// 1. Existing mapping (acct.UserID > 0)
	m := findExactViewer(viewers, plexwatch.PMSAccount{ID: 999, Name: "Unknown", UserID: 3})
	if m == nil || m.ID != 3 {
		t.Fatalf("expected exact match by UserID 3, got %+v", m)
	}

	// 2. Owner match
	m = findExactViewer(viewers, plexwatch.PMSAccount{ID: 1, Name: "owner", Owner: true})
	if m == nil || m.ID != 1 {
		t.Fatalf("expected owner match ID 1, got %+v", m)
	}

	// 3. Stable PlexID match
	m = findExactViewer(viewers, plexwatch.PMSAccount{ID: 2097560, Name: "Someone Else", Owner: false})
	if m == nil || m.ID != 2 {
		t.Fatalf("expected match by PlexID ID 2, got %+v", m)
	}

	// 4. Two users named "Mike" must NOT match automatically during import
	m = findExactViewer(viewers, plexwatch.PMSAccount{ID: 888, Name: "Mike", Owner: false})
	if m != nil {
		t.Fatalf("expected nil exact match for unmapped name 'Mike', got %+v", m)
	}

	// 5. No match
	m = findExactViewer(viewers, plexwatch.PMSAccount{ID: 777, Name: "NonExistent", Owner: false})
	if m != nil {
		t.Fatalf("expected nil match, got %+v", m)
	}
}

func TestSuggestMatchingViewerPriority(t *testing.T) {
	// Viewer 1 has display name "Mike"
	// Viewer 2 has PlexID 55555 and username "othermike"
	viewers := []ViewerRecord{
		{User: core.User{ID: 1, Title: "Mike", Username: "mike_disp"}},
		{User: core.User{ID: 2, PlexID: 55555, Title: "Different Title", Username: "othermike"}},
	}

	// Account has ID 55555 and Name "Mike".
	// PlexID match (Viewer 2) MUST win over name match (Viewer 1).
	s := suggestMatchingViewer(viewers, plexwatch.PMSAccount{ID: 55555, Name: "Mike", Owner: false})
	if s == nil || s.ID != 2 {
		t.Fatalf("expected PlexID to win over name heuristic; got %+v", s)
	}

	// Heuristic match when no exact Plex ID exists
	s = suggestMatchingViewer(viewers, plexwatch.PMSAccount{ID: 99999, Name: "mike_disp", Owner: false})
	if s == nil || s.ID != 1 {
		t.Fatalf("expected heuristic match for mike_disp to ID 1; got %+v", s)
	}

	// Case insensitive and trimming match
	s = suggestMatchingViewer(viewers, plexwatch.PMSAccount{ID: 99999, Name: "  MIKE  ", Owner: false})
	if s == nil || s.ID != 1 {
		t.Fatalf("expected case-insensitive trimmed heuristic match to ID 1; got %+v", s)
	}

	// No match returns nil
	s = suggestMatchingViewer(viewers, plexwatch.PMSAccount{ID: 99999, Name: "Stranger", Owner: false})
	if s != nil {
		t.Fatalf("expected nil suggestion for Stranger, got %+v", s)
	}
}
