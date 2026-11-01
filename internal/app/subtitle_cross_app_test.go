package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/identity"
)

func TestSiloSubtitlePreferenceShowsInJellyfin(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "ada", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: id, Username: "ada", CreatedAt: time.Unix(10_000, 0), UpdatedAt: time.Unix(11_000, 0)}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		copy := *viewer
		return &copy, nil
	}}
	host := frontend.Host{
		Identity:         ident,
		Preferences:      identity.NewPreferences(a.db, "silo"),
		TrackPreferences: surfaceTrackPreferences{app: a},
		Experience: frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies"}}, nil
		}},
		Sessions: frontendtest.Sessions{},
	}
	siloApp, err := (silo.Frontend{MachineID: "test"}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	jellyfin, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	appearance := `{"fontSize":"small","fontFamily":"sans-serif","fontColor":"#fff","backgroundColor":"#000","backgroundStyle":"box","backgroundOpacity":50,"textOutline":false,"textOutlineColor":"#000","position":"top"}`
	body := `{"subtitle_language":"fr","subtitle_mode":"auto"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/profiles/"+strconv.FormatInt(id, 10), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer grant")
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	siloApp.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"subtitle_mode":"auto"`) || !strings.Contains(rec.Body.String(), `"subtitle_language":"fr"`) {
		t.Fatalf("silo %d %s", rec.Code, rec.Body.String())
	}
	settings := httptest.NewRequest(http.MethodPut, "/api/v2/settings/values/playback.subtitle_appearance?scope=profile", strings.NewReader(`{"value":`+appearance+`}`))
	settings.Header.Set("Authorization", "Bearer grant")
	settings.SetPathValue("key", "playback.subtitle_appearance")
	written := httptest.NewRecorder()
	siloApp.Handler().ServeHTTP(written, settings)
	if written.Code != 200 {
		t.Fatalf("appearance %d %s", written.Code, written.Body.String())
	}
	read := httptest.NewRecorder()
	effective := httptest.NewRequest(http.MethodGet, "/api/v2/settings/values/effective?keys=playback.subtitle_appearance&keys=playback.subtitle_mode", nil)
	effective.Header.Set("Authorization", "Bearer grant")
	siloApp.Handler().ServeHTTP(read, effective)
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"fontSize":"small"`) || !strings.Contains(read.Body.String(), `"auto"`) {
		t.Fatalf("effective %d %s", read.Code, read.Body.String())
	}
	me := httptest.NewRecorder()
	jellyReq := httptest.NewRequest(http.MethodGet, "/Users/Me", nil)
	jellyReq.Header.Set("X-Emby-Token", "grant")
	jellyfin.Handler().ServeHTTP(me, jellyReq)
	var user struct {
		Configuration map[string]any
	}
	if me.Code != 200 || json.Unmarshal(me.Body.Bytes(), &user) != nil || user.Configuration["SubtitleMode"] != "Smart" || user.Configuration["SubtitleLanguagePreference"] != "fr" {
		t.Fatalf("jellyfin %d %s", me.Code, me.Body.String())
	}
	account, err := a.users.ActiveUser(ctx, id)
	if err != nil || len(account.SubtitleLanguages) != 1 || account.SubtitleLanguages[0] != "fr" {
		t.Fatalf("account languages %+v %v", account, err)
	}
	rules, err := a.trackprefs.GetRules(ctx, id)
	if err != nil || rules.SubtitleMode != "foreign-only" || !strings.Contains(string(rules.SubtitleAppearance), `"position":"top"`) {
		t.Fatalf("stored %+v %s %v", rules.SubtitleLanguages, rules.SubtitleAppearance, err)
	}
}
