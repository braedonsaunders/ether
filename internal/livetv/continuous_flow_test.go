package livetv

import (
	"bytes"
	"context"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type flowClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFlowClock(t time.Time) *flowClock { return &flowClock{t: t} }
func (c *flowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *flowClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type warmCall struct {
	key    core.RatingKey
	offset time.Duration
}

type fakeContinuousRunner struct {
	mu       sync.Mutex
	decoders []transcode.DecoderRequest
}

func (f *fakeContinuousRunner) StartDecoder(_ context.Context, req transcode.DecoderRequest) (transcode.Decoder, error) {
	f.mu.Lock()
	f.decoders = append(f.decoders, req)
	f.mu.Unlock()
	return &fakeDecoder{}, nil
}

func (f *fakeContinuousRunner) StartEncoder(_ context.Context, _ transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	return nil, core.ErrNotAvailable
}

type fakeDecoder struct{}

func (d *fakeDecoder) Next(context.Context) (transcode.RawFrame, error) {
	return transcode.RawFrame{}, core.ErrNotAvailable
}
func (d *fakeDecoder) Close() error { return nil }

func TestEnabledVirtualChannelsWithNoViewersDoZeroRendersAndZeroWarming(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cacheDir := t.TempDir()
	clock := newFlowClock(time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC))

	var masterRenders atomic.Int64
	var liveFrameRenders atomic.Int64
	var warmCalls atomic.Int64

	var ltv *Service
	oa, err := onair.New(onair.Options{
		DB:       db,
		Dir:      cacheDir,
		Showcase: true,
		Now:      clock.Now,
		Upcoming: func(c context.Context) []core.ClipRequest {
			if ltv == nil {
				return nil
			}
			return ltv.GraphicsRequests(c, "")
		},
		Render: func(_ context.Context, _ scene.Document, _ raster.Bindings, out string) error {
			masterRenders.Add(1)
			return os.WriteFile(out, []byte("mp4"), 0600)
		},
		RenderFrame: func(doc scene.Document, b raster.Bindings, ts float64, w, h int) *image.RGBA {
			liveFrameRenders.Add(1)
			return raster.Render(doc, b, ts, w, h)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oa.Start()
	defer oa.Close()

	reels, err := oa.Reels(ctx)
	if err != nil || len(reels) == 0 {
		t.Fatalf("expected seeded showcase reels, got len=%d err=%v", len(reels), err)
	}
	showcaseReelID := reels[0].ID

	ltv, err = New(Options{
		DB:       db,
		Graphics: oa,
		Now:      clock.Now,
		Warm: func(_ context.Context, _ core.RatingKey, _ time.Duration) error {
			warmCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ltv.Close()

	bp, _ := EraBreakPattern("1980s")
	tonightOn := true
	now := clock.Now()
	for _, id := range []string{"chroma-80", "retro-90"} {
		ch := Channel{
			ID:       id,
			Name:     strings.ToUpper(id),
			Number:   "80",
			Kind:     "virtual",
			Enabled:  true,
			Mode:     "order",
			Timezone: "UTC",
			Epoch:    now.Add(-10 * time.Minute),
			Seed:     42,
			Items: []Item{
				{Key: 101, Title: "Show One", DurationMS: 12 * 60 * 1000, BreakPointsMS: []int64{6 * 60 * 1000}},
				{Key: 102, Title: "Show Two", DurationMS: 20 * 60 * 1000},
			},
			Look: &ChannelLook{
				LookID:       "nocturne",
				Ident:        true,
				UpNext:       true,
				Tonight:      &tonightOn,
				OffAir:       true,
				Breaks:       "between",
				BreakContent: "reel",
				ReelID:       showcaseReelID,
				Placement:    "insert",
				BreakPattern: &bp,
			},
		}
		if id == "retro-90" {
			ch.Number = "90"
		}
		if _, err := ltv.SaveChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}

	// Exercise idle server activity: guide browsing, maintenance passes, upcoming scans.
	ltv.NoteClientActivity()
	if reqs := ltv.GraphicsRequests(ctx, ""); len(reqs) != 0 {
		t.Fatalf("expected 0 graphics requests when enabled channels have NO viewers, got %d", len(reqs))
	}
	ltv.ensureGraphics()
	ltv.warmProgrammes()
	oa.WakeUpcoming()
	time.Sleep(150 * time.Millisecond)

	// EXPLICIT OWNER ASSERTION: zero renders and zero warming when nobody is watching.
	if got := masterRenders.Load(); got != 0 {
		t.Fatalf("expected 0 master renders with no active viewers, got %d", got)
	}
	if got := liveFrameRenders.Load(); got != 0 {
		t.Fatalf("expected 0 live frame renders with no active viewers, got %d", got)
	}
	if got := warmCalls.Load(); got != 0 {
		t.Fatalf("expected 0 content warm calls with no active viewers, got %d", got)
	}
	if got := oa.Visibility().MastersRendered; got != 0 {
		t.Fatalf("expected onair MastersRendered == 0, got %d", got)
	}
	if got := ltv.BroadcastVisibility().WarmCount; got != 0 {
		t.Fatalf("expected livetv WarmCount == 0, got %d", got)
	}

	// Now attach an active viewer ONLY to chroma-80; retro-90 stays enabled with 0 viewers.
	ltv.mu.Lock()
	ltv.sessions["sess-1"] = &session{info: SessionInfo{ID: "sess-1", Channel: Channel{ID: "chroma-80"}}}
	ltv.noteWatchedLocked("chroma-80")
	ltv.mu.Unlock()

	reqs := ltv.GraphicsRequests(ctx, "")
	if len(reqs) == 0 {
		t.Fatalf("expected demand-driven graphics requests once chroma-80 has an active viewer")
	}
	for _, r := range reqs {
		if r.ChannelID != "chroma-80" {
			t.Fatalf("unwatched channel %q leaked into GraphicsRequests: %+v", r.ChannelID, r)
		}
	}
	// Verify nearest non-zero boundary sorts ahead of zero-time background look masters.
	seenZero := false
	var prevBoundary time.Time
	for _, r := range reqs {
		if r.Boundary.IsZero() {
			seenZero = true
			continue
		}
		if seenZero {
			t.Fatalf("timed boundary %v sorted after zero-time reusable master", r.Boundary)
		}
		if !prevBoundary.IsZero() && r.Boundary.Before(prevBoundary) {
			t.Fatalf("boundaries not sorted nearest-first: %v before %v", r.Boundary, prevBoundary)
		}
		prevBoundary = r.Boundary
	}

	// When the viewer disconnects, active demand clears immediately.
	ltv.mu.Lock()
	delete(ltv.sessions, "sess-1")
	ltv.mu.Unlock()
	ltv.onChannelSessionClosed("chroma-80")

	if reqsAfter := ltv.GraphicsRequests(ctx, ""); len(reqsAfter) != 0 {
		t.Fatalf("expected 0 graphics requests immediately after last viewer disconnects, got %d", len(reqsAfter))
	}
}

func TestConfigurableRundownAndFirstClassLineupAroundBreaks(t *testing.T) {
	// 11:00 AM daytime boundary to verify first-class lineup runs before 18:00 with "COMING UP".
	at := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	bp, _ := EraBreakPattern("1980s")
	bp.Spots = 2
	bp.MinSpots = 1
	bp.MinSeconds = 30
	bp.Seconds = 90
	bp.MaxSeconds = 180

	tonightOn := true
	reel := &ReelSnapshot{
		ID:     "ads-80s",
		Anchor: at.Add(-time.Hour),
		Clips: []ReelClip{
			{ID: "spot-1", Name: "Spot 1", Duration: 30 * time.Second, Path: "/tmp/spot1.mp4"},
			{ID: "spot-2", Name: "Spot 2", Duration: 30 * time.Second, Path: "/tmp/spot2.mp4"},
		},
	}

	// 1. Default between-programme pod order: coming-up/upnext -> break -> spots -> back -> ident
	// (no post-break 4-moment pileup after "back"!).
	defChan := Channel{
		ID:           "chroma-default",
		Name:         "CHROMA 80",
		Kind:         "virtual",
		Mode:         "order",
		Timezone:     "UTC",
		Epoch:        at,
		Seed:         7,
		clipDuration: testGraphics{}.Duration,
		reel:         reel,
		Items: []Item{
			{Key: 1, Title: "Morning Show", DurationMS: 15 * 60 * 1000},
			{Key: 2, Title: "Noon Feature", DurationMS: 20 * 60 * 1000},
			{Key: 3, Title: "Afternoon Sci-Fi", DurationMS: 25 * 60 * 1000},
			{Key: 4, Title: "Early News", DurationMS: 30 * 60 * 1000},
		},
		Look: &ChannelLook{
			LookID:       "retro",
			Ident:        true,
			UpNext:       true,
			Tonight:      &tonightOn,
			Breaks:       "between",
			BreakContent: "reel",
			ReelID:       reel.ID,
			Placement:    "insert",
			BreakPattern: &bp,
		},
	}
	defChan.momentHistory = &momentHistory{last: map[string]time.Time{}}
	defPod := defChan.momentsWithin(at, 3*time.Minute)
	var defSlates []string
	backIdx, spotIdx, upnextIdx, tonightIdx := -1, -1, -1, -1
	for i, it := range defPod {
		s := it.Slate
		if it.ReelClipID != "" {
			s = "spot"
			if spotIdx < 0 {
				spotIdx = i
			}
		}
		switch s {
		case "tonight":
			tonightIdx = i
		case "upnext":
			upnextIdx = i
		case "back":
			backIdx = i
		}
		defSlates = append(defSlates, s)
	}
	if tonightIdx < 0 || upnextIdx < 0 || spotIdx < 0 || backIdx < 0 {
		t.Fatalf("expected tonight, upnext, spot, and back in default pod, got %v", defSlates)
	}
	if !(tonightIdx < spotIdx && upnextIdx < spotIdx && spotIdx < backIdx) {
		t.Fatalf("expected lineup/upnext BEFORE spots and back AFTER spots, got %v", defSlates)
	}

	// 2. Custom per-channel rundown around breaks: BeforeBreak=["tonight", "break"], AfterBreak=["back"].
	customChan := defChan
	customLook := *defChan.Look
	customLook.BeforeBreak = []string{"tonight", "break"}
	customLook.AfterBreak = []string{"back"}
	customChan.Look = &customLook
	customChan.momentHistory = &momentHistory{last: map[string]time.Time{}}

	customPod := customChan.momentsWithin(at, 3*time.Minute)
	if len(customPod) < 3 {
		t.Fatalf("expected custom pod items, got %v", customPod)
	}
	if customPod[0].Slate != "tonight" || customPod[1].Slate != "break" {
		t.Fatalf("expected custom BeforeBreak [tonight, break], got [%s, %s]", customPod[0].Slate, customPod[1].Slate)
	}
	if customPod[len(customPod)-1].Slate != "back" {
		t.Fatalf("expected custom AfterBreak to end right on 'back' -> show, got %s", customPod[len(customPod)-1].Slate)
	}

	// 3. Verify clipRequest populates "COMING UP" before 18:00 and "TONIGHT" from 18:00, plus 3-4 upcoming titles.
	svc := &Service{
		o:             Options{Graphics: testGraphics{cached: true}, Now: func() time.Time { return at }},
		reelSnapshots: map[string]*ReelSnapshot{reel.ID: reel},
	}
	dayProg := Programme{Start: at, End: at.Add(8 * time.Second), Item: Item{Slate: "tonight", DurationMS: 8000}}
	dayReq := svc.clipRequest(customChan, dayProg)
	if dayReq.Bind["lineup.heading"] != "COMING UP" {
		t.Fatalf("expected daytime lineup.heading 'COMING UP', got %q", dayReq.Bind["lineup.heading"])
	}
	if dayReq.Bind["list[0].title"] == "" || dayReq.Bind["list[1].title"] == "" || dayReq.Bind["list[2].title"] == "" {
		t.Fatalf("expected next 3-4 programmes in lineup bindings, got %+v", dayReq.Bind)
	}

	eveAt := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	eveProg := Programme{Start: eveAt, End: eveAt.Add(8 * time.Second), Item: Item{Slate: "tonight", DurationMS: 8000}}
	eveReq := svc.clipRequest(customChan, eveProg)
	if eveReq.Bind["lineup.heading"] != "TONIGHT" {
		t.Fatalf("expected evening lineup.heading 'TONIGHT', got %q", eveReq.Bind["lineup.heading"])
	}
}

func TestBreakReturnWarmingNextPodClipResolutionAndTimeoutSubstitution(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	epoch := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	clock := newFlowClock(epoch)

	var warmMu sync.Mutex
	var warms []warmCall

	oa, err := onair.New(onair.Options{
		DB:       db,
		Dir:      t.TempDir(),
		Showcase: true,
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer oa.Close()

	reels, err := oa.Reels(ctx)
	if err != nil || len(reels) == 0 {
		t.Fatalf("expected showcase reels, got %v", err)
	}
	reelID := reels[0].ID

	ltv, err := New(Options{
		DB:       db,
		Graphics: oa,
		Now:      clock.Now,
		Warm: func(_ context.Context, key core.RatingKey, offset time.Duration) error {
			warmMu.Lock()
			warms = append(warms, warmCall{key: key, offset: offset})
			warmMu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ltv.Close()

	bp, _ := EraBreakPattern("1980s")
	bp.ClockAlignment = "none"
	bp.Spots = 2
	bp.MinSpots = 1
	bp.MinSeconds = 40
	bp.Seconds = 80
	bp.MaxSeconds = 120

	ch := Channel{
		ID:       "chroma-80",
		Name:     "CHROMA 80",
		Number:   "80",
		Kind:     "virtual",
		Enabled:  true,
		Mode:     "order",
		Timezone: "UTC",
		Epoch:    epoch,
		Seed:     19,
		Items: []Item{
			{
				Key:           501,
				Title:         "Interrupted Feature",
				DurationMS:    22 * 60 * 1000,
				BreakPointsMS: []int64{7 * 60 * 1000}, // mid-show break at +7m00s!
			},
			{
				Key:        502,
				Title:      "Next Hour Show",
				DurationMS: 25 * 60 * 1000,
			},
		},
		Look: &ChannelLook{
			LookID:       "nocturne",
			BreakContent: "reel",
			ReelID:       reelID,
			Placement:    "insert",
			BreakPattern: &bp,
		},
	}
	if _, err := ltv.SaveChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}

	// Reel snapshot with 3 spots: spot-timeout has NO local path (simulating timed-out archive.org link),
	// while spot-cached-a and spot-cached-b are already durably cached on disk!
	cachedSpotA := filepath.Join(t.TempDir(), "spot-a.mp4")
	cachedSpotB := filepath.Join(t.TempDir(), "spot-b.mp4")
	_ = os.WriteFile(cachedSpotA, []byte("mp4-a"), 0600)
	_ = os.WriteFile(cachedSpotB, []byte("mp4-b"), 0600)
	reel := &ReelSnapshot{
		ID:     reelID,
		Anchor: epoch,
		Clips: []ReelClip{
			{ID: "spot-timeout", Name: "Timed Out Spot", Duration: 30 * time.Second, Path: ""},
			{ID: "spot-cached-a", Name: "Cached Spot A", Duration: 30 * time.Second, Path: cachedSpotA},
			{ID: "spot-cached-b", Name: "Cached Spot B", Duration: 30 * time.Second, Path: cachedSpotB},
		},
	}
	// Let any SaveChannel refresh settle, then install our deterministic reel snapshot.
	time.Sleep(50 * time.Millisecond)
	ltv.mu.Lock()
	ltv.reelSnapshots = map[string]*ReelSnapshot{reelID: reel}
	ltv.mu.Unlock()

	fullSched := Schedule(ltv.graphicsChannel(ch), epoch, epoch.Add(45*time.Minute))
	var part1Prog, midBreakItem, returnProg *Programme
	for i := range fullSched {
		p := fullSched[i]
		if part1Prog == nil && p.Item.Key == 501 && p.sourceOffset == 0 {
			cp := fullSched[i]
			part1Prog = &cp
			continue
		}
		if part1Prog != nil && midBreakItem == nil && p.Item.Key == 0 && p.Start.After(part1Prog.Start) {
			cp := fullSched[i]
			midBreakItem = &cp
		}
		if p.Item.Key == 501 && p.sourceOffset > 0 {
			cp := fullSched[i]
			returnProg = &cp
			break
		}
	}
	if part1Prog == nil || midBreakItem == nil || returnProg == nil {
		t.Fatalf("expected part1, mid-show break pod, and resumed programme in schedule: %+v", fullSched)
	}

	// Attach an active viewer on chroma-80 during Part 1 of Show 501 (5 minutes before the mid-show break).
	clock.Set(part1Prog.Start.Add(2 * time.Minute))
	ltv.mu.Lock()
	ltv.sessions["viewer-1"] = &session{info: SessionInfo{ID: "viewer-1", Channel: Channel{ID: ch.ID}}, last: clock.Now(), mediaStarted: true}
	ltv.activeWatched = map[string]time.Time{ch.ID: clock.Now()}
	ltv.watched = map[string]time.Time{ch.ID: clock.Now()}
	ltv.reelSnapshots = map[string]*ReelSnapshot{reelID: reel}
	ltv.mu.Unlock()

	// 1. Verify next commercial pod's clips are included in GraphicsRequests during the current show segment!
	segReqs := ltv.GraphicsRequests(ctx, "")
	foundNextPodClip := false
	for _, r := range segReqs {
		if r.ReelClipID != "" {
			foundNextPodClip = true
			break
		}
	}
	if !foundNextPodClip {
		t.Fatalf("expected next commercial pod's reel clips to resolve during current segment on watched channel")
	}

	// 2. Step clock to the exact moment the mid-programme break starts and verify immediate warm of returnProg.sourceOffset!
	breakNow := midBreakItem.Start.Add(time.Second)
	clock.Set(breakNow)
	ltv.mu.Lock()
	ltv.sessions["viewer-1"].last = breakNow
	ltv.mu.Unlock()
	ltv.triggerBreakWarm(ch.ID, fullSched, breakNow)

	deadline := time.Now().Add(2 * time.Second)
	var matchedReturnWarm bool
	for time.Now().Before(deadline) {
		warmMu.Lock()
		for _, w := range warms {
			if w.key == 501 && w.offset == returnProg.sourceOffset {
				matchedReturnWarm = true
			}
		}
		warmMu.Unlock()
		if matchedReturnWarm {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !matchedReturnWarm {
		warmMu.Lock()
		got := append([]warmCall(nil), warms...)
		warmMu.Unlock()
		t.Fatalf("expected immediate break-start warm of interrupted show 501 at offset %v, got %+v", returnProg.sourceOffset, got)
	}

	// 3. Verify a timed-out reel clip in the active pod is skipped for an already-cached alternative clip, NEVER a still!
	sub := ltv.substituteReelSlot(ctx, ltv.graphicsChannel(ch), Programme{
		ID:    "pod-timeout-slot",
		Start: midBreakItem.Start,
		End:   midBreakItem.Start.Add(30 * time.Second),
		Item:  Item{Slate: "reel", ReelID: reel.ID, ReelClipID: "spot-timeout", DurationMS: 30000},
	}, reel.ID, fullSched, map[string]bool{})
	if len(sub) == 0 {
		t.Fatalf("expected cached alternative substitution for timed-out reel clip")
	}
	for _, sp := range sub {
		if sp.graphicsFallback {
			t.Fatalf("timed-out reel clip fell back to still instead of cached alternative: %+v", sp)
		}
		if sp.Item.ReelClipID != "spot-cached-a" && sp.Item.ReelClipID != "spot-cached-b" {
			t.Fatalf("expected cached alternative spot-cached-a/b, got %q", sp.Item.ReelClipID)
		}
	}

	// 4. Advance to T-minus 3 minutes before Next Hour Show (502) and verify 1 lookahead warm at offset 0.
	var nextShowStart time.Time
	for _, p := range fullSched {
		if p.Item.Key == 502 {
			nextShowStart = p.Start
			break
		}
	}
	if nextShowStart.IsZero() {
		t.Fatalf("expected show 502 in schedule")
	}
	tMinus := nextShowStart.Add(-3 * time.Minute)
	clock.Set(tMinus)
	ltv.mu.Lock()
	ltv.sessions["viewer-1"].last = tMinus
	ltv.reelSnapshots = map[string]*ReelSnapshot{reelID: reel}
	ltv.mu.Unlock()
	ltv.warmProgrammes()

	warmMu.Lock()
	foundNextWarm := false
	for _, w := range warms {
		if w.key == 502 && w.offset == 0 {
			foundNextWarm = true
		}
	}
	warmMu.Unlock()
	if !foundNextWarm {
		t.Fatalf("expected T-minus warm of next show 502 at offset 0")
	}
}

func TestNeverHoldOnStillOnceStandbyMasterExistsAndVisibilityLogs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cacheDir := t.TempDir()
	clock := newFlowClock(time.Date(2026, 11, 10, 21, 15, 0, 0, time.UTC))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	oa, err := onair.New(onair.Options{
		DB:     db,
		Dir:    cacheDir,
		Logger: logger,
		Now:    clock.Now,
		RenderFrame: func(_ scene.Document, _ raster.Bindings, _ float64, w, h int) *image.RGBA {
			return image.NewRGBA(image.Rect(0, 0, w, h))
		},
		Render: func(_ context.Context, _ scene.Document, _ raster.Bindings, out string) error {
			return os.WriteFile(out, []byte("animated-standby-mp4"), 0600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oa.Start()
	defer oa.Close()

	// Warm/save the look's break master once (via cold live-save airing).
	ms, _, err := oa.OpenMoment(ctx, core.ClipRequest{LookID: "nocturne", Moment: "break"}, 160, 90, 1, 2, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 160*90*3/2)
	for {
		if _, rErr := ms.ReadFrame(buf); rErr != nil {
			break
		}
	}
	_ = ms.Close()

	// Cold airing queues its master independently of the plate reader. Wait
	// for the premise of this test before asking the planner to select it.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, ok := oa.CachedStandby(ctx, "nocturne"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standby master did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ltv, err := New(Options{
		DB:       db,
		Graphics: oa,
		Logger:   logger,
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ltv.Close()

	runner := &fakeContinuousRunner{}
	planner := &scheduleBroadcastPlanner{
		s:       ltv,
		channel: Channel{ID: "chroma-80", Name: "CHROMA 80", Kind: "virtual", Look: &ChannelLook{LookID: "nocturne"}},
		runner:  runner,
		format:  transcode.RawFormat{Width: 320, Height: 180, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2},
	}

	now := clock.Now()
	// Outage screens can use cached standby; opening holds need foreground text.
	scrPlan := planner.screen(now)
	if scrPlan.element != "standby" || !strings.HasPrefix(scrPlan.id, "screen-standby-") {
		t.Fatalf("expected screen() to use looping animated standby master, got id=%q element=%q", scrPlan.id, scrPlan.element)
	}
	momPlan := planner.momentScreenAt(now)
	if momPlan.element != "ident" || !strings.HasPrefix(momPlan.id, "moment-card-") {
		t.Fatalf("expected momentScreenAt() to use complete ident card, got id=%q element=%q", momPlan.id, momPlan.element)
	}

	_, _, err = scrPlan.open(ctx)
	if err != nil {
		t.Fatalf("opening standby screen plan failed: %v", err)
	}
	if len(runner.decoders) != 1 || !runner.decoders[0].Loop {
		t.Fatalf("expected looping decoder on cached look master, got %+v", runner.decoders)
	}

	// Record hold duration + warm handoff lead and assert visibility snapshot + structured log output.
	ltv.warmMu.Lock()
	ltv.warmed = map[warmTarget]time.Time{
		{channelID: "chroma-80", key: 777, offsetSec: 420}: now.Add(-12 * time.Second),
	}
	ltv.warmMu.Unlock()

	planner.noteHold("hold", 800*time.Millisecond)
	planner.noteHandoff(777, 420*time.Second, now)
	ltv.noteMomentPlayout("chroma-80", "back", "live-save", 4.25, "")

	vis := ltv.BroadcastVisibility()
	if vis.HoldCount != 1 || vis.HoldDuration < 800*time.Millisecond {
		t.Fatalf("expected hold duration recorded in visibility, got %+v", vis)
	}
	if vis.HandoffCount != 1 || vis.LastWarmLead != 12*time.Second {
		t.Fatalf("expected 12s warm lead recorded on handoff, got %+v", vis)
	}
	if !strings.Contains(logBuf.String(), "live broadcast visibility") {
		t.Fatalf("expected 'live broadcast visibility' structured Info log, got:\n%s", logBuf.String())
	}
}
