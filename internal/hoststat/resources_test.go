package hoststat

import (
	"testing"
	"time"
)

func TestBucketStart(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 10, 31, 15, 4, 5, 0, loc)
	if got := BucketStart(at, SpanSec, loc); !got.Equal(at.Truncate(time.Second)) {
		t.Fatalf("sec: %v", got)
	}
	if got := BucketStart(at, SpanHour, loc); !got.Equal(time.Date(2026, 10, 31, 15, 0, 0, 0, loc)) {
		t.Fatalf("hour: %v", got)
	}
	if got := BucketStart(at, SpanDay, loc); !got.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, loc)) {
		t.Fatalf("day: %v", got)
	}
	if got := BucketStart(at, SpanWeek, loc); !got.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, loc)) {
		t.Fatalf("iso week: %v", got)
	}
	if got := BucketStart(at, SpanMonth, loc); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("month: %v", got)
	}
	if got := BucketStart(at, SpanYear, loc); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("year: %v", got)
	}
}

func TestResourcesRollsSecondsIntoHours(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := start
	r := NewResources(Options{Now: func() time.Time { return clock }, Loc: time.UTC})
	for i := 0; i < 3602; i++ {
		clock = start.Add(time.Duration(i) * time.Second)
		r.Add(Sample{At: clock, HostCPU: 10})
	}
	secs := r.Query(SpanSec, 0, 0, 0, false, false, false)
	if len(secs) < 3600 || len(secs) > 3601 {
		t.Fatalf("seconds kept %d", len(secs))
	}
	hours := r.Query(SpanHour, 0, 0, 0, false, false, false)
	if len(hours) == 0 {
		t.Fatal("expected rolled-up hour rows")
	}
	if hours[0].HostCPU != 10 {
		t.Fatalf("hour average %v", hours[0].HostCPU)
	}
	// A timespan the sampler does not keep stays empty.
	if got := r.Query(5, 0, 0, 0, false, false, false); len(got) != 0 {
		t.Fatalf("span 5: %d", len(got))
	}
}

func TestResourcesAverages(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r := NewResources(Options{Now: func() time.Time { return now }, Loc: time.UTC})
	r.Add(Sample{At: now, HostCPU: 10})
	r.Add(Sample{At: now.Add(time.Second), HostCPU: 30})
	hours := r.Query(SpanHour, 0, 0, 0, false, false, false)
	if len(hours) != 1 || hours[0].HostCPU != 20 {
		t.Fatalf("average: %+v", hours)
	}
}

func TestResourcesAtFilter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r := NewResources(Options{Now: func() time.Time { return now }, Loc: time.UTC})
	r.Add(Sample{At: now, HostCPU: 1})
	r.Add(Sample{At: now.Add(2 * time.Second), HostCPU: 2})
	r.Add(Sample{At: now.Add(4 * time.Second), HostCPU: 3})
	min := now.Add(2 * time.Second).Unix()
	got := r.Query(SpanSec, min, 0, 0, false, true, false)
	if len(got) != 2 || got[0].HostCPU != 2 {
		t.Fatalf("at>= : %+v", got)
	}
	exact := now.Add(2 * time.Second).Unix()
	got = r.Query(SpanSec, 0, 0, exact, true, false, false)
	if len(got) != 1 || got[0].HostCPU != 2 {
		t.Fatalf("at= : %+v", got)
	}
}
