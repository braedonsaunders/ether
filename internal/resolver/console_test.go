package resolver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(context.Background(), filepath.Join(t.TempDir(), "ether.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// plainCaps selects the default 1080p profile and leaves every score as
// interactive search computes it (no codec lists, not remote).
func plainCaps() core.ClientCaps {
	return core.ClientCaps{Product: "Plex Web", MaxHeight: 1080}
}

func consoleReleases() []core.Release {
	pw := rel("pwd", "The.Signal.1999.1080p.BluRay.x264-PWD", 9000)
	pw.Password = true
	pw.Protocol = core.ProtocolTorrent
	return append(movieReleases(),
		rel("cam", "The.Signal.1999.CAM.x264-GRP", 700),
		pw,
	)
}

func findRelease(t *testing.T, res *ConsoleSearch, id string) ConsoleRelease {
	t.Helper()
	for _, r := range res.Releases {
		if r.Release.ID == id {
			return r
		}
	}
	t.Fatalf("release %q not listed: %+v", id, res.Releases)
	return ConsoleRelease{}
}

func TestConsoleSearchMatchesResolveRanking(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: consoleReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: plainCaps()})
	if err != nil {
		t.Fatal(err)
	}
	search, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if search.Profile != "1080p" || !search.Cached || idx.searches.Load() != 1 {
		t.Fatalf("profile=%q cached=%v searches=%d", search.Profile, search.Cached, idx.searches.Load())
	}
	if search.Winner != res.Release.ID {
		t.Fatalf("console winner %q, resolve picked %q", search.Winner, res.Release.ID)
	}
	if len(search.Releases) != len(consoleReleases()) {
		t.Fatalf("listed %d releases", len(search.Releases))
	}
	trace := svc.Traces()[0]
	for _, ct := range trace.Candidates {
		cr := findRelease(t, search, ct.ReleaseID)
		if cr.Rejected != ct.Rejected || (cr.Rejected && cr.Reason != ct.Reason) {
			t.Errorf("%s: console rejected=%v %q, resolve rejected=%v %q", ct.ReleaseID, cr.Rejected, cr.Reason, ct.Rejected, ct.Reason)
		}
		if !ct.Rejected && math.Abs(cr.Score-ct.Score) > 1e-6 { // the age penalty moves with the clock
			t.Errorf("%s: console score %v, resolve score %v", ct.ReleaseID, cr.Score, ct.Score)
		}
	}
	uhd := findRelease(t, search, "uhd")
	if !uhd.Rejected || uhd.Reason != "above profile max resolution" || uhd.Score == 0 || uhd.Rank != 0 {
		t.Fatalf("uhd %+v", uhd)
	}
	if r := findRelease(t, search, "pwd"); r.Reason != "passworded" {
		t.Fatalf("pwd %+v", r)
	}
	if r := findRelease(t, search, "cam"); r.Reason != "cam not allowed" {
		t.Fatalf("cam %+v", r)
	}
	if first := search.Releases[0]; first.Release.ID != "hd" || first.Rank != 1 || !first.Winner {
		t.Fatalf("first listed %+v", first)
	}
	// The resolution's health pass is known without another check.
	if hd := findRelease(t, search, "hd"); hd.Health == nil || !hd.Health.Healthy || hd.Health.Source != "verified" {
		t.Fatalf("hd health %+v", hd.Health)
	}

	// Without profile gates the 4K remux is acceptable; downloads use the
	// compact download profile; fresh bypasses the cache.
	none, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{Profile: ProfileNone, Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := findRelease(t, none, "uhd"); r.Rejected || none.Cached || idx.searches.Load() != 2 {
		t.Fatalf("no-gates uhd %+v cached=%v searches=%d", r, none.Cached, idx.searches.Load())
	}
	dl, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{Profile: ProfileDownload})
	if err != nil {
		t.Fatal(err)
	}
	if dl.Profile != "1080p+download" || findRelease(t, dl, "uhd").Rejected == false {
		t.Fatalf("download profile %q", dl.Profile)
	}
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{Profile: "nope"}); !errors.Is(err, ErrUnknownProfile) {
		t.Fatalf("unknown profile err=%v", err)
	}
	def, names := svc.ConsoleProfiles()
	if def != "1080p" || strings.Join(names, ",") != "4K,1080p,720p" {
		t.Fatalf("profiles %q %v", def, names)
	}
}

func TestFreshSearchFindingCandidatesAllowsPlaybackRetry(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	idx := &fakeIndexer{}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{})
	defer svc.Close()
	req := core.ResolveRequest{Title: matrixTitle(), Client: plainCaps()}
	if _, err := svc.Resolve(ctx, req); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("initial empty search: %v", err)
	}
	idx.mu.Lock()
	idx.releases = movieReleases()
	idx.mu.Unlock()
	search, err := svc.SearchReleases(ctx, req.Title, SearchOptions{Fresh: true})
	if err != nil || search.Winner == "" {
		t.Fatalf("fresh source discovery: %+v %v", search, err)
	}
	// Finding candidates does not prove health, but it must revoke the old
	// negative verdict so Play can validate the newly discovered source.
	if known, available := svc.Available(ctx, req.Title.Key); known && !available {
		t.Fatal("fresh search found releases but Play is still blocked by the old failure")
	}
	res, err := svc.Resolve(ctx, req)
	if err != nil || res == nil {
		t.Fatalf("retry after source discovery: %+v %v", res, err)
	}
	if idx.searches.Load() != 2 {
		t.Fatalf("retry should reuse fresh discovery: searches=%d", idx.searches.Load())
	}
}

func TestPinHonouredAndGated(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "The Signal (1999)", r720, "the good encode"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("pinned res=%+v err=%v", res, err)
	}
	if tr := svc.Traces()[0]; tr.Pinned != "sd720" || tr.PinStatus != "used" {
		t.Fatalf("trace pin %q %q", tr.Pinned, tr.PinStatus)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinHealthy || p.Note != "the good encode" {
		t.Fatalf("pin %+v", p)
	}
	// A viewer's explicit version pick wins over the pin.
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "hd"})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("viewer pick res=%+v err=%v", res, err)
	}
	// The pin overrides profile policy (a 4K pin on a 1080p profile)...
	uhd, _ := svc.LookupRelease("uhd")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", uhd, ""); err != nil {
		t.Fatal(err)
	}
	caps := webCaps()
	caps.VideoCodecs = append(caps.VideoCodecs, "hevc")
	caps.DolbyVision = true
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: caps})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("4K pin res=%+v err=%v", res, err)
	}
	// ...but not a client that cannot decode it.
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("hevc pin on h264 client res=%+v err=%v", res, err)
	}
	if tr := svc.Traces()[0]; tr.PinStatus != "rejected: video codec unsupported by client" {
		t.Fatalf("pin status %q", tr.PinStatus)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinRejected {
		t.Fatalf("pin %+v", p)
	}
	// Downloads ignore pins.
	dl := webCaps()
	dl.Download = true
	if res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: dl}); err != nil || res.Release.ID == "uhd" {
		t.Fatalf("download res=%+v err=%v", res, err)
	}
	// Blocked releases cannot be pinned.
	if err := svc.MarkBad(ctx, "hd", "broken"); err != nil {
		t.Fatal(err)
	}
	hd, _ := svc.LookupRelease("hd")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", hd, ""); !errors.Is(err, ErrPinBlocked) {
		t.Fatalf("pin blocked err=%v", err)
	}
	if _, err := svc.Pin(ctx, core.ShowKey(1), "", hd, ""); !errors.Is(err, ErrNotPinnable) {
		t.Fatalf("pin show err=%v", err)
	}
}

func TestPinUnhealthyFallsBackAndKeepsPin(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, db, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", r720, ""); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if tr := svc.Traces()[0]; tr.PinStatus != "unhealthy: health: unhealthy" {
		t.Fatalf("pin status %q", tr.PinStatus)
	}
	svc2 := New(idx, fakeHealth{}, db, testCfg(), Options{})
	p, ok := svc2.PinFor(matrixTitle().Key)
	if !ok || p.LastStatus != PinUnhealthy || p.Release.Name != r720.Name || p.LastCheckedAt.IsZero() {
		t.Fatalf("persisted pin %+v ok=%v", p, ok)
	}
	// The pinned release is not auto-blocked by a failed pre-play check.
	if svc2.isBad("sd720") {
		t.Fatal("unhealthy pin was blocklisted")
	}
	if had, err := svc2.Unpin(ctx, matrixTitle().Key); !had || err != nil {
		t.Fatalf("unpin had=%v err=%v", had, err)
	}
	if svc3 := New(idx, fakeHealth{}, db, testCfg(), Options{}); len(svc3.Pins()) != 0 {
		t.Fatalf("pin survived unpin: %+v", svc3.Pins())
	}
}

func TestPinExcludedOnFailoverAndSurvivesSearchLoss(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", r720, ""); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Exclude: []string{"sd720"}})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("failover res=%+v err=%v", res, err)
	}
	tr := svc.Traces()[0]
	if !tr.Failover || tr.PinStatus != "skipped: already tried in this playback" {
		t.Fatalf("failover trace %+v", tr)
	}
	// The release drops out of the indexers: the stored pin still plays.
	idx.mu.Lock()
	idx.releases = movieReleases()[:2]
	idx.mu.Unlock()
	if err := svc.Forget(ctx, matrixTitle()); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("dropped pin res=%+v err=%v", res, err)
	}
	// And while the indexers fail outright.
	failing := &erroringIndexer{fakeIndexer: idx}
	svc.indexer = failing
	if err := svc.Forget(ctx, matrixTitle()); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("pin during outage res=%+v err=%v", res, err)
	}
	if _, err := svc.Unpin(ctx, matrixTitle().Key); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil || !strings.Contains(err.Error(), "indexers down") {
		t.Fatalf("outage without pin err=%v", err)
	}
}

type erroringIndexer struct{ *fakeIndexer }

func (e *erroringIndexer) Search(context.Context, core.SearchRequest) ([]core.Release, error) {
	return nil, errors.New("indexers down")
}

func TestPinIsAutoVariant(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", r720, ""); err != nil {
		t.Fatal(err)
	}
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}, 4)
	if err != nil || len(vs) == 0 {
		t.Fatalf("variants %+v err=%v", vs, err)
	}
	if !vs[0].Auto || vs[0].ID != "sd720" {
		t.Fatalf("auto variant %+v", vs[0])
	}
	for _, v := range vs[1:] {
		if v.ID == "sd720" {
			t.Fatal("pin listed twice")
		}
	}
}

func TestOwnerBlockUnblockPersisted(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := svc.Block(ctx, BlockRequest{ReleaseID: "hd", TitleKey: matrixTitle().Key}, "wrong audio language")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Manual || e.Source != BlockOwner || e.Name != movieReleases()[1].Name || e.TitleName != "The Signal (1999)" || e.Indexer != "fake" {
		t.Fatalf("entry %+v", e)
	}
	// An automatic mark later does not downgrade the owner's block.
	if err := svc.MarkBad(ctx, "hd", "playback failed"); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkBad(ctx, "sd720", "missing articles"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err == nil {
		t.Fatalf("blocked releases still resolved: %+v", res)
	}

	svc2 := New(idx, fakeHealth{}, db, testCfg(), Options{})
	all, total, err := svc2.Blocklist(ctx, BlockFilter{})
	if err != nil || total != 2 || len(all) != 2 {
		t.Fatalf("blocklist %+v total=%d err=%v", all, total, err)
	}
	owner, _, _ := svc2.Blocklist(ctx, BlockFilter{Source: BlockOwner})
	if len(owner) != 1 || owner[0].ReleaseID != "hd" || owner[0].Reason != "wrong audio language" || !owner[0].Manual {
		t.Fatalf("owner entries %+v", owner)
	}
	auto, _, _ := svc2.Blocklist(ctx, BlockFilter{Source: BlockAuto, Query: "720P"})
	if len(auto) != 1 || auto[0].ReleaseID != "sd720" || auto[0].TitleKey != matrixTitle().Key {
		t.Fatalf("auto entries %+v", auto)
	}
	if byTitle, _, _ := svc2.Blocklist(ctx, BlockFilter{TitleKey: core.MovieKey(1)}); len(byTitle) != 0 {
		t.Fatalf("title filter %+v", byTitle)
	}
	if page, total, _ := svc2.Blocklist(ctx, BlockFilter{Limit: 1, Offset: 1}); len(page) != 1 || total != 2 {
		t.Fatalf("page %+v total=%d", page, total)
	}

	got, err := svc2.Unblock(ctx, "hd")
	if err != nil || got.ReleaseID != "hd" || got.TitleKey != matrixTitle().Key {
		t.Fatalf("unblock %+v err=%v", got, err)
	}
	if _, err := svc2.Unblock(ctx, "hd"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second unblock err=%v", err)
	}
	if known, _ := svc2.Available(ctx, matrixTitle().Key); known {
		t.Fatal("unblock kept the title's cool-off")
	}
	svc3 := New(idx, fakeHealth{}, db, testCfg(), Options{})
	if svc3.isBad("hd") || !svc3.isBad("sd720") {
		t.Fatal("unblock not persisted")
	}
	res, err = svc3.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("after unblock res=%+v err=%v", res, err)
	}
}

func TestBlocklistWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc.Block(ctx, BlockRequest{ReleaseID: "x", Name: "Some.Release"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkBad(ctx, "x", "auto"); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.Blocklist(ctx, BlockFilter{})
	if err != nil || total != 1 || !list[0].Manual || list[0].Reason != "blocked by owner" || list[0].Name != "Some.Release" {
		t.Fatalf("list %+v total=%d err=%v", list, total, err)
	}
	if _, err := svc.Unblock(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := svc.Blocklist(ctx, BlockFilter{}); total != 0 {
		t.Fatalf("total %d after unblock", total)
	}
}

func TestHistoryWrittenAndBounded(t *testing.T) {
	for _, withDB := range []bool{true, false} {
		t.Run(fmt.Sprintf("db=%v", withDB), func(t *testing.T) {
			ctx := context.Background()
			var db *sql.DB
			if withDB {
				db = openTestDB(t)
			}
			clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			idx := &fakeIndexer{releases: movieReleases()}
			svc := New(idx, fakeHealth{}, db, testCfg(), Options{Now: clock.now, HistoryMax: 3, HistoryMaxAge: 48 * time.Hour})
			for i := 0; i < 5; i++ {
				clock.add(time.Minute)
				req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
				if i == 4 {
					req.Exclude = []string{"hd"}
				}
				if _, err := svc.Resolve(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			items, total, err := svc.History(ctx, HistoryFilter{})
			if err != nil || total != 3 || len(items) != 3 {
				t.Fatalf("history %+v total=%d err=%v", items, total, err)
			}
			newest := items[0]
			if !newest.Failover || newest.ReleaseID != "sd720" || newest.ReleaseName == "" || newest.Size == 0 || newest.Candidates != 3 || newest.Trace != nil {
				t.Fatalf("newest %+v", newest)
			}
			if !items[1].At.Before(newest.At) || items[1].Failover || items[1].ReleaseID != "hd" {
				t.Fatalf("order %+v", items)
			}
			full, err := svc.HistoryEntry(ctx, newest.ID)
			if err != nil || full.Trace == nil || len(full.Trace.Candidates) != 3 || full.Trace.Winner != "sd720" {
				t.Fatalf("entry %+v err=%v", full, err)
			}
			if len(full.Trace.Discovery) != 1 || full.Trace.Discovery[0].Path != "cache" || full.Trace.Discovery[0].Count != 3 || full.Trace.Limits == nil || full.Trace.Limits.ClientMaxHeight != 1080 {
				t.Fatalf("stored discovery: %+v", full.Trace)
			}
			if _, err := svc.HistoryEntry(ctx, 99999); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("missing entry err=%v", err)
			}
			if other, total, _ := svc.History(ctx, HistoryFilter{TitleKey: core.MovieKey(1)}); len(other) != 0 || total != 0 {
				t.Fatalf("title filter %+v", other)
			}
			// Age bound: an old outcome is dropped on the next write.
			clock.add(72 * time.Hour)
			if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil {
				t.Fatal(err)
			}
			if _, total, _ := svc.History(ctx, HistoryFilter{}); total != 1 {
				t.Fatalf("age bound kept %d rows", total)
			}
			// The in-memory traces keep working.
			if len(svc.Traces()) != 6 {
				t.Fatalf("traces %d", len(svc.Traces()))
			}
		})
	}
}

func TestCompactTraceKeepsWinnerAndPin(t *testing.T) {
	tr := Trace{Winner: "w"}
	for i := 0; i < 80; i++ {
		tr.Candidates = append(tr.Candidates, CandidateTrace{ReleaseID: fmt.Sprint(i), Rejected: true})
	}
	tr.Candidates = append(tr.Candidates, CandidateTrace{ReleaseID: "p", Pinned: true, Rejected: true}, CandidateTrace{ReleaseID: "w"})
	got := compactTrace(tr)
	if len(got.Candidates) != maxTraceCandidates || got.Omitted != 32 {
		t.Fatalf("len=%d omitted=%d", len(got.Candidates), got.Omitted)
	}
	if got.Candidates[0].ReleaseID != "w" || got.Candidates[1].ReleaseID != "p" {
		t.Fatalf("head %+v", got.Candidates[:2])
	}
	if h := historyFromTrace(got); h.Candidates != 82 || h.ReleaseID != "w" {
		t.Fatalf("history %+v", h)
	}
}

func TestForgetClearsCooloffAndSearchCache(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	idx := &fakeIndexer{}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{})
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("err=%v", err)
	}
	if known, avail := svc.Available(ctx, matrixTitle().Key); !known || avail {
		t.Fatal("no cool-off recorded")
	}
	if err := svc.Forget(ctx, matrixTitle()); err != nil {
		t.Fatal(err)
	}
	if known, _ := svc.Available(ctx, matrixTitle().Key); known {
		t.Fatal("cool-off survived forget")
	}
	if known, _ := New(idx, fakeHealth{}, db, testCfg(), Options{}).Available(ctx, matrixTitle().Key); known {
		t.Fatal("persisted cool-off survived forget")
	}
	idx.mu.Lock()
	idx.releases = movieReleases()
	idx.mu.Unlock()
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" || idx.searches.Load() != 2 {
		t.Fatalf("res=%+v err=%v searches=%d", res, err, idx.searches.Load())
	}
}

func TestCheckReleaseOnDemand(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	var checks atomic.Int64
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"sd720": true}, checks: &checks}, db, testCfg(), Options{})
	if _, err := svc.CheckRelease(ctx, "hd", 0); !errors.Is(err, ErrUnknownRelease) {
		t.Fatalf("unknown release err=%v", err)
	}
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", r720, ""); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.CheckRelease(ctx, "hd", matrixTitle().Key)
	if err != nil || !ok.Healthy || ok.Blocked || ok.CheckedAt.IsZero() {
		t.Fatalf("hd %+v err=%v", ok, err)
	}
	if !svc.recentlyVerified(ctx, "hd") {
		t.Fatal("pass not cached as verified")
	}
	// The owner's check is always fresh, even when verified.
	if _, err := svc.CheckRelease(ctx, "hd", matrixTitle().Key); err != nil || checks.Load() != 2 {
		t.Fatalf("checks=%d err=%v", checks.Load(), err)
	}
	bad, err := svc.CheckRelease(ctx, "sd720", matrixTitle().Key)
	if err != nil || bad.Healthy || !bad.Blocked {
		t.Fatalf("sd720 %+v err=%v", bad, err)
	}
	list, _, _ := svc.Blocklist(ctx, BlockFilter{})
	if len(list) != 1 || list[0].ReleaseID != "sd720" || list[0].Source != BlockAuto || list[0].TitleKey != matrixTitle().Key {
		t.Fatalf("blocklist %+v", list)
	}
	if p, _ := svc.PinFor(matrixTitle().Key); p.LastStatus != PinUnhealthy {
		t.Fatalf("pin %+v", p)
	}
	search, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r := findRelease(t, search, "sd720"); !r.Blocked || r.Health == nil || r.Health.Healthy || r.Reason != "marked bad" {
		t.Fatalf("listed sd720 %+v", r)
	}
	if search.PinNote != "skipped: pinned release is blocklisted" || search.Pin == nil {
		t.Fatalf("pin note %q", search.PinNote)
	}
}

func TestBlockingAPinnedReleaseSurfacesOnThePin(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc.SearchReleases(ctx, matrixTitle(), SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	r720, _ := svc.LookupRelease("sd720")
	if _, err := svc.Pin(ctx, matrixTitle().Key, "", r720, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkBad(ctx, "sd720", "playback failed"); err != nil {
		t.Fatal(err)
	}
	p, _ := svc.PinFor(matrixTitle().Key)
	if p.LastStatus != PinUnhealthy || p.LastReason != "blocklisted: playback failed" {
		t.Fatalf("pin %+v", p)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if tr := svc.Traces()[0]; tr.PinStatus != "skipped: pinned release is blocklisted" || tr.TitleName != "The Signal (1999)" {
		t.Fatalf("trace %q %q", tr.PinStatus, tr.TitleName)
	}
}

// A slow failed grab is more useful than fifty untried alternatives when
// explaining startup latency. Keep even zero-duration attempts (coarse clocks).
func TestCompactTraceRetainsFailedAttemptsAheadOfUntriedCandidates(t *testing.T) {
	tr := Trace{Winner: "winner"}
	for i := 0; i < 80; i++ {
		tr.Candidates = append(tr.Candidates, CandidateTrace{ReleaseID: fmt.Sprint(i)})
	}
	tr.Candidates = append(tr.Candidates,
		CandidateTrace{ReleaseID: "failed-fetch", Attempted: true, Rejected: true, Reason: "HTTP 429"},
		CandidateTrace{ReleaseID: "failed-health", HealthChecked: true, Rejected: true},
		CandidateTrace{ReleaseID: "winner", Attempted: true, Healthy: true})
	got := compactTrace(tr)
	if len(got.Candidates) != maxTraceCandidates || got.Candidates[0].ReleaseID != "winner" || got.Candidates[1].ReleaseID != "failed-fetch" || got.Candidates[2].ReleaseID != "failed-health" {
		t.Fatalf("lost startup failures: %+v", got.Candidates[:3])
	}
	if tr.Candidates[0].Attempted || tr.Candidates[0].ReleaseID != "0" {
		t.Fatal("compaction mutated source trace")
	}
}

func TestReleaseBlockPersistsWithoutTitleKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	s := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{})
	if _, err := s.Block(ctx, BlockRequest{ReleaseID: "unknown-title"}, "owner"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{})
	defer s.Close()
	blocked, err := s.ReleaseBlocked(ctx, "unknown-title")
	if err != nil || !blocked {
		t.Fatalf("persisted block: %v %v", blocked, err)
	}
}
