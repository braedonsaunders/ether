package playback

import (
	"context"
	"math"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// Namesake runtime gate. Two catalogue movies can share a normalized title
// and a near year ("Runner" and "The Runner", both 2026); release names then
// cannot tell them apart, so an indexer may serve the other film's file.
// When the catalogue holds a namesake the request is ambiguous, and a probed
// duration that contradicts the catalogue runtime rejects the candidate like
// any other bad release, failing over to the next. Unambiguous titles keep
// the old behaviour, so extended cuts of an only-title are unaffected.

// namesakeRuntimeTolerance is the allowed difference between a probed file
// and the catalogue runtime: the larger of these two bounds.
const (
	namesakeRuntimeSlack      = 8 * time.Minute
	namesakeRuntimeSlackRatio = 0.12
)

// NamesakeCatalog, implemented by the catalogue, reports whether another
// catalogue title namesakes key. The answer is an on-demand indexed lookup;
// playback caches it per title for the life of the manager so a title's
// reselection does not repeat it.
type NamesakeCatalog interface {
	Namesakes(ctx context.Context, key core.RatingKey) (bool, error)
}

// namesakeRequest reports whether t is one of two namesake catalogue titles.
// Plugin kind projections (KindLevel set) are not catalogue titles and are
// never gated.
func (m *Manager) namesakeRequest(ctx context.Context, t *core.Title) bool {
	if t == nil || t.KindLevel != "" || t.Key == 0 {
		return false
	}
	cat, ok := m.deps.Catalog.(NamesakeCatalog)
	if !ok {
		return false
	}
	if ans, ok := m.namesakeLookup(t.Key); ok {
		return ans
	}
	ans, err := cat.Namesakes(ctx, t.Key)
	if err != nil {
		m.log.Debug("namesake lookup failed", "key", t.Key, "err", err)
		return false
	}
	m.rememberNamesake(t.Key, ans)
	return ans
}

// namesakeLookup returns a cached answer for key, if the manager holds one.
func (m *Manager) namesakeLookup(key core.RatingKey) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ans, ok := m.namesake[key]
	return ans, ok
}

// rememberNamesake caches a namesake answer for key.
func (m *Manager) rememberNamesake(key core.RatingKey, ans bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namesake == nil {
		m.namesake = map[core.RatingKey]bool{}
	}
	m.namesake[key] = ans
}

// namesakeTitleConflict reports whether r's reported identifiers name a
// different catalogue title than t. Only explicit identifier conflicts count;
// missing identifiers carry no evidence.
func namesakeTitleConflict(t *core.Title, r core.Release) bool {
	if t == nil || t.IDs == (core.IDs{}) || r.TitleIDs == (core.IDs{}) {
		return false
	}
	_, conflict := releaseparse.CompareTitleIDs(r.TitleIDs, t.IDs)
	return conflict
}

// namesakeDurationMismatch reports whether a probed duration contradicts the
// catalogue runtime by more than the tolerance.
func namesakeDurationMismatch(runtime, probed time.Duration) bool {
	if runtime <= 0 || probed <= 0 {
		return false
	}
	allow := math.Max(namesakeRuntimeSlack.Seconds(), namesakeRuntimeSlackRatio*runtime.Seconds())
	return math.Abs(probed.Seconds()-runtime.Seconds()) > allow
}

// releaseNamesakeRejected gates a candidate for an ambiguous (namesake)
// request: a release whose reported identifiers name the other title is
// rejected before opening. Unambiguous titles are never gated.
func (m *Manager) releaseNamesakeRejected(ctx context.Context, s *session, r core.Release) bool {
	if s.title == nil || !m.namesakeRequest(ctx, s.title) {
		return false
	}
	return namesakeTitleConflict(s.title, r)
}
