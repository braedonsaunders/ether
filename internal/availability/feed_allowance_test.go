package availability

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestFeedAllowanceCountsSplitFeeds(t *testing.T) {
	feeds := []core.ReleaseFeed{
		{ID: "a#tv", Supported: true},
		{ID: "a#movie", Supported: true},
		{ID: "b", Supported: true},
		{ID: "builtin", Supported: false},
	}
	cfg := config.Availability{Monitor: true, PollMinutes: 15, FeedRequests: 400}
	required, allowance, shortfall := feedAllowance(feeds, nil, cfg)
	if required != 3*96 || allowance != 400 || shortfall != 0 {
		t.Fatalf("required=%d allowance=%d shortfall=%d", required, allowance, shortfall)
	}

	cfg.FeedRequests = 200
	required, allowance, shortfall = feedAllowance(feeds, nil, cfg)
	if required != 288 || allowance != 200 || shortfall != 88 {
		t.Fatalf("shortfall: required=%d allowance=%d shortfall=%d", required, allowance, shortfall)
	}

	// Selecting one side counts only that side.
	required, _, shortfall = feedAllowance(feeds, []string{"a#tv"}, cfg)
	if required != 96 || shortfall != 0 {
		t.Fatalf("selected side: required=%d shortfall=%d", required, shortfall)
	}
	// Selecting the indexer covers both of its split sides.
	required, _, _ = feedAllowance(feeds, []string{"a"}, cfg)
	if required != 192 {
		t.Fatalf("selected indexer: required=%d", required)
	}

	cfg.Monitor = false
	if required, _, shortfall := feedAllowance(feeds, nil, cfg); required != 0 || shortfall != 0 {
		t.Fatalf("monitoring off: required=%d shortfall=%d", required, shortfall)
	}
}

func TestFeedAllowanceWarningText(t *testing.T) {
	msg := feedAllowanceWarning(480, 400)
	for _, want := range []string{"480", "400", "allowance", "reduce"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q lacks %q", msg, want)
		}
	}
}
