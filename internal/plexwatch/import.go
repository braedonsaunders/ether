package plexwatch

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
)

// The sweep walks verified movie and show identities in key order (movie
// keys sit below show keys). Episodes are read only for shows whose watched
// episode count or last view moved since the previous read, so the cost of
// a full sweep follows the number of movies and shows Ether has matched,
// not the number of episodes.
var (
	sweepAfter = core.MovieKey(0)
	sweepBelow = core.SeasonKey(0, 0)
)

// activeWindow bounds the viewer activity that marks a show as active.
const activeWindow = 180 * 24 * time.Hour

// importPass reads one bounded slice of the viewer's Plex account state.
// Episodes of shows queued by a sweep page are read before the next page.
// When the sweep wraps, the active-show pass follows: Plex keeps an
// episode's position on the episode only (the show's userState does not
// move), so the episodes of the viewer's active shows are read each sweep.
// An empty cursor means the last sweep finished: a pass that starts there
// with expansions still queued only finishes those, and the next sweep
// starts with the next scheduled pass. finished is true when the sweep and
// the active-show pass are done (or idle) and no expansion is left.
func (s *Service) importPass(ctx context.Context, acct accountRow) (bool, error) {
	if s.guid == nil {
		return true, nil
	}
	budget := s.importBudget
	// drain reads queued show expansions while budget lasts. It reports
	// whether any expansion is still queued.
	drain := func() (bool, error) {
		for budget > 0 {
			requested, more, err := s.expandStep(ctx, acct)
			if err != nil || !more {
				return false, err
			}
			if requested {
				budget--
				if err := s.wait(ctx); err != nil {
					return true, err
				}
			}
		}
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_expand WHERE user_id=?`, acct.userID).Scan(&n)
		return n > 0, err
	}
	var queued int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_expand WHERE user_id=?`, acct.userID).Scan(&queued); err != nil {
		return false, err
	}
	if remain, err := drain(); err != nil || remain {
		return false, err
	}
	if acct.activeAfter >= 0 {
		return s.activePass(ctx, acct, acct.activeAfter, &budget, drain)
	}
	if acct.cursor == "" && queued > 0 {
		return true, nil
	}
	cursor := sweepAfter
	if n, err := strconv.ParseInt(acct.cursor, 10, 64); err == nil && core.RatingKey(n) > cursor {
		cursor = core.RatingKey(n)
	}
	for budget > 0 {
		page, err := s.guid.VerifiedRange(ctx, cursor, sweepBelow, maxStateBatch)
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			if err := s.saveCursor(ctx, acct, "", true); err != nil {
				return false, err
			}
			return s.activePass(ctx, acct, 0, &budget, drain)
		}
		requested, err := s.importBatch(ctx, acct, page)
		if err != nil {
			return false, err
		}
		cursor = page[len(page)-1].Key
		if err := s.saveCursor(ctx, acct, strconv.FormatInt(int64(cursor), 10), false); err != nil {
			return false, err
		}
		if requested {
			budget--
			if err := s.wait(ctx); err != nil {
				return false, err
			}
		}
		if remain, err := drain(); err != nil || remain {
			return false, err
		}
	}
	return false, nil
}

// activePass queues the episodes of the viewer's active shows after the
// show cursor and reads them while budget lasts. The cursor is saved per
// show, so the next pass resumes there. Active shows are those with any
// watch, progress or rating activity in Ether within activeWindow, plus
// shows on the viewer's watchlist; both come from this viewer's own rows,
// so the pass follows the viewer's activity, not the library's size.
func (s *Service) activePass(ctx context.Context, acct accountRow, after int64, budget *int, drain func() (bool, error)) (bool, error) {
	if err := s.saveActive(ctx, acct, after); err != nil {
		return false, err
	}
	shows, err := s.activeShows(ctx, acct.userID, core.RatingKey(after))
	if err != nil {
		return false, err
	}
	for _, show := range shows {
		if *budget <= 0 {
			return false, nil
		}
		// A show already being read keeps its place.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_expand(user_id, show_key, after_key) VALUES(?,?,0)
			ON CONFLICT(user_id, show_key) DO NOTHING`, acct.userID, int64(show)); err != nil {
			return false, err
		}
		if err := s.saveActive(ctx, acct, int64(show)); err != nil {
			return false, err
		}
		if remain, err := drain(); err != nil || remain {
			return false, err
		}
	}
	return true, s.saveActive(ctx, acct, -1)
}

// activeShows lists, in key order after after, the show keys of the
// viewer's recent activity and watchlist.
func (s *Service) activeShows(ctx context.Context, userID int64, after core.RatingKey) ([]core.RatingKey, error) {
	set := map[core.RatingKey]bool{}
	add := func(k core.RatingKey) {
		switch k.Kind() {
		case core.KindShow, core.KindSeason, core.KindEpisode:
			if show := core.ShowKey(k.TMDB()); show > after {
				set[show] = true
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key FROM state_watch WHERE user_id=? AND updated_at>=?`,
		userID, s.now().Add(-activeWindow).UnixNano())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		add(core.RatingKey(k))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if s.watchlist != nil {
		keys, err := s.watchlist(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			add(k)
		}
	}
	out := make([]core.RatingKey, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

// saveActive stores the active-show cursor (-1: not running) for the
// generation that read it.
func (s *Service) saveActive(ctx context.Context, acct accountRow, after int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET active_after=? WHERE user_id=? AND token_generation=? AND revoked=0`,
		after, acct.userID, acct.tokenGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errStaleGeneration
	}
	return nil
}

func (s *Service) wait(ctx context.Context) error {
	if s.pace <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(s.pace)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// saveCursor persists the sweep position for the generation that read it,
// so a reconnect or disconnect during a pass cannot be overwritten.
func (s *Service) saveCursor(ctx context.Context, acct accountRow, cursor string, wrapped bool) error {
	q := `UPDATE plexwatch_account SET import_cursor=?, updated_at=? WHERE user_id=? AND token_generation=? AND revoked=0`
	args := []any{cursor, s.now().UnixNano(), acct.userID, acct.tokenGeneration}
	if wrapped {
		q = `UPDATE plexwatch_account SET import_cursor=?, updated_at=?, last_sweep=? WHERE user_id=? AND token_generation=? AND revoked=0`
		args = []any{cursor, s.now().UnixNano(), s.now().Unix(), acct.userID, acct.tokenGeneration}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errStaleGeneration
	}
	return nil
}

// expandStep reads the next page of episodes for the first queued show.
func (s *Service) expandStep(ctx context.Context, acct accountRow) (requested, more bool, err error) {
	var show, after int64
	err = s.db.QueryRowContext(ctx, `SELECT show_key, after_key FROM plexwatch_expand WHERE user_id=? ORDER BY show_key LIMIT 1`, acct.userID).Scan(&show, &after)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	tmdb := core.RatingKey(show).TMDB()
	lo, hi := core.EpisodeKey(tmdb, 0, 0)-1, core.EpisodeKey(tmdb+1, 0, 0)
	if core.RatingKey(after) > lo {
		lo = core.RatingKey(after)
	}
	page, err := s.guid.VerifiedRange(ctx, lo, hi, maxStateBatch)
	if err != nil {
		return false, false, err
	}
	if len(page) == 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM plexwatch_expand WHERE user_id=? AND show_key=? AND after_key=?`, acct.userID, show, after)
		return false, true, err
	}
	requested, err = s.importBatch(ctx, acct, page)
	if err != nil {
		return requested, false, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE plexwatch_expand SET after_key=? WHERE user_id=? AND show_key=? AND after_key=?`,
		int64(page[len(page)-1].Key), acct.userID, show, after)
	return requested, true, err
}

type remoteRow struct {
	watched    bool
	viewedAt   int64
	offset     int64 // ms
	viewState  string
	rating     int
	leaves     int
	observedAt time.Time
	ours       bool
}

// importBatch reads one batch of userState and applies it. requested is
// false when the page held nothing that could be asked about.
func (s *Service) importBatch(ctx context.Context, acct accountRow, page []plexguid.Verified) (bool, error) {
	byHex := make(map[string]core.RatingKey, len(page))
	keys := make([]string, 0, len(page))
	for _, v := range page {
		if v.GUID == "" || !guidKindMatches(v.GUID, v.Key) {
			continue
		}
		switch v.Key.Kind() {
		case core.KindMovie, core.KindShow, core.KindEpisode:
		default:
			continue
		}
		h := guidHex(v.GUID)
		if h == "" {
			continue
		}
		if _, dup := byHex[h]; dup {
			continue
		}
		byHex[h] = v.Key
		keys = append(keys, h)
	}
	if len(keys) == 0 {
		return false, nil
	}
	states, err := s.client.userStates(ctx, acct.token, keys)
	if err != nil {
		return true, err
	}
	prev, err := s.loadRemote(ctx, acct.userID, byHex)
	if err != nil {
		return true, err
	}
	for _, h := range keys {
		cur, ok := states[h]
		if !ok {
			continue // no state reported: nothing is known, nothing changes
		}
		key := byHex[h]
		if !typeMatches(cur.Type, key) {
			continue
		}
		p, seen := prev[key]
		if key.Kind() == core.KindShow {
			err = s.observeShow(ctx, acct.userID, key, p, seen, cur)
		} else {
			err = s.applyRemote(ctx, acct.userID, key, p, seen, cur)
		}
		if err != nil {
			return true, err
		}
	}
	return true, nil
}

func typeMatches(typ string, key core.RatingKey) bool {
	switch typ {
	case "":
		return true
	case "movie":
		return key.Kind() == core.KindMovie
	case "show":
		return key.Kind() == core.KindShow
	case "episode":
		return key.Kind() == core.KindEpisode
	default:
		return false
	}
}

func (s *Service) loadRemote(ctx context.Context, userID int64, byHex map[string]core.RatingKey) (map[core.RatingKey]remoteRow, error) {
	out := make(map[core.RatingKey]remoteRow, len(byHex))
	if len(byHex) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(byHex)+1)
	args = append(args, userID)
	marks := make([]string, 0, len(byHex))
	for _, k := range byHex {
		args = append(args, int64(k))
		marks = append(marks, "?")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key, watched, viewed_at, view_offset, view_state, rating, leaves, observed_at, ours FROM plexwatch_remote
		WHERE user_id=? AND rating_key IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, observed int64
		var watched, ours int
		var r remoteRow
		if err := rows.Scan(&key, &watched, &r.viewedAt, &r.offset, &r.viewState, &r.rating, &r.leaves, &observed, &ours); err != nil {
			return nil, err
		}
		r.watched, r.ours = watched != 0, ours != 0
		r.observedAt = time.Unix(0, observed)
		out[core.RatingKey(key)] = r
	}
	return out, rows.Err()
}

// observeShow queues the show's episodes for reading when its watched
// episode count or last view moved since the previous read.
func (s *Service) observeShow(ctx context.Context, userID int64, key core.RatingKey, prev remoteRow, seen bool, cur userState) error {
	changed := cur.ViewedLeaf > 0 || cur.ViewedAt > 0
	if seen {
		changed = prev.leaves != cur.ViewedLeaf || prev.viewedAt != cur.ViewedAt
	}
	if changed {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_expand(user_id, show_key, after_key) VALUES(?,?,0)
			ON CONFLICT(user_id, show_key) DO UPDATE SET after_key=0`, userID, int64(key)); err != nil {
			return err
		}
	}
	if !seen && cur.ViewedLeaf == 0 && cur.ViewedAt == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_remote(user_id, rating_key, viewed_at, leaves, observed_at) VALUES(?,?,?,?,?)
		ON CONFLICT(user_id, rating_key) DO UPDATE SET viewed_at=excluded.viewed_at, leaves=excluded.leaves, observed_at=excluded.observed_at`,
		userID, int64(key), cur.ViewedAt, cur.ViewedLeaf, s.now().UnixNano())
	return err
}

// applyRemote brings one Plex change into Ether. Only a change since the
// previous read counts, and the newest change wins per aspect:
//
//   - watched on Plex: Plex's lastViewedAt competes with the local watch
//     clock in state.ApplyRemote.
//   - unwatched or rating changed on Plex: Plex records no time for these,
//     so the change happened after the previous read. A local change after
//     that read wins; otherwise Plex's change applies now.
//   - a resume position set on Plex: its lastViewedAt competes with the
//     local watch clock in state.ApplyRemoteResume, so a stale position
//     never resurrects a title finished or marked in Ether since. A watch
//     change in the same read wins over the position. A position cleared
//     on Plex follows the untimed rule above.
//   - a value Ether wrote itself (ours) is adopted, not applied. Timeline
//     writes move lastViewedAt, so a lastViewedAt within ownSlack of
//     Ether's own write is that write.
//
// Imported changes are marked as account events, which the journal never
// queues for sending, so nothing echoes back to Plex.
func (s *Service) applyRemote(ctx context.Context, userID int64, key core.RatingKey, prev remoteRow, seen bool, cur userState) error {
	now := s.now()
	local, err := s.localWatch(ctx, userID, key)
	if err != nil {
		return err
	}
	// A view on Plex that leaves a new resume position is playback in
	// progress (a rewatch), not another completion.
	resumed := cur.Offset > 0 && cur.Offset != prev.offset
	var watched *bool
	var watchAt time.Time
	on, off := true, false
	switch {
	case !seen:
		if cur.Watched {
			watched, watchAt = &on, unixOrZero(cur.ViewedAt)
		}
	case prev.ours && cur.Watched == prev.watched:
	case cur.Watched && (!prev.watched || (cur.ViewedAt > prev.viewedAt && !resumed)):
		watched, watchAt = &on, unixOrZero(cur.ViewedAt)
	case !cur.Watched && prev.watched:
		if local.exists && local.watched && !local.watchAt.After(prev.observedAt) {
			watched, watchAt = &off, now
		}
	}
	var rating *float64
	var ratingAt time.Time
	switch {
	case !seen:
		if cur.Rating > 0 && local.rating == 0 {
			v := float64(cur.Rating)
			rating, ratingAt = &v, now
		}
	case prev.ours && cur.Rating == prev.rating:
	case cur.Rating != prev.rating:
		if !local.ratingAt.After(prev.observedAt) && plexRating(local.rating) != cur.Rating && (cur.Rating > 0 || local.rating > 0) {
			v := float64(cur.Rating)
			rating, ratingAt = &v, now
		}
	}
	resume := int64(-1)
	var resumeAt time.Time
	ownEcho := seen && prev.ours && cur.ViewedAt <= prev.observedAt.Unix()+ownSlack
	switch {
	case watched != nil, cur.Offset == prev.offset, ownEcho:
	case cur.Offset > 0:
		if cur.ViewedAt > 0 {
			resume, resumeAt = cur.Offset, unixOrZero(cur.ViewedAt)
		}
	default:
		if local.offset > 0 && !local.watchAt.After(prev.observedAt) {
			resume, resumeAt = 0, now
		}
	}
	if watched != nil || rating != nil {
		err := s.state.ApplyRemote(ctx, userID, key, watched, watchAt, rating, ratingAt)
		if err != nil && !errors.Is(err, state.ErrLocalNewer) {
			return err
		}
	}
	applied := false
	if resume >= 0 {
		err := s.state.ApplyRemoteResume(ctx, userID, key, time.Duration(resume)*time.Millisecond, resumeAt)
		if err != nil && !errors.Is(err, state.ErrLocalNewer) {
			return err
		}
		applied = err == nil && resume > 0
	}
	if err := s.applyContinue(ctx, userID, key, prev, seen, cur, local, ownEcho, applied); err != nil {
		return err
	}
	if !seen && !cur.Watched && cur.ViewedAt == 0 && cur.Rating == 0 && cur.Offset == 0 && cur.ViewState == "" {
		return nil
	}
	w := 0
	if cur.Watched {
		w = 1
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plexwatch_remote(user_id, rating_key, watched, viewed_at, view_offset, view_state, rating, observed_at, ours) VALUES(?,?,?,?,?,?,?,?,0)
		ON CONFLICT(user_id, rating_key) DO UPDATE SET watched=excluded.watched, viewed_at=excluded.viewed_at, view_offset=excluded.view_offset,
		  view_state=excluded.view_state, rating=excluded.rating, observed_at=excluded.observed_at, ours=0`,
		userID, int64(key), w, cur.ViewedAt, cur.Offset, cur.ViewState, cur.Rating, now.UnixNano())
	return err
}

// applyContinue brings Plex's Continue Watching removal into Ether, or a
// newer Plex playback of a title removed in Ether back into it.
//
//   - Removed on Plex (viewState turned "ignore") while Ether has the title
//     in progress: Ether removes it too, keeping the resume point. Plex does
//     not time a removal, so it applies only when Ether has not played the
//     title since the previous read (on a first read, since Plex's last
//     view of it). Ether's own removal is recorded as ignore, so it is not
//     read back as Plex's.
//   - Played on Plex after the viewer removed it in Ether: it comes back,
//     as local playback would bring it back. Ether's own position writes
//     (ownEcho) never do.
func (s *Service) applyContinue(ctx context.Context, userID int64, key core.RatingKey, prev remoteRow, seen bool, cur userState, local localState, ownEcho, applied bool) error {
	switch {
	case cur.ViewState == viewIgnore && prev.viewState != viewIgnore:
		if local.offset <= 0 && !applied {
			return nil
		}
		since := unixOrZero(cur.ViewedAt)
		if seen {
			since = prev.observedAt
		}
		if !applied && local.watchAt.After(since) {
			return nil
		}
		if _, hidden, err := s.state.ContinueHiddenAt(ctx, userID, key); err != nil || hidden {
			return err
		}
		if err := s.state.HideContinueAccount(ctx, userID, key); err != nil {
			return err
		}
		if s.continued != nil {
			return s.continued(ctx, userID, key, true)
		}
	case cur.ViewState != viewIgnore && cur.Offset > 0 && cur.ViewedAt > 0 && !ownEcho &&
		(!seen || cur.Offset != prev.offset || prev.viewState == viewIgnore || cur.ViewedAt > prev.viewedAt):
		hiddenAt, hidden, err := s.state.ContinueHiddenAt(ctx, userID, key)
		if err != nil || !hidden || hiddenAt.IsZero() || !unixOrZero(cur.ViewedAt).After(hiddenAt) {
			return err
		}
		if err := s.state.RestoreContinue(ctx, userID, key); err != nil {
			return err
		}
		if s.continued != nil {
			return s.continued(ctx, userID, key, false)
		}
	}
	return nil
}

type localState struct {
	exists   bool
	watched  bool
	offset   time.Duration
	rating   float64
	watchAt  time.Time
	ratingAt time.Time
}

func (s *Service) localWatch(ctx context.Context, userID int64, key core.RatingKey) (localState, error) {
	var count int
	var watchAt, ratingAt, offset int64
	var out localState
	err := s.db.QueryRowContext(ctx, `SELECT view_count, view_offset_ns, user_rating, watch_at, rating_at FROM state_watch WHERE user_id=? AND rating_key=?`, userID, int64(key)).
		Scan(&count, &offset, &out.rating, &watchAt, &ratingAt)
	if errors.Is(err, sql.ErrNoRows) {
		return localState{}, nil
	}
	if err != nil {
		return localState{}, err
	}
	out.exists, out.watched, out.offset = true, count > 0, time.Duration(offset)
	out.watchAt, out.ratingAt = unixNanoOrZero(watchAt), unixNanoOrZero(ratingAt)
	return out, nil
}

func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func unixNanoOrZero(ns int64) time.Time {
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}
