package sessions

import (
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestTimelineQueueRenewsSelectedItemPeriodically(t *testing.T) {
	now := time.Unix(10000, 0)
	qs := NewPlayQueues()
	qs.Configure(func() time.Time { return now }, nil)
	key := core.MovieKey(1)
	next := core.MovieKey(2)
	q, err := qs.CreateWith(1, "client", []core.RatingKey{key, next}, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(item int, wantUpdate time.Time) {
		t.Helper()
		if got := qs.TimelineQueue(1, "client", key, strconv.FormatInt(q.Items[item].ID, 10), true); got != q.Items[item].Key {
			t.Fatalf("rating key = %v, want %v", got, q.Items[item].Key)
		}
		updated, ok := qs.Get(q.ID, 1)
		if !ok || updated.Selected != item || !updated.UpdatedAt.Equal(wantUpdate) {
			t.Fatalf("queue selected=%d updated=%v, want %d at %v", updated.Selected, updated.UpdatedAt, item, wantUpdate)
		}
	}
	now = now.Add(time.Second)
	check(0, q.UpdatedAt)
	check(1, now)
	now = now.Add(queueCacheTTL + time.Second)
	check(1, now)
}
