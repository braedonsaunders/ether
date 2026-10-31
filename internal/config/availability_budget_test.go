package config

import "testing"

func TestAvailabilityBudgetPresets(t *testing.T) {
	base := DefaultAvailability()
	if !base.LegacyDefaultBudgets() {
		t.Fatal("v0.2.57 defaults should move to the scaled preset")
	}
	unlimited := []IndexerCap{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	got := base.Resolved(unlimited)
	if got.Effort != "normal" || got.FeedRequests != 8000 || got.MaxPages != 8 || got.PollMinutes != 15 {
		t.Fatalf("%+v", got)
	}
	capped := base.Resolved([]IndexerCap{{ID: "a", DailyLimit: 100}})
	if capped.FeedRequests != 20 {
		t.Fatalf("background share = %d", capped.FeedRequests)
	}
	custom := base
	custom.FeedRequests = 428
	kept := custom.Resolved(unlimited)
	if kept.Effort != "custom" || kept.FeedRequests != 428 {
		t.Fatalf("custom preset %+v", kept)
	}
}
