package app

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

// libraryProfileCacheEntries bounds the per-title library→profile answer.
// A title is asked for on every resolve, so the cache keeps the request
// path free of repeated filter SQL; the libraries generation invalidates it
// on any owner edit.
const libraryProfileCacheEntries = 8192

// libraryProfiles answers resolver.LibraryProfiles: which quality profile a
// title ranks with, from the library it belongs to (spec #419). A filter
// library that contains the title wins over the built-in Movies/TV Shows
// library; the lowest library id breaks a tie between matching filter
// libraries. Membership is one indexed read per filter library (no walk),
// and the answer is cached per title until the libraries generation moves.
type libraryProfiles struct {
	app *App

	mu    sync.Mutex
	gen   uint64
	cache *lru.Cache[core.RatingKey, string]
}

func newLibraryProfiles(a *App) *libraryProfiles {
	return &libraryProfiles{app: a, cache: lru.New[core.RatingKey, string](lru.Options[core.RatingKey, string]{MaxEntries: libraryProfileCacheEntries})}
}

// QualityProfileForTitle returns the profile name set on the title's most
// specific library, or "" for Default. It never blocks a request on more
// than one indexed read per filter library, and caches the result.
func (l *libraryProfiles) QualityProfileForTitle(ctx context.Context, t *core.Title) string {
	if l == nil || l.app == nil || l.app.libraries == nil || t == nil {
		return ""
	}
	key := t.Key
	if name, ok := l.cached(key); ok {
		return name
	}
	name := l.resolve(ctx, t)
	l.store(key, name)
	return name
}

func (l *libraryProfiles) cached(key core.RatingKey) (string, bool) {
	gen := l.app.libraries.Generation()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen {
		l.cache.Clear()
		l.gen = gen
	}
	return l.cache.Get(key)
}

func (l *libraryProfiles) store(key core.RatingKey, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache.Add(key, name)
}

// resolve picks the winning library and returns its profile name.
func (l *libraryProfiles) resolve(ctx context.Context, t *core.Title) string {
	media := titleMedia(t)
	if media == core.MediaAny {
		return ""
	}
	libs, err := l.app.libraries.List(ctx)
	if err != nil {
		return ""
	}
	// Filter libraries are more specific than the built-in library. Among
	// several matches the lowest id wins; the list is already in sort/id
	// order, but a filter library may sort anywhere, so track the minimum.
	var winner core.Library
	found := false
	for _, lib := range libs {
		if lib.Source != core.LibraryFilter || lib.Filter == nil {
			continue
		}
		if lib.Filter.Type != "" && string(lib.Filter.Type) != media {
			continue
		}
		if l.app.catalog == nil {
			continue
		}
		ok, err := l.app.catalog.TitleInFilter(ctx, *lib.Filter, t.Key)
		if err != nil || !ok {
			continue
		}
		if !found || lib.ID < winner.ID {
			winner, found = lib, true
		}
	}
	if found {
		return winner.QualityProfile
	}
	// No filter library matched: the canonical built-in library.
	if canonical := core.CanonicalLibraryForTitle(t.Type); canonical != 0 {
		if lib, err := l.app.libraries.Get(ctx, canonical); err == nil {
			return lib.QualityProfile
		}
	}
	return ""
}

// titleMedia classifies a title as a movie or a show for library matching.
func titleMedia(t *core.Title) string {
	switch t.Type {
	case core.TypeMovie:
		return core.MediaMovie
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		return core.MediaTV
	}
	switch t.Key.Kind() {
	case core.KindMovie:
		return core.MediaMovie
	case core.KindEpisode:
		return core.MediaTV
	}
	return core.MediaAny
}
