//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
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
	for _, name := range []string{"silo", "webdav", "stremio", "dlna", "m3u"} {
		if handlers[name] == nil {
			t.Fatalf("missing %s handler", name)
		}
	}
	if handlers["plex"] != nil {
		t.Fatal("fixture unexpectedly mounted Plex")
	}
	remoteDiagnostic := httptest.NewRequest("GET", "/__scorecard/detail-offer?handle=0fd07009-a5dc-4225-9ac3-dc79b1ad89d5", nil)
	remoteDiagnostic.RemoteAddr = "203.0.113.2:1234"
	remoteResponse := httptest.NewRecorder()
	handlers["jellyfin"].ServeHTTP(remoteResponse, remoteDiagnostic)
	if remoteResponse.Code != http.StatusNotFound {
		t.Fatalf("remote detail diagnostic: HTTP %d", remoteResponse.Code)
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
	m3uReq := httptest.NewRequest("GET", "/m3u/"+credentials["m3u"]["password"]+"/playlist.m3u", nil)
	m3uReq.RemoteAddr = "127.0.0.1:1234"
	m3uReq = m3uReq.WithContext(context.WithValue(m3uReq.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18102}))
	m3uResp := httptest.NewRecorder()
	handlers["m3u"].ServeHTTP(m3uResp, m3uReq)
	if m3uResp.Code != http.StatusOK || !strings.HasPrefix(m3uResp.Body.String(), "#EXTM3U") {
		t.Fatalf("m3u playlist: HTTP %d", m3uResp.Code)
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
	server := httptest.NewServer(h)
	defer server.Close()
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
	defer func() {
		r := httptest.NewRequest("GET", "/__scorecard/broker", nil)
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var active []struct {
			ID string `json:"id"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &active) != nil {
			t.Error("could not enumerate sessions for cleanup")
			return
		}
		for _, entry := range active {
			body, _ := json.Marshal(map[string]string{"PlaySessionId": entry.ID, "ItemId": itemID})
			r = httptest.NewRequest("POST", "/Sessions/Playing/Stopped", strings.NewReader(string(body)))
			r.Header.Set("Authorization", requestAuth)
			r.Header.Set("Content-Type", "application/json")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Errorf("stop failed: HTTP %d", w.Code)
			}
		}
		r = httptest.NewRequest("POST", "/Sessions/Logout", strings.NewReader("{}"))
		r.Header.Set("Authorization", requestAuth)
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("logout failed: HTTP %d", w.Code)
		}
		r = httptest.NewRequest("GET", "/__scorecard/broker", nil)
		r.RemoteAddr = "127.0.0.1:1"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		active = nil
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &active) != nil || len(active) != 0 {
			t.Error("broker not empty after cleanup")
		}
	}()
	req, e = http.NewRequest("GET", server.URL+"/Items/"+itemID, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", requestAuth)
	resp, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	detailBody, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("item detail HTTP %d: %v", resp.StatusCode, e)
	}
	var detail struct {
		MediaSources []struct {
			ID   string `json:"Id"`
			Path string `json:"Path"`
			ETag string `json:"ETag"`
		} `json:"MediaSources"`
	}
	if e = json.Unmarshal(detailBody, &detail); e != nil || len(detail.MediaSources) != 3 {
		t.Fatalf("item detail offers %d sources: %v", len(detail.MediaSources), e)
	}
	handle, otherHandle := "", ""
	var detailSourceID, detailPath, detailRevision, detailSHA string
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
		if rr.Code == http.StatusNotFound {
			req = httptest.NewRequest("GET", "/__scorecard/detail-offer?handle="+offered.ID, nil)
			req.RemoteAddr = "127.0.0.1:1"
			rr = httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			var bound scorecardDetailOfferRow
			if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &bound) != nil || bound.Handle != offered.ID || bound.ItemID != itemID || bound.Revision != offered.ETag || bound.ExpectedSHA256 == "" || offered.Path == "" || detailSourceID != "" {
				t.Fatalf("invalid bound detail offer: HTTP %d id=%v item=%v revision=%v digest=%v path=%v duplicate=%v", rr.Code, bound.Handle == offered.ID, bound.ItemID == itemID, bound.Revision == offered.ETag, bound.ExpectedSHA256 != "", offered.Path != "", detailSourceID != "")
			}
			detailSourceID, detailPath, detailRevision, detailSHA = bound.ReleaseID, offered.Path, bound.Revision, bound.ExpectedSHA256
			continue
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
	if detailSourceID == "" || detailRevision == "" {
		t.Fatal("missing bound detail offer")
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
	playbackURL := "/Items/" + itemID + "/PlaybackInfo"
	body, _ := json.Marshal(map[string]any{"IsPlayback": true, "MediaSourceId": handle})
	req = httptest.NewRequest("POST", playbackURL, strings.NewReader(string(body)))
	// The fixture appends 6 MiB so the verified hash covers a byte past that
	// tail. ffprobe 8.1.3 reports that tail as format size 6322121 and
	// bit_rate 101052883. A loopback client keeps the 8000 kbps remote cap
	// out of this test; httptest's default peer is the remote address 192.0.2.1.
	req.RemoteAddr = "127.0.0.1:1"
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
	if e = json.Unmarshal(rr.Body.Bytes(), &sessions); e != nil {
		t.Fatalf("invalid sessions: %v", e)
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
	if err := json.Unmarshal(rr.Body.Bytes(), &broker); err != nil {
		t.Fatalf("invalid broker sessions: %v", err)
	}
	selectedID := ""
	for _, row := range broker {
		if row.ID == plan.PlaySessionId && row.ClientID == "8:jellyfintest" {
			selectedID = row.PlaybackID
		}
	}
	matched := false
	for _, row := range sessions {
		if row.ID == selectedID && row.ReleaseID == sourceID && row.SourceID == sourceID && row.ClientID == "8:jellyfintest" && row.ItemID == itemID {
			matched = true
		}
	}
	if !matched {
		t.Fatal("broker did not bind requested version to selected source")
	}
	req = httptest.NewRequest("GET", "/__scorecard/verify?id="+selectedID, nil)
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

	// The source-bound default must be an authenticated offer. Following its
	// lazy grant opens the same source, independently verified by actual bytes.
	parsed, err := url.Parse(detailPath)
	if err != nil || parsed.Host == "" || !strings.HasPrefix(parsed.Path, "/sessions/lazy/") {
		t.Fatal("invalid detail path")
	}
	if parsed.Scheme+"://"+parsed.Host != server.URL {
		t.Fatal("detail path uses an unexpected origin")
	}
	req, err = http.NewRequest("GET", detailPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-127")
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusPartialContent || len(readBytes) != 128 {
		t.Fatalf("bound source direct read: HTTP %d, %d bytes, %v", resp.StatusCode, len(readBytes), err)
	}
	req = httptest.NewRequest("GET", "/__scorecard/sessions", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	sessions = nil
	if json.Unmarshal(rr.Body.Bytes(), &sessions) != nil {
		t.Fatal("invalid sessions")
	}
	req = httptest.NewRequest("GET", "/__scorecard/broker", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	broker = nil
	if json.Unmarshal(rr.Body.Bytes(), &broker) != nil {
		t.Fatal("invalid broker sessions after lazy read")
	}
	lazyPlaybackIDs := map[string]bool{}
	for _, row := range broker {
		if row.ID != plan.PlaySessionId && row.ClientID == "8:jellyfintest" {
			lazyPlaybackIDs[row.PlaybackID] = true
		}
	}
	verified := false
	for _, session := range sessions {
		if !lazyPlaybackIDs[session.ID] || session.ReleaseID != detailSourceID || session.SourceID != detailSourceID || session.ClientID != "8:jellyfintest" || session.ItemID != itemID {
			continue
		}
		req = httptest.NewRequest("GET", "/__scorecard/verify?id="+session.ID, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var actual struct {
			Match  bool   `json:"match"`
			Actual string `json:"actual_sha256"`
		}
		if rr.Code == http.StatusOK && json.Unmarshal(rr.Body.Bytes(), &actual) == nil && actual.Match && actual.Actual == session.ExpectedSHA256 && actual.Actual == detailSHA {
			verified = true
		}
	}
	if !verified {
		t.Fatal("detail source opened without verified matching bytes")
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

func TestScorecardResolverTracesRedactUntrustedText(t *testing.T) {
	traces := []resolver.Trace{{TitleKey: core.MovieKey(123), Err: "secret-token /private/path", Winner: "private", Candidates: []resolver.CandidateTrace{{Name: "secret", ReleaseID: "secret", Reason: "secret-token /private/path", CompatibilityReason: "secret"}, {Rejected: true, Reason: "video codec unsupported by client"}}}}
	b, err := json.Marshal(scorecardResolverTraces(traces))
	if err != nil || strings.Contains(string(b), "secret") || strings.Contains(string(b), "/private") || !strings.Contains(string(b), "video codec unsupported by client") {
		t.Fatalf("trace not redacted: %s %v", b, err)
	}
}

func TestScorecardDetailOfferMatchesFixtureAndScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	entry := ScorecardFile{TMDB: 123, SHA256: strings.Repeat("a", 64)}
	data, _ := json.Marshal([]ScorecardFile{entry})
	if err := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handle := "0fd07009-a5dc-4225-9ac3-dc79b1ad89d5"
	revision := "src-" + strings.Repeat("b", 32)
	now := time.Now()
	_, err = db.ExecContext(ctx, `INSERT INTO sessions_detail_sources(handle,viewer_id,surface,device_id,item_id,release_id,revision,expires) VALUES(?,?,?,?,?,?,?,?)`, handle, 1, "jellyfin", "device", int64(core.MovieKey(123)), scorecardReleaseID(entry), revision, now.Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	got, err := scorecardDetailOffer(ctx, db, dir, "jellyfin", handle, now)
	if err != nil || got.ReleaseID != scorecardReleaseID(entry) || got.ItemID != ids.RatingKeyGUID(core.MovieKey(123)) || got.ExpectedSHA256 != entry.SHA256 || got.Revision != revision {
		t.Fatalf("offer: %+v %v", got, err)
	}
	if _, err = scorecardDetailOffer(ctx, db, dir, "emby", handle, now); err == nil {
		t.Fatal("cross-surface offer accepted")
	}
	if _, err = scorecardDetailOffer(ctx, db, dir, "jellyfin", handle, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired offer accepted")
	}
}

func TestScorecardLabBinariesAndExtraViewers(t *testing.T) {
	t.Setenv("SCORECARD_FFMPEG", "/opt/lab/ffmpeg")
	t.Setenv("SCORECARD_FFPROBE", "/opt/lab/ffprobe")
	cfg := &config.Config{}
	applyScorecardLabBinaries(cfg)
	if cfg.Transcode.FFmpegPath != "/opt/lab/ffmpeg" || cfg.Transcode.FFprobePath != "/opt/lab/ffprobe" {
		t.Fatalf("binaries: %+v", cfg.Transcode)
	}
	t.Setenv("SCORECARD_EXTRA_USERS", `[{"username":"adult","password":"secret-a"},{"username":"child","password":"secret-c"}]`)
	viewers, err := scorecardExtraViewers()
	if err != nil || len(viewers) != 2 || viewers[0][0] != "adult" || viewers[1][0] != "child" {
		t.Fatalf("viewers: %+v %v", viewers, err)
	}
	t.Setenv("SCORECARD_EXTRA_USERS", `[{"username":"adult","password":""}]`)
	if _, err = scorecardExtraViewers(); err == nil {
		t.Fatal("blank extra password accepted")
	}
}

func TestScorecardSearchUsesProbedSizeWithoutStat(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("ab", 32)
	files := []ScorecardFile{{
		TMDB: 10378, Path: filepath.Join(dir, "missing.mp4"), SHA256: sum,
		Info:  core.ReleaseInfo{Height: 1080, VideoCodec: "h264", AudioCodec: "aac"},
		Media: &core.MediaInfo{Size: 42, Container: "mp4"},
	}}
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "scorecard-files.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (scorecardSource{dir}).Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 10378}, Title: "Big Buck Bunny", Year: 2008})
	if err != nil || len(got) != 1 || got[0].Size != 42 {
		t.Fatalf("search: %+v %v", got, err)
	}
	again, err := (scorecardSource{dir}).Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 10378}, Title: "Big Buck Bunny", Year: 2008})
	if err != nil || len(again) != 1 || again[0].Size != 42 {
		t.Fatalf("cached search: %+v %v", again, err)
	}
}
