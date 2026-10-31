package resolver

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestExplicitDubAndMislabelledCamPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, original   string
		prefs            []string
		explicit, reject bool
	}{
		{name: "Primetime (2026) 1080p WEB-DL [Ukr Dub]", original: "en", reject: true},
		{name: "Primetime (2026) WEB DL [RUS Dubbed]", original: "en", reject: true},
		{name: "Primetime (2026) 1080p HQ Pre Multi AAC 2 0 x264", original: "en", reject: true},
		{name: "Primetime.2026.1080p.WEB-DL.HDCAM", original: "en", reject: true},
		{name: "Primetime (2026) 1080p WEB-DL [Ukr Dub]", original: "en", prefs: []string{"uk"}},
		{name: "Primetime (2026) 1080p WEB-DL [Ukr Dub]", original: "uk"},
		{name: "Primetime (2026) 1080p WEB-DL [Ukr Eng Dub]", original: "en"},
		{name: "Primetime (2026) 1080p WEB-DL [Multi Dub]", original: "en"},
		{name: "Primetime (2026) 1080p WEB-DL [Ukr Dub]", original: "en", explicit: true},
		{name: "Cam (2018) 1080p WEB-DL", original: "en"},
		{name: "The Russian Dub (2026) 1080p WEB-DL", original: "en"},
		{name: "Foreign Original (2026) 1080p WEB-DL Russian", original: "ru"},
	} {
		t.Run(fmt.Sprintf("%s/%s/%v/%v", tt.name, tt.original, tt.prefs, tt.explicit), func(t *testing.T) {
			r := core.Release{Name: tt.name, Info: core.ReleaseInfo{Source: core.SourceWEBDL}}
			reason := sourceSuitability(r, &core.Title{Language: tt.original}, core.QualityProfile{PreferredLanguages: tt.prefs}, tt.explicit)
			if (reason != "") != tt.reject {
				t.Fatalf("reason=%q reject=%v", reason, tt.reject)
			}
		})
	}
}

func TestProfileAllowCamIsTheOnlyCamSwitch(t *testing.T) {
	r := core.Release{Name: "Primetime.2026.1080p.WEB-DL.HDCAM", Info: core.ReleaseInfo{Source: core.SourceWEBDL}}
	title := &core.Title{Language: "en"}
	if reason := sourceSuitability(r, title, core.QualityProfile{}, false); reason != "cam not allowed" {
		t.Fatalf("refusing profile reason=%q", reason)
	}
	if reason := sourceSuitability(r, title, core.QualityProfile{AllowCam: true}, false); reason != "" {
		t.Fatalf("allowing profile reason=%q", reason)
	}
}

func TestFeedAndPlaybackRejectForeignDub(t *testing.T) {
	s := New(&fakeIndexer{}, nil, nil, testCfg(), Options{})
	defer s.Close()
	title := matrixTitle()
	title.Language = "en"
	r := rel("dub", "The.Signal.1999.1080p.WEB-DL.[Ukr.Dub]", 4000)
	r.Info = core.ReleaseInfo{Title: "The Signal", Source: core.SourceWEBDL, Height: 1080} // old persisted parsing
	if s.DiscoveryCandidate(title, r) == "" {
		t.Fatal("discovery accepted explicit foreign dub")
	}
	_, reason, err := s.FeedCandidate(t.Context(), title, r)
	if err != nil || reason == "" {
		t.Fatalf("feed reason=%q err=%v", reason, err)
	}
}

func TestKnownUnavailableExpiresAndClearsOnSuccess(t *testing.T) {
	now := time.Now()
	db := store.OpenTest(t)
	s := New(&fakeIndexer{}, nil, db, testCfg(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	s.setAvailability(t.Context(), 42, false, time.Minute)
	keys := []core.RatingKey{42, 43}
	if got := s.KnownUnavailable(t.Context(), keys); !got[42] || got[43] {
		t.Fatal(got)
	}
	// A new service sees the persisted cooldown; no provider call is needed.
	other := New(&fakeIndexer{}, nil, db, testCfg(), Options{Now: func() time.Time { return now }})
	defer other.Close()
	if !other.KnownUnavailable(t.Context(), keys)[42] {
		t.Fatal("restart lost cooldown")
	}
	now = now.Add(2 * time.Minute)
	if len(s.KnownUnavailable(t.Context(), keys)) != 0 {
		t.Fatal("expired cooldown retained")
	}
	s.setAvailability(t.Context(), 42, false, time.Minute)
	s.setAvailability(t.Context(), 42, true, time.Minute)
	if len(s.KnownUnavailable(t.Context(), keys)) != 0 {
		t.Fatal("success did not clear cooldown")
	}
}
