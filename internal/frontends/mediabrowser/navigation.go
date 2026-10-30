package mediabrowser

import (
	"errors"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// navigationQuery delegates neighbor and inclusive-start ordering to the
// policy-filtered Experience rather than scanning frontend-local pages.
func navigationQuery(r *http.Request, q *frontend.ItemQuery, allowStart bool) error {
	adjacent := route.Query(r.URL.Query(), "AdjacentTo")
	start := route.Query(r.URL.Query(), "StartItemId")
	if len(adjacent) > 1 || len(start) > 1 || (len(adjacent) == 1 && len(start) == 1) || (!allowStart && len(start) != 0) {
		return errors.New("invalid navigation anchor")
	}
	if len(adjacent) == 1 {
		key, err := ids.ParseRatingKeyGUID(adjacent[0])
		if err != nil {
			return err
		}
		q.AdjacentTo = key
	}
	if len(start) == 1 {
		key, err := ids.ParseRatingKeyGUID(start[0])
		if err != nil {
			return err
		}
		q.StartAt = key
	}
	return nil
}
