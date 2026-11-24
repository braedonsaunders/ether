package vela

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type libraryLiveExperienceFake struct {
	frontendtest.Experience
	views []frontend.View
}

func (f libraryLiveExperienceFake) Views(context.Context, *frontend.Viewer) ([]frontend.View, error) {
	return f.views, nil
}

type libraryLiveImageGrantsFake struct {
	issued []frontend.ImageGrantRequest
}

func (f *libraryLiveImageGrantsFake) Issue(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
	f.issued = append(f.issued, req)
	return "signed-" + req.Image.Subject + "-" + req.Image.Path, nil
}

func (f *libraryLiveImageGrantsFake) Serve(http.ResponseWriter, *http.Request, string) {}
func (f *libraryLiveImageGrantsFake) Revoke(context.Context, string, string) error     { return nil }

type libraryLiveSourceFake struct {
	listCalls    []frontend.LibraryLiveRequest
	resolveCalls []struct {
		viewID, tab, itemID string
	}
	pages      map[string]frontend.LibraryLivePage
	streams    map[string]frontend.LibraryLiveStream
	resolveErr error
}

func (f *libraryLiveSourceFake) ListLive(_ context.Context, _ *frontend.Viewer, _ string, req frontend.LibraryLiveRequest) (frontend.LibraryLivePage, error) {
	f.listCalls = append(f.listCalls, req)
	if page, ok := f.pages[req.Cursor]; ok {
		return page, nil
	}
	return frontend.LibraryLivePage{}, nil
}

func (f *libraryLiveSourceFake) ResolveLive(_ context.Context, _ *frontend.Viewer, viewID, tab, itemID string) (frontend.LibraryLiveStream, error) {
	f.resolveCalls = append(f.resolveCalls, struct{ viewID, tab, itemID string }{viewID, tab, itemID})
	if f.resolveErr != nil {
		return frontend.LibraryLiveStream{}, f.resolveErr
	}
	if s, ok := f.streams[itemID]; ok {
		return s, nil
	}
	return frontend.LibraryLiveStream{}, frontend.ErrNotFound
}

func TestVelaLibraryWireDateDisplayAndTitleAirDate(t *testing.T) {
	libDate := libraryWire(frontend.View{
		ID:          "42",
		LibraryID:   42,
		Name:        "Tournaments",
		Type:        frontend.TypeMovie,
		PluginID:    "esports-plugin",
		KindKey:     "streams",
		Card:        "landscape",
		DateDisplay: "date",
		Tabs: []frontend.ViewTab{
			{ID: "live", Label: "Live", Kind: "live", Card: "landscape", Search: true, SearchPlaceholder: "Filter streams"},
			{ID: "library", Label: "Archive", Kind: "library", Search: true},
		},
	})
	if libDate["dateDisplay"] != "date" {
		t.Fatalf("libraryWire dateDisplay = %v, want date", libDate["dateDisplay"])
	}
	pres := libDate["presentation"].(object)
	if len(pres) != 1 || pres["card"] != "landscape" {
		t.Fatalf("presentation must stay card-only: %+v", pres)
	}
	tabs := libDate["tabs"].([]object)
	if len(tabs) != 2 || tabs[0]["kind"] != "live" || tabs[0]["card"] != "landscape" || tabs[0]["search"] != true || tabs[0]["searchPlaceholder"] != "Filter streams" {
		t.Fatalf("unexpected live tab wire: %+v", tabs)
	}

	libYear := libraryWire(frontend.View{ID: "1", Name: "Movies", Type: frontend.TypeMovie})
	if libYear["dateDisplay"] != "year" {
		t.Fatalf("default libraryWire dateDisplay = %v, want year", libYear["dateDisplay"])
	}

	tw := titleWire(frontend.Title{
		Key:         core.PluginKey(7),
		Type:        core.TypeMovie,
		Title:       "Finals Game 1",
		Year:        2026,
		Released:    time.Date(2026, 5, 9, 22, 0, 0, 0, time.UTC),
		DateDisplay: "date",
	})
	if tw["airDate"] != "2026-05-09" {
		t.Fatalf("titleWire airDate = %v, want 2026-05-09", tw["airDate"])
	}
	if _, hasReleased := tw["released"]; hasReleased {
		t.Fatalf("titleWire must not emit parallel released field: %+v", tw)
	}
	if tw["dateDisplay"] != "date" {
		t.Fatalf("titleWire dateDisplay = %v, want date", tw["dateDisplay"])
	}

	twYear := titleWire(frontend.Title{
		Key:      core.MovieKey(1),
		Type:     core.TypeMovie,
		Title:    "Standard Movie",
		Year:     2024,
		Released: time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC),
	})
	if twYear["airDate"] != "2024-07-01" {
		t.Fatalf("standard title airDate = %v, want 2024-07-01", twYear["airDate"])
	}
	if _, hasDateDisplay := twYear["dateDisplay"]; hasDateDisplay {
		t.Fatalf("year-default title should omit dateDisplay: %+v", twYear)
	}
}

func TestVelaLibraryLiveEndpointsAndCapabilities(t *testing.T) {
	viewers := 1520
	views := []frontend.View{
		{
			ID:          "42",
			LibraryID:   42,
			Name:        "Esports",
			Type:        frontend.TypeMovie,
			PluginID:    "esports",
			KindKey:     "matches",
			Card:        "landscape",
			DateDisplay: "date",
			Tabs: []frontend.ViewTab{
				{ID: "live", Label: "Live", Kind: "live", Card: "landscape", Search: true, SearchPlaceholder: "Search streams"},
				{ID: "library", Label: "VODs", Kind: "library", Search: true},
			},
		},
		{
			ID:   "1",
			Name: "Movies",
			Type: frontend.TypeMovie,
		},
	}
	liveSrc := &libraryLiveSourceFake{
		pages: map[string]frontend.LibraryLivePage{
			"": {
				Items: []frontend.LibraryLiveItem{{
					ID:       "match-1",
					Title:    "Team A vs Team B",
					Subtitle: "Main Stage",
					Badge:    "1080p60",
					Viewers:  &viewers,
					Tags:     []string{"Championship", "EN"},
					ThumbRef: frontend.LibraryLiveThumbPrefix + "0123456789abcdef0123456789abcdef",
					Live:     true,
				}},
				Filters:    []frontend.LibraryLiveFilter{{Key: "all", Label: "All"}, {Key: "finals", Label: "Finals"}},
				NextCursor: "p2",
			},
			"p2": {
				Items: []frontend.LibraryLiveItem{{
					ID:    "match-2",
					Title: "Team C vs Team D",
					Live:  true,
				}},
				Filters: []frontend.LibraryLiveFilter{{Key: "all", Label: "All"}},
			},
		},
		streams: map[string]frontend.LibraryLiveStream{
			"match-1": {
				URL:     "https://cdn.example.invalid/live/match-1/master.m3u8?token=once",
				Headers: map[string]string{"Referer": "https://esports.example.invalid"},
				Title:   "Team A vs Team B",
			},
		},
	}
	grants := &libraryLiveImageGrantsFake{}
	var planReqs []frontend.PlanRequest
	var stopped []string
	var opens, starts int
	var openErr, startStreamErr error
	sess := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
			planReqs = append(planReqs, req)
			return frontend.PlayPlan{
				ID:          "plan-live-1",
				Decision:    core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCodec: "h264", AudioCodec: "aac", Reason: "live_remux_hls"},
				Media:       core.MediaInfo{Container: "mpegts"},
				Transcoding: true,
				HLS:         true,
			}, nil
		},
		OpenFunc: func(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
			opens++
			if err := openErr; err != nil {
				openErr = nil
				return frontend.Session{}, err
			}
			return frontend.Session{
				ID:        "sess-live-1",
				MediaPath: "/sessions/media/sess-live-1/grant/live/index.m3u8",
				Protocol:  req.Protocol,
				ViewerID:  req.Viewer.ID,
				Device:    req.Device,
				Media:     &core.MediaInfo{Container: "mpegts"},
				StartedAt: time.Now(),
			}, nil
		},
		StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, _ frontend.StreamRequest) (string, error) {
			starts++
			if err := startStreamErr; err != nil {
				startStreamErr = nil
				return "", err
			}
			return "/sessions/media/" + id + "/grant/live/index.m3u8", nil
		},
		StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error {
			stopped = append(stopped, id)
			return nil
		},
	}
	host := frontend.Host{
		Identity:     identityFake{},
		Experience:   libraryLiveExperienceFake{views: views},
		Sessions:     sess,
		Capabilities: frontendtest.Capabilities{},
		ImageGrants:  grants,
		LibraryLive:  liveSrc,
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE vela_replays(key TEXT PRIMARY KEY,digest TEXT NOT NULL,response BLOB,expires INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	inst, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	h := inst.Handler()

	doReq := func(method, target string, payload any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		var body *bytes.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		} else {
			body = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, target, body)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "dev-1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}

	// 1. Capabilities advertises features.libraryLive.available == true.
	w, caps := doReq(http.MethodGet, "/vela/v0/capabilities", nil)
	if w.Code != 200 {
		t.Fatalf("capabilities status = %d", w.Code)
	}
	feats := caps["features"].(map[string]any)
	libLiveFeat := feats["libraryLive"].(map[string]any)
	if libLiveFeat["available"] != true {
		t.Fatalf("features.libraryLive = %+v, want available=true", libLiveFeat)
	}

	// 2. GET /libraries/42/live page 1 with query/filter/limit returns exact contract and signed artwork.
	w, p1 := doReq(http.MethodGet, "/vela/v0/libraries/42/live?tab=live&query=team&filter=finals&limit=20", nil)
	if w.Code != 200 {
		t.Fatalf("live page 1 status = %d body = %s", w.Code, w.Body.String())
	}
	if p1["continuation"] != "more" {
		t.Fatalf("continuation = %v, want more", p1["continuation"])
	}
	nextCursor, _ := p1["nextCursor"].(string)
	if nextCursor == "" || nextCursor == "p2" {
		t.Fatalf("nextCursor must be sealed opaque token, got %q", nextCursor)
	}
	items := p1["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	first := items[0].(map[string]any)
	if first["id"] != "match-1" || first["title"] != "Team A vs Team B" || first["subtitle"] != "Main Stage" || first["badge"] != "1080p60" || first["viewers"].(float64) != 1520 || first["live"] != true {
		t.Fatalf("unexpected live item: %+v", first)
	}
	art := first["artwork"].([]any)
	if len(art) != 1 || len(grants.issued) != 1 || grants.issued[0].Image.Subject != "42" || grants.issued[0].Item != 0 || grants.issued[0].Channel != "" {
		t.Fatalf("unexpected artwork signing: art=%+v issued=%+v", art, grants.issued)
	}

	// 3. Page 2 using sealed cursor succeeds and exhausts; query-mismatched cursor 409s.
	w, p2 := doReq(http.MethodGet, "/vela/v0/libraries/42/live?tab=live&query=team&filter=finals&limit=20&cursor="+nextCursor, nil)
	if w.Code != 200 || p2["continuation"] != "exhausted" {
		t.Fatalf("live page 2 status = %d body = %s", w.Code, w.Body.String())
	}
	w, _ = doReq(http.MethodGet, "/vela/v0/libraries/42/live?tab=live&query=other&filter=finals&limit=20&cursor="+nextCursor, nil)
	if w.Code != 409 {
		t.Fatalf("mismatched cursor status = %d, want 409", w.Code)
	}

	// 4. Non-live library 404s; unknown query param 400s.
	w, _ = doReq(http.MethodGet, "/vela/v0/libraries/1/live", nil)
	if w.Code != 404 {
		t.Fatalf("non-live library status = %d, want 404", w.Code)
	}
	w, _ = doReq(http.MethodGet, "/vela/v0/libraries/42/live?bogus=1", nil)
	if w.Code != 400 {
		t.Fatalf("unknown query param status = %d, want 400", w.Code)
	}

	// 5. POST /libraries/42/live/play returns PlaybackPlan with additive fields and honors idempotencyKey.
	playReq := object{
		"idempotencyKey": "live-play-1",
		"itemId":         "match-1",
		"tab":            "live",
		"liveMode":       "low-delay",
		"client": object{
			"device":           object{"id": "dev-1", "name": "TV", "platform": "web", "appVersion": "0.2.0"},
			"protocolVersions": []int{0},
			"hls":              true,
			"mse":              true,
			"videoCodecs":      []string{"h264"},
			"audioCodecs":      []string{"aac"},
		},
	}
	w, plan := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", playReq)
	if w.Code != 200 {
		t.Fatalf("live play status = %d body = %s", w.Code, w.Body.String())
	}
	if plan["sessionId"] != "sess-live-1" || plan["libraryId"] != "42" || plan["liveItemId"] != "match-1" || plan["title"] != "Team A vs Team B" {
		t.Fatalf("unexpected live PlaybackPlan: %+v", plan)
	}
	liveMeta := plan["live"].(map[string]any)
	if liveMeta["mode"] != "low-delay" {
		t.Fatalf("live.mode = %v, want low-delay", liveMeta["mode"])
	}
	if len(planReqs) != 1 || planReqs[0].DirectLive == nil || planReqs[0].Channel != "" || planReqs[0].DirectLive.LibraryID != 42 {
		t.Fatalf("broker PlanRequest mismatch: %+v", planReqs)
	}

	// Retry with identical idempotencyKey replays without calling ResolveLive or Plan again.
	w2, plan2 := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", playReq)
	if w2.Code != 200 || plan2["sessionId"] != "sess-live-1" || len(planReqs) != 1 || len(liveSrc.resolveCalls) != 1 {
		t.Fatalf("idempotency replay failed: code=%d planReqs=%d resolves=%d", w2.Code, len(planReqs), len(liveSrc.resolveCalls))
	}

	// Standard session snapshot & stop lifecycle work on the live session.
	wSnap, snap := doReq(http.MethodGet, "/vela/v0/sessions/sess-live-1", nil)
	snapPlan, _ := snap["plan"].(map[string]any)
	if wSnap.Code != 200 || snapPlan["libraryId"] != "42" || snapPlan["liveItemId"] != "match-1" {
		t.Fatalf("session snapshot status=%d body=%s", wSnap.Code, wSnap.Body.String())
	}
	wStop, _ := doReq(http.MethodPost, "/vela/v0/sessions/sess-live-1/commands", object{
		"idempotencyKey":   "stop-live-1",
		"kind":             "stop",
		"sourceGeneration": 1,
		"seekGeneration":   1,
	})
	if wStop.Code != 200 || len(stopped) != 1 || stopped[0] != "sess-live-1" {
		t.Fatalf("session stop status=%d stopped=%v body=%s", wStop.Code, stopped, wStop.Body.String())
	}

	// Missing idempotencyKey is rejected with 400.
	noKey := object{"itemId": "match-1", "client": playReq["client"]}
	wNoKey, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", noKey)
	if wNoKey.Code != 400 {
		t.Fatalf("missing idempotencyKey status = %d, want 400", wNoKey.Code)
	}

	// No start slot after Open: the opened session is stopped and the 503 with
	// Retry-After is not kept for its key, so the client's retry with the same
	// key runs the handler again. The success is then replayed.
	retryReq := object{"idempotencyKey": "live-play-retry", "itemId": "match-1", "tab": "live", "client": playReq["client"]}
	stopsBefore := len(stopped)
	startStreamErr = &frontend.SessionBusyError{RetryAfter: time.Second}
	wBusy, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", retryReq)
	if wBusy.Code != 503 || wBusy.Header().Get("Retry-After") != "1" {
		t.Fatalf("refused live play status = %d Retry-After=%q body=%s", wBusy.Code, wBusy.Header().Get("Retry-After"), wBusy.Body.String())
	}
	if len(stopped) != stopsBefore+1 || stopped[len(stopped)-1] != "sess-live-1" {
		t.Fatalf("refused live play did not stop the opened session: stopped=%v", stopped)
	}
	plansBefore, startsBefore := len(planReqs), starts
	wRetry, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", retryReq)
	if wRetry.Code != 200 || len(planReqs) != plansBefore+1 || starts != startsBefore+1 {
		t.Fatalf("live play retry with the same key = %d plans=%d starts=%d (before %d/%d) body=%s", wRetry.Code, len(planReqs), starts, plansBefore, startsBefore, wRetry.Body.String())
	}
	wReplay, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", retryReq)
	if wReplay.Code != 200 || len(planReqs) != plansBefore+1 || starts != startsBefore+1 {
		t.Fatalf("live play success not replayed: %d plans=%d starts=%d", wReplay.Code, len(planReqs), starts)
	}

	// An Open failure on live is answered without Retry-After, so it is still
	// stored for its key and replayed.
	openReq := object{"idempotencyKey": "live-play-open-fail", "itemId": "match-1", "tab": "live", "client": playReq["client"]}
	openErr = &frontend.SessionBusyError{RetryAfter: time.Second}
	opensBefore := opens
	wOpen, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", openReq)
	if wOpen.Code != 503 || wOpen.Header().Get("Retry-After") != "" {
		t.Fatalf("live Open failure status = %d Retry-After=%q body=%s", wOpen.Code, wOpen.Header().Get("Retry-After"), wOpen.Body.String())
	}
	wOpenAgain, _ := doReq(http.MethodPost, "/vela/v0/libraries/42/live/play", openReq)
	if wOpenAgain.Code != 503 || opens != opensBefore+1 || wOpenAgain.Body.String() != wOpen.Body.String() {
		t.Fatalf("live Open failure not replayed: %d opens=%d (before %d) body=%s", wOpenAgain.Code, opens, opensBefore, wOpenAgain.Body.String())
	}

	// Feature unavailable when Host.LibraryLive == nil.
	hostOff := host
	hostOff.LibraryLive = nil
	instOff, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(context.Background(), hostOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instOff.Close(context.Background()) })
	rCap := httptest.NewRequest(http.MethodGet, "/vela/v0/capabilities", nil)
	wCap := httptest.NewRecorder()
	instOff.Handler().ServeHTTP(wCap, rCap)
	var capOff map[string]any
	_ = json.Unmarshal(wCap.Body.Bytes(), &capOff)
	if capOff["features"].(map[string]any)["libraryLive"].(map[string]any)["available"] != false {
		t.Fatalf("expected libraryLive.available=false when Host.LibraryLive is nil")
	}
}
