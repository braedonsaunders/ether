package liveevents

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
)

const (
	// KindEntityFavorite mirrors A4's entity favourite kind stored in favorites_items.
	KindEntityFavorite = "entity"

	maxTrackedAlertStates = 4096
	alertStateRetention   = 24 * time.Hour
)

// EntityFavorite represents one viewer's favourited live-event entity
// (key format: pluginID:feed:entityKind:id).
type EntityFavorite struct {
	ViewerID int64
	Key      string
}

// ParsedEntityKey is the decomposed form of an A4 entity favourite key.
type ParsedEntityKey struct {
	Raw        string
	PluginID   string
	Feed       string
	EntityKind string
	LocalID    string
}

// ParseEntityKey parses a favourite key of the form "pluginID:feed:entityKind:id".
func ParseEntityKey(key string) (ParsedEntityKey, bool) {
	parts := strings.SplitN(strings.TrimSpace(key), ":", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return ParsedEntityKey{}, false
	}
	return ParsedEntityKey{
		Raw:        strings.TrimSpace(key),
		PluginID:   parts[0],
		Feed:       parts[1],
		EntityKind: parts[2],
		LocalID:    parts[3],
	}, true
}

// ParseEventID splits an Ether-namespaced event ID ("pluginID:feedID:localID").
func ParseEventID(eventID, fallbackFeed string) (pluginID, feedID, localID string) {
	parts := strings.SplitN(strings.TrimSpace(eventID), ":", 3)
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return "", fallbackFeed, strings.TrimSpace(eventID)
}

// EventMatchesEntityKey reports whether ev includes the entity identified by key.
func EventMatchesEntityKey(ev core.LiveEvent, key ParsedEntityKey) bool {
	evPlugin, evFeed, _ := ParseEventID(ev.ID, ev.Feed)
	if evPlugin != "" && evPlugin != key.PluginID {
		return false
	}
	if evFeed != "" && evFeed != key.Feed {
		return false
	}
	kindLocal := key.EntityKind + ":" + key.LocalID
	for _, c := range ev.Competitors {
		if entityValueMatches(c.EntityID, c.ID, key.Raw, kindLocal, key.LocalID) {
			return true
		}
	}
	for _, r := range ev.Ranking {
		if entityValueMatches(r.EntityID, "", key.Raw, kindLocal, key.LocalID) {
			return true
		}
	}
	return false
}

func entityValueMatches(entityID, fallbackID, rawKey, kindLocal, localID string) bool {
	eid := strings.TrimSpace(entityID)
	if eid != "" {
		return eid == rawKey || eid == kindLocal || eid == localID
	}
	fid := strings.TrimSpace(fallbackID)
	return fid != "" && fid == localID
}

// FavoriteEntitiesReader supplies entity favourites across viewers for A7.
type FavoriteEntitiesReader interface {
	ListEntityFavorites(ctx context.Context) ([]EntityFavorite, error)
}

// SQLFavoriteEntitiesReader reads A4 entity favourites directly from Ether's
// favorites_items table without coupling to W256's favorites store edits.
type SQLFavoriteEntitiesReader struct {
	db *sql.DB
}

// NewSQLFavoriteEntitiesReader wraps db to list viewer entity favourites.
func NewSQLFavoriteEntitiesReader(db *sql.DB) *SQLFavoriteEntitiesReader {
	if db == nil {
		return nil
	}
	return &SQLFavoriteEntitiesReader{db: db}
}

func (r *SQLFavoriteEntitiesReader) ListEntityFavorites(ctx context.Context) ([]EntityFavorite, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT viewer_id, subject_id FROM favorites_items WHERE kind=? ORDER BY viewer_id, sort_order, added_at`,
		KindEntityFavorite,
	)
	if err != nil {
		// If favorites_items table has not been created in a minimal test DB, treat as empty.
		if strings.Contains(strings.ToLower(err.Error()), "no such table") || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []EntityFavorite
	for rows.Next() {
		var fav EntityFavorite
		if err := rows.Scan(&fav.ViewerID, &fav.Key); err != nil {
			return nil, err
		}
		out = append(out, fav)
	}
	return out, rows.Err()
}

// AlertViewerPolicy reports spoiler-free preference and per-event reveal state.
type AlertViewerPolicy interface {
	SpoilerFree(ctx context.Context, viewerID int64) (bool, error)
	EventRevealed(ctx context.Context, viewerID int64, eventID string) (bool, error)
}

// FlowsEventPublisher is satisfied by *events.Bus.
type FlowsEventPublisher interface {
	PublishTyped(eventType string, payload any, opts ...events.EnvelopeOption) error
}

// ViewerPushDeliverer is satisfied by *notify.ViewerPush.
type ViewerPushDeliverer interface {
	DeliverLiveEventAlert(ctx context.Context, viewerID int64, alert core.LiveEventAlert) error
}

// AlertInAppDeliverer is satisfied by *AlertHub.
type AlertInAppDeliverer interface {
	PublishAlert(viewerID int64, alert core.LiveEventAlert) int
}

// AlertEngineOptions configures an AlertEngine.
type AlertEngineOptions struct {
	Favorites FavoriteEntitiesReader
	Policy    AlertViewerPolicy
	Bus       FlowsEventPublisher
	Push      ViewerPushDeliverer
	InApp     AlertInAppDeliverer
	Now       func() time.Time
}

type eventAlertSnapshot struct {
	state      string
	close      bool
	lastSeenAt time.Time
	settledAt  time.Time
}

type sentAlertKey struct {
	viewerID int64
	eventID  string
	kind     string
}

// AlertEngine evaluates live event updates against viewers' favourited
// entities, detects started/close/final transitions, enforces spoiler-free
// suppression, and fans out alerts to Flows, in-app AlertHub, and ViewerPush.
type AlertEngine struct {
	favorites FavoriteEntitiesReader
	policy    AlertViewerPolicy
	bus       FlowsEventPublisher
	push      ViewerPushDeliverer
	inApp     AlertInAppDeliverer
	now       func() time.Time

	mu     sync.Mutex
	events map[string]*eventAlertSnapshot
	sent   map[sentAlertKey]time.Time
}

// NewAlertEngine constructs an A7 alert detector and dispatcher.
func NewAlertEngine(opts AlertEngineOptions) *AlertEngine {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &AlertEngine{
		favorites: opts.Favorites,
		policy:    opts.Policy,
		bus:       opts.Bus,
		push:      opts.Push,
		inApp:     opts.InApp,
		now:       nowFn,
		events:    make(map[string]*eventAlertSnapshot),
		sent:      make(map[sentAlertKey]time.Time),
	}
}

// SeedScheduleEvent records an event discovered during an hourly schedule
// fetch. Events already in a terminal state ("post", "canceled", "postponed")
// are recorded as settled so historical games never trigger stale alerts.
func (e *AlertEngine) SeedScheduleEvent(ev core.LiveEvent) {
	if e == nil || ev.ID == "" {
		return
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.events[ev.ID]; exists {
		return
	}
	snap := &eventAlertSnapshot{
		state:      ev.Status.State,
		close:      ev.Status.Close,
		lastSeenAt: now,
	}
	if isTerminalEventState(ev.Status.State) {
		snap.settledAt = now
	}
	e.events[ev.ID] = snap
	e.pruneLocked(now)
}

// ProcessEvents evaluates a batch of updated events against the current
// viewer entity favourites and dispatches any newly triggered alerts.
func (e *AlertEngine) ProcessEvents(ctx context.Context, updated []core.LiveEvent) error {
	if e == nil || len(updated) == 0 || e.favorites == nil {
		return nil
	}
	favs, err := e.favorites.ListEntityFavorites(ctx)
	if err != nil {
		return err
	}
	if len(favs) == 0 {
		return nil
	}
	return e.ProcessEventsWithFavorites(ctx, updated, favs)
}

// ProcessEventsWithFavorites evaluates updated events using an already-loaded
// slice of viewer entity favourites.
func (e *AlertEngine) ProcessEventsWithFavorites(ctx context.Context, updated []core.LiveEvent, favs []EntityFavorite) error {
	if e == nil || len(updated) == 0 || len(favs) == 0 {
		return nil
	}

	type viewerFav struct {
		viewerID int64
		key      ParsedEntityKey
	}
	parsed := make([]viewerFav, 0, len(favs))
	for _, f := range favs {
		if f.ViewerID <= 0 {
			continue
		}
		if pk, ok := ParseEntityKey(f.Key); ok {
			parsed = append(parsed, viewerFav{viewerID: f.ViewerID, key: pk})
		}
	}
	if len(parsed) == 0 {
		return nil
	}

	now := e.now()
	for _, ev := range updated {
		if ev.ID == "" {
			continue
		}
		kinds := e.detectTransitions(ev, now)
		if len(kinds) == 0 {
			continue
		}

		// Match every viewer who has at least one favourite entity in ev.
		// If a viewer favourites both competitors, emit once per (viewer, event, kind)
		// using their first matching favourite entity key.
		matchedByViewer := make(map[int64]ParsedEntityKey)
		for _, vf := range parsed {
			if _, already := matchedByViewer[vf.viewerID]; already {
				continue
			}
			if EventMatchesEntityKey(ev, vf.key) {
				matchedByViewer[vf.viewerID] = vf.key
			}
		}
		if len(matchedByViewer) == 0 {
			continue
		}

		for viewerID, entityKey := range matchedByViewer {
			spoilerFree := false
			if e.policy != nil {
				if sf, err := e.policy.SpoilerFree(ctx, viewerID); err == nil && sf {
					revealed := false
					if rev, err := e.policy.EventRevealed(ctx, viewerID, ev.ID); err == nil && rev {
						revealed = true
					}
					spoilerFree = !revealed
				}
			}

			for _, kind := range kinds {
				// Spoiler-free viewers receive start alerts ONLY.
				if spoilerFree && kind != core.LiveEventAlertStarted {
					continue
				}
				if !e.claimSent(viewerID, ev.ID, kind, now) {
					continue
				}
				alert := buildLiveEventAlert(ev, kind, entityKey.Raw, now)
				e.dispatchAlert(ctx, viewerID, ev, alert)
			}
		}
	}
	return nil
}

func (e *AlertEngine) detectTransitions(ev core.LiveEvent, now time.Time) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	prev, exists := e.events[ev.ID]
	state := ev.Status.State
	isClose := ev.Status.Close

	var kinds []string
	if !exists {
		snap := &eventAlertSnapshot{
			state:      state,
			close:      isClose,
			lastSeenAt: now,
		}
		// A cold snapshot establishes state; only witnessed transitions alert.
		if isTerminalEventState(state) {
			snap.settledAt = now
		}
		e.events[ev.ID] = snap
		e.pruneLocked(now)
		return nil
	}

	prevState := prev.state
	prevClose := prev.close
	prev.state = state
	prev.close = isClose
	prev.lastSeenAt = now

	if isTerminalEventState(prevState) {
		// Once settled, no further alerts fire for this event.
		return nil
	}

	if state == core.LiveEventStateIn && prevState != core.LiveEventStateIn {
		kinds = append(kinds, core.LiveEventAlertStarted)
	}
	if state == core.LiveEventStateIn && isClose && !prevClose {
		kinds = append(kinds, core.LiveEventAlertClose)
	}
	if state == core.LiveEventStatePost && prevState != core.LiveEventStatePost {
		kinds = append(kinds, core.LiveEventAlertFinal)
		prev.settledAt = now
	} else if isTerminalEventState(state) {
		prev.settledAt = now
	}

	e.pruneLocked(now)
	return kinds
}

func (e *AlertEngine) claimSent(viewerID int64, eventID, kind string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := sentAlertKey{viewerID: viewerID, eventID: eventID, kind: kind}
	if _, already := e.sent[k]; already {
		return false
	}
	e.sent[k] = now
	return true
}

func (e *AlertEngine) pruneLocked(now time.Time) {
	if len(e.events) <= maxTrackedAlertStates && len(e.sent) <= maxTrackedAlertStates*4 {
		return
	}
	cutoff := now.Add(-alertStateRetention)
	for id, snap := range e.events {
		if (!snap.settledAt.IsZero() && snap.settledAt.Before(cutoff)) || snap.lastSeenAt.Before(cutoff) {
			delete(e.events, id)
		}
	}
	for k, ts := range e.sent {
		if ts.Before(cutoff) {
			delete(e.sent, k)
		}
	}
}

func (e *AlertEngine) dispatchAlert(ctx context.Context, viewerID int64, ev core.LiveEvent, alert core.LiveEventAlert) {
	// 1. Flows event (always emitted for matching alerts).
	if e.bus != nil {
		eventType := flowsEventTypeForAlertKind(alert.Kind)
		if eventType != "" {
			pluginID, _, _ := ParseEventID(ev.ID, ev.Feed)
			payload := events.LiveEventAlertPayload{
				AlertID:   alert.ID,
				Kind:      alert.Kind,
				EventID:   ev.ID,
				Feed:      ev.Feed,
				Category:  ev.Category,
				League:    ev.League.Name,
				EntityKey: alert.EntityKey,
				ViewerID:  viewerID,
				Title:     alert.Title,
				Body:      alert.Body,
				State:     ev.Status.State,
				Close:     ev.Status.Close,
			}
			alertAt := parseEventTime(alert.At)
			if alertAt.IsZero() {
				alertAt = e.now()
			}
			_ = e.bus.PublishTyped(eventType, payload,
				events.WithOccurredAt(alertAt),
				events.WithCorrelation(events.Correlation{
					Key:      "liveevent:" + ev.ID,
					ViewerID: viewerID,
					PluginID: pluginID,
				}),
			)
		}
	}

	// 2. In-app toast stream (/vela/v0/live/stream?alerts=1 via AlertHub).
	deliveredInApp := 0
	if e.inApp != nil {
		deliveredInApp = e.inApp.PublishAlert(viewerID, alert)
	}

	// 3. Viewer push when the app is not open for this viewer.
	if deliveredInApp == 0 && e.push != nil {
		_ = e.push.DeliverLiveEventAlert(ctx, viewerID, alert)
	}
}

func flowsEventTypeForAlertKind(kind string) string {
	switch kind {
	case core.LiveEventAlertStarted:
		return events.EventLiveEventStarted
	case core.LiveEventAlertClose:
		return events.EventLiveEventClose
	case core.LiveEventAlertFinal:
		return events.EventLiveEventFinal
	default:
		return ""
	}
}

func isTerminalEventState(state string) bool {
	switch state {
	case core.LiveEventStatePost, core.LiveEventStateCanceled, core.LiveEventStatePostponed:
		return true
	default:
		return false
	}
}

func buildLiveEventAlert(ev core.LiveEvent, kind, entityKey string, at time.Time) core.LiveEventAlert {
	title := formatEventHeadline(ev)
	var body string
	switch kind {
	case core.LiveEventAlertStarted:
		// Never include score info in a start alert (spoiler-safe by design).
		body = "Live now"
	case core.LiveEventAlertClose:
		score := formatEventScoreline(ev)
		detail := strings.TrimSpace(ev.Status.Detail)
		switch {
		case score != "" && detail != "":
			body = fmt.Sprintf("Close · %s · %s", score, detail)
		case score != "":
			body = "Close · " + score
		case detail != "":
			body = "Close · " + detail
		default:
			body = "Close finish"
		}
	case core.LiveEventAlertFinal:
		score := formatEventScoreline(ev)
		if score != "" {
			body = "Final · " + score
		} else {
			body = "Final"
		}
	}

	return core.LiveEventAlert{
		ID:        fmt.Sprintf("%s:%s:%s", kind, ev.ID, entityKey),
		Kind:      kind,
		EventID:   ev.ID,
		EntityKey: entityKey,
		Title:     title,
		Body:      body,
		At:        at.UTC().Format(time.RFC3339),
	}
}

func formatEventHeadline(ev core.LiveEvent) string {
	base := strings.TrimSpace(ev.Title)
	if base == "" && len(ev.Competitors) == 2 {
		c0, c1 := ev.Competitors[0], ev.Competitors[1]
		away, home := c0, c1
		if c0.Home && !c1.Home {
			away, home = c1, c0
		}
		aName := firstNonEmpty(away.Short, away.Name, away.Abbr)
		hName := firstNonEmpty(home.Short, home.Name, home.Abbr)
		if aName != "" && hName != "" {
			if home.Home && !away.Home {
				base = aName + " at " + hName
			} else {
				base = aName + " vs " + hName
			}
		}
	}
	if base == "" {
		base = ev.ID
	}
	if abbr := strings.TrimSpace(ev.League.Abbr); abbr != "" && !strings.HasPrefix(strings.ToLower(base), strings.ToLower(abbr)) {
		return abbr + " · " + base
	}
	return base
}

func formatEventScoreline(ev core.LiveEvent) string {
	if len(ev.Competitors) != 2 {
		return ""
	}
	c0, c1 := ev.Competitors[0], ev.Competitors[1]
	away, home := c0, c1
	if c0.Home && !c1.Home {
		away, home = c1, c0
	}
	if away.Score == "" && home.Score == "" {
		return ""
	}
	aLabel := firstNonEmpty(away.Abbr, away.Short, away.Name)
	hLabel := firstNonEmpty(home.Abbr, home.Short, home.Name)
	return fmt.Sprintf("%s %s – %s %s", aLabel, away.Score, hLabel, home.Score)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
