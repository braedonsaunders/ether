package conform

// Route describes one registered protocol endpoint.
type Route struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
}

// RouteInventory distinguishes an unavailable front end from an available one with no routes.
type RouteInventory struct {
	Available bool    `json:"available"`
	Routes    []Route `json:"routes"`
}
