package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// schedGitHub is a fakeGitHub with per-file delays. Delays are honoured
// against the request context, and fetch starts/completions are recorded
// so tests can assert on counts, sets and completion order — never on
// exact elapsed time.
type schedGitHub struct {
	srv     *httptest.Server
	files   map[string]string // "radarr/cf/x.json" -> body
	delays  map[string]time.Duration
	started chan string

	mu        sync.Mutex
	fetched   []string
	completed []string
}

func newSchedGitHub(t *testing.T, files map[string]string, delays map[string]time.Duration) *schedGitHub {
	t.Helper()
	f := &schedGitHub{files: files, delays: delays, started: make(chan string, 128)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *schedGitHub) guides(parallel int) *TrashGuides {
	return NewTrashGuides(TrashOptions{
		APIBase: f.srv.URL + "/api", RawBase: f.srv.URL + "/raw",
		UserAgent: "Ether/test", Parallel: parallel,
	})
}

func (f *schedGitHub) serve(w http.ResponseWriter, r *http.Request) {
	const api = "/api/repos/TRaSH-Guides/Guides/contents/docs/json/"
	const raw = "/raw/TRaSH-Guides/Guides/master/docs/json/"
	switch {
	case strings.HasPrefix(r.URL.Path, api):
		dir := strings.TrimPrefix(r.URL.Path, api) + "/"
		type entry struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		var list []entry
		for path := range f.files {
			if name, ok := strings.CutPrefix(path, dir); ok {
				list = append(list, entry{Name: name, Type: "file"})
			}
		}
		if len(list) == 0 {
			http.NotFound(w, r)
			return
		}
		slices.SortFunc(list, func(a, b entry) int { return strings.Compare(a.Name, b.Name) })
		_ = json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(r.URL.Path, raw):
		path := strings.TrimPrefix(r.URL.Path, raw)
		body, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.fetched = append(f.fetched, path)
		f.mu.Unlock()
		f.started <- path
		if d := f.delays[path]; d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		f.completed = append(f.completed, path)
		f.mu.Unlock()
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (f *schedGitHub) fetchedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...)
}

func (f *schedGitHub) completedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.completed...)
}

func cfJSON(id, name string) string {
	return fmt.Sprintf(`{"trash_id":%q,"name":%q,"specifications":[]}`, id, name)
}

// earlyLateTree lays out, in CustomFormats scan order (likely guesses
// first, then cold files in name order):
//
//	early.json, b01..b05.json, zz-late.json, zzz-after-01..04.json
//
// early.json matches the slug guess for "Early"; cf-late hides in
// zz-late.json, past five fillers. With Parallel=4 the early stop must
// bound fetches to the first two waves: at most 8 files, through zzz-after-01.
func earlyLateTree() map[string]string {
	tree := map[string]string{
		"radarr/cf/early.json":   cfJSON("cf-early", "Early"),
		"radarr/cf/zz-late.json": cfJSON("cf-late", "Late"),
	}
	for i := 1; i <= 5; i++ {
		tree[fmt.Sprintf("radarr/cf/b%02d.json", i)] = cfJSON(fmt.Sprintf("cf-b%d", i), "Filler")
	}
	for i := 1; i <= 4; i++ {
		tree[fmt.Sprintf("radarr/cf/zzz-after-%02d.json", i)] = cfJSON(fmt.Sprintf("cf-after%d", i), "After")
	}
	return tree
}

var earlyLateWant = map[string]string{"Early": "cf-early", "Late": "cf-late"}

var earlyLateBound = []string{
	"radarr/cf/early.json",
	"radarr/cf/b01.json", "radarr/cf/b02.json", "radarr/cf/b03.json",
	"radarr/cf/b04.json", "radarr/cf/b05.json",
	"radarr/cf/zz-late.json", "radarr/cf/zzz-after-01.json",
}

func assertFoundEarlyLate(t *testing.T, found map[string]core.CustomFormat) {
	t.Helper()
	if len(found) != 2 || found["cf-early"].Name != "Early" || found["cf-late"].Name != "Late" {
		t.Fatalf("found %+v, want cf-early and cf-late", found)
	}
}

func assertFetchBound(t *testing.T, gh *schedGitHub) {
	t.Helper()
	got := gh.fetchedPaths()
	slices.Sort(got)
	// A callback already scheduled in the last wave may see that all wanted
	// formats were found and skip its HTTP request. Completion order can thus
	// shrink the set, but must never start a later wave or duplicate a fetch.
	seen := make(map[string]bool)
	for _, path := range got {
		if !slices.Contains(earlyLateBound, path) || seen[path] {
			t.Fatalf("fetched %v, want unique requests within the first two waves %v", got, earlyLateBound)
		}
		seen[path] = true
	}
	for _, required := range []string{"radarr/cf/early.json", "radarr/cf/zz-late.json"} {
		if !seen[required] {
			t.Fatalf("missing wanted format request %s in %v", required, got)
		}
	}
}

func TestTrashCustomFormatsSlowEarlyFastLate(t *testing.T) {
	gh := newSchedGitHub(t, earlyLateTree(), map[string]time.Duration{
		"radarr/cf/early.json": 250 * time.Millisecond, // wanted, stuck early in wave 1
		// zz-late.json answers instantly, but sits in wave 2.
	})
	found, err := gh.guides(4).CustomFormats(context.Background(), AppRadarr, earlyLateWant)
	if err != nil {
		t.Fatal(err)
	}
	assertFoundEarlyLate(t, found)
	assertFetchBound(t, gh)
}

func TestTrashCustomFormatsEarlyStopAcrossCompletionOrders(t *testing.T) {
	for name, delays := range map[string]map[string]time.Duration{
		"slow early, fast late": {"radarr/cf/early.json": 250 * time.Millisecond},
		"fast early, slow late": {"radarr/cf/zz-late.json": 250 * time.Millisecond},
		"slow filler in wave 1": {"radarr/cf/b02.json": 250 * time.Millisecond},
		"slow filler in wave 2": {"radarr/cf/b05.json": 250 * time.Millisecond},
		"both wanted slow": {
			"radarr/cf/early.json":   150 * time.Millisecond,
			"radarr/cf/zz-late.json": 150 * time.Millisecond,
		},
	} {
		t.Run(name, func(t *testing.T) {
			gh := newSchedGitHub(t, earlyLateTree(), delays)
			found, err := gh.guides(4).CustomFormats(context.Background(), AppRadarr, earlyLateWant)
			if err != nil {
				t.Fatal(err)
			}
			assertFoundEarlyLate(t, found)
			assertFetchBound(t, gh)
		})
	}
}

func TestTrashCustomFormatsCancellation(t *testing.T) {
	files := map[string]string{}
	delays := map[string]time.Duration{}
	for i := 0; i < 12; i++ {
		path := fmt.Sprintf("radarr/cf/f%02d.json", i)
		files[path] = cfJSON(fmt.Sprintf("cf-f%d", i), "Filler")
		delays[path] = 500 * time.Millisecond
	}
	gh := newSchedGitHub(t, files, delays)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := gh.guides(4).CustomFormats(ctx, AppRadarr, map[string]string{"Nope": "cf-nope"})
		done <- result{err}
	}()
	// Cancel only once the whole first wave (Parallel=4) is in flight.
	for range 4 {
		select {
		case <-gh.started:
		case <-time.After(5 * time.Second):
			t.Fatal("first wave never started")
		}
	}
	cancel()
	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CustomFormats did not return promptly after cancel")
	}
	if n := len(gh.fetchedPaths()); n != 4 {
		t.Fatalf("fetched %d files, want only the first wave of 4", n)
	}
	// No fetch may start after cancellation; the count must be stable.
	time.Sleep(150 * time.Millisecond)
	if n := len(gh.fetchedPaths()); n != 4 {
		t.Fatalf("fetches grew to %d after cancel", n)
	}
}

func TestTrashProfilesSlidingProgressPreserved(t *testing.T) {
	profile := func(id, name string) string {
		return fmt.Sprintf(`{"trash_id":%q,"name":%q,"formatItems":{}}`, id, name)
	}
	files := map[string]string{
		"radarr/quality-profiles/a-slow.json": profile("p-slow", "Slow"),
		"radarr/quality-profiles/b.json":      profile("p-b", "B"),
		"radarr/quality-profiles/c.json":      profile("p-c", "C"),
		"radarr/quality-profiles/d.json":      profile("p-d", "D"),
		"radarr/quality-profiles/e.json":      profile("p-e", "E"),
	}
	gh := newSchedGitHub(t, files, map[string]time.Duration{
		"radarr/quality-profiles/a-slow.json": 300 * time.Millisecond,
	})
	list, err := gh.guides(2).Profiles(context.Background(), AppRadarr)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(list))
	for i, p := range list {
		names[i] = p.Name
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"B", "C", "D", "E", "Slow"}) {
		t.Fatalf("profiles %v, want all five", names)
	}
	// Sliding proof without clocks: with Parallel=2 a wave barrier would
	// complete c/d/e only after a-slow; sliding finishes every fast
	// profile before the slow one, which must complete last.
	completed := gh.completedPaths()
	if len(completed) != 5 || completed[len(completed)-1] != "radarr/quality-profiles/a-slow.json" {
		t.Fatalf("completion order %v, want a-slow.json last", completed)
	}
}

func TestValidateCandidatesSlowEarlyFastLateKeepsRankOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := windowHealth(func(ctx context.Context, id string) (bool, error) {
			if id == "slow-early" {
				select {
				case <-time.After(time.Second): // healthy, well inside the grace
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			return true, nil
		})
		s := New(&fakeIndexer{}, h, nil, testCfg(), Options{})
		defer s.Close()
		cs, tr := windowCandidates("slow-early", "fast-late", "unneeded")
		winner, nzb, err := s.validateCandidates(context.Background(), cs, tr, 0)
		if err != nil || winner != 0 || string(nzb) != "nzb:slow-early" {
			t.Fatalf("winner=%d nzb=%q err=%v, want the slow early candidate to keep rank", winner, nzb, err)
		}
		if !tr.Candidates[0].Healthy || tr.Candidates[2].Attempted {
			t.Fatalf("trace %+v, want lead healthy and third candidate untouched", tr.Candidates)
		}
	})
}
