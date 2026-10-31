package livetv

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

var guideTestNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// xmltvFixture builds a guide with hourly programmes for each channel id,
// starting an hour before guideTestNow and running for the given hours.
func xmltvFixture(hours int, channels ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><tv>`)
	for _, c := range channels {
		id, names, _ := strings.Cut(c, "=")
		if names != "" {
			b.WriteString(`<channel id="` + id + `">`)
			for _, n := range strings.Split(names, "|") {
				b.WriteString(`<display-name>` + n + `</display-name>`)
			}
			b.WriteString(`<icon src="https://owner.invalid/` + id + `.png"/></channel>`)
		}
	}
	for _, c := range channels {
		id, _, _ := strings.Cut(c, "=")
		for h := 0; h < hours; h++ {
			start := guideTestNow.Add(time.Duration(h-1) * time.Hour)
			b.WriteString(`<programme channel="` + id + `" start="` + start.Format("20060102150405 -0700") + `" stop="` + start.Add(time.Hour).Format("20060102150405 -0700") + `"><title>` + id + ` show</title></programme>`)
		}
	}
	b.WriteString(`</tv>`)
	return b.String()
}

func m3uFixture(entries ...string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i, e := range entries {
		id, name, _ := strings.Cut(e, "=")
		attr := ""
		if id != "" {
			attr = ` tvg-id="` + id + `"`
		}
		b.WriteString("#EXTINF:-1" + attr + "," + name + "\nhttps://owner.invalid/live/" + string(rune('a'+i)) + ".m3u8\n")
	}
	return b.String()
}

func ownerJSON(t *testing.T, s *Service, method, target, body string, out any) int {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if out != nil && rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, target, err, rec.Body.String())
		}
	}
	if rec.Code != 200 {
		t.Logf("%s %s -> %d %s", method, target, rec.Code, rec.Body.String())
	}
	return rec.Code
}

func sourceByID(t *testing.T, s *Service, id string) Source {
	t.Helper()
	srcs, err := s.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range srcs {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("source %s missing", id)
	return Source{}
}

func TestGuideOnlySourceAndGuideChannelSearch(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	if _, err := s.ImportSource(ctx, Source{ID: "empty", Name: "Nothing"}); err == nil {
		t.Fatal("source without playlist or guide accepted")
	}
	guide := xmltvFixture(30, "news.one=News One|N1 Network", "sport.two=Sport Two HD") + ""
	// A programme channel without a <channel> element is still searchable.
	guide = strings.Replace(guide, "</tv>", `<programme channel="bare.id" start="20261019120000 +0000" stop="20261019130000 +0000"><title>x</title></programme></tv>`, 1)
	if _, err := s.ImportSource(ctx, Source{ID: "epg", Name: "Owner EPG", Guide: guide}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	if len(cs) != 0 {
		t.Fatalf("guide-only source created channels: %+v", cs)
	}
	src := sourceByID(t, s, "epg")
	if src.HasPlaylist || !src.HasGuide || src.ChannelCount != 0 || src.GuideChannelCount != 3 || src.ProgrammeCount != 61 {
		t.Fatalf("source stats: %+v", src)
	}
	if src.GuideFrom == nil || !src.GuideFrom.Equal(guideTestNow.Add(-time.Hour)) || src.GuideUntil == nil || !src.GuideUntil.Equal(guideTestNow.Add(29*time.Hour)) {
		t.Fatalf("guide span: %v %v", src.GuideFrom, src.GuideUntil)
	}
	if src.NextRefresh != nil || src.Guide != "" {
		t.Fatalf("inline source must not auto refresh or leak upload: %+v", src)
	}

	var found []GuideChannel
	if ownerJSON(t, s, "GET", "/guide/channels?q=n1%20net", "", &found) != 200 || len(found) != 1 || found[0].GuideID != "news.one" || found[0].Name != "News One" || found[0].SourceName != "Owner EPG" || found[0].Logo == "" {
		t.Fatalf("display-name search: %+v", found)
	}
	if found[0].Programmes != 29 || found[0].Until == nil || !found[0].Until.Equal(guideTestNow.Add(29*time.Hour)) {
		t.Fatalf("programme summary: %+v", found[0])
	}
	ownerJSON(t, s, "GET", "/guide/channels?q=SPORT.TWO", "", &found)
	if len(found) != 1 || found[0].GuideID != "sport.two" {
		t.Fatalf("id search: %+v", found)
	}
	ownerJSON(t, s, "GET", "/guide/channels?q=bare", "", &found)
	if len(found) != 1 || found[0].Name != "bare.id" {
		t.Fatalf("bare programme channel: %+v", found)
	}
	ownerJSON(t, s, "GET", "/guide/channels?limit=2", "", &found)
	if len(found) != 2 {
		t.Fatalf("limit: %+v", found)
	}
	ownerJSON(t, s, "GET", "/guide/channels?source=other", "", &found)
	if len(found) != 0 {
		t.Fatalf("source filter: %+v", found)
	}

	// Editing the source without re-uploading keeps the stored guide.
	var saved Source
	if ownerJSON(t, s, "PUT", "/sources/epg", `{"name":"Renamed EPG","refreshHours":-1}`, &saved) != 200 || saved.Name != "Renamed EPG" || saved.ProgrammeCount != 61 || !saved.HasGuide {
		t.Fatalf("metadata edit: %+v", saved)
	}
	if err := s.DeleteSource(ctx, "epg"); err != nil {
		t.Fatal(err)
	}
	ownerJSON(t, s, "GET", "/guide/channels", "", &found)
	if len(found) != 0 {
		t.Fatalf("guide channels survived delete: %+v", found)
	}
}

func TestCrossSourceGuideMappingSurvivesReimportAndSave(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	playlist := Source{ID: "iptv", Name: "Playlist", Playlist: m3uFixture("local.id=Local Channel", "=Second")}
	if _, err := s.ImportSource(ctx, playlist); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "epg", Name: "EPG", Guide: xmltvFixture(10, "remote.id=Remote")}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	c := cs[0]
	if c.GuideID != "local.id" {
		t.Fatalf("unexpected fixture order: %+v", cs)
	}
	ps, _ := s.programmes(ctx, c, guideTestNow, guideTestNow.Add(time.Hour))
	if len(ps) != 0 {
		t.Fatal("channel has guide before mapping")
	}
	// The owner points the channel at another source's guide through PUT,
	// which rejects unknown fields.
	c.GuideSourceID = "epg"
	c.GuideID = "remote.id"
	c.Transcode = true
	body, _ := json.Marshal(c)
	if code := ownerJSON(t, s, "PUT", "/channels/"+c.ID, string(body), nil); code != 200 {
		t.Fatalf("save mapping: %d", code)
	}
	g, err := s.Guide(ctx, ownerUser(), guideTestNow, guideTestNow.Add(2*time.Hour))
	if err != nil || len(g) != 2 || g[0].ChannelID != c.ID || g[0].Item.Title != "remote.id show" {
		t.Fatalf("cross-source guide: %v %+v", err, g)
	}
	if _, err = s.ImportSource(ctx, playlist); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Channel(ctx, c.ID)
	if after.GuideSourceID != "epg" || after.GuideID != "remote.id" || !after.Transcode {
		t.Fatalf("re-import lost owner mapping: %+v", after)
	}
	var cov []GuideCoverage
	ownerJSON(t, s, "GET", "/guide/coverage", "", &cov)
	byID := map[string]GuideCoverage{}
	for _, v := range cov {
		byID[v.ChannelID] = v
	}
	if got := byID[c.ID]; got.Programmes != 9 || got.Until == nil || !got.Until.Equal(guideTestNow.Add(9*time.Hour)) {
		t.Fatalf("coverage mapped: %+v", got)
	}
	if got := byID[cs[1].ID]; got.Programmes != 0 || got.Until != nil {
		t.Fatalf("coverage unmapped: %+v", got)
	}
	// Deleting the guide source drops dangling references.
	if err = s.DeleteSource(ctx, "epg"); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Channel(ctx, c.ID)
	if after.GuideSourceID != "" {
		t.Fatalf("dangling guide source: %+v", after)
	}
}

func ownerUser() *core.User { return &core.User{ID: 1, IsOwner: true} }

func TestGuideCoverageIncludesVirtualAndDisabled(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	v := testChannel()
	v.Enabled = false
	if _, err := s.SaveChannel(ctx, v); err != nil {
		t.Fatal(err)
	}
	cov, err := s.Coverage(ctx)
	if err != nil || len(cov) != 1 || cov[0].ChannelID != v.ID || cov[0].Programmes == 0 || cov[0].Until == nil || cov[0].Until.Before(guideTestNow.Add(24*time.Hour)) {
		t.Fatalf("virtual coverage: %v %+v", err, cov)
	}
}

func TestMatchGuidesByIDAndNameWithDryRunAndOverwrite(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	// The playlist's own guide covers "own.id" and a "Shared Name" channel.
	own := Source{ID: "a-own", Name: "Own", Playlist: m3uFixture(
		"news.us=News Network",            // exact id in the EPG source
		"=US: Sports Central FHD",         // name match with country prefix + quality
		"=UK | Movies +1 HD",              // +1 must not match plain "Movies"
		"=Nothing Like It",                // unmatched
		"own.id=Own Covered",              // already covered by own guide
		"=Shared Name",                    // no tvg-id: the playlist guide does not keep this channel
		"NEWS.US.ALT=Different Name Here", // case-insensitive id
	), Guide: xmltvFixture(30, "own.id=Own Covered", "shared.own=Shared Name")}
	if _, err := s.ImportSource(ctx, own); err != nil {
		t.Fatal(err)
	}
	epg := Source{ID: "b-epg", Name: "EPG", Guide: xmltvFixture(30, "news.us=CNN-ish", "sports=Sports Central|SC", "movies=Movies", "movies1=Movies +1", "own.id=Own Covered", "shared.epg=Shared Name", "news.us.alt=Alt", "stale=Nothing Like It")}
	// "stale" has a channel element but no future programmes in this guide.
	epg.Guide = strings.ReplaceAll(epg.Guide, `<programme channel="stale"`, `<programme channel="stale-old"`)
	if _, err := s.ImportSource(ctx, epg); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	byName := map[string]Channel{}
	for _, c := range cs {
		byName[c.Name] = c
	}

	var dry GuideMatchResult
	if ownerJSON(t, s, "POST", "/guide/match", `{"dryRun":true}`, &dry) != 200 {
		t.Fatal("dry run failed")
	}
	if !dry.DryRun || dry.Matched != 5 || dry.Unmatched != 1 || dry.Skipped != 1 || len(dry.Changes) != 5 {
		t.Fatalf("dry run result: %+v", dry)
	}
	want := map[string][3]string{
		"News Network":           {"b-epg", "news.us", "EPG"},
		"US: Sports Central FHD": {"b-epg", "sports", "EPG"},
		"UK | Movies +1 HD":      {"b-epg", "movies1", "EPG"},
		"Different Name Here":    {"b-epg", "news.us.alt", "EPG"},
		// The playlist source keeps only channels its M3U maps, so the
		// unmapped "Shared Name" listing lives on the guide-only source.
		"Shared Name": {"b-epg", "shared.epg", "EPG"},
	}
	for _, ch := range dry.Changes {
		w, ok := want[ch.ChannelName]
		if !ok || ch.GuideSourceID != w[0] || ch.GuideID != w[1] || ch.GuideSourceName != w[2] {
			t.Fatalf("unexpected change %+v", ch)
		}
	}
	if c, _ := s.Channel(ctx, byName["News Network"].ID); c.GuideSourceID != "" {
		t.Fatal("dry run saved changes")
	}

	var res GuideMatchResult
	ownerJSON(t, s, "POST", "/guide/match", `{"channelIds":["`+byName["News Network"].ID+`"]}`, &res)
	if res.Matched != 1 || len(res.Changes) != 1 {
		t.Fatalf("restricted match: %+v", res)
	}
	ownerJSON(t, s, "POST", "/guide/match", `{}`, &res)
	if res.Matched != 4 || res.Unmatched != 1 || res.Skipped != 2 || len(res.Changes) != 4 {
		t.Fatalf("match result: %+v", res)
	}
	sports, _ := s.Channel(ctx, byName["US: Sports Central FHD"].ID)
	if sports.GuideSourceID != "b-epg" || sports.GuideID != "sports" {
		t.Fatalf("name match not saved: %+v", sports)
	}
	ps, _ := s.programmes(ctx, sports, guideTestNow, guideTestNow.Add(time.Hour))
	if len(ps) != 1 || ps[0].Item.Title != "sports show" {
		t.Fatalf("matched channel has no guide: %+v", ps)
	}

	// Shared Name was not in the playlist's tvg-ids, so the guide-only source supplies it.
	if shared, _ := s.Channel(ctx, byName["Shared Name"].ID); shared.GuideSourceID != "b-epg" || shared.GuideID != "shared.epg" {
		t.Fatalf("unmapped playlist guide channel was kept: %+v", shared)
	}

	// A covered channel mapped to another source keeps it unless overwrite
	// is requested, in which case its own source wins.
	covered := byName["Own Covered"]
	covered.GuideSourceID = "b-epg"
	if _, err := s.SaveChannel(ctx, covered); err != nil {
		t.Fatal(err)
	}
	ownerJSON(t, s, "POST", "/guide/match", `{}`, &res)
	if c, _ := s.Channel(ctx, covered.ID); c.GuideSourceID != "b-epg" {
		t.Fatalf("covered channel changed without overwrite: %+v", c)
	}
	ownerJSON(t, s, "POST", "/guide/match", `{"overwrite":true}`, &res)
	if c, _ := s.Channel(ctx, covered.ID); c.GuideSourceID != "" || c.GuideID != "own.id" {
		t.Fatalf("overwrite did not prefer own source: %+v", c)
	}
	if res.Skipped != 0 || res.Unmatched != 1 || len(res.Changes) != 1 {
		t.Fatalf("overwrite result: %+v", res)
	}
}

func TestNormalizeGuideName(t *testing.T) {
	same := [][2]string{
		{"US: CNN HD", "CNN"},
		{"UK | BBC One FHD", "bbc one"},
		{"|UK| BBC One", "BBC-One"},
		{"Discovery (US) 4K", "Discovery"},
		{"Channel 4 +1", "Channel 4+1"},
		{"Sky Sports [VIP] UHD", "Sky Sports"},
	}
	for _, v := range same {
		if a, b := normalizeGuideName(v[0]), normalizeGuideName(v[1]); a != b || a == "" {
			t.Errorf("%q (%q) != %q (%q)", v[0], a, v[1], b)
		}
	}
	diff := [][2]string{{"Channel 4 +1", "Channel 4"}, {"Movies (+1)", "Movies"}, {"Sport 1", "Sport 2"}}
	for _, v := range diff {
		if normalizeGuideName(v[0]) == normalizeGuideName(v[1]) {
			t.Errorf("%q matched %q", v[0], v[1])
		}
	}
}

func TestAutoRefreshDueAndErrorRetention(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	var hits atomic.Int32
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/list.m3u":
			w.Write([]byte(m3uFixture("one=One")))
		case "/guide.xml":
			w.Write([]byte(xmltvFixture(30, "one=One")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := testService(t, Options{Now: clock.Now, HTTPClient: srv.Client()})
	if _, err := s.ImportSource(ctx, Source{ID: "url", Name: "URL source", PlaylistURL: srv.URL + "/list.m3u", GuideURL: srv.URL + "/guide.xml"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "manual", Name: "Manual", GuideURL: srv.URL + "/guide.xml", RefreshHours: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "upload", Name: "Upload", Guide: xmltvFixture(2, "x=X")}); err != nil {
		t.Fatal(err)
	}
	var raw string
	s.o.DB.QueryRow(`SELECT data FROM livetv_sources WHERE id='url'`).Scan(&raw)
	if strings.Contains(raw, "#EXTM3U") || strings.Contains(raw, "<tv") {
		t.Fatalf("fetched content persisted: %s", raw)
	}
	if src := sourceByID(t, s, "url"); src.NextRefresh == nil || !src.NextRefresh.Equal(guideTestNow.Add(6*time.Hour)) {
		t.Fatalf("next refresh: %+v", src.NextRefresh)
	}
	base := hits.Load()
	clock.Set(guideTestNow.Add(5 * time.Hour))
	if due := s.refreshDueSources(ctx); len(due) != 0 || hits.Load() != base {
		t.Fatalf("refreshed early: %v", due)
	}
	clock.Set(guideTestNow.Add(6 * time.Hour))
	if due := s.refreshDueSources(ctx); len(due) != 1 || due[0] != "url" || hits.Load() != base+2 {
		t.Fatalf("due refresh: %v hits=%d", due, hits.Load()-base)
	}
	if src := sourceByID(t, s, "url"); !src.UpdatedAt.Equal(guideTestNow.Add(6 * time.Hour)) {
		t.Fatalf("refresh did not update: %+v", src)
	}

	failing.Store(true)
	failedAt := guideTestNow.Add(12 * time.Hour)
	clock.Set(failedAt)
	if due := s.refreshDueSources(ctx); len(due) != 1 {
		t.Fatalf("failed refresh not attempted: %v", due)
	}
	src := sourceByID(t, s, "url")
	if src.LastError == "" || src.LastErrorAt == nil || !src.LastErrorAt.Equal(failedAt) || src.ChannelCount != 1 || src.ProgrammeCount != 30 {
		t.Fatalf("failure handling: %+v", src)
	}
	if src.RefreshFailures != 1 || src.NextRefresh == nil || !src.NextRefresh.Equal(failedAt.Add(30*time.Minute)) {
		t.Fatalf("first backoff: %d %v", src.RefreshFailures, src.NextRefresh)
	}
	cs, _ := s.Channels(ctx)
	if len(cs) != 1 {
		t.Fatalf("failed refresh wiped channels: %+v", cs)
	}
	clock.Set(failedAt.Add(29 * time.Minute))
	if due := s.refreshDueSources(ctx); len(due) != 0 {
		t.Fatalf("retried too soon: %v", due)
	}
	secondFail := failedAt.Add(30 * time.Minute)
	clock.Set(secondFail)
	if due := s.refreshDueSources(ctx); len(due) != 1 {
		t.Fatalf("first retry not attempted: %v", due)
	}
	if src = sourceByID(t, s, "url"); src.RefreshFailures != 2 || !src.NextRefresh.Equal(secondFail.Add(time.Hour)) {
		t.Fatalf("second backoff: %d %v", src.RefreshFailures, src.NextRefresh)
	}
	clock.Set(secondFail.Add(59 * time.Minute))
	if due := s.refreshDueSources(ctx); len(due) != 0 {
		t.Fatalf("backoff ignored: %v", due)
	}
	failing.Store(false)
	clock.Set(secondFail.Add(time.Hour))
	if due := s.refreshDueSources(ctx); len(due) != 1 {
		t.Fatalf("retry not attempted: %v", due)
	}
	if src = sourceByID(t, s, "url"); src.LastError != "" || src.LastErrorAt != nil || src.RefreshFailures != 0 {
		t.Fatalf("success did not clear error: %+v", src)
	}
	// Manual-only and inline sources never auto refresh.
	clock.Set(guideTestNow.Add(1000 * time.Hour))
	if due := s.refreshDueSources(ctx); len(due) != 1 || due[0] != "url" {
		t.Fatalf("manual or upload source refreshed: %v", due)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGzipGuideURLAndDecompressedCap(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	guide := gzipBytes(t, xmltvFixture(5, "zipped=Zipped Channel"))
	bomb := gzipBytes(t, strings.Repeat("<", 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Encoding header: detection must use the magic bytes.
		w.Header().Set("Content-Type", "application/octet-stream")
		switch r.URL.Path {
		case "/epg.xml.gz":
			w.Write(guide)
		case "/bomb":
			w.Write(bomb)
		}
	}))
	defer srv.Close()
	s := testService(t, Options{Now: clock.Now, HTTPClient: srv.Client()})
	src, err := s.ImportSource(ctx, Source{ID: "gz", Name: "Gzip EPG", GuideURL: srv.URL + "/epg.xml.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if src.ProgrammeCount != 5 || src.GuideChannelCount != 1 {
		t.Fatalf("gzip guide import: %+v", src)
	}
	body, err := s.readURL(ctx, srv.URL+"/bomb", 1024)
	if err == nil || !strings.Contains(err.Error(), "decompression") || body != "" {
		t.Fatalf("decompressed cap not enforced: %v (%d bytes)", err, len(body))
	}
	if len(bomb) > 1024 {
		t.Fatal("fixture must be under the compressed cap")
	}
	if _, err = gunzipBounded([]byte{0x1f, 0x8b, 0, 0}, 1024); err == nil {
		t.Fatal("corrupt gzip accepted")
	}
	if out, err := gunzipBounded([]byte("<tv/>"), 1024); err != nil || out != "<tv/>" {
		t.Fatalf("plain body altered: %q %v", out, err)
	}
}

// Data imported before guide channels were persisted keeps working: the guide
// channel list falls back to programme channel ids until the next refresh.
func TestLegacyGuideDataWithoutGuideChannels(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// A v1-only livetv schema with live data, as on an existing server.
	legacy := []string{
		`CREATE TABLE schema_migrations (component TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (component, version))`,
		`INSERT INTO schema_migrations VALUES ('livetv', 1)`,
		`CREATE TABLE livetv_channels(id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE livetv_sources(id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE livetv_programmes(source_id TEXT NOT NULL, channel_id TEXT NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source_id,channel_id,start_ms))`,
		`CREATE INDEX livetv_programmes_window ON livetv_programmes(channel_id,start_ms,end_ms)`,
		`CREATE TABLE livetv_policies(user_id INTEGER PRIMARY KEY, data TEXT NOT NULL)`,
		`INSERT INTO livetv_sources VALUES ('legacy', '{"id":"legacy","name":"Legacy","playlist":"#EXTM3U\n","updatedAt":"2026-10-19T00:00:00Z"}')`,
		`INSERT INTO livetv_channels VALUES ('c1', '{"id":"c1","number":"1","name":"Legacy One","kind":"iptv","enabled":true,"sourceId":"legacy","guideId":"legacy.one","streamUrl":"https://owner.invalid/1.m3u8","epoch":"1970-01-01T00:00:00Z","seed":0,"shuffle":false,"items":null}')`,
		`INSERT INTO livetv_channels VALUES ('c2', '{"id":"c2","number":"2","name":"Legacy Two","kind":"iptv","enabled":false,"sourceId":"legacy","streamUrl":"https://owner.invalid/2.m3u8","epoch":"1970-01-01T00:00:00Z","seed":0,"shuffle":false,"items":null}')`,
	}
	for _, id := range []string{"legacy.one", "legacy.two"} {
		for h := 0; h < 3; h++ {
			start := guideTestNow.Add(time.Duration(h) * time.Hour)
			p, _ := json.Marshal(Programme{ID: id, ChannelID: id, Start: start, End: start.Add(time.Hour), Item: Item{Title: id + " show", DurationMS: 3600000}})
			legacy = append(legacy, fmt.Sprintf(`INSERT INTO livetv_programmes VALUES ('legacy', '%s', %d, %d, '%s')`, id, start.UnixMilli(), start.Add(time.Hour).UnixMilli(), p))
		}
	}
	for _, q := range legacy {
		if _, err = db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{DB: db, Now: clock.Now})
	c1, err := s.Channel(ctx, "c1")
	if err != nil || c1.GuideSourceID != "" {
		t.Fatalf("legacy channel: %v %+v", err, c1)
	}
	if ps, _ := s.programmes(ctx, c1, guideTestNow, guideTestNow.Add(time.Hour)); len(ps) != 1 {
		t.Fatalf("legacy guide lost: %+v", ps)
	}
	var found []GuideChannel
	ownerJSON(t, s, "GET", "/guide/channels?q=legacy", "", &found)
	if len(found) != 2 || found[0].GuideID != "legacy.one" || found[0].Name != "legacy.one" || found[0].Programmes != 3 || found[0].SourceName != "Legacy" {
		t.Fatalf("fallback guide channels: %+v", found)
	}
	if src := sourceByID(t, s, "legacy"); src.GuideChannelCount != 2 || src.ChannelCount != 2 || src.ProgrammeCount != 6 {
		t.Fatalf("fallback stats: %+v", src)
	}
	// The disabled channel, listed by id, matches the fallback by
	// case-insensitive id.
	c2, _ := s.Channel(ctx, "c2")
	c2.Name = "Legacy Two"
	c2.GuideID = "LEGACY.TWO"
	if _, err = s.SaveChannel(ctx, c2); err != nil {
		t.Fatal(err)
	}
	var res GuideMatchResult
	ownerJSON(t, s, "POST", "/guide/match", `{"channelIds":["c1","c2"]}`, &res)
	if res.Matched != 1 || res.Skipped != 1 || len(res.Changes) != 1 || res.Changes[0].GuideID != "legacy.two" || res.Changes[0].GuideSourceID != "legacy" {
		t.Fatalf("fallback match: %+v", res)
	}
	cov, _ := s.Coverage(ctx)
	for _, v := range cov {
		if v.Programmes != 3 {
			t.Fatalf("legacy coverage: %+v", cov)
		}
	}
}

func TestAdaptiveRefreshFollowsGuideHorizon(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Listings start an hour ago and end 3h from guideTestNow; "long"
		// is unmapped, so its own span is irrelevant.
		w.Write([]byte(xmltvFixture(4, "short=Short", "long=Long")))
	}))
	defer srv.Close()
	s := testService(t, Options{Now: clock.Now, HTTPClient: srv.Client()})
	if _, err := s.ImportSource(ctx, Source{ID: "own", Name: "Playlist", Playlist: m3uFixture("short=Short Channel")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "epg", Name: "Short EPG", GuideURL: srv.URL + "/epg.xml.gz"}); err != nil {
		t.Fatal(err)
	}
	// Unmapped guide channels do not drive the schedule.
	if src := sourceByID(t, s, "epg"); !src.NextRefresh.Equal(guideTestNow.Add(6 * time.Hour)) {
		t.Fatalf("unmapped schedule: %v", src.NextRefresh)
	}
	var res GuideMatchResult
	ownerJSON(t, s, "POST", "/guide/match", `{}`, &res)
	if len(res.Changes) != 1 || res.Changes[0].GuideSourceID != "epg" {
		t.Fatalf("match: %+v", res)
	}
	// The mapped channel's listings end at +3h: refresh an hour before.
	if src := sourceByID(t, s, "epg"); !src.NextRefresh.Equal(guideTestNow.Add(2 * time.Hour)) {
		t.Fatalf("horizon schedule: %v", src.NextRefresh)
	}
	clock.Set(guideTestNow.Add(2*time.Hour - time.Minute))
	if due := s.refreshDueSources(ctx); len(due) != 0 {
		t.Fatalf("refreshed before horizon: %v", due)
	}
	// The upstream has not published more: an hour of listings is left, so
	// the horizon is now, but refreshes never come more often than every 30
	// minutes.
	refreshed := guideTestNow.Add(2 * time.Hour)
	clock.Set(refreshed)
	if due := s.refreshDueSources(ctx); len(due) != 1 || due[0] != "epg" {
		t.Fatalf("horizon refresh not due: %v", due)
	}
	if src := sourceByID(t, s, "epg"); !src.NextRefresh.Equal(refreshed.Add(30 * time.Minute)) {
		t.Fatalf("minimum interval: %v", src.NextRefresh)
	}
}

func TestNextRefreshRules(t *testing.T) {
	at := guideTestNow
	url := Source{GuideURL: "https://owner.invalid/epg.xml", UpdatedAt: at}
	if _, ok := nextRefresh(Source{Guide: "<tv/>", UpdatedAt: at}, nil); ok {
		t.Fatal("inline upload scheduled")
	}
	manual := url
	manual.RefreshHours = -1
	if _, ok := nextRefresh(manual, nil); ok {
		t.Fatal("manual source scheduled")
	}
	custom := url
	custom.RefreshHours = 2
	if n, _ := nextRefresh(custom, nil); !n.Equal(at.Add(2 * time.Hour)) {
		t.Fatalf("custom interval: %v", n)
	}
	far := at.Add(48 * time.Hour)
	if n, _ := nextRefresh(url, &far); !n.Equal(at.Add(6 * time.Hour)) {
		t.Fatalf("far horizon: %v", n)
	}
	playlistOnly := Source{PlaylistURL: "https://owner.invalid/list.m3u", UpdatedAt: at}
	near := at.Add(90 * time.Minute)
	if n, _ := nextRefresh(playlistOnly, &near); !n.Equal(at.Add(6 * time.Hour)) {
		t.Fatalf("horizon applied without a guide URL: %v", n)
	}
	failedAt := at.Add(time.Hour)
	for failures, want := range map[int]time.Duration{1: 30 * time.Minute, 2: time.Hour, 3: 2 * time.Hour, 4: 4 * time.Hour, 5: 6 * time.Hour, 9: 6 * time.Hour, 100: 6 * time.Hour} {
		failed := url
		failed.LastErrorAt, failed.RefreshFailures = &failedAt, failures
		if n, _ := nextRefresh(failed, &near); !n.Equal(failedAt.Add(want)) {
			t.Fatalf("backoff after %d failures: %v want +%v", failures, n.Sub(failedAt), want)
		}
	}
}

func TestMatchGuidesLeavesDisabledChannelsUnlessListed(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	if _, err := s.ImportSource(ctx, Source{ID: "a-own", Name: "Own", Playlist: m3uFixture("=News Network", "=Sports Central")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "b-epg", Name: "EPG", Guide: xmltvFixture(30, "news=News Network", "sports=Sports Central")}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	var retired Channel
	for _, c := range cs {
		if c.Name == "Sports Central" {
			c.Enabled = false
			if _, err := s.SaveChannel(ctx, c); err != nil {
				t.Fatal(err)
			}
			retired = c
		}
	}
	var dry GuideMatchResult
	if ownerJSON(t, s, "POST", "/guide/match", `{"dryRun":true}`, &dry) != 200 {
		t.Fatal("dry run failed")
	}
	if dry.Matched != 1 || len(dry.Changes) != 1 || dry.Changes[0].ChannelName != "News Network" {
		t.Fatalf("disabled channel should be left out: %+v", dry)
	}
	var listed GuideMatchResult
	if ownerJSON(t, s, "POST", "/guide/match", `{"dryRun":true,"channelIds":["`+retired.ID+`"]}`, &listed) != 200 {
		t.Fatal("listed dry run failed")
	}
	if listed.Matched != 1 || listed.Changes[0].ChannelID != retired.ID {
		t.Fatalf("a listed disabled channel should still match: %+v", listed)
	}
}
