package resolver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func preferRelease(t *testing.T, svc *Service, id string, fallback bool) Pin {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	rel, ok := svc.LookupRelease(id)
	if !ok {
		t.Fatalf("release %q not cached", id)
	}
	p, err := svc.PinWithFallback(ctx, matrixTitle().Key, "", rel, "", fallback)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A healthy preferred release plays exactly like a strict pin: it is the
// Auto winner and keeps the pin's profile exemptions (a 4K preference on a
// 1080p profile).
func TestPreferredPinPlaysWhenUsable(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	for _, prefer := range []string{"", (versionSlot{Kind: "auto"}).id()} {
		res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: prefer})
		if err != nil || res.Release.ID != "sd720" || res.PreferenceFailed != "" || len(res.Alternates) != 0 {
			t.Fatalf("preference with selector %q: %+v %v", prefer, res, err)
		}
		if prefer != "" && res.VersionSelector != prefer {
			t.Fatalf("lost Auto selector: %+v", res)
		}
		if tr := svc.Traces()[0]; tr.Pinned != "sd720" || tr.PinStatus != "used" {
			t.Fatalf("trace pin %q %q", tr.Pinned, tr.PinStatus)
		}
	}
	preferRelease(t, svc, "uhd", true)
	caps := webCaps()
	caps.VideoCodecs = append(caps.VideoCodecs, "hevc")
	caps.DolbyVision = true
	if res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: caps}); err != nil || res.Release.ID != "uhd" {
		t.Fatalf("4K preference on a 1080p profile: %+v %v", res, err)
	}
	// Downloads use their own profile and ignore the preference.
	dl := webCaps()
	dl.Download = true
	if res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: dl}); err != nil || res.Release.ID == "uhd" {
		t.Fatalf("download used the preference: %+v %v", res, err)
	}
}

// An unhealthy preferred release records why on the pin and normal ranking
// takes over in the same request; a strict pin in the same state stops.
func TestPreferredPinFallsBackWhenUnhealthy(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, db, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" || res.PreferenceFailed != "sd720" {
		t.Fatalf("fallback: %+v %v", res, err)
	}
	tr := svc.Traces()[0]
	if tr.Pinned != "" || !strings.HasPrefix(tr.PinStatus, "fell back: unhealthy") || tr.Winner != "hd" || tr.Err != "" {
		t.Fatalf("fallback trace %+v", tr)
	}
	seen := 0
	for _, c := range tr.Candidates {
		if c.ReleaseID == "sd720" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("preference traced %d times: %+v", seen, tr.Candidates)
	}
	for _, a := range res.Alternates {
		if a.ID == "sd720" {
			t.Fatalf("failed preference returned as an alternate: %+v", res.Alternates)
		}
	}
	p, _ := svc.PinFor(matrixTitle().Key)
	if !p.Fallback || p.LastStatus != PinUnhealthy || p.LastReason == "" {
		t.Fatalf("pin outcome %+v", p)
	}
	// The pin is kept and persisted with its mode.
	if err := svc.flushBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, db, testCfg(), Options{})
	defer reloaded.Close()
	if p, ok := reloaded.PinFor(matrixTitle().Key); !ok || !p.Fallback || p.LastStatus != PinUnhealthy {
		t.Fatalf("persisted preference %+v ok=%v", p, ok)
	}

	strict := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, nil, testCfg(), Options{})
	defer strict.Close()
	preferRelease(t, strict, "sd720", false)
	if res, err := strict.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) || res != nil {
		t.Fatalf("strict pin fell back: %+v %v", res, err)
	}
}

// Only a strict pin may start on an inconclusive health sample: a preference
// has an alternative, so a timed-out sample falls back like a failure.
func TestPreferredPinDoesNotKeepUnverifiedSample(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: movieReleases()}, timeoutHealth(map[string]error{"sd720": context.DeadlineExceeded}), nil, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID == "sd720" || res.PreferenceFailed != "sd720" {
		t.Fatalf("unverified preference: %+v %v", res, err)
	}
}

// A preference that a hard gate rejects (a passworded torrent) falls back too.
func TestPreferredPinFallsBackWhenRejected(t *testing.T) {
	ctx := context.Background()
	releases := movieReleases()
	pw := rel("pwd", "The.Signal.1999.1080p.BluRay.x264-PWD", 9000)
	pw.Password, pw.Protocol = true, core.ProtocolTorrent
	svc := New(&fakeIndexer{releases: append(releases, pw)}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "pwd", true)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" || res.PreferenceFailed != "pwd" {
		t.Fatalf("rejected preference: %+v %v", res, err)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinRejected || p.LastReason != "passworded" {
		t.Fatalf("pin outcome %+v", p)
	}
}

// A failover that already tried the preference skips it without a second
// outcome, and normal ranking continues.
func TestPreferredPinSkippedOnceTried(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Exclude: []string{"sd720"}})
	if err != nil || res.Release.ID != "hd" || res.PreferenceFailed != "sd720" {
		t.Fatalf("failover: %+v %v", res, err)
	}
	if tr := svc.Traces()[0]; tr.PinStatus != "skipped: preferred release already tried" {
		t.Fatalf("trace %+v", tr)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != "" {
		t.Fatalf("skip recorded an outcome: %+v", p)
	}
}

// The preference is retried on the next ordinary Auto once the existing
// failed-health memory expires; the last status is never a latch.
func TestPreferredPinRetriedAfterFailureMemory(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	health := &switchHealth{unhealthy: map[string]bool{"sd720": true}}
	svc := New(&fakeIndexer{releases: movieReleases()}, health, nil, testCfg(), Options{Now: clock.Now})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	if res, err := svc.Resolve(ctx, req); err != nil || res.Release.ID != "hd" {
		t.Fatalf("first: %+v %v", res, err)
	}
	if id, eligible := svc.PreferredRelease(req); id != "sd720" || eligible {
		t.Fatalf("preference inside failure memory: %q eligible=%v", id, eligible)
	}
	health.set("sd720", false)
	clock.Add(failedHealthTTL + time.Second)
	if id, eligible := svc.PreferredRelease(req); id != "sd720" || !eligible {
		t.Fatalf("preference after failure memory: %q eligible=%v", id, eligible)
	}
	res, err := svc.Resolve(ctx, req)
	if err != nil || res.Release.ID != "sd720" || res.PreferenceFailed != "" {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinHealthy {
		t.Fatalf("pin after retry %+v", p)
	}
}

type switchHealth struct {
	mu        sync.Mutex
	unhealthy map[string]bool
}

func (h *switchHealth) set(id string, bad bool) {
	h.mu.Lock()
	h.unhealthy[id] = bad
	h.mu.Unlock()
}

func (h *switchHealth) Check(ctx context.Context, nzb []byte) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, bad := range h.unhealthy {
		if bad && strings.Contains(string(nzb), id) {
			return false, nil
		}
	}
	return true, nil
}

// An explicit version pick bypasses a preference, including a pick of the
// same release; a strict pin keeps its own path for that pick.
func TestExplicitPickBypassesPreference(t *testing.T) {
	ctx := context.Background()
	for _, fallback := range []bool{true, false} {
		svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{})
		preferRelease(t, svc, "sd720", fallback)
		res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "sd720"})
		if err != nil || res.Release.ID != "sd720" {
			t.Fatalf("fallback=%v same-release pick: %+v %v", fallback, res, err)
		}
		tr := svc.Traces()[0]
		if fallback && (tr.Pinned != "" || tr.PinStatus != "skipped: viewer picked a version") {
			t.Fatalf("preference handled a same-release pick: %+v", tr)
		}
		if !fallback && (tr.Pinned != "sd720" || tr.PinStatus != "used") {
			t.Fatalf("strict same-release pick changed: %+v", tr)
		}
		if p, _ := svc.PinFor(matrixTitle().Key); fallback && p.LastStatus != "" {
			t.Fatalf("explicit pick recorded a preference outcome: %+v", p)
		}
		res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "hd"})
		if err != nil || res.Release.ID != "hd" {
			t.Fatalf("fallback=%v other pick: %+v %v", fallback, res, err)
		}
		svc.Close()
	}
}

// Viewer remembered choices (title and show) keep precedence over a preference.
func TestViewerChoiceBeatsPreference(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, openTestDB(t), testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	viewer := &core.User{ID: 7, Username: "carol"}
	hd, _ := svc.LookupRelease("hd")
	if err := svc.RecordViewerChoice(ctx, viewer.ID, matrixTitle(), "", hd, nil); err != nil {
		t.Fatal(err)
	}
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), User: viewer}
	res, err := svc.Resolve(ctx, req)
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("viewer choice: %+v %v", res, err)
	}
	if id, _ := svc.PreferredRelease(req); id != "" {
		t.Fatalf("preference reported over a viewer choice: %q", id)
	}
	other := core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), User: &core.User{ID: 8}}
	if res, err := svc.Resolve(ctx, other); err != nil || res.Release.ID != "sd720" {
		t.Fatalf("other viewer: %+v %v", res, err)
	}
}

// In-flight resolves are keyed by the applicable pinned release and mode, so
// a request for preference B never joins a resolve that tries A.
func TestPinFlightKeyCarriesReleaseAndMode(t *testing.T) {
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	keys := map[string]string{"none": svc.pinFlightKey(req)}
	preferRelease(t, svc, "sd720", false)
	keys["strict A"] = svc.pinFlightKey(req)
	preferRelease(t, svc, "sd720", true)
	keys["prefer A"] = svc.pinFlightKey(req)
	preferRelease(t, svc, "hd", true)
	keys["prefer B"] = svc.pinFlightKey(req)
	seen := map[string]string{}
	for name, k := range keys {
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s and %s share flight key %q", name, prev, k)
		}
		seen[k] = name
	}
	if keys["strict A"] != "sd720" {
		t.Fatalf("strict flight key changed: %q", keys["strict A"])
	}
}

// Menus mark a preference only as the eligible Auto winner. One inside its
// failure memory is labelled like any other release and is not Auto.
func TestPreferredPinLabels(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{Now: clock.Now})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	vs, err := svc.Variants(ctx, req, 4)
	if err != nil || len(vs) == 0 || vs[0].ID != "sd720" || !vs[0].Pinned {
		t.Fatalf("eligible preference variants %+v %v", vs, err)
	}
	slots, err := svc.VersionSlots(ctx, req, 4, true)
	if err != nil || len(slots) == 0 || slots[0].Release.ID != "sd720" || !slots[0].Pinned {
		t.Fatalf("eligible preference slots %+v %v", slots, err)
	}
	svc.noteFailedHealth("sd720")
	vs, err = svc.Variants(ctx, req, 4)
	if err != nil || len(vs) == 0 || vs[0].ID == "sd720" {
		t.Fatalf("failed preference stayed Auto: %+v %v", vs, err)
	}
	for _, v := range vs {
		if v.Pinned {
			t.Fatalf("failed preference badged: %+v", v)
		}
	}
	slots, err = svc.VersionSlots(ctx, req, 4, true)
	if err != nil || len(slots) == 0 || slots[0].Release.ID == "sd720" {
		t.Fatalf("failed preference stayed in the Auto slot: %+v %v", slots, err)
	}
	for _, v := range slots {
		if v.Pinned {
			t.Fatalf("failed preference slot badged: %+v", v)
		}
	}
}

// Strict pins remain the remembered-source identity decision; a preference is
// rechecked like any other remembered source.
func TestRememberedIdentityExemptionIsStrictOnly(t *testing.T) {
	ctx := context.Background()
	namesake := rel("other", "Different.Film.2004.1080p.WEB-DL.H.264-GRP", 8000)
	for _, fallback := range []bool{false, true} {
		svc := New(&fakeIndexer{releases: append(movieReleases(), namesake)}, fakeHealth{}, nil, testCfg(), Options{})
		if _, err := svc.PinWithFallback(ctx, matrixTitle().Key, "", namesake, "", fallback); err != nil {
			t.Fatal(err)
		}
		if got := svc.RememberedReleaseAllowed(ctx, matrixTitle(), namesake); got == fallback {
			t.Fatalf("fallback=%v remembered namesake allowed=%v", fallback, got)
		}
		svc.Close()
	}
}

// Old rows (no fallback column value) and callers that do not pass the mode
// stay strict; a title rekey carries the mode.
func TestPinModeMigrationAndRekey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO resolver_pins(rating_key, release_id, release_json, title_name, pinned_at) VALUES (?,?,?,?,?)`,
		int64(core.MovieKey(604)), "old", `{"id":"old","name":"Old.Release.1080p"}`, "Old", 1); err != nil {
		t.Fatal(err)
	}
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, db, testCfg(), Options{})
	defer svc.Close()
	if p, ok := svc.PinFor(core.MovieKey(604)); !ok || p.Fallback {
		t.Fatalf("old row %+v ok=%v", p, ok)
	}
	hd := rel("hd", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)
	if p, err := svc.Pin(ctx, matrixTitle().Key, "", hd, ""); err != nil || p.Fallback {
		t.Fatalf("Pin without a mode: %+v %v", p, err)
	}
	if _, err := svc.PinWithFallback(ctx, matrixTitle().Key, "", hd, "", true); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	from := int64(matrixTitle().Key)
	keys, err := svc.RekeyTitle(ctx, tx, []core.TitleKeyRange{{First: from, Last: from, Delta: 10}}, "The Signal")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	svc.TitlePinsRebound(ctx, keys)
	if p, ok := svc.PinFor(core.RatingKey(from + 10)); !ok || !p.Fallback || p.Release.ID != "hd" {
		t.Fatalf("rekeyed preference %+v ok=%v", p, ok)
	}
	if _, ok := svc.PinFor(matrixTitle().Key); ok {
		t.Fatal("rekey left the old pin")
	}
}

// A viewer's show-level episode choice keeps precedence over a preference that
// does not match it; without a matching candidate the preference is next.
func TestShowChoiceBeatsPreference(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg()
	cfg.Resolver.OfferHigherVersions = true
	ep1, ep2 := testEpisodeTitle(95396, 1, 1), testEpisodeTitle(95396, 1, 2)
	episode := func(id, name string, height int, source core.Source, codec string, gb int64) core.Release {
		return core.Release{ID: id, Indexer: "idx", Protocol: core.ProtocolUsenet, Size: gb << 30, Name: name,
			Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{2}, Height: height, Source: source, VideoCodec: codec, AudioCodec: "eac3"}}
	}
	web := episode("ep2-1080p-webdl", "Severance.S01E02.1080p.WEB-DL.DDP5.1.H.264-GRP", 1080, core.SourceWEBDL, "h264", 4)
	remux := episode("ep2-1080p-remux", "Severance.S01E02.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-GRP", 1080, core.SourceRemux, "h264", 14)
	idx := &episodeChoiceIndexer{byEpisode: map[int][]core.Release{2: {web, remux}}}
	svc := New(idx, fakeHealth{}, openTestDB(t), cfg, Options{})
	defer svc.Close()
	if _, err := svc.PinWithFallback(ctx, ep2.Key, "", web, "", true); err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 7, Username: "mark"}
	picked := core.Release{ID: "ep1-1080p-remux", Indexer: "idx", Protocol: core.ProtocolUsenet, Size: 13 << 30,
		Name: "Severance.S01E01.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-GRP",
		Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{1}, Height: 1080, Source: core.SourceRemux, VideoCodec: "h264", AudioCodec: "dtshd"}}
	if err := svc.RecordViewerChoice(ctx, viewer.ID, ep1, "slot:alt1", picked, nil); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: ep2, User: viewer, Client: appleTVCaps()})
	if err != nil || res.Release.ID != "ep2-1080p-remux" {
		t.Fatalf("show choice: %+v %v", res, err)
	}
	if p, _ := svc.PinFor(ep2.Key); p.LastStatus != "" {
		t.Fatalf("show choice still tried the preference: %+v", p)
	}
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: ep2, User: &core.User{ID: 8}, Client: appleTVCaps()})
	if err != nil || res.Release.ID != "ep2-1080p-webdl" || svc.Traces()[0].PinStatus != "used" {
		t.Fatalf("viewer without a show choice: %+v %v", res, err)
	}
}

// The mode round-trips through single and batch saves and every listing; an
// omitted field keeps today's strict pin.
func TestConsoleHTTPPinMode(t *testing.T) {
	f := newConsoleFixture(t)
	key := matrixTitle().Key.String()
	if code := f.do(t, "GET", "/search?key="+key, "", nil); code != 200 {
		t.Fatalf("search status %d", code)
	}
	var pin Pin
	if code := f.do(t, "POST", "/pins", `{"key":"`+key+`","releaseId":"hd"}`, &pin); code != 200 || pin.Fallback {
		t.Fatalf("strict pin status %d %+v", code, pin)
	}
	if code := f.do(t, "POST", "/pins", `{"key":"`+key+`","releaseId":"hd","fallback":true}`, &pin); code != 200 || !pin.Fallback {
		t.Fatalf("preferred pin status %d %+v", code, pin)
	}
	var search ConsoleSearch
	if code := f.do(t, "GET", "/search?key="+key, "", &search); code != 200 || search.Pin == nil || !search.Pin.Fallback {
		t.Fatalf("search pin %d %+v", code, search.Pin)
	}
	other := core.MovieKey(604).String()
	var batch struct {
		Pins []Pin `json:"pins"`
	}
	body := `{"pins":[{"key":"` + key + `","releaseId":"hd"},{"key":"` + other + `","releaseId":"uhd"}],"note":"n","fallback":true}`
	if code := f.do(t, "POST", "/pins/batch", body, &batch); code != 200 || len(batch.Pins) != 2 || !batch.Pins[0].Fallback || !batch.Pins[1].Fallback {
		t.Fatalf("batch status %d %+v", code, batch.Pins)
	}
	body = `{"pins":[{"key":"` + other + `","releaseId":"uhd"}]}`
	if code := f.do(t, "POST", "/pins/batch", body, &batch); code != 200 || len(batch.Pins) != 1 || batch.Pins[0].Fallback {
		t.Fatalf("strict batch status %d %+v", code, batch.Pins)
	}
	var list struct {
		Pins []Pin `json:"pins"`
	}
	if code := f.do(t, "GET", "/pins", "", &list); code != 200 || len(list.Pins) != 2 {
		t.Fatalf("list status %d %+v", code, list.Pins)
	}
	for _, p := range list.Pins {
		if want := p.TitleKey == matrixTitle().Key; p.Fallback != want {
			t.Fatalf("listed pin mode %+v", p)
		}
	}
	ep := core.EpisodeKey(77, 1, 2)
	if _, err := f.svc.PinWithFallback(context.Background(), ep, "", rel("e2", "Show.S01E02.1080p.WEB-DL", 2000), "", true); err != nil {
		t.Fatal(err)
	}
	var kids struct {
		Items []childTitle `json:"items"`
	}
	if code := f.do(t, "GET", "/titles/"+core.SeasonKey(77, 1).String()+"/children", "", &kids); code != 200 || len(kids.Items) != 2 {
		t.Fatalf("children status %d %+v", code, kids.Items)
	}
	if kids.Items[0].Pinned || kids.Items[0].Fallback || !kids.Items[1].Pinned || !kids.Items[1].Fallback {
		t.Fatalf("children pin modes %+v", kids.Items)
	}
}

// resolver.pin.unhealthy drives "remove dead pins" flows and critical alerts.
// A preference that falls back is kept to be retried, so only a strict pin
// publishes it.
func TestPinUnhealthyEventIsStrictOnly(t *testing.T) {
	ctx := context.Background()
	bus, rec := events.NewTestBus(t)
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, nil, testCfg(), Options{Events: bus})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	if res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil || res.Release.ID != "hd" {
		t.Fatalf("preference fallback: %+v %v", res, err)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinUnhealthy {
		t.Fatalf("preference outcome not recorded: %+v", p)
	}
	strict := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, nil, testCfg(), Options{Events: bus})
	defer strict.Close()
	preferRelease(t, strict, "sd720", false)
	if _, err := strict.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil {
		t.Fatal("strict pin played while unhealthy")
	}
	// Removing the strict pin publishes after both resolves, so every
	// earlier event has been recorded once it arrives.
	if _, err := strict.Unpin(ctx, matrixTitle().Key); err != nil {
		t.Fatal(err)
	}
	rec.WaitFor(t, events.EventResolverPinRemoved, time.Second)
	if got := rec.OfType(events.EventResolverPinUnhealthy); len(got) != 1 {
		t.Fatalf("pin unhealthy events %d, want only the strict pin's", len(got))
	}
}

// Inside a preference's failure window Auto neither validates it again nor
// records a new failure, so the window is not extended; once it expires the
// preference is tried again.
func TestPreferenceFailureWindowNotRefreshed(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	health := &switchHealth{unhealthy: map[string]bool{"sd720": true}}
	svc := New(&fakeIndexer{releases: movieReleases()}, health, nil, testCfg(), Options{Now: clock.Now})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	if res, err := svc.Resolve(ctx, req); err != nil || res.Release.ID != "hd" {
		t.Fatalf("first: %+v %v", res, err)
	}
	first, _ := svc.PinFor(matrixTitle().Key)
	health.set("sd720", false)
	clock.Add(failedHealthTTL / 2)
	res, err := svc.Resolve(ctx, req)
	if err != nil || res.Release.ID != "hd" || res.PreferenceFailed != "sd720" {
		t.Fatalf("inside the window: %+v %v", res, err)
	}
	tr := svc.Traces()[0]
	if tr.PinStatus != "skipped: preferred release failed recently" {
		t.Fatalf("inside the window trace %q", tr.PinStatus)
	}
	for _, c := range tr.Candidates {
		if c.ReleaseID == "sd720" {
			t.Fatalf("preference validated inside its window: %+v", c)
		}
	}
	if p, _ := svc.PinFor(matrixTitle().Key); !p.LastCheckedAt.Equal(first.LastCheckedAt) {
		t.Fatalf("window outcome re-recorded: %v, first %v", p.LastCheckedAt, first.LastCheckedAt)
	}
	clock.Add(failedHealthTTL/2 + time.Second)
	if res, err := svc.Resolve(ctx, req); err != nil || res.Release.ID != "sd720" || res.PreferenceFailed != "" {
		t.Fatalf("after the original window: %+v %v", res, err)
	}
}

type gatedHealth struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (h *gatedHealth) Check(ctx context.Context, _ []byte) (bool, error) {
	first := false
	h.once.Do(func() { first = true; close(h.started) })
	if first {
		select {
		case <-h.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return true, nil
}

// A viewer show choice that matches no candidate leaves the pin in charge, so
// the in-flight key still carries the pin: after an owner edit from A to B an
// overlapping request resolves B instead of joining A's resolve.
func TestPinFlightKeyWithUnmatchedShowChoice(t *testing.T) {
	ctx := context.Background()
	ep1, ep2 := testEpisodeTitle(95396, 1, 1), testEpisodeTitle(95396, 1, 2)
	episode := func(id, name string, source core.Source, gb int64) core.Release {
		return core.Release{ID: id, Indexer: "idx", Protocol: core.ProtocolUsenet, Size: gb << 30, Name: name,
			Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{2}, Height: 1080, Source: source, VideoCodec: "h264", AudioCodec: "eac3"}}
	}
	web := episode("ep2-1080p-webdl", "Severance.S01E02.1080p.WEB-DL.DDP5.1.H.264-GRP", core.SourceWEBDL, 4)
	bluray := episode("ep2-1080p-bluray", "Severance.S01E02.1080p.BluRay.x264-GRP", core.SourceBluRay, 8)
	health := &gatedHealth{started: make(chan struct{}), release: make(chan struct{})}
	svc := New(&episodeChoiceIndexer{byEpisode: map[int][]core.Release{2: {web, bluray}}}, health, openTestDB(t), testCfg(), Options{})
	defer svc.Close()
	viewer := &core.User{ID: 7, Username: "mark"}
	picked := core.Release{ID: "ep1-2160p-remux", Indexer: "idx", Protocol: core.ProtocolUsenet, Size: 40 << 30,
		Name: "Severance.S01E01.2160p.BluRay.REMUX.HEVC.DV.TrueHD.7.1-GRP",
		Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{1}, Height: 2160, DolbyVision: true, Source: core.SourceRemux, VideoCodec: "hevc", AudioCodec: "truehd"}}
	if err := svc.RecordViewerChoice(ctx, viewer.ID, ep1, "slot:alt1", picked, nil); err != nil {
		t.Fatal(err)
	}
	for _, fallback := range []bool{true, false} {
		if _, err := svc.PinWithFallback(ctx, ep2.Key, "", web, "", fallback); err != nil {
			t.Fatal(err)
		}
		req := core.ResolveRequest{Title: ep2, User: viewer, Client: appleTVCaps()}
		if svc.viewerShowChoiceForResolve(req) == nil {
			t.Fatal("show choice not active")
		}
		keyA := svc.pinFlightKey(req)
		if _, err := svc.PinWithFallback(ctx, ep2.Key, "", bluray, "", fallback); err != nil {
			t.Fatal(err)
		}
		if keyB := svc.pinFlightKey(req); keyA == "" || keyA == keyB {
			t.Fatalf("fallback=%v flight keys %q and %q", fallback, keyA, keyB)
		}
	}
	if _, err := svc.PinWithFallback(ctx, ep2.Key, "", web, "", true); err != nil {
		t.Fatal(err)
	}
	req := core.ResolveRequest{Title: ep2, User: viewer, Client: appleTVCaps()}
	type result struct {
		res *core.Resolution
		err error
	}
	first := make(chan result, 1)
	go func() {
		res, err := svc.Resolve(ctx, req)
		first <- result{res, err}
	}()
	<-health.started
	if _, err := svc.PinWithFallback(ctx, ep2.Key, "", bluray, "", true); err != nil {
		t.Fatal(err)
	}
	second := make(chan result, 1)
	go func() {
		res, err := svc.Resolve(ctx, req)
		second <- result{res, err}
	}()
	var got result
	select {
	case got = <-second:
	case <-time.After(5 * time.Second):
	}
	close(health.release)
	if a := <-first; a.err != nil || a.res.Release.ID != web.ID {
		t.Fatalf("first resolve %+v %v", a.res, a.err)
	}
	if got.res == nil {
		got = <-second
	}
	if got.err != nil || got.res.Release.ID != bluray.ID {
		t.Fatalf("overlapping resolve after the edit: %+v %v", got.res, got.err)
	}
}

// A same-tier earlier Auto play does not relabel Auto while an eligible
// preference is the Auto winner.
func TestPlayedAutoDoesNotHidePreference(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: variantReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	primeSearchCache(t, svc, appleTVCaps())
	base, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil || len(base) < 2 || variantKey(base[0].Release.Info) != variantKey(base[1].Release.Info) {
		t.Fatalf("fixture variants %+v %v", base, err)
	}
	if _, err := svc.PinWithFallback(ctx, matrixTitle().Key, "", base[0].Release, "", true); err != nil {
		t.Fatal(err)
	}
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), PlayedRelease: base[1].Release.ID}, 4)
	if err != nil || len(vs) == 0 || vs[0].Release.ID != base[0].Release.ID || !vs[0].Auto || !vs[0].Pinned {
		t.Fatalf("played release replaced the preferred Auto winner %s: %v", base[0].Release.ID, err)
	}
}

// The latest trace entry for a release is its reason: the owner-pin attempt
// is traced after any earlier unpinned look at the same release.
func TestCandidateReasonLatestEntryWins(t *testing.T) {
	tr := Trace{Candidates: []CandidateTrace{
		{ReleaseID: "a", Reason: "above profile max resolution"},
		{ReleaseID: "b", Reason: "other"},
		{ReleaseID: "a", HealthChecked: true},
	}}
	if got := candidateReason(&tr, "a"); got != "health: unhealthy" {
		t.Fatalf("reason %q", got)
	}
}

// A passing manual Check makes a failed preference usable on the next Play:
// it drops the negative Usenet verdict (a release with an NZB URL, which
// validation reads before fetching) and the persisted failed-grab rows, so
// neither the next Auto nor a restart inside the window suppresses it.
func TestPassingCheckClearsPreferenceFailure(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	releases := movieReleases()
	for i := range releases {
		releases[i].NZBURL = "https://indexer.example/get/" + releases[i].ID
		releases[i].Protocol, releases[i].Poster = core.ProtocolUsenet, "poster@example.invalid"
	}
	health := &switchHealth{unhealthy: map[string]bool{"sd720": true}}
	svc := New(&fakeIndexer{releases: releases}, health, db, testCfg(), Options{})
	defer svc.Close()
	preferRelease(t, svc, "sd720", true)
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	if res, err := svc.Resolve(ctx, req); err != nil || res.Release.ID != "hd" || res.PreferenceFailed != "sd720" {
		t.Fatalf("first: %+v %v", res, err)
	}
	if err := svc.flushBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resolver_failed_grabs`).Scan(&rows); err != nil || rows == 0 {
		t.Fatalf("fixture: no failed-grab rows after the failure: %d %v", rows, err)
	}
	health.set("sd720", false)
	if hr, err := svc.CheckRelease(ctx, "sd720", matrixTitle().Key); err != nil || !hr.Healthy {
		t.Fatalf("check: %+v %v", hr, err)
	}
	if err := svc.flushBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resolver_failed_grabs`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed-grab rows after a passing check: %d %v", rows, err)
	}
	restarted := New(&fakeIndexer{releases: releases}, health, db, testCfg(), Options{})
	defer restarted.Close()
	if id, eligible := restarted.PreferredRelease(req); id != "sd720" || !eligible {
		t.Fatalf("preference after restart: %q eligible=%v", id, eligible)
	}
	res, err := svc.Resolve(ctx, req)
	if err != nil || res.Release.ID != "sd720" || res.PreferenceFailed != "" {
		t.Fatalf("next Auto after a passing check: %+v %v", res, err)
	}
	if svc.recentlyFailedRelease(res.Release) {
		t.Fatal("a new failure was recorded for the checked preference")
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinHealthy {
		t.Fatalf("pin after a passing check %+v", p)
	}
}

// A viewer title choice the request excludes does not clear the pin from the
// in-flight key: two overlapping requests that exclude it across an owner
// edit resolve their own pins instead of sharing one resolve.
func TestPinFlightKeyWithExcludedViewerChoice(t *testing.T) {
	ctx := context.Background()
	health := &gatedHealth{started: make(chan struct{}), release: make(chan struct{})}
	svc := New(&fakeIndexer{releases: movieReleases()}, health, openTestDB(t), testCfg(), Options{})
	defer svc.Close()
	viewer := &core.User{ID: 7, Username: "carol"}
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	hd, _ := svc.LookupRelease("hd")
	if err := svc.RecordViewerChoice(ctx, viewer.ID, matrixTitle(), "", hd, nil); err != nil {
		t.Fatal(err)
	}
	caps := webCaps()
	caps.VideoCodecs = append(caps.VideoCodecs, "hevc")
	caps.DolbyVision = true
	req := core.ResolveRequest{Title: matrixTitle(), Client: caps, User: viewer, Exclude: []string{"hd"}}
	for _, fallback := range []bool{true, false} {
		preferRelease(t, svc, "sd720", fallback)
		keyA := svc.pinFlightKey(req)
		preferRelease(t, svc, "uhd", fallback)
		if keyB := svc.pinFlightKey(req); keyA == "" || keyA == keyB {
			t.Fatalf("fallback=%v flight keys %q and %q", fallback, keyA, keyB)
		}
	}
	preferRelease(t, svc, "sd720", true)
	type result struct {
		res *core.Resolution
		err error
	}
	first := make(chan result, 1)
	go func() {
		res, err := svc.Resolve(ctx, req)
		first <- result{res, err}
	}()
	<-health.started
	preferRelease(t, svc, "uhd", true)
	second := make(chan result, 1)
	go func() {
		res, err := svc.Resolve(ctx, req)
		second <- result{res, err}
	}()
	var got result
	select {
	case got = <-second:
	case <-time.After(5 * time.Second):
	}
	close(health.release)
	if a := <-first; a.err != nil || a.res.Release.ID != "sd720" {
		t.Fatalf("first resolve %+v %v", a.res, a.err)
	}
	if got.res == nil {
		got = <-second
	}
	if got.err != nil || got.res.Release.ID != "uhd" {
		t.Fatalf("overlapping resolve after the edit: %+v %v", got.res, got.err)
	}
}

// A viewer show choice that matches no candidate leaves the preference in
// charge; PreferredRelease reports what that resolve did, so playback's
// recovery and reuse checks see the same preference.
func TestPreferredReleaseWithUnmatchedShowChoice(t *testing.T) {
	ctx := context.Background()
	ep1, ep2 := testEpisodeTitle(95396, 1, 1), testEpisodeTitle(95396, 1, 2)
	web := core.Release{ID: "ep2-1080p-webdl", Indexer: "idx", Protocol: core.ProtocolUsenet, Size: 4 << 30, Name: "Severance.S01E02.1080p.WEB-DL.DDP5.1.H.264-GRP",
		Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{2}, Height: 1080, Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "eac3"}}
	svc := New(&episodeChoiceIndexer{byEpisode: map[int][]core.Release{2: {web}}}, fakeHealth{}, openTestDB(t), testCfg(), Options{})
	defer svc.Close()
	viewer := &core.User{ID: 7, Username: "mark"}
	picked := core.Release{ID: "ep1-2160p-remux", Indexer: "idx", Protocol: core.ProtocolUsenet, Size: 40 << 30,
		Name: "Severance.S01E01.2160p.BluRay.REMUX.HEVC.DV.TrueHD.7.1-GRP",
		Info: core.ReleaseInfo{Title: "Severance", Seasons: []int{1}, Episodes: []int{1}, Height: 2160, DolbyVision: true, Source: core.SourceRemux, VideoCodec: "hevc", AudioCodec: "truehd"}}
	if err := svc.RecordViewerChoice(ctx, viewer.ID, ep1, "slot:alt1", picked, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PinWithFallback(ctx, ep2.Key, "", web, "", true); err != nil {
		t.Fatal(err)
	}
	req := core.ResolveRequest{Title: ep2, User: viewer, Client: appleTVCaps()}
	if svc.viewerShowChoiceForResolve(req) == nil {
		t.Fatal("show choice not active")
	}
	res, err := svc.Resolve(ctx, req)
	if err != nil || res.Release.ID != web.ID || len(res.Alternates) != 0 {
		t.Fatalf("unmatched show choice: %+v %v", res, err)
	}
	if id, eligible := svc.PreferredRelease(req); id != web.ID || !eligible {
		t.Fatalf("PreferredRelease %q eligible=%v, resolve used %s", id, eligible, res.Release.ID)
	}
	if id := svc.PinnedReleaseID(req); id != "" {
		t.Fatalf("strict lookup changed: %q", id)
	}
}
