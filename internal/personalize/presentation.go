package personalize

import (
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Keep a browsing page's positions through model, feedback and watch-state
// refreshes. This stores identities only: every retained item must still pass
// the current candidate generation, policy, context and cross-surface filters.
// A new context/layout or five minutes without browsing starts a fresh draft.
const presentationTTL = 5 * time.Minute
const maxPresentations = 256

type presentedRow struct {
	id   string
	keys []core.RatingKey
}
type presentation struct {
	seed int64
	rows []presentedRow
	at   time.Time
}

func presentationKey(k memoKey, generic bool) memoKey {
	k.model, k.stats, k.intent, k.lead = 0, 0, 0, 0
	k.cw = ""
	k.owner = 0 // owner collections are rebuilt live; their refresh is not a new browsing session
	// Android and Android TV poll /hubs with a new count and often a new
	// session id. Those are the same browsing visit: one bandit sample per
	// viewer, library surface and device class until the window expires.
	k.session = ""
	k.client = ""
	k.limit = 0
	k.hour = 0
	// The first personal model should replace cold-start picks.
	if generic {
		k.model = 1
	}
	return k
}

func (e *Engine) assemblePage(a *assembly, key memoKey, draft bool) []core.HomeRow {
	if draft {
		return a.run()
	}
	e.presentationMu.Lock()
	defer e.presentationMu.Unlock()
	now := e.now()
	if e.presentations == nil {
		e.presentations = map[memoKey]presentation{}
	}
	origKey := key
	key = presentationKey(origKey, a.model.generic)
	a.visit = now.UnixNano()
	if old, ok := e.presentations[key]; ok && !now.Before(old.at) && now.Sub(old.at) < presentationTTL {
		a.previous = old.rows
		a.visit = old.seed
	} else if !a.model.generic && a.newViewer() {
		if coldOld, ok := e.presentations[presentationKey(origKey, true)]; ok && !now.Before(coldOld.at) && now.Sub(coldOld.at) < presentationTTL {
			a.previous = coldOld.rows
			a.visit = coldOld.seed
		}
	}
	rows := a.run()
	p := presentation{at: now, seed: a.visit}
	for _, row := range rows {
		if row.Hidden {
			continue
		}
		r := presentedRow{id: row.HubID}
		for _, t := range row.Items {
			r.keys = append(r.keys, t.Key)
		}
		p.rows = append(p.rows, r)
	}
	if len(e.presentations) >= maxPresentations {
		var oldest memoKey
		var at time.Time
		for k, v := range e.presentations {
			if at.IsZero() || v.at.Before(at) {
				oldest, at = k, v.at
			}
		}
		delete(e.presentations, oldest)
	}
	e.presentations[key] = p
	return rows
}

func (e *Engine) clearPresentation(userID int64) {
	e.presentationMu.Lock()
	for k := range e.presentations {
		if k.user == userID {
			delete(e.presentations, k)
		}
	}
	e.presentationMu.Unlock()
	e.memoMu.Lock()
	for k := range e.memo {
		if k.user == userID {
			delete(e.memo, k)
		}
	}
	e.memoMu.Unlock()
}

func (a *assembly) retainRowOrder(rows []*rankedRow) {
	order := map[string]int{}
	for i, r := range a.previous {
		order[r.id] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		pi, oi := order[rows[i].hubID]
		pj, oj := order[rows[j].hubID]
		if oi != oj {
			return oi
		}
		return oi && pi < pj
	})
}

func (a *assembly) previousCount(hubID string) int {
	for _, r := range a.previous {
		if r.id == hubID {
			return len(r.keys)
		}
	}
	return 0
}

// Reserve the surviving cards in their old row before any other row can
// draft them. Newly eligible cards fill holes at the tail. This prevents a
// changed Top 10 score from stealing cards out of every shelf below it.
func (a *assembly) retainItems(sel []*rankedRow, picked map[*rankedRow][]candItem, claimed map[core.RatingKey]bool, inPhase func(*rankedRow) bool) {
	previous := map[string][]core.RatingKey{}
	for _, r := range a.previous {
		previous[r.id] = r.keys
	}
	for _, r := range sel {
		if r.c.source != core.RowSourceEngine || r.c.keepOrder || !inPhase(r) {
			continue
		}
		eligible := map[core.RatingKey]candItem{}
		for _, it := range r.items {
			eligible[it.t.Key] = it
		}
		for _, k := range previous[r.hubID] {
			if len(picked[r]) >= a.rowCap(r.c) {
				break
			}
			if it, ok := eligible[k]; ok && !claimed[k] {
				picked[r] = append(picked[r], it)
				claimed[k] = true
			}
		}
	}
}

// A score change must not remove a still-eligible card merely because it
// fell outside MMR's bounded ranking window. Keep it available for reservation.
func (a *assembly) keepPreviouslyShown(c *candRow, eligible, ranked []candItem) []candItem {
	if c.source != core.RowSourceEngine {
		return ranked
	}
	wanted := map[core.RatingKey]bool{}
	for _, r := range a.previous {
		if r.id == a.hubID(c) {
			for _, k := range r.keys {
				wanted[k] = true
			}
			break
		}
	}
	for _, it := range ranked {
		delete(wanted, it.t.Key)
	}
	for _, it := range eligible {
		if wanted[it.t.Key] {
			ranked = append(ranked, it)
			delete(wanted, it.t.Key)
		}
	}
	return ranked
}

// Memo hits count as browsing activity, so a feedback refresh never starts a
// new visit merely because assembly was avoided by the response cache.
func (e *Engine) touchPresentation(k memoKey, generic bool) {
	e.presentationMu.Lock()
	defer e.presentationMu.Unlock()
	k = presentationKey(k, generic)
	if p, ok := e.presentations[k]; ok {
		p.at = e.now()
		e.presentations[k] = p
	}
}
