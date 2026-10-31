package personalize

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// Personalized post-play "hook queue" (roadmap item #30). Instead of
// generic TMDB related order, candidates from the similar, franchise-next
// and because-you-watched pools are ranked per viewer by
//
//	score = tasteFactor × similarityFactor × runtimeFactor
//
// where tasteFactor maps the taste fit (-1..1) onto a positive factor,
// similarity is the feature overlap with the just-watched title, and
// runtimeFit asks whether the candidate ends before the viewer's usual
// stop time tonight. The ranking is deterministic (ties by quality, then
// key) and every pick keeps its strongest reason for the micro-reason
// header (#31). Rankings are cached per user and just-watched title for
// the session so the pick pre-buffered near the end of playback is the
// pick the post-play endpoint serves first.

const (
	postplayTTL      = 30 * time.Minute
	postplayMaxCache = 256
	// rewatchGap mirrors the comfort-rewatches rule: a finished title is
	// only post-play material again after this long unseen.
	rewatchGap = 45 * 24 * time.Hour
)

// PostPlay implements core.PostPlayer.
func (e *Engine) PostPlay(ctx context.Context, user *core.User, justWatched core.RatingKey, hc core.HubContext, limit int) ([]core.PostPlayPick, error) {
	if limit <= 0 {
		limit = 12
	}
	if hc.Now.IsZero() {
		hc.Now = e.now().In(e.loc)
	}
	u := core.User{}
	if user != nil {
		u = *user
	}
	tk := tasteKey(justWatched)
	excluded, exclusionSig, err := e.exclusions(ctx, &u)
	if err != nil {
		return nil, err
	}
	ck := postplayKey{user: u.ID, key: tk, exclusions: exclusionSig}
	hs := e.hookState()
	hs.mu.Lock()
	if ent := hs.postplay[ck]; ent != nil && ent.hour == hc.Now.Hour() && hc.Now.Sub(ent.at) < postplayTTL && len(ent.picks) >= limit {
		picks := append([]core.PostPlayPick(nil), ent.picks[:limit]...)
		hs.mu.Unlock()
		return picks, nil
	}
	hs.mu.Unlock()

	picks, err := e.rankPostPlay(ctx, &u, justWatched, tk, hc, limit, excluded)
	if err != nil {
		return nil, err
	}
	hs.mu.Lock()
	if len(hs.postplay) >= postplayMaxCache {
		for k, ent := range hs.postplay {
			if hc.Now.Sub(ent.at) > postplayTTL {
				delete(hs.postplay, k)
			}
		}
		if len(hs.postplay) >= postplayMaxCache {
			hs.postplay = map[postplayKey]*postplayEntry{}
		}
	}
	hs.postplay[ck] = &postplayEntry{picks: append([]core.PostPlayPick(nil), picks...), at: hc.Now, hour: hc.Now.Hour()}
	hs.mu.Unlock()
	return picks, nil
}

func (e *Engine) rankPostPlay(ctx context.Context, user *core.User, justWatched, tk core.RatingKey, hc core.HubContext, limit int, excluded excludedTitles) ([]core.PostPlayPick, error) {
	src, err := e.catalog.Get(ctx, tk)
	if err != nil {
		return nil, fmt.Errorf("postplay: just-watched title: %w", err)
	}
	m := e.modelFor(ctx, user, false)
	var ts *Taste
	if m != nil {
		ts = m.taste
	}
	now := hc.Now

	// Candidate pools, remembering which pool vouched for each title.
	type cand struct {
		t             core.Title
		franchiseNext bool
		fromSimilar   bool
	}
	pool := map[core.RatingKey]*cand{}
	add := func(items []core.Title, similar bool) {
		for _, t := range items {
			if t.Key == tk || t.Key == justWatched || excluded.contains(&t) {
				continue
			}
			c := pool[t.Key]
			if c == nil {
				c = &cand{t: t}
				pool[t.Key] = c
			}
			c.fromSimilar = c.fromSimilar || similar
		}
	}
	if items, err := e.catalog.Similar(ctx, tk, 40); err == nil {
		add(items, true)
	} else {
		e.log.Debug("postplay: similar failed", "key", tk, "err", err)
	}
	// Franchise: the next unwatched entry of the same TMDB collection.
	var franchiseNext core.RatingKey
	if src.CollectionID != 0 {
		if _, items, err := e.catalog.Collection(ctx, src.CollectionID); err == nil {
			add(items, false)
			seenSelf := false
			for _, t := range items {
				if t.Key == tk {
					seenSelf = true
					continue
				}
				if seenSelf && franchiseNext == 0 && !excluded.contains(&t) && !e.postplayWatched(ctx, user, ts, &t, now) {
					franchiseNext = t.Key
				}
			}
		}
	}
	// Because-you-watched and friends: the model's own fresh candidate
	// rows are a ready-made, already-filtered pool.
	if m != nil {
		for _, r := range m.rows {
			switch r.kind {
			case KindBecause, KindTop10, KindTrendingTaste, KindPerson, KindHiddenGems, KindFranchise, KindCrowd, KindCrowdTrending:
			default:
				continue
			}
			items := make([]core.Title, 0, len(r.items))
			for _, it := range r.items {
				items = append(items, it.t)
			}
			add(items, r.kind == KindBecause)
		}
	}
	if franchiseNext != 0 {
		if c := pool[franchiseNext]; c != nil {
			c.franchiseNext = true
		}
	}

	// Watch statuses in one batch for the watched/rewatch filter.
	statuses := map[core.RatingKey]core.WatchStatus{}
	if e.state != nil && len(pool) > 0 {
		keys := make([]core.RatingKey, 0, len(pool))
		for k := range pool {
			keys = append(keys, k)
		}
		if got, err := e.state.GetMany(ctx, user.ID, keys); err == nil {
			statuses = got
		}
	}
	allowed := e.allowedFor(user)
	budget := ts.stopBudgetMinutes(hc, now)

	picks := make([]core.PostPlayPick, 0, len(pool))
	for _, c := range pool {
		t := &c.t
		if t.Type != core.TypeMovie && t.Type != core.TypeShow {
			continue
		}
		if !t.Released.IsZero() && t.Released.After(now) {
			continue
		}
		if !contentrating.Allowed(t.ContentRating, user.MaxContentRating) {
			continue
		}
		if allowed != nil && !allowed(t.Type) {
			continue
		}
		if e.opts.Allowed != nil && !e.opts.Allowed(user, t) {
			continue
		}
		if watched, rewatch := postplaySeen(ts, statuses[t.Key], t, now); watched && !(rewatch || c.franchiseNext) {
			continue
		}
		fit, why := ts.Fit(t)
		taste := tasteFactor(ts, t, fit)
		sim := titleSimilarity(src, t)
		if c.franchiseNext {
			sim = math.Max(sim, 0.95)
		} else if c.fromSimilar {
			// TMDB's own similar list is weak extra evidence the feature
			// overlap may miss (sparse keywords/people).
			sim = math.Max(sim, 0.3)
		}
		rt := runtimeFit(t, budget, hc)
		score := taste * (0.15 + 0.85*sim) * (0.2 + 0.8*rt)
		if c.franchiseNext {
			// Continuing the story just finished is the post-play
			// equivalent of Up Next: it gets a head start, though taste
			// and a bad runtime fit can still beat it.
			score *= 1.3
		}
		picks = append(picks, core.PostPlayPick{
			Title: *t, Score: round3(score), TasteFit: round3(fit),
			Similarity: round3(sim), RuntimeFit: round3(rt),
			Reason: postplayReason(src, t, c.franchiseNext, why),
		})
	}
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].Score != picks[j].Score {
			return picks[i].Score > picks[j].Score
		}
		if qi, qj := quality(&picks[i].Title), quality(&picks[j].Title); qi != qj {
			return qi > qj
		}
		return picks[i].Title.Key < picks[j].Title.Key
	})
	if len(picks) > limit {
		picks = picks[:limit]
	}
	return picks, nil
}

// tasteFactor maps a taste fit onto a positive multiplicative factor. A
// viewer without a usable taste model falls back to catalogue quality, so
// the queue is still sensible on day one.
func tasteFactor(ts *Taste, t *core.Title, fit float64) float64 {
	if ts == nil || ts.Confidence < minPersonalConfidence {
		return 0.4 + 0.6*quality(t)
	}
	return clamp(0.55+0.45*fit, 0.05, 1)
}

// postplaySeen reports whether the viewer already watched (or started) a
// candidate, and if so whether it is rewatch-worthy: finished long ago and
// either rewatched before or rated 8+.
func postplaySeen(ts *Taste, ws core.WatchStatus, t *core.Title, now time.Time) (watched, rewatch bool) {
	if ev := ts.evidenceFor(t.Key); ev != nil {
		switch ev.Kind {
		case EvidenceFinished:
			return true, (ev.Rewatched || ev.Rating >= 8) && !ev.At.IsZero() && now.Sub(ev.At) >= rewatchGap
		case EvidenceWatching, EvidenceAbandoned, EvidenceRated:
			return true, false
		}
	}
	if ws.ViewCount > 0 {
		return true, ws.UserRating >= 8 && !ws.LastViewedAt.IsZero() && now.Sub(ws.LastViewedAt) >= rewatchGap
	}
	if ws.ViewOffset > 0 {
		return true, false
	}
	return false, false
}

// postplayWatched is the single-title variant used while walking a
// franchise in order (before the batch status lookup exists).
func (e *Engine) postplayWatched(ctx context.Context, user *core.User, ts *Taste, t *core.Title, now time.Time) bool {
	var ws core.WatchStatus
	if e.state != nil {
		ws, _ = e.state.Get(ctx, user.ID, t.Key)
	}
	watched, rewatch := postplaySeen(ts, ws, t, now)
	return watched && !rewatch
}

// titleSimilarity is the feature overlap between two titles in 0..1:
// genres weigh most, then people, keywords, franchise and decade — the
// same features the taste model spreads weight over.
func titleSimilarity(a, b *core.Title) float64 {
	if a == nil || b == nil {
		return 0
	}
	s := 0.0
	s += 0.35 * setJaccard(normGenres(a.Genres), normGenres(b.Genres))
	s += 0.25 * peopleOverlap(a, b)
	s += 0.2 * setJaccard(lowerAll(a.Keywords), lowerAll(b.Keywords))
	if a.CollectionID != 0 && a.CollectionID == b.CollectionID {
		s += 0.15
	}
	if a.Year > 0 && b.Year > 0 {
		switch d := math.Abs(float64(a.Year/10 - b.Year/10)); {
		case d == 0:
			s += 0.05
		case d == 1:
			s += 0.02
		}
	}
	return clamp(s, 0, 1)
}

func setJaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	inter := 0
	seen := map[string]bool{}
	for _, x := range b {
		if set[x] && !seen[x] {
			inter++
		}
		seen[x] = true
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// peopleOverlap is the share of the smaller people set (directors,
// writers, top cast) present in both titles: one shared director of two
// counts for more than Jaccard over long cast lists.
func peopleOverlap(a, b *core.Title) float64 {
	pa, pb := peopleIDs(a), peopleIDs(b)
	if len(pa) == 0 || len(pb) == 0 {
		return 0
	}
	set := map[int64]bool{}
	for _, id := range pa {
		set[id] = true
	}
	inter := 0
	for _, id := range pb {
		if set[id] {
			inter++
		}
	}
	return clamp(float64(inter)/float64(min(len(pa), len(pb))), 0, 1)
}

func peopleIDs(t *core.Title) []int64 {
	var out []int64
	seen := map[int64]bool{}
	add := func(p core.Person) {
		if p.TMDB != 0 && !seen[p.TMDB] {
			seen[p.TMDB] = true
			out = append(out, p.TMDB)
		}
	}
	for _, p := range t.Directors {
		add(p)
	}
	for i, p := range t.Writers {
		if i < 3 {
			add(p)
		}
	}
	for i, p := range t.Cast {
		if i < 5 {
			add(p)
		}
	}
	return out
}

func lowerAll(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, strings.ToLower(x))
	}
	return out
}

// stopBudgetMinutes estimates how many minutes the viewer still has
// tonight: the context target session length, capped by the time until
// their usual stop clock — 23:30 on weeknights, 00:30 on weekends, nudged
// later when their peak starts run late.
func (ts *Taste) stopBudgetMinutes(hc core.HubContext, now time.Time) float64 {
	budget := ts.targetMinutes(hc)
	stopHour, stopMin := 23, 30
	if hc.Weekend {
		stopHour, stopMin = 0, 30
	}
	if ts != nil {
		for _, h := range ts.Habits.PeakHours {
			if h >= 22 { // a night owl: give them an extra hour
				stopHour++
				break
			}
		}
	}
	// A stop past midnight (00:30) is today's when it is still ahead (the
	// small hours) and tomorrow's otherwise (seen from the evening).
	stop := time.Date(now.Year(), now.Month(), now.Day(), stopHour%24, stopMin, 0, 0, now.Location())
	if !stop.After(now) {
		// Tonight's stop is the coming one (a 00:30 stop seen from the
		// evening before, or already past in the small hours — then the
		// context budget above governs anyway).
		stop = stop.AddDate(0, 0, 1)
	}
	if until := stop.Sub(now).Minutes(); until > 0 && until < budget {
		budget = until
	}
	return math.Max(budget, 15)
}

// runtimeFit scores a candidate's runtime (episode runtime for shows)
// against the remaining budget: 1 when it ends in time, decaying past it —
// steeply late at night, when a 45-minute episode beats a 2-hour film.
func runtimeFit(t *core.Title, budget float64, hc core.HubContext) float64 {
	m := minutes(t)
	if m <= 0 || budget <= 0 {
		return 0.7
	}
	if m <= budget {
		return 1
	}
	k := 1.0
	if hc.Daypart == core.DaypartLate {
		k = 1.6
	}
	over := (m - budget) / budget
	return clamp(math.Exp(-k*over), 0.05, 1)
}

// postplayReason keeps the strongest human reason for a pick.
func postplayReason(src, t *core.Title, franchiseNext bool, tasteWhy string) string {
	switch {
	case franchiseNext && t.CollectionName != "":
		return "Next in " + t.CollectionName
	case tasteWhy != "":
		return tasteWhy
	case src != nil && src.Title != "":
		return "Because you watched " + src.Title
	}
	return ""
}

var _ core.PostPlayer = (*Engine)(nil)
