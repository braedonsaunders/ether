package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend/kit/route"

// Routes returns the routes registered on this instance's protocol router.
func (i *instance) Routes() []route.Route { return i.router.Routes() }
