//go:build !plexparity

package app

import "testing"

func TestParityHooksDisabledInProduction(t *testing.T) {
	if parityClock() != nil || len(parityCatalogueOptions()) != 0 || len(parityCollectionOptions()) != 0 || len(parityTMDBOptions()) != 0 {
		t.Fatal("fixture dependencies enabled in a production build")
	}
}
