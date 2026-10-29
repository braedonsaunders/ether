package plex

import "testing"

func TestRouteInventory(t *testing.T) {
	inv := RouteInventory()
	if !inv.Available || len(inv.Routes) == 0 {
		t.Fatal(inv)
	}
	found := false
	for _, r := range inv.Routes {
		if r.Method == "GET" && r.Pattern == "/identity" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing GET /identity")
	}
}
