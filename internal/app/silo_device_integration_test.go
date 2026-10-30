package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type siloTestBroker = frontendtest.Sessions
type siloDeviceTestBroker struct{ siloTestBroker }

func (siloDeviceTestBroker) ServeMediaAccess(w http.ResponseWriter, r *http.Request, access frontend.SessionAccess) {
	if access.Viewer == nil || access.Protocol != "silo" || access.Device.ID != "tv" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TestSiloMediaRequiresActualBoundGrantAfterRevocation(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := grants.Issue(ctx, owner.ID, nil, "silo", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	phone, err := grants.Issue(ctx, owner.ID, nil, "silo", core.Device{ID: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE identity_grants SET last_used=last_used-1000 WHERE viewer_id=? AND surface='silo' AND device_id='tv'`, owner.ID); err != nil {
		t.Fatal(err)
	}
	var usedBefore, expiryBefore int64
	if err = a.db.QueryRowContext(ctx, `SELECT last_used,expires FROM identity_grants WHERE viewer_id=? AND surface='silo' AND device_id='tv'`, owner.ID).Scan(&usedBefore, &expiryBefore); err != nil {
		t.Fatal(err)
	}
	inst, err := (silo.Frontend{MachineID: "test"}).Mount(ctx, frontend.Host{Identity: surfaceIdentity{surface: "silo", grants: grants}, Sessions: siloDeviceTestBroker{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(token, device string) int {
		r := httptest.NewRequest("GET", "/api/v2/stream/session/grant/file", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Silo-Device-Id", device)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if got := call(tv, "tv"); got != http.StatusOK {
		t.Fatalf("valid grant: %d", got)
	}
	if got := call(phone, "tv"); got != http.StatusUnauthorized {
		t.Fatalf("same viewer, forged device: %d", got)
	}
	var usedAfter, expiryAfter int64
	if err = a.db.QueryRowContext(ctx, `SELECT last_used,expires FROM identity_grants WHERE viewer_id=? AND surface='silo' AND device_id='tv'`, owner.ID).Scan(&usedAfter, &expiryAfter); err != nil {
		t.Fatal(err)
	}
	if usedAfter != usedBefore || expiryAfter != expiryBefore {
		t.Fatalf("media request changed grant activity: %d/%d -> %d/%d", usedBefore, expiryBefore, usedAfter, expiryAfter)
	}
	if err := grants.RevokeDevice(ctx, owner.ID, "tv"); err != nil {
		t.Fatal(err)
	}
	if got := call(tv, "tv"); got != http.StatusUnauthorized {
		t.Fatalf("revoked after open: %d", got)
	}
}
