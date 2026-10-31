package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// siloPlayMedia is the real broker's playback double: one local file, with the
// audio the watch document has to advertise.
type siloPlayMedia struct{ *offerRevisionPlayback }

func (p *siloPlayMedia) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	ps, err := p.offerRevisionPlayback.PrepareSurface(ctx, u, key, caps, release, exact)
	if err != nil || ps == nil {
		return ps, err
	}
	ps.Info.Streams = []core.StreamInfo{{Type: "audio", Codec: "aac", Language: "eng", Channels: 2, Default: true}}
	return ps, nil
}

// TestSiloWatchPlaybackResume replays the Apple TV play sequence against the
// real session broker. A cold title has no catalogue edition, so watch must
// open one source before playback/start can run.
func TestSiloWatchPlaybackResume(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	path := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(path, []byte("original source"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	playback := &siloPlayMedia{&offerRevisionPlayback{&localRevisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}, source: localRevisionFile{file}}}}
	progress := state.New(db, state.Options{})
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	key := core.MovieKey(166)
	related := core.MovieKey(167)
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, item core.RatingKey) (*core.Title, error) {
		if item != key {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: item, Type: core.TypeMovie, Title: "Abzurdah"}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		DB: db, Events: events, Playback: playback, Lookup: lookup, MediaLookup: lookup,
		ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: progress,
		OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	offers, err := NewSourceOffers(db, broker, "silo")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 1}
	exp := frontendtest.Experience{
		ItemFunc: func(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			if req.Key != key {
				return frontend.ItemDetail{}, frontend.ErrNotFound
			}
			status, err := progress.Get(ctx, req.Viewer.ID, req.Key)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: core.TypeMovie, Title: "Abzurdah", Year: 2015}, UserData: status}, nil
		},
		SimilarFunc: func(context.Context, *core.Viewer, core.Device, core.RatingKey, frontend.ItemQuery) (frontend.SearchPage, error) {
			return frontend.SearchPage{Page: core.Page{Items: []core.Title{{Key: related, Type: core.TypeMovie, Title: "Sintel"}}, Total: 1}}, nil
		},
	}
	inst, err := (silo.Frontend{MachineID: "silo-play"}).Mount(ctx, frontend.Host{
		Identity:     offerAdmissionIdentity{frontendtest.Identity{ResolveFunc: func(context.Context, string) (*core.Viewer, error) { return viewer, nil }}},
		Experience:   exp,
		Sessions:     broker,
		Capabilities: frontendtest.Capabilities{},
		SourceOffers: offers,
		Config:       func() frontend.Settings { return frontend.Settings{"serverName": "Mac Prism Lab"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		r.Header.Set("X-Silo-Client", "Silo Apple TV")
		r.Header.Set("X-Silo-Client-Version", "1.0")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	guid := ids.RatingKeyGUID(key)
	health := call(http.MethodGet, "/api/v1/health", "")
	if health.Code != 200 || !strings.Contains(health.Body.String(), `"status":"ok"`) || !strings.Contains(health.Body.String(), `"server_name":"Mac Prism Lab"`) {
		t.Fatalf("health %d %s", health.Code, health.Body.String())
	}
	item := call(http.MethodGet, "/api/v2/catalog/items/"+guid, "")
	if item.Code != 200 || !strings.Contains(item.Body.String(), `"versions":[]`) {
		t.Fatalf("catalog item probed: %d %s", item.Code, item.Body.String())
	}
	watch := call(http.MethodGet, "/api/v2/watch/"+guid, "")
	var watched struct {
		Versions []struct {
			FileID      string  `json:"file_id"`
			AudioTracks []any   `json:"audio_tracks"`
			Duration    float64 `json:"duration_seconds"`
		} `json:"versions"`
	}
	if watch.Code != 200 || json.Unmarshal(watch.Body.Bytes(), &watched) != nil || len(watched.Versions) != 1 || watched.Versions[0].FileID == "" || len(watched.Versions[0].AudioTracks) != 1 || watched.Versions[0].Duration <= 0 {
		t.Fatalf("watch %d %s", watch.Code, watch.Body.String())
	}
	similar := call(http.MethodGet, "/api/v2/recommendations/similar/"+guid+"?backdrop_width=2688&limit=12&poster_width=440", "")
	if similar.Code != 200 || !strings.Contains(similar.Body.String(), `"title":"Sintel"`) || strings.Contains(similar.Body.String(), `"page"`) {
		t.Fatalf("similar %d %s", similar.Code, similar.Body.String())
	}
	versions := call(http.MethodGet, "/api/v2/catalog/items/"+guid+"/versions", "")
	if versions.Code != 200 || !strings.Contains(versions.Body.String(), `"file_id"`) || !strings.Contains(versions.Body.String(), `"audio_tracks"`) {
		t.Fatalf("versions %d %s", versions.Code, versions.Body.String())
	}
	caps := call(http.MethodGet, "/api/v2/playback/capabilities", "")
	var discovery struct {
		Installation string `json:"installation_id"`
	}
	if caps.Code != 200 || json.Unmarshal(caps.Body.Bytes(), &discovery) != nil || discovery.Installation == "" {
		t.Fatalf("caps %d %s", caps.Code, caps.Body.String())
	}
	play := func(position float64, attempt string) map[string]any {
		raw, _ := json.Marshal(map[string]any{
			"protocol_version": 3, "file_id": watched.Versions[0].FileID,
			"installation_id": discovery.Installation, "playback_attempt_id": attempt,
			"start_position": position,
		})
		w := call(http.MethodPost, "/api/v2/playback/start", string(raw))
		var body map[string]any
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("start %d %s", w.Code, w.Body.String())
		}
		return body
	}
	started := play(0, "attempt-1")
	sessionID, _ := started["session_id"].(string)
	plan, _ := started["playback_plan"].(map[string]any)
	stream, _ := plan["stream"].(map[string]any)
	streamURL, _ := stream["url"].(string)
	if sessionID == "" || !strings.HasPrefix(streamURL, "/api/v2/stream/") {
		t.Fatalf("start document %+v", started)
	}
	media := call(http.MethodGet, streamURL, "")
	if media.Code != 200 || !strings.Contains(media.Body.String(), "source bytes") {
		t.Fatalf("stream %d %s", media.Code, media.Body.String())
	}
	sample, _ := json.Marshal(map[string]any{
		"installation_id": discovery.Installation, "position": 90, "sequence": 1, "is_paused": false, "stop_id": "stop-1",
	})
	stopped := call(http.MethodDelete, "/api/v2/playback/"+sessionID, string(sample))
	if stopped.Code != 200 || !strings.Contains(stopped.Body.String(), `"stopped"`) {
		t.Fatalf("stop %d %s", stopped.Code, stopped.Body.String())
	}
	again := call(http.MethodGet, "/api/v2/watch/"+guid, "")
	var resumed struct {
		UserData struct {
			Position float64 `json:"position_seconds"`
		} `json:"user_data"`
		Versions []struct {
			FileID string `json:"file_id"`
		} `json:"versions"`
	}
	if again.Code != 200 || json.Unmarshal(again.Body.Bytes(), &resumed) != nil || resumed.UserData.Position != 90 || len(resumed.Versions) != 1 || resumed.Versions[0].FileID == "" {
		t.Fatalf("resume watch %d %s", again.Code, again.Body.String())
	}
	watched.Versions[0].FileID = resumed.Versions[0].FileID
	resumedPlay := play(90, "attempt-2")
	resumedPlan, _ := resumedPlay["playback_plan"].(map[string]any)
	timeline, _ := resumedPlan["timeline"].(map[string]any)
	if timeline["source_start_seconds"] != 90.0 {
		got, _ := json.Marshal(timeline)
		t.Fatalf("resume timeline %s", got)
	}
}
