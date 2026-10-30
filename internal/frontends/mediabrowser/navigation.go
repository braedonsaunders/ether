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

// navigationWithVersion resolves a version anchor through the broker. The
// returned handle is preserved on the matching episode in the response.
func (i *instance) navigationWithVersion(r *http.Request, viewer *frontend.Viewer, q *frontend.ItemQuery, allowStart bool) (string, error) {
	err := navigationQuery(r, q, allowStart)
	if err == nil || !allowStart {
		return "", err
	}
	start := route.Query(r.URL.Query(), "StartItemId")
	if len(start) != 1 || len(route.Query(r.URL.Query(), "AdjacentTo")) != 0 {
		return "", err
	}
	scope, scopeErr := i.versionScope(r, viewer)
	if scopeErr != nil {
		return "", err
	}
	target, scopeErr := i.host.VersionHandles.Resolve(i.identityContext(r), scope, start[0])
	if scopeErr != nil || target.Item == 0 {
		return "", err
	}
	q.StartAt = target.Item
	return start[0], nil
}
