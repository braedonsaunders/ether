package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves a TRaSH Guides tree the way GitHub does: directory
// listings on the contents API, files on the raw host.
type fakeGitHub struct {
	srv   *httptest.Server
	files map[string]string // "radarr/cf/x.json" -> body

	mu        sync.Mutex
	listCalls int
	fileCalls map[string]int
	agents    map[string]bool
	limited   atomic.Bool
	down      atomic.Bool
}

func newFakeGitHub(t *testing.T, files map[string]string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{files: files, fileCalls: map[string]int{}, agents: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.agents[r.Header.Get("User-Agent")] = true
	f.mu.Unlock()
	if f.down.Load() {
		http.Error(w, "boom", http.StatusServiceUnavailable)
		return
	}
	const api = "/api/repos/TRaSH-Guides/Guides/contents/docs/json/"
	const raw = "/raw/TRaSH-Guides/Guides/master/docs/json/"
	switch {
	case strings.HasPrefix(r.URL.Path, api):
		if f.limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("ref") != "master" || r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.listCalls++
		f.mu.Unlock()
		dir := strings.TrimPrefix(r.URL.Path, api) + "/"
		type entry struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		list := []entry{{Name: "README.md", Type: "file"}, {Name: "nested", Type: "dir"}}
		for path := range f.files {
			if name, ok := strings.CutPrefix(path, dir); ok {
				list = append(list, entry{Name: name, Type: "file"})
			}
		}
		if len(list) == 2 {
			http.NotFound(w, r)
			return
		}
		slices.SortFunc(list, func(a, b entry) int { return strings.Compare(a.Name, b.Name) })
		_ = json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(r.URL.Path, raw):
		path := strings.TrimPrefix(r.URL.Path, raw)
		f.mu.Lock()
		f.fileCalls[path]++
		f.mu.Unlock()
		body, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) guides(now func() time.Time) *TrashGuides {
	return NewTrashGuides(TrashOptions{APIBase: f.srv.URL + "/api", RawBase: f.srv.URL + "/raw", UserAgent: "Ether/test", Now: now})
}

func (f *fakeGitHub) fetched() (lists, files int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.fileCalls {
		files += n
	}
	return f.listCalls, files
}

// trashTree is a small Radarr/Sonarr tree. Custom format files are named
// unlike their formats where TRaSH's are, so lookup cannot rely on names.
func trashTree() map[string]string {
	return map[string]string{
		"radarr/quality-profiles/hd-bluray-web.json": `{"trash_id":"p1","name":"HD Bluray + WEB","group":1,
			"trash_description":"Prefers <b>720p/1080p</b> Bluray &amp; WEB.<br>See the <a href=\"https://trash-guides.info/x\">guide</a>.",
			"trash_url":"https://trash-guides.info/Radarr/hd","upgradeAllowed":true,"cutoff":"Bluray-1080p","minFormatScore":0,
			"language":"Original","items":[{"name":"Bluray-1080p","allowed":true},{"name":"WEB 1080p","allowed":true,"items":["WEBRip-1080p","WEBDL-1080p"]},
			{"name":"Raw-HD","allowed":false}],"formatItems":{"BR-DISK":"cf-brdisk","WEB Tier 01":"cf-web1","Freeleech":"cf-free"}}`,
		"radarr/quality-profiles/sqp-1-2160p.json": `{"trash_id":"p2","name":"[SQP] SQP-1 (2160p)","group":98,"trash_score_set":"sqp-1-2160p",
			"trash_url":"javascript:alert(1)","minFormatScore":1000,"items":[{"name":"Bluray-2160p","allowed":true},{"name":"WEB 2160p","allowed":true,"items":["WEBRip-2160p","WEBDL-2160p"]}],
			"formatItems":{"BR-DISK":"cf-brdisk","WEB Tier 01":"cf-web1"}}`,
		"radarr/quality-profiles/broken.json": `{"oops":true}`,
		"radarr/cf/br-disk.json":              `{"trash_id":"cf-brdisk","name":"BR-DISK","trash_scores":{"default":-10000},"specifications":[{"name":"BR-DISK","implementation":"QualityModifierSpecification","negate":false,"required":true,"fields":{"value":4}}]}`,
		"radarr/cf/web-tier-1.json":           `{"trash_id":"cf-web1","name":"WEB Tier 01","trash_scores":{"default":1700,"sqp-1-2160p":1800},"specifications":[{"name":"FLUX","implementation":"ReleaseGroupSpecification","negate":false,"required":false,"fields":{"value":"^(FLUX)$"}}]}`,
		"radarr/cf/freeleech.json":            `{"trash_id":"cf-free","name":"FreeLeech","trash_scores":{"default":0},"specifications":[{"name":"FreeLeech","implementation":"IndexerFlagSpecification","negate":false,"required":false,"fields":{"value":1}}]}`,
		"radarr/cf/unrelated-1.json":          `{"trash_id":"cf-x1","name":"X1","specifications":[{"name":"x","implementation":"ReleaseTitleSpecification","fields":{"value":"x1"}}]}`,
		"radarr/cf/unrelated-2.json":          `{"trash_id":"cf-x2","name":"X2","specifications":[{"name":"x","implementation":"ReleaseTitleSpecification","fields":{"value":"x2"}}]}`,
		"sonarr/quality-profiles/web-1080p.json": `{"trash_id":"s1","name":"WEB-1080p","items":[{"name":"WEB 1080p","allowed":true,"items":["WEBRip-1080p","WEBDL-1080p"]},
			{"name":"Bluray-1080p Remux","allowed":false}],"formatItems":{"Remux Tier 01":"scf-remux1"}}`,
		"sonarr/cf/remux-tier-01.json": `{"trash_id":"scf-remux1","name":"Remux Tier 01","trash_scores":{"default":1900},"specifications":[{"name":"Remux","implementation":"SourceSpecification","negate":false,"required":true,"fields":{"value":7}}]}`,
	}
}

// trashGroupTree adds the cf-groups layout: the Base Profile carries no
// formatItems of its own, so the formats it is supposed to have all come
// from the guide's default-on groups.
func trashGroupTree() map[string]string {
	t := trashTree()
	t["radarr/quality-profiles/base-profile.json"] = `{"trash_id":"p3","name":"Base Profile","group":99,
		"items":[{"name":"WEBDL-1080p","allowed":true}],"formatItems":{}}`
	t["radarr/cf-groups/audio-formats.json"] = `{"name":"[Audio] Audio Formats","trash_id":"g1","default":"true",
		"quality_profiles":{"include":{"Base Profile":"p3","HD Bluray + WEB":"p1"}},
		"custom_formats":[{"name":"DD+ ATMOS","trash_id":"cf-grp1","required":true},
			{"name":"MP3","trash_id":"cf-grp2"},
			{"name":"TrueHD","trash_id":"cf-grp3","default":true},
			{"name":"BR-DISK","trash_id":"cf-brdisk","required":true}]}`
	t["radarr/cf-groups/release-groups.json"] = `{"name":"[Release Groups] HQ","trash_id":"g2",
		"quality_profiles":{"include":{"Base Profile":"p3"}},
		"custom_formats":[{"name":"WEB Tier 01","trash_id":"cf-web1"}]}`
	t["radarr/cf-groups/unwanted.json"] = `{"name":"[Unwanted] Unwanted Formats","trash_id":"g3","default":"true",
		"quality_profiles":{"include":{"[SQP] SQP-1 (2160p)":"p2"}},
		"custom_formats":[{"name":"X1","trash_id":"cf-x1","required":true}]}`
	t["radarr/cf/grp-1.json"] = `{"trash_id":"cf-grp1","name":"DD+ ATMOS","trash_scores":{"default":2500},"specifications":[{"name":"atmos","implementation":"ReleaseTitleSpecification","fields":{"value":"ATMOS"}}]}`
	t["radarr/cf/grp-3.json"] = `{"trash_id":"cf-grp3","name":"TrueHD","trash_scores":{"default":1750},"specifications":[{"name":"thd","implementation":"ReleaseTitleSpecification","fields":{"value":"TrueHD"}}]}`
	return t
}

func TestTrashProfilesListsSummariesSafely(t *testing.T) {
	gh := newFakeGitHub(t, trashTree())
	g := gh.guides(nil)
	list, err := g.Profiles(context.Background(), AppRadarr)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "hd-bluray-web" || list[1].ID != "sqp-1-2160p" {
		t.Fatalf("profiles (group order, broken file skipped): %+v", list)
	}
	hd := list[0]
	if hd.Name != "HD Bluray + WEB" || hd.Formats != 3 || hd.URL != "https://trash-guides.info/Radarr/hd" || hd.TrashID != "p1" {
		t.Fatalf("summary %+v", hd)
	}
	if want := "Prefers 720p/1080p Bluray & WEB.\nSee the guide."; hd.Description != want {
		t.Fatalf("description %q, want %q", hd.Description, want)
	}
	if list[1].URL != "" || list[1].ScoreSet != "sqp-1-2160p" {
		t.Fatalf("non-TRaSH link kept or score set lost: %+v", list[1])
	}
	if _, err := g.Profiles(context.Background(), AppRadarr); err != nil {
		t.Fatal(err)
	}
	if lists, files := gh.fetched(); lists != 1 || files != 3 {
		t.Fatalf("second listing should be cached: %d listings, %d files", lists, files)
	}
	if !gh.agents["Ether/test"] || len(gh.agents) != 1 {
		t.Fatalf("user agents %v", gh.agents)
	}
	if _, err := g.Profiles(context.Background(), "lidarr"); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestTrashProfileOnlyServesListedFiles(t *testing.T) {
	gh := newFakeGitHub(t, trashTree())
	g := gh.guides(nil)
	if _, err := g.Profile(context.Background(), AppRadarr, "../cf/br-disk"); !errors.Is(err, ErrTrashNotFound) {
		t.Fatalf("path escape: %v", err)
	}
	if _, err := g.Profile(context.Background(), AppRadarr, "nope"); !errors.Is(err, ErrTrashNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	data, err := g.Profile(context.Background(), AppRadarr, "sqp-1-2160p")
	if err != nil || !strings.Contains(string(data), "SQP-1") {
		t.Fatalf("profile %s %v", data, err)
	}
}

func TestTrashProfileGroupFormats(t *testing.T) {
	gh := newFakeGitHub(t, trashGroupTree())
	g := gh.guides(nil)
	got, err := g.ProfileGroupFormats(context.Background(), AppRadarr, "p3")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"DD+ ATMOS": "cf-grp1", "TrueHD": "cf-grp3", "BR-DISK": "cf-brdisk"}
	if !maps.Equal(got, want) {
		t.Fatalf("group formats %v, want %v (required and default-on of default groups only)", got, want)
	}
	// A profile with no TRaSH ID keeps only its own formatItems.
	if got, err := g.ProfileGroupFormats(context.Background(), AppRadarr, ""); err != nil || len(got) != 0 {
		t.Fatalf("no trash ID: %v %v", got, err)
	}
	if _, err := g.ProfileGroupFormats(context.Background(), "lidarr", "p3"); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestTrashCustomFormatsFindsByIDAndStopsEarly(t *testing.T) {
	tree := trashTree()
	for i := range 40 {
		tree["radarr/cf/zz-filler-"+strconv.Itoa(i)+".json"] = `{"trash_id":"filler","name":"Filler","specifications":[]}`
	}
	gh := newFakeGitHub(t, tree)
	// One fetch at a time makes the stopping point exact; with more in
	// flight, how many extra files start before the last match lands
	// depends on scheduling.
	g := NewTrashGuides(TrashOptions{APIBase: gh.srv.URL + "/api", RawBase: gh.srv.URL + "/raw", UserAgent: "Ether/test", Parallel: 1})
	// "WEB Tier 01" lives in web-tier-1.json, not the guessed web-tier-01.json.
	found, err := g.CustomFormats(context.Background(), AppRadarr, map[string]string{"BR-DISK": "cf-brdisk", "WEB Tier 01": "cf-web1"})
	if err != nil {
		t.Fatal(err)
	}
	if found["cf-brdisk"].Name != "BR-DISK" || found["cf-web1"].Scores["sqp-1-2160p"] != 1800 || len(found) != 2 {
		t.Fatalf("found %+v", found)
	}
	// The guessed br-disk.json first, then the rest in name order up to
	// web-tier-1.json; none of the fillers after it.
	want := 1
	for name := range tree {
		if f, ok := strings.CutPrefix(name, "radarr/cf/"); ok && f != "br-disk.json" && f <= "web-tier-1.json" {
			want++
		}
	}
	_, files := gh.fetched()
	if files != want {
		t.Fatalf("fetched %d files, want %d (stop once every format is found)", files, want)
	}
	// Cached: the same lookup fetches nothing.
	if _, err := g.CustomFormats(context.Background(), AppRadarr, map[string]string{"BR-DISK": "cf-brdisk"}); err != nil {
		t.Fatal(err)
	}
	if lists, again := gh.fetched(); again != files || lists != 1 {
		t.Fatalf("lookup not cached: %d listings, %d -> %d files", lists, files, again)
	}
	// An ID TRaSH doesn't have: every file is searched, nothing invented.
	found, err = g.CustomFormats(context.Background(), AppRadarr, map[string]string{"Gone": "cf-gone"})
	if err != nil || len(found) != 0 {
		t.Fatalf("missing id: %v %v", found, err)
	}
}

func TestTrashServesStaleCopiesWhenGitHubFails(t *testing.T) {
	gh := newFakeGitHub(t, trashTree())
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	g := gh.guides(clock)
	if _, err := g.Profiles(context.Background(), AppRadarr); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	gh.down.Store(true)
	list, err := g.Profiles(context.Background(), AppRadarr)
	if err != nil || len(list) != 2 {
		t.Fatalf("stale copy not served: %v %v", list, err)
	}
	// Nothing cached for Sonarr: the failure is reported, without the URL.
	_, err = g.Profiles(context.Background(), AppSonarr)
	if !errors.Is(err, ErrTrashUnavailable) || strings.Contains(err.Error(), gh.srv.URL) {
		t.Fatalf("sonarr while down: %v", err)
	}
}

func TestTrashRateLimitIsExplained(t *testing.T) {
	gh := newFakeGitHub(t, trashTree())
	gh.limited.Store(true)
	_, err := gh.guides(nil).Profiles(context.Background(), AppRadarr)
	if !errors.Is(err, ErrTrashUnavailable) || !strings.Contains(err.Error(), "hourly request limit") || !strings.Contains(err.Error(), "try again in 20 minutes") {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestTrashConcurrentListingsShareOneRequest(t *testing.T) {
	gh := newFakeGitHub(t, trashTree())
	g := gh.guides(nil)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.list(context.Background(), AppRadarr, trashKindFormats); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if lists, _ := gh.fetched(); lists != 1 {
		t.Fatalf("%d listing requests for one directory", lists)
	}
}

func TestTrashCacheIsBounded(t *testing.T) {
	g := NewTrashGuides(TrashOptions{})
	big := make([]byte, 1<<20)
	for i := range 40 {
		g.store("radarr/cf/"+strconv.Itoa(i)+".json", big)
	}
	if g.bytes > trashCacheBytes || len(g.files) > trashCacheBytes/len(big) {
		t.Fatalf("cache holds %d bytes in %d files", g.bytes, len(g.files))
	}
}

func TestPlainTextAndSlug(t *testing.T) {
	if got := plainText(`<p>One &lt;two&gt;</p><script>x</script>  three`); got != "One <two>\nthree" {
		t.Fatalf("plain text %q", got)
	}
	if got := trashSlug("DD+ ATMOS (undefined)"); got != "dd-atmos-undefined" {
		t.Fatalf("slug %q", got)
	}
}
