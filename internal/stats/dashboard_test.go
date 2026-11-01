package stats

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

func TestDashboardBandwidthBuckets(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 4, 5, 0, time.UTC)
	clock := now
	d := NewDashboard(DashboardOptions{Now: func() time.Time { return clock }, Loc: time.UTC})
	d.RecordBytes(1, "owner", "web", "Chrome", "Chrome", false, 100)
	d.RecordBytes(1, "owner", "web", "Chrome", "Chrome", false, 50)
	clock = now.Add(2 * time.Second)
	d.RecordBytes(1, "owner", "tv", "Apple TV", "tvOS", true, 20)

	sec := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}})
	if sec.TotalSize != 2 {
		t.Fatalf("seconds: %+v", sec)
	}
	if sec.Rows[0].Bytes != 150 || sec.Rows[0].Lan != 0 || sec.Rows[1].Bytes != 20 || sec.Rows[1].Lan != 1 {
		t.Fatalf("rows: %+v", sec.Rows)
	}
	if len(sec.Devices) != 2 || len(sec.Accounts) != 1 || sec.Accounts[0].Name != "owner" {
		t.Fatalf("lookup: %+v %+v", sec.Devices, sec.Accounts)
	}

	hour := d.Bandwidth(Query{Timespans: []int{hoststat.SpanHour}})
	if hour.TotalSize != 2 {
		t.Fatalf("hour series: %+v", hour)
	}
	lan := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}, HasLan: true, Lan: 1})
	if lan.TotalSize != 1 || lan.Rows[0].Bytes != 20 {
		t.Fatalf("lan filter: %+v", lan)
	}
	none := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}, HasAccount: true, AccountID: 99})
	if none.TotalSize != 0 || len(none.Devices) != 0 || len(none.Accounts) != 0 {
		t.Fatalf("unknown account: %+v", none)
	}
	page := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}, Offset: 1, Limit: 1, HasLimit: true})
	if page.TotalSize != 2 || len(page.Rows) != 1 || len(page.Devices) != 1 {
		t.Fatalf("page: %+v", page)
	}
	empty := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}, Limit: 0, HasLimit: true})
	if empty.TotalSize != 2 || len(empty.Rows) != 0 || len(empty.Devices) != 0 {
		t.Fatalf("non-positive limit: %+v", empty)
	}
}

func TestDashboardMediaRollups(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 4, 5, 0, time.UTC)
	d := NewDashboard(DashboardOptions{Now: func() time.Time { return now }, Loc: time.UTC})
	d.RecordPlay(1, "owner", "web", 1, 1000)
	d.RecordPlay(1, "owner", "web", 1, 1000)
	d.RecordPlay(1, "owner", "web", 4, 2000)
	got := d.Media(Query{Timespans: []int{hoststat.SpanHour, hoststat.SpanDay}})
	if got.TotalSize != 4 { // two types × two spans
		t.Fatalf("media: %+v", got)
	}
	var movies, episodes int64
	for _, row := range got.Media {
		if row.Timespan != hoststat.SpanHour {
			continue
		}
		switch row.MetadataType {
		case 1:
			movies = row.Count
		case 4:
			episodes = row.Count
		}
	}
	if movies != 2 || episodes != 1 {
		t.Fatalf("counts movie=%d episode=%d", movies, episodes)
	}
	if len(d.Media(Query{Timespans: []int{hoststat.SpanSec}}).Media) != 0 {
		t.Fatal("media does not keep per-second rows")
	}
}

func TestDashboardAnonymousDevice(t *testing.T) {
	d := NewDashboard(DashboardOptions{Now: func() time.Time { return time.Unix(1, 0) }, Loc: time.UTC})
	devs := d.Devices()
	if len(devs) != 1 || devs[0].ID != 1 || devs[0].ClientIdentifier != "" {
		t.Fatalf("anon: %+v", devs)
	}
	web := d.SeeDevice("web", "Chrome", "Chrome")
	if web.ID != 2 || web.Name != "Chrome" {
		t.Fatalf("web: %+v", web)
	}
	if _, ok := d.Device(99); ok {
		t.Fatal("unknown device")
	}
}

func TestDashboardSecondsAreWallHour(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := start
	d := NewDashboard(DashboardOptions{Now: func() time.Time { return clock }, Loc: time.UTC})
	d.RecordBytes(1, "owner", "web", "Chrome", "Chrome", false, 10)
	clock = start.Add(2 * time.Hour)
	d.RecordBytes(1, "owner", "web", "Chrome", "Chrome", false, 20)
	sec := d.Bandwidth(Query{Timespans: []int{hoststat.SpanSec}})
	if sec.TotalSize != 1 || sec.Rows[0].Bytes != 20 {
		t.Fatalf("old second dropped: %+v", sec)
	}
}
