package plex

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSupersededEncoderLifecycle(t *testing.T) {
	for _, reused := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "reused"}[reused], func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			tr := f.srv.d.Transcoder.(*issue94Transcoder)
			start := func(play, trans string) {
				h := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", play, "Accept", "application/json"}
				for _, path := range []string{"decision?", "start.m3u8?"} {
					r := f.get(t, "/video/:/transcode/universal/"+path+issue94Query(trans, 3000), h...)
					if r.Code != 200 {
						t.Fatalf("%s %d", path, r.Code)
					}
				}
			}
			start("old", "old-trans")
			old, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if !ok {
				t.Fatal("no source")
			}
			newID := "new-trans"
			if reused {
				newID = "old-trans"
			}
			start("new", newID)
			current, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if !ok || current.ID != old.ID {
				t.Fatal("source not reused")
			}
			r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=stopped&time=1000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "old")
			if r.Code != 200 {
				t.Fatal(r.Code)
			}
			for i := 0; i < 2; i++ {
				f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=2000", "X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "new")
			}
			if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
				t.Fatal("source stopped")
			}
			if _, ok := tr.Session(newID); !ok {
				t.Fatal("replacement stopped")
			}
			if !reused {
				if _, ok := tr.Session("old-trans"); ok {
					t.Fatal("old encoder remains")
				}
				if _, ok := f.srv.transcodes.get("old-trans"); ok {
					t.Fatal("old entry remains")
				}
			}
		})
	}
}

type lifecycleBlockingEncoder struct {
	*issue94Transcoder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *lifecycleBlockingEncoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.issue94Transcoder.StartHLS(ctx, r)
}
func TestLifecycleBlockedStartHandoff(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			b := &lifecycleBlockingEncoder{issue94Transcoder: f.srv.d.Transcoder.(*issue94Transcoder), entered: make(chan struct{}), release: make(chan struct{})}
			f.srv.d.Transcoder = b
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(b.release) }) }
			defer release()
			headers := func(id string) []string {
				return []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Accept", "application/json"}
			}
			request := func(part, id, play string) int {
				return f.get(t, "/video/:/transcode/universal/"+part+"?"+issue94Query(id, 3000), headers(play)...).Code
			}
			if got := request("decision", "old-trans", "old"); got != 200 {
				t.Fatal(got)
			}
			oldDone := make(chan int, 1)
			go func() { oldDone <- request("start.m3u8", "old-trans", "old") }()
			select {
			case <-b.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("start not entered")
			}
			newID := "new-trans"
			if shared {
				newID = "old-trans"
			}
			newDone := make(chan int, 1)
			go func() {
				if got := request("decision", newID, "new"); got != 200 {
					newDone <- got
					return
				}
				newDone <- request("start.m3u8", newID, "new")
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				waiting := f.srv.playbackLifecycle.References(fmt.Sprintf("owner/%d/phone", ownerUser.ID)) >= 2
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("replacement did not wait on blocked start")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case got := <-newDone:
				t.Fatalf("replacement escaped start serialization: %d", got)
			default:
			}
			release()
			if got := <-oldDone; got != 200 {
				t.Fatal(got)
			}
			if got := <-newDone; got != 200 {
				t.Fatal(got)
			}
			// An old start queued after the authoritative replacement cannot revive it.
			if got := request("start.m3u8", "old-trans", "old"); got != http.StatusGone {
				t.Fatalf("late start=%d", got)
			}
			for _, path := range []string{"/video/:/transcode/universal/stop?session=old-trans", "/:/timeline?ratingKey=1000000000000550&state=stopped"} {
				if r := f.get(t, path, headers("old")...); r.Code != 200 {
					t.Fatal(r.Code)
				}
			}
			if _, ok := b.Session(newID); !ok {
				t.Fatal("replacement encoder stopped")
			}
			if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
				t.Fatal("replacement source stopped")
			}
			if !shared {
				if _, ok := b.Session("old-trans"); ok {
					t.Fatal("old encoder remains")
				}
			}
		})
	}
}

func TestLifecycleForwardedStaleStop(t *testing.T) {
	f, m := issue94Fixture(t, true, "open")
	h := func(id string) []string {
		return []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Accept", "application/json"}
	}
	for _, id := range []string{"old", "new"} {
		for _, part := range []string{"decision", "start.m3u8"} {
			if r := f.get(t, "/video/:/transcode/universal/"+part+"?"+issue94Query(id, 3000), h(id)...); r.Code != 200 {
				t.Fatal(r.Code)
			}
		}
		if id == "old" {
			f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1", h(id)...)
		}
	}
	f.srv.endLocalSession(trackID(ownerUser.ID, "phone", "old"), time.Second)
	if _, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key); !ok {
		t.Fatal("forwarded old stop killed current source")
	}
	if _, ok := f.srv.d.Transcoder.Session("old"); ok {
		t.Fatal("forwarded old stop left encoder")
	}
	if _, ok := f.srv.d.Transcoder.Session("new"); !ok {
		t.Fatal("forwarded old stop killed new encoder")
	}
}

func TestLifecycleStopBetweenDecisionAndStart(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprint(omitted), func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			h := func(id string) []string {
				return []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", id, "Accept", "application/json"}
			}
			for _, part := range []string{"decision", "start.m3u8"} {
				if r := f.get(t, "/video/:/transcode/universal/"+part+"?"+issue94Query("old-trans", 3000), h("old")...); r.Code != 200 {
					t.Fatal(r.Code)
				}
			}
			if r := f.get(t, "/video/:/transcode/universal/decision?"+issue94Query("new-trans", 3000), h("new")...); r.Code != 200 {
				t.Fatal(r.Code)
			}
			source, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if !ok {
				t.Fatal("missing prepared replacement")
			}
			stopID := "old"
			if omitted {
				stopID = ""
			}
			if r := f.get(t, "/video/:/transcode/universal/stop?session=old-trans", h(stopID)...); r.Code != 200 {
				t.Fatal(r.Code)
			}
			current, ok := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if !ok || current.ID != source.ID {
				t.Fatal("old encoder stop killed prepared replacement source")
			}
			if _, ok := f.srv.d.Transcoder.Session("old-trans"); ok {
				t.Fatal("old encoder remains")
			}
			if r := f.get(t, "/video/:/transcode/universal/start.m3u8?"+issue94Query("new-trans", 3000), h("new")...); r.Code != 200 {
				t.Fatal(r.Code)
			}
			if _, ok := f.srv.d.Transcoder.Session("new-trans"); !ok {
				t.Fatal("replacement did not start")
			}
		})
	}
}
