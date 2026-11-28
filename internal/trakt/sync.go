package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// ---- id mapping -----------------------------------------------------------

type traktIDs struct {
	TMDB int64  `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
	TVDB int64  `json:"tvdb,omitempty"`
}

func idsOf(ids core.IDs) traktIDs {
	return traktIDs{TMDB: ids.TMDB, IMDB: ids.IMDB, TVDB: ids.TVDB}
}

func isPlayableTraktKey(key core.RatingKey) bool {
	if !key.Valid() || key.TMDB() <= 0 {
		return false
	}
	k := key.Kind()
	return k == core.KindMovie || k == core.KindEpisode
}

func isRatableTraktKey(key core.RatingKey) bool {
	if !key.Valid() || key.TMDB() <= 0 {
		return false
	}
	k := key.Kind()
	return k == core.KindMovie || k == core.KindShow || k == core.KindSeason || k == core.KindEpisode
}

func isSyncableTraktEvent(ev state.Event) bool {
	switch ev.Kind {
	case state.EventCompleted:
		return ev.Offline && isPlayableTraktKey(ev.Key)
	case state.EventProgress:
		// A new offline observation can advance recency while keeping the
		// same resume point or preserving a completed viewing.
		return (!ev.Offline || ev.ResumeSet && !ev.Completed) && isPlayableTraktKey(ev.Key)
	case state.EventScrobble, state.EventUnscrobble:
		return isPlayableTraktKey(ev.Key)
	case state.EventRated:
		return isRatableTraktKey(ev.Key)
	default:
		return false
	}
}

// payloadFor builds the Trakt movie/show/season/episode object for a catalogue title.
func payloadFor(t *core.Title) (kind string, payload map[string]any) {
	if t == nil {
		return "", nil
	}
	var k core.KeyKind
	if t.Key != 0 {
		k = t.Key.Kind()
	} else {
		switch t.Type {
		case core.TypeMovie:
			k = core.KindMovie
		case core.TypeShow:
			k = core.KindShow
		case core.TypeSeason:
			k = core.KindSeason
		case core.TypeEpisode:
			k = core.KindEpisode
		}
	}
	switch k {
	case core.KindEpisode:
		season, episode := t.Season, t.Episode
		if season == 0 && episode == 0 && t.Key.Kind() == core.KindEpisode {
			season, episode = t.Key.Season(), t.Key.Episode()
		}
		showIDs := t.ShowIDs
		if showIDs.TMDB == 0 && t.Key.Kind() == core.KindEpisode {
			showIDs.TMDB = t.Key.TMDB()
		}
		return "episode", map[string]any{
			"show":    map[string]any{"title": t.ShowTitle, "ids": idsOf(showIDs)},
			"episode": map[string]any{"season": season, "number": episode, "ids": idsOf(t.IDs)},
		}
	case core.KindSeason:
		season := t.Season
		if season == 0 && t.Key.Kind() == core.KindSeason {
			season = t.Key.Season()
		}
		// Season rows carry the TMDB season id in t.IDs; Trakt's nested
		// shows[].seasons[] payload expects the show's ids.
		showIDs := t.ShowIDs
		if showIDs.TMDB == 0 && t.Key.Kind() == core.KindSeason {
			showIDs.TMDB = t.Key.TMDB()
		}
		show := map[string]any{"title": t.ShowTitle, "ids": idsOf(showIDs)}
		if t.ShowYear > 0 {
			show["year"] = t.ShowYear
		}
		return "season", map[string]any{
			"show":   show,
			"season": map[string]any{"number": season},
		}
	case core.KindShow:
		ids := t.IDs
		if ids.TMDB == 0 && t.Key.Kind() == core.KindShow {
			ids.TMDB = t.Key.TMDB()
		}
		return "show", map[string]any{"show": map[string]any{"title": t.Title, "year": t.Year, "ids": idsOf(ids)}}
	case core.KindMovie:
		ids := t.IDs
		if ids.TMDB == 0 && t.Key.Kind() == core.KindMovie {
			ids.TMDB = t.Key.TMDB()
		}
		return "movie", map[string]any{"movie": map[string]any{"title": t.Title, "year": t.Year, "ids": idsOf(ids)}}
	default:
		return "", nil
	}
}

// titleFor resolves an event's key to a catalogue title.
func (c *Client) titleFor(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if !isRatableTraktKey(key) {
		return nil, core.ErrNotFound
	}
	if c.catalog != nil {
		if t, err := c.catalog.Get(ctx, key); err == nil && t != nil {
			return t, nil
		}
	}
	// Fall back to a key-derived shell: TMDB ids are encoded in the key.
	switch key.Kind() {
	case core.KindMovie:
		return &core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}}, nil
	case core.KindShow:
		return &core.Title{Key: key, Type: core.TypeShow, IDs: core.IDs{TMDB: key.TMDB()}}, nil
	case core.KindSeason:
		return &core.Title{Key: key, Type: core.TypeSeason, ShowKey: core.ShowKey(key.TMDB()), ShowIDs: core.IDs{TMDB: key.TMDB()}, Season: key.Season()}, nil
	case core.KindEpisode:
		return &core.Title{Key: key, Type: core.TypeEpisode, ShowKey: core.ShowKey(key.TMDB()), ShowIDs: core.IDs{TMDB: key.TMDB()}, Season: key.Season(), Episode: key.Episode()}, nil
	default:
		return nil, core.ErrNotFound
	}
}

// ---- scrobbling -----------------------------------------------------------

// Scrobble pushes one playback state to Trakt: action is "start", "pause"
// or "stop"; progress is 0-100. Trakt marks >=80% on stop as watched.
func (c *Client) Scrobble(ctx context.Context, userID int64, key core.RatingKey, action string, progress float64) error {
	if !c.Enabled() || !isPlayableTraktKey(key) || !c.Connected(ctx, userID) {
		return nil
	}
	return c.scrobble(ctx, userID, key, action, progress, nil)
}

const scrobblePath = "/scrobble/"

func (c *Client) scrobble(ctx context.Context, userID int64, key core.RatingKey, action string, progress float64, link *string) error {
	t, err := c.titleFor(ctx, key)
	if err != nil {
		return err
	}
	kind, payload := payloadFor(t)
	if kind != "movie" && kind != "episode" {
		return nil
	}
	payload["progress"] = progress
	status, b, err := c.authedForLink(ctx, userID, link, http.MethodPost, scrobblePath+action, payload)
	if err != nil && status == 0 {
		return err
	}
	// A status whose body could not be read is judged by the status.
	// Trakt returns 409 when the same item was just scrobbled.
	if status != http.StatusCreated && status != http.StatusOK && status != http.StatusConflict {
		detail := truncate(string(b), 200)
		if err != nil {
			detail = err.Error()
		}
		return fmt.Errorf("trakt scrobble %s: %w: %s", action, traktStatus(status), detail)
	}
	return nil
}

// traktStatus is a non-success HTTP status returned by Trakt.
type traktStatus int

func (s traktStatus) Error() string { return fmt.Sprintf("HTTP %d", int(s)) }

// retryableScrobble includes temporary errors from the stop or its OAuth refresh.
func retryableScrobble(err error) bool {
	var st traktStatus
	if errors.As(err, &st) {
		return st == http.StatusTooManyRequests || st >= 500 && st <= 599
	}
	var ue *url.Error
	var lookup tokenLookupError
	return !errors.Is(err, context.Canceled) && (errors.As(err, &ue) || errors.As(err, &lookup))
}

func syncBody(kind string, item map[string]any, extra map[string]any) map[string]any {
	switch kind {
	case "episode":
		// /sync/history and /sync/ratings accept episodes nested in shows/seasons.
		show, _ := item["show"].(map[string]any)
		ep, _ := item["episode"].(map[string]any)
		if show == nil || ep == nil {
			return nil
		}
		season, _ := ep["season"].(int)
		epItem := map[string]any{"number": ep["number"]}
		for k, v := range extra {
			epItem[k] = v
		}
		return map[string]any{"shows": []any{map[string]any{
			"title":   show["title"],
			"ids":     show["ids"],
			"seasons": []any{map[string]any{"number": season, "episodes": []any{epItem}}},
		}}}
	case "season":
		show, _ := item["show"].(map[string]any)
		sn, _ := item["season"].(map[string]any)
		if show == nil || sn == nil {
			return nil
		}
		seasonItem := map[string]any{"number": sn["number"]}
		for k, v := range extra {
			seasonItem[k] = v
		}
		showItem := map[string]any{
			"title":   show["title"],
			"ids":     show["ids"],
			"seasons": []any{seasonItem},
		}
		if year, ok := show["year"].(int); ok && year > 0 {
			showItem["year"] = year
		}
		return map[string]any{"shows": []any{showItem}}
	case "show":
		s, _ := item["show"].(map[string]any)
		if s == nil {
			return nil
		}
		showItem := map[string]any{}
		for k, v := range s {
			showItem[k] = v
		}
		for k, v := range extra {
			showItem[k] = v
		}
		return map[string]any{"shows": []any{showItem}}
	case "movie":
		m, _ := item["movie"].(map[string]any)
		if m == nil {
			return nil
		}
		movie := map[string]any{}
		for k, v := range m {
			movie[k] = v
		}
		for k, v := range extra {
			movie[k] = v
		}
		return map[string]any{"movies": []any{movie}}
	default:
		return nil
	}
}

// AddHistory marks a movie or episode watched on Trakt at the given time.
// Show and season keys are ignored: marking one episode watched emits parent
// touch events for its season and show, and whole-show/season marks already
// expand to per-episode events.
func (c *Client) AddHistory(ctx context.Context, userID int64, key core.RatingKey, watchedAt time.Time) error {
	if !isPlayableTraktKey(key) {
		return nil
	}
	return c.sync(ctx, userID, "/sync/history", key, map[string]any{"watched_at": watchedAt.UTC().Format(time.RFC3339)})
}

// SetWatchlist pushes a catalogue movie or show to a linked Trakt account.
// A disconnected account is intentionally local-only.
func (c *Client) SetWatchlist(ctx context.Context, userID int64, title core.Title, add bool) (bool, error) {
	if !c.Connected(ctx, userID) {
		return false, nil
	}
	var group string
	switch title.Type {
	case core.TypeMovie:
		group = "movies"
	case core.TypeShow:
		group = "shows"
	default:
		return true, fmt.Errorf("trakt watchlist: unsupported title type")
	}
	ids := title.IDs
	if ids.TMDB == 0 {
		ids.TMDB = title.Key.TMDB()
	}
	path := "/sync/watchlist"
	if !add {
		path += "/remove"
	}
	status, body, err := c.authed(ctx, userID, http.MethodPost, path, map[string]any{group: []any{map[string]any{"title": title.Title, "year": title.Year, "ids": idsOf(ids)}}})
	if err != nil {
		return true, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return true, fmt.Errorf("trakt watchlist: HTTP %d", status)
	}
	var result struct {
		NotFound map[string][]json.RawMessage `json:"not_found"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return true, fmt.Errorf("trakt watchlist: invalid response")
	}
	for _, entries := range result.NotFound {
		if len(entries) > 0 {
			return true, fmt.Errorf("trakt watchlist: title not found")
		}
	}
	c.invalidateUser(userID)
	return true, nil
}

// RemoveHistory removes a movie or episode from the user's Trakt history.
func (c *Client) RemoveHistory(ctx context.Context, userID int64, key core.RatingKey) error {
	if !isPlayableTraktKey(key) {
		return nil
	}
	return c.sync(ctx, userID, "/sync/history/remove", key, nil)
}

// RemoveRating clears a title's rating on Trakt.
func (c *Client) RemoveRating(ctx context.Context, userID int64, key core.RatingKey) error {
	if !isRatableTraktKey(key) {
		return nil
	}
	return c.sync(ctx, userID, "/sync/ratings/remove", key, nil)
}

// AddRating rates a title on Trakt (1-10).
func (c *Client) AddRating(ctx context.Context, userID int64, key core.RatingKey, rating float64) error {
	if !isRatableTraktKey(key) {
		return nil
	}
	r := int(rating + 0.5)
	if r < 1 {
		r = 1
	}
	if r > 10 {
		r = 10
	}
	return c.sync(ctx, userID, "/sync/ratings", key, map[string]any{"rating": r})
}

func (c *Client) sync(ctx context.Context, userID int64, path string, key core.RatingKey, extra map[string]any) error {
	if !c.Connected(ctx, userID) {
		return nil
	}
	t, err := c.titleFor(ctx, key)
	if err != nil {
		return nil
	}
	kind, item := payloadFor(t)
	body := syncBody(kind, item, extra)
	if body == nil {
		return nil
	}
	status, b, err := c.authed(ctx, userID, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("trakt %s: HTTP %d: %s", path, status, truncate(string(b), 200))
	}
	return nil
}

// ---- event feed -----------------------------------------------------------

type scrobbleKey struct {
	user int64
	key  core.RatingKey
}

type scrobbleState struct {
	action  string
	at      time.Time
	watched bool
	linkID  string
}

// pendingStop is a final scrobble/stop being sent or awaiting a retry, bound
// to the Trakt account of its first send, whose stamp stays meanwhile.
type pendingStop struct {
	progress float64
	linkID   string
	cancel   context.CancelFunc // non-nil while an attempt is in flight
	attempts int
	first    time.Time
	next     time.Time
}

const (
	stopAttempts    = 3                // sends of one final stop, the first included
	stopRetrySpace  = 30 * time.Second // times the attempts made so far
	stopRetryMaxAge = 10 * time.Minute
	stopDedupe      = 5 * time.Minute
	traktWatched    = 80 // /scrobble/stop below 80% only updates resume progress
)

// startRefresh is how often a continuing "playing" timeline re-sends
// scrobble/start; Trakt expires an un-refreshed scrobble after the item's
// runtime, so this only matters for very long sessions.
const startRefresh = 15 * time.Minute

// shouldScrobble de-duplicates timeline updates: Plex clients report
// progress every ~10s, but Trakt only needs a call when playback state
// changes (start/pause/stop), plus a periodic start refresh.
func (c *Client) shouldScrobble(k scrobbleKey, linkID, action string) bool {
	c.scMu.Lock()
	defer c.scMu.Unlock()
	return c.stampScrobbleLocked(k, linkID, action, false)
}

func (c *Client) stampScrobbleLocked(k scrobbleKey, linkID, action string, watched bool) bool {
	now := c.now()
	last, ok := c.lastScro[k]
	if ok && last.linkID == linkID && last.action == action && (!watched || last.watched) {
		window := startRefresh // repeated start/pause timelines
		if action == "stop" {
			window = stopDedupe // duplicate stop reports of one viewing
		}
		if now.Sub(last.at) < window {
			return false
		}
	}
	c.lastScro[k] = scrobbleState{action: action, at: now, watched: watched, linkID: linkID}
	if len(c.lastScro) > 1000 {
		for kk, st := range c.lastScro {
			if now.Sub(st.at) > 6*time.Hour {
				delete(c.lastScro, kk)
			}
		}
	}
	return true
}

// HandleEvent maps one state event to Trakt calls. Safe to call directly;
// SubscribeTo + Start give the queued, non-blocking version.
//
//   - Timeline progress becomes scrobble start/pause/stop (de-duplicated).
//     A progress event that crosses the watched threshold sends
//     scrobble/stop at 100%, which Trakt records as a play, so the
//     EventCompleted that follows it is not pushed again.
//   - Explicit scrobble/unscrobble (mark watched/unwatched) go to
//     /sync/history and /sync/history/remove.
//   - Ratings go to /sync/ratings (0 removes the rating).
func (c *Client) HandleEvent(ctx context.Context, ev state.Event) {
	if err := c.handleEvent(ctx, ev); err != nil {
		c.log.Debug("trakt sync failed", "kind", ev.Kind, "user", ev.UserID, "key", ev.Key, "err", err)
	}
}

func (c *Client) handleEvent(ctx context.Context, ev state.Event) error {
	if !c.Enabled() {
		return nil
	}
	progress := min(max(ev.Fraction*100, 0), 100)
	if ev.Imported && (ev.Kind == state.EventProgress || ev.At.IsZero()) {
		return nil
	}
	if !isSyncableTraktEvent(ev) {
		return nil
	}
	tokens, connected, err := c.Tokens(ctx, ev.UserID)
	if err != nil || !connected {
		return err
	}
	switch ev.Kind {
	case state.EventProgress:
		k := scrobbleKey{ev.UserID, ev.Key}
		action := ""
		switch {
		case ev.Completed:
			action, progress = "stop", 100
		case ev.State == "stopped":
			action = "stop"
		case ev.State == "paused":
			action = "pause"
		case ev.State == "playing" || ev.State == "buffering" || ev.State == "":
			if ev.Fraction > 0 {
				action = "start"
			}
		}
		if action == "stop" {
			err = c.stop(ctx, k, tokens.linkID, progress)
		} else if action != "" || ev.State == "playing" || ev.State == "buffering" {
			// Reports in the credits of the same viewing can follow a
			// threshold completion while its stop is still awaiting a retry.
			if ev.Fraction >= state.WatchedThreshold && c.recentWatchedStop(k, tokens.linkID) {
				return nil
			}
			c.cancelStops(ev.UserID, ev.Key)
		}
		if action != "" && action != "stop" && c.shouldScrobble(k, tokens.linkID, action) {
			err = c.scrobble(ctx, ev.UserID, ev.Key, action, progress, &tokens.linkID)
		}
		if ev.Completed {
			c.invalidateUser(ev.UserID)
		}
	case state.EventCompleted:
		if ev.Offline {
			err = c.AddHistory(ctx, ev.UserID, ev.Key, ev.At)
			c.invalidateUser(ev.UserID)
		}
		// Live completions are recorded by the progress event's scrobble/stop.
	case state.EventScrobble:
		if !ev.Imported {
			c.cancelStops(ev.UserID, ev.Key)
		}
		err = c.AddHistory(ctx, ev.UserID, ev.Key, ev.At)
		c.invalidateUser(ev.UserID)
	case state.EventUnscrobble:
		c.cancelStops(ev.UserID, ev.Key)
		err = c.RemoveHistory(ctx, ev.UserID, ev.Key)
		c.invalidateUser(ev.UserID)
	case state.EventRated:
		if ev.Rating > 0 {
			err = c.AddRating(ctx, ev.UserID, ev.Key, ev.Rating)
		} else {
			err = c.RemoveRating(ctx, ev.UserID, ev.Key)
		}
	}
	return err
}

func (c *Client) recentWatchedStop(k scrobbleKey, linkID string) bool {
	c.scMu.Lock()
	defer c.scMu.Unlock()
	if p := c.stops[k]; p != nil && p.linkID == linkID && p.progress >= traktWatched {
		return true
	}
	last := c.lastScro[k]
	return last.linkID == linkID && last.watched && c.now().Sub(last.at) < stopDedupe
}

// stop registers the attempt and its deduplication stamp atomically.
func (c *Client) stop(ctx context.Context, k scrobbleKey, linkID string, progress float64) error {
	c.scMu.Lock()
	if p := c.stops[k]; p != nil {
		if p.linkID == linkID {
			p.progress = max(p.progress, progress)
			c.scMu.Unlock()
			return nil
		}
		c.dropStopLocked(k)
	}
	if !c.stampScrobbleLocked(k, linkID, "stop", progress >= traktWatched) {
		c.scMu.Unlock()
		return nil
	}
	p := &pendingStop{progress: progress, linkID: linkID, first: c.now()}
	c.stops[k] = p
	c.scMu.Unlock()
	return c.sendStop(ctx, k, p)
}

func (c *Client) sendStop(ctx context.Context, k scrobbleKey, p *pendingStop) error {
	c.scMu.Lock()
	if c.stops[k] != p || p.cancel != nil || c.now().Before(p.next) {
		c.scMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.cancel = cancel
	progress := p.progress
	c.scMu.Unlock()

	err := c.scrobble(ctx, k.user, k.key, "stop", progress, &p.linkID)
	c.scMu.Lock()
	defer c.scMu.Unlock()
	if c.stops[k] != p {
		return err
	}
	p.cancel = nil
	p.attempts++
	switch {
	case err == nil && progress < traktWatched && p.progress >= traktWatched:
		// The in-flight send only saved resume progress. Send the completion
		// with its own budget. A watched stop is never replayed for an upgrade.
		p.attempts = 0
		p.next = c.now()
		c.wakeStopRetries()
	case err != nil && !retryableScrobble(err):
		c.dropStopLocked(k)
	case err != nil && p.attempts < stopAttempts:
		p.next = c.now().Add(time.Duration(p.attempts) * stopRetrySpace)
		c.wakeStopRetries()
	default:
		delete(c.stops, k)
		last := c.lastScro[k]
		last.at, last.watched = c.now(), progress >= traktWatched
		c.lastScro[k] = last
		if err != nil {
			c.log.Warn("trakt scrobble stop failed; giving up", "user", k.user, "key", k.key, "attempts", p.attempts, "err", err)
		}
	}
	return err
}

func (c *Client) wakeStopRetries() {
	select {
	case c.retryWake <- struct{}{}:
	default:
	}
}

// runStopRetries only wakes for actual pending work. Running separately from
// the event and import worker keeps slow retry requests out of the live queue.
func (c *Client) runStopRetries(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.retryWake:
		}
		for {
			c.scMu.Lock()
			var delay time.Duration
			pending := false
			now := c.now()
			for _, p := range c.stops {
				if p.cancel != nil || p.next.IsZero() {
					continue
				}
				wait := min(p.next.Sub(now), p.first.Add(stopRetryMaxAge).Sub(now))
				if !pending || wait < delay {
					delay, pending = wait, true
				}
			}
			c.scMu.Unlock()
			if !pending {
				break
			}
			// A live event may already be queued. Give the live worker a
			// chance to cancel the stop before trying the due work again.
			t := time.NewTimer(max(delay, time.Second))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-c.retryWake:
				t.Stop()
			case <-t.C:
				c.retryStops(ctx)
			}
		}
	}
}

// cancelStops drops the pending stop for key. A changed Trakt connection
// passes key 0 to clear all of that viewer's pending stops and stamps.
func (c *Client) cancelStops(userID int64, key core.RatingKey) {
	c.scMu.Lock()
	defer c.scMu.Unlock()
	if key != 0 {
		k := scrobbleKey{userID, key}
		if c.stops[k] != nil {
			c.dropStopLocked(k)
		}
		return
	}
	for k := range c.stops {
		if k.user == userID {
			c.dropStopLocked(k)
		}
	}
	for k := range c.lastScro {
		if k.user == userID {
			delete(c.lastScro, k)
		}
	}
}

// dropStopLocked drops k's pending stop and its stamp, so the next stop is sent.
func (c *Client) dropStopLocked(k scrobbleKey) {
	if p := c.stops[k]; p != nil && p.cancel != nil {
		p.cancel()
	}
	delete(c.stops, k)
	if c.lastScro[k].action == "stop" {
		delete(c.lastScro, k)
	}
}

// retryStops expires old stops and sends due ones, yielding to queued live
// events so they can supersede any stop for the same title.
func (c *Client) retryStops(ctx context.Context) {
	now := c.now()
	due := map[scrobbleKey]*pendingStop{}
	c.scMu.Lock()
	for k, p := range c.stops {
		if p.cancel != nil || p.next.IsZero() {
			continue
		}
		if now.Sub(p.first) >= stopRetryMaxAge {
			delete(c.stops, k)
			c.log.Warn("trakt scrobble stop expired; giving up", "user", k.user, "key", k.key, "attempts", p.attempts)
		} else if !now.Before(p.next) {
			due[k] = p
		}
	}
	c.scMu.Unlock()
	for k, p := range due {
		select {
		case <-c.done:
			return
		default:
		}
		if ctx.Err() != nil || !c.Enabled() {
			return
		}
		c.evMu.Lock()
		live := slices.ContainsFunc(c.queue, func(ev state.Event) bool {
			return ev.UserID == k.user && ev.Key == k.key &&
				(ev.Kind == state.EventProgress || ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble)
		})
		c.evMu.Unlock()
		if live {
			continue
		}
		remaining := p.first.Add(stopRetryMaxAge).Sub(c.now())
		if remaining <= 0 {
			continue
		}
		ectx, cancel := context.WithTimeout(ctx, min(30*time.Second, remaining))
		if c.sendStop(ectx, k, p) == nil {
			c.invalidateUser(k.user)
		}
		cancel()
	}
}

// SubscribeTo feeds state events into the scrobble queue (non-blocking).
func (c *Client) SubscribeTo(src interface {
	Subscribe(func(state.Event)) func()
}) func() {
	return src.Subscribe(func(ev state.Event) {
		if ev.Account || !isSyncableTraktEvent(ev) {
			return
		}
		if ev.Imported || (ev.Offline && ev.Kind == state.EventCompleted) {
			// Historical writes carry their original observation time and
			// use the durable history queue.
			if ev.Offline {
				ev.Kind = state.EventScrobble
			}
			// Bulk history must not compete with disposable timeline updates.
			// Persist it before returning to the importer, without network I/O.
			c.enqueueImport(ev)
			return
		}
		c.evMu.Lock()
		if len(c.queue) >= c.qsize {
			c.queue = c.queue[1:] // drop the oldest; Trakt is unreachable or slow
		}
		c.queue = append(c.queue, ev)
		c.evMu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	})
}

// Start runs the event worker until Close. Typical wiring:
//
//	go traktClient.Start(ctx)
//	unsub := traktClient.SubscribeTo(stateSvc)
func (c *Client) Start(ctx context.Context) {
	retryCtx, cancel := context.WithCancel(ctx)
	retryDone := make(chan struct{})
	go func() {
		defer close(retryDone)
		c.runStopRetries(retryCtx)
	}()
	defer func() {
		cancel()
		<-retryDone
	}()
	retry := time.NewTicker(30 * time.Second)
	defer retry.Stop()
	// Drain records persisted by an earlier process immediately on startup.
	select {
	case c.notify <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-c.notify:
		case <-retry.C:
		}
		for {
			c.evMu.Lock()
			if len(c.queue) == 0 {
				c.evMu.Unlock()
				break
			}
			ev := c.queue[0]
			c.queue = c.queue[1:]
			c.evMu.Unlock()
			ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
			c.HandleEvent(ectx, ev)
			cancel()
		}
		if err := c.drainImports(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("trakt imported history sync deferred", "err", err)
		}
	}
}

// Close stops the event worker.
func (c *Client) Close() {
	c.once.Do(func() { close(c.done) })
}

// ---- pulling data back ------------------------------------------------------

type traktTitle struct {
	Title string   `json:"title"`
	Year  int      `json:"year"`
	IDs   traktIDs `json:"ids"`
}

type historyMovie struct {
	WatchedAt time.Time  `json:"watched_at"`
	Movie     traktTitle `json:"movie"`
}

type historyEpisode struct {
	WatchedAt time.Time `json:"watched_at"`
	Show      struct {
		traktTitle
	} `json:"show"`
	Episode struct {
		Season int      `json:"season"`
		Number int      `json:"number"`
		IDs    traktIDs `json:"ids"`
	} `json:"episode"`
}

// PullHistory fetches the user's Trakt history and maps it to Ether
// history entries (via TMDB ids in the rating keys), for merging into the
// recommendation model. Entries Trakt has no TMDB id for are skipped.
//
// Results are cached (Options.CacheTTL) and served stale while refreshing.
func (c *Client) PullHistory(ctx context.Context, userID int64, limit int) ([]core.HistoryEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	if !c.Connected(ctx, userID) {
		return nil, nil
	}
	return cachedCall(c, ctx, cacheKey("history", userID, limit), func(ctx context.Context) ([]core.HistoryEntry, error) {
		return c.pullHistory(ctx, userID, limit)
	})
}

func (c *Client) pullHistory(ctx context.Context, userID int64, limit int) ([]core.HistoryEntry, error) {
	var out []core.HistoryEntry
	status, b, err := c.authed(ctx, userID, http.MethodGet, fmt.Sprintf("/sync/history/movies?page=1&limit=%d", limit), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("trakt history movies: HTTP %d", status)
	}
	var movies []historyMovie
	if err := json.Unmarshal(b, &movies); err != nil {
		return nil, fmt.Errorf("trakt history decode: %w", err)
	}
	for _, m := range movies {
		if m.Movie.IDs.TMDB != 0 {
			out = append(out, core.HistoryEntry{UserID: userID, Key: core.MovieKey(m.Movie.IDs.TMDB), ViewedAt: m.WatchedAt, Completed: true})
		}
	}
	status, b, err = c.authed(ctx, userID, http.MethodGet, fmt.Sprintf("/sync/history/episodes?page=1&limit=%d", limit), nil)
	if err != nil {
		return out, err
	}
	if status != http.StatusOK {
		return out, fmt.Errorf("trakt history episodes: HTTP %d", status)
	}
	var eps []historyEpisode
	if err := json.Unmarshal(b, &eps); err != nil {
		return out, fmt.Errorf("trakt history decode: %w", err)
	}
	for _, e := range eps {
		if e.Show.IDs.TMDB != 0 {
			out = append(out, core.HistoryEntry{UserID: userID, Key: core.EpisodeKey(e.Show.IDs.TMDB, e.Episode.Season, e.Episode.Number), ViewedAt: e.WatchedAt, Completed: true})
		}
	}
	return out, nil
}

// resolveTraktTitles maps Trakt movie/show objects to catalogue titles,
// synthesising a minimal Title when the catalogue doesn't have it yet.
func (c *Client) resolveTraktTitles(ctx context.Context, items []traktTitle, typ core.MediaType) []core.Title {
	var out []core.Title
	for _, it := range items {
		if it.IDs.TMDB == 0 {
			continue
		}
		key := core.MovieKey(it.IDs.TMDB)
		if typ == core.TypeShow {
			key = core.ShowKey(it.IDs.TMDB)
		}
		if c.catalog != nil {
			if t, err := c.catalog.Get(ctx, key); err == nil && t != nil {
				out = append(out, *t)
				continue
			}
		}
		out = append(out, core.Title{
			Key: key, Type: typ, Title: it.Title, Year: it.Year,
			IDs: core.IDs{TMDB: it.IDs.TMDB, IMDB: it.IDs.IMDB, TVDB: it.IDs.TVDB},
		})
	}
	return out
}

// Recommendations returns Trakt's personalised movie+show recommendations,
// interleaved, as catalogue titles. Cached like PullHistory. Users who have
// not connected Trakt get (nil, nil).
func (c *Client) Recommendations(ctx context.Context, userID int64, limit int) ([]core.Title, error) {
	if limit <= 0 {
		limit = 20
	}
	if !c.Connected(ctx, userID) {
		return nil, nil
	}
	return cachedCall(c, ctx, cacheKey("recs", userID, limit), func(ctx context.Context) ([]core.Title, error) {
		return c.recommendations(ctx, userID, limit)
	})
}

func (c *Client) recommendations(ctx context.Context, userID int64, limit int) ([]core.Title, error) {
	var lists [][]core.Title
	var firstErr error
	for _, p := range []struct {
		path string
		typ  core.MediaType
	}{{"/recommendations/movies?ignore_collected=true&ignore_watchlisted=true&limit=", core.TypeMovie},
		{"/recommendations/shows?ignore_collected=true&ignore_watchlisted=true&limit=", core.TypeShow}} {
		status, b, err := c.authed(ctx, userID, http.MethodGet, fmt.Sprintf("%s%d", p.path, limit), nil)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("trakt recommendations: HTTP %d", status)
		}
		var items []traktTitle
		if err == nil {
			if derr := json.Unmarshal(b, &items); derr != nil {
				err = fmt.Errorf("trakt recommendations decode: %w", derr)
			}
		}
		if err != nil {
			// One failing list (e.g. shows) should not lose the other.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		lists = append(lists, c.resolveTraktTitles(ctx, items, p.typ))
	}
	if len(lists) == 0 {
		return nil, firstErr
	}
	var out []core.Title
	for i := 0; len(out) < limit; i++ {
		added := false
		for _, l := range lists {
			if i < len(l) {
				out = append(out, l[i])
				added = true
			}
		}
		if !added {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type watchlistItem struct {
	Type  string      `json:"type"`
	Movie *traktTitle `json:"movie,omitempty"`
	Show  *traktTitle `json:"show,omitempty"`
}

// Watchlist returns the user's Trakt watchlist as catalogue titles.
// Returns ErrNotConnected when Trakt is disabled or the user has not linked it
// (rather than nil, nil) so sync callers never mistake a missing or unreadable
// connection for an empty remote watchlist.
//
// Cached like PullHistory.
func (c *Client) Watchlist(ctx context.Context, userID int64, limit int) ([]core.Title, error) {
	if limit <= 0 {
		limit = 20
	}
	if !c.Enabled() {
		return nil, ErrNotConnected
	}
	if _, err := c.tokensForLink(ctx, userID, nil); err != nil {
		return nil, err
	}
	return cachedCall(c, ctx, cacheKey("watchlist", userID, limit), func(ctx context.Context) ([]core.Title, error) {
		return c.watchlist(ctx, userID, limit)
	})
}

func (c *Client) watchlist(ctx context.Context, userID int64, limit int) ([]core.Title, error) {
	status, b, err := c.authed(ctx, userID, http.MethodGet, "/sync/watchlist?sort=rank", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("trakt watchlist: HTTP %d", status)
	}
	var items []watchlistItem
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("trakt watchlist decode: %w", err)
	}
	// Keep the user's rank order across movies and shows.
	var out []core.Title
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		switch strings.ToLower(it.Type) {
		case "movie":
			if it.Movie != nil {
				out = append(out, c.resolveTraktTitles(ctx, []traktTitle{*it.Movie}, core.TypeMovie)...)
			}
		case "show":
			if it.Show != nil {
				out = append(out, c.resolveTraktTitles(ctx, []traktTitle{*it.Show}, core.TypeShow)...)
			}
		}
	}
	return out, nil
}
