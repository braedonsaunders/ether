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
	}
	if !reflect.DeepEqual(order, []string{"zeta", "alpha", "alpha", "middle"}) {
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
	defer func() {
		if recover() == nil {
			t.Fatal("missing dependency was accepted")
		}
	}()
	registered()
}
