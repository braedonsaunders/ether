package main

import (
	"slices"
	"testing"
)

func TestInventoryReportsUncoveredRoutes(t *testing.T) {
	missing := missingRoutes()
	if !slices.Contains(missing, "/downloadQueue/{queue}") {
		t.Fatalf("uncovered download queue absent from inventory: %v", missing)
	}
	if slices.Contains(missing, "/identity") {
		t.Fatal("identity is already covered")
	}
}
