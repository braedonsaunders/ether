//go:build plexparity

package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestParitySettleWarmWaitsUntilTheProbeFinishes(t *testing.T) {
	user := &core.User{ID: 9}
	caps := core.ClientCaps{ClientID: "adult", Product: "Plex for Android (TV)", Protocol: "plex"}
	key := core.MovieKey(10004)
	done := make(chan struct{})
	m := &Manager{
		now:    time.Now,
		warm:   map[identity]*warmEntry{},
		closed: true, // Prewarm refuses, leaving the planted in-flight entry to be observed.
	}
	m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}] = &warmEntry{
		done:    done,
		expires: time.Now().Add(time.Minute),
	}
	errc := make(chan error, 1)
	go func() {
		errc <- m.ParitySettleWarm(context.Background(), user, key, caps)
	}()
	select {
	case err := <-errc:
		t.Fatalf("returned before the probe finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(done)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the probe")
	}
}

func TestParitySettleWarmReturnsWhenNoProbeIsRunning(t *testing.T) {
	m := &Manager{now: time.Now, warm: map[identity]*warmEntry{}, closed: true}
	if err := m.ParitySettleWarm(context.Background(), &core.User{ID: 9}, core.MovieKey(10004), core.ClientCaps{ClientID: "adult", Product: "Plex"}); err != nil {
		t.Fatal(err)
	}
}

func TestParitySettlePendingWaitsForEveryInFlightProbe(t *testing.T) {
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	expires := time.Now().Add(time.Minute)
	m := &Manager{now: time.Now, warm: map[identity]*warmEntry{
		{userID: 1, clientID: "a", key: core.MovieKey(1)}: {done: doneA, expires: expires},
		{userID: 2, clientID: "b", key: core.MovieKey(2)}: {done: doneB, expires: expires},
	}}
	errc := make(chan error, 1)
	go func() {
		errc <- m.ParitySettlePending(context.Background())
	}()
	close(doneA)
	select {
	case err := <-errc:
		t.Fatalf("returned with one probe still running: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(doneB)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the probes")
	}
}

func TestParityProbeNowPublishesFactsForLaterReads(t *testing.T) {
	key := core.MovieKey(10002)
	title := &core.Title{Key: key, Type: core.TypeMovie, Title: "Fixture Movie 002", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	cat := &fakeCatalog{titles: map[core.RatingKey]*core.Title{key: title}}
	eng := newFakeEngine(&fakeRelease{ID: "rel", Name: "parity.mp4", Size: 4096})
	probed := &core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Width: 320, Height: 180}}}
	m := &Manager{
		now:  time.Now,
		warm: map[identity]*warmEntry{},
		deps: Deps{
			Catalog:       cat,
			Resolver:      newFakeResolver("rel"),
			Engine:        eng,
			SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return core.QualityProfile{} },
			NativeProbe: func(context.Context, core.MediaSource) (*core.MediaInfo, error) {
				return probed, nil
			},
		},
	}
	user := &core.User{ID: 1}
	caps := core.ClientCaps{ClientID: "parity-owner", Product: "Plex Web"}
	if err := m.ParityProbeNow(context.Background(), user, key, caps); err != nil {
		t.Fatal(err)
	}
	info, ok := m.WarmProbeInfo(user, caps, key)
	if !ok || info.Video() == nil || info.Video().Width != 320 || info.Estimated {
		t.Fatalf("published probe ok=%v estimated=%v info=%+v", ok, info.Estimated, info)
	}
	first := m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}]
	if err := m.ParityProbeNow(context.Background(), user, key, caps); err != nil {
		t.Fatal(err)
	}
	if eng.openCount() != 1 {
		t.Fatalf("second probe opened the file again: %d", eng.openCount())
	}
	detached := m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}]
	if detached == nil || detached == first {
		t.Fatal("probe stayed on the prewarm entry prefetch can delete")
	}
	if !detached.expires.After(time.Now().Add(time.Hour)) {
		t.Fatalf("probe expires too soon: %s", detached.expires)
	}
	// The prewarm goroutine deletes only its own pointer after prefetch fails.
	if m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}] == first {
		delete(m.warm, identity{userID: user.ID, clientID: caps.ClientID, key: key})
	}
	if info, ok := m.WarmProbeInfo(user, caps, key); !ok || info.Video() == nil || info.Video().Width != 320 {
		t.Fatalf("prefetch delete removed the detached probe ok=%v info=%+v", ok, info)
	}
	show := core.ShowKey(20001)
	cat.titles[show] = &core.Title{Key: show, Type: core.TypeShow, Title: "Fixture Show"}
	if err := m.ParityProbeNow(context.Background(), user, show, caps); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.WarmProbeInfo(user, caps, show); ok {
		t.Fatal("a show was probed")
	}
}

func TestParityProbeNowSurvivesTheWarmCap(t *testing.T) {
	key := core.MovieKey(10002)
	title := &core.Title{Key: key, Type: core.TypeMovie, Title: "Fixture Movie 002", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	probed := &core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Width: 320, Height: 180}}}
	m := &Manager{
		now:     time.Now,
		warm:    map[identity]*warmEntry{},
		byIdent: map[identity]*session{},
		deps: Deps{
			Catalog:       &fakeCatalog{titles: map[core.RatingKey]*core.Title{key: title}},
			Resolver:      newFakeResolver("rel"),
			Engine:        newFakeEngine(&fakeRelease{ID: "rel", Name: "parity.mp4", Size: 4096}),
			SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return core.QualityProfile{} },
			NativeProbe: func(context.Context, core.MediaSource) (*core.MediaInfo, error) {
				return probed, nil
			},
		},
	}
	user := &core.User{ID: 1}
	caps := core.ClientCaps{ClientID: "parity-owner", Product: "Plex Web"}
	if err := m.ParityProbeNow(context.Background(), user, key, caps); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	for i := 0; i < 40; i++ {
		k := core.MovieKey(20000 + int64(i))
		m.warm[identity{userID: 1, clientID: "parity-owner", key: k}] = &warmEntry{done: done, expires: time.Now().Add(time.Minute)}
	}
	if m.Prewarm(context.Background(), user, core.MovieKey(30001), caps, core.PrewarmOptions{}) {
		t.Fatal("speculative warm-up started by evicting a fixture probe")
	}
	info, ok := m.WarmProbeInfo(user, caps, key)
	if !ok || info.Video() == nil || info.Video().Width != 320 {
		t.Fatalf("warm cap dropped the fixture probe ok=%v info=%+v", ok, info)
	}
}

func TestParitySettlePendingIgnoresFinishedAndExpiredProbes(t *testing.T) {
	finished := make(chan struct{})
	close(finished)
	m := &Manager{now: time.Now, warm: map[identity]*warmEntry{
		{userID: 1, clientID: "a", key: core.MovieKey(1)}: {done: finished, expires: time.Now().Add(time.Minute)},
		{userID: 1, clientID: "b", key: core.MovieKey(2)}: {done: make(chan struct{}), expires: time.Now().Add(-time.Second)},
	}}
	if err := m.ParitySettlePending(context.Background()); err != nil {
		t.Fatal(err)
	}
}
