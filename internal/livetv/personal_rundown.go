package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 7, `CREATE TABLE livetv_personal(viewer_id INTEGER NOT NULL,channel_id TEXT NOT NULL,day TEXT NOT NULL,rundown TEXT NOT NULL,cursor_ms BIGINT NOT NULL,built_at BIGINT NOT NULL,away_ms BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(viewer_id,channel_id,day));`)
	store.RegisterViewerTable("livetv", "livetv_personal", "viewer_id", store.ViewerMergeMove, "channel_id", "day")
}

type personalKey struct {
	viewer  int64
	channel string
}
type personalClock struct {
	cursor                     int64
	at, timeAway, welcomeUntil time.Time
	active                     bool
}
type personalRow struct {
	programmes []Programme
	cursor     int64
	built      time.Time
}

func (s *Service) readPersonal(ctx context.Context, k personalKey) (personalRow, error) {
	var r personalRow
	var b []byte
	var built int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT rundown,cursor_ms,built_at FROM livetv_personal WHERE viewer_id=? AND channel_id=? ORDER BY built_at DESC LIMIT 1`, k.viewer, k.channel).Scan(&b, &r.cursor, &built)
	if err == nil {
		err = json.Unmarshal(b, &r.programmes)
		r.built = time.UnixMilli(built)
	}
	return r, err
}

func immutablePersonal(old []Programme, now time.Time) ([]Programme, time.Time) {
	var keep []Programme
	end := now
	for _, p := range old {
		if !p.Start.After(now.Add(30 * time.Minute)) {
			keep = append(keep, p)
			if p.End.After(end) {
				end = p.End
			}
		}
	}
	return keep, end
}

// composePersonal preserves all programmes whose start falls in the frozen
// window, including a programme that straddles its boundary.
func composePersonal(c Channel, old []Programme, picks []core.PersonalPick, now time.Time) []Programme {
	out, at := immutablePersonal(old, now)
	until := now.Add(48 * time.Hour)
	last := map[core.RatingKey]time.Time{}
	showTime := map[string]int64{}
	kindTime := map[string]float64{}
	pickTime := map[core.RatingKey]float64{}
	for _, p := range out {
		last[p.Item.Key] = p.Start
		if p.Item.ShowKey != 0 {
			showTime[p.Start.Format("2006-01-02")+p.Item.ShowKey.String()] += p.End.Sub(p.Start).Milliseconds()
		}
	}
	weights := map[string]float64{}
	for _, i := range c.Personal.Ingredients {
		if i.Enabled && i.Share > 0 {
			if i.Kind == "collections" {
				for _, key := range i.Collections {
					weights["collection:"+key.String()] = i.Share / math.Max(1, float64(len(i.Collections)))
				}
			} else {
				weights[i.Kind] = i.Share
			}
		}
	}
	for at.Before(until) {
		best := -1
		merit := math.Inf(-1)
		lens := ""
		var block *Block
		local := at.In(zoneOf(c.Timezone))
		minute := local.Hour()*60 + local.Minute()
		for i := range c.Blocks {
			b := &c.Blocks[i]
			day := len(b.Days) == 0
			for _, d := range b.Days {
				day = day || d == int(local.Weekday())
			}
			if day && minute >= b.Start && minute < b.End {
				block = b
				lens = b.Name
				break
			}
		}
		for i, p := range picks {
			if p.Title == nil || p.Title.Runtime <= 0 {
				continue
			}
			t := p.Title
			duration := t.Runtime.Milliseconds()
			if prev, ok := last[t.Key]; ok && at.Sub(prev) < time.Duration(c.Personal.RepeatHours)*time.Hour {
				continue
			}
			if c.Personal.NightMaxMinutes > 0 && (local.Hour() >= 22 || local.Hour() < 6) && t.Runtime > time.Duration(c.Personal.NightMaxMinutes)*time.Minute {
				continue
			}
			dayShow := local.Format("2006-01-02") + t.ShowKey.String()
			if t.ShowKey != 0 && c.Personal.MaxShowShare > 0 && showTime[dayShow]+duration > int64(c.Personal.MaxShowShare)*864000 {
				continue
			}
			if block != nil && block.Filter != nil {
				f := block.Filter
				if len(f.Genres) > 0 {
					match := false
					for _, a := range f.Genres {
						for _, g := range t.Genres {
							match = match || a == g
						}
					}
					if !match {
						continue
					}
				}
			}
			weight := weights[p.Kind]
			if weight <= 0 {
				continue
			}
			score := -kindTime[p.Kind]/weight + math.Min(p.Score, 1) - pickTime[t.Key]
			if score > merit {
				best = i
				merit = score
			}
		}
		if best < 0 {
			end := at.Add(5 * time.Minute)
			out = append(out, Programme{ID: fmt.Sprintf("%s-%d", c.ID, at.UnixMilli()), ChannelID: c.ID, Start: at, End: end, Item: Item{Slate: offAirSlate, Title: c.Name, DurationMS: end.Sub(at).Milliseconds()}})
			at = end
			continue
		}
		pick := picks[best]
		it := personalItem(*pick.Title)
		end := at.Add(time.Duration(it.DurationMS) * time.Millisecond)
		out = append(out, Programme{ID: fmt.Sprintf("%s-%d", c.ID, at.UnixMilli()), ChannelID: c.ID, Start: at, End: end, Item: it, Reason: pick.Reason, Lens: lens})
		last[it.Key] = at
		pickTime[it.Key] += 1
		if it.ShowKey != 0 {
			showTime[local.Format("2006-01-02")+it.ShowKey.String()] += it.DurationMS
		}
		kindTime[pick.Kind] += float64(it.DurationMS)
		at = end
	}
	return out
}

func (s *Service) buildPersonal(ctx context.Context, c Channel, u *core.User) error {
	k := personalKey{u.ID, c.ID}
	old, err := s.readPersonal(ctx, k)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := s.o.Now()
	if !old.built.IsZero() && now.Sub(old.built) < 10*time.Minute {
		return nil
	}
	s.personalMu.Lock()
	revision := s.personalDirty[u.ID]
	prior := s.personalRecipes[k]
	s.personalMu.Unlock()
	var generation uint64
	if s.o.Watchlist != nil {
		generation = s.o.Watchlist.Generation()
	}
	pol, _ := s.Policy(ctx, u.ID)
	fingerprint, _ := json.Marshal(struct {
		Recipe              *PersonalRecipe
		Blocks              []Block
		Viewer              *core.User
		Policy              Policy
		Revision, Watchlist uint64
	}{c.Personal, c.Blocks, u, pol, revision, generation})
	if prior == string(fingerprint) && now.Sub(old.built) < time.Hour {
		return nil
	}
	cursor := old.cursor
	if cursor == 0 || !c.Personal.PauseWhenAway {
		cursor = now.UnixMilli()
	}
	weights := map[string]float64{}
	for _, i := range c.Personal.Ingredients {
		if i.Enabled && i.Share > 0 {
			if i.Kind == "collections" {
				for _, key := range i.Collections {
					weights["collection:"+key.String()] = i.Share
				}
			} else {
				weights[i.Kind] = i.Share
			}
		}
	}
	picks, err := s.o.Picker.Picks(ctx, u, weights, 500)
	if err != nil {
		return err
	}

	safe := picks[:0]
	for _, p := range picks {
		if p.Title != nil && s.allowed(ctx, u, c, &Programme{Item: itemFromTitle(*p.Title)}) {
			safe = append(safe, p)
		}
	}
	// Picking may wait for the home model. Freeze against the clock after
	// that wait, so the next thirty minutes cannot move during a slow build.
	if c.Personal.PauseWhenAway {
		cursor = s.personalCursor(k, cursor, s.o.Now())
	} else {
		cursor = s.o.Now().UnixMilli()
	}
	ps := composePersonal(c, old.programmes, safe, time.UnixMilli(cursor))
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	records := s.personalIndexRecords(ctx, ps)
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	day := time.UnixMilli(cursor).In(zoneOf(c.Timezone)).Format("2006-01-02")
	result, err := tx.ExecContext(ctx, `INSERT INTO livetv_personal(viewer_id,channel_id,day,rundown,cursor_ms,built_at) VALUES(?,?,?,?,?,?) ON CONFLICT(viewer_id,channel_id,day) DO UPDATE SET rundown=excluded.rundown,built_at=excluded.built_at WHERE livetv_personal.built_at=?`, u.ID, c.ID, time.UnixMilli(cursor).In(zoneOf(c.Timezone)).Format("2006-01-02"), b, cursor, now.UnixMilli(), old.built.UnixMilli())
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if err = s.indexPersonal(ctx, tx, u.ID, c.ID, day, records); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}

	if err == nil {
		s.personalMu.Lock()
		if s.personalLineup == nil {
			s.personalLineup = map[int64]time.Time{}
		}
		at := now
		if !at.After(s.personalLineup[u.ID]) {
			at = s.personalLineup[u.ID].Add(time.Second)
		}
		s.personalLineup[u.ID] = at
		if s.personalRecipes == nil {
			s.personalRecipes = map[personalKey]string{}
		}
		s.personalRecipes[k] = string(fingerprint)
		s.personalMu.Unlock()
		if s.o.PersonalChanged != nil {
			s.o.PersonalChanged(u.ID)
		}
	}
	return err
}

func (s *Service) personalProgrammes(ctx context.Context, c Channel, u *core.User, from, until time.Time) ([]Programme, error) {
	return s.personalProgrammesLimit(ctx, c, u, from, until, 0)
}
func (s *Service) personalProgrammesLimit(ctx context.Context, c Channel, u *core.User, from, until time.Time, limit int) ([]Programme, error) {
	out := []Programme{}
	if u == nil || u.ID <= 0 || s.o.Picker == nil {
		return out, nil
	}
	var cursorMS int64
	var day string
	err := s.o.DB.QueryRowContext(ctx, `SELECT day,cursor_ms FROM livetv_personal WHERE viewer_id=? AND channel_id=? ORDER BY built_at DESC LIMIT 1`, u.ID, c.ID).Scan(&day, &cursorMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotAvailable
	}
	if err != nil {
		return nil, err
	}
	shift := time.Duration(0)
	if c.Personal.PauseWhenAway {
		now := s.o.Now()
		k := personalKey{u.ID, c.ID}
		s.personalMu.Lock()
		if clock := s.personalClocks[k]; clock != nil && clock.active && now.Before(clock.welcomeUntil) {
			// The programme begins after welcome, not at each planner refresh.
			now = clock.welcomeUntil
		}
		s.personalMu.Unlock()
		cursor := s.personalCursor(k, cursorMS, now)
		shift = time.Duration(now.UnixMilli()-cursor) * time.Millisecond
	}

	// Seek the latest start at/before the requested window, then scan only
	// overlapping/future rows. The primary key bounds work by returned output.
	query := `SELECT data FROM livetv_personal_programmes WHERE viewer_id=? AND channel_id=? AND day=? AND start_ms>=COALESCE((SELECT MAX(start_ms) FROM livetv_personal_programmes WHERE viewer_id=? AND channel_id=? AND day=? AND start_ms<=?),?) AND start_ms<? AND end_ms>? ORDER BY start_ms`
	args := []any{u.ID, c.ID, day, u.ID, c.ID, day, from.Add(-shift).UnixMilli(), from.Add(-shift).UnixMilli(), until.Add(-shift).UnixMilli(), from.Add(-shift).UnixMilli()}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.o.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var record personalProgrammeRecord
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &record); err != nil {
			return nil, err
		}
		p := record.Programme
		p.policyTag = &broadcastPolicyTag{programme: p, title: record.Title, catalog: record.Catalog}
		p.Start = p.Start.Add(shift)
		p.End = p.End.Add(shift)
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) personalLoop() {
	defer s.wg.Done()

	if source, ok := s.o.State.(interface {
		Subscribe(func(state.Event)) func()
	}); ok {
		unsub := source.Subscribe(func(e state.Event) { s.PersonalChanged(e.UserID) })
		defer unsub()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	next := time.Time{}
	for {
		now := s.o.Now()
		s.advancePersonal(now)
		if !now.Before(next) {
			next = now.Add(time.Minute)
			users, err := s.o.Users.Users(s.ctx)
			if err == nil {
				cs, err := s.virtualChannels(s.ctx)
				if err == nil {
					for _, u := range users {

						for _, c := range cs {
							if c.Personal == nil || !c.Enabled {
								continue
							}
							c = s.personalChannel(s.ctx, c, &u)
							if !s.allowed(s.ctx, &u, c, nil) {
								continue
							}
							if err := s.buildPersonal(s.ctx, c, &u); err != nil {
								s.o.Logger.Debug("personal rundown build failed", "viewer", u.ID, "channel", c.ID, "error", err)
							}
							if c.Look != nil && s.o.Graphics != nil {
								r, err := s.readPersonal(s.ctx, personalKey{u.ID, c.ID})
								if err == nil {
									for _, p := range r.programmes {
										if p.Start.UnixMilli() <= r.cursor && p.End.UnixMilli() > r.cursor {
											minutes := (r.cursor - p.Start.UnixMilli()) / 60000
											p.Item.Slate = "welcome"
											p.Item.Summary = strconv.FormatInt(minutes, 10)
											s.o.Graphics.Ensure(s.ctx, s.clipRequest(c, p))
											break
										}
									}
								}
							}

						}
					}
				}
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Service) advancePersonal(now time.Time) {
	active := map[personalKey]bool{}
	s.mu.Lock()
	for _, ss := range s.sessions {
		if ss.info.Channel.Personal != nil && ss.info.Channel.Personal.PauseWhenAway {
			active[personalKey{ss.info.UserID, ss.info.Channel.ID}] = true
		}
	}
	s.mu.Unlock()
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	if s.personalClocks == nil {
		s.personalClocks = map[personalKey]*personalClock{}
	}
	for k := range active {
		if s.personalClocks[k] == nil {
			r, err := s.readPersonal(s.ctx, k)
			if err == nil {
				s.personalClocks[k] = &personalClock{cursor: r.cursor, at: now, timeAway: now}
			}
		}
	}
	for k, clock := range s.personalClocks {
		if !clock.active {
			continue
		}
		if clock.active && now.After(clock.welcomeUntil) {
			start := clock.at
			if clock.welcomeUntil.After(start) {
				start = clock.welcomeUntil
			}
			clock.cursor += now.UnixMilli() - start.UnixMilli()
		}
		if clock.active && !active[k] {
			clock.timeAway = now
		}
		clock.active = active[k]
		clock.at = now
		_, _ = s.o.DB.ExecContext(s.ctx, `UPDATE livetv_personal SET cursor_ms=?,away_ms=? WHERE viewer_id=? AND channel_id=?`, clock.cursor, now.UnixMilli(), k.viewer, k.channel)
	}
}

func (s *Service) beginPersonal(c Channel, u *core.User) {
	if c.Personal == nil || !c.Personal.PauseWhenAway || u == nil {
		return
	}
	k := personalKey{u.ID, c.ID}
	now := s.o.Now()
	r, err := s.readPersonalClock(s.ctx, k)
	if err != nil {
		return
	}
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	if s.personalClocks == nil {
		s.personalClocks = map[personalKey]*personalClock{}
	}
	clock := s.personalClocks[k]
	if clock == nil {
		var away int64
		_ = s.o.DB.QueryRowContext(s.ctx, `SELECT away_ms FROM livetv_personal WHERE viewer_id=? AND channel_id=?`, u.ID, c.ID).Scan(&away)
		clock = &personalClock{cursor: r.cursor}
		if away > 0 {
			clock.timeAway = time.UnixMilli(away)
		}
		s.personalClocks[k] = clock
	}
	if clock.active {
		return
	}
	clock.at = now
	clock.active = true
	if !clock.timeAway.IsZero() && now.Sub(clock.timeAway) > 10*time.Minute && c.Look != nil && s.o.Graphics != nil {
		if d, ok := s.o.Graphics.Duration(c.Look.LookID, "welcome"); ok && d > 0 {
			clock.welcomeUntil = now.Add(d)
		}
	}
}
func (s *Service) endPersonal(c Channel, viewer int64) {
	if c.Personal == nil || !c.Personal.PauseWhenAway {
		return
	}
	// Another session for the same viewer keeps their clock running.
	s.mu.Lock()
	active := false
	for _, ss := range s.sessions {
		if ss.info.UserID == viewer && ss.info.Channel.ID == c.ID {
			ss.mu.Lock()
			active = active || !ss.closed
			ss.mu.Unlock()
		}
	}
	s.mu.Unlock()
	if active {
		return
	}
	now := s.o.Now()
	k := personalKey{viewer, c.ID}
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	clock := s.personalClocks[k]
	if clock == nil || !clock.active {
		return
	}
	if now.After(clock.welcomeUntil) {
		start := clock.at
		if clock.welcomeUntil.After(start) {
			start = clock.welcomeUntil
		}
		clock.cursor += now.UnixMilli() - start.UnixMilli()
	}
	clock.active = false
	clock.timeAway = now
	clock.at = now
	_, _ = s.o.DB.ExecContext(s.ctx, `UPDATE livetv_personal SET cursor_ms=?,away_ms=? WHERE viewer_id=? AND channel_id=?`, clock.cursor, now.UnixMilli(), viewer, c.ID)
}
func (s *Service) personalWelcome(c Channel, now time.Time) (Programme, bool) {
	if c.Personal == nil || c.personalViewer == nil {
		return Programme{}, false
	}
	k := personalKey{c.personalViewer.ID, c.ID}
	s.personalMu.Lock()
	clock := s.personalClocks[k]
	if clock == nil || !clock.active || !now.Before(clock.welcomeUntil) {
		s.personalMu.Unlock()
		return Programme{}, false
	}
	end, cursor := clock.welcomeUntil, clock.cursor
	s.personalMu.Unlock()
	r, err := s.readPersonal(s.ctx, k)
	if err != nil {
		return Programme{}, false
	}
	for _, p := range r.programmes {
		if p.Start.UnixMilli() <= cursor && p.End.UnixMilli() > cursor {
			resumeMinutes := (cursor - p.Start.UnixMilli()) / 60000
			p.ID = "welcome-" + c.ID + "-" + strconv.FormatInt(end.UnixMilli(), 10)
			p.Start = now
			if c.Look != nil {
				if d, ok := s.o.Graphics.Duration(c.Look.LookID, "welcome"); ok {
					p.Start = end.Add(-d)
				}
			}
			p.End = end
			p.Item.Slate = "welcome"
			p.Item.Summary = strconv.FormatInt(resumeMinutes, 10)
			p.Item.Key = 0
			return p, true
		}
	}
	return Programme{}, false
}

// PersonalChanged invalidates only this viewer; recomposition remains throttled.
func (s *Service) PersonalChanged(viewer int64) {
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	if s.personalDirty == nil {
		s.personalDirty = map[int64]uint64{}
	}
	s.personalDirty[viewer]++
}

func personalItem(t core.Title) Item {
	it := itemFromTitle(t)
	it.MediaType = string(t.Type)
	it.Genres = t.Genres
	it.Art = t.BackdropPath
	it.ShowTitle = t.ShowTitle
	if t.Type == core.TypeEpisode {
		it.EpisodeTitle = t.Title
	}
	return it
}

func (s *Service) personalCursor(k personalKey, cursor int64, now time.Time) int64 {
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	if clock := s.personalClocks[k]; clock != nil {
		cursor = clock.cursor
		if clock.active && now.After(clock.welcomeUntil) {
			start := clock.at
			if clock.welcomeUntil.After(start) {
				start = clock.welcomeUntil
			}
			cursor += now.UnixMilli() - start.UnixMilli()
		}
	}
	return cursor
}
