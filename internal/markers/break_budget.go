package markers

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// BreakScanPolicy bounds extra pre-air source reads. Zero uses defaults;
// negative daily/release limits disable full scans. Chapters/cache bypass it.
type BreakScanPolicy struct {
	DailyBytes        int64 `json:"dailyBytes,omitempty"`
	ReleaseBytes      int64 `json:"releaseBytes,omitempty"`
	LargeReleaseBytes int64 `json:"largeReleaseBytes,omitempty"`
	AllowLarge        bool  `json:"allowLarge,omitempty"`
}

var ErrBreakBandwidth = errors.New("act-break pre-air bandwidth budget unavailable")

func init() {
	store.Register("markers_breaks", 2, `CREATE TABLE markers_breaks_budget(day TEXT PRIMARY KEY,reserved_bytes INTEGER NOT NULL,read_bytes INTEGER NOT NULL);`)
}
func (p BreakScanPolicy) limits() (int64, int64, int64) {
	if p.DailyBytes == 0 {
		p.DailyBytes = 1 << 30
	}
	if p.ReleaseBytes == 0 {
		p.ReleaseBytes = 256 << 20
	}
	if p.LargeReleaseBytes <= 0 {
		p.LargeReleaseBytes = 1 << 30
	}
	return p.DailyBytes, p.ReleaseBytes, p.LargeReleaseBytes
}
func (a *BreakAnalyzer) reserveScan(ctx context.Context, in BreakInput) (int64, error) {
	p := BreakScanPolicy{}
	if a.ScanPolicy != nil {
		p = a.ScanPolicy()
	}
	daily, release, large := p.limits()
	size := in.Info.Size
	// Reserve two file reads plus bounded head/tail overhead. Unknown sizes fail
	// closed. This is deliberately conservative for muxed media and source retries.
	if size <= 0 || daily <= 0 || release <= 0 || (!p.AllowLarge && size > large) || size > (release-(8<<20))/2 {
		return 0, ErrBreakBandwidth
	}
	amount := 2*size + (8 << 20)
	if amount > daily {
		return 0, ErrBreakBandwidth
	}
	day := in.budgetDay
	if day == "" {
		day = time.Now().UTC().Format("2006-01-02")
	}
	result, err := a.DB.ExecContext(ctx, `INSERT INTO markers_breaks_budget(day,reserved_bytes,read_bytes) VALUES(?,?,0) ON CONFLICT(day) DO UPDATE SET reserved_bytes=markers_breaks_budget.reserved_bytes+excluded.reserved_bytes WHERE markers_breaks_budget.reserved_bytes+excluded.reserved_bytes<=?`, day, amount, daily)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrBreakBandwidth
	}
	return amount, nil
}
