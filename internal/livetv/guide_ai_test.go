package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type guideFake struct {
	calls   int
	invalid string
	labels  map[string]string
}

func (f *guideFake) Enabled() bool { return true }
func (f *guideFake) ChatGuide(ctx context.Context, system, input string) (string, error) {
	f.calls++
	if strings.Contains(input, "owner.invalid") || strings.Contains(input, "password") {
		return "", fmt.Errorf("private input leaked")
	}
	var items []aiItem
	if e := json.Unmarshal([]byte(input), &items); e != nil {
		return "", e
	}
	out := []aiDecision{}
	for _, item := range items {
		d := aiDecision{Channel: item.ID, Confidence: .98, Reason: "Station identity and region agree"}
		for _, g := range item.Candidates {
			if g.Name == f.labels[item.ID] {
				d.Candidate = g.ID
				break
			}
		}
		if f.invalid != "" {
			d.Candidate = f.invalid
		}
		out = append(out, d)
	}
	b, _ := json.Marshal(map[string]any{"decisions": out})
	return string(b), nil
}

// Real station brands and distributor-style noise, with independently labelled
// identities. This measures retrieval/protocol behavior with an oracle fake,
// not the semantic accuracy of a live model.
var messyStations = []string{"ESPN 2", "ESPN", "Fox Sports 1", "Fox Sports 2", "NBC Sports", "CBS Sports Network", "NFL Network", "NBA TV", "MLB Network", "NHL Network", "Golf Channel", "Tennis Channel", "CNN", "CNN International", "BBC News", "Sky News", "Fox News", "MSNBC", "CNBC", "Bloomberg", "ABC", "CBS", "NBC", "FOX", "PBS", "The CW", "USA Network", "TNT", "TBS", "AMC", "FX", "FXX", "Syfy", "Bravo", "E!", "A&E", "History", "Discovery", "National Geographic", "Animal Planet", "TLC", "Food Network", "Cooking Channel", "HGTV", "Travel Channel", "Weather Channel", "Cartoon Network", "Nickelodeon", "Disney Channel", "Disney Junior", "Disney XD", "Boomerang", "Comedy Central", "Paramount Network", "MTV", "VH1", "CMT", "BET", "Hallmark Channel", "Lifetime", "Oxygen", "Investigation Discovery", "Science Channel", "MotorTrend"}

func seedGuideAI(t *testing.T, f *guideFake, count int) *Service {
	t.Helper()
	ctx := context.Background()
	s := testService(t, Options{GuideAI: f, Now: func() time.Time { return guideTestNow }})
	var xmlChannels []string
	regions := []string{"US", "CA", "UK", "AU", "NZ", "DE", "FR", "IT", "ES", "BR"}
	for _, region := range regions {
		for n, name := range messyStations {
			for v, suffix := range []string{" HD", " SD", " +1", " +2", " International"} {
				xmlChannels = append(xmlChannels, fmt.Sprintf("%s-%d-%d=%s|%s", region, n, v, region+": "+name+suffix, region+": "+name+suffix))
			}
		}
	}
	if _, e := s.ImportSource(ctx, Source{ID: "guide", Name: "World XMLTV", Guide: strings.ReplaceAll(xmltvFixture(3, xmlChannels...), "&", "&amp;")}); e != nil {
		t.Fatal(e)
	}
	tx, e := s.o.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for i := 0; i < count; i++ {
		n := i % len(messyStations)
		name := messyStations[n]
		id := fmt.Sprintf("iptv-%03d", i)
		region := regions[(i/len(messyStations))%len(regions)]
		guideID := ""
		display := fmt.Sprintf("%s| %s HD ᴿᴬᵂ", region, name)
		switch i % 5 {
		case 0:
			guideID = fmt.Sprintf("%s-%d-0", region, n)
			display = name
		case 1:
			display = fmt.Sprintf("%s: %s FHD [VIP]", region, name)
		case 2:
			display = fmt.Sprintf("%s | %s HD backup", region, name)
		case 3:
			display = fmt.Sprintf("%s: %s HEVC", region, name)
		}
		c := Channel{ID: id, Name: display, Kind: "iptv", Enabled: true, SourceID: "playlist", GuideID: guideID, StreamURL: "https://owner.invalid/password/stream"}
		b, _ := json.Marshal(c)
		if _, e = tx.Exec(`INSERT INTO livetv_channels(id,data) VALUES(?,?)`, id, string(b)); e != nil {
			t.Fatal(e)
		}
		f.labels[id] = region + ": " + name + " HD"
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestGuideAIMessyMeasurement(t *testing.T) {
	if raceEnabled {
		// Keep the complete fixture without charging race instrumentation against
		// the production matcher's wall-clock deadline. Normal measurements use
		// real time; the offline fake client never needs an actual timeout here.
		synctest.Test(t, guideAIMessyMeasurement)
		return
	}
	guideAIMessyMeasurement(t)
}

func guideAIMessyMeasurement(t *testing.T) {
	ctx := context.Background()
	f := &guideFake{labels: map[string]string{}}
	s := seedGuideAI(t, f, 640)
	deterministic, e := s.MatchGuides(ctx, GuideMatchRequest{DryRun: true})
	if e != nil {
		t.Fatal(e)
	}
	estimate, e := s.MatchGuides(ctx, GuideMatchRequest{AI: "estimate"})
	if e != nil || f.calls != 0 {
		t.Fatalf("estimate: %+v %v calls %d", estimate, e, f.calls)
	}

	result, e := s.MatchGuides(ctx, GuideMatchRequest{AI: "propose"})
	if e != nil {
		t.Fatal(e)
	}
	added, correct := 0, 0
	for _, c := range result.Changes {
		if c.Method == "ai" {
			added++
			if c.GuideName == f.labels[c.ChannelID] {
				correct++
			}
		}
	}
	if added == 0 || correct != added {
		t.Fatalf("AI precision %d/%d", correct, added)
	}
	calls := f.calls
	cached, e := s.MatchGuides(ctx, GuideMatchRequest{AI: "propose"})
	if e != nil {
		t.Fatal(e)
	}
	// A second run may process the deferred tail. Fully cached runs must be free.
	for cached.AI.Deferred > 0 {
		cached, e = s.MatchGuides(ctx, GuideMatchRequest{AI: "propose"})
		if e != nil {
			t.Fatal(e)
		}
	}
	calls = f.calls
	_, e = s.MatchGuides(ctx, GuideMatchRequest{AI: "propose"})
	if e != nil || f.calls != calls {
		t.Fatalf("cache rerun: %v calls %d -> %d", e, calls, f.calls)
	}
	t.Logf("MEASUREMENT channels=640 XMLTV=3200 deterministic=%d (%.2f%%) AI_added_first_run=%d labelled_precision=%d/%d first_run_calls=%d elapsed=%s deferred=%d cached_rerun_calls=0 total_AI=%d", deterministic.Matched, float64(deterministic.Matched)*100/640, added, correct, added, result.AI.Calls, time.Duration(result.AI.ElapsedMS)*time.Millisecond, result.AI.Deferred, cached.Matched-deterministic.Matched)
	// No proposal changed the mapping. Apply only one chosen mapping, from cache.
	for _, p := range result.Changes {
		if p.Method != "ai" {
			continue
		}
		c, e := s.Channel(ctx, p.ChannelID)
		if e != nil {
			t.Fatal(e)
		}
		if c.GuideID == p.GuideID {
			t.Fatal("proposal wrote mapping")
		}
		req := GuideMatchRequest{AI: "apply", Reviewed: true, ChannelIDs: []string{p.ChannelID}, ProposalKeys: map[string]string{p.ChannelID: p.ProposalKey}}
		applied, e := s.MatchGuides(ctx, req)
		if e != nil || applied.Matched != 1 {
			t.Fatalf("apply %+v %v", applied, e)
		}
		break
	}
}
func TestGuideAIRejectsInventedIDs(t *testing.T) {
	f := &guideFake{labels: map[string]string{}, invalid: "invented"}
	s := seedGuideAI(t, f, 2)
	// An invented id is dropped; it never maps a channel or stops the run.
	res, e := s.MatchGuides(context.Background(), GuideMatchRequest{AI: "propose"})
	if e != nil {
		t.Fatalf("invented candidate stopped the run: %v", e)
	}
	for _, c := range res.Changes {
		if c.Method == "ai" {
			t.Fatalf("invented candidate proposed: %+v", c)
		}
	}
}

func TestGuideAISemanticGuards(t *testing.T) {
	c := Channel{Name: "US: ESPN 2 +1 HD"}
	guides := []*guideCandidate{{source: "s", guide: "a", name: "US: ESPN 2 HD", region: "US"}, {source: "s", guide: "b", name: "CA: ESPN 2 +1 HD", region: "CA"}, {source: "s", guide: "c", name: "US: ESPN 2 +1 HD", region: "US"}}
	list := aiShortlist(c, guides, nil)
	for _, g := range list {
		if g.guide != "c" {
			t.Fatalf("region/timeshift leak: %+v", g)
		}
	}
	if len(list) != 1 {
		t.Fatalf("expected exact shifted service, got %d", len(list))
	}
}
func TestGuideAIRejectsStaleReview(t *testing.T) {
	f := &guideFake{labels: map[string]string{}}
	s := seedGuideAI(t, f, 2)
	ctx := context.Background()
	r, e := s.MatchGuides(ctx, GuideMatchRequest{AI: "propose"})
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range r.Changes {
		if p.Method != "ai" {
			continue
		}
		c, e := s.Channel(ctx, p.ChannelID)
		if e != nil {
			t.Fatal(e)
		}
		c.Name = "A different station"
		b, _ := json.Marshal(c)
		if _, e = s.o.DB.Exec(`UPDATE livetv_channels SET data=? WHERE id=?`, b, c.ID); e != nil {
			t.Fatal(e)
		}
		_, e = s.MatchGuides(ctx, GuideMatchRequest{AI: "apply", Reviewed: true, ChannelIDs: []string{c.ID}, ProposalKeys: map[string]string{c.ID: p.ProposalKey}})
		if e == nil {
			t.Fatal("stale proposal applied")
		}
		return
	}
	t.Fatal("missing AI proposal")
}

func TestGuideAIUnconfiguredKeepsDeterministic(t *testing.T) {
	f := &guideFake{labels: map[string]string{}}
	s := seedGuideAI(t, f, 2)
	s.o.GuideAI = nil
	r, e := s.MatchGuides(context.Background(), GuideMatchRequest{AI: "estimate"})
	if e != nil {
		t.Fatal(e)
	}
	if r.AI.Configured || r.Matched != 1 || f.calls != 0 {
		t.Fatalf("unconfigured estimate %+v", r)
	}
}
