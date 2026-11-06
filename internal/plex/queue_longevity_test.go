package plex

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Advancing the server clock exercises the real queue TTL without claiming
// that a few seconds of execution is a year of real device playback.
func TestQueueLongevityActiveTimelineSurvivesExpiry(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "standalone"
		if shared {
			name = "shared-cold-cache"
		}
		t.Run(name, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(fixedNow.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			f := newFixture(func(d *Deps) {
				d.Now, d.DisableEnrichment, d.Hooks = now, true, Hooks{}
				if shared {
					d.DB = store.OpenTest(t)
					d.Cluster = clusterbus.NewHub().Node("longevity")
				}
			})
			defer f.srv.Close()
			qs := f.srv.queues
			active, err := qs.createWith(ownerUser.ID, "active", []core.RatingKey{bbE1.Key, bbE2.Key}, now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			idle, err := qs.createWith(ownerUser.ID, "abandoned", []core.RatingKey{alien.Key}, now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			// Include 30-day, year-end and leap-year crossings. The selected
			// item need not change: long pauses and repeated timelines count.
			for hour := 1; hour <= 24*370; hour++ {
				clock.Add(int64(time.Hour))
				if shared {
					qs.forget(active.id)
				} // a different/cold node
				qs.selectItem(ownerUser.ID, active.items[0].id)
				qs.selectItem(friendUser.ID, idle.items[0].id) // cannot keep another user's queue alive
				qs.expire(now(), 6*time.Hour)
				if _, ok := qs.get(active.id, ownerUser.ID); !ok {
					t.Fatalf("active queue expired after %d hours of valid timelines", hour)
				}
				if hour > 6 {
					if _, ok := qs.get(idle.id, ownerUser.ID); ok {
						t.Fatal("abandoned queue retained")
					}
				}
			}
			clock.Add(int64(7 * time.Hour))
			qs.expire(now(), 6*time.Hour)
			if _, ok := qs.get(active.id, ownerUser.ID); ok {
				t.Fatal("queue retained after timelines stop")
			}
		})
	}
}

// Wall clocks can repeat a value (coarse resolution or a clock adjustment).
// Queue capacity must not turn equal timestamps into random eviction of the
// queue that a client has just created.
func TestQueueCapacityKeepsNewestWhenTimestampsTie(t *testing.T) {
	qs := newPlayQueueStore()
	qs.now = func() time.Time { return fixedNow }
	for i := 0; i < maxQueues+100; i++ {
		q, err := qs.createWith(ownerUser.ID, "burst-"+strconv.Itoa(i), []core.RatingKey{alien.Key}, fixedNow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := qs.get(q.id, ownerUser.ID); !ok {
			t.Fatal("new queue immediately lost")
		}
	}
	for id := int64(1); id <= 100; id++ {
		if _, ok := qs.get(id, ownerUser.ID); ok {
			t.Fatalf("old queue %d kept ahead of newer equal-time queues", id)
		}
	}
	for id := int64(101); id <= int64(maxQueues+100); id++ {
		if _, ok := qs.get(id, ownerUser.ID); !ok {
			t.Fatalf("newer queue %d evicted at equal timestamp", id)
		}
	}
}
