package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type multiviewFakeLive struct {
	mu      sync.Mutex
	seq     int
	active  map[string]multiviewTuneRecord
	stopped []string
	tuneErr func(channel string, caps core.ClientCaps) error
}

type multiviewTuneRecord struct {
	id      string
	channel string
	caps    core.ClientCaps
}

func newMultiviewFakeLive() *multiviewFakeLive {
	return &multiviewFakeLive{active: map[string]multiviewTuneRecord{}}
}

func (*multiviewFakeLive) Allowed(_ context.Context, _ *core.Viewer, id string) error {
	if id == "" || id == "missing" {
		return core.ErrNotFound
	}
	return nil
}

func (l *multiviewFakeLive) Tune(ctx context.Context, v *core.Viewer, ch string, caps core.ClientCaps, cs string) (string, error) {
	id, _, err := l.TuneMedia(ctx, v, ch, caps, cs)
	return id, err
}

func (l *multiviewFakeLive) TuneMedia(_ context.Context, _ *core.Viewer, ch string, caps core.ClientCaps, _ string) (string, *core.MediaInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tuneErr != nil {
		if err := l.tuneErr(ch, caps); err != nil {
			return "", nil, err
		}
	}
	l.seq++
	id := fmt.Sprintf("live-%d", l.seq)
	l.active[id] = multiviewTuneRecord{id: id, channel: ch, caps: caps}
	h := 1080
	if caps.MaxHeight > 0 && caps.MaxHeight < h {
		h = caps.MaxHeight
	}
	return id, &core.MediaInfo{
		Container: "mpegts",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: h * 16 / 9, Height: h},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, Default: true},
		},
	}, nil
}

func (*multiviewFakeLive) Serve(w http.ResponseWriter, _ *http.Request, _ *core.Viewer, id, tail, base string) {
	_, _ = w.Write([]byte(id + ":" + base + tail))
}

func (l *multiviewFakeLive) Stop(_ context.Context, _ *core.Viewer, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.active, id)
	l.stopped = append(l.stopped, id)
	return nil
}

func (l *multiviewFakeLive) activeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.active)
}

func (l *multiviewFakeLive) record(id string) (multiviewTuneRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.active[id]
	return r, ok
}

func newMultiviewBroker(t *testing.T, live LiveBackend, now *time.Time) (*Broker, *fakePlayback, *EventBus) {
	t.Helper()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	t.Cleanup(events.Close)
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}
	clock := func() time.Time {
		if now != nil {
			return *now
		}
		return time.Now()
	}
	b, err := NewBroker(BrokerDeps{
		SurfacePolicy: testSessionPolicy,
		Live:          live,
		Events:        events,
		Playback:      pb,
		Transcoder:    fakeBrokerTranscoder{},
		Progress:      &fakeProgress{},
		ActiveViewer:  active,
		CanPlay:       func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit:  func(*core.Viewer) int { return 0 },
		OnTimeline:    func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
		Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b, pb, events
}

func TestMultiviewFourTilesSurviveAndSameTileReplaces(t *testing.T) {
	ctx := context.Background()
	live := newMultiviewFakeLive()
	b, _, _ := newMultiviewBroker(t, live, nil)
	surf := BindSurface(b, "vela").(interface {
		frontend.Sessions
		frontend.MultiviewSessions
	})
	v := &core.Viewer{ID: 7}
	dev := core.Device{ID: "apple-tv"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}

	group, err := surf.CreateMultiviewGroup(ctx, access)
	if err != nil || group.GroupID == "" || group.MaxTiles != frontend.MaxMultiviewTiles {
		t.Fatalf("CreateMultiviewGroup = %+v, err=%v", group, err)
	}

	openTile := func(ch string, tile int, mvMaxHeight int) frontend.Session {
		t.Helper()
		plan, err := surf.Plan(ctx, frontend.PlanRequest{
			Viewer:    v,
			Device:    dev,
			Protocol:  "vela",
			Channel:   ch,
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: tile, MaxHeight: mvMaxHeight},
			Caps:      core.ClientCaps{MaxHeight: 2160},
		})
		if err != nil {
			t.Fatalf("Plan tile %d: %v", tile, err)
		}
		s, err := surf.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: plan.ID})
		if err != nil {
			t.Fatalf("Open tile %d: %v", tile, err)
		}
		return s
	}

	var tiles [4]frontend.Session
	for i := 0; i < 4; i++ {
		tiles[i] = openTile(fmt.Sprintf("ch-%d", i), i, 0)
	}

	list, err := surf.Sessions(ctx, access)
	if err != nil || len(list) != 4 {
		t.Fatalf("expected 4 surviving tiles, got %d (err=%v)", len(list), err)
	}
	if live.activeCount() != 4 {
		t.Fatalf("expected 4 active live backends, got %d", live.activeCount())
	}

	// Re-tuning tile 1 replaces ONLY tile 1 while keeping tiles 0, 2, 3 intact.
	replaced := openTile("ch-replacement", 1, 0)
	list, err = surf.Sessions(ctx, access)
	if err != nil || len(list) != 4 {
		t.Fatalf("expected 4 tiles after replacing tile 1, got %d (err=%v)", len(list), err)
	}
	if live.activeCount() != 4 {
		t.Fatalf("expected 4 active live backends after tile replacement, got %d", live.activeCount())
	}
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	if ids[tiles[1].ID] || !ids[replaced.ID] || !ids[tiles[0].ID] || !ids[tiles[2].ID] || !ids[tiles[3].ID] {
		t.Fatalf("wrong tiles survived replacement: %+v", ids)
	}
}

func TestMultiviewNonGroupPlaybackSupersedesWholeGroup(t *testing.T) {
	ctx := context.Background()
	live := newMultiviewFakeLive()
	b, _, _ := newMultiviewBroker(t, live, nil)
	v := &core.Viewer{ID: 8}
	dev := core.Device{ID: "tv-living"}
	otherDev := core.Device{ID: "iphone"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}

	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	openGroup := func() {
		for i := 0; i < 4; i++ {
			p, err := b.Plan(ctx, frontend.PlanRequest{
				Viewer: v, Device: dev, Protocol: "vela", Channel: fmt.Sprintf("ch-%d", i),
				Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: i},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 1. Playing on another device does NOT supersede the group.
	openGroup()
	pOther, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: otherDev, Protocol: "vela", Channel: "ch-other"})
	if err != nil {
		t.Fatal(err)
	}
	otherSess, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: otherDev, Protocol: "vela", PlanID: pOther.ID})
	if err != nil {
		t.Fatal(err)
	}
	if live.activeCount() != 5 {
		t.Fatalf("other device superseded group: active=%d, want 5", live.activeCount())
	}

	// 2. Non-group live playback on the SAME device supersedes the whole 4-tile group.
	pSolo, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: dev, Protocol: "vela", Channel: "ch-solo"})
	if err != nil {
		t.Fatal(err)
	}
	solo, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: pSolo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if live.activeCount() != 2 {
		t.Fatalf("non-group live play did not supersede whole group: active=%d, want 2 (solo + otherDev)", live.activeCount())
	}
	list, _ := b.Sessions(ctx, access)
	if len(list) != 2 {
		t.Fatalf("expected solo + otherDev sessions, got %d", len(list))
	}
	_ = b.Stop(ctx, access, solo.ID)
	_ = b.Stop(ctx, frontend.SessionAccess{Viewer: v, Device: otherDev, Protocol: "vela"}, otherSess.ID)

	// 3. Non-group VOD playback on the SAME device also supersedes the whole 4-tile group.
	openGroup()
	if live.activeCount() != 4 {
		t.Fatalf("expected 4 live tiles before VOD, got %d", live.activeCount())
	}
	pVOD, err := b.Plan(ctx, frontend.PlanRequest{
		Viewer: v, Device: dev, Protocol: "vela", Item: core.MovieKey(42),
		Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	vodSess, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: pVOD.ID})
	if err != nil {
		t.Fatal(err)
	}
	if live.activeCount() != 0 {
		t.Fatalf("VOD play on same device left %d live tiles active", live.activeCount())
	}
	list, _ = b.Sessions(ctx, access)
	if len(list) != 1 || list[0].ID != vodSess.ID {
		t.Fatalf("expected only VOD session %q, got %+v", vodSess.ID, list)
	}
}

func TestMultiviewGroupScopingTTLAndDelete(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	live := newMultiviewFakeLive()
	b, _, _ := newMultiviewBroker(t, live, &now)
	v1 := &core.Viewer{ID: 1}
	v2 := &core.Viewer{ID: 2}
	dev1 := core.Device{ID: "dev-1"}
	dev2 := core.Device{ID: "dev-2"}

	group, err := b.CreateMultiviewGroup(ctx, frontend.SessionAccess{Viewer: v1, Device: dev1, Protocol: "vela"})
	if err != nil {
		t.Fatal(err)
	}

	// Wrong viewer, wrong device, and wrong protocol are all rejected with ErrUnauthorized.
	for _, tc := range []struct {
		name  string
		v     *core.Viewer
		dev   core.Device
		proto frontend.Protocol
	}{
		{"other_viewer", v2, dev1, "vela"},
		{"other_device", v1, dev2, "vela"},
		{"other_protocol", v1, dev1, "silo"},
	} {
		_, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: tc.v, Device: tc.dev, Protocol: tc.proto, Channel: "ch-1",
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: 0},
		})
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("%s Plan: got %v, want ErrUnauthorized", tc.name, err)
		}
		if err := b.DeleteMultiviewGroup(ctx, frontend.SessionAccess{Viewer: tc.v, Device: tc.dev, Protocol: tc.proto}, group.GroupID); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("%s Delete: got %v, want ErrUnauthorized", tc.name, err)
		}
	}

	// Open 3 tiles and then delete the group -> all 3 tiles stop.
	access := frontend.SessionAccess{Viewer: v1, Device: dev1, Protocol: "vela"}
	for i := 0; i < 3; i++ {
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v1, Device: dev1, Protocol: "vela", Channel: fmt.Sprintf("ch-%d", i),
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: i},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: v1, Device: dev1, Protocol: "vela", PlanID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if live.activeCount() != 3 {
		t.Fatalf("expected 3 tiles before delete, got %d", live.activeCount())
	}
	if err := b.DeleteMultiviewGroup(ctx, access, group.GroupID); err != nil {
		t.Fatalf("DeleteMultiviewGroup: %v", err)
	}
	if live.activeCount() != 0 {
		t.Fatalf("expected 0 active tiles after delete, got %d", live.activeCount())
	}
	if err := b.DeleteMultiviewGroup(ctx, access, group.GroupID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete: got %v, want ErrNotFound", err)
	}

	// TTL expiry using deterministic clock (no wall-clock sleep).
	expGroup, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(multiviewGroupTTL + time.Minute)
	if _, err := b.Plan(ctx, frontend.PlanRequest{
		Viewer: v1, Device: dev1, Protocol: "vela", Channel: "ch-1",
		Multiview: &frontend.MultiviewTarget{GroupID: expGroup.GroupID, Tile: 0},
	}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expired group Plan: got %v, want ErrNotFound", err)
	}
}

func TestMultiviewTileMaxHeightRoundingAndFocusedReplan(t *testing.T) {
	// Verify binding pixel rounding rule: 480 / 720 / 1080 / 2160.
	for _, tc := range []struct {
		device, tiles, declared int
		want                    int
	}{
		{2160, 2, 0, 1080},
		{2160, 3, 0, 720},
		{2160, 4, 0, 720},
		{2160, 4, 360, 480},
		{2160, 4, 480, 480},
		{2160, 4, 540, 720},
		{2160, 4, 900, 1080},
		{2160, 4, 1440, 2160},
		{1080, 4, 1440, 1080}, // never exceeds device cap
		{720, 2, 0, 720},      // 720p device stays 720p
	} {
		got := multiviewTileMaxHeight(tc.device, tc.tiles, tc.declared)
		if got != tc.want {
			t.Fatalf("multiviewTileMaxHeight(dev=%d, tiles=%d, decl=%d) = %d, want %d", tc.device, tc.tiles, tc.declared, got, tc.want)
		}
	}

	ctx := context.Background()
	live := newMultiviewFakeLive()
	b, _, _ := newMultiviewBroker(t, live, nil)
	v := &core.Viewer{ID: 11}
	dev := core.Device{ID: "tv-4k"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}

	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	var sessions [4]frontend.Session
	for i := 0; i < 4; i++ {
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v, Device: dev, Protocol: "vela", Channel: fmt.Sprintf("ch-%d", i),
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: i},
			Caps:      core.ClientCaps{MaxHeight: 2160},
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions[i], err = b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Tile 0 & 1 got 1080p, tile 2 & 3 (3rd and 4th tiles) got 720p.
	if sessions[1].Multiview == nil || sessions[1].Multiview.MaxHeight != 1080 {
		t.Fatalf("2-tile maxHeight = %+v, want 1080", sessions[1].Multiview)
	}
	if sessions[2].Multiview == nil || sessions[2].Multiview.MaxHeight != 720 {
		t.Fatalf("3-tile maxHeight = %+v, want 720", sessions[2].Multiview)
	}
	if sessions[3].Multiview == nil || sessions[3].Multiview.MaxHeight != 720 {
		t.Fatalf("4-tile maxHeight = %+v, want 720", sessions[3].Multiview)
	}

	// Upgrade focused tile 3 via the StartStream replan path to 1080p (rounding 900 -> 1080).
	oldPath := sessions[3].MediaPath
	h900 := 900
	newPath, err := b.StartStream(ctx, access, sessions[3].ID, frontend.StreamRequest{Format: frontend.StreamHLS, MaxHeight: &h900})
	if err != nil {
		t.Fatalf("replan tile 3 to 900->1080: %v", err)
	}
	if newPath == oldPath || !strings.HasSuffix(newPath, "/live/index.m3u8") {
		t.Fatalf("replan did not rotate live media path: old=%q new=%q", oldPath, newPath)
	}
	// All 4 tiles still survive and tile 3 was re-tuned with MaxHeight 1080.
	if live.activeCount() != 4 {
		t.Fatalf("expected 4 active live tiles after replan, got %d", live.activeCount())
	}
	rec := httptest.NewRecorder()
	b.ServeMedia(rec, httptest.NewRequest(http.MethodGet, newPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("replanned media serve status = %d", rec.Code)
	}
	liveID := strings.SplitN(rec.Body.String(), ":", 2)[0]
	tuned, ok := live.record(liveID)
	if !ok || tuned.caps.MaxHeight != 1080 {
		t.Fatalf("replanned live caps = %+v (ok=%v), want MaxHeight=1080", tuned.caps, ok)
	}
}

func TestMultiviewClusterStoreAllowsFourActiveTiles(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	hub := clusterbus.NewHub()
	node := hub.Node("node-a")
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(node, active)
	t.Cleanup(events.Close)
	live := newMultiviewFakeLive()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}
	b, err := NewBroker(BrokerDeps{
		SurfacePolicy: testSessionPolicy,
		DB:            db,
		Cluster:       node,
		Live:          live,
		Events:        events,
		Playback:      pb,
		Transcoder:    fakeBrokerTranscoder{},
		Progress:      &fakeProgress{},
		ActiveViewer:  active,
		CanPlay:       func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit:  func(*core.Viewer) int { return 0 },
		OnTimeline:    func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(ctx) })

	v := &core.Viewer{ID: 5}
	dev := core.Device{ID: "cluster-tv"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}
	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v, Device: dev, Protocol: "vela", Channel: fmt.Sprintf("ch-%d", i),
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: i},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID}); err != nil {
			t.Fatalf("cluster Open tile %d: %v", i, err)
		}
	}
	var activeRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_sessions WHERE user_id=? AND surface='vela' AND device_id=? AND active=1`, v.ID, dev.ID).Scan(&activeRows); err != nil || activeRows != 4 {
		t.Fatalf("cluster active multiview rows = %d (err=%v), want 4", activeRows, err)
	}

	// Non-group play supersedes all 4 rows in cluster store.
	pSolo, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: dev, Protocol: "vela", Channel: "ch-solo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: pSolo.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_sessions WHERE user_id=? AND surface='vela' AND device_id=? AND active=1`, v.ID, dev.ID).Scan(&activeRows); err != nil || activeRows != 1 {
		t.Fatalf("cluster active rows after non-group play = %d (err=%v), want 1", activeRows, err)
	}
}

type barrierMultiviewLive struct {
	*multiviewFakeLive
	onTune func(ctx context.Context, ch string, caps core.ClientCaps) (*core.MediaInfo, error)
}

func (l *barrierMultiviewLive) TuneMedia(ctx context.Context, v *core.Viewer, ch string, caps core.ClientCaps, cs string) (string, *core.MediaInfo, error) {
	if l.onTune != nil {
		override, err := l.onTune(ctx, ch, caps)
		if err != nil {
			return "", nil, err
		}
		if override != nil {
			l.mu.Lock()
			l.seq++
			id := fmt.Sprintf("live-%d", l.seq)
			l.active[id] = multiviewTuneRecord{id: id, channel: ch, caps: caps}
			l.mu.Unlock()
			return id, override, nil
		}
	}
	return l.multiviewFakeLive.TuneMedia(ctx, v, ch, caps, cs)
}

func TestMultiviewParallelTileOpensDoNotSerializeOnDeviceGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	entered := 0
	allEntered := make(chan struct{})
	live := &barrierMultiviewLive{
		multiviewFakeLive: newMultiviewFakeLive(),
		onTune: func(ctx context.Context, ch string, caps core.ClientCaps) (*core.MediaInfo, error) {
			mu.Lock()
			entered++
			if entered == 4 {
				close(allEntered)
			}
			mu.Unlock()
			select {
			case <-allEntered:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	b, _, _ := newMultiviewBroker(t, live, nil)
	v := &core.Viewer{ID: 21}
	dev := core.Device{ID: "web-parallel"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}
	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 4)
	for tile := 0; tile < 4; tile++ {
		go func(tile int) {
			p, err := b.Plan(ctx, frontend.PlanRequest{
				Viewer: v, Device: dev, Protocol: "vela", Channel: fmt.Sprintf("ch-%d", tile),
				Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: tile, MaxHeight: 480},
				Caps:      core.ClientCaps{MaxHeight: 1080, HLSContainers: []string{"fmp4"}},
			})
			if err != nil {
				errs <- err
				return
			}
			if p.HLSContainer != "fmp4" || p.Decision.Container != "fmp4" {
				errs <- fmt.Errorf("tile %d plan container = %q/%q, want fmp4", tile, p.HLSContainer, p.Decision.Container)
				return
			}
			_, err = b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID})
			errs <- err
		}(tile)
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("parallel tile open failed: %v", err)
		}
	}
	if live.activeCount() != 4 {
		t.Fatalf("active live tiles = %d, want 4", live.activeCount())
	}
}

func TestMultiviewHandoverAndInPlaceReplanContracts(t *testing.T) {
	ctx := context.Background()
	var live *barrierMultiviewLive
	var b *Broker
	live = &barrierMultiviewLive{multiviewFakeLive: newMultiviewFakeLive()}
	b, _, _ = newMultiviewBroker(t, live, nil)

	v := &core.Viewer{ID: 31}
	dev := core.Device{ID: "web-upgrade"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}
	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}

	// Open tile 0 at 480p on a 1080p-capable channel.
	openTile0 := func(maxH int) (frontend.Session, error) {
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v, Device: dev, Protocol: "vela", Channel: "ch-hd",
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: 0, MaxHeight: maxH},
			Caps:      core.ClientCaps{MaxHeight: 1080, HLSContainers: []string{"fmp4"}},
		})
		if err != nil {
			return frontend.Session{}, err
		}
		return b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID})
	}
	sessA, err := openTile0(480)
	if err != nil {
		t.Fatalf("open sessA: %v", err)
	}
	if sessA.Media == nil || sessA.Media.Container != "mpegts" && sessA.Media.Container != "fmp4" {
		t.Fatalf("unexpected media: %+v", sessA.Media)
	}

	// 1. Handover failure: old session A must stay active and playable.
	sawHandoverOnFail := false
	oldStillActiveDuringFail := false
	live.onTune = func(tuneCtx context.Context, ch string, caps core.ClientCaps) (*core.MediaInfo, error) {
		sawHandoverOnFail = core.LiveHandover(tuneCtx)
		rec := httptest.NewRecorder()
		b.ServeMedia(rec, httptest.NewRequest(http.MethodGet, sessA.MediaPath, nil))
		oldStillActiveDuringFail = rec.Code == http.StatusOK && live.activeCount() == 1
		return nil, core.ErrStreamLimit
	}
	if _, err := openTile0(1080); !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("expected failed handover ErrStreamLimit, got %v", err)
	}
	if !sawHandoverOnFail || !oldStillActiveDuringFail {
		t.Fatalf("handover failure invariants broken: handover=%v oldActive=%v", sawHandoverOnFail, oldStillActiveDuringFail)
	}
	rec := httptest.NewRecorder()
	b.ServeMedia(rec, httptest.NewRequest(http.MethodGet, sessA.MediaPath, nil))
	if rec.Code != http.StatusOK || live.activeCount() != 1 {
		t.Fatalf("sessA died after failed handover: status=%d active=%d", rec.Code, live.activeCount())
	}

	// 2. Handover success: old session A stays active WHILE B is tuning, then ends ONLY AFTER B commits.
	oldActiveDuringB := false
	live.onTune = func(tuneCtx context.Context, ch string, caps core.ClientCaps) (*core.MediaInfo, error) {
		if !core.LiveHandover(tuneCtx) {
			return nil, errors.New("missing LiveHandover on handover tune")
		}
		rec := httptest.NewRecorder()
		b.ServeMedia(rec, httptest.NewRequest(http.MethodGet, sessA.MediaPath, nil))
		oldActiveDuringB = rec.Code == http.StatusOK
		return nil, nil
	}
	sessB, err := openTile0(1080)
	if err != nil {
		t.Fatalf("handover open sessB: %v", err)
	}
	if !oldActiveDuringB {
		t.Fatal("sessA was ended before sessB finished tuning")
	}
	if live.activeCount() != 1 {
		t.Fatalf("expected only sessB active after handover commit, got %d", live.activeCount())
	}
	recAfterA := httptest.NewRecorder()
	b.ServeMedia(recAfterA, httptest.NewRequest(http.MethodGet, sessA.MediaPath, nil))
	if recAfterA.Code == http.StatusOK {
		t.Fatal("sessA should be ended after sessB committed")
	}
	recAfterB := httptest.NewRecorder()
	b.ServeMedia(recAfterB, httptest.NewRequest(http.MethodGet, sessB.MediaPath, nil))
	if recAfterB.Code != http.StatusOK {
		t.Fatalf("sessB not playable after handover: status=%d", recAfterB.Code)
	}

	// 3. In-place replan when source cannot go higher (480p source replanned 480 -> 1080):
	// returns exact same URL and keeps old live output running without rotating.
	live.onTune = func(tuneCtx context.Context, ch string, caps core.ClientCaps) (*core.MediaInfo, error) {
		return &core.MediaInfo{
			Container: "fmp4",
			Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 854, Height: 480},
				{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, Default: true},
			},
		}, nil
	}
	sessSD, err := openTile0(480)
	if err != nil {
		t.Fatalf("open SD tile: %v", err)
	}
	h1080 := 1080
	samePath, err := b.StartStream(ctx, access, sessSD.ID, frontend.StreamRequest{Format: frontend.StreamHLS, MaxHeight: &h1080})
	if err != nil {
		t.Fatalf("replan SD 480->1080: %v", err)
	}
	if samePath != sessSD.MediaPath {
		t.Fatalf("SD source replan rotated URL when source cannot go higher: old=%q new=%q", sessSD.MediaPath, samePath)
	}
	if live.activeCount() != 1 {
		t.Fatalf("redundant tune leaked after unchanged SD replan: active=%d, want 1", live.activeCount())
	}
}

// CleanupStoppedLive retires an idle-reaped multiview tile from the broker
// independently of the group TTL without disturbing sibling tiles, and active
// live media fetches refresh the shortened group TTL.
func TestMultiviewCleanupStoppedLiveRetiresIdleTileAndMediaRefreshesGroupTTL(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	live := newMultiviewFakeLive()
	v := &core.Viewer{ID: 7, Username: "alice"}
	dev := core.Device{ID: "atv-idle", Product: "Vela"}
	access := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: "vela"}
	b, _, _ := newMultiviewBroker(t, live, &now)

	if multiviewGroupTTL >= 4*time.Hour {
		t.Fatalf("multiviewGroupTTL = %v, want shorter TTL refreshed by activity", multiviewGroupTTL)
	}
	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	openTile := func(tile int, ch string) frontend.Session {
		t.Helper()
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v, Device: dev, Protocol: "vela", Channel: ch,
			Multiview: &frontend.MultiviewTarget{GroupID: group.GroupID, Tile: tile},
			Caps:      core.ClientCaps{MaxHeight: 1080, HLSContainers: []string{"fmp4"}},
		})
		if err != nil {
			t.Fatalf("plan tile %d: %v", tile, err)
		}
		s, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID})
		if err != nil {
			t.Fatalf("open tile %d: %v", tile, err)
		}
		return s
	}
	s0 := openTile(0, "ch-0")
	s1 := openTile(1, "ch-1")

	b.mu.Lock()
	playID0 := b.sessions[s0.ID].playID
	b.mu.Unlock()

	// Subscribe to events to confirm SessionStopped is published for reaped tile 0.
	evCh, err := b.Events(ctx, frontend.EventScope{Viewer: v, Protocol: "vela", SessionID: s0.ID, Device: dev})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate livetv.Service.expireSessions() reaping idle tile 0 well before group TTL.
	now = now.Add(35 * time.Second)
	if !b.CleanupStoppedLive(ctx, playID0) {
		t.Fatal("CleanupStoppedLive returned false for active tile 0")
	}
	select {
	case ev := <-evCh:
		if ev.Type != "SessionStopped" || ev.SessionID != s0.ID {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SessionStopped on reaped tile 0")
	}

	b.mu.Lock()
	_, has0 := b.sessions[s0.ID]
	_, has1 := b.sessions[s1.ID]
	b.mu.Unlock()
	if has0 || !has1 {
		t.Fatalf("after CleanupStoppedLive(tile0): has0=%v has1=%v, want false/true", has0, has1)
	}

	// Active media fetches on tile 1 refresh the group TTL past its initial expiry.
	now = now.Add(multiviewGroupTTL - 5*time.Minute)
	rec := httptest.NewRecorder()
	b.ServeMedia(rec, httptest.NewRequest(http.MethodGet, s1.MediaPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("serve tile 1 media = %d, want 200", rec.Code)
	}
	// Advance past original expiresAt (now + 10m > original 30m), group must still be alive!
	now = now.Add(10 * time.Minute)
	s2 := openTile(2, "ch-2")
	if s2.Multiview == nil || s2.Multiview.GroupID != group.GroupID {
		t.Fatalf("group expired despite active tile media fetches: %+v", s2.Multiview)
	}
}

func TestNonGroupEpochPrunedWithoutActiveMultiview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	live := newMultiviewFakeLive()
	v := &core.Viewer{ID: 7, Username: "alice"}
	b, _, _ := newMultiviewBroker(t, live, &now)

	openSingle := func(devID string) {
		t.Helper()
		dev := core.Device{ID: devID, Product: "Vela"}
		p, err := b.Plan(ctx, frontend.PlanRequest{
			Viewer: v, Device: dev, Protocol: "vela", Channel: "ch-1",
			Caps: core.ClientCaps{MaxHeight: 1080, HLSContainers: []string{"fmp4"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: dev, Protocol: "vela", PlanID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 8; i++ {
		openSingle(fmt.Sprintf("ephemeral-%d", i))
	}
	b.mu.Lock()
	idleCount := len(b.nonGroupEpoch)
	b.mu.Unlock()
	if idleCount != 0 {
		t.Fatalf("nonGroupEpoch retained %d entries with no multiview groups, want 0", idleCount)
	}

	mvDev := core.Device{ID: "mv-dev", Product: "Vela"}
	access := frontend.SessionAccess{Viewer: v, Device: mvDev, Protocol: "vela"}
	group, err := b.CreateMultiviewGroup(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	openSingle(mvDev.ID)
	b.mu.Lock()
	activeCount := len(b.nonGroupEpoch)
	b.mu.Unlock()
	if activeCount != 1 {
		t.Fatalf("nonGroupEpoch during active multiview = %d, want 1", activeCount)
	}

	if err := b.DeleteMultiviewGroup(ctx, access, group.GroupID); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	afterDelete := len(b.nonGroupEpoch)
	b.mu.Unlock()
	if afterDelete != 0 {
		t.Fatalf("nonGroupEpoch after multiview group deletion = %d, want 0", afterDelete)
	}
}
