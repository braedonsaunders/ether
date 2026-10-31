package watchparty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func testEngine(now *time.Time, allow func(*core.Viewer) error, streams func(*core.Viewer) error) *Engine {
	return New(Options{
		Now:     func() time.Time { return *now },
		UseLead: true,
		Lead:    time.Second,
		Allow: func(_ context.Context, _ frontend.Protocol, v *core.Viewer, _ core.RatingKey) error {
			if allow == nil {
				return nil
			}
			return allow(v)
		},
		Streams: func(_ context.Context, _ frontend.Protocol, v *core.Viewer, _ core.Device, _ core.RatingKey) error {
			if streams == nil {
				return nil
			}
			return streams(v)
		},
		ItemID: func(k core.RatingKey) string { return "item-" + string(rune('0'+k)) },
	})
}

func actor(id int64, protocol frontend.Protocol, device string) frontend.PartyActor {
	return frontend.PartyActor{Viewer: &core.Viewer{ID: id, Username: "viewer"}, Device: core.Device{ID: device}, Protocol: protocol}
}

func TestSharedClockAndRejoin(t *testing.T) {
	start := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	now := start
	engine := testEngine(&now, nil, nil)
	ctx := context.Background()
	host := actor(1, "silo", "couch")
	guest := actor(2, "jellyfin", "tv")
	created, err := engine.Create(ctx, host, frontend.PartyCreate{RoomID: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	hostEvents, hostDone := engine.Subscribe(host)
	defer hostDone()
	joined, err := engine.Join(ctx, guest, frontend.PartyJoin{Code: created.Code})
	if err != nil || joined.RoomID != created.RoomID || len(joined.Members) != 2 {
		t.Fatal(err, joined.RoomID, len(joined.Members))
	}
	guestEvents, guestDone := engine.Subscribe(guest)
	defer guestDone()
	if _, err := engine.Select(ctx, host, created.RoomID, created.RoomToken, frontend.PartyPick{Item: 5, FileID: "9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Start(ctx, host, created.RoomID, created.RoomToken); err != nil {
		t.Fatal(err)
	}
	if err := engine.Report(ctx, host, frontend.PartyReport{Kind: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Report(ctx, guest, frontend.PartyReport{Kind: "ready"}); err != nil {
		t.Fatal(err)
	}
	hostCmd := waitCommand(t, hostEvents)
	guestCmd := waitCommand(t, guestEvents)
	if !hostCmd.ExecuteAt.Equal(start.Add(time.Second)) || !guestCmd.ExecuteAt.Equal(hostCmd.ExecuteAt) || hostCmd.Position != 0 || guestCmd.Position != 0 {
		t.Fatal(hostCmd, guestCmd)
	}
	now = start.Add(6 * time.Second)
	hostView, err := engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil {
		t.Fatal(err)
	}
	guestView, err := engine.Get(ctx, guest, created.RoomID, "")
	if err != nil {
		t.Fatal(err)
	}
	if hostView.Position != 5*time.Second || guestView.Position != hostView.Position || hostView.Playback != "playing" {
		t.Fatal(hostView.Position, guestView.Position, hostView.Playback)
	}
	if err := engine.Transport(ctx, guest, created.RoomID, "", frontend.PartyInput{Action: "pause", Position: hostView.Position}); err != nil {
		t.Fatal(err)
	}
	paused, err := engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil || !paused.Paused || paused.Position != 6*time.Second {
		t.Fatal(err, paused.Paused, paused.Position)
	}
	hostDone()
	dropped, err := engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil || dropped.Members[0].Connected || len(dropped.Members) != 2 {
		t.Fatalf("drop: %+v %v", dropped.Members, err)
	}
	again, err := engine.Join(ctx, host, frontend.PartyJoin{Code: created.Code})
	if err != nil || again.RoomID != created.RoomID || len(again.Members) != 2 {
		t.Fatal(err, again)
	}
	rejoined, rejoinDone := engine.Subscribe(host)
	defer rejoinDone()
	_ = rejoined
	back, err := engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil || !back.HostConnected {
		t.Fatal(err, back.HostConnected)
	}
}

func TestPermissionDenialDoesNotRevealTheTitle(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	engine := testEngine(&now, func(v *core.Viewer) error {
		if v.ID == 2 {
			return core.ErrNotFound
		}
		return nil
	}, nil)
	ctx := context.Background()
	host := actor(1, "silo", "couch")
	guest := actor(2, "silo", "phone")
	created, err := engine.Create(ctx, host, frontend.PartyCreate{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Select(ctx, host, created.RoomID, created.RoomToken, frontend.PartyPick{Item: 5}); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Join(ctx, guest, frontend.PartyJoin{GroupID: created.RoomID})
	if !errors.Is(err, frontend.ErrPartyDenied) {
		t.Fatal(err)
	}
	view, err := engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil || len(view.Members) != 1 || view.ContentID == "" {
		t.Fatal(err, view.Members, view.ContentID)
	}
}

func TestRateStaysSharedForSiloAndRefusesJellyfin(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	start := now
	engine := testEngine(&now, nil, nil)
	ctx := context.Background()
	host := actor(1, "silo", "a")
	other := actor(3, "silo", "b")
	created, err := engine.Create(ctx, host, frontend.PartyCreate{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Join(ctx, other, frontend.PartyJoin{Code: created.Code}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Select(ctx, host, created.RoomID, created.RoomToken, frontend.PartyPick{Item: 4}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Transport(ctx, host, created.RoomID, created.RoomToken, frontend.PartyInput{Action: "play", Rate: 2}); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Second + 3*time.Second)
	view, err := engine.Get(ctx, other, created.RoomID, "")
	if err != nil || view.Rate != 2 || view.Position != 6*time.Second {
		t.Fatal(err, view.Rate, view.Position)
	}
	jf := actor(9, "jellyfin", "tv")
	if _, err := engine.Join(ctx, jf, frontend.PartyJoin{Code: created.Code}); err != nil {
		t.Fatal(err)
	}
	err = engine.Transport(ctx, host, created.RoomID, created.RoomToken, frontend.PartyInput{Action: "play", Rate: 3})
	if !errors.Is(err, frontend.ErrPartyForbidden) {
		t.Fatal(err)
	}
	view, err = engine.Get(ctx, host, created.RoomID, created.RoomToken)
	if err != nil || view.Rate != 2 {
		t.Fatal(err, view.Rate)
	}
}

func TestStreamLimitBlocksOnlyThatMember(t *testing.T) {
	now := time.Now()
	engine := testEngine(&now, nil, func(v *core.Viewer) error {
		if v.ID == 2 {
			return core.ErrStreamLimit
		}
		return nil
	})
	ctx := context.Background()
	host := actor(1, "jellyfin", "a")
	guest := actor(2, "jellyfin", "b")
	created, err := engine.Create(ctx, host, frontend.PartyCreate{Name: "Friday"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Join(ctx, guest, frontend.PartyJoin{GroupID: created.RoomID}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Queue(ctx, host, frontend.PartyQueue{Kind: "replace", Items: []core.RatingKey{8}}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Get(ctx, host, created.RoomID, "")
	if err != nil || view.Playback != "waiting" {
		t.Fatal(err, view.Playback)
	}
	var guestBlocked bool
	for _, m := range view.Members {
		if m.ViewerID == 2 && m.Blocked {
			guestBlocked = true
		}
	}
	if !guestBlocked {
		t.Fatal(view.Members)
	}
	if err := engine.Report(ctx, host, frontend.PartyReport{Kind: "ready"}); err != nil {
		t.Fatal(err)
	}
	playing, err := engine.Get(ctx, host, created.RoomID, "")
	if err != nil || playing.Playback != "playing" {
		t.Fatal(err, playing.Playback)
	}
}

func waitCommand(t *testing.T, events <-chan frontend.PartyEvent) frontend.PartyCommand {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "transport" || ev.Type == "command" {
				if ev.Command.Action != "play" {
					continue
				}
				return ev.Command
			}
		case <-deadline:
			t.Fatal("no play command")
		}
	}
}
