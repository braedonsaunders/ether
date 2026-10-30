package trakt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// payloadFor builds the Trakt movie/episode object for a catalogue title.
func payloadFor(t *core.Title) (kind string, payload map[string]any) {
	if t.Type == core.TypeEpisode || t.Key.Kind() == core.KindEpisode {
		season, episode := t.Season, t.Episode
		if season == 0 && episode == 0 {
			season, episode = t.Key.Season(), t.Key.Episode()
		}
		showIDs := t.ShowIDs
		if showIDs.TMDB == 0 {
			showIDs.TMDB = t.Key.TMDB()
		}
		return "episode", map[string]any{
			"show":    map[string]any{"title": t.ShowTitle, "ids": idsOf(showIDs)},
			"episode": map[string]any{"season": season, "number": episode, "ids": idsOf(t.IDs)},
		}
	}
	ids := t.IDs
	if ids.TMDB == 0 && t.Key.Kind() == core.KindMovie {
		ids.TMDB = t.Key.TMDB()
	}
	return "movie", map[string]any{"movie": map[string]any{"title": t.Title, "year": t.Year, "ids": idsOf(ids)}}
}

// titleFor resolves an event's key to a catalogue title.
func (c *Client) titleFor(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if c.catalog != nil {
		if t, err := c.catalog.Get(ctx, key); err == nil && t != nil {
			return t, nil
		}
	}
	// Fall back to a key-derived shell: TMDB ids are encoded in the key.
	switch key.Kind() {
	case core.KindMovie:
		return &core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}}, nil
	case core.KindEpisode:
		return &core.Title{Key: key, Type: core.TypeEpisode, ShowIDs: core.IDs{TMDB: key.TMDB()}, Season: key.Season(), Episode: key.Episode()}, nil
	default:
		return nil, core.ErrNotFound
	}
}

// ---- scrobbling -----------------------------------------------------------

// Scrobble pushes one playback state to Trakt: action is "start", "pause"
// or "stop"; progress is 0-100. Trakt marks >=80% on stop as watched.
func (c *Client) Scrobble(ctx context.Context, userID int64, key core.RatingKey, action string, progress float64) error {
	if !c.Connected(ctx, userID) {
		return nil
	}
	t, err := c.titleFor(ctx, key)
	if err != nil {
		return nil // seasons/shows don't scrobble
	}
	_, payload := payloadFor(t)
	payload["progress"] = progress
	status, b, err := c.authed(ctx, userID, http.MethodPost, "/scrobbling/"+action, payload)
	if err != nil {
		return err
	}
	// 409 = a scrobble is already in progress; not an error for us.
	if status != http.StatusCreated && status != http.StatusOK && status != http.StatusConflict {
		return fmt.Errorf("trakt scrobble %s: HTTP %d: %s", action, status, truncate(string(b), 200))
	}
	return nil
}

func syncBody(kind string, item map[string]any, extra map[string]any) map[string]any {
	inner := map[string]any{}
	for k, v := range item {
		inner[k] = v
	}
	for k, v := range extra {
		inner[k] = v
	}
	if kind == "episode" {
		// /sync/history wants episodes nested in shows/seasons; the flat
		// {show, episode} episode shape is also accepted for history adds
		// via "episodes" — build the documented nested form.
		show := item["show"]
		ep, _ := item["episode"].(map[string]any)
		season, _ := ep["season"].(int)
		epItem := map[string]any{"number": ep["number"]}
		for k, v := range extra {
			epItem[k] = v
		}
		return map[string]any{"shows": []any{map[string]any{
			"title":   show.(map[string]any)["title"],
			"ids":     show.(map[string]any)["ids"],
			"seasons": []any{map[string]any{"number": season, "episodes": []any{epItem}}},
		}}}
	}
	movie := map[string]any{}
	if m, ok := item["movie"].(map[string]any); ok {
		for k, v := range m {
			movie[k] = v
		}
	}
	for k, v := range extra {
		movie[k] = v
	}
	return map[string]any{"movies": []any{movie}}
}

// AddHistory marks a title watched on Trakt at the given time.
func (c *Client) AddHistory(ctx context.Context, userID int64, key core.RatingKey, watchedAt time.Time) error {
	return c.sync(ctx, userID, "/sync/history", key, map[string]any{"watched_at": watchedAt.UTC().Format(time.RFC3339)})
}

// RemoveHistory removes a title from the user's Trakt history.
func (c *Client) RemoveHistory(ctx context.Context, userID int64, key core.RatingKey) error {
	return c.sync(ctx, userID, "/sync/history/remove", key, nil)
}

// RemoveRating clears a title's rating on Trakt.
func (c *Client) RemoveRating(ctx context.Context, userID int64, key core.RatingKey) error {
	return c.sync(ctx, userID, "/sync/ratings/remove", key, nil)
}

// AddRating rates a title on Trakt (1-10).
func (c *Client) AddRating(ctx context.Context, userID int64, key core.RatingKey, rating float64) error {
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
	status, b, err := c.authed(ctx, userID, http.MethodPost, path, syncBody(kind, item, extra))
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
	action string
	at     time.Time
}

// startRefresh is how often a continuing "playing" timeline re-sends
// scrobble/start; Trakt expires an un-refreshed scrobble after the item's
// runtime, so this only matters for very long sessions.
const startRefresh = 15 * time.Minute

// shouldScrobble de-duplicates timeline updates: Plex clients report
// progress every ~10s, but Trakt only needs a call when playback state
// changes (start/pause/stop), plus a periodic start refresh.
func (c *Client) shouldScrobble(userID int64, key core.RatingKey, action string) bool {
	k := scrobbleKey{userID, key}
	now := c.now()
	c.scMu.Lock()
	defer c.scMu.Unlock()
	last, ok := c.lastScro[k]
	if ok && last.action == action {
		window := startRefresh // repeated start/pause timelines
		if action == "stop" {
			window = 5 * time.Minute // duplicate stop reports of one viewing
		}
		if now.Sub(last.at) < window {
			return false
		}
	}
	c.lastScro[k] = scrobbleState{action: action, at: now}
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
	if !c.Enabled() || !c.Connected(ctx, ev.UserID) {
		return nil
	}
	var err error
	progress := min(max(ev.Fraction*100, 0), 100)
	if ev.Imported && (ev.Kind == state.EventProgress || ev.At.IsZero()) {
		return nil
	}
	switch ev.Kind {
	case state.EventProgress:
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
		if action != "" && c.shouldScrobble(ev.UserID, ev.Key, action) {
			err = c.Scrobble(ctx, ev.UserID, ev.Key, action, progress)
		}
		if ev.Completed {
			c.invalidateUser(ev.UserID)
		}
	case state.EventCompleted:
		// Already recorded by the scrobble/stop of the progress event.
	case state.EventScrobble:
		err = c.AddHistory(ctx, ev.UserID, ev.Key, ev.At)
		c.invalidateUser(ev.UserID)
	case state.EventUnscrobble:
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

// SubscribeTo feeds state events into the scrobble queue (non-blocking).
func (c *Client) SubscribeTo(src interface {
	Subscribe(func(state.Event)) func()
}) func() {
	return src.Subscribe(func(ev state.Event) {
		if ev.Account {
			return
		}
		if ev.Imported {
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
//
// Cached like PullHistory.
func (c *Client) Watchlist(ctx context.Context, userID int64, limit int) ([]core.Title, error) {
	if limit <= 0 {
		limit = 20
	}
	if !c.Connected(ctx, userID) {
		return nil, nil
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
