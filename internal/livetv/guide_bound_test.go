package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCurrentProgrammeDoesNotScanTheGuide(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	ctx := context.Background()
	const sourcesN = 4
	const channelsN = 6
	const history = 2000
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	encodedBySlot := make([][]byte, history)
	startMSBySlot := make([]int64, history)
	endMSBySlot := make([]int64, history)
	for n := 0; n < history; n++ {
		from := now.Add(-time.Duration(history-n) * time.Hour)
		programme := Programme{Start: from, End: from.Add(time.Hour), Item: Item{Title: "Old"}}
		if n == history-1 {
			programme = Programme{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Item: Item{Title: "On now"}}
		}
		encoded, err := json.Marshal(programme)
		if err != nil {
			t.Fatal(err)
		}
		encodedBySlot[n] = encoded
		startMSBySlot[n] = programme.Start.UnixMilli()
		endMSBySlot[n] = programme.End.UnixMilli()
	}
	for src := 0; src < sourcesN; src++ {
		sourceID := fmt.Sprintf("src%d", src)
		for ch := 0; ch < channelsN; ch++ {
			id := fmt.Sprintf("s%d-c%d", src, ch)
			channel := Channel{ID: id, Number: fmt.Sprintf("%d", ch+1), Name: id, Kind: "iptv", Enabled: true, SourceID: sourceID, GuideID: id}
			body, err := json.Marshal(channel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, id, body); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < history; n++ {
				if _, err = stmt.ExecContext(ctx, sourceID, id, startMSBySlot[n], endMSBySlot[n], encodedBySlot[n]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	owner := &core.User{ID: 1, IsOwner: true}
	start := time.Now()
	channels, err := s.AllowedChannels(ctx, owner)
	if err != nil || len(channels) != sourcesN*channelsN {
		t.Fatalf("channels %d %v", len(channels), err)
	}
	if elapsed := time.Since(start); elapsed > guideBound(400*time.Millisecond) {
		t.Fatalf("allowed channels scanned the guide: %s", elapsed)
	}
	start = time.Now()
	hub, err := s.HomeHub(ctx, owner, 24)
	if err != nil || len(hub.Items) == 0 || hub.Items[0].Title != "On now" {
		t.Fatalf("home hub %+v %v", hub.Items, err)
	}
	if elapsed := time.Since(start); elapsed > guideBound(400*time.Millisecond) {
		t.Fatalf("plex on-now hub scanned the guide: %s", elapsed)
	}
	memo := WithChannelMemo(ctx)
	start = time.Now()
	for n := 0; n < 40; n++ {
		ok, err := s.ChannelPermitted(memo, owner, channels[0].ID)
		if err != nil || !ok {
			t.Fatalf("channel permit %v %v", ok, err)
		}
	}
	if elapsed := time.Since(start); elapsed > guideBound(50*time.Millisecond) {
		t.Fatalf("repeat channel checks scanned the guide: %s", elapsed)
	}
	before := s.policyReads.Load()
	start = time.Now()
	nowChannels, programmes, err := s.OnNow(ctx, owner, now, now.Add(24*time.Hour))
	if err != nil || len(nowChannels) != sourcesN*channelsN {
		t.Fatalf("on now channels %d %v", len(nowChannels), err)
	}
	if reads := s.policyReads.Load() - before; reads != 1 {
		t.Fatalf("on now read policy %d times", reads)
	}
	perChannel := map[string]int{}
	for _, programme := range programmes {
		perChannel[programme.ChannelID]++
		if perChannel[programme.ChannelID] > 2 {
			t.Fatalf("on now loaded %d programmes for %s", perChannel[programme.ChannelID], programme.ChannelID)
		}
	}
	if elapsed := time.Since(start); elapsed > guideBound(400*time.Millisecond) {
		t.Fatalf("on now scanned the guide: %s", elapsed)
	}
}
