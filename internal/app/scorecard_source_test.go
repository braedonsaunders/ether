//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestScorecardFilesAndSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	content := []byte("identical media bytes")
	if e := os.WriteFile(path, content, 0600); e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(content)
	entries := []ScorecardFile{{TMDB: 123, Path: path, SHA256: hex.EncodeToString(hash[:]), Info: core.ReleaseInfo{VideoCodec: "hevc", Container: "mkv"}}}
	if e := VerifyScorecardFiles(entries); e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(entries)
	if e := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	s := scorecardSource{dir}
	releases, e := s.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 123}, Title: "Example"})
	if e != nil || len(releases) != 1 {
		t.Fatalf("%v %v", releases, e)
	}
	payload, e := s.FetchNZB(context.Background(), releases[0])
	if e != nil {
		t.Fatal(e)
	}
	media, e := s.Open(context.Background(), releases[0].ID, payload)
	if e != nil {
		t.Fatal(e)
	}
	defer media.Close()
	buf := make([]byte, len(content))
	if _, e = media.ReadAt(buf, 0); e != nil || string(buf) != string(content) {
		t.Fatalf("%q %v", buf, e)
	}
	entries[0].SHA256 = hex.EncodeToString(make([]byte, 32))
	if VerifyScorecardFiles(entries) == nil {
		t.Fatal("changed digest accepted")
	}
}

func TestDistinctVersions(t *testing.T) {
	dir := t.TempDir()
	files := []ScorecardFile{}
	for _, body := range []string{"first", "second"} {
		path := filepath.Join(dir, body+".mp4")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(body))
		files = append(files, ScorecardFile{TMDB: 123, Path: path, SHA256: hex.EncodeToString(digest[:])})
	}
	if err := VerifyScorecardFiles(files); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(files)
	if err := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (scorecardSource{dir}).Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 123}})
	if err != nil || len(got) != 2 || got[0].ID == got[1].ID {
		t.Fatalf("versions: %+v, %v", got, err)
	}
}

func TestScorecardServeRejectsMismatchAndServesViewer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mp4")
	bytes := []byte("fixture")
	if e := os.WriteFile(path, bytes, 0600); e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(bytes)
	files := []ScorecardFile{{TMDB: 123, Path: path, SHA256: hex.EncodeToString(digest[:])}}
	data, _ := json.Marshal(files)
	if e := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	tmdb := httptest.NewServer(http.NotFoundHandler())
	defer tmdb.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", tmdb.URL)
	t.Setenv("ETHER_PARITY_PLEXTV_URL", tmdb.URL)
	handlers, closeApp, e := startScorecard(context.Background(), dir, "fixture-viewer", "fixture-password", nil, func(context.Context, core.MediaSource) (*core.MediaInfo, error) {
		return &core.MediaInfo{Container: "mp4"}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer closeApp()
	for _, name := range []string{"silo", "webdav", "stremio", "dlna"} {
		if handlers[name] == nil {
			t.Fatalf("missing %s handler", name)
		}
	}
	if handlers["plex"] != nil {
		t.Fatal("fixture unexpectedly mounted Plex")
	}
	credentialPath := filepath.Join(dir, "scorecard-app-credentials.json")
	fileInfo, err := os.Stat(credentialPath)
	if err != nil || fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("fixture credentials mode: %v", err)
	}
	credentialBytes, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]map[string]string
	if err = json.Unmarshal(credentialBytes, &credentials); err != nil {
		t.Fatal(err)
	}
	webReq := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	webReq.SetBasicAuth(credentials["webdav"]["username"], credentials["webdav"]["password"])
	webReq.Header.Set("Depth", "0")
	webResp := httptest.NewRecorder()
	handlers["webdav"].ServeHTTP(webResp, webReq)
	if webResp.Code != http.StatusMultiStatus {
		t.Fatalf("webdav login: HTTP %d", webResp.Code)
	}
	badReq := httptest.NewRequest("PROPFIND", "/webdav/", nil)
	badReq.SetBasicAuth(credentials["webdav"]["username"], credentials["stremio"]["password"])
	badResp := httptest.NewRecorder()
	handlers["webdav"].ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusUnauthorized {
		t.Fatalf("cross-surface grant accepted: HTTP %d", badResp.Code)
	}
	stremioReq := httptest.NewRequest("GET", "/stremio/"+credentials["stremio"]["password"]+"/manifest.json", nil)
	stremioResp := httptest.NewRecorder()
	handlers["stremio"].ServeHTTP(stremioResp, stremioReq)
	if stremioResp.Code != http.StatusOK {
		t.Fatalf("stremio manifest: HTTP %d", stremioResp.Code)
	}
	dlnaReq := httptest.NewRequest("GET", "/description.xml", nil)
	dlnaReq.RemoteAddr = "127.0.0.1:1234"
	dlnaResp := httptest.NewRecorder()
	handlers["dlna"].ServeHTTP(dlnaResp, dlnaReq)
	if dlnaResp.Code != http.StatusOK {
		t.Fatalf("dlna description: HTTP %d", dlnaResp.Code)
	}
	dlnaReq = httptest.NewRequest("GET", "/description.xml", nil)
	dlnaReq.RemoteAddr = "203.0.113.5:1234"
	dlnaResp = httptest.NewRecorder()
	handlers["dlna"].ServeHTTP(dlnaResp, dlnaReq)
	if dlnaResp.Code != http.StatusForbidden {
		t.Fatalf("dlna public peer: HTTP %d", dlnaResp.Code)
	}
	siloResp := httptest.NewRecorder()
	handlers["silo"].ServeHTTP(siloResp, httptest.NewRequest("GET", "/api/v2/system/identity", nil))
	if siloResp.Code != http.StatusOK {
		t.Fatalf("silo identity: HTTP %d", siloResp.Code)
	}
	for _, name := range []string{"jellyfin", "emby"} {
		rr := httptest.NewRecorder()
		handlers[name].ServeHTTP(rr, httptest.NewRequest("GET", "/System/Info/Public", nil))
		if rr.Code != 200 {
			t.Fatalf("%s HTTP %d", name, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/Users/AuthenticateByName", strings.NewReader(`{"Username":"fixture-viewer","Pw":"fixture-password"}`))
	req.Header.Set("Authorization", `MediaBrowser Client="test", DeviceId="test"`)
	req.Header.Set("Content-Type", "application/json")
	handlers["jellyfin"].ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fixture viewer login: %d %s", rr.Code, rr.Body.String())
	}
	var login struct {
		AccessToken string
		User        struct {
			Policy struct{ IsAdministrator bool }
		}
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &login); err != nil || login.AccessToken == "" || login.User.Policy.IsAdministrator {
		t.Fatalf("invalid fixture login or owner permissions: %v", err)
	}
	if _, _, e = StartScorecard(context.Background(), dir, "fixture-viewer", "fixture-password", nil); e == nil {
		t.Fatal("existing data directory accepted")
	}
}

func TestScorecardVariantSelectionUsesVerifiedBytes(t *testing.T) {
	dir := t.TempDir()
	path, e := filepath.Abs("../transcode/testdata/nativeprobe/h264_aac_movtext_faststart.mp4")
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	data = append(data, make([]byte, 6<<20)...)
	path = filepath.Join(dir, "fixture.mp4")
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(data)
	file := ScorecardFile{TMDB: 123, Path: path, SHA256: hex.EncodeToString(sum[:]), Info: core.ReleaseInfo{Height: 1080, Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "aac", Container: "mp4"}}
	secondPath := filepath.Join(dir, "second.mp4")
	secondData := append(append([]byte(nil), data...), 1)
	if e = os.WriteFile(secondPath, secondData, 0600); e != nil {
		t.Fatal(e)
	}
	secondSum := sha256.Sum256(secondData)
	second := file
	second.Path = secondPath
	second.SHA256 = hex.EncodeToString(secondSum[:])
	payload, _ := json.Marshal([]ScorecardFile{file, second})
	if e = os.WriteFile(filepath.Join(dir, "scorecard-files.json"), payload, 0600); e != nil {
		t.Fatal(e)
	}
	tmdb := httptest.NewServer(http.NotFoundHandler())
	defer tmdb.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", tmdb.URL)
	t.Setenv("ETHER_PARITY_PLEXTV_URL", tmdb.URL)
	items := []ScorecardItem{{Name: "Example", Type: "Movie", Path: path, ProviderIds: map[string]string{"Tmdb": "123"}, ProductionYear: 2020, RunTimeTicks: 600000000}}
	handlers, cleanup, e := StartScorecard(context.Background(), dir, "fixture-viewer", "fixture-password", items)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	h := handlers["jellyfin"]
	req := httptest.NewRequest("POST", "/Users/AuthenticateByName", strings.NewReader(`{"Username":"fixture-viewer","Pw":"fixture-password"}`))
	req.Header.Set("Authorization", `MediaBrowser Client="test", DeviceId="test"`)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("login %d", rr.Code)
	}
	var login struct{ AccessToken string }
	if e = json.Unmarshal(rr.Body.Bytes(), &login); e != nil || login.AccessToken == "" {
		t.Fatal(e)
	}
	sourceID := scorecardReleaseID(second)
	itemID := ids.RatingKeyGUID(core.MovieKey(123))
	requestAuth := `MediaBrowser Client="test", DeviceId="test", Token="` + login.AccessToken + `"`
	req = httptest.NewRequest("GET", "/Items/"+itemID, nil)
	req.Header.Set("Authorization", requestAuth)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("item detail HTTP %d", rr.Code)
	}
	var detail struct {
		MediaSources []struct {
			ID string `json:"Id"`
		} `json:"MediaSources"`
	}
	if e = json.Unmarshal(rr.Body.Bytes(), &detail); e != nil || len(detail.MediaSources) != 2 {
		t.Fatalf("item detail offers %d sources: %v", len(detail.MediaSources), e)
	}
	handle, otherHandle := "", ""
	for _, offered := range detail.MediaSources {
		if offered.ID == "" || offered.ID == sourceID {
			t.Fatal("version is not opaque")
		}
		req = httptest.NewRequest("GET", "/__scorecard/offer?handle="+offered.ID, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var mapped struct {
			Handle         string `json:"handle"`
			ItemID         string `json:"item_id"`
			ReleaseID      string `json:"release_id"`
			ExpectedSHA256 string `json:"expected_sha256"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &mapped) != nil {
			t.Fatalf("offer mapping HTTP %d", rr.Code)
		}
		if mapped.ItemID != itemID || mapped.Handle != offered.ID {
			t.Fatal("offer parent or handle mismatch")
		}
		if mapped.ReleaseID == sourceID && mapped.ExpectedSHA256 == second.SHA256 {
			handle = offered.ID
		} else {
			otherHandle = offered.ID
		}
	}
	if handle == "" {
		t.Fatal("matching version not offered")
	}
	for _, tc := range []struct {
		handler http.Handler
		remote  string
	}{{h, "192.0.2.1:1"}, {handlers["emby"], "127.0.0.1:1"}} {
		req = httptest.NewRequest("GET", "/__scorecard/offer?handle="+handle, nil)
		req.RemoteAddr = tc.remote
		rr = httptest.NewRecorder()
		tc.handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("offer escaped scope: HTTP %d", rr.Code)
		}
	}
	url := "/Items/" + itemID + "/PlaybackInfo"
	body, _ := json.Marshal(map[string]any{"IsPlayback": true, "MediaSourceId": handle})
	req = httptest.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", `MediaBrowser Client="test", DeviceId="test", Token="`+login.AccessToken+`"`)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("playback HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var plan struct{ PlaySessionId string }
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil || plan.PlaySessionId == "" {
		t.Fatalf("missing broker session: %v", err)
	}
	req = httptest.NewRequest("GET", "/__scorecard/sessions", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sessions []struct {
		ID             string `json:"id"`
		ReleaseID      string `json:"release_id"`
		SourceID       string `json:"source_id"`
		ExpectedSHA256 string `json:"expected_sha256"`
		ClientID       string `json:"client_id"`
		ItemID         string `json:"item_id"`
	}
	if e = json.Unmarshal(rr.Body.Bytes(), &sessions); e != nil || len(sessions) != 1 {
		t.Fatalf("sessions %s: %v", rr.Body.String(), e)
	}
	if sessions[0].ReleaseID != sourceID || sessions[0].SourceID != sourceID || sessions[0].ClientID != "8:jellyfintest" || sessions[0].ItemID != ids.RatingKeyGUID(core.MovieKey(123)) {
		t.Fatalf("selected wrong source: %+v", sessions[0])
	}
	req = httptest.NewRequest("GET", "/__scorecard/broker", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var broker []struct {
		ID         string `json:"id"`
		PlaybackID string `json:"playback_id"`
		ClientID   string `json:"client_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &broker); err != nil || len(broker) != 1 || broker[0].ID != plan.PlaySessionId || broker[0].PlaybackID != sessions[0].ID || broker[0].ClientID != sessions[0].ClientID {
		t.Fatalf("broker linkage: %s %v", rr.Body.String(), err)
	}
	req = httptest.NewRequest("GET", "/__scorecard/verify?id="+sessions[0].ID, nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var check struct {
		Match     bool   `json:"match"`
		ReleaseID string `json:"release_id"`
		SourceID  string `json:"source_id"`
		Actual    string `json:"actual_sha256"`
	}
	if e = json.Unmarshal(rr.Body.Bytes(), &check); e != nil || !check.Match || check.ReleaseID != sourceID || check.SourceID != sourceID || check.Actual != second.SHA256 {
		t.Fatalf("verification %s: %v", rr.Body.String(), e)
	}
	if otherHandle == "" {
		t.Fatal("missing other offered handle")
	}
	req = httptest.NewRequest("GET", "/Videos/"+otherHandle+"/stream?static=true&PlaySessionId="+plan.PlaySessionId+"&MediaSourceId="+otherHandle, nil)
	req.Header.Set("Authorization", requestAuth)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("different handle matched session: HTTP %d", rr.Code)
	}

}

func TestScorecardVariantsUsePreparedProbe(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fixture.mp4")
	if err := os.WriteFile(p, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("fixture"))
	entries := []ScorecardFile{{TMDB: 456, Path: p, SHA256: hex.EncodeToString(hash[:])}}
	calls := 0
	probe := func(_ context.Context, _ core.MediaSource) (*core.MediaInfo, error) {
		calls++
		return &core.MediaInfo{Container: "mp4", Size: 7}, nil
	}
	if err := prepareScorecardFiles(context.Background(), entries, probe); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	a := &App{opts: Options{DataDir: dir}}
	title := &core.Title{Type: core.TypeMovie, IDs: core.IDs{TMDB: 456}, Title: "Fixture"}
	for i := 0; i < 3; i++ {
		v, err := scorecardVariants(context.Background(), a, nil, title, core.ClientCaps{})
		if err != nil || len(v) != 1 || v[0].Info.Size != 7 {
			t.Fatalf("lookup: %+v %v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
}

func TestScorecardEventsDoNotExposeLogAttributes(t *testing.T) {
	entries := []admin.LogEntry{{Message: "prepare failed", Attrs: map[string]string{"err": "not available yet: secret /private/path", "token": "secret-token"}}, {Message: "secret message", Attrs: map[string]string{"err": "secret"}}}
	events := scorecardEvents(entries)
	data, e := json.Marshal(events)
	if e != nil {
		t.Fatal(e)
	}
	if len(events) != 1 || events[0].Category != "not available yet" || strings.Contains(string(data), "secret") || strings.Contains(string(data), "/private") {
		t.Fatalf("unexpected sanitized events: %s", data)
	}
}
