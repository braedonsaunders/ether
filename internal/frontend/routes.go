package frontend

import "github.com/braedonsaunders/ether/internal/conform"

// RouteSource exposes the exact routes registered by a mounted frontend.
// A missing implementation means inventory is unavailable, not empty.
type RouteSource interface{ Routes() []conform.Route }
