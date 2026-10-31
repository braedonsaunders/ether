package personalize

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/schedule"
)

// ---- model cache and background builds ------------------------------------------

// request asks for a viewer's model to be (re)built in the background and
// returns a handle that completes when it is.
func (e *Engine) request(userID int64) *flight {
	e.mu.Lock()
	if f, ok := e.flights[userID]; ok {
		if f.started {
			// The running build may predate what triggered this request
			// (a watch-state change): build again when it finishes.
			f.again = true
		}
		e.mu.Unlock()
		return f
	}
	f := &flight{done: make(chan struct{})}
	e.flights[userID] = f
	closed := e.closed
	e.mu.Unlock()
	if closed {
		e.finish(userID)
		return f
	}
	select {
	case e.queue <- userID:
	default:
		// Queue full: hand off without blocking the caller.
		go func() {
			select {
			case e.queue <- userID:
			case <-e.ctx.Done():
				e.finish(userID)
			}
		}()
	}
	return f
}

func (e *Engine) finish(userID int64) {
	e.mu.Lock()
	f := e.flights[userID]
	delete(e.flights, userID)
	e.mu.Unlock()
	if f != nil {
		close(f.done)
	}
}

func (e *Engine) worker(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Release anyone waiting on queued builds.
			for {
				select {
				case id := <-e.queue:
					e.finish(id)
				default:
					return
				}
			}
		case id := <-e.queue:
			for {
				e.mu.Lock()
				if f := e.flights[id]; f != nil {
					f.started, f.again = true, false
				}
				e.mu.Unlock()
				e.buildAndStore(ctx, id)
				// Decide and retire the flight under one lock: a request
				// that lands between the check and the retirement would
				// otherwise set again on a flight about to close, and its
				// rebuild would be lost.
				e.mu.Lock()
				f := e.flights[id]
				again := f != nil && f.again && ctx.Err() == nil
				if !again {
					delete(e.flights, id)
				}
				e.mu.Unlock()
				if !again {
					if f != nil {
						close(f.done)
					}
					break
				}
			}
		}
	}
}

// buildAndStore builds a model and installs it unless a newer one landed
// meanwhile.
func (e *Engine) buildAndStore(ctx context.Context, userID int64) (*userModel, error) {
	bctx, cancel := context.WithTimeout(ctx, e.opts.BuildTimeout)
	defer cancel()
	e.mu.Lock()
	u, ok := e.users[userID]
	e.mu.Unlock()
	if !ok {
		u = core.User{ID: userID}
	}
	if userID == genericID {
		u = core.User{ID: genericID}
	}
	m, err := e.buildModel(bctx, u, userID == genericID)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("personalize: model build failed", "user", userID, "err", err)
		}
		return nil, err
	}
	if !m.generic && e.opts.NotInterestedSnapshot != nil {
		signals, revision, err := e.opts.NotInterestedSnapshot(bctx, &u)
		if err != nil {
			return nil, err
		}
		_, signature := snapshotSet(signals, revision)
		if signature != m.preferenceSignature {
			e.request(userID)
			return nil, fmt.Errorf("personalize: preference changed during model build")
		}
	}
	e.mu.Lock()
	if old := e.models[userID]; old == nil || !old.builtAt.After(m.builtAt) {
		e.version++
		m.version = e.version
		e.models[userID] = m
	}
	e.mu.Unlock()
	e.log.Debug("personalize: model built", "user", userID, "rows", len(m.rows), "took", m.took,
		"confidence", fmt.Sprintf("%.2f", m.taste.Confidence))
	return m, nil
}

// ModelReady reports whether a request can rank without waiting for a build.
// A fresh viewer model or the generic model is enough. Plex still waits in
// modelFor when neither exists; Jellyfin and Emby Next Up use this so a cold
// start serves the un-personalized order instead.
func (e *Engine) ModelReady(userID int64) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	fresh := func(m *userModel) bool {
		return m != nil && now.Sub(m.builtAt) <= e.opts.ModelTTL
	}
	return fresh(e.models[userID]) || fresh(e.models[genericID])
}

// modelFor returns the viewer's model for a request. A stale model is
// served while a rebuild runs. A viewer without a model gets the generic
// model (and their own is queued); with wait set (owner preview) the call
// waits for the viewer's own build instead.
func (e *Engine) modelFor(ctx context.Context, user *core.User, wait bool) *userModel {
	var signature string
	revisionOK := true
	if e.opts.NotInterestedSnapshot != nil && user != nil && user.ID > 0 {
		signals, rev, err := e.opts.NotInterestedSnapshot(ctx, user)
		revisionOK = err == nil
		_, signature = snapshotSet(signals, rev)
	}
	e.mu.Lock()
	if user.ID != 0 || user.Username != "" {
		e.users[user.ID] = *user
	}
	m := e.models[user.ID]
	if !revisionOK || (m != nil && e.opts.NotInterestedSnapshot != nil && m.preferenceSignature != signature) {
		m = nil
	}
	stale := m != nil && e.now().Sub(m.builtAt) > e.opts.ModelTTL
	e.mu.Unlock()
	if m != nil && !stale {
		return m
	}
	f := e.request(user.ID)
	if m != nil {
		return m
	}
	if wait {
		select {
		case <-f.done:
		case <-ctx.Done():
		}
		e.mu.Lock()
		m = e.models[user.ID]
		e.mu.Unlock()
		if m != nil && revisionOK && (e.opts.NotInterestedSnapshot == nil || m.preferenceSignature == signature) {
			return m
		}
	}
	e.mu.Lock()
	g := e.models[genericID]
	e.mu.Unlock()
	if g != nil {
		return g
	}
	// First requests after startup: wait briefly for the generic model.
	gf := e.request(genericID)
	t := time.NewTimer(e.opts.ColdWait)
	defer t.Stop()
	select {
	case <-gf.done:
	case <-f.done:
	case <-t.C:
	case <-ctx.Done():
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := e.models[user.ID]; m != nil && revisionOK && (e.opts.NotInterestedSnapshot == nil || m.preferenceSignature == signature) {
		return m
	}
	return e.models[genericID]
}

// Rebuild builds a viewer's model now and waits for it (owner refresh,
// tests).
func (e *Engine) Rebuild(ctx context.Context, user *core.User) error {
	if user == nil {
		return fmt.Errorf("personalize: no user")
	}
	e.mu.Lock()
	e.users[user.ID] = *user
	e.mu.Unlock()
	_, err := e.buildAndStore(ctx, user.ID)
	return err
}

// scheduler keeps active viewers' models fresh and applies retention.
func (e *Engine) scheduler(ctx context.Context) {
	defer e.wg.Done()
	lastPurge := time.Time{}
	tick := func() {
		e.refreshActive(ctx)
		if e.now().Round(0).Sub(lastPurge.Round(0)) > 24*time.Hour {
			lastPurge = e.now()
			if n, err := e.store.purge(ctx, e.now(), e.opts.Retention); err != nil {
				e.log.Warn("personalize: retention failed", "err", err)
			} else if n > 0 {
				e.log.Info("personalize: retention removed old rows", "rows", n)
			}
		}
	}
	for ctx.Err() == nil {
		tick()
		if !schedule.Wait(ctx, e.now, e.opts.RefreshEvery) {
			return
		}
	}
}

// refreshActive queues builds for viewers active in the last 30 days whose
// model is missing or stale, and for the generic model.
func (e *Engine) refreshActive(ctx context.Context) {
	now := e.now()
	seen, err := e.store.lastSeen(ctx)
	if err != nil {
		e.log.Debug("personalize: last seen failed", "err", err)
	}
	var users []core.User
	if e.opts.Users != nil {
		if us, err := e.opts.Users.Users(ctx); err == nil {
			users = us
		}
	}
	e.mu.Lock()
	for _, u := range users {
		e.users[u.ID] = u
	}
	type cand struct {
		id   int64
		seen time.Time
	}
	var due []cand
	for id := range e.users {
		last := seen[id]
		if m := e.models[id]; m != nil && now.Sub(m.builtAt) < e.opts.ModelTTL {
			continue
		}
		if last.IsZero() && e.models[id] == nil && len(seen) > 0 {
			// Never seen on Home: build on first visit instead.
			continue
		}
		if !last.IsZero() && now.Sub(last) > 30*24*time.Hour {
			continue
		}
		due = append(due, cand{id, last})
	}
	g := e.models[genericID]
	e.mu.Unlock()
	if g == nil || now.Sub(g.builtAt) > e.opts.ModelTTL {
		e.request(genericID)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].seen.After(due[j].seen) })
	for _, c := range due {
		e.request(c.id)
	}
}

// ---- building --------------------------------------------------------------------

// buildModel gathers a viewer's evidence, builds the taste model and runs
// every generator.
func (e *Engine) buildModel(ctx context.Context, user core.User, generic bool) (*userModel, error) {
	start := e.now()
	b := &builder{e: e, ctx: ctx, user: user, generic: generic, now: start, touched: map[core.RatingKey]bool{},
		statuses: map[core.RatingKey]core.WatchStatus{}, memo: map[string][]core.Title{}, fits: map[core.RatingKey]fitMemo{},
		hydrateLeft: hydrateBudget}
	fatigue := map[core.RatingKey]float64{}
	var preferenceRevision uint64
	var preferenceSignature string
	if generic {
		b.taste = buildTaste(tasteInput{userID: user.ID, now: start, loc: e.loc})
	} else {
		in, touched, err := e.gatherTaste(ctx, user)
		if err != nil {
			return nil, err
		}
		b.taste = buildTaste(in)
		preferenceRevision = in.preferenceRevision
		preferenceSignature = in.preferenceSignature
		b.touched = touched
		for k, days := range in.shownDays {
			if days >= 2 {
				fatigue[k] = clamp(float64(days-1)/5, 0, 1)
			}
		}
	}
	var rows []*candRow
	for _, g := range generators {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rows = append(rows, e.runGenerator(g, b)...)
	}
	m := &userModel{user: user, generic: generic, taste: b.taste, rows: rows, fatigue: fatigue, builtAt: start, preferenceRevision: preferenceRevision, preferenceSignature: preferenceSignature,
		crowd: b.crowd().features(), buzz: b.buzz().scores()}
	m.took = e.now().Sub(start)
	return m, nil
}

func (e *Engine) runGenerator(g generator, b *builder) (rows []*candRow) {
	defer func() {
		if v := recover(); v != nil {
			e.log.Error("personalize: row generator panicked", "kind", g.kind, "panic", v)
			rows = nil
		}
	}()
	return g.run(b)
}

// buildTaste builds only the taste model (owner views, PersonalScorer).
func (e *Engine) buildTaste(ctx context.Context, user core.User) (*Taste, map[core.RatingKey]int, error) {
	in, _, err := e.gatherTaste(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	return buildTaste(in), in.shownDays, nil
}

// gatherTaste collects a viewer's evidence: history, statuses, plays,
// impressions without plays, Trakt history and overrides, and resolves
// the titles involved.
func (e *Engine) gatherTaste(ctx context.Context, user core.User) (tasteInput, map[core.RatingKey]bool, error) {
	now := e.now()
	in := tasteInput{userID: user.ID, now: now, loc: e.loc, halfLife: e.opts.HalfLife,
		statuses: map[core.RatingKey]core.WatchStatus{}, titles: map[core.RatingKey]core.Title{}}
	if e.opts.Favorites != nil {
		favorites, err := e.opts.Favorites(ctx, &user)
		if err != nil {
			return in, nil, fmt.Errorf("favorites: %w", err)
		}
		in.favorites = favorites
	}
	if e.opts.NotInterestedSnapshot != nil {
		signals, revision, err := e.opts.NotInterestedSnapshot(ctx, &user)
		if err != nil {
			return in, nil, fmt.Errorf("not interested: %w", err)
		}
		in.notInterested, in.preferenceRevision = signals, revision
		_, in.preferenceSignature = snapshotSet(signals, revision)
	} else if e.opts.NotInterested != nil {
		signals, err := e.opts.NotInterested(ctx, &user)
		if err != nil {
			return in, nil, fmt.Errorf("not interested: %w", err)
		}
		in.notInterested = signals
	}
	touched := map[core.RatingKey]bool{}
	var err error
	if e.state != nil {
		if in.history, err = e.state.History(ctx, user.ID, time.Time{}, 1500); err != nil {
			return in, nil, fmt.Errorf("history: %w", err)
		}
		inprog, err := e.state.InProgress(ctx, user.ID, 100)
		if err != nil {
			return in, nil, fmt.Errorf("in progress: %w", err)
		}
		keySet := map[core.RatingKey]bool{}
		for _, h := range in.history {
			keySet[h.Key], keySet[tasteKey(h.Key)] = true, true
		}
		for _, ws := range inprog {
			keySet[ws.Key], keySet[tasteKey(ws.Key)] = true, true
		}
		keys := make([]core.RatingKey, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		if in.statuses, err = e.state.GetMany(ctx, user.ID, keys); err != nil {
			return in, nil, fmt.Errorf("statuses: %w", err)
		}
	}
	// Plays on Trakt from other apps count as completions for titles the
	// local log has never seen.
	if th, ok := e.trakt.(recommend.TraktHistorySource); ok && e.trakt != nil {
		tctx, cancel := context.WithTimeout(ctx, e.opts.SourceTimeout)
		hist, err := th.PullHistory(tctx, user.ID, 300)
		cancel()
		if err != nil {
			e.log.Debug("personalize: trakt history failed", "user", user.ID, "err", err)
		}
		local := map[core.RatingKey]bool{}
		for _, h := range in.history {
			local[tasteKey(h.Key)] = true
		}
		for _, h := range hist {
			if !local[tasteKey(h.Key)] {
				h.Completed = true
				in.history = append(in.history, h)
			}
		}
		sort.SliceStable(in.history, func(i, j int) bool { return in.history[i].ViewedAt.After(in.history[j].ViewedAt) })
	}
	if in.plays, err = e.store.plays(ctx, user.ID, now.AddDate(-1, 0, 0)); err != nil {
		return in, nil, fmt.Errorf("plays: %w", err)
	}
	shown, err := e.store.shownDays(ctx, user.ID, now.AddDate(0, 0, -30))
	if err != nil {
		return in, nil, fmt.Errorf("impressions: %w", err)
	}
	if in.overrides, err = e.store.overrides(ctx, user.ID); err != nil {
		return in, nil, fmt.Errorf("overrides: %w", err)
	}

	for _, h := range in.history {
		touched[tasteKey(h.Key)] = true
	}
	for k, ws := range in.statuses {
		if ws.ViewCount > 0 || ws.ViewOffset > 0 || ws.UserRating > 0 {
			touched[tasteKey(k)] = true
		}
	}
	for _, p := range in.plays {
		touched[p.tasteKey] = true
	}
	in.shownDays = map[core.RatingKey]int{}
	for k, d := range shown {
		if !touched[tasteKey(k)] {
			in.shownDays[k] = d
		}
	}

	// Resolve titles: most recent activity first, then the most-ignored.
	type kt struct {
		k  core.RatingKey
		at time.Time
	}
	latest := map[core.RatingKey]time.Time{}
	for _, h := range in.history {
		if tk := tasteKey(h.Key); h.ViewedAt.After(latest[tk]) {
			latest[tk] = h.ViewedAt
		}
	}
	for k, ws := range in.statuses {
		tk := tasteKey(k)
		if _, ok := latest[tk]; !ok && (ws.ViewCount > 0 || ws.ViewOffset > 0 || ws.UserRating > 0) {
			latest[tk] = ws.LastViewedAt
		}
	}
	for _, p := range in.plays {
		if p.updated.After(latest[p.tasteKey]) {
			latest[p.tasteKey] = p.updated
		}
	}
	for _, favorite := range in.favorites {
		if favorite.Key != 0 {
			key := tasteKey(favorite.Key)
			if _, ok := latest[key]; !ok || favorite.AddedAt.After(latest[key]) {
				latest[key] = favorite.AddedAt
			}
		}
	}
	for _, signal := range in.notInterested {
		if signal.Key != 0 {
			key := tasteKey(signal.Key)
			if _, ok := latest[key]; !ok || signal.AddedAt.After(latest[key]) {
				latest[key] = signal.AddedAt
			}
		}
	}
	order := make([]kt, 0, len(latest))
	for k, at := range latest {
		order = append(order, kt{k, at})
	}
	sort.Slice(order, func(i, j int) bool {
		if !order[i].at.Equal(order[j].at) {
			return order[i].at.After(order[j].at)
		}
		return order[i].k < order[j].k
	})
	if len(order) > 250 {
		order = order[:250]
	}
	ignored := make([]kt, 0, len(in.shownDays))
	for k, d := range in.shownDays {
		if d >= 3 {
			ignored = append(ignored, kt{tasteKey(k), time.Unix(int64(d), 0)})
		}
	}
	sort.Slice(ignored, func(i, j int) bool {
		if !ignored[i].at.Equal(ignored[j].at) {
			return ignored[i].at.After(ignored[j].at)
		}
		return ignored[i].k < ignored[j].k
	})
	if len(ignored) > 60 {
		ignored = ignored[:60]
	}
	requested := append(order, ignored...)
	keys := make([]core.RatingKey, 0, len(requested))
	for _, x := range requested {
		keys = append(keys, x.k)
	}
	cached := mirroredTitles(ctx, e.catalog, keys)
	for _, x := range requested {
		if ctx.Err() != nil {
			return in, nil, ctx.Err()
		}
		if _, ok := in.titles[x.k]; ok {
			continue
		}
		if t, err := historyTitle(ctx, e.catalog, cached, x.k); err == nil && t != nil {
			in.titles[x.k] = *t
		}
	}
	return in, touched, nil
}

// ---- server-wide row statistics ---------------------------------------------------

type statsSnapshot struct {
	at     time.Time
	global map[string]arm           // by prior kind
	users  map[int64]map[string]arm // by user, kind
}

// statsWindow is how far back row engagement counts for the bandit.
const statsWindow = 90 * 24 * time.Hour

// statsFor returns cached row statistics, refreshing them every few
// minutes. They are small aggregate queries over local tables.
func (e *Engine) statsFor(ctx context.Context) *statsSnapshot {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	if e.stats != nil && e.now().Sub(e.stats.at) < 10*time.Minute {
		return e.stats
	}
	s, err := e.computeStats(ctx)
	if err != nil {
		e.log.Debug("personalize: row stats failed", "err", err)
		if e.stats != nil {
			return e.stats
		}
		return &statsSnapshot{at: e.now(), global: map[string]arm{}, users: map[int64]map[string]arm{}}
	}
	e.stats = s
	return s
}

func (e *Engine) invalidateStats() {
	e.statsMu.Lock()
	e.stats = nil
	e.statsMu.Unlock()
}

func (e *Engine) computeStats(ctx context.Context) (*statsSnapshot, error) {
	since := e.now().Add(-statsWindow)
	shows, err := e.store.kindShows(ctx, since)
	if err != nil {
		return nil, err
	}
	plays, err := e.store.plays(ctx, 0, since)
	if err != nil {
		return nil, err
	}
	plays = sessions(plays, e.loc)
	s := &statsSnapshot{at: e.now(), global: map[string]arm{}, users: map[int64]map[string]arm{}}
	user := func(id int64) map[string]arm {
		if s.users[id] == nil {
			s.users[id] = map[string]arm{}
		}
		return s.users[id]
	}
	for uid, kinds := range shows {
		for kind, n := range kinds {
			u := user(uid)
			a := u[kind]
			a.shows += n
			u[kind] = a
			g := s.global[priorKind(kind)]
			g.shows += n
			s.global[priorKind(kind)] = g
		}
	}
	// (surface, kind) arms (surfaces.go): the same evidence split by the
	// screen it was served on.
	surf, err := e.store.surfaceShows(ctx, since)
	if err != nil {
		return nil, err
	}
	for uid, keys := range surf {
		for key, n := range keys {
			surface, kind, _ := strings.Cut(key, "|")
			u := user(uid)
			a := u[key]
			a.shows += n
			u[key] = a
			gk := surfaceKey(surface, priorKind(kind))
			g := s.global[gk]
			g.shows += n
			s.global[gk] = g
		}
	}
	for _, p := range plays {
		if p.kind == "" {
			continue
		}
		u := user(p.userID)
		a := u[p.kind]
		a.reward += p.reward()
		u[p.kind] = a
		g := s.global[priorKind(p.kind)]
		g.reward += p.reward()
		s.global[priorKind(p.kind)] = g
		surface := surfaceOfHub(p.hubID)
		sk := surfaceKey(surface, p.kind)
		a = u[sk]
		a.reward += p.reward()
		u[sk] = a
		gk := surfaceKey(surface, priorKind(p.kind))
		g = s.global[gk]
		g.reward += p.reward()
		s.global[gk] = g
	}
	// A kind cannot earn more reward than it had row-days (several plays
	// from one row on one day are one success for the Beta posterior).
	clampArm := func(a arm) arm {
		if a.reward > a.shows {
			a.shows = a.reward
		}
		return a
	}
	for k, a := range s.global {
		s.global[k] = clampArm(a)
	}
	for _, kinds := range s.users {
		for k, a := range kinds {
			kinds[k] = clampArm(a)
		}
	}
	return s, nil
}

// ---- owner collection rows ---------------------------------------------------------

type ownerEntry struct {
	rows    []core.CollectionRow
	at      time.Time
	loading bool
	done    chan struct{}
}

// ownerRowsTTL is how long owner collection rows are reused per context.
const ownerRowsTTL = 5 * time.Minute

// InvalidateOwnerRows drops cached owner collection rows and assembled-page
// memos so collection creates, edits, deletes and rebuilds take effect on the
// next Home or section request.
func (e *Engine) InvalidateOwnerRows() {
	e.clearOwnerRows()
	e.notify("owner_rows")
}

func (e *Engine) clearOwnerRows() {
	e.mu.Lock()
	for k, v := range e.owner {
		if !v.loading {
			delete(e.owner, k)
		} else {
			v.at = time.Time{}
		}
	}
	e.mu.Unlock()
	e.memoMu.Lock()
	e.memo = map[memoKey]*memoEntry{}
	e.memoMu.Unlock()
}

// ownerRows returns owner collection rows for the viewer and context from a
// per-context cache. Misses fetch in the background; the request waits at
// most OwnerRowsWait for a context it has never fetched.
func (e *Engine) ownerRows(ctx context.Context, user *core.User, hc core.HubContext, section core.MediaType, d core.HomeLayout, wait bool) ([]*candRow, int64) {
	if e.opts.Collections == nil {
		return nil, 0
	}
	raw, _ := json.Marshal(d)
	hash := fnv.New64a()
	_, _ = hash.Write(raw)
	key := strconv.FormatInt(user.ID, 10) + "|" + hc.Bucket() + "|" + hc.Now.Format("2006-01-02-15") + "|" + string(section) + "|" + fmt.Sprint(hash.Sum64(), user.MaxContentRating, user.AllowedSections, user.IsOwner)
	e.mu.Lock()
	ent := e.owner[key]
	now := e.now()
	if ent == nil {
		ent = &ownerEntry{}
		e.owner[key] = ent
	}
	fresh := !ent.at.IsZero() && now.Sub(ent.at) < ownerRowsTTL
	if !fresh && !ent.loading {
		ent.loading = true
		ent.done = make(chan struct{})
		u := *user
		go e.fetchOwnerRows(key, &u, hc, ent, selectedCollections(d, user.ID, sectionName(section), hc))
	}
	done, have := ent.done, !ent.at.IsZero()
	e.mu.Unlock()
	if !have && done != nil {
		delay := e.opts.OwnerRowsWait
		if wait {
			delay = e.opts.SourceTimeout
		}
		t := time.NewTimer(delay)
		select {
		case <-done:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	e.mu.Lock()
	rows, at := ent.rows, ent.at
	e.mu.Unlock()
	var out []*candRow
	for _, cr := range rows {
		if c := e.ownerCandidate(cr, section, user); c != nil {
			out = append(out, c)
		}
	}
	return out, at.UnixNano()
}

func (e *Engine) fetchOwnerRows(key string, user *core.User, hc core.HubContext, ent *ownerEntry, selected []string) {
	defer func() {
		if v := recover(); v != nil {
			e.log.Error("personalize: owner rows panicked", "panic", v)
		}
		e.mu.Lock()
		ent.loading = false
		close(ent.done)
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(e.ctx, e.opts.SourceTimeout)
	defer cancel()
	rows, err := e.opts.Collections.HomeRowsFor(ctx, user, hc)
	if err != nil {
		e.log.Debug("personalize: owner rows failed", "err", err)
		return
	}
	if source, ok := e.opts.Collections.(core.CollectionLayoutSource); ok {
		seen := map[string]bool{}
		for _, row := range rows {
			seen[row.CollectionID] = true
		}
		for _, id := range selected {
			if seen[id] {
				continue
			}
			row, err := source.LayoutRow(ctx, user, hc, id)
			if err != nil {
				e.log.Debug("personalize: selected collection unavailable", "id", id, "err", err)
				continue
			}
			if row != nil {
				rows = append(rows, *row)
				seen[row.CollectionID] = true
			}
		}
	}
	e.mu.Lock()
	ent.rows, ent.at = rows, e.now()
	if len(e.owner) > 5000 {
		for k, v := range e.owner {
			if k != key && !v.loading {
				delete(e.owner, k)
			}
		}
	}
	e.mu.Unlock()
}

// ownerCandidate adapts an owner collection row for this screen.
func (e *Engine) ownerCandidate(cr core.CollectionRow, section core.MediaType, user *core.User) *candRow {
	if section == "" && !cr.HomeRow || section != "" && !cr.SectionRow {
		return nil
	}
	if len(cr.Hub.Items) == 0 {
		return nil
	}
	hub := cr.Hub
	if hub.ID == "" {
		hub.ID = "collection." + cr.CollectionID
	}
	e.mu.Lock()
	var ts *Taste
	if m := e.models[user.ID]; m != nil {
		ts = m.taste
	}
	e.mu.Unlock()
	items := make([]candItem, 0, len(hub.Items))
	for i, t := range hub.Items {
		fit, why := ts.Fit(&t)
		if why == "" {
			why = "In " + hub.Title
		}
		items = append(items, candItem{t: t, base: rankScore(i, len(hub.Items)), fit: fit, qual: quality(&t), reason: why})
	}
	desc := hub.Title
	c := &candRow{kind: KindCollectionPrefix + cr.CollectionID, id: hub.ID, title: hub.Title,
		reason: "Owner collection “" + desc + "”, targeted at this viewer.", context: "collection",
		source: core.RowSourceOwner, items: items, minItems: 3, keepOrder: true, scale: 1, hub: &hub}
	return c
}
