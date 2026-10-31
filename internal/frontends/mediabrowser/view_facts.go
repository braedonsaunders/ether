package mediabrowser

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// viewFact is one library's listable total and its earliest and latest
// date-added values. gen is the catalogue generation that produced it.
type viewFact struct {
	gen              uint64
	count            int
	counted          bool
	earliest, latest time.Time
	dated            bool
}

type viewFactKey struct {
	viewer   int64
	view     string
	rating   string
	sections string
}

type viewFactCache struct {
	mu   sync.Mutex
	m    map[viewFactKey]viewFact
	busy map[viewFactKey]bool
}

func (c *viewFactCache) get(key viewFactKey) (viewFact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fact, ok := c.m[key]
	return fact, ok && fact.counted
}

func (c *viewFactCache) start(key viewFactKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[key] {
		return false
	}
	if c.busy == nil {
		c.busy = map[viewFactKey]bool{}
	}
	c.busy[key] = true
	return true
}

func (c *viewFactCache) store(key viewFactKey, fact viewFact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[viewFactKey]viewFact{}
	}
	c.m[key] = fact
}

func (c *viewFactCache) done(key viewFactKey) {
	c.mu.Lock()
	delete(c.busy, key)
	c.mu.Unlock()
}

func (i *instance) fillJellyfinViewFacts(r *http.Request, viewer *frontend.Viewer, viewID string, item map[string]any) {
	if i == nil || i.host.Experience == nil || r == nil || viewer == nil || item == nil {
		return
	}
	key, gen, cacheable := i.viewFactKey(viewer, viewID)
	if cacheable {
		if fact, ok := i.views.get(key); ok && fact.gen == gen {
			applyViewFact(item, fact)
			return
		} else if ok {
			applyViewFact(item, fact)
			if i.views.start(key) {
				copied := *viewer
				dev := device(r)
				go i.refreshViewFact(key, gen, &copied, dev, viewID)
			}
			return
		}
	}
	fact := i.loadViewFact(r.Context(), viewer, device(r), viewID)
	if cacheable && fact.counted {
		fact.gen = gen
		i.views.store(key, fact)
	}
	applyViewFact(item, fact)
}

func (i *instance) refreshViewFact(key viewFactKey, gen uint64, viewer *frontend.Viewer, dev frontend.Device, viewID string) {
	defer i.views.done(key)
	ctx, cancel := context.WithTimeout(i.life, 30*time.Second)
	defer cancel()
	fact := i.loadViewFact(ctx, viewer, dev, viewID)
	if !fact.counted {
		return
	}
	fact.gen = gen
	i.views.store(key, fact)
}

func (i *instance) viewFactKey(viewer *frontend.Viewer, viewID string) (viewFactKey, uint64, bool) {
	if i.host.PageFreshness == nil || viewer == nil {
		return viewFactKey{}, 0, false
	}
	sections := append([]int(nil), viewer.AllowedSections...)
	sort.Ints(sections)
	return viewFactKey{viewer: viewer.ID, view: viewID, rating: viewer.MaxContentRating, sections: fmt.Sprint(sections)}, i.host.PageFreshness().Catalog, true
}

func (i *instance) loadViewFact(ctx context.Context, viewer *frontend.Viewer, dev frontend.Device, viewID string) viewFact {
	page, err := i.host.Experience.LibraryItems(ctx, viewer, dev, viewID, frontend.ItemQuery{})
	if err != nil {
		return viewFact{}
	}
	fact := viewFact{count: page.Total, counted: true, dated: true}
	now := time.Now()
	for _, title := range page.Items {
		added := title.DateAdded(now)
		if added.IsZero() {
			continue
		}
		if fact.earliest.IsZero() || added.Before(fact.earliest) {
			fact.earliest = added
		}
		if fact.latest.IsZero() || added.After(fact.latest) {
			fact.latest = added
		}
	}
	return fact
}

func applyViewFact(item map[string]any, fact viewFact) {
	if item == nil || !fact.counted {
		return
	}
	item["ChildCount"] = fact.count
	if !fact.earliest.IsZero() {
		item["DateCreated"] = wireTime(fact.earliest)
	}
	if !fact.latest.IsZero() {
		item["DateLastMediaAdded"] = wireTime(fact.latest)
	}
}
