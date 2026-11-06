package markers

import (
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestBreakBandwidthPolicyAndChapterBypass(t *testing.T) {
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := BreakAnalyzer{DB: db, FFmpeg: "must-not-run"}
	in := BreakInput{ReleaseKey: "remux", Info: core.MediaInfo{Size: 4550000000, Duration: 22 * time.Minute, Streams: []core.StreamInfo{{Type: "audio"}}}}
	if _, err := a.Analyze(t.Context(), in); !errors.Is(err, ErrBreakBandwidth) {
		t.Fatal(err)
	}
	in.Info.Chapters = []core.Chapter{{Title: "Act 2", Start: 7 * time.Minute}}
	if points, err := a.Analyze(t.Context(), in); err != nil || len(points) != 1 {
		t.Fatal(points, err)
	}
	var used int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(reserved_bytes),0) FROM markers_breaks_budget`).Scan(&used); err != nil || used != 0 {
		t.Fatal(used, err)
	}
	a.ScanPolicy = func() BreakScanPolicy { return BreakScanPolicy{DailyBytes: 30 << 20, ReleaseBytes: 100 << 20} }
	in.Info.Size = 10 << 20
	if _, err := a.reserveScan(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := a.reserveScan(t.Context(), in); !errors.Is(err, ErrBreakBandwidth) {
		t.Fatal("daily budget not enforced", err)
	}
}
func TestBreakTapPartialReuseAndSeekSafety(t *testing.T) {
	tap := NewBreakTap(3 * time.Minute)
	light, dark := make([]byte, 64), make([]byte, 64)
	for i := range light {
		light[i] = 200
		dark[i] = 16
	}
	// Only a partial viewing is available. The second black scene has audio.
	for tick := 600; tick < 1400; tick++ {
		y := light
		pcm := []byte{0xe8, 3}
		if tick >= 700 && tick < 710 {
			y = dark
			pcm = []byte{0, 0}
		}
		if tick >= 1300 && tick < 1310 {
			y = dark
		}
		tap.Frame(time.Duration(tick)*100*time.Millisecond, 100*time.Millisecond, y, pcm, 8, 8)
	}
	points := tap.Points()
	if len(points) != 1 || points[0].AtMS != 70500 {
		t.Fatal(points)
	}
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := BreakAnalyzer{DB: db, ScanPolicy: func() BreakScanPolicy { return BreakScanPolicy{DailyBytes: -1} }}
	in := BreakInput{ReleaseKey: "aired", Revision: "revision", Info: core.MediaInfo{Duration: 3 * time.Minute}}
	if err := a.ObservePoints(t.Context(), in, points); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Analyze(t.Context(), in); err != nil || len(got) != 1 {
		t.Fatal("partial verified facts not reused", got, err)
	}
	if _, err := db.Exec(`INSERT INTO markers_breaks_cache VALUES(?,?,?,?,?)`, in.ReleaseKey, breakVersion, 180000, "[]", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Analyze(t.Context(), in); err != nil || len(got) != 1 {
		t.Fatal("full/partial fact merge lost verified act", got, err)
	}
	seek := NewBreakTap(3 * time.Minute)
	for _, at := range []time.Duration{70 * time.Second, 70100 * time.Millisecond, 75 * time.Second, 75100 * time.Millisecond} {
		seek.Frame(at, 100*time.Millisecond, dark, []byte{0, 0}, 8, 8)
	}
	if got := seek.Points(); len(got) != 0 {
		t.Fatal("seek or unfinished black run invented act", got)
	}
}

func BenchmarkBreakTap(b *testing.B) {
	tap := NewBreakTap(24 * time.Hour)
	y := make([]byte, 1280*720)
	for n := range y {
		y[n] = 120
	}
	pcm := make([]byte, 1600*2*2)
	for n := 0; n < len(pcm); n += 2 {
		pcm[n] = 0xe8
		pcm[n+1] = 3
	}
	span := time.Second / 30
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		tap.Frame(time.Duration(n)*span, span, y, pcm, 1280, 720)
	}
}
