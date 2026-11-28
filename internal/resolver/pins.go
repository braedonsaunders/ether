package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

// Owner pins
//
// A pin selects the source for playback. Resolve validates only that release;
// rejection, proven failed health, blocklisting and failover exclusion are errors,
// never permission to substitute another release. Pins override owner quality
// and identity heuristics, but retain source safety gates below. Client codec
// and HDR support are delivery decisions: playback can remux, transcode or
// tone-map the pinned source, or explain why no delivery path exists.
// Sampling timeouts alone allow an unverified start; real source reads remain
// authoritative and the pin never permits automatic substitution.
//
// An explicit viewer version choice overrides a pin. The logical Auto slot
// still honours it. Offline downloads retain their own profile and ignore pins.
//
// A pin with Fallback is the owner's preferred release. It applies to the same
// requests and, when used, gets the same gates and exemptions as a strict pin,
// except those that exist only because nothing else may play: an inconclusive
// sampling timeout and the remembered-source identity exemption stay strict.
// When the preferred release is missing, rejected or unhealthy, the request
// continues with normal ranking. An explicit version pick bypasses it, even a
// pick of the same release. No outcome is a latch: the next ordinary Auto tries
// the preference again once the existing failure memory has expired.

func init() {
	store.Register("resolver", 12, `ALTER TABLE resolver_pins ADD COLUMN fallback INTEGER NOT NULL DEFAULT 0;`)
}

// PinStatus is the outcome of the last time a pin was tried or checked.
type PinStatus string

const (
	PinHealthy     PinStatus = "healthy"     // selected for playback; LastReason notes unverified starts
	PinUnhealthy   PinStatus = "unhealthy"   // failed a health check or the NZB is gone
	PinRejected    PinStatus = "rejected"    // a hard gate excluded it for a client
	PinUnavailable PinStatus = "unavailable" // a transient failure (provider outage)
)

// Pin is an owner's release pin for a title.
type Pin struct {
	TitleKey      core.RatingKey `json:"titleKey,string"`
	TitleName     string         `json:"titleName"`
	Release       core.Release   `json:"release"`
	Note          string         `json:"note,omitempty"`
	PinnedAt      time.Time      `json:"pinnedAt"`
	LastStatus    PinStatus      `json:"lastStatus,omitempty"`
	LastReason    string         `json:"lastReason,omitempty"`
	LastCheckedAt time.Time      `json:"lastCheckedAt"`
	// Fallback makes the pin a preference: normal ranking takes over when the
	// release cannot be used. False (strict) for every older pin.
	Fallback bool `json:"fallback"`
}

func pinnableKey(k core.RatingKey) bool {
	return k.Valid() && (k.Kind() == core.KindMovie || k.Kind() == core.KindEpisode)
}

// Pin pins rel to a movie or episode, replacing any earlier pin. The full
// release is stored, so the pin works after the release drops out of
// cached search results. The pin is strict; see PinWithFallback.
func (s *Service) Pin(ctx context.Context, key core.RatingKey, titleName string, rel core.Release, note string) (Pin, error) {
	return s.PinWithFallback(ctx, key, titleName, rel, note, false)
}

// PinWithFallback is Pin with the mode: fallback stores a preferred release
// that normal ranking replaces when it cannot be used.
func (s *Service) PinWithFallback(ctx context.Context, key core.RatingKey, titleName string, rel core.Release, note string, fallback bool) (Pin, error) {
	if !pinnableKey(key) {
		return Pin{}, ErrNotPinnable
	}
	if rel.ID == "" {
		return Pin{}, fmt.Errorf("resolver: empty release id")
	}
	if s.isBadRelease(rel) {
		return Pin{}, ErrPinBlocked
	}
	if titleName == "" {
		s.con.mu.Lock()
		titleName = s.con.recent[rel.ID].titleName
		s.con.mu.Unlock()
	}
	note = strings.TrimSpace(note)
	p := Pin{TitleKey: key, TitleName: titleName, Release: rel, Note: note, PinnedAt: s.now(), Fallback: fallback}
	if s.db != nil {
		raw, err := json.Marshal(rel)
		if err != nil {
			return Pin{}, err
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO resolver_pins(rating_key, release_id, release_json, title_name, note, pinned_at, last_status, last_reason, last_checked_at, fallback)
			 VALUES (?,?,?,?,?,?,'','',0,?)
			 ON CONFLICT(rating_key) DO UPDATE SET release_id=excluded.release_id, release_json=excluded.release_json,
			   title_name=excluded.title_name, note=excluded.note, pinned_at=excluded.pinned_at,
			   last_status='', last_reason='', last_checked_at=0, fallback=excluded.fallback`,
			int64(key), rel.ID, string(raw), titleName, note, p.PinnedAt.Unix(), store.BoolInt(fallback))
		if err != nil {
			return Pin{}, err
		}
	}
	s.con.mu.Lock()
	s.con.pins[key] = p
	s.con.mu.Unlock()
	s.InvalidateDetailVariants()
	s.notifyPin(key)
	s.emitPinCreated(p, "owner")
	return p, nil
}

// Unpin removes a title's pin; it reports whether there was one.
func (s *Service) Unpin(ctx context.Context, key core.RatingKey) (bool, error) {
	s.con.mu.Lock()
	prev, had := s.con.pins[key]
	delete(s.con.pins, key)
	s.con.mu.Unlock()
	if had {
		s.InvalidateDetailVariants()
	}
	if s.db == nil {
		if had {
			s.emitPinRemoved(key, prev, true, "owner")
		}
		return had, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM resolver_pins WHERE rating_key=?`, int64(key))
	if err != nil {
		return had, err
	}
	s.InvalidateDetailVariants()
	s.notifyPin(key)
	n, _ := res.RowsAffected()
	removed := had || n > 0
	if removed {
		s.emitPinRemoved(key, prev, had, "owner")
	}
	return removed, nil
}

// Pins lists every pin, newest first.
func (s *Service) Pins() []Pin {
	s.con.mu.Lock()
	out := make([]Pin, 0, len(s.con.pins))
	for _, p := range s.con.pins {
		out = append(out, p)
	}
	s.con.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PinnedAt.Equal(out[j].PinnedAt) {
			return out[i].PinnedAt.After(out[j].PinnedAt)
		}
		return out[i].TitleKey < out[j].TitleKey
	})
	return out
}

// PinFor returns a title's pin.
func (s *Service) PinFor(key core.RatingKey) (Pin, bool) {
	s.con.mu.Lock()
	defer s.con.mu.Unlock()
	p, ok := s.con.pins[key]
	return p, ok
}

// pinColumns are the resolver_pins columns scanPin reads.
const pinColumns = `rating_key, release_json, title_name, note, pinned_at, last_status, last_reason, last_checked_at, fallback`

// scanPin reads one resolver_pins row (pinColumns).
func scanPin(row interface{ Scan(...any) error }) (Pin, error) {
	var key, pinnedAt, checkedAt, fallback int64
	var raw, status string
	var p Pin
	if err := row.Scan(&key, &raw, &p.TitleName, &p.Note, &pinnedAt, &status, &p.LastReason, &checkedAt, &fallback); err != nil {
		return p, err
	}
	p.TitleKey = core.RatingKey(key)
	p.Fallback = fallback != 0
	if err := json.Unmarshal([]byte(raw), &p.Release); err != nil || p.Release.ID == "" {
		return p, fmt.Errorf("resolver pin %d unreadable: %v", key, err)
	}
	p.PinnedAt = time.Unix(pinnedAt, 0)
	p.LastStatus = PinStatus(status)
	if checkedAt > 0 {
		p.LastCheckedAt = time.Unix(checkedAt, 0)
	}
	return p, nil
}

// loadPins replaces the in-memory pins with the stored ones.
func (s *Service) loadPins(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pinColumns+` FROM resolver_pins`)
	if err != nil {
		s.log.Warn("resolver pins unavailable", "err", err)
		return
	}
	defer rows.Close()
	pins := map[core.RatingKey]Pin{}
	for rows.Next() {
		p, err := scanPin(rows)
		if err != nil {
			s.log.Warn("resolver pin unreadable", "err", err)
			continue
		}
		pins[p.TitleKey] = p
	}
	if rows.Err() != nil {
		return
	}
	s.con.mu.Lock()
	s.con.pins = pins
	s.con.mu.Unlock()
}

// reloadPin re-reads one title's pin after another node changed it.
func (s *Service) reloadPin(ctx context.Context, key core.RatingKey) {
	if s.db == nil || key == 0 {
		return
	}
	p, err := scanPin(s.db.QueryRowContext(ctx, `SELECT `+pinColumns+` FROM resolver_pins WHERE rating_key=?`, int64(key)))
	s.con.mu.Lock()
	switch {
	case err == nil:
		s.con.pins[key] = p
	case errors.Is(err, sql.ErrNoRows):
		delete(s.con.pins, key) // unpinned on another node
	}
	s.con.mu.Unlock()
	s.InvalidateDetailVariants()
}

// notePin records the outcome of trying a title's pin, when releaseID is
// still the pinned release.
func (s *Service) notePin(ctx context.Context, key core.RatingKey, releaseID string, status PinStatus, reason string) {
	s.notePinForClient(ctx, key, releaseID, status, reason, "")
}

func (s *Service) notePinForClient(ctx context.Context, key core.RatingKey, releaseID string, status PinStatus, reason, client string) {
	now := s.now()
	s.con.mu.Lock()
	p, ok := s.con.pins[key]
	matched := ok && p.Release.ID == releaseID
	if matched {
		p.LastStatus, p.LastReason, p.LastCheckedAt = status, reason, now
		s.con.pins[key] = p
	}
	s.con.mu.Unlock()
	if matched {
		s.emitPinStatusEvent(p, status, reason, client)
	}
	if !ok || s.db == nil {
		return
	}
	s.persistBookkeeping(ctx, func(ctx context.Context) {
		_, err := s.db.ExecContext(ctx,
			`UPDATE resolver_pins SET last_status=?, last_reason=?, last_checked_at=? WHERE rating_key=? AND release_id=?`,
			string(status), reason, now.Unix(), int64(key), releaseID)
		if err != nil {
			s.log.Debug("resolver pin status not saved", "title", key, "err", err)
			return
		}
		s.notifyPin(key)
	})
}

// notePinsBlocked marks pins of a newly blocklisted release as failing, so
// the Pinned list shows why the pin is being skipped.
func (s *Service) notePinsBlocked(ctx context.Context, releaseID, reason string) {
	type hit struct {
		key core.RatingKey
		id  string
	}
	s.con.mu.Lock()
	var hits []hit
	for k, p := range s.con.pins {
		if p.Release.ID == releaseID {
			hits = append(hits, hit{key: k, id: p.Release.ID})
		}
	}
	s.con.mu.Unlock()
	for _, h := range hits {
		s.notePin(ctx, h.key, h.id, PinUnhealthy, "blocklisted: "+reason)
	}
}

// skipPreferenceKey carries, on a resolve's context, the preferred release
// that resolve already tried and could not use; ranking leaves it out.
type skipPreferenceKey struct{}

// pinForResolve returns the title's pin when it applies to req, else nil
// and, when there is a pin, why it does not apply.
func (s *Service) pinForResolve(req core.ResolveRequest) (*Pin, string) {
	if req.Title == nil {
		return nil, ""
	}
	p, ok := s.PinFor(req.Title.Key)
	if !ok {
		return nil, ""
	}
	switch {
	case req.Prefer != "" && req.Prefer != p.Release.ID && !autoVersion(req.Prefer):
		return nil, "skipped: viewer picked another version"
	case p.Fallback && req.Prefer != "" && !autoVersion(req.Prefer):
		// A strict pin keeps a same-release pick on its own path so nothing
		// else can play; a preference has no such constraint to keep.
		return nil, "skipped: viewer picked a version"
	case req.Client.Download:
		return nil, "skipped: downloads use their own profile"
	}
	return &p, ""
}

// PinnedReleaseID returns the authoritative playback source, if this request
// uses a strict owner pin. Playback uses it to recheck warm/resume and failover
// paths that can otherwise reuse a release without calling Resolve. An active
// viewer remembered choice overrides the owner pin in the Auto slot. A
// preferred pin (Fallback) is not authoritative; see PreferredRelease.
func (s *Service) PinnedReleaseID(req core.ResolveRequest) string {
	if s.viewerTitleChoiceForResolve(req, nil) != nil || s.viewerShowChoiceForResolve(req) != nil {
		return ""
	}
	if pin, _ := s.pinForResolve(req); pin != nil && !pin.Fallback {
		return pin.Release.ID
	}
	return ""
}

// PreferredRelease returns the owner's preferred release (a pin with
// Fallback) when it applies to this request, and whether it may be tried now:
// not blocklisted, not inside the brief failed-health window and not rejected
// for the title. Playback resolves an eligible preference before reusing warm
// or remembered sources and before racing alternates; once it is excluded or
// ineligible, ordinary choices are admitted. A viewer title choice keeps its
// precedence. A viewer show choice does not hide the preference: Resolve uses
// the preference whenever that choice matches no candidate, and a session the
// show choice put elsewhere is not on its preference, so it never returns to it.
func (s *Service) PreferredRelease(req core.ResolveRequest) (string, bool) {
	if req.Title == nil || s.viewerTitleChoiceForResolve(req, nil) != nil {
		return "", false
	}
	pin, _ := s.pinForResolve(req)
	if pin == nil || !pin.Fallback {
		return "", false
	}
	return pin.Release.ID, s.preferenceEligible(req.Title.Key, pin.Release)
}

// preferenceEligible reports whether a preferred release may be tried now.
// These are the existing failure memories; a pin's last status is not one.
func (s *Service) preferenceEligible(key core.RatingKey, rel core.Release) bool {
	return !s.isBadRelease(rel) && !s.recentlyFailedRelease(rel) && !s.rejectedFor(key, rel)
}

// pinFlightKey separates in-flight resolves by the applicable pin: its release
// and its mode. Two requests share a resolve only when they would try the same
// pinned or preferred release in the same way. A viewer show choice that
// matches no candidate leaves the pin in charge, so it keeps the pin here, as
// does a viewer title choice the request excludes.
func (s *Service) pinFlightKey(req core.ResolveRequest) string {
	exclude := make(map[string]bool, len(req.Exclude))
	for _, id := range req.Exclude {
		exclude[id] = true
	}
	if s.viewerTitleChoiceForResolve(req, exclude) != nil {
		return ""
	}
	pin, _ := s.pinForResolve(req)
	switch {
	case pin == nil:
		return ""
	case pin.Fallback:
		return "prefer:" + pin.Release.ID
	}
	return pin.Release.ID
}

func autoVersion(prefer string) bool {
	slot, ok := parseVersionSlot(prefer)
	return ok && slot.Kind == "auto"
}

// pinRejectReason is the gate set for an owner pin (see the package
// comment above): "" to accept.
func (s *Service) pinRejectReason(rel core.Release, sreq core.SearchRequest, _ core.ClientCaps, exclude map[string]bool) string {
	switch {
	case rel.Password && rel.Protocol == core.ProtocolTorrent:
		return "passworded"
	case exclude[rel.ID]:
		return "excluded (already tried)"
	case s.isBadRelease(rel):
		return "marked bad"
	case nonVideoRelease(rel, nil, sreq):
		return ReasonNotAVideoFile
	case sreq.Type == core.TypeEpisode && rel.Info.FullSeason && rel.Protocol != core.ProtocolTorrent && rel.Protocol != core.ProtocolUsenet:
		return "season pack (skipped in v1)"
	case sreq.Type == core.TypeEpisode && rel.Info.FullSeason && !packCoversSeasonCour(rel.Info, sreq):
		return "season/episode mismatch"
	}
	return ""
}

// withRelease returns results with rel appended when it is not already
// among them.
func withRelease(results []core.Release, rel core.Release) []core.Release {
	for _, r := range results {
		if r.ID == rel.ID {
			return results
		}
	}
	if rel.Info.Title == "" && rel.Info.Height == 0 && rel.Info.Source == core.SourceUnknown {
		rel.Info = releaseparse.Parse(rel.Name)
	}
	return append(results, rel)
}

// pinFirst moves the pinned candidate to the front: Resolve tries it
// before the ranking.
func pinFirst(cands []candidate, pinID string) []candidate {
	if pinID == "" {
		return cands
	}
	for i, c := range cands {
		if c.rel.ID == pinID {
			out := make([]candidate, 0, len(cands))
			out = append(out, c)
			out = append(out, cands[:i]...)
			return append(out, cands[i+1:]...)
		}
	}
	return cands
}

// candidateReason is a candidate's trace reason, for pin status notes. The
// latest entry wins: the pin attempt traces after any earlier look.
func candidateReason(tr *Trace, id string) string {
	for i := len(tr.Candidates) - 1; i >= 0; i-- {
		if c := tr.Candidates[i]; c.ReleaseID == id {
			if c.Reason != "" {
				return c.Reason
			}
			if c.HealthChecked && !c.Healthy {
				return "health: unhealthy"
			}
		}
	}
	return "not playable for this client"
}

// RekeyTitle moves the owner's release choices inside a title-match
// transaction. An existing destination pin remains the owner's choice.
func (s *Service) RekeyTitle(ctx context.Context, tx *sql.Tx, ranges []core.TitleKeyRange, name string) ([]core.RatingKey, error) {
	var changed []core.RatingKey
	for _, r := range ranges {
		keys, err := rekeyPins(ctx, tx, r, name)
		if err != nil {
			return nil, err
		}
		changed = append(changed, keys...)
	}
	return changed, nil
}

func rekeyPins(ctx context.Context, tx *sql.Tx, r core.TitleKeyRange, name string) ([]core.RatingKey, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM resolver_pins WHERE rating_key BETWEEN ? AND ? RETURNING `+pinColumns, r.First, r.Last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pins []Pin
	for rows.Next() {
		p, err := scanPin(rows)
		if err != nil {
			return nil, err
		}
		pins = append(pins, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(pins)*2)
	for _, p := range pins {
		dest := p.TitleKey + core.RatingKey(r.Delta)
		data, err := json.Marshal(p.Release)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO resolver_pins(rating_key,release_id,release_json,title_name,note,pinned_at,fallback)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(rating_key) DO NOTHING`, int64(dest), p.Release.ID, string(data), name, p.Note, p.PinnedAt.Unix(), store.BoolInt(p.Fallback)); err != nil {
			return nil, err
		}
		keys = append(keys, p.TitleKey, dest)
	}
	return keys, nil
}

// TitlePinsRebound publishes the committed bindings to the in-memory pin index.
func (s *Service) TitlePinsRebound(ctx context.Context, keys []core.RatingKey) {
	for _, key := range keys {
		s.reloadPin(ctx, key)
		s.notifyPin(key)
	}
}
