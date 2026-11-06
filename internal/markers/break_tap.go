package markers

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("markers_breaks", 3, `CREATE TABLE markers_breaks_observed(release_key TEXT NOT NULL,version INTEGER NOT NULL,duration_ms INTEGER NOT NULL,points TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(release_key,version));`)
}

// PlaybackBreakInput binds playout observations to the same immutable release
// key as pre-air analysis, without performing source reads or resolving media.
func PlaybackBreakInput(ps *core.PlaySession) BreakInput {
	if ps == nil || ps.Source == nil {
		return BreakInput{}
	}
	info := ps.Info
	info.Size = ps.Source.Size()
	id := ps.Source.ReleaseID()
	payload := ""
	if ps.Resolution != nil {
		id = ps.Resolution.Release.ID
		payload = fmt.Sprintf("%x", sha256.Sum256(ps.Resolution.NZB))
	}
	revision := core.SourceRevision(ps)
	return BreakInput{Key: ps.Key, ReleaseID: id, Revision: revision, Info: info, ReleaseKey: fmt.Sprintf("%s:%s:%d", revision, payload, info.Duration.Milliseconds())}
}

// BreakTap samples already decoded limited-range Y and signed little-endian PCM.
// It performs no I/O, allocation per frame, buffering of media or extra decode.
// Every frame samples <=2304 luma values plus its cheap audio RMS envelope.
// Only verified positive points survive a partial viewing; no empty full-scan
// cache entry is created from a tune, seek or interrupted programme.
type BreakTap struct {
	signals                  breakSignals
	blackStart, silenceStart float64
	last                     float64
	started                  bool
	duration                 time.Duration
}

func NewBreakTap(duration time.Duration) *BreakTap {
	return &BreakTap{blackStart: -1, silenceStart: -1, duration: duration}
}
func (t *BreakTap) Frame(at, span time.Duration, y, pcm []byte, width, height int) {
	if width < 1 || height < 1 || len(y) < width*height || span <= 0 {
		return
	}
	start, end := at.Seconds(), (at + span).Seconds()
	if t.started && (start < t.last-.001 || start-t.last > .15) {
		t.blackStart = -1
		t.silenceStart = -1
	}
	t.started = true
	dark, total := 0, 0
	for row := 0; row < height; row += max(1, (height+35)/36) {
		for col := 0; col < width; col += max(1, (width+63)/64) {
			total++
			if y[row*width+col] <= 34 {
				dark++
			}
		}
	}
	black := dark*100 >= total*98
	if black && t.blackStart < 0 {
		t.blackStart = start
	}
	if !black && t.blackStart >= 0 {
		if len(t.signals.black) < 4096 {
			t.signals.black = append(t.signals.black, breakRun{t.blackStart, start})
		}
		t.blackStart = -1
	}
	var sum int64
	count := 0
	for n := 0; n+1 < len(pcm); n += 2 {
		v := int64(int16(binary.LittleEndian.Uint16(pcm[n:])))
		sum += v * v
		count++
	}
	silent := count > 0 && sum/int64(count) <= 33955
	if silent && t.silenceStart < 0 {
		t.silenceStart = start
	}
	if !silent && t.silenceStart >= 0 {
		if len(t.signals.silent) < 4096 {
			t.signals.silent = append(t.signals.silent, breakRun{t.silenceStart, start})
		}
		t.silenceStart = -1
	}
	t.last = end
}
func (t *BreakTap) Points() []BreakPoint {
	signals := t.signals

	if t.silenceStart >= 0 {
		signals.silent = append(append([]breakRun(nil), signals.silent...), breakRun{t.silenceStart, t.last})
	}
	return combineBreakSignals(signals, t.duration)
}

// ObservePoints merges verified facts from partial playout into a separate
// release cache. It never represents a partial/empty viewing as a full scan.
func (a *BreakAnalyzer) ObservePoints(ctx context.Context, in BreakInput, points []BreakPoint) error {
	if in.Revision == "" || in.ReleaseKey == "" || in.Info.Estimated || in.Info.Duration <= 0 || len(points) == 0 {
		return nil
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO markers_breaks_observed VALUES(?,?,?,?,?) ON CONFLICT(release_key,version) DO UPDATE SET updated_at=excluded.updated_at`, in.ReleaseKey, breakVersion, in.Info.Duration.Milliseconds(), "[]", time.Now().Unix())
	if err != nil {
		return err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT points FROM markers_breaks_observed WHERE release_key=? AND version=?`, in.ReleaseKey, breakVersion).Scan(&raw); err != nil {
		return err
	}
	var prior []BreakPoint
	if err = json.Unmarshal([]byte(raw), &prior); err != nil {
		return err
	}
	merged := SelectBreaks(append(prior, points...), in.Info.Duration.Milliseconds(), 256, 60000)
	b, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE markers_breaks_observed SET points=? WHERE release_key=? AND version=?`, string(b), in.ReleaseKey, breakVersion)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CachedBreakPoints returns only locally stored positive evidence; no resolve,
// probe or decode. Full analyses and partial playout facts reinforce each other.
func (a *BreakAnalyzer) CachedBreakPoints(ctx context.Context, in BreakInput) ([]BreakPoint, error) {
	points, _, _, err := a.cachedBreakFacts(ctx, in)
	return points, err
}
func (a *BreakAnalyzer) cachedBreakFacts(ctx context.Context, in BreakInput) ([]BreakPoint, bool, bool, error) {
	rows, err := a.DB.QueryContext(ctx, `SELECT points,1 FROM markers_breaks_cache WHERE release_key=? AND version=? AND duration_ms=? UNION ALL SELECT points,0 FROM markers_breaks_observed WHERE release_key=? AND version=? AND duration_ms=?`, in.ReleaseKey, breakVersion, in.Info.Duration.Milliseconds(), in.ReleaseKey, breakVersion, in.Info.Duration.Milliseconds())
	if err != nil {
		return nil, false, false, err
	}
	defer rows.Close()
	var points []BreakPoint
	full, found := false, false
	for rows.Next() {
		var raw string
		var complete int
		if err = rows.Scan(&raw, &complete); err != nil {
			return nil, false, false, err
		}
		var saved []BreakPoint
		if err = json.Unmarshal([]byte(raw), &saved); err != nil {
			return nil, false, false, err
		}
		if complete == 1 {
			full = true
			found = true
		}
		if len(saved) > 0 {
			found = true
		}
		points = append(points, saved...)
	}
	if err = rows.Err(); err != nil {
		return nil, false, false, err
	}
	return SelectBreaks(points, in.Info.Duration.Milliseconds(), 256, 60000), found, full, nil
}
