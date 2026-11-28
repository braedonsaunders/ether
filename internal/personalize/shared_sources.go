package personalize

import (
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// Model builds draw most of their candidates from plain catalogue browses
// (popular in a genre, acclaimed, new), which are the same list for every
// viewer who browses under the owner's catalogue rules with no rating cap.
// The catalogue's own page cache lasts a minute at most and any catalogue
// write ends it, so without this every new viewer's first model re-ran the
// whole showcase (15 genres x 2 types, 125k rows on the lab catalogue).
//
// Sharing is limited to such viewers and keyed by the viewer-policy epoch:
// every write that could make a cached title disallowed for anyone moves
// the epoch, so a shared list can never carry a stale allow. A viewer with
// a rating cap or catalogue rules of their own always browses for
// themselves.
const (
	sharedBrowseTTL = 15 * time.Minute
	sharedBrowseMax = 256
)

type sharedBrowses struct {
	mu       sync.Mutex
	entries  map[string]sharedBrowse
	showcase *sharedShowcase
}

type sharedBrowse struct {
	items []core.Title
	epoch int64
	at    time.Time
}

type sharedShowcase struct {
	windows map[string][]core.Title
	epoch   int64
	at      time.Time
}

func (b *builder) sharedViewerUnrestricted() bool {
	if b.user.MaxContentRating != "" {
		return false
	}
	if f := b.e.opts.CatalogRulesRevision; f != nil && f(&b.user) != 0 {
		return false
	}
	return true
}

// sharedBrowseKey reports the shared cache key for q, or "" when this
// builder's viewer must browse alone.
func (b *builder) sharedBrowseKey(q core.BrowseQuery) string {
	if !b.sharedViewerUnrestricted() || q.Filter.MaxContentRating != "" || q.Filter.Group != nil {
		return ""
	}
	return fmt.Sprintf("%+v", q)
}

func (e *Engine) sharedShowcaseGet(epoch int64, now time.Time) (map[string][]core.Title, bool) {
	e.shared.mu.Lock()
	defer e.shared.mu.Unlock()
	ent := e.shared.showcase
	if ent == nil || ent.epoch != epoch || now.Sub(ent.at) >= sharedBrowseTTL || now.Before(ent.at) {
		return nil, false
	}
	out := make(map[string][]core.Title, len(ent.windows))
	for k, items := range ent.windows {
		out[k] = append([]core.Title(nil), items...)
	}
	return out, true
}

func (e *Engine) sharedShowcasePut(epoch int64, now time.Time, windows map[string][]core.Title) {
	cp := make(map[string][]core.Title, len(windows))
	for k, items := range windows {
		cp[k] = append([]core.Title(nil), items...)
	}
	e.shared.mu.Lock()
	defer e.shared.mu.Unlock()
	e.shared.showcase = &sharedShowcase{windows: cp, epoch: epoch, at: now}
}

// policyEpoch is read once per build: one indexed point read.
func (b *builder) policyEpoch() (int64, bool) {
	if !b.epochRead {
		b.epochRead = true
		epoch, err := viewerpolicy.Read(b.ctx, b.e.opts.DB)
		b.epoch, b.epochOK = epoch, err == nil
	}
	return b.epoch, b.epochOK
}

func (e *Engine) sharedBrowseGet(key string, epoch int64, now time.Time) ([]core.Title, bool) {
	e.shared.mu.Lock()
	defer e.shared.mu.Unlock()
	ent, ok := e.shared.entries[key]
	if !ok || ent.epoch != epoch || now.Sub(ent.at) >= sharedBrowseTTL || now.Before(ent.at) {
		return nil, false
	}
	return append([]core.Title(nil), ent.items...), true
}

func (e *Engine) sharedBrowsePut(key string, epoch int64, now time.Time, items []core.Title) {
	e.shared.mu.Lock()
	defer e.shared.mu.Unlock()
	if e.shared.entries == nil || len(e.shared.entries) >= sharedBrowseMax {
		e.shared.entries = map[string]sharedBrowse{}
	}
	e.shared.entries[key] = sharedBrowse{items: append([]core.Title(nil), items...), epoch: epoch, at: now}
}
