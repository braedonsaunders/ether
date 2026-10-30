//go:build plexparity && conformlab

package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSiloLabCleanupFailureAndSessionAccounting(t *testing.T) {
	active, prepared := siloLabPartitionSessions([]*core.PlaySession{{ID: "active"}, {ID: "prepared", PreparedOnly: true}})
	if len(active) != 1 || active[0].ID != "active" || len(prepared) != 1 || prepared[0].ID != "prepared" {
		t.Fatalf("wrong session accounting: active=%+v prepared=%+v", active, prepared)
	}
	failure := errors.New("stop failed")
	called := ""
	err := siloLabStopActive(active, func(id string) error { called = id; return failure })
	if called != "active" || !errors.Is(err, failure) {
		t.Fatalf("stop failure lost: called=%q error=%v", called, err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		if siloLabCleanupStatus("logout", status) == nil {
			t.Errorf("status %d incorrectly accepted", status)
		}
	}
	if err := siloLabCleanupStatus("logout", http.StatusNoContent); err != nil {
		t.Fatal(err)
	}
}
