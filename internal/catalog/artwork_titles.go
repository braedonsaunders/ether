package catalog

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Artwork for titles the catalogue has not stored.
//
// Search and list hits (TMDB results and long-tail hits) are summaries in
// memory, not rows: a row is written when the title is opened. Their
// artwork must still be grantable and servable, or every frontend that
// signs images from stored rows shows them without a poster. ArtworkTitle
// and ArtworkTitles are CachedGet and CachedGetMany plus that memory, with
// the same visibility gates. They never fetch and never write.

// heldTitle is a movie or show known without a row: a list or search
// summary, else the long-tail entry, with what a poster lookup learned
// (poster, date) laid over it, else the lookup's own summary. dateUnknown
// reports a tail hit whose release date nobody has read yet: search lets
// those through, so their artwork must be served too.
func (c *Catalog) heldTitle(key core.RatingKey) (t core.Title, dateUnknown, ok bool) {
	if key.Kind() != core.KindMovie && key.Kind() != core.KindShow {
		return core.Title{}, false, false
	}
	info, looked := c.posters.get(key, c.now())
	if t, ok = c.listSummary(key); !ok {
		var e tailEntry
		if e, ok = c.tailEntry(key); ok {
			t, dateUnknown = e.Title, e.DateUnknown
		}
	}
	if !ok {
		if looked && info.title.Key == key {
			return info.title, false, true
		}
		return core.Title{}, false, false
	}
	if looked {
		info.applyTo(&t)
		dateUnknown = dateUnknown && t.Released.IsZero()
	}
	return t, dateUnknown, true
}

// summaryTitle is heldTitle without the date state.
func (c *Catalog) summaryTitle(key core.RatingKey) (core.Title, bool) {
	t, _, ok := c.heldTitle(key)
	return t, ok
}

// UndatedSearchHit reports a long-tail search hit with no row whose release
// date is not known yet. Search shows those, so artwork that requires a
// released title admits them.
func (c *Catalog) UndatedSearchHit(ctx context.Context, key core.RatingKey) bool {
	if _, err := c.loadByKey(ctx, key); !errors.Is(err, core.ErrNotFound) {
		return false
	}
	t, dateUnknown, ok := c.heldTitle(key)
	return ok && dateUnknown && !t.Adult
}

// ArtworkTitle is CachedGet that also admits a title held only in memory.
func (c *Catalog) ArtworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	stored, err := c.loadByKey(ctx, key)
	if err == nil {
		if !c.visibleTitle(&stored.t) {
			return nil, core.ErrNotFound
		}
		return &stored.t, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	t, dateUnknown, ok := c.heldTitle(key)
	if !ok || !(c.visibleTitle(&t) || (dateUnknown && !t.Adult)) {
		return nil, core.ErrNotFound
	}
	return &t, nil
}

// ArtworkTitles is CachedGetMany that also admits titles held only in
// memory. A title of the viewer's current export page comes as that page
// read it (exportCache): the facts its policy reads, so an export's signer
// does not read the whole library a second time.
func (c *Catalog) ArtworkTitles(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	exported := map[core.RatingKey]core.Title{}
	rest := make([]core.RatingKey, 0, len(keys))
	for _, key := range keys {
		if t, ok := c.exportTitle(ctx, key, gen, minute); ok {
			exported[key] = t
		} else {
			rest = append(rest, key)
		}
	}
	out, err := c.CachedGetMany(ctx, rest)
	if err != nil {
		return nil, err
	}
	for key, t := range exported {
		if c.cachedListVisible(&t) {
			out[key] = t
		}
	}
	for _, key := range keys {
		if _, ok := out[key]; ok {
			continue
		}
		// A row CachedGetMany left out is hidden: memory must not bring
		// it back. Only keys with no row fall through (one key seek each,
		// for the page's search hits).
		if _, err := c.loadByKey(ctx, key); !errors.Is(err, core.ErrNotFound) {
			continue
		}
		if t, dateUnknown, ok := c.heldTitle(key); ok && (c.cachedListVisible(&t) || (dateUnknown && !t.Adult)) {
			out[key] = t
		}
	}
	return out, nil
}

// MemoryTitle tells an artwork reader how to read key without promoting it.
// stored reports a row (read it as usual); otherwise t is the title held in
// memory, or nil when the catalogue knows nothing of key. Reading a
// search hit's thumbnail must not store the hit: only opening it does.
func (c *Catalog) MemoryTitle(ctx context.Context, key core.RatingKey) (t *core.Title, stored bool, err error) {
	if _, err := c.loadByKey(ctx, key); err == nil {
		return nil, true, nil
	} else if !errors.Is(err, core.ErrNotFound) {
		return nil, false, err
	}
	if mem, ok := c.summaryTitle(key); ok {
		return &mem, false, nil
	}
	return nil, false, nil
}

// PersistsArtwork reports whether facts derived from key's artwork
// (blurhashes, UltraBlur colours) may be written: yes for a stored title
// and for every key that is not a title; no for a movie, show, season or
// episode with no row. Search and list hits are read-only: their artwork
// facts stay in memory, or every search typed would add rows.
func (c *Catalog) PersistsArtwork(ctx context.Context, key core.RatingKey) bool {
	switch key.Kind() {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode:
	default:
		return true
	}
	// Persistence depends only on whether a row exists. Hydrating a title
	// here loads every credit for a cache miss while merely touching artwork.
	var present int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM catalog_titles WHERE key=?`, int64(key)).Scan(&present)
	return err == nil
}

// ArtworkRows is ArtworkTitles for an artwork request that lists no credited
// people's portraits (frontend.WithArtworkRows): stored movies and shows come
// as their rows with the facts the catalogue's policy for ctx's viewer reads
// and facts, never read in full nor put in the title cache. They are kept as
// that viewer's partial titles (exportCache) for the image reads that follow
// a grant; a title already cached in full is served from the cache. Other
// keys are read as ArtworkTitles reads them.
func (c *Catalog) ArtworkRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	out := make(map[core.RatingKey]core.Title, len(keys))
	var rows, other []core.RatingKey
	for _, key := range keys {
		if key.Kind() != core.KindMovie && key.Kind() != core.KindShow {
			other = append(other, key)
			continue
		}
		if t, ok := c.cachedStored(key, gen, minute); ok {
			if c.cachedListVisible(&t) {
				out[key] = t
			}
			continue
		}
		if t, ok := c.exportTitle(ctx, key, gen, minute); ok {
			if c.cachedListVisible(&t) {
				out[key] = t
			}
			continue
		}
		rows = append(rows, key)
	}
	facts |= c.viewerPolicyFacts(ctx) | c.policyFacts(c.visibilityFor(ctx))
	for part := range store.Chunks(rows, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		q, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		var items []core.Title
		for q.Next() {
			st, err := scanTitle(q)
			if err != nil {
				q.Close()
				return nil, err
			}
			items = append(items, st.t)
		}
		err = q.Err()
		q.Close()
		if err != nil {
			return nil, err
		}
		if err := c.hydrateFacts(ctx, items, facts); err != nil {
			return nil, err
		}
		stored := make(map[core.RatingKey]bool, len(items))
		visible := items[:0:0]
		for i := range items {
			stored[items[i].Key] = true
			if c.cachedListVisible(&items[i]) {
				out[items[i].Key] = items[i]
				visible = append(visible, items[i])
			}
		}
		// The image reads that follow a grant find the rows the grant read.
		c.rememberExportTitles(ctx, gen, minute, visible)
		// A key with no row is a search or list hit held in memory.
		for _, key := range part {
			if stored[key] {
				continue
			}
			if t, dateUnknown, ok := c.heldTitle(key); ok && (c.cachedListVisible(&t) || (dateUnknown && !t.Adult)) {
				out[key] = t
			}
		}
	}
	if len(other) > 0 {
		full, err := c.ArtworkTitles(ctx, other)
		if err != nil {
			return nil, err
		}
		for k, t := range full {
			out[k] = t
		}
	}
	return out, nil
}
