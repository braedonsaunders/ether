package mediabrowser

import (
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestNavigationQueryUsesPolicyFilteredAnchors(t *testing.T) {
	anchor := ids.RatingKeyGUID(5)
	for _, tc := range []struct {
		query           string
		allowStart      bool
		adjacent, start frontend.RatingKey
		fail            bool
	}{
		{"adjacentTo=" + anchor, false, 5, 0, false},
		{"startItemId=" + anchor, true, 0, 5, false},
		{"startItemId=" + anchor, false, 0, 0, true},
		{"adjacentTo=" + anchor + "&adjacentTo=" + anchor, true, 0, 0, true},
		{"adjacentTo=" + anchor + "&startItemId=" + anchor, true, 0, 0, true},
		{"adjacentTo=invalid", true, 0, 0, true},
	} {
		q := frontend.ItemQuery{}
		err := navigationQuery(httptest.NewRequest("GET", "/Items?"+tc.query, nil), &q, tc.allowStart)
		if (err != nil) != tc.fail || q.AdjacentTo != tc.adjacent || q.StartAt != tc.start {
			t.Fatalf("%s: q=%+v err=%v", tc.query, q, err)
		}
	}
}
