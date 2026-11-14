package cachefill

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// WatchReader deliberately cannot write history. Preloading is never a view.
type WatchReader interface {
	History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error)
	InProgress(context.Context, int64, int) ([]core.WatchStatus, error)
}

type Selection struct {
	Key         core.RatingKey `json:"key"`
	Title       string         `json:"title"`
	Type        core.MediaType `json:"type"`
	Score       float64        `json:"score"`
	Catalogue   float64        `json:"catalogue"`
	Household   float64        `json:"household"`
	Reason      string         `json:"reason"`
	ReasonCodes []string       `json:"reasonCodes,omitempty"`
	ReasonArgs  map[string]int `json:"reasonArgs,omitempty"`
	State       string         `json:"state,omitempty"` // cached | queued | failed
}
type candidate struct {
	Title core.Title
	// priority is independent of popularity and owner blend weights.
	priority int // 3: active continuation, 2: owner pin, 1: watchlist, 0: speculation
	intent   *WarmIntent
	Selection
}
type cursor struct {
	key    core.RatingKey
	at     time.Time
	resume bool
}
type signals struct {
	frequency map[core.RatingKey]float64
	viewers   map[core.RatingKey]int
	cursors   map[core.RatingKey]map[int64]cursor
	resumes   map[core.RatingKey]bool
	genres    map[string]float64
	count     int
}

func contentAllowed(c config.Cache, k core.RatingKey) bool {
	return k.Kind() == core.KindMovie && c.PreloadContent != "tv" || k.Kind() == core.KindEpisode && c.PreloadContent != "movies"
}
func rootKey(k core.RatingKey) core.RatingKey {
	if k.Kind() == core.KindEpisode {
		return core.ShowKey(k.TMDB())
	}
	return k
}
func displayTitle(t core.Title) string {
	if t.Type == core.TypeEpisode {
		return fmt.Sprintf("%s · S%02dE%02d · %s", t.ShowTitle, t.Season, t.Episode, t.Title)
	}
	return t.Title
}
func (m *Manager) learn(ctx context.Context, cfg config.Cache) (signals, error) {
	s := signals{frequency: map[core.RatingKey]float64{}, viewers: map[core.RatingKey]int{}, cursors: map[core.RatingKey]map[int64]cursor{}, resumes: map[core.RatingKey]bool{}, genres: map[string]float64{}}
	if m.deps.State == nil || m.deps.Household == nil {
		return s, nil
	}
	users, err := m.deps.Household(ctx, cfg.PreloadHouseholdScope)
	if err != nil {
		return s, fmt.Errorf("load cache household: %w", err)
	}
	// Deterministic bounded work even on a heavily shared server.
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	if len(users) > 64 {
		users = users[:64]
	}
	now := m.deps.Now()
	days := cfg.PreloadHistoryDays
	if days <= 0 {
		days = 90
	}
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	halfLife := min(30.0, max(1.0, float64(days)/3))
	seenUsers := map[int64]bool{}
	for _, u := range users {
		if seenUsers[u.ID] {
			continue
		}
		seenUsers[u.ID] = true
		history, err := m.deps.State.History(ctx, u.ID, since, 2000)
		if err != nil {
			return s, fmt.Errorf("read household history: %w", err)
		}
		progress, err := m.deps.State.InProgress(ctx, u.ID, 200)
		if err != nil {
			return s, fmt.Errorf("read household progress: %w", err)
		}
		activeSince := now.Add(-7 * 24 * time.Hour)
		active := false
		for _, h := range history {
			if !h.ViewedAt.Before(activeSince) && !h.ViewedAt.After(now) {
				active = true
				break
			}
		}
		for _, p := range progress {
			at := p.LastViewedAt
			if at.IsZero() {
				at = p.UpdatedAt
			}
			if !at.Before(activeSince) && !at.After(now) {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		s.count++
		own := map[core.RatingKey]float64{}
		daily := map[string]int{}
		setCursor := func(k core.RatingKey, at time.Time, resume bool) {
			if k.Kind() != core.KindEpisode || at.Before(activeSince) || at.After(now) {
				return
			}
			root := rootKey(k)
			if s.cursors[root] == nil {
				s.cursors[root] = map[int64]cursor{}
			}
			prev := s.cursors[root][u.ID]
			if at.After(prev.at) || at.Equal(prev.at) && (resume || k > prev.key) {
				s.cursors[root][u.ID] = cursor{k, at, resume}
			}
		}
		// History contains both start and completion events. Only completions
		// contribute frequency; a start must not double-count the same viewing.
		for _, h := range history {
			if h.ViewedAt.Before(since) || h.ViewedAt.After(now) {
				continue
			}
			if h.Key.Kind() != core.KindMovie && h.Key.Kind() != core.KindEpisode {
				continue
			}
			if !h.Completed {
				continue
			}
			setCursor(h.Key, h.ViewedAt, false)
			k := rootKey(h.Key)
			bucket := fmt.Sprintf("%d/%s", k, h.ViewedAt.UTC().Format("2006-01-02"))
			daily[bucket]++
			// A bulk season scrobble or long binge cannot overwhelm everyone else.
			if daily[bucket] > 4 {
				continue
			}
			own[k] += math.Exp2(-now.Sub(h.ViewedAt).Hours() / 24 / halfLife)
		}
		for _, p := range progress {
			at := p.LastViewedAt
			if at.IsZero() {
				at = p.UpdatedAt
			}
			if at.Before(activeSince) || at.After(now) {
				continue
			}
			if p.Key.Kind() != core.KindMovie && p.Key.Kind() != core.KindEpisode {
				continue
			}
			setCursor(p.Key, at, true)
			s.resumes[p.Key] = true
			own[rootKey(p.Key)] += .5 * math.Exp2(-now.Sub(at).Hours()/24/halfLife)
		}
		for k, n := range own {
			// Saturate each viewer separately; one prolific viewer never gets
			// thousands of times the influence of another household member.
			s.frequency[k] += math.Log1p(min(8, n)) / math.Log(9)
			s.viewers[k]++
		}
	}
	for k, f := range s.frequency {
		t, err := m.deps.Catalog.CachedTitle(ctx, k)
		if err != nil || t == nil {
			continue
		}
		for _, g := range t.Genres {
			s.genres[g] += f / float64(max(1, len(t.Genres)))
		}
	}
	return s, nil
}
func (s signals) score(t core.Title) float64 {
	direct := .7*(1-math.Exp(-s.frequency[t.Key])) + .3*float64(s.viewers[t.Key])/float64(max(1, s.count))
	// Genre affinity is deliberately weaker than a direct viewing signal;
	// it lets unseen titles compete without pretending they were watched.
	maxGenre := 0.0
	for _, v := range s.genres {
		maxGenre = max(maxGenre, v)
	}
	affinity := 0.0
	for _, g := range t.Genres {
		if maxGenre > 0 {
			affinity += s.genres[g] / maxGenre
		}
	}
	affinity = .35 * affinity / float64(max(1, len(t.Genres)))
	return min(1, max(direct, affinity))
}

type selectionPlan struct {
	Candidates       []candidate
	Shows            map[core.RatingKey]bool
	UnavailableShows map[core.RatingKey]bool
}

func (m *Manager) selectCandidates(ctx context.Context, cfg config.Cache, maxTitles int) (selectionPlan, error) {
	s, err := m.learn(ctx, cfg)
	if err != nil {
		return selectionPlan{}, err
	}
	now := m.deps.Now()
	order := core.SortPopularity
	if cfg.PreloadSource == "recent" {
		order = core.SortReleased
	}
	types := []core.MediaType{core.TypeMovie, core.TypeShow}
	if cfg.PreloadContent == "movies" {
		types = []core.MediaType{core.TypeMovie}
	}
	if cfg.PreloadContent == "tv" {
		types = []core.MediaType{core.TypeShow}
	}
	roots := map[core.RatingKey]core.Title{}
	catalogueRanks := map[core.RatingKey]int{}
	pool := min(15000, max(60, maxTitles*3))
	if cfg.SpeculativeWarming != "off" && s.count > 0 && cfg.PreloadEnabled {
		for _, typ := range types {
			for offset := 0; offset < pool; offset += 50 {
				if err := ctx.Err(); err != nil {
					return selectionPlan{}, err
				}
				page, err := m.deps.Catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: typ}, Sort: order, Desc: true, Offset: offset, Limit: min(50, pool-offset)})
				if err != nil {
					return selectionPlan{}, fmt.Errorf("browse preload catalogue: %w", err)
				}
				for i, t := range page.Items {
					if t.Type == typ && !t.Adult && t.IsReleased(now) {
						roots[t.Key] = t
						catalogueRanks[t.Key] = offset + i
					}
				}
				if len(page.Items) == 0 || offset+len(page.Items) >= page.Total {
					break
				}
			}
		}
	}
	// Household favourites need not appear in the top catalogue page.
	for k := range s.frequency {
		if cfg.SpeculativeWarming == "off" && len(s.cursors[k]) == 0 && !s.resumes[k] {
			continue
		}
		if _, ok := roots[k]; ok {
			continue
		}
		t, err := m.deps.Catalog.CachedTitle(ctx, k)
		if err != nil || t == nil || t.Adult || !t.IsReleased(now) {
			continue
		}
		if t.Type == core.TypeMovie && cfg.PreloadContent != "tv" || t.Type == core.TypeShow && cfg.PreloadContent != "movies" {
			roots[k] = *t
		}
	}
	pinned := map[core.RatingKey]bool{}
	for _, k := range cfg.KeepWarmTitles {
		pinned[k] = true
		if t, err := m.deps.Catalog.CachedTitle(ctx, k); err == nil && t != nil && !t.Adult && t.IsReleased(now) {
			roots[k] = *t
		}
	}
	peak := map[core.MediaType]float64{}
	for _, t := range roots {
		peak[t.Type] = max(peak[t.Type], t.Popularity)
	}
	weight := .65
	if cfg.PreloadHouseholdWeight != nil {
		weight = float64(*cfg.PreloadHouseholdWeight) / 100
	}
	if cfg.PreloadSource == "popular" || cfg.PreloadSource == "recent" || len(s.frequency) == 0 {
		weight = 0
	}
	var ranked []candidate
	for _, t := range roots {
		cat := 0.0
		if peak[t.Type] > 0 && cfg.PreloadSource != "recent" {
			cat = math.Log1p(max(0, t.Popularity)) / math.Log1p(peak[t.Type])
		} else if rank, ok := catalogueRanks[t.Key]; ok {
			cat = 1 / (1 + float64(rank)/20)
		}
		household := s.score(t)
		reason := "Catalogue popularity"
		codes := []string{"catalogue-popularity"}
		args := map[string]int{}
		if cfg.PreloadSource == "recent" {
			reason = "Recent release"
			codes = []string{"recent-release"}
		}
		if weight > 0 && household > 0 {
			if s.viewers[t.Key] > 0 {
				reason = fmt.Sprintf("Recent viewing from %d household viewer(s)", s.viewers[t.Key])
				codes = []string{"household-viewing"}
				args["viewers"] = s.viewers[t.Key]
			} else {
				reason = "Matches household genre interests"
				codes = []string{"household-genres"}
			}
		}
		priority := 0
		if pinned[t.Key] {
			priority = 2
			reason = "Owner pin"
			codes = []string{"owner-pin"}
		}
		if len(s.cursors[t.Key]) > 0 || s.resumes[t.Key] {
			priority = 3
		}
		if priority == 0 && (!cfg.PreloadEnabled || cfg.SpeculativeWarming == "off" || s.count == 0) {
			continue
		}
		ranked = append(ranked, candidate{Title: t, priority: priority, Selection: Selection{Key: t.Key, Title: displayTitle(t), Type: t.Type, Score: 100 * ((1-weight)*cat + weight*household), Catalogue: 100 * cat, Household: 100 * household, Reason: reason, ReasonCodes: codes, ReasonArgs: args}})
	}
	sortCandidates(ranked)
	var out []candidate
	plannedShows := map[core.RatingKey]bool{}
	unavailableShows := map[core.RatingKey]bool{}
	// Bounded metadata expansion: never enumerate every season of every show.
	showLimit := min(500, max(20, maxTitles*2))
	shows := 0
	for _, c := range ranked {
		if c.Title.Type == core.TypeMovie {
			out = append(out, c)
			continue
		}
		if shows >= showLimit {
			continue
		}
		shows++
		eps, err := m.episodeWindow(ctx, c.Title, s.cursors[c.Title.Key], cfg)
		if err != nil {
			if ctx.Err() != nil {
				return selectionPlan{}, ctx.Err()
			}
			unavailableShows[c.Title.Key] = true
			m.deps.Logger.Debug("skip preload episode metadata", "key", c.Key, "err", err)
			continue
		}
		plannedShows[c.Title.Key] = true
		for i, t := range eps {
			ec := c
			ec.Title = t
			ec.Selection.Key = t.Key
			ec.Selection.Title = displayTitle(t)
			ec.Selection.Type = core.TypeEpisode
			ec.Score *= math.Pow(.92, float64(i))
			ec.ReasonCodes = append([]string(nil), c.ReasonCodes...)
			ec.ReasonArgs = maps.Clone(c.ReasonArgs)
			if len(s.cursors[c.Title.Key]) > 0 {
				ec.Reason += " · next in a household viewing sequence"
				ec.ReasonCodes = append(ec.ReasonCodes, "next-in-sequence")
			} else {
				ec.Reason += " · series opening"
				ec.ReasonCodes = append(ec.ReasonCodes, "series-opening")
			}
			if s.resumes[t.Key] {
				ec.Reason = "Continue an in-progress episode · " + c.Reason
				ec.ReasonCodes = append(ec.ReasonCodes, "continue-in-progress")
			}
			out = append(out, ec)
		}
	}
	if m.deps.WatchlistIntents != nil {
		intents, err := m.deps.WatchlistIntents(ctx)
		if err != nil {
			return selectionPlan{}, err
		}
		watchlist, err := m.watchlistCandidates(ctx, cfg, intents)
		if err != nil {
			return selectionPlan{}, err
		}
		out = append(out, watchlist...)
	}
	sortCandidates(out)
	// Status is an owner-only explanation of the next selection, not a second
	// manifest and not individual users' history. No names or account IDs leave.
	preview := make([]Selection, 0, min(12, len(out)))
	for _, c := range out[:min(12, len(out))] {
		preview = append(preview, c.Selection)
	}
	m.mu.Lock()
	m.status.Selection = preview
	m.status.Viewers = s.count
	m.mu.Unlock()
	return selectionPlan{Candidates: out, Shows: plannedShows, UnavailableShows: unavailableShows}, nil
}
func sortCandidates(v []candidate) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].priority != v[j].priority {
			return v[i].priority > v[j].priority
		}
		if v[i].intent != nil && v[j].intent != nil && !v[i].intent.Added.Equal(v[j].intent.Added) {
			return v[i].intent.Added.After(v[j].intent.Added)
		}
		if v[i].Score != v[j].Score {
			return v[i].Score > v[j].Score
		}
		return v[i].Selection.Key < v[j].Selection.Key
	})
}

// episodeWindow merges each viewer's continuation fairly, with a shared
// per-show ceiling. A new show starts at its first regular aired episode.
// Fetching season metadata is bounded, and never resolves/downloads media.
func (m *Manager) episodeWindow(ctx context.Context, show core.Title, positions map[int64]cursor, cfg config.Cache) ([]core.Title, error) {
	limit := cfg.PreloadEpisodesAhead
	if limit <= 0 {
		limit = 3
	}
	seasons, err := m.deps.Catalog.Children(ctx, show.Key)
	if err != nil {
		return nil, err
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].Season < seasons[j].Season })
	now := m.deps.Now()
	var cursors []cursor
	for _, p := range positions {
		cursors = append(cursors, p)
	}
	sort.Slice(cursors, func(i, j int) bool {
		if !cursors[i].at.Equal(cursors[j].at) {
			return cursors[i].at.After(cursors[j].at)
		}
		return cursors[i].key < cursors[j].key
	})
	if len(cursors) == 0 {
		cursors = []cursor{{}}
	}
	cache := map[core.RatingKey][]core.Title{}
	fetches := 0
	queues := make([][]core.Title, len(cursors))
	for i, p := range cursors {
		for _, season := range seasons {
			if season.Season <= 0 || season.Season < p.key.Season() || !season.IsReleased(now) {
				continue
			}
			eps, ok := cache[season.Key]
			if !ok {
				if fetches >= 12 {
					break
				}
				fetches++
				eps, err = m.deps.Catalog.Children(ctx, season.Key)
				if err != nil {
					return nil, err
				}
				sort.Slice(eps, func(i, j int) bool { return eps[i].Episode < eps[j].Episode })
				cache[season.Key] = eps
			}
			for _, ep := range eps {
				if ep.Type != core.TypeEpisode || ep.Adult || !ep.IsReleased(now) || ep.Season <= 0 || ep.Episode <= 0 {
					continue
				}
				if p.key != 0 && (ep.Season < p.key.Season() || ep.Season == p.key.Season() && (ep.Episode < p.key.Episode() || ep.Episode == p.key.Episode() && !p.resume)) {
					continue
				}
				if ep.ShowKey != 0 && ep.ShowKey != show.Key {
					continue
				}
				ep.ShowKey = show.Key
				ep.ShowTitle = show.Title
				queues[i] = append(queues[i], ep)
				if len(queues[i]) >= limit {
					break
				}
			}
			if len(queues[i]) >= limit {
				break
			}
		}
	}
	out := make([]core.Title, 0, limit)
	seen := map[core.RatingKey]bool{}
	for n := 0; n < limit && len(out) < limit; n++ {
		for _, q := range queues {
			if n < len(q) && !seen[q[n].Key] {
				out = append(out, q[n])
				seen[q[n].Key] = true
			}
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
