package config

// Availability bounds automatic recent-release monitoring and optional checks.
// Zero budgets disable the corresponding background work.
type Availability struct {
	DisableMovieEvidenceGate bool     `json:"disableMovieEvidenceGate"`
	MovieEvidenceDays        int      `json:"movieEvidenceDays"` // zero preserves the 60-day default
	Monitor                  bool     `json:"monitor"`
	Verify                   bool     `json:"verify"`
	PreferVerified           bool     `json:"preferVerified"`
	NewlyAvailable           bool     `json:"newlyAvailable"`
	IndexerIDs               []string `json:"indexerIds"` // empty = all supported enabled indexers
	Debrid                   bool     `json:"debrid"`
	PollMinutes              int      `json:"pollMinutes"`
	MaxPages                 int      `json:"maxPages"`
	FeedRequests             int      `json:"feedRequests"`  // total per UTC day, across indexers
	CheckRequests            int      `json:"checkRequests"` // indexer + provider API calls per day
	Grabs                    int      `json:"grabs"`
	Checks                   int      `json:"checks"`
	CheckBytes               int64    `json:"checkBytes"`     // conservative wire reservations for validation
	ReservePercent           int      `json:"reservePercent"` // preserve this share of configured indexer caps
	// Effort is gentle, normal, unlimited, or custom. Empty means the
	// v0.2.57 default budgets still apply and should scale; any other
	// stored budget is a custom choice.
	Effort string `json:"effort,omitempty"`
	// CatchUp walks older feed pages. It is optional and never required
	// before a title can appear.
	CatchUp bool `json:"catchUp,omitempty"`
	// DigitalDateEvidence lets a passed digital or physical release date
	// show a movie before a playable file has been seen. Theatrical dates
	// never count.
	DigitalDateEvidence bool `json:"digitalDateEvidence,omitempty"`
}

// IndexerCap is one indexer's own limit. DailyLimit 0 means the indexer
// reports no daily cap ("unlimited"). PageMax is the caps document's page
// size; 0 means it was not reported.
type IndexerCap struct {
	ID         string
	DailyLimit int
	PageMax    int
}

func DefaultAvailability() Availability {
	return Availability{Monitor: true, PollMinutes: 15, MaxPages: 5, FeedRequests: 400, CheckRequests: 100, Grabs: 20, Checks: 20, CheckBytes: 512 << 20, ReservePercent: 80, Debrid: true, PreferVerified: true, NewlyAvailable: true}
}

// EffectiveMovieEvidenceDays preserves legacy configurations.
func (a Availability) EffectiveMovieEvidenceDays() int {
	if a.MovieEvidenceDays <= 0 {
		return 60
	}
	return a.MovieEvidenceDays
}

// LegacyDefaultBudgets reports the v0.2.57 shipped feed allowance.
func (a Availability) LegacyDefaultBudgets() bool {
	return a.Effort == "" && a.FeedRequests == 400 && a.PollMinutes == 15 && a.MaxPages == 5
}

// CustomBudgets reports an owner-chosen allowance. Presets recompute.
func (a Availability) CustomBudgets() bool {
	switch a.Effort {
	case "gentle", "normal", "unlimited":
		return false
	case "custom":
		return true
	default:
		return !a.LegacyDefaultBudgets()
	}
}

// Resolved applies Gentle / Normal / Unlimited. Custom and non-default
// saved numbers are kept. Unlimited indexers get a generous share; a
// configured daily cap contributes only the background portion.
func (a Availability) Resolved(indexers []IndexerCap) Availability {
	if a.CustomBudgets() {
		if a.Effort == "" {
			a.Effort = "custom"
		}
		return a
	}
	effort := a.Effort
	if effort == "" {
		effort = "normal"
	}
	poll, pages, generous := 15, 8, 2000
	switch effort {
	case "gentle":
		poll, pages, generous = 60, 3, 150
	case "unlimited":
		poll, pages, generous = 10, 20, 8000
	}
	a.Effort = effort
	a.PollMinutes = poll
	a.MaxPages = pages
	reserve := min(99, max(0, a.ReservePercent))
	sum := 0
	n := 0
	for _, ix := range indexers {
		n++
		if ix.DailyLimit > 0 {
			usable := ix.DailyLimit * (100 - reserve) / 100
			if usable < 1 {
				usable = 1
			}
			sum += usable
			continue
		}
		sum += generous
	}
	if n == 0 {
		sum = generous
	}
	a.FeedRequests = sum
	return a
}
