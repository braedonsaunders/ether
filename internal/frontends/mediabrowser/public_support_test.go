package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// PublicRoutes registers connection endpoints that do not require a grant.
// A user picker is intentionally not exposed until the host can supply its
// authorized list of visible viewers.
func PublicRoutes(r *route.Router, info PublicInfo) error {
	return publicRoutes(r, func() PublicInfo { return info }, nil, nil, nil)
}
