package mediabrowser

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// libraryCounter is the indexed library size. The view list must not browse
// or hydrate titles to learn it.
type libraryCounter interface {
	LibraryCount(context.Context, *frontend.Viewer, string) (int, error)
}

type viewFact struct {
	gen   uint64
	count int
}

type viewFactKey struct {
	viewer   int64
	view     string
	rating   string
	sections string
}

type viewFactCache struct {
	mu sync.Mutex
	m  map[viewFactKey]viewFact
}

func (i *instance) fillJellyfinViewFacts(r *http.Request, viewer *frontend.Viewer, viewID string, item map[string]any) {
	if i == nil || r == nil || viewer == nil || item == nil || i.host.Experience == nil {
		return
	}
	counter, ok := i.host.Experience.(libraryCounter)
	if !ok {
		return
	}
	key, gen, cacheable := i.viewFactKey(viewer, viewID)
	if cacheable {
		if fact, hit := i.views.get(key); hit && fact.gen == gen {
			item["ChildCount"] = fact.count
			return
		}
	}
	count, err := counter.LibraryCount(r.Context(), viewer, viewID)
	if err != nil {
		return
	}
	if cacheable {
		i.views.store(key, viewFact{gen: gen, count: count})
	}
	item["ChildCount"] = count
}

func (c *viewFactCache) get(key viewFactKey) (viewFact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fact, ok := c.m[key]
	return fact, ok
}

func (c *viewFactCache) store(key viewFactKey, fact viewFact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[viewFactKey]viewFact{}
	}
	c.m[key] = fact
}

func (i *instance) viewFactKey(viewer *frontend.Viewer, viewID string) (viewFactKey, uint64, bool) {
	if i.host.PageFreshness == nil || viewer == nil {
		return viewFactKey{}, 0, false
	}
	sections := append([]int(nil), viewer.AllowedSections...)
	sort.Ints(sections)
	return viewFactKey{viewer: viewer.ID, view: viewID, rating: viewer.MaxContentRating, sections: fmt.Sprint(sections)}, i.host.PageFreshness().Catalog, true
}
