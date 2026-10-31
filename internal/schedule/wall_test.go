package schedule

import (
	"context"
	"testing"
	"time"
)

func TestWallDeadlineResumeAndClockCorrections(t *testing.T) {
	start := time.Now()
	d := New(start, 24*time.Hour)
	if d.at != d.at.Round(0) {
		t.Fatal("deadline retained monotonic time")
	}
	if got := d.Remaining(start.Add(72 * time.Hour)); got != 0 {
		t.Fatalf("suspended past deadline: %v", got)
	}
	d = New(start.Add(72*time.Hour), 24*time.Hour)
	if got := d.Remaining(start.Add(72 * time.Hour)); got != 24*time.Hour {
		t.Fatal("missed intervals replayed", got)
	}
	if got := d.Remaining(start); got != 24*time.Hour {
		t.Fatal("backwards correction not rebased", got)
	}
	if got := d.Remaining(start.Add(time.Hour)); got != 23*time.Hour {
		t.Fatal("deadline repeatedly postponed", got)
	}
}

func TestWaitNoticesSleepWithinBoundAndCancels(t *testing.T) {
	now := time.Now()
	calls := 0
	ok := wait(context.Background(), func() time.Time { return now }, 24*time.Hour, func(_ context.Context, d time.Duration) bool {
		calls++
		if d > MaxWait {
			t.Fatal("unbounded elapsed timer", d)
		}
		now = now.Add(72 * time.Hour)
		return true
	})
	if !ok || calls != 1 {
		t.Fatal(ok, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Wait(ctx, time.Now, 0) {
		t.Fatal("canceled overdue job ran")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	ok = wait(ctx, time.Now, time.Hour, func(context.Context, time.Duration) bool { cancel(); return false })
	if ok {
		t.Fatal("cancellation ignored")
	}
}

func TestPersistedSchedulesRecoverWithoutBacklog(t *testing.T) {
	now := time.Now()
	for _, last := range []time.Time{{}, now.Add(-72 * time.Hour), now.Add(72 * time.Hour)} {
		if !Due(now, last, 24*time.Hour) {
			t.Fatal("overdue or clock-corrected schedule stuck", last)
		}
		last = now // successful attempt is persisted by the owning component
		if Due(now, last, 24*time.Hour) {
			t.Fatal("replayed missed interval")
		}
	}
}
