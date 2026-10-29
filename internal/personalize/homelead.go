package personalize

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Cross-surface dedupe. A Movies or TV Shows page (with DedupeHome on, the
// default) never repeats what the viewer's Home leads with in the same
// context: the titles in the first rows a person actually sees there, and
// the Home's own seeded rows (the same Because-you-watched seed, the same
// person). It is computed from the memoised Home assembly and the same
// layout pass the front end runs, so it is deterministic and cheap.

// Home lead window: the first leadRows rows of the Home as arranged for
// its page length, and the first leadItems titles of each.
const (
	leadRows  = 6
	leadItems = 10
)

// homeLead is what the Home leads with.
type homeLead struct {
	keys map[core.RatingKey]bool // taste keys (episodes fold onto shows)
	rows map[string]bool         // lead row ids without the screen prefix
	// same holds, for every row visible anywhere on the Home, the titles
	// it shows there: a section's copy of the same row (New & Noteworthy,
	// Trending) never opens with them either.
	same map[string]map[core.RatingKey]bool
	sig  uint64
}

// holds reports whether a title is held back from a section row.
func (l *homeLead) holds(rowID string, k core.RatingKey) bool {
	return l.keys[k] || l.same[rowID][k]
}

// homeLeadFor computes the Home lead for a section request: the Home
// assembly for the same viewer and context (memoised, or the draft layout
// in a preview), arranged for the Home's page length.
func (e *Engine) homeLeadFor(ctx context.Context, user *core.User, hc core.HubContext, layout core.HomeLayout, limit int, wait bool, allow func(context.Context, *core.User, *core.Title) bool) *homeLead {
	hhc := hc
	hhc.SectionID = 0
	p := e.homeWithPolicy(ctx, user, hhc, "", limit, wait, allow)
	rows := arrangeLayout(p.Rows, layout, userID(user), hhc, surfHome, e.opts.MaxHomeRows)
	return leadFromRows(rows, min(limit, leadItems))
}

// leadFromRows collects the lead from arranged Home rows.
func leadFromRows(rows []core.HomeRow, items int) *homeLead {
	l := &homeLead{keys: map[core.RatingKey]bool{}, rows: map[string]bool{}, same: map[string]map[core.RatingKey]bool{}}
	seen := 0
	for _, r := range rows {
		if r.Hidden {
			continue
		}
		if discoveryPriority(r.Kind) >= 5 {
			seen++
		}
		if r.Source == core.RowSourcePlex {
			continue // Continue Watching: in-progress titles never recur as picks
		}
		id := strings.TrimPrefix(r.HubID, "home.")
		same := map[core.RatingKey]bool{}
		for i, t := range r.Items {
			if i >= items {
				break
			}
			same[tasteKey(t.Key)] = true
			if seen <= leadRows {
				l.keys[tasteKey(t.Key)] = true
			}
		}
		l.same[id] = same
		if seen <= leadRows {
			l.rows[id] = true
		}
	}
	h := fnv.New64a()
	keys := make([]int64, 0, len(l.keys))
	for k := range l.keys {
		keys = append(keys, int64(k))
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		_, _ = h.Write([]byte(strconv.FormatInt(k, 36) + ","))
	}
	ids := make([]string, 0, len(l.same))
	for id := range l.same {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		lead := "-"
		if l.rows[id] {
			lead = "+"
		}
		_, _ = h.Write([]byte(lead + id + ":"))
		ks := make([]int64, 0, len(l.same[id]))
		for k := range l.same[id] {
			ks = append(ks, int64(k))
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
		for _, k := range ks {
			_, _ = h.Write([]byte(strconv.FormatInt(k, 36) + ","))
		}
		_, _ = h.Write([]byte(";"))
	}
	l.sig = h.Sum64() | 1 // never 0, which means "no dedupe"
	return l
}

// holdBackHomeLead splits a ranked row into the titles it may lead with
// and the Home lead's titles it holds back. A row the exclusion would
// leave too thin is demoted instead (surfaceMult): it keeps the held
// titles, after all of its own (demoted = true). A row left with almost
// nothing of its own would only repeat the Home and is dropped (unless the
// owner placed it).
func (a *assembly) holdBackHomeLead(c *candRow, items []candItem) (kept, held []candItem, demoted bool) {
	if a.lead == nil || len(a.lead.keys) == 0 && len(a.lead.same[c.id]) == 0 || c.source == core.RowSourcePlex {
		return items, nil, false
	}
	// Each page must offer its best picks and timely discovery even when
	// Home already shows them. Cross-page dedupe must not erase New or
	// Trending from a library. Within-page dedupe and row floors still apply.
	if c.kind == KindTop10 || c.kind == KindTop10Today || c.kind == KindNewReleases || c.kind == KindTrending {
		return items, nil, false
	}
	if a.demoted == nil {
		a.demoted = map[*candRow]bool{}
	}
	if a.held == nil {
		a.held = map[*candRow][]core.RatingKey{}
	}
	kept = make([]candItem, 0, len(items))
	var visible []core.RatingKey
	cap := a.rowCap(c)
	for i, it := range items {
		if a.lead.holds(c.id, tasteKey(it.t.Key)) {
			held = append(held, it)
			if i < cap {
				visible = append(visible, it.t.Key)
			}
			continue
		}
		kept = append(kept, it)
	}
	if len(held) == 0 {
		return items, nil, false
	}
	thin, empty := max(c.minItems, 4), 2
	if c.minItems <= 1 {
		thin, empty = 1, 1 // rows that are fine with one title (new seasons)
	}
	if len(kept) < thin {
		if len(kept) < empty && !a.ownerPlaced(c, a.hubID(c)) {
			// Nothing of its own to lead with: the page would only repeat
			// the Home's row.
			return nil, nil, false
		}
		a.demoted[c] = true
		return kept, held, true
	}
	a.held[c] = visible
	return kept, nil, false
}

// countHeld counts the distinct Home-lead titles kept off rows that made
// it onto the page (core.HomePreview.HomeDeduped).
func (a *assembly) countHeld(out []core.HomeRow) {
	on := map[string]bool{}
	for _, r := range out {
		on[r.HubID] = true
	}
	keys := map[core.RatingKey]bool{}
	for c, ks := range a.held {
		if !on[a.hubID(c)] {
			continue
		}
		for _, k := range ks {
			keys[k] = true
		}
	}
	a.homeDeduped = len(keys)
}
