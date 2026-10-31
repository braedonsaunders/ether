package resolver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func abzurdah() *core.Title {
	return &core.Title{Key: core.MovieKey(353989), Type: core.TypeMovie, IDs: core.IDs{TMDB: 353989},
		Title: "Abzurdah", Year: 2015, Runtime: 90 * time.Minute, Language: "es"}
}

// The Apple TV case: the only 1080p failed its check and an unlabelled
// DVD rip was next. A comparable release from the rescue search must win
// over the drastic step down.
func TestFailoverTriesComparableBeforeDrasticDowngrade(t *testing.T) {
	hd := withParsed(rel("hd", "Abzurdah.2015.1080p.WEB-DL.EAC3.5.1.H.264-GRP", 4000))
	sd := withParsed(rel("sd", "Abzurdah (2015) DVDRIP Spanish", 800))
	rescued := withParsed(rel("bluray", "Abzurdah.2015.SPANISH.1080p.BluRay.x264-GRP", 6000))
	idx := &rescueFakeIndexer{fakeIndexer: &fakeIndexer{releases: []core.Release{hd, sd}}, rescued: []core.Release{rescued}}
	s := New(idx, fakeHealth{unhealthy: map[string]bool{"hd": true}}, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: abzurdah(), Client: appleTV4K()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release.ID != "bluray" {
		t.Fatalf("played %q, want the rescued 1080p before the DVD rip", res.Release.Name)
	}
}

// With nothing comparable left, the downgrade still plays and says why.
func TestDrasticDowngradeIsExplained(t *testing.T) {
	hd := withParsed(rel("hd", "Abzurdah.2015.1080p.WEB-DL.EAC3.5.1.H.264-GRP", 4000))
	sd := withParsed(rel("sd", "Abzurdah (2015) DVDRIP Spanish", 800))
	idx := &rescueFakeIndexer{fakeIndexer: &fakeIndexer{releases: []core.Release{hd, sd}}}
	s := New(idx, fakeHealth{unhealthy: map[string]bool{"hd": true}}, nil, testCfg(), Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: abzurdah(), Client: appleTV4K()})
	if err != nil || res.Release.ID != "sd" {
		t.Fatalf("res=%v err=%v, want the DVD rip", res, err)
	}
	tr := s.Traces()
	if len(tr) == 0 || !strings.Contains(tr[len(tr)-1].Downgrade, "1080p") {
		t.Fatalf("trace downgrade %q, want an explanation naming 1080p", tr[len(tr)-1].Downgrade)
	}
	// A failover that excludes the failed 1080p remembers it was on offer.
	s2 := New(&rescueFakeIndexer{fakeIndexer: &fakeIndexer{releases: []core.Release{hd, sd}}}, fakeHealth{}, nil, testCfg(), Options{})
	defer s2.Close()
	if _, err := s2.SearchReleases(context.Background(), abzurdah(), SearchOptions{Profile: "4K"}); err != nil {
		t.Fatal(err)
	}
	res, err = s2.Resolve(context.Background(), core.ResolveRequest{Title: abzurdah(), Client: appleTV4K(), Exclude: []string{"hd"}})
	if err != nil || res.Release.ID != "sd" {
		t.Fatalf("failover res=%v err=%v", res, err)
	}
	tr = s2.Traces()
	if !strings.Contains(tr[len(tr)-1].Downgrade, "tried before") {
		t.Fatalf("failover downgrade %q", tr[len(tr)-1].Downgrade)
	}
}
