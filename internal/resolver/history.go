package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// HistoryRecord is one persisted resolution outcome.
type HistoryRecord struct {
	ID        int64          `json:"id"`
	At        time.Time      `json:"at"`
	TitleKey  core.RatingKey `json:"titleKey,string"`
	TitleName string         `json:"titleName"`
	Profile   string         `json:"profile,omitempty"`
	// Max-height ceilings behind Profile (configured / client caps /
	// effective). ProfileMaxHeight carries presence: nil on records
	// written before the configured ceiling was tracked (unknown), a
	// pointer to 0 for an explicitly unlimited ("Any") profile.
	ProfileMaxHeight   *int   `json:"profileMaxHeight,omitempty"`
	ClientMaxHeight    int    `json:"clientMaxHeight,omitempty"`
	EffectiveMaxHeight int    `json:"effectiveMaxHeight,omitempty"`
	ReleaseID          string `json:"releaseId,omitempty"`
	ReleaseName        string `json:"releaseName,omitempty"`
	Indexer            string `json:"indexer,omitempty"`
	Size               int64  `json:"size,omitempty"`
	Error              string `json:"error,omitempty"`
	Failover           bool   `json:"failover,omitempty"`
	Pinned             bool   `json:"pinned,omitempty"` // the owner's pin was used
	Candidates         int    `json:"candidates"`
	// Trace is the full decision (candidates capped at 50); only
	// HistoryEntry fills it.
	Trace *Trace `json:"trace,omitempty"`
}

// HistoryFilter narrows History. Zero values match everything.
type HistoryFilter struct {
	TitleKey core.RatingKey
	Limit    int // default 50, at most 500
	Offset   int
}

// compactTrace bounds a trace for storage: the winner, the pinned release
// and attempted candidates are kept ahead of untried and statically rejected ones.
func compactTrace(tr Trace) Trace {
	if len(tr.Candidates) <= maxTraceCandidates {
		return tr
	}
	rank := func(c CandidateTrace) int {
		switch {
		case c.ReleaseID != "" && c.ReleaseID == tr.Winner:
			return 0
		case c.Pinned:
			return 1
		case c.Attempted || c.HealthChecked || c.FetchDuration > 0 || c.HealthDuration > 0:
			return 2
		case !c.Rejected:
			return 3
		}
		return 4
	}
	cands := append([]CandidateTrace(nil), tr.Candidates...)
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i]) < rank(cands[j]) })
	tr.Omitted = len(cands) - maxTraceCandidates
	tr.Candidates = cands[:maxTraceCandidates]
	return tr
}

func historyFromTrace(tr Trace) HistoryRecord {
	h := HistoryRecord{At: tr.At, TitleKey: tr.TitleKey, TitleName: tr.TitleName, Profile: tr.Profile,
		ReleaseID: tr.Winner, Error: tr.Err, Failover: tr.Failover, Pinned: tr.PinStatus == "used",
		Candidates: len(tr.Candidates) + tr.Omitted}
	if tr.Limits != nil {
		h.ProfileMaxHeight, h.ClientMaxHeight, h.EffectiveMaxHeight = tr.Limits.ProfileConfiguredMaxHeight, tr.Limits.ClientMaxHeight, tr.Limits.ProfileMaxHeight
	}
	for _, c := range tr.Candidates {
		if c.ReleaseID != "" && c.ReleaseID == tr.Winner {
			h.ReleaseName, h.Indexer, h.Size = c.Name, c.Indexer, c.Size
			break
		}
	}
	return h
}

// recordHistory persists one resolution outcome and prunes rows beyond the
// retention bounds (count and age) in the same pass.
func (s *Service) recordHistory(ctx context.Context, tr Trace) {
	tr = compactTrace(tr)
	h := historyFromTrace(tr)
	if s.db == nil {
		s.con.mu.Lock()
		s.con.historySeq++
		h.ID = s.con.historySeq
		h.Trace = &tr
		s.con.history = append(s.con.history, h)
		cutoff := s.now().Add(-s.con.historyAge)
		keep := s.con.history[:0]
		for _, x := range s.con.history {
			if !x.At.Before(cutoff) {
				keep = append(keep, x)
			}
		}
		if over := len(keep) - s.con.historyMax; over > 0 {
			keep = keep[over:]
		}
		s.con.history = append([]HistoryRecord(nil), keep...)
		s.con.mu.Unlock()
		return
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var id int64
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO resolver_history(resolved_at, rating_key, title_name, profile, release_id, release_name, indexer, size, error, failover, pinned, candidates, trace)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		h.At.Unix(), int64(h.TitleKey), h.TitleName, h.Profile, h.ReleaseID, h.ReleaseName, h.Indexer, h.Size,
		h.Error, boolInt(h.Failover), boolInt(h.Pinned), h.Candidates, string(raw)).Scan(&id)
	if err != nil {
		s.log.Debug("resolver history not saved", "err", err)
		return
	}
	// Ids only grow, so the newest historyMax rows are those above
	// id-historyMax; both bounds use an index.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_history WHERE id<=? OR resolved_at<?`,
		id-int64(s.con.historyMax), s.now().Add(-s.con.historyAge).Unix()); err != nil {
		s.log.Debug("resolver history not pruned", "err", err)
	}
}

const historyColumns = `id, resolved_at, rating_key, title_name, profile, release_id, release_name, indexer, size, error, failover, pinned, candidates`

func scanHistory(r rowScanner, withTrace bool) (HistoryRecord, error) {
	var h HistoryRecord
	var at, key int64
	var failover, pinned int
	dest := []any{&h.ID, &at, &key, &h.TitleName, &h.Profile, &h.ReleaseID, &h.ReleaseName, &h.Indexer, &h.Size, &h.Error, &failover, &pinned, &h.Candidates}
	var raw string
	if withTrace {
		dest = append(dest, &raw)
	}
	if err := r.Scan(dest...); err != nil {
		return HistoryRecord{}, err
	}
	h.At = time.Unix(at, 0)
	h.TitleKey = core.RatingKey(key)
	h.Failover, h.Pinned = failover == 1, pinned == 1
	if withTrace && raw != "" {
		var tr Trace
		if err := json.Unmarshal([]byte(raw), &tr); err == nil {
			h.Trace = &tr
			// The ceilings live only in the trace JSON (no new columns);
			// surface them on the record HistoryEntry returns.
			if tr.Limits != nil {
				h.ProfileMaxHeight, h.ClientMaxHeight, h.EffectiveMaxHeight = tr.Limits.ProfileConfiguredMaxHeight, tr.Limits.ClientMaxHeight, tr.Limits.ProfileMaxHeight
			}
		}
	}
	return h, nil
}

// History lists persisted resolution outcomes, newest first, without
// their traces (the stored trace JSON is read only to surface its
// max-height ceilings on each record), plus the total matching count.
func (s *Service) History(ctx context.Context, f HistoryFilter) ([]HistoryRecord, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if s.db == nil {
		s.con.mu.Lock()
		var all []HistoryRecord
		for i := len(s.con.history) - 1; i >= 0; i-- {
			h := s.con.history[i]
			if f.TitleKey != 0 && h.TitleKey != f.TitleKey {
				continue
			}
			h.Trace = nil
			all = append(all, h)
		}
		s.con.mu.Unlock()
		total := len(all)
		if f.Offset >= len(all) {
			return []HistoryRecord{}, total, nil
		}
		all = all[f.Offset:]
		if len(all) > f.Limit {
			all = all[:f.Limit]
		}
		return all, total, nil
	}
	cond, args := "", []any{}
	if f.TitleKey != 0 {
		cond, args = " WHERE rating_key=?", append(args, int64(f.TitleKey))
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resolver_history`+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// The ceilings live only in the stored trace JSON (no columns of
	// their own), so scan it in and drop the trace itself: the listing
	// stays trace-free, as documented.
	rows, err := s.db.QueryContext(ctx, `SELECT `+historyColumns+`, trace FROM resolver_history`+cond+
		` ORDER BY resolved_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []HistoryRecord{}
	for rows.Next() {
		h, err := scanHistory(rows, true)
		if err != nil {
			return nil, 0, err
		}
		h.Trace = nil
		out = append(out, h)
	}
	return out, total, rows.Err()
}

// HistoryEntry returns one history record with its full trace, or
// core.ErrNotFound.
func (s *Service) HistoryEntry(ctx context.Context, id int64) (HistoryRecord, error) {
	if s.db == nil {
		s.con.mu.Lock()
		defer s.con.mu.Unlock()
		for _, h := range s.con.history {
			if h.ID == id {
				return h, nil
			}
		}
		return HistoryRecord{}, core.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+historyColumns+`, trace FROM resolver_history WHERE id=?`, id)
	h, err := scanHistory(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryRecord{}, core.ErrNotFound
	}
	return h, err
}
