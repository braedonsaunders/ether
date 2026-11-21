package resolver

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/vault"
)

type replayVault struct {
	vault.Store
	entry   vault.Entry
	entries []vault.Entry
}

func (v *replayVault) allEntries() []vault.Entry {
	if len(v.entries) > 0 {
		return v.entries
	}
	return []vault.Entry{v.entry}
}

func (v *replayVault) Search(context.Context, core.SearchRequest) ([]vault.Entry, error) {
	return append([]vault.Entry(nil), v.allEntries()...), nil
}
func (v *replayVault) Entry(_ context.Context, k vault.Key) (vault.Entry, error) {
	for _, e := range v.allEntries() {
		if e.Key == k {
			return e, nil
		}
	}
	return v.entry, nil
}
func (v *replayVault) Get(context.Context, vault.Key) (*vault.Record, error) {
	return &vault.Record{NZB: &nzb.NZB{Files: []*nzb.File{{Segments: []nzb.Segment{{MessageID: "sample"}}}}}}, nil
}
func (v *replayVault) MarkHealth(_ context.Context, k vault.Key, h vault.Health) error {
	for i := range v.entries {
		if v.entries[i].Key == k {
			v.entries[i].Health = h
		}
	}
	if v.entry.Key == k || len(v.entries) == 0 {
		v.entry.Health = h
	}
	return nil
}
func (v *replayVault) VerifyArticles(context.Context, []string, []string) (bool, error) {
	return false, nil
}
func (v *replayVault) Touch(context.Context, vault.Key, bool) error { return nil }
func (v *replayVault) ByAlias(context.Context, string, string) (vault.Key, bool, error) {
	return "", false, nil
}

func TestLibraryReplayCutoffFingerprintAndDeadFallback(t *testing.T) {
	for _, scenario := range []string{"replay", "lower quality", "changed profile", "dead"} {
		t.Run(scenario, func(t *testing.T) {
			req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
			p := core.QualityProfile{Name: "Library", MaxHeight: 1080, Language: "any"}
			v := &replayVault{entry: vault.Entry{Key: "sample", Kind: vault.KindUsenet, Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, Health: vault.HealthOK, HealthAt: time.Now()}}
			idx := &fakeIndexer{releases: []core.Release{rel("live", "The.Signal.1999.1080p.BluRay.x264.Proper-GRP", 8100)}}
			s := New(idx, fakeHealth{}, nil, testCfg(), Options{Vault: v, Verifier: v})
			v.entry.ProfileFP = ProfileFingerprint(p, wantedAudio(p, req.User, s.catalogLanguage()))
			switch scenario {
			case "changed profile":
				v.entry.ProfileFP = "older"
			case "lower quality":
				v.entry.Name = "The.Signal.1999.720p.WEB-DL.H264-GRP"
			case "dead":
				v.entry.HealthAt = time.Time{}
			}
			res, err := s.resolve(context.Background(), req, p)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "replay" {
				if res.Release.ID != "vault-sample" || idx.searches.Load() > 0 {
					t.Fatalf("expected library replay, got %s", res.Release.ID)
				}
			} else if res.Release.ID != "live" {
				t.Fatalf("expected live result, got %s", res.Release.ID)
			}
			if scenario == "dead" && v.entry.Health != vault.HealthDead {
				t.Fatal("dead sample was not recorded")
			}
		})
	}
}

func TestVaultPreferReplayAndAlternates(t *testing.T) {
	for _, preferredOnly := range []bool{false, true} {
		name := "prefer"
		if preferredOnly {
			name = "prefer-only"
		}
		t.Run(name, func(t *testing.T) {
			req := core.ResolveRequest{
				Title:         matrixTitle(),
				Client:        webCaps(),
				Prefer:        "vault-lower",
				PreferredOnly: preferredOnly,
			}
			p := core.QualityProfile{Name: "Library", MaxHeight: 1080, Language: "any"}
			v := &replayVault{entries: []vault.Entry{
				{Key: "top", Kind: vault.KindUsenet, Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, ProfileFP: "older", Health: vault.HealthOK, HealthAt: time.Now()},
				{Key: "lower", Kind: vault.KindUsenet, Name: "The.Signal.1999.720p.WEB-DL.H264-ALT", Size: 4000 << 20, ProfileFP: "older", Health: vault.HealthOK, HealthAt: time.Now()},
			}}
			idx := &fakeIndexer{releases: []core.Release{rel("live", "The.Signal.1999.1080p.BluRay.x264.Proper-GRP", 8100)}}
			s := New(idx, fakeHealth{}, nil, testCfg(), Options{Vault: v, Verifier: v})
			defer s.Close()

			res, err := s.resolve(context.Background(), req, p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Release.ID != "vault-lower" {
				t.Fatalf("got release %s, want vault-lower", res.Release.ID)
			}
			if idx.searches.Load() != 0 {
				t.Fatalf("expected no indexer search, got %d", idx.searches.Load())
			}
			if len(res.Alternates) != 1 || res.Alternates[0].ID != "vault-top" {
				t.Fatalf("alternates = %+v, want [vault-top]", res.Alternates)
			}
		})
	}
}

func TestVaultPreferMissingFallsBackToLiveSearch(t *testing.T) {
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "missing-id"}
	p := core.QualityProfile{Name: "Library", MaxHeight: 1080, Language: "any"}
	v := &replayVault{entry: vault.Entry{Key: "sample", Kind: vault.KindUsenet, Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, Health: vault.HealthOK, HealthAt: time.Now()}}
	idx := &fakeIndexer{releases: []core.Release{rel("live", "The.Signal.1999.1080p.BluRay.x264.Proper-GRP", 8100)}}
	s := New(idx, fakeHealth{}, nil, testCfg(), Options{Vault: v, Verifier: v})
	defer s.Close()
	v.entry.ProfileFP = ProfileFingerprint(p, wantedAudio(p, req.User, s.catalogLanguage()))

	res, err := s.resolve(context.Background(), req, p)
	if err != nil {
		t.Fatal(err)
	}
	if idx.searches.Load() == 0 || res.Release.ID != "live" {
		t.Fatalf("expected live fallback, searches=%d release=%s", idx.searches.Load(), res.Release.ID)
	}
}

func TestVaultPreferDeadMarksDeadAndFallsBackToLiveSearch(t *testing.T) {
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "vault-sample"}
	p := core.QualityProfile{Name: "Library", MaxHeight: 1080, Language: "any"}
	v := &replayVault{entry: vault.Entry{Key: "sample", Kind: vault.KindUsenet, Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, Health: vault.HealthOK}}
	idx := &fakeIndexer{releases: []core.Release{rel("live", "The.Signal.1999.1080p.BluRay.x264.Proper-GRP", 8100)}}
	s := New(idx, fakeHealth{}, nil, testCfg(), Options{Vault: v, Verifier: v})
	defer s.Close()
	v.entry.ProfileFP = ProfileFingerprint(p, wantedAudio(p, req.User, s.catalogLanguage()))

	res, err := s.resolve(context.Background(), req, p)
	if err != nil {
		t.Fatal(err)
	}
	if v.entry.Health != vault.HealthDead {
		t.Fatalf("expected dead vault entry to be marked dead, got %v", v.entry.Health)
	}
	if idx.searches.Load() == 0 || res.Release.ID != "live" {
		t.Fatalf("expected live fallback, searches=%d release=%s", idx.searches.Load(), res.Release.ID)
	}
}

func TestVaultPreferredOnlyEmptyPreferDoesNotReturnArbitraryVaultRelease(t *testing.T) {
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), PreferredOnly: true}
	p := core.QualityProfile{Name: "Library", MaxHeight: 1080, Language: "any"}
	v := &replayVault{entry: vault.Entry{Key: "sample", Kind: vault.KindUsenet, Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP", Size: 8000 << 20, Health: vault.HealthOK, HealthAt: time.Now()}}
	idx := &fakeIndexer{releases: []core.Release{rel("live", "The.Signal.1999.1080p.BluRay.x264.Proper-GRP", 8100)}}
	s := New(idx, fakeHealth{}, nil, testCfg(), Options{Vault: v, Verifier: v})
	defer s.Close()
	fp := ProfileFingerprint(p, wantedAudio(p, req.User, s.catalogLanguage()))
	v.entry.ProfileFP = fp

	var tr Trace
	sreq := s.searchRequest(context.Background(), req.Title)
	_, hit, retry := s.libraryCandidates(context.Background(), req, sreq, p, fp, &tr, false)
	if hit != nil || retry {
		t.Fatalf("PreferredOnly with empty Prefer must not return vault hit: hit=%+v retry=%v", hit, retry)
	}
	if _, err := s.resolve(context.Background(), req, p); err != nil {
		t.Fatal(err)
	}
	if idx.searches.Load() == 0 {
		t.Fatal("expected live search when PreferredOnly is set with empty Prefer")
	}
}
