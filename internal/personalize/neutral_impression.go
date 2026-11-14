package personalize

import "github.com/braedonsaunders/ether/internal/core"

// RecordNeutralImpression records a validated server-issued row snapshot using
// the existing impression and ranker paths.
func (e *Engine) RecordNeutralImpression(user *core.User, imp core.NeutralImpression) {
	s := imp.Situation
	hc := core.HubContext{Device: s.Device, Daypart: s.Daypart, Weekend: s.Weekend, Now: s.Now, SectionID: s.LibraryID, Product: s.Product, Platform: s.Platform, SessionID: s.SessionID, ClientID: s.ClientID}
	h := core.Hub{ID: imp.RowID, Title: imp.Title}
	for _, key := range imp.Items {
		h.Items = append(h.Items, core.Title{Key: key})
	}
	if user == nil || imp.RowID == "" || imp.Kind == "" || imp.Rank < 0 {
		return
	}
	at := hc.Now
	if at.IsZero() {
		at = e.now()
	}
	entry := impression{userID: user.ID, at: at, hc: hc, rows: []servedRow{{hubID: imp.RowID, kind: imp.Kind, title: imp.Title, position: imp.Rank, keys: append([]core.RatingKey(nil), imp.Items...)}}}
	e.mu.Lock()
	e.lastCtx[user.ID] = hc
	if _, ok := e.users[user.ID]; !ok {
		e.users[user.ID] = *user
	}
	e.mu.Unlock()
	e.rec.record(entry)
	e.rankerRecordServeNeutral(user, hc, []core.Hub{h}, at, map[string]string{imp.RowID: imp.Kind}, imp.Rank)
}

var _ core.NeutralImpressionSink = (*Engine)(nil)

// SemanticRowKind returns the registered producer kind for a ranked row. The
// fallback matches the existing impression classification for external rows.
func (e *Engine) SemanticRowKind(id string) string { return e.kindOfHub(id) }

// RecordNeutralEntryImpression records only the card actually reported visible.
// It neither counts another shelf serve nor marks its offscreen peers shown.
func (e *Engine) RecordNeutralEntryImpression(user *core.User, imp core.NeutralImpression, entry core.NeutralEntryRef) {
	if user == nil || imp.RowID == "" || imp.Kind == "" || imp.Rank < 0 || entry.Kind != "title" {
		return
	}
	index := -1
	for n, ref := range imp.Entries {
		if ref == entry {
			index = n
			break
		}
	}
	if index < 0 {
		return
	}
	key, err := core.ParseRatingKey(entry.ID)
	if err != nil {
		return
	}
	member := false
	for _, item := range imp.Items {
		if item == key {
			member = true
			break
		}
	}
	if !member {
		return
	}
	s := imp.Situation
	hc := core.HubContext{Device: s.Device, Daypart: s.Daypart, Weekend: s.Weekend, Now: s.Now, SectionID: s.LibraryID, Product: s.Product, Platform: s.Platform, SessionID: s.SessionID, ClientID: s.ClientID}
	at := hc.Now
	if at.IsZero() {
		at = e.now()
	}
	e.mu.Lock()
	e.lastCtx[user.ID] = hc
	if _, ok := e.users[user.ID]; !ok {
		e.users[user.ID] = *user
	}
	e.mu.Unlock()
	e.rec.record(impression{userID: user.ID, at: at, hc: hc, rows: []servedRow{{hubID: imp.RowID, kind: imp.Kind, title: imp.Title, position: imp.Rank, keys: []core.RatingKey{key}, itemsOnly: true, itemOffset: index}}})
}
