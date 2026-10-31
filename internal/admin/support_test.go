package admin_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/diagnostics"
)

func TestSupportCaptureIsOwnerBound(t *testing.T) {
	store := newStore(t, func(c *config.Config) {
		seededConfig(c)
		c.SetupComplete = true
	})
	rec := diagnostics.New(diagnostics.Deps{})
	rec.Observe(diagnostics.Event{Kind: diagnostics.KindDevice, DeviceKey: "tv", DeviceName: "Living Room"})
	h := admin.New(admin.Deps{
		Config: store,
		IsOwnerToken: func(_ context.Context, tok string) bool {
			return tok == ownerToken || tok == "other-session"
		},
		Support: rec,
	})

	devices := serve(h, authReq(http.MethodGet, "/api/support/devices", ""))
	if devices.Code != http.StatusOK || !strings.Contains(devices.Body.String(), "Living Room") {
		t.Fatalf("devices %d %s", devices.Code, devices.Body.String())
	}

	created := serve(h, authReq(http.MethodPost, "/api/support/captures", `{"deviceId":"missing"}`))
	if created.Code != http.StatusConflict || !strings.Contains(created.Body.String(), "stale_device") {
		t.Fatalf("stale %d %s", created.Code, created.Body.String())
	}

	id := rec.Devices()[0].ID
	created = serve(h, authReq(http.MethodPost, "/api/support/captures", `{"deviceId":"`+id+`","note":"see /tmp/help"}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	var view struct {
		ID string `json:"id"`
	}
	decode(t, created, &view)

	other := req(http.MethodGet, "/api/support/captures/"+view.ID, "")
	other.Header.Set("X-Plex-Token", "other-session")
	otherRec := serve(h, other)
	missing := serve(h, authReq(http.MethodGet, "/api/support/captures/"+view.ID+"no", ""))
	unknown := serve(h, authReq(http.MethodGet, "/api/support/captures/0123456789abcdef0123456789abcdef", ""))
	if otherRec.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound || otherRec.Body.String() != unknown.Body.String() {
		t.Fatalf("other %d %s unknown %d %s", otherRec.Code, otherRec.Body.String(), unknown.Code, unknown.Body.String())
	}
	if missing.Code != http.StatusNotFound {
		t.Fatal(missing.Code)
	}

	noCSRF := req(http.MethodPost, "/api/support/captures/"+view.ID+"/finish", `{}`)
	noCSRF.Header.Del("X-Ether-Request")
	noCSRF.Header.Set("X-Plex-Token", ownerToken)
	if serve(h, noCSRF).Code != http.StatusForbidden {
		t.Fatal("csrf")
	}

	finished := serve(h, authReq(http.MethodPost, "/api/support/captures/"+view.ID+"/finish", `{"includeTitles":false}`))
	if finished.Code != http.StatusOK || !strings.Contains(finished.Body.String(), "Usernames are not in the ZIP") {
		t.Fatalf("finish %d %s", finished.Code, finished.Body.String())
	}
	archive := serve(h, authReq(http.MethodGet, "/api/support/captures/"+view.ID+"/archive", ""))
	body, _ := io.ReadAll(archive.Body)
	if archive.Code != http.StatusOK || len(body) < 2 || body[0] != 'P' || body[1] != 'K' {
		t.Fatalf("archive %d %d bytes", archive.Code, len(body))
	}
	if !strings.Contains(archive.Header().Get("Content-Type"), "application/zip") {
		t.Fatal(archive.Header().Get("Content-Type"))
	}
}

func authReq(method, target, body string) *http.Request {
	r := req(method, target, body)
	r.Header.Set("X-Plex-Token", ownerToken)
	return r
}
