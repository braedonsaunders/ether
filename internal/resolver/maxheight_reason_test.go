package resolver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// derefCeiling flattens a presence-carrying ceiling for assertions
// and failure messages: -1 when absent (unknown), else the value.
func derefCeiling(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// The max-height rejection must name whoever actually limited the
// release: the client's caps or the owner's configured profile (#86).
func TestMaxHeightRejectionNamesLimiter(t *testing.T) {
	s := New(&fakeIndexer{}, nil, nil, testCfg(), Options{})
	defer s.Close()
	capsFor := func(maxHeight int) core.ClientCaps {
		return core.ClientCaps{Product: "test", MaxHeight: maxHeight, VideoCodecs: []string{"h264", "hevc"}}
	}
	profileFor := func(raw, client int) core.QualityProfile {
		q := config.Quality{Default: "p", Profiles: []core.QualityProfile{{Name: "p", MaxHeight: raw}}}
		return SelectProfile(q, capsFor(client), nil)
	}
	r := withParsed(rel("uhd", "The.Signal.1999.2160p.WEB-DL.HEVC-GRP", 8000))

	for _, tc := range []struct {
		name      string
		raw       int // configured profile ceiling
		client    int // caps ceiling
		preferred bool
		want      string
	}{
		{"client caps below profile", 2160, 1080, false, "above client max height"},
		{"profile below client caps", 1080, 2160, false, "above profile max height"},
		{"any profile with 1080 client", 0, 1080, false, "above client max height"},
		{"any profile with 720 client", 0, 720, false, "above client max height"},
		{"tie keeps profile limiter", 1080, 1080, false, "above profile max height"},
		{"preferred keeps the client limiter", 2160, 1080, true, "above client max height"},
		{"preferred keeps the profile limiter", 1080, 2160, true, "above profile max height"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := profileFor(tc.raw, tc.client)
			wantConfigured := tc.raw
			if tc.raw == 0 {
				// "Any" is snapshotted as the unlimited sentinel, apart
				// from an unset field on a hand-built profile.
				wantConfigured = configuredUnlimited
			}
			if profile.ConfiguredMaxHeight != wantConfigured {
				t.Fatalf("configured ceiling=%d want %d (profile %+v)", profile.ConfiguredMaxHeight, wantConfigured, profile)
			}
			if tc.raw == 0 && (profile.MaxHeight != tc.client || traceConfiguredMaxHeight(profile) == nil || *traceConfiguredMaxHeight(profile) != 0) {
				t.Fatalf("any profile effective=%d traced=%d, want %d/0 (Any)",
					profile.MaxHeight, derefCeiling(traceConfiguredMaxHeight(profile)), tc.client)
			}
			got := s.rejectReason(r, matrixTitle(), searchRequestFor(matrixTitle()), profile, s.resolverCfg(), capsFor(tc.client), nil, tc.preferred)
			if got != tc.want {
				t.Fatalf("reason=%q want %q", got, tc.want)
			}
			// An owner pin never passes through the height gates at all.
			if pin := s.pinRejectReason(r, searchRequestFor(matrixTitle()), capsFor(tc.client), nil); pin != "" {
				t.Fatalf("pin reason=%q want bypass", pin)
			}
		})
	}
}

// A resolve records all three ceilings on its trace and history record,
// and history written before they existed still decodes.
func TestTraceAndHistoryRecordMaxHeightCeilings(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	caps := appleTVCaps() // selects the 4K profile (2160 configured)
	caps.MaxHeight = 1080 // ... clamped by this client
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("resolve res=%+v err=%v", res, err)
	}
	traces := svc.Traces()
	if len(traces) == 0 {
		t.Fatal("no trace recorded")
	}
	tr := traces[0]
	if tr.Limits == nil || derefCeiling(tr.Limits.ProfileConfiguredMaxHeight) != 2160 || tr.Limits.ClientMaxHeight != 1080 || tr.Limits.ProfileMaxHeight != 1080 {
		t.Fatalf("trace limits=%+v, want configured/client/effective 2160/1080/1080", tr.Limits)
	}
	for _, c := range tr.Candidates {
		if c.ReleaseID == "uhd" && c.Reason != "above client max height" {
			t.Fatalf("uhd reason=%q want client limiter", c.Reason)
		}
	}
	records, total, err := svc.History(context.Background(), HistoryFilter{})
	if err != nil || total != 1 || len(records) != 1 {
		t.Fatalf("history records=%v total=%d err=%v", records, total, err)
	}
	if h := records[0]; derefCeiling(h.ProfileMaxHeight) != 2160 || h.ClientMaxHeight != 1080 || h.EffectiveMaxHeight != 1080 {
		t.Fatalf("history ceilings configured=%d client=%d effective=%d, want 2160/1080/1080",
			derefCeiling(h.ProfileMaxHeight), h.ClientMaxHeight, h.EffectiveMaxHeight)
	}
	// Legacy payloads (no ceiling fields) decode with zero values.
	var old Trace
	if err := json.Unmarshal([]byte(`{"profile":"1080p"}`), &old); err != nil {
		t.Fatal(err)
	}
	if h := historyFromTrace(old); h.ProfileMaxHeight != nil || h.ClientMaxHeight != 0 || h.EffectiveMaxHeight != 0 {
		t.Fatalf("legacy history ceilings=%+v want absent/zero", h)
	}
}

// A remote client must be resolved under q.Remote, not q.Default, and
// the trace/history must name that remote profile and its own ceiling
// as the limiter when it is the distinct, lower ceiling (#86).
func TestRemoteProfileCeilingRecordedInHistory(t *testing.T) {
	cfg := testCfg()
	q := fixtureQuality()
	q.Default = "4K"  // 2160 configured for local/default clients
	q.Remote = "720p" // 720 configured for remote clients: distinct ceiling
	q.Rules = nil     // a device rule would win over the remote profile
	cfg.Quality = q

	// Direct selection: Remote picks q.Remote and snapshots its ceiling.
	caps := appleTVCaps() // client itself allows 2160
	caps.Remote = true
	if p := SelectProfile(cfg.Quality, caps, nil); p.Name != "720p" || p.ConfiguredMaxHeight != 720 || p.MaxHeight != 720 {
		t.Fatalf("remote profile name=%q configured=%d effective=%d, want 720p/720/720", p.Name, p.ConfiguredMaxHeight, p.MaxHeight)
	}
	// Contrast: the same caps without Remote pick the Default profile.
	local := caps
	local.Remote = false
	if p := SelectProfile(cfg.Quality, local, nil); p.Name != "4K" || p.ConfiguredMaxHeight != 2160 {
		t.Fatalf("local profile name=%q configured=%d, want 4K/2160", p.Name, p.ConfiguredMaxHeight)
	}

	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("remote resolve res=%+v err=%v, want sd720 winner under the 720 remote ceiling", res, err)
	}
	traces := svc.Traces()
	if len(traces) == 0 {
		t.Fatal("no trace recorded")
	}
	tr := traces[0]
	if tr.Profile != "720p" {
		t.Fatalf("trace profile=%q want remote profile %q", tr.Profile, "720p")
	}
	if tr.Limits == nil || derefCeiling(tr.Limits.ProfileConfiguredMaxHeight) != 720 || tr.Limits.ClientMaxHeight != 2160 || tr.Limits.ProfileMaxHeight != 720 {
		t.Fatalf("remote trace limits=%+v, want configured/client/effective 720/2160/720", tr.Limits)
	}
	for _, id := range []string{"uhd", "hd"} {
		found := false
		for _, c := range tr.Candidates {
			if c.ReleaseID == id {
				found = true
				if c.Reason != "above profile max height" {
					t.Fatalf("%s reason=%q want profile limiter (remote ceiling)", id, c.Reason)
				}
			}
		}
		if !found {
			t.Fatalf("candidate %s missing from trace", id)
		}
	}
	records, total, err := svc.History(context.Background(), HistoryFilter{})
	if err != nil || total != 1 || len(records) != 1 {
		t.Fatalf("history records=%v total=%d err=%v", records, total, err)
	}
	if h := records[0]; h.Profile != "720p" || derefCeiling(h.ProfileMaxHeight) != 720 || h.ClientMaxHeight != 2160 || h.EffectiveMaxHeight != 720 {
		t.Fatalf("remote history profile=%q ceilings configured=%d client=%d effective=%d, want 720p/720/2160/720",
			h.Profile, derefCeiling(h.ProfileMaxHeight), h.ClientMaxHeight, h.EffectiveMaxHeight)
	}
}

// A download resolve reshapes the 4K profile down to 1080p, but the
// trace must still record the owner's configured 2160 ceiling: the
// snapshot is taken before downloadProfile runs (#86).
func TestDownloadResolveRecordsConfiguredCeiling(t *testing.T) {
	if p := SelectProfile(fixtureQuality(), func() core.ClientCaps {
		c := appleTVCaps()
		c.Download = true
		return c
	}(), nil); p.ConfiguredMaxHeight != 2160 || p.MaxHeight != 1080 {
		t.Fatalf("download profile configured=%d effective=%d, want 2160/1080", p.ConfiguredMaxHeight, p.MaxHeight)
	}

	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	caps := appleTVCaps()
	caps.Download = true
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("download resolve res=%+v err=%v", res, err)
	}
	tr := svc.Traces()[0]
	if tr.Limits == nil || derefCeiling(tr.Limits.ProfileConfiguredMaxHeight) != 2160 || tr.Limits.ClientMaxHeight != 2160 || tr.Limits.ProfileMaxHeight != 1080 {
		t.Fatalf("download trace limits=%+v, want configured/client/effective 2160/2160/1080", tr.Limits)
	}
	for _, c := range tr.Candidates {
		if c.ReleaseID == "uhd" && c.Reason != "above profile max height" {
			t.Fatalf("uhd download reason=%q want profile limiter (download variant)", c.Reason)
		}
	}
}

// The console download path builds its profile without SelectProfile;
// it snapshots the configured ceiling itself.
func TestConsoleDownloadProfileRecordsConfiguredCeiling(t *testing.T) {
	cfg := testCfg()
	cfg.Quality.Default = "4K"
	svc := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer svc.Close()
	p, caps, err := svc.consoleProfile(ProfileDownload)
	if err != nil || !caps.Download {
		t.Fatalf("console download caps=%+v err=%v", caps, err)
	}
	if p.ConfiguredMaxHeight != 2160 || p.MaxHeight != 1080 {
		t.Fatalf("console download configured=%d effective=%d, want 2160/1080", p.ConfiguredMaxHeight, p.MaxHeight)
	}

	cfg.Quality.Default = "any"
	cfg.Quality.Profiles = []core.QualityProfile{{Name: "any"}}
	svc2 := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer svc2.Close()
	p, _, err = svc2.consoleProfile(ProfileDownload)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfiguredMaxHeight != configuredUnlimited || p.MaxHeight != 1080 {
		t.Fatalf("console any-download configured=%d effective=%d, want %d/1080", p.ConfiguredMaxHeight, p.MaxHeight, configuredUnlimited)
	}
}

// The DB-backed listing must surface the ceilings stored in the trace
// JSON, even though History returns no traces; rows written before the
// ceilings existed (or with no trace at all) list as zero, without error.
func TestHistoryFromDatabaseExposesCeilings(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{})
	defer svc.Close()
	caps := appleTVCaps() // selects the 4K profile (2160 configured)
	caps.MaxHeight = 1080 // ... clamped by this client
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: caps}); err != nil {
		t.Fatal(err)
	}
	legacyAt := time.Now().Add(time.Hour).Unix()
	for _, row := range []struct{ profile, trace string }{
		{"legacy", `{"profile":"legacy"}`},
		{"empty", ""},
	} {
		legacyAt++
		if _, err := db.ExecContext(ctx,
			`INSERT INTO resolver_history(resolved_at, rating_key, title_name, profile, trace) VALUES (?,?,?,?,?)`,
			legacyAt, int64(matrixTitle().Key), "Legacy", row.profile, row.trace); err != nil {
			t.Fatal(err)
		}
	}
	records, total, err := svc.History(ctx, HistoryFilter{})
	if err != nil || total != 3 || len(records) != 3 {
		t.Fatalf("history records=%v total=%d err=%v", records, total, err)
	}
	byProfile := map[string]HistoryRecord{}
	for _, h := range records {
		if h.Trace != nil {
			t.Fatalf("listing must stay trace-free: %+v", h)
		}
		byProfile[h.Profile] = h
	}
	if h := byProfile["4K"]; derefCeiling(h.ProfileMaxHeight) != 2160 || h.ClientMaxHeight != 1080 || h.EffectiveMaxHeight != 1080 {
		t.Fatalf("db history ceilings configured=%d client=%d effective=%d, want 2160/1080/1080",
			derefCeiling(h.ProfileMaxHeight), h.ClientMaxHeight, h.EffectiveMaxHeight)
	}
	for _, name := range []string{"legacy", "empty"} {
		if h := byProfile[name]; h.ProfileMaxHeight != nil || h.ClientMaxHeight != 0 || h.EffectiveMaxHeight != 0 {
			t.Fatalf("%s history ceilings=%+v want absent/zero", name, h)
		}
	}
}

// The real pre-PR history shape has effective/client ceilings but no
// profileConfiguredMaxHeight: the configured ceiling was never
// recorded, so it must stay absent (unknown), never collapse to 0/Any.
func TestLegacyTraceShapeHasUnknownConfiguredCeiling(t *testing.T) {
	var tr Trace
	if err := json.Unmarshal([]byte(`{"limits":{"profileMinHeight":1080,"profileMaxHeight":1080,"clientMaxHeight":1080,"clientMaxBitrateKbps":0}}`), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Limits == nil || tr.Limits.ProfileConfiguredMaxHeight != nil {
		t.Fatalf("legacy trace configured=%v, want absent (nil)", tr.Limits)
	}
	h := historyFromTrace(tr)
	if h.ProfileMaxHeight != nil || h.ClientMaxHeight != 1080 || h.EffectiveMaxHeight != 1080 {
		t.Fatalf("legacy history configured=%d client=%d effective=%d, want absent/1080/1080",
			derefCeiling(h.ProfileMaxHeight), h.ClientMaxHeight, h.EffectiveMaxHeight)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"profileMaxHeight"`) {
		t.Fatalf("legacy history JSON %s must omit profileMaxHeight (unknown, not Any)", raw)
	}
}

// A newly resolved explicitly unlimited ("Any") profile records its
// configured ceiling as present 0 — distinguishable from legacy
// unknown, in the trace, its JSON and the history record.
func TestExplicitlyUnlimitedProfileRecordsPresentZeroConfiguredCeiling(t *testing.T) {
	cfg := testCfg()
	cfg.Quality = config.Quality{Default: "any", Profiles: []core.QualityProfile{{Name: "any"}}}
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	caps := appleTVCaps()
	caps.MaxHeight = 1080
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("any resolve res=%+v err=%v", res, err)
	}
	tr := svc.Traces()[0]
	if tr.Limits == nil || tr.Limits.ProfileConfiguredMaxHeight == nil || *tr.Limits.ProfileConfiguredMaxHeight != 0 {
		t.Fatalf("any trace configured=%d, want present 0 (explicit Any)", derefCeiling(tr.Limits.ProfileConfiguredMaxHeight))
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"profileConfiguredMaxHeight":0`) {
		t.Fatalf("any trace JSON %s, want explicit profileConfiguredMaxHeight:0", raw)
	}
	records, total, err := svc.History(context.Background(), HistoryFilter{})
	if err != nil || total != 1 || len(records) != 1 {
		t.Fatalf("history records=%v total=%d err=%v", records, total, err)
	}
	if h := records[0]; h.ProfileMaxHeight == nil || *h.ProfileMaxHeight != 0 || h.ClientMaxHeight != 1080 || h.EffectiveMaxHeight != 1080 {
		t.Fatalf("any history configured=%d client=%d effective=%d, want present 0/1080/1080",
			derefCeiling(h.ProfileMaxHeight), h.ClientMaxHeight, h.EffectiveMaxHeight)
	}
}
