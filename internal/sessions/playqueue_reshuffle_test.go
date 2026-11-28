package sessions

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestReshuffleLeavesQueueUntouchedWhenPreallocateFails(t *testing.T) {
	qs := NewPlayQueues()
	qs.Configure(nil, &PlayQueuePersistence{
		Counter: func(context.Context, string, int) (int64, error) { return 0, errors.New("counter down") },
	})
	var items []QueueItem
	for n := 1; n <= 8; n++ {
		items = append(items, QueueItem{ID: int64(n), Key: core.MovieKey(int64(n))})
	}
	q := &PlayQueue{Items: append([]QueueItem(nil), items...), Version: 3}
	qs.Reshuffle(q, true)
	if q.Shuffled || q.Original != nil || q.Version != 3 || !reflect.DeepEqual(q.Items, items) {
		t.Fatalf("failed reshuffle changed the queue: shuffled=%v version=%d items=%v", q.Shuffled, q.Version, q.Items)
	}
}
