//go:build scorecard && !plexparity

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// Exercises the actual App-installed broker and durable catalog/auth fixtures.
// Estimate reaches the production broker's viewer and title policy, without
// requiring a resolver or an encoder. It does not claim HTTP/device coverage.
func TestSpikeBrokerRestrictedSectionTransition(t *testing.T) {
	stub := httptest.NewServer(http.NotFoundHandler())
	defer stub.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", stub.URL)
	t.Setenv("ETHER_PARITY_PLEXTV_URL", stub.URL)
	for _, startSpike := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal-start", true: "spike-start"}[startSpike], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			dir := t.TempDir()
			var localFiles []string
			if startSpike {
				// Supply a real, disposable media fixture for Spike startup. The
				// episode exercised below remains the durable catalog fixture.
				data, err := os.ReadFile(filepath.Join("..", "transcode", "testdata", "nativeprobe", "h264_opus.mp4"))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "Spike fixture.mp4")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				localFiles = []string{path}
			}
			cfg, err := config.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Update(func(c *config.Config) error {
				c.Spike.Enabled = startSpike
				c.Spike.LocalFiles = localFiles
				c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
				c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			local, err := identity.NewLocal(a.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := local.CreateOwner(ctx, "owner", "offline-test-password"); err != nil {
				t.Fatal(err)
			}
			if err := a.users.RefreshViewers(ctx); err != nil {
				t.Fatal(err)
			}
			if err := a.users.SyncFriends(ctx, []plextv.Friend{{PlexID: 4321, Username: "fixture", AllLibs: true}}); err != nil {
				t.Fatal(err)
			}
			var viewerID int64
			if err := a.db.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE plex_id=?`, 4321).Scan(&viewerID); err != nil {
				t.Fatal(err)
			}
			viewer, err := a.users.ActiveUser(ctx, viewerID)
			if err != nil {
				t.Fatal(err)
			}
			released := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			show := core.Title{Key: core.ShowKey(431), Type: core.TypeShow, IDs: core.IDs{TMDB: 431}, Title: "Fixture show", Released: released}
			episode := core.Title{Key: core.EpisodeKey(431, 1, 1), ShowKey: show.Key, Type: core.TypeEpisode, IDs: core.IDs{TMDB: 431}, Title: "Fixture episode", Released: released}
			for _, title := range []core.Title{show, episode} {
				if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
					t.Fatal(err)
				}
				if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
					t.Fatal(err)
				}
			}
			if a.frontends == nil || a.frontends.broker == nil {
				t.Fatal("app broker unavailable")
			}
			setSection := func(section int) error {
				viewer.AllowedSections = []int{section}
				return a.users.UpdateUser(ctx, *viewer)
			}
			plan := func(section int) error {
				if err := setSection(section); err != nil {
					return err
				}
				current, err := a.users.ActiveUser(ctx, viewerID)
				if err != nil {
					return err
				}
				_, err = a.frontends.broker.Plan(ctx, frontend.PlanRequest{Viewer: current, Device: core.Device{ID: "fixture-tv"}, Protocol: "jellyfin", Item: episode.Key, Intent: frontend.PlayIntent{Estimate: true, Options: core.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1}}})
				return err
			}
			grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			grant, err := grants.Issue(ctx, viewerID, nil, "jellyfin", core.Device{ID: "fixture-tv"})
			if err != nil {
				t.Fatal(err)
			}
			instance, err := a.frontends.registry.Mount(ctx, "jellyfin")
			if err != nil {
				t.Fatal(err)
			}
			defer a.frontends.registry.Unmount(context.Background(), "jellyfin")
			httpHost := httptest.NewServer(instance.Handler())
			defer httpHost.Close()
			httpPlan := func(section int) int {
				if err := setSection(section); err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequestWithContext(ctx, "POST", httpHost.URL+"/Items/"+ids.RatingKeyGUID(episode.Key)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":false}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Emby-Token", grant)
				req.Header.Set("X-Emby-Device-Id", "fixture-tv")
				resp, err := httpHost.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp.StatusCode
			}
			// Establish that the same currently released episode is reachable by
			// a viewer authorized for the TV section before testing restriction.
			if err := plan(2); err != nil {
				t.Fatalf("authorized TV control: %v", err)
			}
			if status := httpPlan(2); status != http.StatusOK {
				t.Fatalf("authorized TV HTTP control: %d", status)
			}
			beforeErr, beforeHTTP := plan(1), httpPlan(1)
			if err := a.cfg.Update(func(c *config.Config) error { c.Spike.Enabled = !startSpike; return nil }); err != nil {
				t.Fatal(err)
			}
			afterErr, afterHTTP := plan(1), httpPlan(1)
			if err := plan(2); err != nil {
				t.Fatalf("authorized TV control after toggle: %v", err)
			}
			if status := httpPlan(2); status != http.StatusOK {
				t.Fatalf("authorized TV HTTP control after toggle: %d", status)
			}
			t.Logf("restricted broker before=%v after=%v; HTTP before=%d after=%d", beforeErr, afterErr, beforeHTTP, afterHTTP)
			if !errors.Is(beforeErr, core.ErrNotFound) || !errors.Is(afterErr, core.ErrNotFound) || beforeHTTP == http.StatusOK || afterHTTP == http.StatusOK {
				t.Fatalf("restricted TV reached broker/HTTP: before=%v/%d after=%v/%d", beforeErr, beforeHTTP, afterErr, afterHTTP)
			}
		})
	}
}
