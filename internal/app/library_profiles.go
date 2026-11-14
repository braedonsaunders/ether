package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
// library; the lowest library id with a compatible explicit profile breaks
// a tie between matching filter/collection libraries, independent of entry point.
// Membership is one indexed read per filter library (no walk),
// and the answer is cached until local generations move or the short TTL expires.
type libraryProfiles struct {
	app *App

	mu         sync.Mutex
	gen        uint64
	catalogGen uint64
	configGen  uint64
	at         time.Time
	cache      *lru.Cache[core.RatingKey, string]
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
	key := libraryProfileKey(t)
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
	catalogGen := l.app.catalog.WriteGeneration()
	configGen := l.app.cfg.Generation()
	if l.gen != gen || l.configGen != configGen || l.catalogGen != catalogGen || time.Since(l.at) > 30*time.Second {
		l.cache.Clear()
		l.gen = gen
		l.catalogGen = catalogGen
		l.configGen = configGen
		l.at = time.Now()
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
	// Filter and collection libraries are more specific than the built-in
	// library. Among several matches the lowest id wins; the list is
	// already in sort/id order, but such a library may sort anywhere, so
	// track the minimum.
	var winner core.Library
	found := false
	profiles := l.app.cfg.Read().Quality.Profiles
	for _, lib := range libs {
		if lib.QualityProfile == "" {
			continue
		}
		scope := core.MediaMovie
		if media == string(core.TypeShow) {
			scope = core.MediaTV
		}
		if _, ok := core.FindProfile(profiles, lib.QualityProfile, scope); !ok {
			continue
		}
		if lib.Source != core.LibraryFilter && lib.Source != core.LibraryCollection {
			continue
		}
		var membership core.Filter
		switch lib.Source {
		case core.LibraryFilter:
			if lib.Filter == nil {
				continue
			}
			if lib.Filter.Type != "" && string(lib.Filter.Type) != media {
				continue
			}
			membership = *lib.Filter
		case core.LibraryCollection:
			if lib.CollectionID <= 0 || (lib.MediaType != "" && string(lib.MediaType) != media) {
				continue
			}
			membership = core.Filter{Type: lib.MediaType, CollectionMembers: lib.CollectionID}
		}
		membership.CatalogueSources = lib.Sources
		if l.app.catalog == nil {
			continue
		}
		ok, err := l.app.catalog.TitleInFilter(ctx, membership, libraryProfileKey(t))
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
	canonical := core.LibraryMovies
	if media == string(core.TypeShow) {
		canonical = core.LibraryShows
	}
	if canonical != 0 {
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
		return string(core.TypeShow)
	}
	switch t.Key.Kind() {
	case core.KindMovie:
		return core.MediaMovie
	case core.KindShow, core.KindSeason, core.KindEpisode:
		return string(core.TypeShow)
	}
	return core.MediaAny
}

// Seasons and episodes inherit membership and the cached profile of their show.
func libraryProfileKey(t *core.Title) core.RatingKey {
	if (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) && t.ShowKey != 0 {
		return t.ShowKey
	}
	switch t.Key.Kind() {
	case core.KindSeason, core.KindEpisode:
		return core.ShowKey(t.Key.TMDB())
	}
	return t.Key
}

// PolicyKey binds durable feed evidence to library profile assignments and filters.
func (l *libraryProfiles) PolicyKey(ctx context.Context) string {
	libs, err := l.app.libraries.List(ctx)
	if err != nil {
		return "unavailable"
	}
	b, _ := json.Marshal(libs)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (a *App) playbackProfile(caps core.ClientCaps, user *core.User, key core.RatingKey) core.QualityProfile {
	ctx := context.Background()
	// Plugin items open their own ByteSource; video release filters have no meaning.
	if key.IsPluginKey() {
		return core.QualityProfile{}
	}
	// Encoded keys identify movie/show and the parent show without hydrating
	// metadata or calling a catalogue provider on the playback path.
	return a.resolver.ProfileForTitle(ctx, caps, user, &core.Title{Key: key})
}
