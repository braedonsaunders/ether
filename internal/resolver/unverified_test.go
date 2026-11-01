package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet"
)

// fetchFailIndexer is fakeIndexer whose NZB fetch fails for chosen ids.
type fetchFailIndexer struct {
	*fakeIndexer
	fail map[string]error
}

func (f fetchFailIndexer) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if err := f.fail[r.ID]; err != nil {
		return nil, err
	}
	return f.fakeIndexer.FetchNZB(ctx, r)
}

func timeoutHealth(fail map[string]error) *continuationHealth {
	return &continuationHealth{checks: map[string]int{}, failure: fail}
}

func alternateIDs(res *core.Resolution) []string {
	var ids []string
	for _, r := range res.Alternates {
		ids = append(ids, r.ID)
	}
	return ids
}

func assertUnverifiedPlay(t *testing.T, s *Service, res *core.Resolution, want string) {
	t.Helper()
	ctx := context.Background()
	if res.Release.ID != want {
		t.Fatalf("played %q, want %q", res.Release.ID, want)
	}
	if s.recentlyVerified(ctx, want) {
		t.Fatal("unverified winner recorded in the verified cache")
	}
	if e, ok := s.getAvailability(ctx, int64(res.Title)); ok {
		t.Fatalf("unverified play cached availability %+v", e)
	}
	tr := s.Traces()[0]
	if !tr.Unverified || tr.Winner != want || tr.Err != "" {
		t.Fatalf("trace unverified=%v winner=%q err=%q", tr.Unverified, tr.Winner, tr.Err)
	}
	for _, c := range tr.Candidates {
		if c.ReleaseID == want && !strings.HasPrefix(c.Reason, unverifiedReason) {
			t.Fatalf("winner reason %q does not say it played unverified", c.Reason)
		}
	}
}

// Every check times out (a congested provider): the best-ranked release
// plays, unverified, and the other timed-out ones stay armed for
// mid-stream failover.
func TestAllHealthTimeoutsPlayBestRankedUnverified(t *testing.T) {
	for _, race := range []int{0, 2} {
		h := timeoutHealth(map[string]error{
			"source00": context.DeadlineExceeded,
			"source01": context.DeadlineExceeded,
			"source02": context.DeadlineExceeded,
		})
		cfg := testCfg()
		cfg.Resolver.HealthRace = race
		s := New(&fakeIndexer{releases: continuationReleases(3)}, h, nil, cfg, Options{})
		res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
		if err != nil {
			t.Fatalf("race=%d: %v", race, err)
		}
		assertUnverifiedPlay(t, s, res, "source00")
		if got := alternateIDs(res); len(got) != 2 || got[0] != "source01" || got[1] != "source02" {
			t.Fatalf("race=%d: alternates %v, want the other timed-out releases", race, got)
		}
		if string(res.NZB) != "nzb:source00" {
			t.Fatalf("race=%d: NZB %q", race, res.NZB)
		}
		s.Close()
	}
}

// A confirmed-dead release is never the last resort, nor an alternate,
// even when it outranks the timed-out one.
func TestMixedDeadAndTimeoutNeverPlaysDead(t *testing.T) {
	h := timeoutHealth(map[string]error{
		"source00": core.ErrArticleMissing,
		"source01": context.DeadlineExceeded,
		"source02": core.ErrReleaseRejected,
	})
	s := New(&fakeIndexer{releases: continuationReleases(3)}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil {
		t.Fatal(err)
	}
	assertUnverifiedPlay(t, s, res, "source01")
	if got := alternateIDs(res); len(got) != 0 {
		t.Fatalf("alternates %v include a confirmed-dead release", got)
	}
}

// Only confirmed failures: no last resort, the title is unavailable.
func TestAllDeadStillUnavailable(t *testing.T) {
	h := timeoutHealth(map[string]error{"source00": core.ErrArticleMissing, "source01": core.ErrArticleCorrupt})
	s := New(&fakeIndexer{releases: continuationReleases(2)}, h, nil, testCfg(), Options{})
	defer s.Close()
	if res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("all dead: %+v %v, want ErrNotAvailable", res, err)
	}
}

// One verified release wins exactly as before; the timed-out one ranked
// above it is not chosen and the trace is not marked unverified.
func TestVerifiedCandidateBeatsTimedOut(t *testing.T) {
	h := timeoutHealth(map[string]error{"source00": context.DeadlineExceeded})
	s := New(&fakeIndexer{releases: continuationReleases(2)}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "source01" {
		t.Fatalf("resolution %+v %v, want verified source01", res, err)
	}
	if tr := s.Traces()[0]; tr.Unverified {
		t.Fatal("verified winner traced as unverified")
	}
	if !s.recentlyVerified(context.Background(), "source01") {
		t.Fatal("verified winner missing from the verified cache")
	}
}

// A timed-out NZB fetch has nothing to play: the transient error stands.
func TestFetchTimeoutIsNoLastResort(t *testing.T) {
	idx := fetchFailIndexer{fakeIndexer: &fakeIndexer{releases: continuationReleases(1)}, fail: map[string]error{"source00": context.DeadlineExceeded}}
	s := New(idx, timeoutHealth(map[string]error{}), nil, testCfg(), Options{})
	defer s.Close()
	if res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch timeout: %+v %v", res, err)
	}
}

// A viewer's chosen version that times out still leads the last resort.
func TestPreferredTimedOutLeadsLastResort(t *testing.T) {
	h := timeoutHealth(map[string]error{"source00": context.DeadlineExceeded, "source01": context.DeadlineExceeded})
	s := New(&fakeIndexer{releases: continuationReleases(2)}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "source01"})
	if err != nil {
		t.Fatal(err)
	}
	assertUnverifiedPlay(t, s, res, "source01")
	if got := alternateIDs(res); len(got) != 1 || got[0] != "source00" {
		t.Fatalf("alternates %v", got)
	}
}

// The history the owner reads records the unverified play.
func TestUnverifiedPlayRecordedInHistory(t *testing.T) {
	db := openTestDB(t)
	h := timeoutHealth(map[string]error{"source00": context.DeadlineExceeded})
	s := New(&fakeIndexer{releases: continuationReleases(1)}, h, db, testCfg(), Options{})
	defer s.Close()
	ctx := context.Background()
	if _, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := s.History(ctx, HistoryFilter{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("history %v %v", recs, err)
	}
	if !recs[0].Unverified || recs[0].ReleaseID != "source00" {
		t.Fatalf("history record %+v not marked unverified", recs[0])
	}
	full, err := s.HistoryEntry(ctx, recs[0].ID)
	if err != nil || full.Trace == nil || !full.Trace.Unverified {
		t.Fatalf("history trace %+v %v", full.Trace, err)
	}
}

// Availability validation runs as a background check; an unverified
// release is no answer for it, so the transient error stands.
func TestBackgroundCheckTakesNoLastResort(t *testing.T) {
	h := timeoutHealth(map[string]error{"source00": context.DeadlineExceeded})
	s := New(&fakeIndexer{releases: continuationReleases(1)}, h, nil, testCfg(), Options{})
	defer s.Close()
	ctx := core.WithBackgroundBudget(context.Background(), func(context.Context, string, string, int64) error { return nil })
	if res, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("background check: %+v %v", res, err)
	}
}

// The last resort keeps the resolver's quality order: with every check
// timed out, an unverified 1080p plays before an unverified drastic
// downgrade, which stays a failover alternate.
func TestLastResortNeverPrefersDowngrade(t *testing.T) {
	sd := withParsed(rel("sd", "The Signal (1999) DVDRIP", 800))
	hd := withParsed(rel("hd", "The.Signal.1999.1080p.WEB-DL.EAC3.5.1.H.264-GRP", 4000))
	h := timeoutHealth(map[string]error{"sd": context.DeadlineExceeded, "hd": context.DeadlineExceeded})
	s := New(&fakeIndexer{releases: []core.Release{sd, hd}}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTV4K()})
	if err != nil {
		t.Fatal(err)
	}
	assertUnverifiedPlay(t, s, res, "hd")
	if got := alternateIDs(res); len(got) != 1 || got[0] != "sd" {
		t.Fatalf("alternates %v, want the downgrade kept for failover", got)
	}
}

// The 1080p is confirmed dead and the only downgrade timed out: the
// downgrade plays unverified, and the trace explains both.
func TestLastResortDowngradeIsExplained(t *testing.T) {
	sd := withParsed(rel("sd", "The Signal (1999) DVDRIP", 800))
	hd := withParsed(rel("hd", "The.Signal.1999.1080p.WEB-DL.EAC3.5.1.H.264-GRP", 4000))
	h := timeoutHealth(map[string]error{"sd": context.DeadlineExceeded, "hd": core.ErrArticleMissing})
	s := New(&fakeIndexer{releases: []core.Release{sd, hd}}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTV4K()})
	if err != nil {
		t.Fatal(err)
	}
	assertUnverifiedPlay(t, s, res, "sd")
	if got := alternateIDs(res); len(got) != 0 {
		t.Fatalf("alternates %v include the dead 1080p", got)
	}
	if tr := s.Traces()[0]; !strings.Contains(tr.Downgrade, "1080p") {
		t.Fatalf("trace downgrade %q, want an explanation naming 1080p", tr.Downgrade)
	}
}

// Errors as they reach the resolver from stream/usenet: only a slow but
// answering provider qualifies for the last resort; an unreachable one
// (no providers, breaker backoff, auth lockout, refused/failed dials, DNS)
// never does, even when the error chain also carries a timeout.
func TestCheckTimedOutClassification(t *testing.T) {
	ioTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	dns := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "news.invalid", IsNotFound: true}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"check deadline", context.DeadlineExceeded, true},
		{"validation budget", fmt.Errorf("stream: health: %w", context.DeadlineExceeded), true},
		{"idle-read stall", fmt.Errorf("provider: nntp: BODY: %w", ioTimeout), true},
		{"STAT i/o timeout", fmt.Errorf("provider: nntp: STAT pipeline reply 3/12: %w", ioTimeout), true},
		{"no providers", usenet.ErrNoProviders, false},
		{"all in backoff", fmt.Errorf("%w: %w", usenet.ErrNoProviders, fmt.Errorf("p1: %w for 2s: dial tcp: i/o timeout", usenet.ErrProviderBackoff)), false},
		{"backoff", fmt.Errorf("p1: %w", usenet.ErrProviderBackoff), false},
		{"auth lockout", fmt.Errorf("p1: %w", usenet.ErrProviderDisabled), false},
		{"auth failed", fmt.Errorf("p1: %w", usenet.ErrAuthFailed), false},
		{"service unavailable", fmt.Errorf("p1: %w", usenet.ErrServiceUnavailable), false},
		{"refused", fmt.Errorf("nntp: dial news.invalid:563: %w", refused), false},
		{"dns", fmt.Errorf("nntp: dial news.invalid:563: %w", dns), false},
		{"dial timeout", fmt.Errorf("nntp: dial news.invalid:563: %w", dialTimeout), false},
		{"other transient", errors.New("provider unavailable"), false},
		{"missing", core.ErrArticleMissing, false},
		{"rejected", core.ErrReleaseRejected, false},
		{"debrid miss", core.ErrNotCached, false},
		{"cancelled", context.Canceled, false},
	}
	for _, c := range cases {
		if got := checkTimedOut(c.err); got != c.want {
			t.Errorf("%s: checkTimedOut(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// A slow provider: every check stalls on an idle-read timeout, so the
// best-ranked release plays unverified.
func TestSlowProviderPlaysUnverified(t *testing.T) {
	stall := fmt.Errorf("provider: nntp: BODY: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
	h := timeoutHealth(map[string]error{"source00": stall, "source01": stall})
	s := New(&fakeIndexer{releases: continuationReleases(2)}, h, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil {
		t.Fatal(err)
	}
	assertUnverifiedPlay(t, s, res, "source00")
}

// Every provider in breaker backoff, or refusing connections: a quick
// error and nothing cached, never an unverified play.
func TestProviderOutageNeverPlaysUnverified(t *testing.T) {
	refused := fmt.Errorf("nntp: dial news.invalid:563: %w", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	outages := map[string]error{
		"backoff": fmt.Errorf("%w: %w", usenet.ErrNoProviders, fmt.Errorf("p1: %w", usenet.ErrProviderBackoff)),
		"refused": fmt.Errorf("p1: %w", refused),
	}
	for name, outage := range outages {
		t.Run(name, func(t *testing.T) {
			h := timeoutHealth(map[string]error{"source00": outage, "source01": outage})
			s := New(&fakeIndexer{releases: continuationReleases(2)}, h, nil, testCfg(), Options{})
			defer s.Close()
			ctx := context.Background()
			req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
			res, err := s.Resolve(ctx, req)
			if err == nil || errors.Is(err, core.ErrNotAvailable) {
				t.Fatalf("outage: %+v %v, want the transient error", res, err)
			}
			if tr := s.Traces()[0]; tr.Unverified || tr.Winner != "" {
				t.Fatalf("outage trace unverified=%v winner=%q", tr.Unverified, tr.Winner)
			}
			if e, ok := s.getAvailability(ctx, int64(req.Title.Key)); ok {
				t.Fatalf("outage cached availability %+v", e)
			}
			for _, id := range []string{"source00", "source01"} {
				if s.recentlyVerified(ctx, id) || s.recentlyFailedHealth(id) {
					t.Fatalf("outage cached %s as verified or failed", id)
				}
			}
		})
	}
}
