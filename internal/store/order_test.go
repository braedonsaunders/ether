package store

import (
	"reflect"
	"testing"
)

func TestMigrationDependenciesOrderAndValidate(t *testing.T) {
	regMu.Lock()
	oldMigrations, oldDependencies := migrations, dependencies
	migrations = nil
	dependencies = map[string]map[string]bool{}
	regMu.Unlock()
	t.Cleanup(func() { regMu.Lock(); migrations, dependencies = oldMigrations, oldDependencies; regMu.Unlock() })
	Register("alpha", 2, "SELECT 2")
	Register("alpha", 1, "SELECT 1")
	Register("middle", 1, "SELECT 1")
	Register("zeta", 1, "SELECT 1")
	RegisterAfter("alpha", "zeta")
	var order []string
	for _, m := range registered() {
		order = append(order, m.component)
		if m.component == "alpha" && (len(order) == 3 && m.version != 1 || len(order) == 4 && m.version != 2) {
			t.Fatal("component versions are not ordered")
		}
	}
	if !reflect.DeepEqual(order, []string{"middle", "zeta", "alpha", "alpha"}) {
		t.Fatalf("order %v", order)
	}
	RegisterAfter("zeta", "alpha")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("cycle was accepted")
			}
		}()
		registered()
	}()
	regMu.Lock()
	delete(dependencies, "zeta")
	regMu.Unlock()
	RegisterAfter("middle", "missing")
	if got := registered(); len(got) != 4 {
		t.Fatalf("partial registry: %d", len(got))
	}
}
