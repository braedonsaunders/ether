package vela

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type versionsEditionsFake struct {
	mu           sync.Mutex
	slots        []frontend.ReleaseVariant
	choice       *frontend.ViewerChoiceSummary
	searches     int
	clearCalls   int
	lastCleared  string
	slotRequests int
}

func (f *versionsEditionsFake) Editions(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) ([]frontend.Version, error) {
	return nil, nil
}
func (f *versionsEditionsFake) RefillSearchCache(*frontend.Title) <-chan struct{} {
	f.mu.Lock()
	f.searches++
	f.mu.Unlock()
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (f *versionsEditionsFake) Available(context.Context, frontend.RatingKey) (bool, bool) {
	return true, true
}

func (f *versionsEditionsFake) Variants(ctx context.Context, req frontend.ResolveRequest, max int) ([]frontend.ReleaseVariant, error) {
	return f.VersionSlots(ctx, req, max, true)
}

func (f *versionsEditionsFake) VersionSlots(_ context.Context, _ frontend.ResolveRequest, _ int, _ bool) ([]frontend.ReleaseVariant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slotRequests++
	out := append([]frontend.ReleaseVariant(nil), f.slots...)
	if f.choice == nil {
		for i := range out {
			out[i].Remembered = false
		}
	}
	return out, nil
}

func (f *versionsEditionsFake) ViewerChoiceFor(_ context.Context, _ *frontend.Viewer, _ *frontend.Title) (*frontend.ViewerChoiceSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.choice == nil {
		return nil, nil
	}
	cp := *f.choice
	return &cp, nil
}

func (f *versionsEditionsFake) ClearViewerChoice(_ context.Context, _ *frontend.Viewer, _ *frontend.Title, scope string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls++
	f.lastCleared = scope
	if f.choice == nil {
		return false, nil
	}
	f.choice = nil
	return true, nil
}

type versionsHandlesFake struct {
	item   frontend.RatingKey
	toUUID map[string]string
}

func (h *versionsHandlesFake) Issue(_ context.Context, _ frontend.VersionScope, key frontend.RatingKey, sel string) (string, error) {
	if key != h.item {
		return "", frontend.ErrNotFound
	}
	if u, ok := h.toUUID[sel]; ok {
		return u, nil
	}
	return "", frontend.ErrNotFound
}

func (h *versionsHandlesFake) IssueBatch(ctx context.Context, scope frontend.VersionScope, key frontend.RatingKey, sels []string) (map[string]string, error) {
	out := make(map[string]string, len(sels))
	for _, s := range sels {
		u, err := h.Issue(ctx, scope, key, s)
		if err != nil {
			return nil, err
		}
		out[s] = u
	}
	return out, nil
}

func (h *versionsHandlesFake) Resolve(_ context.Context, _ frontend.VersionScope, handle string) (frontend.VersionTarget, error) {
	for sel, u := range h.toUUID {
		if u == handle {
			return frontend.VersionTarget{Item: h.item, Release: sel}, nil
		}
	}
	return frontend.VersionTarget{}, frontend.ErrNotFound
}

func (h *versionsHandlesFake) MatchSession(context.Context, frontend.VersionScope, string, string) error {
	return nil
}

type versionsSessionsFake struct {
	sessionFake
	mu             sync.Mutex
	planFunc       func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error)
	switchFunc     func(context.Context, frontend.SessionAccess, string, string, time.Duration) (frontend.PlayPlan, error)
	lastPlanReq    frontend.PlanRequest
	lastSwitchPos  time.Duration
	lastSwitchVer  string
	startStreamCnt int
}

func (s *versionsSessionsFake) Plan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	s.mu.Lock()
	s.lastPlanReq = req
	fn := s.planFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return frontend.PlayPlan{
		ID:            "plan-1",
		Media:         frontend.MediaInfo{Container: "mkv", Duration: 2 * time.Hour, Streams: []frontend.StreamInfo{{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160}, {Index: 1, Type: "audio", Codec: "truehd", Channels: 8, Language: "eng", Default: true}}},
		Decision:      frontend.Decision{Kind: frontend.DecisionDirectPlay, AudioStream: 1, SubtitleStream: -1, Reason: "direct_play"},
		DirectPlay:    true,
		DirectStream:  true,
		HLS:           true,
		ETag:          "etag-1",
		VersionHandle: req.Version,
		ReleaseID:     "rel-dv-remux",
	}, nil
}

func (s *versionsSessionsFake) Open(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	return frontend.Session{ID: "sess-1", Item: frontend.MovieKey(603)}, nil
}

func (s *versionsSessionsFake) StartStream(ctx context.Context, a frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
	s.mu.Lock()
	s.startStreamCnt++
	fn := s.StartStreamFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, a, id, req)
	}
	if req.Format == frontend.StreamHLS {
		return "/sessions/media/" + id + "/master.m3u8", nil
	}
	return "/sessions/media/" + id + "/file", nil
}

func (s *versionsSessionsFake) SwitchVersion(ctx context.Context, a frontend.SessionAccess, id, versionHandle string, pos time.Duration) (frontend.PlayPlan, error) {
	s.mu.Lock()
	s.lastSwitchVer = versionHandle
	s.lastSwitchPos = pos
	fn := s.switchFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, a, id, versionHandle, pos)
	}
	return frontend.PlayPlan{
		ID:            id,
		Media:         frontend.MediaInfo{Container: "mp4", Duration: 2 * time.Hour, Streams: []frontend.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng", Default: true}}},
		Decision:      frontend.Decision{Kind: frontend.DecisionDirectPlay, AudioStream: 1, SubtitleStream: -1, Reason: "direct_play"},
		DirectPlay:    true,
		DirectStream:  true,
		HLS:           true,
		ETag:          "etag-2",
		VersionHandle: versionHandle,
		ReleaseID:     "rel-1080p-webdl",
	}, nil
}

func openVelaTestReplayDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE vela_replays(key TEXT PRIMARY KEY,digest TEXT NOT NULL,response BLOB,expires INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mountVersionsVela(t *testing.T, movieKey frontend.RatingKey, editions *versionsEditionsFake, handles *versionsHandlesFake, sess *versionsSessionsFake) http.Handler {
	t.Helper()
	db := openVelaTestReplayDB(t)
	titles := map[frontend.RatingKey]frontend.Title{
		movieKey:                  {Key: movieKey, Type: frontend.TypeMovie, Title: "The Matrix", Year: 1999},
		frontend.ShowKey(10):      {Key: frontend.ShowKey(10), Type: frontend.TypeShow, Title: "Severance"},
		frontend.SeasonKey(10, 1): {Key: frontend.SeasonKey(10, 1), Type: frontend.TypeSeason, Title: "Season 1"},
		frontend.EpisodeKey(10, 1, 2): {
			Key: frontend.EpisodeKey(10, 1, 2), ShowKey: frontend.ShowKey(10),
			Type: frontend.TypeEpisode, Title: "Half Loop", Season: 1, Episode: 2,
		},
	}
	exp := frontendtest.Experience{
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			if title, ok := titles[req.Key]; ok {
				return frontend.ItemDetail{Title: title}, nil
			}
			return frontend.ItemDetail{}, frontend.ErrNotFound
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	}
	host := frontend.Host{
		Identity:       identityFake{},
		Experience:     exp,
		Sessions:       sess,
		Editions:       editions,
		VersionHandles: handles,
		Capabilities:   frontendtest.Capabilities{},
	}
	inst, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.Handler()
}

func velaReq(method, path, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set("X-Vela-Device-Id", "dev-1")
	return r
}

func TestVelaVersionsEndpointAndChoiceClearing(t *testing.T) {
	movieKey := frontend.MovieKey(603)
	movieGUID := ids.RatingKeyGUID(movieKey)
	showGUID := ids.RatingKeyGUID(frontend.ShowKey(10))
	seasonGUID := ids.RatingKeyGUID(frontend.SeasonKey(10, 1))

	hAuto := "11111111-1111-4111-8111-111111111111"
	hAlt := "22222222-2222-4222-8222-222222222222"
	editions := &versionsEditionsFake{
		choice: &frontend.ViewerChoiceSummary{Scope: "title", Label: "4K DV Remux"},
		slots: []frontend.ReleaseVariant{
			{
				ID: "slot:auto", Auto: true, Remembered: true, Availability: frontend.VersionInstant,
				Edition: "Director's Cut",
				Release: frontend.Release{
					ID: "rel-dv-remux", Size: 58 << 30,
					Name: "/mnt/media/The.Matrix.1999.2160p.REMUX.DV.TrueHD.7.1.Atmos-SECRETGRP.mkv",
					Info: frontend.ReleaseInfo{
						Height: 2160, Source: frontend.Source("remux"), VideoCodec: "hevc",
						DolbyVision: true, AudioCodec: "truehd", Atmos: true, Channels: "7.1", Group: "SECRETGRP",
					},
				},
				Info: frontend.MediaInfo{
					Estimated: false,
					Size:      58 << 30,
					Streams: []frontend.StreamInfo{
						{Type: "video", Codec: "hevc", Width: 3840, Height: 2160, DolbyVision: true, DolbyVisionProfile: 8},
						{Type: "audio", Codec: "truehd", Channels: 8, ChannelLayout: "7.1", Language: "eng", Default: true, Profile: "Dolby TrueHD + Dolby Atmos"},
						{Type: "audio", Codec: "ac3", Channels: 6, Language: "fre"},
					},
				},
			},
			{
				ID: "slot:alt1", Pinned: true, Availability: frontend.VersionCached,
				Release: frontend.Release{
					ID: "rel-1080p-webdl", Size: 7 << 30,
					Name: "The.Matrix.1999.1080p.WEB-DL.DDP5.1.Atmos.H.264-OTHERGRP",
					Info: frontend.ReleaseInfo{
						Height: 1080, Source: frontend.Source("webdl"), VideoCodec: "h264",
						AudioCodec: "eac3", Atmos: true, Channels: "5.1", Group: "OTHERGRP",
					},
				},
				Info: frontend.MediaInfo{Estimated: true},
			},
		},
	}
	handles := &versionsHandlesFake{item: movieKey, toUUID: map[string]string{"slot:auto": hAuto, "slot:alt1": hAlt}}
	handler := mountVersionsVela(t, movieKey, editions, handles, &versionsSessionsFake{})

	// 1. Capabilities advertises features.versions.available = true.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("GET", "/vela/v0/capabilities", ""))
	if w.Code != 200 {
		t.Fatalf("capabilities %d: %s", w.Code, w.Body.String())
	}
	var capResp struct {
		Features map[string]struct{ Available bool } `json:"features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &capResp); err != nil {
		t.Fatal(err)
	}
	if !capResp.Features["versions"].Available {
		t.Fatalf("features.versions.available = false, want true")
	}

	// 2. GET /vela/v0/titles/{id}/versions returns clean human labels, probed audio,
	// remembered + pinned badges, and ZERO raw release names/groups/paths!
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("GET", "/vela/v0/titles/"+movieGUID+"/versions", ""))
	if w.Code != 200 {
		t.Fatalf("GET versions %d: %s", w.Code, w.Body.String())
	}
	rawBody := w.Body.String()
	for _, forbidden := range []string{"SECRETGRP", "OTHERGRP", "/mnt/media", ".mkv", "slot:auto", "rel-dv-remux"} {
		if strings.Contains(rawBody, forbidden) {
			t.Fatalf("versions response leaked raw token %q in %s", forbidden, rawBody)
		}
	}
	var listResp struct {
		TitleID           string  `json:"titleId"`
		SelectedVersionID *string `json:"selectedVersionId"`
		RememberedChoice  *struct {
			Scope  string `json:"scope"`
			Active bool   `json:"active"`
			Label  string `json:"label"`
		} `json:"rememberedChoice"`
		Versions []struct {
			ID           string   `json:"id"`
			Label        string   `json:"label"`
			Recommended  bool     `json:"recommended"`
			Remembered   bool     `json:"remembered"`
			Pinned       bool     `json:"pinned"`
			Resolution   string   `json:"resolution"`
			DynamicRange string   `json:"dynamicRange"`
			VideoCodec   string   `json:"videoCodec"`
			Source       string   `json:"source"`
			Edition      string   `json:"edition"`
			SizeBytes    int64    `json:"sizeBytes"`
			Languages    []string `json:"languages"`
			Availability string   `json:"availability"`
			Audio        *struct {
				Codec    string `json:"codec"`
				Channels string `json:"channels"`
				Atmos    bool   `json:"atmos"`
			} `json:"audio"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.TitleID != movieGUID || listResp.SelectedVersionID == nil || *listResp.SelectedVersionID != hAuto {
		t.Fatalf("unexpected header fields: %+v", listResp)
	}
	if listResp.RememberedChoice == nil || listResp.RememberedChoice.Scope != "title" || !listResp.RememberedChoice.Active {
		t.Fatalf("rememberedChoice = %+v, want active title choice", listResp.RememberedChoice)
	}
	if len(listResp.Versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(listResp.Versions))
	}
	v0 := listResp.Versions[0]
	if v0.ID != hAuto || v0.Recommended || !v0.Remembered || v0.Resolution != "4K" || v0.DynamicRange != "dv" || v0.VideoCodec != "hevc" || v0.Source != "Remux" || v0.Edition != "Director's Cut" || v0.Availability != "instant" {
		t.Fatalf("v0 = %+v", v0)
	}
	if v0.Audio == nil || v0.Audio.Codec != "truehd" || v0.Audio.Channels != "7.1" || !v0.Audio.Atmos {
		t.Fatalf("v0.Audio = %+v", v0.Audio)
	}
	if len(v0.Languages) != 2 || v0.Languages[0] != "en" || v0.Languages[1] != "fr" {
		t.Fatalf("v0.Languages = %v, want [en fr]", v0.Languages)
	}
	if !strings.Contains(v0.Label, "4K Dolby Vision Remux") || !strings.Contains(v0.Label, "TrueHD Atmos 7.1") {
		t.Fatalf("v0.Label = %q", v0.Label)
	}
	v1 := listResp.Versions[1]
	if v1.ID != hAlt || !v1.Pinned || v1.Remembered || v1.Availability != "cached" {
		t.Fatalf("v1 = %+v", v1)
	}
	if editions.searches != 0 {
		t.Fatalf("GET /versions triggered %d RefillSearchCache calls, want 0", editions.searches)
	}

	// 3. Non-playable show/season rejected with 400 request/not_playable on GET /versions.
	for _, guid := range []string{showGUID, seasonGUID} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, velaReq("GET", "/vela/v0/titles/"+guid+"/versions", ""))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"not_playable"`) {
			t.Fatalf("GET versions on %s: %d %s", guid, w.Code, w.Body.String())
		}
	}

	// 4. DELETE /vela/v0/titles/{id}/versions/choice clears choice and returns refreshed versions!
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("DELETE", "/vela/v0/titles/"+movieGUID+"/versions/choice", `{"idempotencyKey":"clear-1","scope":"all"}`))
	if w.Code != 200 {
		t.Fatalf("DELETE choice %d: %s", w.Code, w.Body.String())
	}
	var clearResp struct {
		Cleared  bool `json:"cleared"`
		Versions []struct {
			Recommended bool `json:"recommended"`
			Remembered  bool `json:"remembered"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &clearResp); err != nil {
		t.Fatal(err)
	}
	if !clearResp.Cleared || len(clearResp.Versions) != 2 || !clearResp.Versions[0].Recommended || clearResp.Versions[0].Remembered {
		t.Fatalf("clearResp = %+v", clearResp)
	}
	// Idempotent replay does NOT invoke ClearViewerChoice a second time.
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, velaReq("DELETE", "/vela/v0/titles/"+movieGUID+"/versions/choice", `{"idempotencyKey":"clear-1","scope":"all"}`))
	if w2.Code != 200 || editions.clearCalls != 1 {
		t.Fatalf("idempotent replay: code=%d clearCalls=%d", w2.Code, editions.clearCalls)
	}
	// Show is allowed on DELETE choice; season is rejected with 400 not_playable.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("DELETE", "/vela/v0/titles/"+showGUID+"/versions/choice", `{"idempotencyKey":"clear-show","scope":"show"}`))
	if w.Code != 200 {
		t.Fatalf("DELETE show choice %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("DELETE", "/vela/v0/titles/"+seasonGUID+"/versions/choice", `{"idempotencyKey":"clear-season"}`))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"not_playable"`) {
		t.Fatalf("DELETE season choice %d: %s", w.Code, w.Body.String())
	}
}

func TestVelaPlaybackWithVersionAndInSessionVersionSwitch(t *testing.T) {
	movieKey := frontend.MovieKey(603)
	movieGUID := ids.RatingKeyGUID(movieKey)
	hAuto := "11111111-1111-4111-8111-111111111111"
	hAlt := "22222222-2222-4222-8222-222222222222"
	hSlow := "33333333-3333-4333-8333-333333333333"

	editions := &versionsEditionsFake{
		slots: []frontend.ReleaseVariant{
			{ID: "slot:auto", Auto: true, Availability: frontend.VersionInstant, Release: frontend.Release{ID: "rel-dv-remux", Info: frontend.ReleaseInfo{Height: 2160, Source: frontend.Source("remux"), DolbyVision: true}}},
			{ID: "slot:alt1", Availability: frontend.VersionCached, Release: frontend.Release{ID: "rel-1080p-webdl", Info: frontend.ReleaseInfo{Height: 1080, Source: frontend.Source("webdl")}}},
			{ID: "slot:slow", Availability: frontend.VersionFetch, Release: frontend.Release{ID: "rel-slow", Info: frontend.ReleaseInfo{Height: 2160, Source: frontend.Source("bluray")}}},
		},
	}
	handles := &versionsHandlesFake{item: movieKey, toUUID: map[string]string{"slot:auto": hAuto, "slot:alt1": hAlt, "slot:slow": hSlow}}
	sess := &versionsSessionsFake{}
	handler := mountVersionsVela(t, movieKey, editions, handles, sess)

	// 1. POST /playback with versionId + channelId rejected with 400 invalid_request.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("POST", "/vela/v0/playback", `{"idempotencyKey":"bad-target","channelId":"ch-1","versionId":"`+hAlt+`","client":{"device":{"id":"dev-1"}}}`))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_request"`) {
		t.Fatalf("channel+versionId = %d %s", w.Code, w.Body.String())
	}

	// 2. POST /playback with valid versionId returns PlaybackPlan containing versions + selectedVersionId.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("POST", "/vela/v0/playback", `{"idempotencyKey":"play-1","titleId":"`+movieGUID+`","versionId":"`+hAuto+`","positionSeconds":120,"client":{"device":{"id":"dev-1"},"hls":true}}`))
	if w.Code != 200 {
		t.Fatalf("POST playback %d: %s", w.Code, w.Body.String())
	}
	var playResp struct {
		SessionID         string   `json:"sessionId"`
		Revision          uint64   `json:"revision"`
		SourceGeneration  uint64   `json:"sourceGeneration"`
		SeekGeneration    uint64   `json:"seekGeneration"`
		SelectedVersionID string   `json:"selectedVersionId"`
		Versions          []object `json:"versions"`
		Paths             []object `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &playResp); err != nil {
		t.Fatal(err)
	}
	if playResp.SelectedVersionID != hAuto || len(playResp.Versions) != 3 || len(playResp.Paths) == 0 {
		t.Fatalf("playResp = %+v", playResp)
	}

	// 3. In-session version switch to slow/fetching version returns 503 source/version_preparing
	// with Retry-After, leaves active session untouched, and does NOT persist 503 replay!
	preparing := true
	sess.switchFunc = func(_ context.Context, _ frontend.SessionAccess, id, ver string, pos time.Duration) (frontend.PlayPlan, error) {
		if ver == hSlow && preparing {
			return frontend.PlayPlan{}, &frontend.PreparingError{RetryAfter: 5 * time.Second}
		}
		return frontend.PlayPlan{
			ID:            id,
			Media:         frontend.MediaInfo{Container: "mp4", Duration: 2 * time.Hour},
			Decision:      frontend.Decision{Kind: frontend.DecisionDirectPlay, AudioStream: -1, SubtitleStream: -1, Reason: "direct_play"},
			DirectPlay:    true,
			ETag:          "etag-switched",
			VersionHandle: ver,
			ReleaseID:     "rel-slow",
		}, nil
	}
	cmdBody := `{"idempotencyKey":"switch-slow","kind":"version","versionId":"` + hSlow + `","sourceGeneration":1,"seekGeneration":1}`
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("POST", "/vela/v0/sessions/"+playResp.SessionID+"/commands", cmdBody))
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), `"version_preparing"`) {
		t.Fatalf("slow switch = %d Retry-After=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	// Verify active session snapshot is completely undisturbed (same generations and selectedVersionId!).
	wSnap := httptest.NewRecorder()
	handler.ServeHTTP(wSnap, velaReq("GET", "/vela/v0/sessions/"+playResp.SessionID, ""))
	if wSnap.Code != 200 || !strings.Contains(wSnap.Body.String(), `"selectedVersionId":"`+hAuto+`"`) || !strings.Contains(wSnap.Body.String(), `"sourceGeneration":1`) {
		t.Fatalf("snapshot disturbed after 503: %d %s", wSnap.Code, wSnap.Body.String())
	}

	// 4. Retrying with the SAME idempotencyKey once ready succeeds (because 503 was NOT cached in vela_replays)!
	preparing = false
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("POST", "/vela/v0/sessions/"+playResp.SessionID+"/commands", cmdBody))
	if w.Code != 200 {
		t.Fatalf("retry switch %d: %s", w.Code, w.Body.String())
	}
	var switchResp struct {
		SessionID         string  `json:"sessionId"`
		Revision          uint64  `json:"revision"`
		SourceGeneration  uint64  `json:"sourceGeneration"`
		SeekGeneration    uint64  `json:"seekGeneration"`
		PositionSeconds   float64 `json:"positionSeconds"`
		SelectedVersionID string  `json:"selectedVersionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &switchResp); err != nil {
		t.Fatal(err)
	}
	if switchResp.SessionID != playResp.SessionID || switchResp.SourceGeneration != 2 || switchResp.SeekGeneration != 2 || switchResp.Revision != 2 || switchResp.SelectedVersionID != hSlow || switchResp.PositionSeconds != 120 {
		t.Fatalf("switchResp = %+v", switchResp)
	}
	if sess.lastSwitchPos != 120*time.Second {
		t.Fatalf("lastSwitchPos = %v, want 120s (current position)", sess.lastSwitchPos)
	}

	// 5. Stale generation rejected with 409 stale_generation.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("POST", "/vela/v0/sessions/"+playResp.SessionID+"/commands", `{"idempotencyKey":"switch-stale","kind":"version","versionId":"`+hAlt+`","sourceGeneration":1,"seekGeneration":1}`))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"stale_generation"`) {
		t.Fatalf("stale switch = %d %s", w.Code, w.Body.String())
	}
}

func TestPluginVersionsEndpointW152ShapeAndClearChoice(t *testing.T) {
	pluginKey := frontend.PluginKey(77)
	hAuto := "00000000-0000-4000-8000-000000000101"
	h1080 := "00000000-0000-4000-8000-000000000102"
	hFeed := "00000000-0000-4000-8000-000000000103"

	editions := &versionsEditionsFake{
		slots: []frontend.ReleaseVariant{
			{
				ID:           frontend.KindAutoVersionID,
				Label:        "Auto · 4K Remux",
				Auto:         true,
				Remembered:   true,
				Availability: frontend.VersionCached,
				Release: frontend.Release{
					ID:   "kind:4k",
					Name: "Concert.2025.2160p.REMUX.HEVC-GRP",
					Size: 30 << 30,
					Info: frontend.ReleaseInfo{Height: 2160, VideoCodec: "hevc", Source: frontend.Source("remux")},
				},
				Info: frontend.MediaInfo{Estimated: true, Size: 30 << 30},
			},
			{
				ID:           "kind:1080p",
				Label:        "1080p WEB-DL",
				Availability: frontend.VersionFetch,
				Release: frontend.Release{
					ID:   "kind:1080p",
					Name: "Concert.2025.1080p.WEB-DL.H.264-GRP",
					Size: 8 << 30,
					Info: frontend.ReleaseInfo{Height: 1080, VideoCodec: "h264", Source: frontend.Source("webdl")},
				},
				Info: frontend.MediaInfo{Estimated: true, Size: 8 << 30},
			},
			{
				ID:           "kind:src:mirror",
				Label:        "Backup Mirror",
				Availability: frontend.VersionCached,
				Release: frontend.Release{
					ID:   "kind:src:mirror",
					Name: "Backup Mirror",
				},
				Info: frontend.MediaInfo{Estimated: true},
			},
		},
		choice: &frontend.ViewerChoiceSummary{
			Scope: "title", Active: true, Label: "4K Remux", ReleaseID: "kind:4k",
		},
	}
	handles := &versionsHandlesFake{
		item: pluginKey,
		toUUID: map[string]string{
			frontend.KindAutoVersionID: hAuto,
			"kind:1080p":               h1080,
			"kind:src:mirror":          hFeed,
		},
	}
	handler := mountVersionsVela(t, pluginKey, editions, handles, &versionsSessionsFake{})

	titleID := ids.RatingKeyGUID(pluginKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, velaReq("GET", "/vela/v0/titles/"+titleID+"/versions", ""))
	if w.Code != 200 {
		t.Fatalf("GET plugin versions %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		TitleID           string `json:"titleId"`
		SelectedVersionID string `json:"selectedVersionId"`
		RememberedChoice  *struct {
			Scope  string `json:"scope"`
			Active bool   `json:"active"`
			Label  string `json:"label"`
		} `json:"rememberedChoice"`
		Versions []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			Recommended bool   `json:"recommended"`
			Remembered  bool   `json:"remembered"`
			Resolution  string `json:"resolution"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TitleID != titleID || resp.SelectedVersionID != hAuto || len(resp.Versions) != 3 {
		t.Fatalf("unexpected plugin versions payload: %+v", resp)
	}
	if resp.Versions[0].ID != hAuto || !resp.Versions[0].Remembered || resp.Versions[0].Resolution != "4K" {
		t.Fatalf("index 0 must be synthetic Auto slot with remembered 4K metadata: %+v", resp.Versions[0])
	}
	if resp.Versions[1].ID != h1080 || resp.Versions[1].Resolution != "1080p" {
		t.Fatalf("index 1 = %+v, want 1080p", resp.Versions[1])
	}
	// A non-release plugin source with a human label keeps its label instead of "Standard".
	if resp.Versions[2].ID != hFeed || resp.Versions[2].Label != "Backup Mirror" {
		t.Fatalf("index 2 = %+v, want Backup Mirror label", resp.Versions[2])
	}
	if resp.RememberedChoice == nil || !resp.RememberedChoice.Active || resp.RememberedChoice.Scope != "title" {
		t.Fatalf("rememberedChoice = %+v, want active title choice", resp.RememberedChoice)
	}

	// Clearing the remembered choice succeeds for a plugin key and refreshes versions.
	wClear := httptest.NewRecorder()
	handler.ServeHTTP(wClear, velaReq("DELETE", "/vela/v0/titles/"+titleID+"/versions/choice", `{"idempotencyKey":"clear-plugin-1"}`))
	if wClear.Code != 200 || editions.clearCalls != 1 {
		t.Fatalf("DELETE choice = %d clearCalls=%d body=%s", wClear.Code, editions.clearCalls, wClear.Body.String())
	}
}
