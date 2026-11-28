package backup

import (
	"context"
	"net/http"
	"testing"
)

// The backup list is owner-only like create and download: before setup a
// tokenless LAN client must not learn snapshot names, nodes or paths.
func TestBackupListRequiresSetUpOwner(t *testing.T) {
	s := standalone(t)
	if _, err := s.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, _ := backupAPI(t, s, false)
	if w := (call{method: "GET", path: "/"}).do(h); w.Code != http.StatusForbidden {
		t.Fatalf("tokenless pre-setup list: %d, want 403", w.Code)
	}
	h, _ = backupAPI(t, s, true)
	if w := (call{method: "GET", path: "/", owner: true}).do(h); w.Code != http.StatusOK {
		t.Fatalf("owner list: %d %s", w.Code, w.Body)
	}
}
