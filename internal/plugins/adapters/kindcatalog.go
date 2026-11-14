package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

const (
	defaultKindRefresh       = time.Hour
	defaultLiveSearchTimeout = 2 * time.Second
	maxLiveSearchPerPlugin   = 2
	liveSearchBackgroundWait = 10 * time.Second
)

// KindCatalogDeps configures a KindCatalog adapter.
type KindCatalogDeps struct {
	TitleInLibrary func(context.Context, core.Library, core.RatingKey) (bool, error)

	Manager           *plugins.Manager
	Libraries         *libraries.Service
	Items             *pluginitems.Service
	Blobs             *blobcache.Cache
	BlobDir           string
	Logger            *slog.Logger
	LiveSearchTimeout time.Duration
}

type liveSearchCall struct {
	done  chan struct{}
	items []core.KindItem
}

// KindCatalog bridges plugin media-kind declarations and lifecycle events to
// the libraries, pluginitems and blobcache stores, and implements
// experience.PluginLibraries so client surfaces read exclusively from the
// local store on browse, detail, children and facet paths.
type KindCatalog struct {
	titleInLibrary func(context.Context, core.Library, core.RatingKey) (bool, error)

	syncCtx           context.Context
	mgr               *plugins.Manager
	libs              *libraries.Service
	items             *pluginitems.Service
	blobs             *blobcache.Cache
	blobDir           string
	log               *slog.Logger
	liveSearchTimeout time.Duration

	quotaMu sync.RWMutex
	quotas  map[string]blobcache.Quota

	liveMu    sync.Mutex
	liveCalls map[string]*liveSearchCall
	liveSems  map[string]chan struct{}
}

var _ experience.PluginLibraries = (*KindCatalog)(nil)

// AttachKindCatalog constructs a KindCatalog and registers its OnSync and
// OnRemove lifecycle hooks on deps.Manager. Call before Manager.Start so
// already-installed kind plugins are reconciled on startup.
func AttachKindCatalog(syncCtx context.Context, deps KindCatalogDeps) *KindCatalog {
	if syncCtx == nil {
		syncCtx = context.Background()
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	timeout := deps.LiveSearchTimeout
	if timeout <= 0 {
		timeout = defaultLiveSearchTimeout
	}
	c := &KindCatalog{
		syncCtx:           syncCtx,
		titleInLibrary:    deps.TitleInLibrary,
		mgr:               deps.Manager,
		libs:              deps.Libraries,
		items:             deps.Items,
		blobs:             deps.Blobs,
		blobDir:           deps.BlobDir,
		log:               log.With("component", "kindcatalog"),
		liveSearchTimeout: timeout,
		quotas:            make(map[string]blobcache.Quota),
		liveCalls:         make(map[string]*liveSearchCall),
		liveSems:          make(map[string]chan struct{}),
	}
	if deps.Manager != nil {
		deps.Manager.OnSync(c.syncPlugin)
		deps.Manager.OnRemove(c.removePlugin)
	}
	return c
}

// QuotaFor returns the declared blobcache quota for (pluginID, libraryKey),
// used by blobcache.Cache.HostBackends and KindResolver.
func (c *KindCatalog) QuotaFor(pluginID, libraryKey string) blobcache.Quota {
	if c == nil {
		return blobcache.Quota{}
	}
	c.quotaMu.RLock()
	q, ok := c.quotas[pluginID+"\x00"+libraryKey]
	c.quotaMu.RUnlock()
	if ok {
		return q
	}
	return blobcache.Quota{}
}

func (c *KindCatalog) setQuota(pluginID, libraryKey string, decl plugins.KindCacheDecl) blobcache.Quota {
	q := blobcache.Quota{
		MaxBytes: decl.MaxBytes,
		TTL:      parseKindDuration(decl.TTL, 0),
	}
	c.quotaMu.Lock()
	c.quotas[pluginID+"\x00"+libraryKey] = q
	c.quotaMu.Unlock()
	return q
}

func (c *KindCatalog) deleteQuota(pluginID, libraryKey string) {
	c.quotaMu.Lock()
	delete(c.quotas, pluginID+"\x00"+libraryKey)
	c.quotaMu.Unlock()
}

func (c *KindCatalog) syncPlugin(ctx context.Context, r plugins.Record) {
	if c == nil || c.items == nil || c.libs == nil {
		return
	}
	var declared []plugins.MediaKindMeta
	if e := r.Manifest.EffectiveEther(); e != nil {
		for _, k := range e.Contributes.MediaKinds {
			if k.HasLibrary() {
				declared = append(declared, k)
			}
		}
	}
	existing, err := c.items.ListKinds(ctx, r.ID)
	if err != nil {
		c.log.Warn("list plugin kinds failed", "plugin", r.ID, "err", err)
		return
	}
	// Binding owner rule: plugins without any library-declaring mediaKinds
	// never touch the libraries store or create blobcache namespaces.
	if len(declared) == 0 && len(existing) == 0 {
		return
	}

	active := c.recordAllowsCatalogSync(ctx, r)
	keep := make(map[string]bool, len(declared))
	for _, k := range declared {
		keep[k.Key] = true
		libName := c.resolvePluginText(ctx, r.ID, k.Library.Name)
		if strings.TrimSpace(libName) == "" {
			libName = k.Key
		}
		lib, err := c.libs.UpsertPluginLibrary(ctx, r.ID, k.Key, libraries.PluginLibraryDecl{
			Name:    libName,
			Present: k.Present,
		})
		if err != nil {
			c.log.Warn("upsert plugin library failed", "plugin", r.ID, "kind", k.Key, "err", err)
			continue
		}
		q := c.setQuota(r.ID, lib.Key, k.Cache)
		if c.blobs != nil {
			c.blobs.BindPluginLibrary(r.ID, lib.Key)
			if c.blobNamespaceExistsOnDisk(lib.Key) {
				c.blobs.Namespace(lib.Key, q)
			}
		}
		if _, err := c.items.UpsertKind(ctx, r.ID, lib.ID, k, false); err != nil {
			c.log.Warn("upsert plugin kind failed", "plugin", r.ID, "kind", k.Key, "err", err)
			if c.mgr != nil {
				c.mgr.RecordFailure(ctx, r.ID, err.Error())
			}
			c.items.StopKindSync(r.ID, k.Key)
			continue
		}
		if active && k.Sync.Mode == plugins.KindSyncPull {
			interval := parseKindDuration(k.Sync.Refresh, defaultKindRefresh)
			c.items.StartKindSync(c.syncCtx, r.ID, k.Key, interval)
		} else {
			c.items.StopKindSync(r.ID, k.Key)
		}
	}

	// Purge any previously installed kind whose declaration or library block
	// was removed on update (pluginitems -> libraries -> blobcache).
	for _, old := range existing {
		if keep[old.KindKey] {
			continue
		}
		c.items.StopKindSync(r.ID, old.KindKey)
		var oldKey string
		if oldLib, err := c.libs.Get(ctx, old.LibraryID); err == nil {
			oldKey = oldLib.Key
		}
		if err := c.items.PurgeKind(ctx, r.ID, old.KindKey); err != nil {
			c.log.Warn("purge removed kind failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if err := c.libs.Delete(ctx, old.LibraryID); err != nil && !errors.Is(err, core.ErrNotFound) {
			c.log.Warn("delete removed kind library failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if oldKey != "" {
			c.deleteQuota(r.ID, oldKey)
			c.purgeBlobNamespace(oldKey)
		}
	}
}

func (c *KindCatalog) removePlugin(ctx context.Context, pluginID string) {
	if c == nil || c.items == nil {
		return
	}
	c.items.StopPluginSync(pluginID)
	existing, err := c.items.ListKinds(ctx, pluginID)
	if err != nil {
		c.log.Warn("list kinds on remove failed", "plugin", pluginID, "err", err)
	}
	// Non-kind plugins (or kinds that never declared a library) leave
	// libraries and blobcache completely untouched.
	if len(existing) == 0 {
		return
	}

	var libIDs []core.LibraryID
	var libKeys []string
	seenID := make(map[core.LibraryID]bool, len(existing))
	if c.libs != nil {
		for _, rec := range existing {
			if rec.LibraryID > 0 && !seenID[rec.LibraryID] {
				seenID[rec.LibraryID] = true
				libIDs = append(libIDs, rec.LibraryID)
				if lib, err := c.libs.Get(ctx, rec.LibraryID); err == nil && lib.Key != "" {
					libKeys = append(libKeys, lib.Key)
				}
			}
		}
	}

	// Spec §2.5 removal order: pluginitems -> libraries -> blobcache.
	if err := c.items.PurgePlugin(ctx, pluginID); err != nil {
		c.log.Warn("purge plugin items failed", "plugin", pluginID, "err", err)
	}
	if c.libs != nil {
		for _, id := range libIDs {
			if err := c.libs.Delete(ctx, id); err != nil && !errors.Is(err, core.ErrNotFound) {
				c.log.Warn("delete plugin library failed", "plugin", pluginID, "library", id, "err", err)
			}
		}
	}
	for _, key := range libKeys {
		c.deleteQuota(pluginID, key)
		c.purgeBlobNamespace(key)
	}
}

func (c *KindCatalog) blobNamespaceExistsOnDisk(libKey string) bool {
	if c.blobDir == "" || libKey == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(c.blobDir, libKey))
	return err == nil && info.IsDir()
}

func (c *KindCatalog) purgeBlobNamespace(libKey string) {
	if c.blobs == nil || libKey == "" {
		return
	}
	if c.blobDir != "" && !c.blobNamespaceExistsOnDisk(libKey) {
		return
	}
	c.blobs.Namespace(libKey, blobcache.Quota{}).Purge()
}

func (c *KindCatalog) resolvePluginText(ctx context.Context, pluginID, val string) string {
	val = strings.TrimSpace(val)
	key, ok := strings.CutPrefix(val, "$t:")
	if !ok || c.mgr == nil {
		return val
	}
	if resolved, ok := c.mgr.ResolveText(ctx, pluginID, key, "en"); ok && strings.TrimSpace(resolved) != "" {
		return resolved
	}
	return key
}

func (c *KindCatalog) recordAllowsCatalogSync(ctx context.Context, r plugins.Record) bool {
	if !r.Enabled || !r.Manifest.HasCapability(plugins.CapCatalogProvider) {
		return false
	}
	if c.mgr != nil {
		blockers, err := c.mgr.SetupBlockers(ctx, r.ID)
		if err != nil || len(blockers) > 0 {
			return false
		}
	}
	return true
}

// Libraries implements experience.PluginLibraries.
func (c *KindCatalog) Libraries(ctx context.Context) ([]core.Library, error) {
	if c == nil || c.libs == nil {
		return nil, nil
	}
	return c.libs.List(ctx)
}

// Library implements experience.PluginLibraries.
func (c *KindCatalog) Library(ctx context.Context, id core.LibraryID) (core.Library, error) {
	if c == nil || c.libs == nil {
		return core.Library{}, core.ErrNotFound
	}
	return c.libs.Get(ctx, id)
}

// IsPluginLibraryActive reports whether lib's owning plugin is installed,
// enabled, has catalog_provider active (not disabled via RolesOff), declares a
// library for KindKey, and has no required setup blockers. Runtime crash state
// intentionally does NOT hide the library so synced items remain browsable
// offline when a plugin fails at runtime.
func (c *KindCatalog) IsPluginLibraryActive(ctx context.Context, lib core.Library) bool {
	if c == nil || c.mgr == nil || lib.Source != core.LibraryPlugin || lib.PluginID == "" || lib.KindKey == "" {
		return false
	}
	r, err := c.mgr.Get(ctx, lib.PluginID)
	if err != nil {
		return false
	}
	if !c.recordAllowsCatalogSync(ctx, r) {
		return false
	}
	e := r.Manifest.EffectiveEther()
	if e == nil {
		return false
	}
	for _, k := range e.Contributes.MediaKinds {
		if k.Key == lib.KindKey {
			return k.HasLibrary()
		}
	}
	return false
}

// RootMediaType implements experience.PluginLibraries.
func (c *KindCatalog) RootMediaType(ctx context.Context, lib core.Library) core.MediaType {
	if c == nil || c.items == nil {
		return core.TypeMovie
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil || len(rec.Decl.Levels) == 0 {
		return core.TypeMovie
	}
	proj := pluginitems.Project(core.KindItem{Level: rec.Decl.Levels[0].Key}, rec.Decl, lib.Present)
	if proj.Type == "" {
		return core.TypeMovie
	}
	return proj.Type
}

// Count implements experience.PluginLibraries.
func (c *KindCatalog) Count(ctx context.Context, lib core.Library) (int, error) {
	if c == nil || c.items == nil {
		return 0, nil
	}
	policy, err := pluginitems.LibraryPolicy(ctx, lib)
	if err != nil {
		return 0, err
	}
	if !policy.Passive() {
		page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, Policy: policy, Limit: 1})
		return page.Total, err
	}
	tally, err := c.items.LibraryTally(ctx, lib.ID)
	if err != nil {
		return 0, err
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err == nil && len(rec.Decl.Levels) > 0 {
		return tally[rec.Decl.Levels[0].Key], nil
	}
	total := 0
	for _, v := range tally {
		total += v
	}
	return total, nil
}

// Browse implements experience.PluginLibraries. It reads strictly from the
// pluginitems store and never invokes the plugin runtime.
func (c *KindCatalog) Browse(ctx context.Context, lib core.Library, q frontend.ItemQuery, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	level := browseLevelForQuery(rec.Decl, lib.Present, q)
	if level == "-" {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0}, nil
	}
	policy, err := pluginitems.LibraryPolicy(ctx, lib)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	bq := pluginitems.BrowseQuery{
		NoTotal:   q.NoTotal,
		Policy:    policy,
		LibraryID: lib.ID,
		Level:     level,
		Sort:      mapBrowseSort(q.Sort, q.Desc),
		Facets:    mapFilterToFacets(rec.Decl, q.Filter),
		Cursor:    cursor,
		Limit:     limit,
	}
	page, err := c.items.Browse(ctx, bq)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	titles := c.projectItems(ctx, rec, lib, page.Items)
	return experience.PluginBrowsePage{
		Items:      titles,
		Total:      page.Total,
		NextCursor: page.NextCursor,
	}, nil
}

// Item implements experience.PluginLibraries.
func (c *KindCatalog) Item(ctx context.Context, key core.RatingKey) (experience.PluginItemView, error) {
	if c == nil || c.items == nil || c.libs == nil || !key.IsPluginKey() {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	item, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	hidden, e := c.items.ItemHidden(core.WithExplicitSearch(ctx), lib, item.Ref)
	if e != nil || hidden {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	titles := c.projectItems(ctx, rec, lib, []core.KindItem{item})
	return experience.PluginItemView{
		Library:     lib,
		Title:       titles[0],
		Images:      projectImages(item.Art),
		Unavailable: item.Unavailable,
		Playable:    item.Unavailable == nil && item.Playable && levelIsPlayable(rec.Decl, item.Level),
	}, nil
}

// ItemsMany implements experience.PluginLibraries.
func (c *KindCatalog) ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]experience.PluginItemView, error) {
	out := make(map[core.RatingKey]experience.PluginItemView, len(keys))
	if c == nil || c.items == nil || c.libs == nil || len(keys) == 0 {
		return out, nil
	}
	libsByID := make(map[core.LibraryID]core.Library)
	recsByID := make(map[core.LibraryID]pluginitems.KindRecord)
	for _, key := range keys {
		if !key.IsPluginKey() {
			continue
		}
		if _, seen := out[key]; seen {
			continue
		}
		item, libID, err := c.items.GetByKey(ctx, key)
		if err != nil {
			continue
		}
		lib, ok := libsByID[libID]
		if !ok {
			lib, err = c.libs.Get(ctx, libID)
			if err != nil {
				continue
			}
			libsByID[libID] = lib
		}
		rec, ok := recsByID[libID]
		if !ok {
			rec, err = c.items.GetKindByLibrary(ctx, libID)
			if err != nil {
				continue
			}
			recsByID[libID] = rec
		}
		hidden, e := c.items.ItemHidden(core.WithExplicitSearch(ctx), lib, item.Ref)
		if e != nil || hidden {
			continue
		}
		titles := c.projectItems(ctx, rec, lib, []core.KindItem{item})
		out[key] = experience.PluginItemView{
			Library:     lib,
			Title:       titles[0],
			Images:      projectImages(item.Art),
			Unavailable: item.Unavailable,
			Playable:    item.Unavailable == nil && item.Playable && levelIsPlayable(rec.Decl, item.Level),
		}
	}
	return out, nil
}

// Children implements experience.PluginLibraries.
func (c *KindCatalog) Children(ctx context.Context, parent core.RatingKey, recursive bool, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil || c.libs == nil || !parent.IsPluginKey() {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	parentItem, libID, err := c.items.GetByKey(ctx, parent)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	_, levelIdx, _ := rec.Decl.Level(parentItem.Level)
	if !recursive || levelIdx < 0 || levelIdx >= len(rec.Decl.Levels)-2 {
		policy, e := pluginitems.LibraryPolicy(ctx, lib)
		if e != nil {
			return experience.PluginBrowsePage{}, e
		}
		page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, ParentRef: parentItem.Ref, Cursor: cursor, Limit: limit, Policy: policy, Sort: "index"})
		if err != nil {
			return experience.PluginBrowsePage{}, err
		}
		return experience.PluginBrowsePage{
			Items:      c.projectItems(ctx, rec, lib, page.Items),
			Total:      page.Total,
			NextCursor: page.NextCursor,
		}, nil
	}

	// Recursive multi-level expansion (e.g. root -> intermediate -> leaf):
	// walk descendants breadth-first from the store with bounded pages.
	var leaves []core.KindItem
	queue := []core.ItemRef{parentItem.Ref}
	const maxExpanded = 2000
	for len(queue) > 0 && len(leaves) < maxExpanded {
		cur := queue[0]
		queue = queue[1:]
		childCursor := ""
		for {
			policy, e := pluginitems.LibraryPolicy(ctx, lib)
			if e != nil {
				return experience.PluginBrowsePage{}, e
			}
			page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, ParentRef: cur, Cursor: childCursor, Limit: 200, Policy: policy, Sort: "index"})
			if err != nil {
				return experience.PluginBrowsePage{}, err
			}
			for _, it := range page.Items {
				_, idx, _ := rec.Decl.Level(it.Level)
				if idx >= 0 && idx < len(rec.Decl.Levels)-1 && !it.Playable {
					queue = append(queue, it.Ref)
				} else {
					leaves = append(leaves, it)
					if len(leaves) >= maxExpanded {
						break
					}
				}
			}
			if page.NextCursor == "" || len(page.Items) == 0 || len(leaves) >= maxExpanded {
				break
			}
			childCursor = page.NextCursor
		}
	}
	offset := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n >= 0 {
			offset = n
		}
	}
	if limit <= 0 {
		limit = 200
	}
	start := min(offset, len(leaves))
	end := min(start+limit, len(leaves))
	next := ""
	if end < len(leaves) {
		next = strconv.Itoa(end)
	}
	return experience.PluginBrowsePage{
		Items:      c.projectItems(ctx, rec, lib, leaves[start:end]),
		Total:      len(leaves),
		NextCursor: next,
	}, nil
}

// Ancestors implements experience.PluginLibraries, returning ancestors ordered
// from root to immediate parent.
func (c *KindCatalog) Ancestors(ctx context.Context, key core.RatingKey) ([]core.Title, core.Library, error) {
	if c == nil || c.items == nil || c.libs == nil || !key.IsPluginKey() {
		return nil, core.Library{}, core.ErrNotFound
	}
	cur, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return nil, core.Library{}, core.ErrNotFound
	}
	var chain []core.KindItem
	for depth := 0; depth < 5 && cur.ParentID != ""; depth++ {
		p, pLibID, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, cur.ParentID)
		if err != nil || pLibID != libID {
			return nil, core.Library{}, core.ErrNotFound
		}
		chain = append(chain, p)
		cur = p
	}
	slices.Reverse(chain)
	return c.projectItems(ctx, rec, lib, chain), lib, nil
}

// Facets implements experience.PluginLibraries.
func (c *KindCatalog) Facets(ctx context.Context, lib core.Library, facet string) ([]core.FacetValue, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	rootLevel := ""
	if len(rec.Decl.Levels) > 0 {
		rootLevel = rec.Decl.Levels[0].Key
	}
	fields := resolveFacetFields(rec.Decl, facet)
	if len(fields) == 0 {
		return []core.FacetValue{}, nil
	}
	merged := make(map[string]int)
	for _, fieldKey := range fields {
		counts, err := c.items.Facets(ctx, lib.ID, rootLevel, fieldKey, 500)
		if err != nil {
			return nil, err
		}
		for _, fc := range counts {
			if fc.Value != "" {
				merged[fc.Value] += fc.Count
			}
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.FacetValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, core.FacetValue{
			ID:    core.FacetID(facet, k),
			Key:   k,
			Title: k,
			Count: merged[k],
		})
	}
	return out, nil
}

// Search implements experience.PluginLibraries: local FTS5/tsvector search plus
// optional 2-second bounded live plugin search with background upsert (§15.4).
func (c *KindCatalog) Search(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, limit int) ([]core.Title, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	policy, err := pluginitems.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return nil, err
	}
	localPage, err := c.items.Search(ctx, pluginitems.SearchQuery{
		Policy:    policy,
		LibraryID: lib.ID,
		Query:     query,
		Facets:    mapFilterToFacets(rec.Decl, q.Filter),
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	combined := localPage.Items
	if rec.Decl.Browse.LiveSearch && c.mgr != nil && c.IsPluginLibraryActive(ctx, lib) {
		liveItems := c.runLiveSearch(ctx, rec, query, limit)
		if len(liveItems) > 0 {
			seen := make(map[core.RatingKey]bool, len(combined)+len(liveItems))
			merged := make([]core.KindItem, 0, len(combined)+len(liveItems))
			for _, it := range combined {
				if it.Key != 0 && !seen[it.Key] {
					seen[it.Key] = true
					merged = append(merged, it)
				}
			}
			for _, it := range liveItems {
				if it.Key != 0 && !seen[it.Key] {
					seen[it.Key] = true
					merged = append(merged, it)
				}
			}
			combined = merged
		}
	}
	allowed := combined[:0]
	for _, it := range combined {
		hidden, e := c.items.ItemHidden(core.WithExplicitSearch(ctx), lib, it.Ref)
		if e != nil {
			return nil, e
		}
		if !hidden {
			allowed = append(allowed, it)
		}
	}
	return c.projectItems(ctx, rec, lib, allowed), nil
}

// MemberOf implements experience.PluginLibraries for a collection
// library: it reports whether key is a shared member of the collection,
// one indexed read on collections_members, never a walk.
func (c *KindCatalog) MemberOf(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error) {
	if c == nil || c.libs == nil || key == 0 {
		return false, nil
	}
	if c.titleInLibrary != nil {
		return c.titleInLibrary(ctx, lib, key)
	}
	ok, err := c.libs.SourceMember(ctx, lib, key)
	if err != nil || !ok {
		return ok, err
	}
	if lib.Source == core.LibraryCollection {
		return c.libs.CollectionMember(ctx, lib.CollectionID, key)
	}
	return len(lib.Rules) == 0 && lib.VisibilityDefault != "hide", nil
}

func (c *KindCatalog) runLiveSearch(ctx context.Context, rec pluginitems.KindRecord, query string, limit int) []core.KindItem {
	sfKey := rec.PluginID + "\x00" + rec.KindKey + "\x00" + strings.ToLower(strings.TrimSpace(query))

	c.liveMu.Lock()
	call, inflight := c.liveCalls[sfKey]
	if !inflight {
		call = &liveSearchCall{done: make(chan struct{})}
		c.liveCalls[sfKey] = call
		sem := c.liveSems[rec.PluginID]
		if sem == nil {
			sem = make(chan struct{}, maxLiveSearchPerPlugin)
			c.liveSems[rec.PluginID] = sem
		}
		go c.execLiveSearch(sfKey, sem, rec, query, limit, call)
	}
	c.liveMu.Unlock()

	budget := c.liveSearchTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < budget {
			budget = rem
		}
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case <-call.done:
		return call.items
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (c *KindCatalog) execLiveSearch(sfKey string, sem chan struct{}, rec pluginitems.KindRecord, query string, limit int, call *liveSearchCall) {
	defer func() {
		close(call.done)
		c.liveMu.Lock()
		delete(c.liveCalls, sfKey)
		c.liveMu.Unlock()
	}()

	bgCtx, cancel := context.WithTimeout(c.syncCtx, liveSearchBackgroundWait)
	defer cancel()

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-bgCtx.Done():
		return
	}

	body, err := json.Marshal(core.KindBrowseRequest{Query: query, Limit: limit})
	if err != nil {
		return
	}
	resp, err := c.mgr.Call(bgCtx, rec.PluginID, plugins.Request{
		Method: "POST",
		Path:   "/ether/v1/kinds/" + rec.KindKey + "/search",
		Body:   body,
	})
	if err != nil || resp.Status != 200 || len(resp.Body) == 0 {
		return
	}
	var page core.KindPage
	if err := json.Unmarshal(resp.Body, &page); err != nil || len(page.Items) == 0 {
		return
	}
	if _, err := c.items.UpsertItems(bgCtx, rec.PluginID, rec.KindKey, page.Items); err != nil {
		c.log.Debug("live search upsert failed", "plugin", rec.PluginID, "kind", rec.KindKey, "err", err)
		return
	}
	stored := make([]core.KindItem, 0, len(page.Items))
	for _, raw := range page.Items {
		if it, _, err := c.items.GetByExternal(bgCtx, rec.PluginID, rec.KindKey, raw.ID); err == nil {
			stored = append(stored, it)
		}
	}
	call.items = stored
}

func (c *KindCatalog) projectItems(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, items []core.KindItem) []core.Title {
	if len(items) == 0 {
		return []core.Title{}
	}
	rootCache := make(map[string]core.Title)
	resolveRoot := func(parentID string) (core.Title, bool) {
		if parentID == "" {
			return core.Title{}, false
		}
		if t, ok := rootCache[parentID]; ok {
			return t, t.Key != 0
		}
		curID := parentID
		var root core.KindItem
		found := false
		for depth := 0; depth < 4 && curID != ""; depth++ {
			p, pLib, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, curID)
			if err != nil || pLib != lib.ID {
				break
			}
			root = p
			found = true
			curID = p.ParentID
		}
		if !found {
			rootCache[parentID] = core.Title{}
			return core.Title{}, false
		}
		rt := pluginitems.Project(root, rec.Decl, lib.Present)
		rt.LibraryID = lib.ID
		if rt.ContentRating == "" {
			rt.ContentRating = extractContentRating(root.Fields)
		}
		rootCache[parentID] = rt
		return rt, true
	}

	out := make([]core.Title, len(items))
	for i, it := range items {
		t := pluginitems.Project(it, rec.Decl, lib.Present)
		t.LibraryID = lib.ID
		if t.ContentRating == "" {
			t.ContentRating = extractContentRating(it.Fields)
		}
		if (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) && it.ParentID != "" {
			if root, ok := resolveRoot(it.ParentID); ok {
				t.ShowKey = root.Key
				t.ShowTitle = root.Title
				t.ShowYear = root.Year
			}
		}
		out[i] = t
	}
	return out
}

func extractContentRating(fields map[string]json.RawMessage) string {
	for _, key := range []string{"contentRating", "certification", "mpaa"} {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func projectImages(art []core.KindArtRef) []frontend.Image {
	if len(art) == 0 {
		return nil
	}
	out := make([]frontend.Image, 0, len(art))
	for _, a := range art {
		u := strings.TrimSpace(a.URL)
		if u == "" {
			continue
		}
		kind := a.Role
		if kind == "" {
			kind = "poster"
		}
		out = append(out, frontend.Image{
			Kind:   kind,
			Path:   u,
			Width:  a.Width,
			Height: a.Height,
		})
	}
	return out
}

func levelIsPlayable(decl plugins.MediaKindMeta, levelKey string) bool {
	lv, _, ok := decl.Level(levelKey)
	return ok && lv.Playable
}

func browseLevelForQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) string {
	if len(decl.Levels) == 0 {
		return ""
	}
	if !q.Recursive && len(q.Types) == 0 {
		return decl.Levels[0].Key
	}
	if len(q.Types) == 0 {
		return decl.Levels[0].Key
	}
	// Match requested core.MediaType against projected level types.
	var matched []string
	for _, lv := range decl.Levels {
		if !q.Recursive && lv.Key != decl.Levels[0].Key {
			continue
		}
		pt := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present).Type
		if slices.Contains(q.Types, pt) {
			matched = append(matched, lv.Key)
		}
	}
	if len(matched) == 0 {
		return "-"
	}
	return matched[0]
}

func mapBrowseSort(sortBy core.SortOrder, desc bool) string {
	var col string
	switch sortBy {
	case "", core.SortTitle:
		col = "title"
	case core.SortAdded:
		col = "added"
	case core.SortReleased:
		col = "released"
	case core.SortYear:
		col = "year"
	case core.SortRating:
		col = "rating"
	case core.SortPopularity:
		col = "rating"
	default:
		col = "title"
	}
	if sortBy == "" && !desc {
		return "title:asc"
	}
	if desc {
		return col + ":desc"
	}
	return col + ":asc"
}

func resolveFacetFields(decl plugins.MediaKindMeta, facet string) []string {
	for _, f := range decl.Browse.Facets {
		if f == facet {
			return []string{f}
		}
	}
	if facet == "genre" {
		for _, fd := range decl.Fields {
			if fd.Indexed && (fd.Key == "genre" || fd.Key == "genres" || fd.Key == "tags") {
				return []string{fd.Key}
			}
		}
		if len(decl.Browse.Facets) > 0 {
			return append([]string(nil), decl.Browse.Facets...)
		}
	}
	return nil
}

func mapFilterToFacets(decl plugins.MediaKindMeta, f core.Filter) map[string][]string {
	if len(f.Genres) == 0 && f.Studio == "" {
		return nil
	}
	indexed := make(map[string]bool, len(decl.Fields))
	for _, fd := range decl.Fields {
		if fd.Indexed {
			indexed[fd.Key] = true
		}
	}
	out := make(map[string][]string)
	if len(f.Genres) > 0 {
		fields := resolveFacetFields(decl, "genre")
		if len(fields) > 0 && indexed[fields[0]] {
			out[fields[0]] = append([]string(nil), f.Genres...)
		}
	}
	if f.Studio != "" {
		for _, cand := range []string{"studio", "studios", "publisher", "author", "channel"} {
			if indexed[cand] {
				out[cand] = []string{f.Studio}
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseKindDuration(raw string, fallback time.Duration) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return fallback
}
