package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

const (
	defaultKindRefresh       = time.Hour
	defaultLiveSearchTimeout = 2 * time.Second
	maxLiveSearchPerPlugin   = 2
	liveSearchBackgroundWait = 10 * time.Second
)

// KindCatalogDeps configures a KindCatalog adapter.
type KindCatalogDeps struct {
	TitleInLibrary  func(context.Context, core.Library, core.RatingKey) (bool, error)
	TitlesInLibrary func(context.Context, core.Library, []core.RatingKey) (map[core.RatingKey]bool, error)

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
	titleInLibrary  func(context.Context, core.Library, core.RatingKey) (bool, error)
	titlesInLibrary func(context.Context, core.Library, []core.RatingKey) (map[core.RatingKey]bool, error)

	syncCtx           context.Context
	mgr               *plugins.Manager
	libs              *libraries.Service
	items             *pluginitems.Service
	blobs             *blobcache.Cache
	blobDir           string
	log               *slog.Logger
	liveSearchTimeout time.Duration
	customLiveTimeout bool

	quotaMu sync.RWMutex
	quotas  map[string]blobcache.Quota

	liveMu    sync.Mutex
	liveCalls map[string]*liveSearchCall
	liveSems  map[string]chan struct{}

	hydrateMu       sync.Mutex
	hydrateInflight map[string]*containerHydrateCall
	hydrateSems     map[string]chan struct{}
}

var (
	_ experience.PluginLibraries             = (*KindCatalog)(nil)
	_ experience.PluginPeopleLibraries       = (*KindCatalog)(nil)
	_ experience.PluginPresentationLibraries = (*KindCatalog)(nil)
	_ experience.PluginLevelFacetLibraries   = (*KindCatalog)(nil)
	_ experience.PluginBatchBrowseLibraries  = (*KindCatalog)(nil)
)

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
	customLiveTimeout := deps.LiveSearchTimeout > 0
	timeout := deps.LiveSearchTimeout
	if timeout <= 0 {
		timeout = defaultLiveSearchTimeout
	}
	c := &KindCatalog{
		syncCtx:           syncCtx,
		titleInLibrary:    deps.TitleInLibrary,
		titlesInLibrary:   deps.TitlesInLibrary,
		mgr:               deps.Manager,
		libs:              deps.Libraries,
		items:             deps.Items,
		blobs:             deps.Blobs,
		blobDir:           deps.BlobDir,
		log:               log.With("component", "kindcatalog"),
		liveSearchTimeout: timeout,
		customLiveTimeout: customLiveTimeout,
		quotas:            make(map[string]blobcache.Quota),
		liveCalls:         make(map[string]*liveSearchCall),
		liveSems:          make(map[string]chan struct{}),
		hydrateInflight:   make(map[string]*containerHydrateCall),
		hydrateSems:       make(map[string]chan struct{}),
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

// RefreshPlugin reconciles an explicitly restored library with current setup
// and enabled state, including restarting its background synchronization.
func (c *KindCatalog) RefreshPlugin(ctx context.Context, r plugins.Record) { c.syncPlugin(ctx, r) }

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
	// The owner confirmed a non-additive kind change at install: kinds whose
	// shape changed are rebuilt (their items purged); others are untouched.
	replace := c.mgr != nil && c.mgr.TakeReplaceLibraries(r.ID)
	keep := make(map[string]bool, len(declared))
	for _, k := range declared {
		keep[k.Key] = true
		libName := c.resolvePluginTextFallback(ctx, r.ID, k.Library.Name, k.Key)
		presInput := kindPresentationInput(k)
		initial := libraries.ResolveKindPresentations(presInput, core.PresentationOverrides{})
		lib, err := c.libs.UpsertPluginLibrary(ctx, r.ID, k.Key, libraries.PluginLibraryDecl{
			Name:    libName,
			Present: initial.Effective,
		})
		if errors.Is(err, libraries.ErrDeleted) {
			c.items.CancelKindSync(r.ID, k.Key)
			continue
		}
		if err != nil {
			c.log.Warn("upsert plugin library failed", "plugin", r.ID, "kind", k.Key, "err", err)
			continue
		}
		// Reconcile HOW each app sees the library from the current manifest
		// and kind shape while honoring the owner's sticky on/off switches
		// (lib.PresentOverrides). This heals pre-W170 stale rows on startup
		// and applies updated itemTypes/bases on plugin updates without ever
		// clobbering an owner toggle.
		reconciled := libraries.ResolveKindPresentations(presInput, lib.PresentOverrides)
		if !presentationsEqual(lib.Present, reconciled.Effective) {
			if err := c.libs.SetPresent(ctx, lib.ID, reconciled.Effective); err != nil {
				c.log.Warn("reconcile plugin library presentation failed", "plugin", r.ID, "kind", k.Key, "err", err)
			} else {
				lib.Present = reconciled.Effective
			}
		}
		q := c.setQuota(r.ID, lib.Key, k.Cache)
		if c.blobs != nil {
			c.blobs.BindPluginLibrary(r.ID, lib.Key)
			if c.blobNamespaceExistsOnDisk(lib.Key) {
				c.blobs.Namespace(lib.Key, q)
			}
		}
		// Cancel any in-flight pull sync BEFORE UpsertKind so a running
		// sync pass releases kindLock after at most one chunk rather than
		// blocking plugin install/upgrade/disable.
		c.items.CancelKindSync(r.ID, k.Key)
		if _, err := c.items.UpsertKind(ctx, r.ID, lib.ID, k, replace); err != nil {
			c.log.Warn("upsert plugin kind failed", "plugin", r.ID, "kind", k.Key, "err", err)
			if c.mgr != nil {
				c.mgr.RecordFailure(ctx, r.ID, err.Error())
			}
			continue
		}
		if active && k.Sync.Mode == plugins.KindSyncPull {
			interval := parseKindDuration(k.Sync.Refresh, defaultKindRefresh)
			c.items.StartKindSync(c.syncCtx, r.ID, k.Key, interval)
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
		if oldLib, err := c.libs.Retained(ctx, old.LibraryID); err == nil {
			oldKey = oldLib.Key
		}
		if err := c.items.PurgeKind(ctx, r.ID, old.KindKey); err != nil {
			c.log.Warn("purge removed kind failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if err := c.libs.Purge(ctx, old.LibraryID); err != nil && !errors.Is(err, core.ErrNotFound) {
			c.log.Warn("delete removed kind library failed", "plugin", r.ID, "kind", old.KindKey, "err", err)
		}
		if oldKey != "" {
			c.deleteQuota(r.ID, oldKey)
			c.purgeBlobNamespace(oldKey)
		}
	}
}

// presentationsEqual compares two declared presentations by their JSON
// shape, the form both the manifest and the library store use.
func presentationsEqual(a, b core.Presentations) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}

func kindPresentationInput(k plugins.MediaKindMeta) libraries.KindPresentationInput {
	levels := make([]libraries.KindPresentationLevel, len(k.Levels))
	for i, lv := range k.Levels {
		levels[i] = libraries.KindPresentationLevel{
			Key:       lv.Key,
			Parent:    lv.Parent,
			Media:     lv.Media,
			Role:      lv.Role,
			Playable:  lv.Playable,
			Container: lv.Container,
		}
	}
	return libraries.KindPresentationInput{
		Media:   k.Media,
		Levels:  levels,
		Present: k.Present,
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
				if lib, err := c.libs.Retained(ctx, rec.LibraryID); err == nil && lib.Key != "" {
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
			if err := c.libs.Purge(ctx, id); err != nil && !errors.Is(err, core.ErrNotFound) {
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
	return c.resolvePluginTextFallback(ctx, pluginID, val, "")
}

func (c *KindCatalog) resolvePluginTextFallback(ctx context.Context, pluginID, val, fallback string) string {
	var resolve func(string) (string, bool)
	if c != nil && c.mgr != nil {
		resolve = c.mgr.ResolveTextFunc(ctx, pluginID, "")
	}
	return plugins.ResolveTextValue(val, fallback, resolve)
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
	// A page may check many items from the same plugin library. Remember
	// missing/disabled providers too: a missing plugin must not cost one SQL
	// lookup per candidate. The read-request memo expires after two seconds;
	// background and write contexts still recheck the current record each time.
	active, _ := reqmemo.Do(ctx, fmt.Sprintf("plugin-library-active:%p:%v:%s:%s", c, lib.ID, lib.PluginID, lib.KindKey), func() (bool, error) {
		r, err := c.mgr.Get(ctx, lib.PluginID)
		if errors.Is(err, plugins.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !c.recordAllowsCatalogSync(ctx, r) {
			return false, nil
		}
		e := r.Manifest.EffectiveEther()
		if e == nil {
			return false, nil
		}
		for _, k := range e.Contributes.MediaKinds {
			if k.Key == lib.KindKey {
				return k.HasLibrary(), nil
			}
		}
		return false, nil
	})
	return active
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
	proj := pluginitems.Project(core.KindItem{Level: rec.Decl.Levels[0].Key}, rec.Decl, lib.Present, lib.CardShape)
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
	policy, err := c.items.LibraryPolicy(ctx, lib)
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

type pivRequestCache struct {
	mu    sync.RWMutex
	items map[core.RatingKey]experience.PluginItemView
}

func (c *KindCatalog) pivMemo(ctx context.Context) *pivRequestCache {
	if c == nil || c.items == nil || !reqmemo.Active(ctx) {
		return nil
	}
	key := fmt.Sprintf("kindcatalog:piv:%p:%d:%d", c, core.CatalogViewerID(ctx), c.items.WriteGen())
	cache, err := reqmemo.Do(ctx, key, func() (*pivRequestCache, error) {
		return &pivRequestCache{items: make(map[core.RatingKey]experience.PluginItemView)}, nil
	})
	if err != nil {
		return nil
	}
	return cache
}

func (c *KindCatalog) buildPluginItemView(rec pluginitems.KindRecord, lib core.Library, landscape bool, it core.KindItem, title core.Title) (experience.PluginItemView, []core.KindItemPart) {
	parts := syntheticAudiobookParts(it, title)
	view := experience.PluginItemView{
		Library:      lib,
		Title:        title,
		Images:       projectImages(it.Art, landscape),
		Chapters:     extractItemDetailChapters(it, parts),
		Unavailable:  it.Unavailable,
		Playable:     it.Unavailable == nil && it.Playable && levelIsPlayable(rec.Decl, it.Level),
		SkipContinue: shouldSkipItemContinue(rec.Decl, lib.Present, it.Level),
	}
	return view, parts
}

func (c *KindCatalog) rememberBrowseViews(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, items []core.KindItem, titles []core.Title) {
	cache := c.pivMemo(ctx)
	if cache == nil || len(items) == 0 || len(items) != len(titles) {
		return
	}
	landscape := lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape
	cache.mu.Lock()
	for i, it := range items {
		if it.Key == 0 {
			continue
		}
		if _, exists := cache.items[it.Key]; exists {
			continue
		}
		view, _ := c.buildPluginItemView(rec, lib, landscape, it, titles[i])
		cache.items[it.Key] = view
	}
	cache.mu.Unlock()
}

// BrowseBatch implements experience.PluginBatchBrowseLibraries by executing all
// shelf queries for one library together in pluginitems.Service.BrowseBatch and
// projecting every unique item once.
func (c *KindCatalog) BrowseBatch(ctx context.Context, lib core.Library, queries []frontend.ItemQuery) ([]experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil {
		return nil, core.ErrNotFound
	}
	if len(queries) == 0 {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return nil, err
	}

	out := make([]experience.PluginBrowsePage, len(queries))
	bqs := make([]pluginitems.BrowseQuery, 0, len(queries))
	bqIdx := make([]int, 0, len(queries))
	flatFlags := make([]bool, 0, len(queries))
	viewerID := core.CatalogViewerID(ctx)

	for i, q := range queries {
		if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
			out[i] = experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}
			continue
		}
		level, levels, anyLevel, matched := browseLevelsForQuery(rec.Decl, lib.Present, q)
		if !matched {
			out[i] = experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}
			continue
		}
		limit := q.Limit
		if limit <= 0 {
			limit = pluginitems.DefaultBrowseLimit
		}
		bq := pluginitems.BrowseQuery{
			NoTotal:          q.NoTotal,
			CheapTotal:       q.CheapTotal,
			Policy:           policy,
			LibraryID:        lib.ID,
			ViewerID:         viewerID,
			Level:            level,
			Levels:           levels,
			AnyLevel:         anyLevel,
			PersonID:         q.Filter.Person,
			Sort:             mapBrowseSort(q.Sort, q.Desc),
			Facets:           mapFilterToFacets(rec.Decl, q.Filter),
			ExcludeFacets:    mapExcludeGenresToFacets(rec.Decl, q.Filter.ExcludeGenres),
			Watched:          q.Watched,
			MaxContentRating: q.Filter.MaxContentRating,
			ExcludeKeys:      q.Filter.ExcludeKeys,
			Limit:            limit,
		}
		if len(q.Filter.TabRules) > 0 || q.Filter.TabFallback != "" {
			tp, tpErr := c.items.CompilePolicy(ctx, lib.ID, q.Filter.TabRules, q.Filter.TabFallback)
			if tpErr != nil {
				return nil, tpErr
			}
			bq.TabPolicy = tp
		}
		bqs = append(bqs, bq)
		bqIdx = append(bqIdx, i)
		flatFlags = append(flatFlags, flatAudioQuery(rec.Decl, lib.Present, q))
	}
	if len(bqs) == 0 {
		return out, nil
	}

	pages, err := c.items.BrowseBatch(ctx, bqs)
	if err != nil {
		return nil, err
	}

	// Deduplicate items across all shelves so projectItems runs once per unique item.
	uniqueItems := make([]core.KindItem, 0, len(bqs)*8)
	uniqueIdx := make(map[core.ItemRef]int, len(bqs)*8)
	for _, p := range pages {
		for _, it := range p.Items {
			if _, seen := uniqueIdx[it.Ref]; !seen {
				uniqueIdx[it.Ref] = len(uniqueItems)
				uniqueItems = append(uniqueItems, it)
			}
		}
	}
	uniqueTitles := c.projectItems(ctx, rec, lib, uniqueItems)
	anyNonFlat := false
	for _, flat := range flatFlags {
		if !flat {
			anyNonFlat = true
			break
		}
	}
	if anyNonFlat {
		c.rememberBrowseViews(ctx, rec, lib, uniqueItems, uniqueTitles)
	}

	for j, p := range pages {
		origIdx := bqIdx[j]
		flat := flatFlags[j]
		titles := make([]core.Title, len(p.Items))
		for k, it := range p.Items {
			t := uniqueTitles[uniqueIdx[it.Ref]]
			if flat && it.Playable && it.Unavailable == nil {
				t = flatAlbumIncarnation(t)
			}
			titles[k] = t
		}
		out[origIdx] = experience.PluginBrowsePage{
			Items:      titles,
			Total:      p.Total,
			TotalExact: p.TotalExact,
			NextCursor: p.NextCursor,
		}
	}
	return out, nil
}

// Browse implements experience.PluginLibraries. It reads strictly from the
// pluginitems store and never invokes the plugin runtime.
func (c *KindCatalog) Browse(ctx context.Context, lib core.Library, q frontend.ItemQuery, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	level, levels, anyLevel, matched := browseLevelsForQuery(rec.Decl, lib.Present, q)
	if !matched {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	bq := pluginitems.BrowseQuery{
		NoTotal:          q.NoTotal,
		CheapTotal:       q.CheapTotal,
		Policy:           policy,
		LibraryID:        lib.ID,
		ViewerID:         core.CatalogViewerID(ctx),
		Level:            level,
		Levels:           levels,
		AnyLevel:         anyLevel,
		PersonID:         q.Filter.Person,
		Sort:             mapBrowseSort(q.Sort, q.Desc),
		Facets:           mapFilterToFacets(rec.Decl, q.Filter),
		ExcludeFacets:    mapExcludeGenresToFacets(rec.Decl, q.Filter.ExcludeGenres),
		Watched:          q.Watched,
		MaxContentRating: q.Filter.MaxContentRating,
		ExcludeKeys:      q.Filter.ExcludeKeys,
		Cursor:           cursor,
		Limit:            limit,
	}
	if len(q.Filter.TabRules) > 0 || q.Filter.TabFallback != "" {
		tp, tpErr := c.items.CompilePolicy(ctx, lib.ID, q.Filter.TabRules, q.Filter.TabFallback)
		if tpErr != nil {
			return experience.PluginBrowsePage{}, tpErr
		}
		bq.TabPolicy = tp
	}
	page, err := c.items.Browse(ctx, bq)
	if err != nil {
		return experience.PluginBrowsePage{}, err
	}
	titles := c.projectItems(ctx, rec, lib, page.Items)
	if flatAudioQuery(rec.Decl, lib.Present, q) {
		for i, item := range page.Items {
			if item.Playable && item.Unavailable == nil {
				titles[i] = flatAlbumIncarnation(titles[i])
			}
		}
	} else {
		c.rememberBrowseViews(ctx, rec, lib, page.Items, titles)
	}
	return experience.PluginBrowsePage{
		Items:      titles,
		Total:      page.Total,
		TotalExact: page.TotalExact,
		NextCursor: page.NextCursor,
	}, nil
}

// Item implements experience.PluginLibraries.
func (c *KindCatalog) Item(ctx context.Context, key core.RatingKey) (experience.PluginItemView, error) {
	if c == nil || c.items == nil || c.libs == nil || !key.IsPluginKey() {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	if parentKey, partIdx, isPart := key.PluginPart(); isPart {
		return c.syntheticPartItemView(ctx, parentKey, partIdx)
	}
	item, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		if part, parentItem, partLibID, perr := c.items.GetPartByKey(ctx, key); perr == nil {
			return c.projectPartView(ctx, part, parentItem, partLibID)
		}
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
	c.items.PinItemAsync(item.Ref)
	if !core.NoContainerExpand(ctx) && rec.Decl.Browse.Expand.ExpandsLevel(item.Level) {
		didExpand, timedOut := c.maybeExpandContainer(ctx, rec, lib, item)
		if timedOut {
			core.ReportDetailPending(ctx, key, "children")
		}
		if didExpand {
			if refreshed, _, gerr := c.items.GetByKey(ctx, key); gerr == nil {
				item = refreshed
			}
		}
	}
	titles := c.projectItems(ctx, rec, lib, []core.KindItem{item})
	parts := syntheticAudiobookParts(item, titles[0])
	if len(parts) == 0 && item.Playable && item.Unavailable == nil {
		if stored, serr := c.items.ListParts(ctx, item.Ref); serr == nil && len(stored) > 1 {
			parts = make([]core.KindItemPart, len(stored))
			for i, sp := range stored {
				parts[i] = sp.KindItemPart()
			}
		}
	}
	return experience.PluginItemView{
		Library:      lib,
		Title:        titles[0],
		Images:       projectImages(item.Art, lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape),
		Chapters:     extractItemDetailChapters(item, parts),
		Unavailable:  item.Unavailable,
		Playable:     item.Unavailable == nil && item.Playable && levelIsPlayable(rec.Decl, item.Level),
		SkipContinue: shouldSkipItemContinue(rec.Decl, lib.Present, item.Level),
	}, nil
}

func (c *KindCatalog) projectPartView(ctx context.Context, part pluginitems.StoredPart, parentItem core.KindItem, libID core.LibraryID) (experience.PluginItemView, error) {
	lib, err := c.libs.Get(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	hidden, e := c.items.ItemHidden(core.WithExplicitSearch(ctx), lib, parentItem.Ref)
	if e != nil || hidden {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	c.items.PinItemAsync(parentItem.Ref)
	parentTitles := c.projectItems(ctx, rec, lib, []core.KindItem{parentItem})
	landscape := lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape
	return experience.PluginItemView{
		Library:      lib,
		Title:        projectPartTitle(rec, lib, parentTitles[0], part),
		Images:       projectImages(parentItem.Art, landscape),
		Unavailable:  parentItem.Unavailable,
		Playable:     parentItem.Unavailable == nil && parentItem.Playable && levelIsPlayable(rec.Decl, parentItem.Level),
		SkipContinue: shouldSkipItemContinue(rec.Decl, lib.Present, parentItem.Level),
	}, nil
}

func projectPartTitle(rec pluginitems.KindRecord, lib core.Library, parent core.Title, part pluginitems.StoredPart) core.Title {
	title := part.Name
	if title == "" {
		title = fmt.Sprintf("Part %d", part.PartIndex+1)
	}
	mediaClass := parent.MediaClass
	if mediaClass == "" {
		mediaClass = core.MediaClass(rec.Decl.Media)
	}
	parents := make([]core.TitleParent, 0, len(parent.MediaParents)+1)
	parents = append(parents, core.TitleParent{
		Key:   parent.Key,
		Title: parent.Title,
		Role:  parent.MediaRole,
	})
	parents = append(parents, parent.MediaParents...)

	mbType := parent.MBItemType
	switch mediaClass {
	case core.MediaClassAudiobook:
		mbType = "AudioBook"
	case core.MediaClassMusic, core.MediaClassPodcast:
		mbType = "Audio"
	}

	out := parent
	out.Key = part.Key
	out.Title = title
	out.SortTitle = title
	out.LibraryID = lib.ID
	out.ParentKey = parent.Key
	out.ParentTitle = parent.Title
	out.ItemIndex = part.PartIndex + 1
	out.ChildCount = 0
	out.LeafCount = 0
	out.MediaClass = mediaClass
	out.MediaRole = core.MediaRolePart
	out.MediaParents = parents
	out.MBItemType = mbType
	if part.DurationSec > 0 {
		out.Runtime = time.Duration(part.DurationSec * float64(time.Second))
	} else {
		out.Runtime = 0
	}
	return out
}

func (c *KindCatalog) syntheticPartItemView(ctx context.Context, parentKey core.RatingKey, partIdx int) (experience.PluginItemView, error) {
	item, libID, err := c.items.GetByKey(ctx, parentKey)
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
	c.items.PinItemAsync(item.Ref)
	titles := c.projectItems(ctx, rec, lib, []core.KindItem{item})
	parts := syntheticAudiobookParts(item, titles[0])
	if partIdx < 0 || partIdx >= len(parts) {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	partTitle, ok := buildSyntheticPartTitle(titles[0], parts[partIdx])
	if !ok {
		return experience.PluginItemView{}, core.ErrNotFound
	}
	return experience.PluginItemView{
		Library:      lib,
		Title:        partTitle,
		Images:       projectImages(item.Art, lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape),
		Playable:     item.Unavailable == nil && item.Playable && levelIsPlayable(rec.Decl, item.Level),
		SkipContinue: shouldSkipItemContinue(rec.Decl, lib.Present, item.Level),
		Unavailable:  item.Unavailable,
	}, nil
}

// ItemsMany implements experience.PluginLibraries using batched item, artwork,
// and visibility queries per library rather than one query per key.
func (c *KindCatalog) ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]experience.PluginItemView, error) {
	out := make(map[core.RatingKey]experience.PluginItemView, len(keys))
	if c == nil || c.items == nil || c.libs == nil || len(keys) == 0 {
		return out, nil
	}
	type partReq struct {
		key       core.RatingKey
		parentKey core.RatingKey
		partIdx   int
	}
	pivCache := c.pivMemo(ctx)
	fetchKeys := make([]core.RatingKey, 0, len(keys))
	seenFetch := make(map[core.RatingKey]bool, len(keys))
	wantDirect := make(map[core.RatingKey]bool, len(keys))
	partParents := make(map[core.RatingKey]bool)
	var partReqs []partReq
	for _, k := range keys {
		if !k.IsPluginKey() {
			continue
		}
		if parentKey, partIdx, isPart := k.PluginPart(); isPart {
			partReqs = append(partReqs, partReq{key: k, parentKey: parentKey, partIdx: partIdx})
			partParents[parentKey] = true
			if !seenFetch[parentKey] {
				seenFetch[parentKey] = true
				fetchKeys = append(fetchKeys, parentKey)
			}
		}
	}
	if pivCache != nil {
		pivCache.mu.RLock()
		for _, k := range keys {
			if !k.IsPluginKey() {
				continue
			}
			if _, _, isPart := k.PluginPart(); isPart {
				continue
			}
			if !partParents[k] {
				if cached, ok := pivCache.items[k]; ok {
					out[k] = cached
					continue
				}
			}
			wantDirect[k] = true
			if !seenFetch[k] {
				seenFetch[k] = true
				fetchKeys = append(fetchKeys, k)
			}
		}
		pivCache.mu.RUnlock()
	} else {
		for _, k := range keys {
			if !k.IsPluginKey() {
				continue
			}
			if _, _, isPart := k.PluginPart(); isPart {
				continue
			}
			wantDirect[k] = true
			if !seenFetch[k] {
				seenFetch[k] = true
				fetchKeys = append(fetchKeys, k)
			}
		}
	}
	if len(fetchKeys) == 0 {
		return out, nil
	}
	keyed, err := c.items.GetByKeys(ctx, fetchKeys)
	if err != nil {
		return out, err
	}
	byLib := make(map[core.LibraryID][]core.KindItem)
	var libOrder []core.LibraryID
	seenKey := make(map[core.RatingKey]bool, len(keyed))
	var missing []core.RatingKey
	for _, key := range fetchKeys {
		if seenKey[key] {
			continue
		}
		ki, ok := keyed[key]
		if !ok {
			if wantDirect[key] {
				missing = append(missing, key)
			}
			continue
		}
		seenKey[key] = true
		if _, exists := byLib[ki.LibraryID]; !exists {
			libOrder = append(libOrder, ki.LibraryID)
		}
		byLib[ki.LibraryID] = append(byLib[ki.LibraryID], ki.Item)
	}
	if len(missing) > 0 {
		if parts, perr := c.items.GetPartsByKeys(ctx, missing); perr == nil && len(parts) > 0 {
			for k, part := range parts {
				if parentItem, libID, gerr := c.items.GetByRef(ctx, part.ItemRef); gerr == nil {
					if view, verr := c.projectPartView(ctx, part, parentItem, libID); verr == nil {
						out[k] = view
					}
				}
			}
		}
	}

	type projectedParent struct {
		item  core.KindItem
		view  experience.PluginItemView
		parts []core.KindItemPart
	}
	var parents map[core.RatingKey]projectedParent
	if len(partReqs) > 0 {
		parents = make(map[core.RatingKey]projectedParent, len(partReqs))
	}

	visCtx := core.WithExplicitSearch(ctx)
	for _, libID := range libOrder {
		lib, err := c.libs.Get(ctx, libID)
		if err != nil {
			continue
		}
		rec, err := c.items.GetKindByLibrary(ctx, libID)
		if err != nil {
			continue
		}
		candidates := byLib[libID]
		refs := make([]core.ItemRef, len(candidates))
		for i, it := range candidates {
			refs[i] = it.Ref
		}
		visibleRefs, err := c.items.FilterVisibleRefs(visCtx, lib, refs)
		if err != nil {
			continue
		}
		visibleItems := make([]core.KindItem, 0, len(candidates))
		for _, it := range candidates {
			if visibleRefs[it.Ref] {
				visibleItems = append(visibleItems, it)
			}
		}
		if len(visibleItems) == 0 {
			continue
		}
		titles := c.projectItems(ctx, rec, lib, visibleItems)
		landscape := lib.EffectiveCardShape(rec.Decl.Library.EffectiveCard()) == core.CardLandscape
		if pivCache != nil {
			pivCache.mu.Lock()
		}
		for i, it := range visibleItems {
			view, parts := c.buildPluginItemView(rec, lib, landscape, it, titles[i])
			if pivCache != nil && it.Key != 0 {
				pivCache.items[it.Key] = view
			}
			if wantDirect[it.Key] {
				out[it.Key] = view
			}
			if parents != nil {
				parents[it.Key] = projectedParent{item: it, view: view, parts: parts}
			}
		}
		if pivCache != nil {
			pivCache.mu.Unlock()
		}
	}
	for _, pr := range partReqs {
		pp, ok := parents[pr.parentKey]
		if !ok || pr.partIdx < 0 || pr.partIdx >= len(pp.parts) {
			continue
		}
		partTitle, ok := buildSyntheticPartTitle(pp.view.Title, pp.parts[pr.partIdx])
		if !ok {
			continue
		}
		out[pr.key] = experience.PluginItemView{
			Library:      pp.view.Library,
			Title:        partTitle,
			Images:       pp.view.Images,
			Playable:     pp.view.Playable,
			SkipContinue: pp.view.SkipContinue,
			Unavailable:  pp.view.Unavailable,
		}
	}
	return out, nil
}

// flatPlayableAlbum reports whether a kind is the single-level playable
// album shape a flat music plugin declares: one playable level typed as a
// container (MediaBrowser MusicAlbum, or Plex base artist). Clients of
// those protocols only ever play leaf items, so such an album is also
// presented once as its own Audio/track child (see flatAlbumIncarnation);
// Jellyfin's album play button and Songs tab, and Plex's album track list,
// all reach the playable item that way.
func flatPlayableAlbum(decl plugins.MediaKindMeta, present core.Presentations) bool {
	if len(decl.Levels) != 1 || !decl.Levels[0].Playable {
		return false
	}
	if present.MediaBrowser != nil && present.MediaBrowser.ItemTypes[decl.Levels[0].Key] == "MusicAlbum" {
		return true
	}
	return present.Plex != nil && present.Plex.Base == "artist"
}

// flatAlbumIncarnation is a flat playable album presented as its own single
// playable child: same item and Key, typed as the leaf (Audio), naming
// itself as its album.
func flatAlbumIncarnation(t core.Title) core.Title {
	origRole := t.MediaRole
	if origRole == "" {
		origRole = core.MediaRoleAlbum
	}
	parent := core.TitleParent{Key: t.Key, Title: t.Title, Role: origRole}
	parents := make([]core.TitleParent, 0, len(t.MediaParents)+1)
	parents = append(parents, parent)
	parents = append(parents, t.MediaParents...)
	t.SelfChild = true
	t.MBItemType = "Audio"
	t.MediaRole = core.MediaRoleTrack
	t.MediaParents = parents
	t.ParentKey = t.Key
	t.ParentTitle = t.Title
	t.ChildCount = 0
	t.LeafCount = 0
	return t
}

// wantsMBType reports whether the query's IncludeItemTypes named the given
// MediaBrowser type (carried lowercased in UnservedTypes).
func wantsMBType(q frontend.ItemQuery, name string) bool {
	for _, kind := range q.UnservedTypes {
		if strings.EqualFold(kind, name) {
			return true
		}
	}
	return false
}

// excludesMBType reports whether the query excluded the given MediaBrowser
// type by name.
func excludesMBType(q frontend.ItemQuery, name string) bool {
	for _, excluded := range q.ExcludedTypes {
		if strings.EqualFold(string(excluded), name) {
			return true
		}
	}
	return false
}

// flatAudioQuery reports whether q asks for Audio items a flat playable
// album kind answers with its albums' Audio incarnations (Jellyfin's Songs
// tab), without also asking for the albums themselves.
func flatAudioQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) bool {
	return flatPlayableAlbum(decl, present) && wantsMBType(q, "Audio") &&
		!wantsMBType(q, "MusicAlbum") && !excludesMBType(q, "Audio")
}

// Children implements experience.PluginLibraries.
func hasGrandchildLevels(decl plugins.MediaKindMeta, parentLevel string) bool {
	for _, child := range decl.ChildLevels(parentLevel) {
		if decl.HasChildLevels(child.Key) {
			return true
		}
	}
	return false
}

func resolveFilteredChildLevels(decl plugins.MediaKindMeta, _ core.Presentations, parentLevel string, recursive bool, reqLevel, reqRole string) []string {
	reqLevel = strings.TrimSpace(reqLevel)
	reqRole = strings.TrimSpace(reqRole)
	var candidates []plugins.KindLevel
	if !recursive {
		candidates = decl.ChildLevels(parentLevel)
	} else {
		queue := []string{parentLevel}
		seen := map[string]bool{parentLevel: true}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, ch := range decl.ChildLevels(cur) {
				if seen[ch.Key] {
					continue
				}
				seen[ch.Key] = true
				candidates = append(candidates, ch)
				queue = append(queue, ch.Key)
			}
		}
	}
	if reqLevel != "" {
		filtered := candidates[:0]
		for _, lv := range candidates {
			if strings.EqualFold(lv.Key, reqLevel) {
				filtered = append(filtered, lv)
			}
		}
		candidates = filtered
	}
	if reqRole == "" {
		out := make([]string, 0, len(candidates))
		for _, lv := range candidates {
			out = append(out, lv.Key)
		}
		return out
	}
	// Spec: match a child level's declared role first, then a role-less child
	// level by its level key so every media.childCounts key works with ?role=.
	var byRole []string
	for _, lv := range candidates {
		if declRole := strings.TrimSpace(lv.Role); declRole != "" && strings.EqualFold(declRole, reqRole) {
			byRole = append(byRole, lv.Key)
		}
	}
	if len(byRole) > 0 {
		return byRole
	}
	var byKey []string
	for _, lv := range candidates {
		if strings.TrimSpace(lv.Role) == "" && strings.EqualFold(lv.Key, reqRole) {
			byKey = append(byKey, lv.Key)
		}
	}
	return byKey
}

func resolveChildLevelsForQuery(decl plugins.MediaKindMeta, present core.Presentations, parentLevel string, q frontend.ItemQuery) (levels []string, filtering bool, allDirect bool) {
	reqLevel := strings.TrimSpace(q.Level)
	reqRole := strings.TrimSpace(q.Role)
	hasTypeFilter := len(q.Types) > 0 || len(q.UnservedTypes) > 0 || len(q.ExcludedTypes) > 0
	if reqLevel == "" && reqRole == "" && (!hasTypeFilter || !decl.HasChildLevels(parentLevel)) {
		return nil, false, false
	}
	levelKeys := resolveFilteredChildLevels(decl, present, parentLevel, q.Recursive, reqLevel, reqRole)
	if hasTypeFilter {
		matched := levelKeys[:0]
		for _, lk := range levelKeys {
			lv, idx, ok := decl.Level(lk)
			if !ok {
				continue
			}
			projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
			if len(q.Types) > 0 || len(q.UnservedTypes) > 0 {
				if !levelMatchesQueryTypes(lv, idx, decl, present, projected, q) {
					continue
				}
			} else if levelExcludedByQuery(lv, idx, decl, present, projected, q) {
				continue
			}
			matched = append(matched, lv.Key)
		}
		levelKeys = matched
	}
	if len(levelKeys) == 0 {
		return nil, true, false
	}
	direct := true
	for _, lk := range levelKeys {
		if decl.LevelParent(lk) != parentLevel {
			direct = false
			break
		}
	}
	return levelKeys, true, direct
}

func (c *KindCatalog) Children(ctx context.Context, parent core.RatingKey, recursive bool, cursor string, limit int) (experience.PluginBrowsePage, error) {
	return c.ChildrenQuery(ctx, parent, frontend.ItemQuery{Recursive: recursive}, cursor, limit)
}

// ChildrenFiltered implements experience.PluginFilteredChildrenLibraries,
// pushing optional child level and semantic role filters directly into SQL so
// filtered child paging stays O(page).
func (c *KindCatalog) ChildrenFiltered(ctx context.Context, parent core.RatingKey, recursive bool, reqLevel, reqRole, cursor string, limit int) (experience.PluginBrowsePage, error) {
	return c.ChildrenQuery(ctx, parent, frontend.ItemQuery{
		Recursive: recursive,
		Level:     reqLevel,
		Role:      reqRole,
	}, cursor, limit)
}

// ChildrenQuery implements experience.PluginQueryChildrenLibraries, pushing
// optional child level, role, and media-type filters directly into SQL so
// filtered and typed child paging stays O(page).
func (c *KindCatalog) ChildrenQuery(ctx context.Context, parent core.RatingKey, q frontend.ItemQuery, cursor string, limit int) (experience.PluginBrowsePage, error) {
	if c == nil || c.items == nil || c.libs == nil || !parent.IsPluginKey() {
		return experience.PluginBrowsePage{}, core.ErrNotFound
	}
	if parent.IsPluginPartKey() {
		return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
	}
	parentItem, libID, err := c.items.GetByKey(ctx, parent)
	if err != nil {
		if _, _, _, perr := c.items.GetPartByKey(ctx, parent); perr == nil {
			return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
		}
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
	_, parentRole := pluginitems.ResolveMediaClassAndRole(parentItem.Level, rec.Decl, lib.Present)
	if cursor == "" && !core.NoContainerExpand(ctx) {
		if rec.Decl.Browse.Expand.ExpandsLevel(parentItem.Level) {
			if _, timedOut := c.maybeExpandContainer(ctx, rec, lib, parentItem); timedOut {
				core.ReportDetailPending(ctx, parent, "children")
			}
		} else {
			c.maybeHydrateContainer(ctx, rec, lib, parentItem, levelIdx)
		}
	}

	recursive := q.Recursive
	targetLevels, filtering, allDirect := resolveChildLevelsForQuery(rec.Decl, lib.Present, parentItem.Level, q)
	var targetLevelSet map[string]bool
	if filtering {
		if len(targetLevels) == 0 {
			return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
		}
		targetLevelSet = make(map[string]bool, len(targetLevels))
		for _, lv := range targetLevels {
			targetLevelSet[lv] = true
		}
	}

	if !recursive && !filtering && parentRole == core.MediaRoleArtist {
		policy, e := c.items.LibraryPolicy(ctx, lib)
		if e != nil {
			return experience.PluginBrowsePage{}, e
		}
		var all []core.KindItem
		const maxArtistReleases = 1000
		childCursor := ""
		for len(all) < maxArtistReleases {
			page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{NoTotal: true, LibraryID: lib.ID, ParentRef: parentItem.Ref, Cursor: childCursor, Limit: 200, Policy: policy, Sort: "index"})
			if err != nil {
				return experience.PluginBrowsePage{}, err
			}
			all = append(all, page.Items...)
			if page.NextCursor == "" || len(page.Items) == 0 {
				break
			}
			childCursor = page.NextCursor
		}
		if len(all) > maxArtistReleases {
			all = all[:maxArtistReleases]
		}
		sortArtistReleaseItems(all)
		offset := 0
		if cursor != "" {
			if n, err := strconv.Atoi(cursor); err == nil && n >= 0 {
				offset = n
			}
		}
		if limit <= 0 {
			limit = 200
		}
		start := min(offset, len(all))
		end := min(start+limit, len(all))
		next := ""
		if end < len(all) {
			next = strconv.Itoa(end)
		}
		return experience.PluginBrowsePage{
			Items:      c.projectItems(ctx, rec, lib, all[start:end]),
			Total:      len(all),
			TotalExact: true,
			NextCursor: next,
		}, nil
	}
	if !recursive || !hasGrandchildLevels(rec.Decl, parentItem.Level) || (filtering && allDirect) {
		if strings.HasPrefix(cursor, "part:") && !filtering {
			self := c.projectItems(ctx, rec, lib, []core.KindItem{parentItem})
			if parts := syntheticAudiobookParts(parentItem, self[0]); len(parts) > 1 {
				return paginateSyntheticParts(self[0], parts, cursor, limit), nil
			}
			return experience.PluginBrowsePage{Items: []core.Title{}, Total: 0, TotalExact: true}, nil
		}
		policy, e := c.items.LibraryPolicy(ctx, lib)
		if e != nil {
			return experience.PluginBrowsePage{}, e
		}
		page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{LibraryID: lib.ID, ParentRef: parentItem.Ref, Levels: targetLevels, Cursor: cursor, Limit: limit, Policy: policy, Sort: "index"})
		if err != nil {
			return experience.PluginBrowsePage{}, err
		}
		if len(page.Items) == 0 && page.Total == 0 && !filtering {
			self := c.projectItems(ctx, rec, lib, []core.KindItem{parentItem})
			if parts := syntheticAudiobookParts(parentItem, self[0]); len(parts) > 1 {
				return paginateSyntheticParts(self[0], parts, cursor, limit), nil
			}
			if cursor == "" && limit != 0 && parentItem.Playable && parentItem.Unavailable == nil {
				if storedParts, perr := c.items.ListParts(ctx, parentItem.Ref); perr == nil && len(storedParts) > 0 && len(self) > 0 {
					partTitles := make([]core.Title, 0, len(storedParts))
					for _, part := range storedParts {
						partTitles = append(partTitles, projectPartTitle(rec, lib, self[0], part))
						if limit > 0 && len(partTitles) >= limit {
							break
						}
					}
					return experience.PluginBrowsePage{
						Items:      partTitles,
						Total:      len(storedParts),
						TotalExact: true,
					}, nil
				}
				if flatPlayableAlbum(rec.Decl, lib.Present) {
					// A flat playable album has no track items to list; present
					// the album itself once as its playable child.
					return experience.PluginBrowsePage{Items: []core.Title{flatAlbumIncarnation(self[0])}, Total: 1, TotalExact: true}, nil
				}
			}
		}
		return experience.PluginBrowsePage{
			Items:      c.projectItems(ctx, rec, lib, page.Items),
			Total:      page.Total,
			TotalExact: page.TotalExact,
			NextCursor: page.NextCursor,
		}, nil
	}

	// Recursive multi-level expansion (e.g. root -> intermediate -> leaf):
	// walk descendants breadth-first from the store with bounded pages.
	kindMedia := ResolveKindMediaClass(rec.Decl, lib.Present)
	parentMedia := rec.Decl.EffectiveLevelMedia(parentItem.Level, kindMedia)
	var leaves []core.KindItem
	queue := []core.ItemRef{parentItem.Ref}
	const maxExpanded = 2000
	for len(queue) > 0 && len(leaves) < maxExpanded {
		cur := queue[0]
		queue = queue[1:]
		childCursor := ""
		for {
			policy, e := c.items.LibraryPolicy(ctx, lib)
			if e != nil {
				return experience.PluginBrowsePage{}, e
			}
			page, err := c.items.Browse(ctx, pluginitems.BrowseQuery{NoTotal: true, LibraryID: lib.ID, ParentRef: cur, Cursor: childCursor, Limit: 200, Policy: policy, Sort: "index"})
			if err != nil {
				return experience.PluginBrowsePage{}, err
			}
			for _, it := range page.Items {
				if !filtering && rec.Decl.EffectiveLevelMedia(it.Level, kindMedia) != parentMedia {
					// Unfiltered recursive leaf expansion stays within the
					// container's own media class (e.g. artist Play, Shuffle,
					// playlist, and scrobble stay on audio and exclude video
					// concerts).
					continue
				}
				isContainer := rec.Decl.HasChildLevels(it.Level) && !it.Playable
				if isContainer {
					queue = append(queue, it.Ref)
				}
				if filtering {
					if targetLevelSet[it.Level] {
						leaves = append(leaves, it)
						if len(leaves) >= maxExpanded {
							break
						}
					}
				} else if !isContainer {
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
	if len(leaves) == 0 && !filtering {
		self := c.projectItems(ctx, rec, lib, []core.KindItem{parentItem})
		if parts := syntheticAudiobookParts(parentItem, self[0]); len(parts) > 1 {
			return paginateSyntheticParts(self[0], parts, cursor, limit), nil
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
	if parentKey, _, isPart := key.PluginPart(); isPart {
		anc, lib, err := c.Ancestors(ctx, parentKey)
		if err != nil {
			return nil, core.Library{}, err
		}
		piv, err := c.Item(core.WithNoContainerExpand(ctx), parentKey)
		if err != nil {
			return nil, core.Library{}, err
		}
		return append(anc, piv.Title), lib, nil
	}
	cur, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		if _, parentItem, _, perr := c.items.GetPartByKey(ctx, key); perr == nil {
			anc, lib, aerr := c.Ancestors(ctx, parentItem.Key)
			if aerr != nil {
				return nil, core.Library{}, aerr
			}
			piv, ierr := c.Item(core.WithNoContainerExpand(ctx), parentItem.Key)
			if ierr != nil {
				return nil, core.Library{}, ierr
			}
			return append(anc, piv.Title), lib, nil
		}
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
	return c.FacetsAtLevel(ctx, lib, "", facet)
}

// FacetsAtLevel implements experience.PluginLevelFacetLibraries. level may be
// empty (defaults to the kind's root level), an exact declared level key, or a
// protocol item type/role (for example "album", "track", "MusicAlbum", "Audio").
func (c *KindCatalog) FacetsAtLevel(ctx context.Context, lib core.Library, level, facet string) ([]core.FacetValue, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	targetLevel, ok := resolveProtocolLevel(rec.Decl, lib.Present, level)
	if !ok {
		return []core.FacetValue{}, nil
	}
	if facet == "person" {
		policy, err := c.items.LibraryPolicy(ctx, lib)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(level) != "" && targetLevel != "" {
			var entries []core.FacetValue
			seen := make(map[int64]bool)
			for _, fd := range rec.Decl.Fields {
				if fd.Type != "people" {
					continue
				}
				page, pErr := c.items.FacetPeople(ctx, pluginitems.FacetPeopleQuery{
					LibraryID: lib.ID,
					Level:     targetLevel,
					Policy:    policy,
					FieldKey:  fd.Key,
					Order:     "name",
					Limit:     500,
				})
				if pErr != nil {
					return nil, pErr
				}
				for _, p := range page.Items {
					if !core.IsPluginPersonID(p.PersonID) || strings.TrimSpace(p.Name) == "" || seen[p.PersonID] {
						continue
					}
					seen[p.PersonID] = true
					keyStr := strconv.FormatInt(p.PersonID, 10)
					entries = append(entries, core.FacetValue{
						ID:    core.FacetID("person", keyStr),
						Key:   keyStr,
						Title: strings.TrimSpace(p.Name),
						Count: p.Count,
					})
				}
			}
			sort.Slice(entries, func(i, j int) bool {
				li, lj := strings.ToLower(entries[i].Title), strings.ToLower(entries[j].Title)
				if li != lj {
					return li < lj
				}
				return entries[i].Key < entries[j].Key
			})
			return entries, nil
		}
		type personEntry struct {
			id   int64
			name string
		}
		var entries []personEntry
		_, _, err = c.items.FindPersonID(ctx, lib.ID, policy, func(pid int64, name string) bool {
			if core.IsPluginPersonID(pid) && strings.TrimSpace(name) != "" {
				entries = append(entries, personEntry{id: pid, name: strings.TrimSpace(name)})
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool {
			li, lj := strings.ToLower(entries[i].name), strings.ToLower(entries[j].name)
			if li != lj {
				return li < lj
			}
			return entries[i].id < entries[j].id
		})
		out := make([]core.FacetValue, 0, len(entries))
		for _, e := range entries {
			keyStr := strconv.FormatInt(e.id, 10)
			out = append(out, core.FacetValue{
				ID:    core.FacetID("person", keyStr),
				Key:   keyStr,
				Title: e.name,
				Count: 1,
			})
		}
		return out, nil
	}
	fields := resolveFacetFields(rec.Decl, facet)
	if len(fields) == 0 {
		return []core.FacetValue{}, nil
	}
	merged := make(map[string]int)
	for _, fieldKey := range fields {
		page, err := c.items.Facets(ctx, lib.ID, targetLevel, fieldKey, 500)
		if err != nil {
			return nil, err
		}
		for _, fc := range page.Values {
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
	if q.Filter.Person != 0 && !core.IsPluginPersonID(q.Filter.Person) {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	searchLevels, ok := searchLevelsForQuery(rec.Decl, lib.Present, q)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	visCtx := core.WithExplicitSearch(ctx)
	policy, err := c.items.LibraryPolicy(visCtx, lib)
	if err != nil {
		return nil, err
	}
	facets := mapFilterToFacets(rec.Decl, q.Filter)
	sq := pluginitems.SearchQuery{
		NoTotal:   true,
		Policy:    policy,
		LibraryID: lib.ID,
		ViewerID:  core.CatalogViewerID(ctx),
		Query:     query,
		Levels:    searchLevels,
		PersonID:  q.Filter.Person,
		Facets:    facets,
		Limit:     limit,
	}
	if len(q.Filter.TabRules) > 0 || q.Filter.TabFallback != "" {
		tp, tpErr := c.items.CompilePolicy(visCtx, lib.ID, q.Filter.TabRules, q.Filter.TabFallback)
		if tpErr != nil {
			return nil, tpErr
		}
		sq.TabPolicy = tp.ForSearch()
	}
	localPage, err := c.items.Search(ctx, sq)
	if err != nil {
		return nil, err
	}
	combined := localPage.Items
	if rec.Decl.Browse.LiveSearch && c.mgr != nil && c.IsPluginLibraryActive(ctx, lib) {
		liveItems := c.runLiveSearch(ctx, rec, query, limit)
		if len(liveItems) > 0 {
			if len(facets) > 0 || sq.TabPolicy != nil {
				if refreshed, rErr := c.items.Search(ctx, sq); rErr == nil {
					combined = refreshed.Items
				}
			} else {
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
	}
	refs := make([]core.ItemRef, len(combined))
	for i, it := range combined {
		refs[i] = it.Ref
	}
	visibleRefs, err := c.items.FilterVisibleRefs(visCtx, lib, refs)
	if err != nil {
		return nil, err
	}
	allowed := combined[:0]
	for _, it := range combined {
		if visibleRefs[it.Ref] {
			allowed = append(allowed, it)
		}
	}
	projected := c.projectItems(ctx, rec, lib, allowed)
	if flatAudioQuery(rec.Decl, lib.Present, q) {
		for i, item := range allowed {
			if item.Playable && item.Unavailable == nil {
				projected[i] = flatAlbumIncarnation(projected[i])
			}
		}
	}
	for i := range projected {
		projected[i].SearchScore = scorePluginSearchHit(projected[i], allowed[i], query)
	}
	sort.SliceStable(projected, func(i, j int) bool {
		if projected[i].SearchScore != projected[j].SearchScore {
			return projected[i].SearchScore > projected[j].SearchScore
		}
		return projected[i].Popularity > projected[j].Popularity
	})
	return projected, nil
}

// SearchPeople implements experience.PluginPeopleLibraries.
func (c *KindCatalog) SearchPeople(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, personLimit, itemLimit int) ([]experience.PluginPersonMatch, error) {
	if c == nil || c.items == nil {
		return nil, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	allowedLevels, ok := searchLevelsForQuery(rec.Decl, lib.Present, q)
	if !ok {
		return nil, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return nil, err
	}
	if itemLimit <= 0 {
		itemLimit = 25
	}
	matches, err := c.items.SearchPeople(ctx, lib.ID, policy, query, personLimit, itemLimit)
	if err != nil || len(matches) == 0 {
		return nil, nil
	}
	out := make([]experience.PluginPersonMatch, 0, len(matches))
	for _, m := range matches {
		titles := c.projectItems(ctx, rec, lib, m.Items)
		if len(allowedLevels) > 0 {
			filtered := titles[:0]
			for _, t := range titles {
				if slices.Contains(allowedLevels, t.KindLevel) {
					filtered = append(filtered, t)
				}
			}
			titles = filtered
		}
		if len(titles) == 0 {
			continue
		}
		out = append(out, experience.PluginPersonMatch{
			PersonID:    m.ID,
			Name:        m.Name,
			CreditCount: m.CreditCount,
			ArtworkItem: m.SampleItem.Key,
			HasThumb:    m.Thumb != "",
			Roles:       m.Roles,
			Titles:      titles,
		})
	}
	return out, nil
}

// PersonByID implements experience.PluginPeopleLibraries.
func (c *KindCatalog) PersonByID(ctx context.Context, lib core.Library, personID int64, itemLimit int) (experience.PluginPersonMatch, bool, error) {
	if c == nil || c.items == nil || !core.IsPluginPersonID(personID) {
		return experience.PluginPersonMatch{}, false, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginPersonMatch{}, false, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return experience.PluginPersonMatch{}, false, err
	}
	if itemLimit <= 0 {
		itemLimit = 50
	}
	m, ok, err := c.items.PersonByID(ctx, lib.ID, policy, personID, itemLimit)
	if err != nil || !ok {
		return experience.PluginPersonMatch{}, false, err
	}
	titles := c.projectItems(ctx, rec, lib, m.Items)
	return experience.PluginPersonMatch{
		PersonID:    m.ID,
		Name:        m.Name,
		CreditCount: m.CreditCount,
		ArtworkItem: m.SampleItem.Key,
		HasThumb:    m.Thumb != "",
		Roles:       m.Roles,
		Titles:      titles,
	}, true, nil
}

// FindPersonID implements experience.PluginPeopleLibraries.
func (c *KindCatalog) FindPersonID(ctx context.Context, lib core.Library, match func(personID int64, name string) bool) (int64, bool, error) {
	if c == nil || c.items == nil || match == nil {
		return 0, false, nil
	}
	policy, err := c.items.LibraryPolicy(core.WithExplicitSearch(ctx), lib)
	if err != nil {
		return 0, false, err
	}
	return c.items.FindPersonID(ctx, lib.ID, policy, match)
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

// MembersOf is MemberOf for a batch of keys in one set-based query.
func (c *KindCatalog) MembersOf(ctx context.Context, lib core.Library, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	if c == nil || c.libs == nil || len(keys) == 0 {
		return nil, nil
	}
	if c.titlesInLibrary != nil {
		return c.titlesInLibrary(ctx, lib, keys)
	}
	out := make(map[core.RatingKey]bool, len(keys))
	for _, k := range keys {
		ok, err := c.MemberOf(ctx, lib, k)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = true
		}
	}
	return out, nil
}

func (c *KindCatalog) runLiveSearch(ctx context.Context, rec pluginitems.KindRecord, query string, limit int) []core.KindItem {
	sfKey := rec.PluginID + "\x00" + rec.KindKey + "\x00" + strings.ToLower(strings.TrimSpace(query))
	aiSearch := !c.customLiveTimeout && c.mgr != nil && c.mgr.AISearchGranted(ctx, rec.PluginID)
	viewerID := core.CatalogViewerID(ctx)

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
		go c.execLiveSearch(sfKey, sem, rec, query, limit, viewerID, aiSearch, call)
	}
	c.liveMu.Unlock()

	budget := c.liveSearchTimeout
	if aiSearch {
		budget = plugins.MaxAISearchTimeout
	}
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

func (c *KindCatalog) execLiveSearch(sfKey string, sem chan struct{}, rec pluginitems.KindRecord, query string, limit int, viewerID int64, aiSearch bool, call *liveSearchCall) {
	defer func() {
		close(call.done)
		c.liveMu.Lock()
		delete(c.liveCalls, sfKey)
		c.liveMu.Unlock()
	}()

	bgWait := liveSearchBackgroundWait
	if aiSearch {
		bgWait = plugins.AISearchRouteDeadline
	}
	bgCtx, cancel := context.WithTimeout(c.syncCtx, bgWait)
	defer cancel()
	if viewerID > 0 {
		bgCtx = plugins.WithViewer(bgCtx, viewerID)
	}

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
	if _, err := c.items.UpsertPinnedItems(bgCtx, rec.PluginID, rec.KindKey, page.Items); err != nil {
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
	var resolveText pluginitems.TextResolver
	if c != nil && c.mgr != nil {
		resolveText = c.mgr.ResolveTextFunc(ctx, rec.PluginID, "")
	}
	type cachedParent struct {
		title    core.Title
		parentID string
		ok       bool
	}
	itemCache := make(map[string]cachedParent)
	lookupItem := func(extID string) cachedParent {
		if extID == "" {
			return cachedParent{}
		}
		if entry, ok := itemCache[extID]; ok {
			return entry
		}
		p, pLib, err := c.items.GetByExternal(ctx, rec.PluginID, rec.KindKey, extID)
		if err != nil || pLib != lib.ID {
			itemCache[extID] = cachedParent{}
			return cachedParent{}
		}
		pt := pluginitems.ProjectWithText(p, rec.Decl, lib.Present, resolveText, lib.CardShape)
		pt.LibraryID = lib.ID
		scopeTitleMediaParents(&pt, lib.ID)
		if pt.ContentRating == "" {
			pt.ContentRating = extractContentRating(p.Fields)
		}
		entry := cachedParent{title: pt, parentID: p.ParentID, ok: true}
		itemCache[extID] = entry
		return entry
	}

	// resolveChain returns ancestors ordered nearest-first (up to 4 hops) and
	// the root ancestor title.
	resolveChain := func(parentID string) (parents []core.TitleParent, immediate core.Title, root core.Title, ok bool) {
		curID := parentID
		seen := make(map[string]bool, 4)
		for depth := 0; depth < 4 && curID != ""; depth++ {
			if seen[curID] {
				break
			}
			seen[curID] = true
			entry := lookupItem(curID)
			if !entry.ok {
				break
			}
			if depth == 0 {
				immediate = entry.title
			}
			root = entry.title
			ok = true
			role := entry.title.MediaRole
			if role == "" {
				role = core.MediaRoleCollection
			}
			parents = append(parents, core.TitleParent{
				Key:   entry.title.Key,
				Title: entry.title.Title,
				Role:  role,
			})
			curID = entry.parentID
		}
		return parents, immediate, root, ok
	}

	flatAlbum := flatPlayableAlbum(rec.Decl, lib.Present)
	refs := make([]core.ItemRef, len(items))
	for i, it := range items {
		refs[i] = it.Ref
	}
	partCounts, _ := c.items.CountPartsMany(ctx, refs)
	var parentCounts map[core.ItemRef]pluginitems.ParentCounts
	if len(rec.Decl.Levels) >= 2 {
		containerRefs := make([]core.ItemRef, 0, len(items))
		for _, it := range items {
			if rec.Decl.HasChildLevels(it.Level) {
				containerRefs = append(containerRefs, it.Ref)
			}
		}
		if len(containerRefs) > 0 {
			parentCounts, _ = c.items.ParentCountsMany(ctx, lib.ID, containerRefs)
		}
	}
	out := make([]core.Title, len(items))
	for i, it := range items {
		t := pluginitems.ProjectWithText(it, rec.Decl, lib.Present, resolveText, lib.CardShape)
		t.LibraryID = lib.ID
		scopeTitleMediaParents(&t, lib.ID)
		if len(rec.Decl.Levels) >= 2 && rec.Decl.HasChildLevels(it.Level) {
			// Direct children and per-level counts for tallied parent levels come
			// strictly from plugin_item_parent_tally, ignoring any plugin-supplied
			// item.ChildCount while preserving LeafCount when higher.
			t.ChildCount = 0
			t.ChildCounts = nil
			if pc, ok := parentCounts[it.Ref]; ok {
				t.ChildCount = pc.ChildCount
				if pc.LeafCount > t.LeafCount {
					t.LeafCount = pc.LeafCount
				}
				if len(pc.ByLevel) > 0 {
					counts := make(map[string]int, len(pc.ByLevel))
					for childLevel, n := range pc.ByLevel {
						if n <= 0 {
							continue
						}
						key := childLevel
						if clv, _, ok := rec.Decl.Level(childLevel); ok && strings.TrimSpace(clv.Role) != "" {
							key = strings.TrimSpace(clv.Role)
						}
						counts[key] += n
					}
					if len(counts) > 0 {
						t.ChildCounts = counts
					}
				}
			}
		}
		if nParts := partCounts[it.Ref]; nParts > 1 && t.ChildCount == 0 {
			t.ChildCount = nParts
			if t.LeafCount == 0 {
				t.LeafCount = nParts
			}
		} else if len(core.ExtractKindItemPartsFromItem(it)) > 1 && t.ChildCount == 0 {
			nParts := len(core.ExtractKindItemPartsFromItem(it))
			t.ChildCount = nParts
			if t.LeafCount == 0 {
				t.LeafCount = nParts
			}
		}
		if flatAlbum && it.Playable && it.Unavailable == nil {
			// The album lists its one synthetic playable child.
			if t.ChildCount == 0 {
				t.ChildCount = 1
			}
			if t.LeafCount == 0 {
				t.LeafCount = 1
			}
		}
		if parts := syntheticAudiobookParts(it, t); len(parts) > 1 {
			if t.ChildCount == 0 {
				t.ChildCount = len(parts)
			}
			if t.LeafCount == 0 {
				t.LeafCount = len(parts)
			}
			if t.Runtime == 0 {
				var sum time.Duration
				for _, p := range parts {
					if p.DurationSec > 0 {
						sum += time.Duration(p.DurationSec * float64(time.Second))
					}
				}
				if sum > 0 {
					t.Runtime = sum
				}
			}
		}
		if t.ContentRating == "" {
			t.ContentRating = extractContentRating(it.Fields)
		}
		if it.ParentID != "" {
			if parents, parent, root, ok := resolveChain(it.ParentID); ok {
				t.ParentKey = parent.Key
				t.ParentTitle = parent.Title
				hasSeries := false
				for _, p := range parents {
					if p.Role == core.MediaRoleSeries {
						hasSeries = true
						break
					}
				}
				if !hasSeries && len(t.MediaParents) > 0 {
					merged := make([]core.TitleParent, 0, len(parents)+len(t.MediaParents))
					merged = append(merged, parents...)
					merged = append(merged, t.MediaParents...)
					t.MediaParents = merged
				} else {
					t.MediaParents = parents
				}
				if t.Type == core.TypeSeason || t.Type == core.TypeEpisode {
					t.ShowKey = root.Key
					t.ShowTitle = root.Title
					t.ShowYear = root.Year
				}
				// An episode under a season container takes that season's
				// number and name, so apps do not label it "Season 0".
				if t.Type == core.TypeEpisode && parent.Type == core.TypeSeason && parent.Key != root.Key {
					if t.Season == 0 {
						t.Season = parent.Season
					}
					if t.SeasonTitle == "" {
						t.SeasonTitle = parent.Title
					}
				}
			}
		}
		dedupeMediaFacts(&t)
		out[i] = t
	}
	return out
}

func scopeTitleMediaParents(t *core.Title, libID core.LibraryID) {
	if t == nil || libID <= 0 || len(t.MediaParents) == 0 {
		return
	}
	prefix := "facet:" + strconv.FormatInt(int64(libID), 10) + ":"
	for i := range t.MediaParents {
		fid := t.MediaParents[i].FacetID
		if !strings.HasPrefix(fid, "facet:") {
			continue
		}
		rest := strings.TrimPrefix(fid, "facet:")
		first, after, ok := strings.Cut(rest, ":")
		if !ok || first == "" || after == "" {
			continue
		}
		// Already library-scoped if the first segment after "facet:" is a positive integer.
		if n, err := strconv.ParseInt(first, 10, 64); err == nil && n > 0 && strconv.FormatInt(n, 10) == first {
			continue
		}
		t.MediaParents[i].FacetID = prefix + rest
	}
}

func dedupeMediaFacts(t *core.Title) {
	if t == nil || len(t.MediaFacts) == 0 {
		return
	}
	blocked := make(map[string]bool, 4+len(t.MediaParents)+len(t.MediaCredits))
	add := func(s string) {
		if n := strings.ToLower(strings.TrimSpace(s)); n != "" {
			blocked[n] = true
		}
	}
	add(t.Title)
	add(t.ParentTitle)
	add(t.ShowTitle)
	for _, p := range t.MediaParents {
		add(p.Title)
	}
	for _, c := range t.MediaCredits {
		add(c.Name)
	}
	kept := t.MediaFacts[:0]
	for _, f := range t.MediaFacts {
		if blocked[strings.ToLower(strings.TrimSpace(f.Value))] {
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		t.MediaFacts = nil
	} else {
		t.MediaFacts = kept
	}
}

func syntheticAudiobookParts(item core.KindItem, title core.Title) []core.KindItemPart {
	if !item.Playable || item.Unavailable != nil {
		return nil
	}
	if title.MediaClass != core.MediaClassAudiobook && title.MediaClass != core.MediaClassPodcast && title.MediaRole != core.MediaRoleAudiobook {
		return nil
	}
	parts := core.ExtractKindItemPartsFromItem(item)
	if len(parts) <= 1 {
		return nil
	}
	return parts
}

func cleanPartTitle(raw string, oneBasedIndex int) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Sprintf("Part %d", oneBasedIndex)
	}
	hasPath := strings.ContainsAny(s, `/\`)
	if hasPath {
		s = filepath.Base(strings.ReplaceAll(s, `\`, "/"))
	}
	ext := strings.ToLower(filepath.Ext(s))
	switch ext {
	case ".mp3", ".m4b", ".m4a", ".aac", ".ogg", ".opus", ".flac", ".wav", ".wma":
		base := strings.TrimSuffix(s, filepath.Ext(s))
		if base = strings.TrimSpace(strings.ReplaceAll(base, "_", " ")); base != "" {
			return base
		}
	}
	return s
}

func extractItemDetailChapters(item core.KindItem, parts []core.KindItemPart) []core.Chapter {
	if declared := core.ExtractKindItemChaptersFromItem(item); len(declared) > 0 {
		return declared
	}
	if len(parts) <= 1 {
		return nil
	}
	hasDuration := false
	for _, p := range parts {
		if p.DurationSec > 0 {
			hasDuration = true
			break
		}
	}
	if !hasDuration {
		return nil
	}
	out := make([]core.Chapter, 0, len(parts))
	var cursor time.Duration
	for i, p := range parts {
		dur := time.Duration(p.DurationSec * float64(time.Second))
		if dur < 0 {
			dur = 0
		}
		out = append(out, core.Chapter{
			ID:    int64(i),
			Title: cleanPartTitle(p.Name, i+1),
			Start: cursor,
			End:   cursor + dur,
		})
		cursor += dur
	}
	return out
}

func buildSyntheticPartTitle(parent core.Title, part core.KindItemPart) (core.Title, bool) {
	partKey, ok := core.PluginPartKey(parent.Key, part.Index)
	if !ok {
		return core.Title{}, false
	}
	parentRole := parent.MediaRole
	if parentRole == "" {
		parentRole = core.MediaRoleAudiobook
	}
	parents := make([]core.TitleParent, 0, 1+len(parent.MediaParents))
	parents = append(parents, core.TitleParent{
		Key:   parent.Key,
		Title: parent.Title,
		Role:  parentRole,
	})
	parents = append(parents, parent.MediaParents...)

	title := cleanPartTitle(part.Name, part.Index+1)
	var dur time.Duration
	if part.DurationSec > 0 {
		dur = time.Duration(part.DurationSec * float64(time.Second))
	}
	return core.Title{
		Key:           partKey,
		Type:          core.TypeEpisode,
		LibraryID:     parent.LibraryID,
		ItemIndex:     part.Index + 1,
		PluginAddedAt: parent.PluginAddedAt,
		LandscapeCard: parent.LandscapeCard,
		DateDisplay:   parent.DateDisplay,
		MBItemType:    "Audio",
		ParentKey:     parent.Key,
		ParentTitle:   parent.Title,
		ShowKey:       parent.Key,
		ShowTitle:     parent.Title,
		ShowYear:      parent.Year,
		Season:        1,
		Episode:       part.Index + 1,
		Title:         title,
		SortTitle:     fmt.Sprintf("%05d %s", part.Index+1, strings.ToLower(title)),
		Year:          parent.Year,
		Released:      parent.Released,
		Runtime:       dur,
		Genres:        append([]string(nil), parent.Genres...),
		PosterPath:    parent.PosterPath,
		BackdropPath:  parent.BackdropPath,
		MediaClass:    parent.MediaClass,
		MediaRole:     core.MediaRolePart,
		MediaParents:  parents,
		MediaCredits:  append([]core.MediaCredit(nil), parent.MediaCredits...),
	}, true
}

func paginateSyntheticParts(parent core.Title, parts []core.KindItemPart, cursor string, limit int) experience.PluginBrowsePage {
	offset := 0
	if cursor != "" {
		raw := strings.TrimPrefix(cursor, "part:")
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	if limit <= 0 {
		limit = 200
	}
	start := min(offset, len(parts))
	end := min(start+limit, len(parts))
	items := make([]core.Title, 0, end-start)
	for _, p := range parts[start:end] {
		if pt, ok := buildSyntheticPartTitle(parent, p); ok {
			items = append(items, pt)
		}
	}
	next := ""
	if end < len(parts) {
		next = "part:" + strconv.Itoa(end)
	}
	return experience.PluginBrowsePage{
		Items:      items,
		Total:      len(parts),
		TotalExact: true,
		NextCursor: next,
	}
}

func itemFieldString(fields map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := fields[k]
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

func itemFieldPopularity(fields map[string]json.RawMessage) float64 {
	for _, k := range []string{"popularity", "fans", "listeners"} {
		raw, ok := fields[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil && f > 0 {
			return f
		}
	}
	return 0
}

// releaseTypeRank orders music releases for artist pages and search tie-breaking:
// album (0) > EP (1) > single (2) > compilation/live (3) > remix/b-side (4) > other (5).
func releaseTypeRank(it core.KindItem) int {
	rt := strings.ToLower(itemFieldString(it.Fields, "releaseType", "release_type", "albumType", "album_type", "type"))
	switch rt {
	case "album", "studio", "studio album", "lp":
		return 0
	case "ep":
		return 1
	case "single":
		return 2
	case "compilation", "greatest hits", "anthology", "live", "soundtrack":
		return 3
	case "remix", "remixes", "b-side", "b-sides", "bsides", "instrumental", "karaoke":
		return 4
	}
	titleLow := strings.ToLower(it.Title)
	switch {
	case strings.Contains(titleLow, "remix") || strings.Contains(titleLow, "b-side") || strings.Contains(titleLow, "b side"):
		return 4
	case strings.Contains(titleLow, "greatest hits") || strings.Contains(titleLow, "best of") || strings.Contains(titleLow, "anthology") || strings.Contains(titleLow, "compilation"):
		return 3
	case rt == "":
		return 0
	default:
		return 5
	}
}

func releaseDateKey(it core.KindItem) int64 {
	if it.Released > 0 {
		return it.Released
	}
	if it.Year > 0 {
		return time.Date(it.Year, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return 0
}

func sortArtistReleaseItems(items []core.KindItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := releaseTypeRank(items[i]), releaseTypeRank(items[j])
		if ri != rj {
			return ri < rj
		}
		di, dj := releaseDateKey(items[i]), releaseDateKey(items[j])
		if di != dj {
			return di > dj
		}
		pi, pj := itemFieldPopularity(items[i].Fields), itemFieldPopularity(items[j].Fields)
		if pi != pj {
			return pi > pj
		}
		si := core.EffectiveSortTitle(items[i].Title, items[i].SortTitle)
		sj := core.EffectiveSortTitle(items[j].Title, items[j].SortTitle)
		if si != sj {
			return si < sj
		}
		return items[i].Key < items[j].Key
	})
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

func projectImages(art []core.KindArtRef, landscape bool) []frontend.Image {
	if len(art) == 0 {
		return nil
	}
	out := make([]frontend.Image, 0, len(art)+1)
	hasThumb := false
	var backdrop *frontend.Image
	for _, a := range art {
		u := strings.TrimSpace(a.URL)
		if u == "" {
			continue
		}
		kind := a.Role
		if kind == "" {
			kind = "poster"
		}
		im := frontend.Image{
			Kind:   kind,
			Path:   u,
			Width:  a.Width,
			Height: a.Height,
		}
		switch kind {
		case frontend.ImageThumb:
			hasThumb = true
		case frontend.ImageBackdrop:
			if backdrop == nil {
				cp := im
				backdrop = &cp
			}
		}
		out = append(out, im)
	}
	if landscape && !hasThumb && backdrop != nil {
		cp := *backdrop
		cp.Kind = frontend.ImageThumb
		out = append(out, cp)
	}
	return out
}

func levelIsPlayable(decl plugins.MediaKindMeta, levelKey string) bool {
	lv, _, ok := decl.Level(levelKey)
	return ok && lv.Playable
}

// ResolveKindMediaClass derives the effective MediaClass for a declared kind
// via the shared presentation resolver (MediaKindMeta.Media, KindLevel.Role,
// presentation metadata, and level shape).
func ResolveKindMediaClass(decl plugins.MediaKindMeta, present core.Presentations) core.MediaClass {
	in := kindPresentationInput(decl)
	in.Present = present
	return libraries.ResolveKindPresentations(in, core.PresentationOverrides{}).MediaClass
}

func shouldSkipItemContinue(decl plugins.MediaKindMeta, present core.Presentations, levelKey string) bool {
	levelClass, levelRole := pluginitems.ResolveMediaClassAndRole(levelKey, decl, present)
	if levelClass == core.MediaClassMusic {
		return true
	}
	if levelRole == core.MediaRoleTrack {
		return true
	}
	return false
}

// SkipResume reports whether playback progress for key must not persist a
// resume offset (music libraries and track-role items). LastViewedAt and
// completion tracking continue normally so history shelves work.
func (c *KindCatalog) SkipResume(ctx context.Context, key core.RatingKey) bool {
	if c == nil || c.items == nil || !key.IsPluginKey() {
		return false
	}
	item, libID, err := c.items.GetByKey(ctx, key)
	if err != nil {
		return false
	}
	rec, err := c.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return false
	}
	present := rec.Decl.Present
	if c.libs != nil {
		if lib, lErr := c.libs.Get(ctx, libID); lErr == nil {
			present = lib.Present
		}
	}
	return shouldSkipItemContinue(rec.Decl, present, item.Level)
}

func effectiveDeclLevel(decl plugins.MediaKindMeta, level string) (string, bool) {
	level = strings.TrimSpace(level)
	if level == "" {
		if len(decl.Levels) > 0 {
			return decl.Levels[0].Key, true
		}
		return "", true
	}
	if lv, _, ok := decl.Level(level); ok {
		return lv.Key, true
	}
	return "", false
}

func levelMatchesTypeName(lv plugins.KindLevel, idx int, decl plugins.MediaKindMeta, present core.Presentations, projected core.Title, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.EqualFold(name, string(projected.Type)) || strings.EqualFold(name, projected.MBItemType) {
		return true
	}
	if lv.Role != "" && strings.EqualFold(name, lv.Role) {
		return true
	}
	isVideoBranch := decl.IsVideoTitleLevel(lv.Key)
	depth := decl.LevelDepth(lv.Key)
	maxNonVideoDepth := 0
	for _, candidate := range decl.Levels {
		if decl.IsVideoTitleLevel(candidate.Key) {
			continue
		}
		if d := decl.LevelDepth(candidate.Key); d > maxNonVideoDepth {
			maxNonVideoDepth = d
		}
	}
	switch strings.ToLower(name) {
	case "series":
		return projected.Type == core.TypeShow
	case "musicartist":
		return projected.MBItemType == "" && core.MediaRole(lv.Role) == core.MediaRoleArtist
	case "musicalbum":
		return projected.MBItemType == "" && core.MediaRole(lv.Role) == core.MediaRoleAlbum
	case "audio":
		return projected.MBItemType == "" && core.MediaRole(lv.Role) == core.MediaRoleTrack
	case "clip":
		return isVideoBranch
	case "artist":
		if isVideoBranch {
			return false
		}
		if strings.EqualFold(projected.MBItemType, "MusicArtist") || core.MediaRole(lv.Role) == core.MediaRoleArtist {
			return true
		}
		return present.Plex != nil && strings.EqualFold(present.Plex.Base, "artist") && depth == 0 && maxNonVideoDepth >= 2
	case "album":
		if isVideoBranch {
			return false
		}
		if strings.EqualFold(projected.MBItemType, "MusicAlbum") || core.MediaRole(lv.Role) == core.MediaRoleAlbum {
			return true
		}
		if present.Plex != nil && strings.EqualFold(present.Plex.Base, "artist") {
			return (maxNonVideoDepth <= 1 && depth == 0) || (maxNonVideoDepth >= 2 && depth == 1)
		}
	case "track":
		if isVideoBranch {
			return false
		}
		if strings.EqualFold(projected.MBItemType, "Audio") || core.MediaRole(lv.Role) == core.MediaRoleTrack {
			return true
		}
		return present.Plex != nil && strings.EqualFold(present.Plex.Base, "artist") && maxNonVideoDepth >= 1 && decl.IsLeafLevel(lv.Key)
	case "photo":
		if isVideoBranch {
			return false
		}
		if strings.EqualFold(projected.MBItemType, "Photo") {
			return true
		}
		return present.Plex != nil && strings.EqualFold(present.Plex.Base, "photo") && decl.IsLeafLevel(lv.Key)
	}
	return false
}

func resolveProtocolLevel(decl plugins.MediaKindMeta, present core.Presentations, raw string) (string, bool) {
	if lv, ok := effectiveDeclLevel(decl, raw); ok {
		return lv, true
	}
	for idx, lv := range decl.Levels {
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelMatchesTypeName(lv, idx, decl, present, projected, raw) {
			return lv.Key, true
		}
	}
	return "", false
}

func levelExcludedByQuery(lv plugins.KindLevel, idx int, decl plugins.MediaKindMeta, present core.Presentations, projected core.Title, q frontend.ItemQuery) bool {
	for _, excluded := range q.ExcludedTypes {
		if levelMatchesTypeName(lv, idx, decl, present, projected, string(excluded)) {
			return true
		}
	}
	return false
}

// levelMatchesQueryTypes reports whether a level's projected title answers
// the query's type filter: by its core media type (movie/show/season/episode),
// its declared level role or Plex music/photo type (artist/album/track/photo),
// or the MediaBrowser item type the presentation declares for it
// (MusicAlbum, Audio, Book, …), which clients name in IncludeItemTypes and
// the query carries in UnservedTypes. A level the client excluded by any
// matching name never matches.
func levelMatchesQueryTypes(lv plugins.KindLevel, idx int, decl plugins.MediaKindMeta, present core.Presentations, projected core.Title, q frontend.ItemQuery) bool {
	if levelExcludedByQuery(lv, idx, decl, present, projected, q) {
		return false
	}
	for _, want := range q.Types {
		if levelMatchesTypeName(lv, idx, decl, present, projected, string(want)) {
			return true
		}
	}
	for _, kind := range q.UnservedTypes {
		if levelMatchesTypeName(lv, idx, decl, present, projected, kind) {
			return true
		}
	}
	return false
}

func browseLevelsForQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) (level string, levels []string, anyLevel bool, ok bool) {
	if len(decl.Levels) == 0 {
		if strings.TrimSpace(q.Level) != "" {
			return "", nil, false, false
		}
		return "", nil, true, true
	}
	flatAudio := flatAudioQuery(decl, present, q)
	if reqLevel := strings.TrimSpace(q.Level); reqLevel != "" {
		lv, idx, found := decl.Level(reqLevel)
		if !found {
			return "", nil, false, false
		}
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelExcludedByQuery(lv, idx, decl, present, projected, q) {
			return "", nil, false, false
		}
		if len(q.Types) > 0 || len(q.UnservedTypes) > 0 {
			if !levelMatchesQueryTypes(lv, idx, decl, present, projected, q) && !flatAudio {
				return "", nil, false, false
			}
		}
		return lv.Key, nil, false, true
	}
	if len(q.Types) == 0 && len(q.UnservedTypes) == 0 {
		if q.Filter.Person > 0 {
			if len(q.ExcludedTypes) == 0 {
				return "", nil, true, true
			}
			var allowed []string
			for idx, lv := range decl.Levels {
				projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
				if !levelExcludedByQuery(lv, idx, decl, present, projected, q) {
					allowed = append(allowed, lv.Key)
				}
			}
			if len(allowed) == 0 {
				return "", nil, false, false
			}
			if len(allowed) == 1 {
				return allowed[0], nil, false, true
			}
			return "", allowed, false, true
		}
		// An exclusion alone still narrows the root listing.
		if len(q.ExcludedTypes) == 0 {
			return decl.Levels[0].Key, nil, false, true
		}
		root := pluginitems.Project(core.KindItem{Level: decl.Levels[0].Key}, decl, present)
		if levelExcludedByQuery(decl.Levels[0], 0, decl, present, root, q) {
			return "", nil, false, false
		}
		return decl.Levels[0].Key, nil, false, true
	}
	var matched []string
	for idx, lv := range decl.Levels {
		if !q.Recursive && lv.Key != decl.Levels[0].Key {
			continue
		}
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelMatchesQueryTypes(lv, idx, decl, present, projected, q) || flatAudio {
			matched = append(matched, lv.Key)
		}
	}
	if len(matched) == 0 {
		return "", nil, false, false
	}
	if len(matched) == 1 {
		return matched[0], nil, false, true
	}
	return "", matched, false, true
}

func searchLevelsForQuery(decl plugins.MediaKindMeta, present core.Presentations, q frontend.ItemQuery) ([]string, bool) {
	if len(decl.Levels) == 0 {
		if strings.TrimSpace(q.Level) != "" {
			return nil, false
		}
		return nil, true
	}
	flatAudio := flatAudioQuery(decl, present, q)
	if reqLevel := strings.TrimSpace(q.Level); reqLevel != "" {
		lv, idx, found := decl.Level(reqLevel)
		if !found {
			return nil, false
		}
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if levelExcludedByQuery(lv, idx, decl, present, projected, q) {
			return nil, false
		}
		if len(q.Types) > 0 || len(q.UnservedTypes) > 0 {
			if !levelMatchesQueryTypes(lv, idx, decl, present, projected, q) && !flatAudio {
				return nil, false
			}
		}
		return []string{lv.Key}, true
	}
	if len(q.Types) == 0 && len(q.UnservedTypes) == 0 && len(q.ExcludedTypes) == 0 {
		return nil, true
	}
	var matched []string
	for idx, lv := range decl.Levels {
		projected := pluginitems.Project(core.KindItem{Level: lv.Key}, decl, present)
		if len(q.Types) == 0 && len(q.UnservedTypes) == 0 {
			if !levelExcludedByQuery(lv, idx, decl, present, projected, q) {
				matched = append(matched, lv.Key)
			}
		} else if levelMatchesQueryTypes(lv, idx, decl, present, projected, q) || flatAudio {
			matched = append(matched, lv.Key)
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	if len(matched) == len(decl.Levels) {
		return nil, true
	}
	return matched, true
}

func searchWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func normalizeArticleSort(s string) string {
	return core.TitleSortKey(s)
}

func scorePluginSearchHit(t core.Title, raw core.KindItem, query string) float64 {
	qNorm := strings.ToLower(strings.TrimSpace(query))
	qSort := normalizeArticleSort(query)
	tNorm := strings.ToLower(strings.TrimSpace(t.Title))
	tSort := core.EffectiveSortTitle(t.Title, t.SortTitle)

	var base float64
	exact := false
	switch {
	case qNorm != "" && (tNorm == qNorm || (qSort != "" && tSort == qSort)):
		base = 1.0
		exact = true
	case qNorm != "" && (strings.HasPrefix(tNorm, qNorm) || (qSort != "" && strings.HasPrefix(tSort, qSort))):
		base = 0.90
	default:
		qTokens := searchWords(query)
		allInTitle := false
		if len(qTokens) > 0 {
			tWords := searchWords(t.Title)
			allInTitle = len(tWords) > 0
			for _, qt := range qTokens {
				found := false
				for _, tw := range tWords {
					if strings.HasPrefix(tw, qt) {
						found = true
						break
					}
				}
				if !found {
					allInTitle = false
					break
				}
			}
		}
		if allInTitle {
			base = 0.85
		} else {
			base = 0.60
		}
	}

	// Popularity prior aligned with catalog search_titles.go (0..0.08),
	// separating iconic works from obscure covers/matches of the same title.
	if t.Popularity > 0 {
		base += 0.08 * min(1.0, math.Log10(1+t.Popularity)/6.0)
	}
	// On exact title matches in music libraries, prefer studio albums and
	// tracks over tribute artists, cover singles, or remixes.
	if exact && t.MediaClass == core.MediaClassMusic {
		switch t.MediaRole {
		case core.MediaRoleAlbum:
			switch releaseTypeRank(raw) {
			case 0:
				base += 0.015
			case 1:
				base += 0.006
			}
		case core.MediaRoleTrack:
			base += 0.012
		}
	}
	return base
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
	case core.SortIndex:
		col = "index"
	case core.SortLastViewed, core.SortPlayed:
		col = "last_viewed"
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
	for _, fd := range decl.Fields {
		if fd.Key == facet {
			return []string{fd.Key}
		}
	}
	if facet == "genre" {
		for _, fd := range decl.Fields {
			if fd.Key == "genre" || fd.Key == "genres" || fd.Key == "tags" {
				return []string{fd.Key}
			}
		}
		if len(decl.Browse.Facets) > 0 {
			return append([]string(nil), decl.Browse.Facets...)
		}
	}
	return nil
}

// mapExcludeGenresToFacets turns viewer-blocked genres into ExcludeFacets
// over the declared genre-bearing fields (the fields Project reads into
// Title.Genres), so the store drops them in SQL before counting or paging.
func mapExcludeGenresToFacets(decl plugins.MediaKindMeta, genres []string) map[string][]string {
	if len(genres) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(decl.Fields))
	for _, fd := range decl.Fields {
		declared[fd.Key] = true
	}
	out := make(map[string][]string)
	for _, key := range []string{"genre", "genres", "tags", "tag"} {
		if declared[key] {
			out[key] = append([]string(nil), genres...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapFilterToFacets(decl plugins.MediaKindMeta, f core.Filter) map[string][]string {
	if len(f.Genres) == 0 && f.Studio == "" && len(f.Facets) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(decl.Fields))
	filterable := make(map[string]bool, len(decl.Fields)*2+4)
	filterable["year"] = true
	filterable["decade"] = true
	filterable["released"] = true
	filterable["rating"] = true
	for _, fd := range decl.Fields {
		declared[fd.Key] = true
		filterable[fd.Key] = true
		if fd.Type == "date" {
			filterable[fd.Key+"-decade"] = true
		}
	}
	out := make(map[string][]string)
	for k, vals := range f.Facets {
		if !filterable[k] || len(vals) == 0 {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	if len(f.Genres) > 0 {
		fields := resolveFacetFields(decl, "genre")
		if len(fields) > 0 && declared[fields[0]] {
			out[fields[0]] = append(out[fields[0]], f.Genres...)
		}
	}
	if f.Studio != "" {
		for _, cand := range []string{"studio", "studios", "publisher", "author", "channel"} {
			if declared[cand] {
				out[cand] = append(out[cand], f.Studio)
				break
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	return nil
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

func fillPluginTabDefaults(vt *frontend.ViewTab, sorts []string) {
	if vt.Sort != "" && core.IsLastViewedSort(vt.Sort) {
		vt.Sort = string(core.SortLastViewed)
	}
	switch {
	case vt.Kind == "collections" && vt.Facet != "":
		if vt.Order == "" {
			vt.Order = "count"
		}
		effSort := vt.Sort
		if effSort == "" {
			if slices.Contains(sorts, "released") {
				effSort = "released"
			} else if len(sorts) > 0 {
				effSort = sorts[0]
			} else {
				effSort = "server"
			}
		}
		effDir := vt.Direction
		if effDir == "" {
			switch effSort {
			case "server", "title", "index":
				effDir = "asc"
			default:
				effDir = "desc"
			}
		}
		vt.Sort = effSort
		vt.Direction = effDir
	case vt.Kind == "people" && vt.Facet != "":
		if vt.Order == "" {
			vt.Order = "name"
		}
	case vt.Kind == "view":
		if vt.Layout == "" {
			vt.Layout = core.TabLayoutGrid
		}
		if vt.Sort != "" && vt.Direction == "" {
			switch vt.Sort {
			case "server", "title", "index":
				vt.Direction = "asc"
			default:
				vt.Direction = "desc"
			}
		}
	}
}

// Presentation implements experience.PluginPresentationLibraries.
func (c *KindCatalog) Presentation(ctx context.Context, lib core.Library) (experience.PluginPresentation, bool) {
	if c == nil || c.items == nil || lib.Source != core.LibraryPlugin {
		return experience.PluginPresentation{}, false
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginPresentation{}, false
	}
	decl := rec.Decl
	sorts := append([]string(nil), decl.Browse.Sorts...)

	var resolveText func(string) (string, bool)
	if c != nil && c.mgr != nil {
		resolveText = c.mgr.ResolveTextFunc(ctx, rec.PluginID, "")
	}

	var tabs []frontend.ViewTab
	if lib.TabsCustomized {
		tabs = make([]frontend.ViewTab, 0, len(lib.Tabs))
		for _, tab := range lib.Tabs {
			if tab.Hidden {
				continue
			}
			effSearch := tab.EffectiveSearch()
			var placeholder string
			if effSearch && strings.TrimSpace(tab.SearchPlaceholder) != "" {
				placeholder = plugins.ResolveTextValue(tab.SearchPlaceholder, "", resolveText)
			}
			vt := frontend.ViewTab{
				ID:                tab.Key,
				Label:             plugins.ResolveTextValue(tab.Label, tab.Key, resolveText),
				Kind:              tab.Kind,
				Level:             tab.Level,
				Facet:             tab.Facet,
				Order:             tab.Order,
				Sort:              tab.Sort,
				Direction:         tab.Direction,
				Card:              tab.EffectiveCard(),
				Layout:            tab.EffectiveLayout(),
				Search:            effSearch,
				SearchPlaceholder: placeholder,
				Filters:           tab.Filters.Clone(),
			}
			fillPluginTabDefaults(&vt, sorts)
			tabs = append(tabs, vt)
		}
	} else {
		rawTabs := decl.Library.EffectiveTabs()
		tabs = make([]frontend.ViewTab, 0, len(rawTabs))
		for _, tab := range rawTabs {
			vt := frontend.ViewTab{
				ID:        tab.Key,
				Label:     plugins.ResolveTextValue(tab.Label, tab.Key, resolveText),
				Kind:      tab.Kind,
				Level:     tab.Level,
				Facet:     tab.Facet,
				Order:     tab.Order,
				Sort:      tab.Sort,
				Direction: tab.Direction,
				Card:      tab.EffectiveCard(),
				Layout:    tab.EffectiveLayout(),
				Filters:   tab.Filters.Clone(),
			}
			if tab.Kind == "collections" || tab.Kind == "people" || tab.Kind == "view" || tab.Kind == "live" {
				vt.Search = tab.EffectiveSearch()
				if vt.Search && strings.TrimSpace(tab.SearchPlaceholder) != "" {
					vt.SearchPlaceholder = plugins.ResolveTextValue(tab.SearchPlaceholder, "", resolveText)
				}
			}
			fillPluginTabDefaults(&vt, sorts)
			tabs = append(tabs, vt)
		}
	}

	fieldsByKey := make(map[string]plugins.KindField, len(decl.Fields))
	for _, fd := range decl.Fields {
		fieldsByKey[fd.Key] = fd
	}
	resolveViewFacet := func(fk string) (frontend.ViewFacet, bool) {
		if fd, ok := fieldsByKey[fk]; ok {
			lbl := plugins.ResolveTextValue(fd.Label, fk, resolveText)
			return frontend.ViewFacet{
				ID:    fk,
				Label: lbl,
				Type:  mapVelaFacetType(fd.Type),
				Multi: true,
			}, true
		}
		if base, ok := libraryfilter.SplitDateDecadeFacet(fk); ok {
			if fd, exists := fieldsByKey[base]; exists && fd.Type == "date" {
				lbl := plugins.ResolveTextValue(fd.Label, base, resolveText)
				return frontend.ViewFacet{
					ID:    fk,
					Label: lbl + " (decade)",
					Type:  "date",
					Multi: true,
				}, true
			}
		}
		switch fk {
		case "year":
			return frontend.ViewFacet{ID: "year", Label: "Year", Type: "int", Multi: true}, true
		case "decade":
			return frontend.ViewFacet{ID: "decade", Label: "Decade", Type: "int", Multi: true}, true
		case "released":
			return frontend.ViewFacet{ID: "released", Label: "Released", Type: "date", Multi: true}, true
		case "rating":
			return frontend.ViewFacet{ID: "rating", Label: "Rating", Type: "int", Multi: true}, true
		default:
			return frontend.ViewFacet{}, false
		}
	}

	facets := make([]frontend.ViewFacet, 0, len(decl.Browse.Facets)+len(tabs))
	seenFacet := make(map[string]bool, len(decl.Browse.Facets)+len(tabs))
	for _, fk := range decl.Browse.Facets {
		if seenFacet[fk] {
			continue
		}
		if vf, ok := resolveViewFacet(fk); ok {
			seenFacet[fk] = true
			facets = append(facets, vf)
		}
	}
	for _, vt := range tabs {
		if vt.Facet == "" || seenFacet[vt.Facet] {
			continue
		}
		if vf, ok := resolveViewFacet(vt.Facet); ok {
			seenFacet[vt.Facet] = true
			facets = append(facets, vf)
		}
	}

	convertShelves := func(raw []plugins.KindLibraryShelf) []experience.PluginShelf {
		if len(raw) == 0 {
			return nil
		}
		out := make([]experience.PluginShelf, 0, len(raw))
		for _, sh := range raw {
			lbl := plugins.ResolveTextValue(sh.Label, sh.Key, resolveText)
			out = append(out, experience.PluginShelf{
				Key:       sh.Key,
				Label:     lbl,
				Level:     sh.Level,
				Sort:      mapPluginShelfSort(sh.EffectiveSort()),
				Direction: sh.EffectiveDirection(),
				Filters:   sh.EffectiveFilters(),
				TabFilter: sh.EffectiveTabFilter(),
			})
		}
		return out
	}

	shelves := convertShelves(decl.Library.Shelves)
	var tabShelves map[string][]experience.PluginShelf
	for _, tab := range decl.Library.Tabs {
		if tab.Kind == "recommended" && len(tab.Shelves) > 0 {
			if tabShelves == nil {
				tabShelves = make(map[string][]experience.PluginShelf)
			}
			tabShelves[tab.Key] = convertShelves(tab.Shelves)
		}
	}

	kindMedia := ResolveKindMediaClass(decl, lib.Present)
	var rootLevel string
	var levelMedia map[string]core.MediaClass
	var levelParents map[string]string
	if len(decl.Levels) > 0 {
		rootLevel = decl.Levels[0].Key
		levelMedia = make(map[string]core.MediaClass, len(decl.Levels))
		levelParents = make(map[string]string, len(decl.Levels))
		for _, lv := range decl.Levels {
			levelMedia[lv.Key] = decl.EffectiveLevelMedia(lv.Key, kindMedia)
			if p := decl.LevelParent(lv.Key); p != "" {
				levelParents[lv.Key] = p
			}
		}
	}

	return experience.PluginPresentation{
		Media:        kindMedia,
		Card:         lib.EffectiveCardShape(decl.Library.EffectiveCard()),
		DateDisplay:  decl.Library.EffectiveDateDisplay(),
		Tabs:         tabs,
		Sorts:        sorts,
		Facets:       facets,
		Shelves:      shelves,
		TabShelves:   tabShelves,
		RootLevel:    rootLevel,
		LevelMedia:   levelMedia,
		LevelParents: levelParents,
	}, true
}

func mapPluginShelfSort(raw string) core.SortOrder {
	switch raw {
	case "title":
		return core.SortTitle
	case "added":
		return core.SortAdded
	case "released", "":
		return core.SortReleased
	case "year":
		return core.SortYear
	case "rating":
		return core.SortRating
	case "index":
		return core.SortIndex
	case "lastViewed", "last_viewed", "lastviewed":
		return core.SortLastViewed
	default:
		return core.SortReleased
	}
}

func mapVelaFacetType(raw string) string {
	switch raw {
	case "enum", "bool":
		return "enum"
	case "people":
		return "people"
	case "int", "float":
		return "int"
	case "date":
		return "date"
	case "duration":
		return "duration"
	default:
		return "strings"
	}
}

// FacetBuckets implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetBuckets(ctx context.Context, lib core.Library, field string) ([]frontend.FacetBucket, error) {
	return c.FacetBucketsAtLevel(ctx, lib, "", field)
}

// FacetBucketsAtLevel implements experience.PluginLevelFacetLibraries.
func (c *KindCatalog) FacetBucketsAtLevel(ctx context.Context, lib core.Library, level, field string) ([]frontend.FacetBucket, error) {
	if c == nil || c.items == nil {
		return []frontend.FacetBucket{}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return nil, err
	}
	targetLevel, ok := effectiveDeclLevel(rec.Decl, level)
	if !ok {
		return []frontend.FacetBucket{}, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return nil, err
	}
	buckets, err := c.items.FacetBuckets(ctx, lib.ID, targetLevel, policy, field)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.FacetBucket, len(buckets))
	for i, b := range buckets {
		out[i] = frontend.FacetBucket{
			ID:    b.ID,
			Label: b.Label,
			Count: b.Count,
		}
	}
	return out, nil
}

// FacetCollections implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetCollections(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error) {
	return c.FacetCollectionsAtLevel(ctx, lib, "", field, order, query, offset, limit)
}

// FacetCollectionsAtLevel implements experience.PluginLevelFacetLibraries.
func (c *KindCatalog) FacetCollectionsAtLevel(ctx context.Context, lib core.Library, level, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error) {
	if c == nil || c.items == nil {
		return frontend.FacetCollectionPage{Items: []frontend.FacetCollection{}}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	targetLevel, ok := effectiveDeclLevel(rec.Decl, level)
	if !ok {
		return frontend.FacetCollectionPage{Items: []frontend.FacetCollection{}}, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	page, err := c.items.FacetCollections(ctx, pluginitems.FacetCollectionsQuery{
		LibraryID: lib.ID,
		Level:     targetLevel,
		Policy:    policy,
		FieldKey:  field,
		Order:     order,
		Query:     query,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	var newestRaw []core.KindItem
	newestIdx := make([]int, 0, len(page.Items))
	for i, col := range page.Items {
		if col.NewestItem != nil {
			newestRaw = append(newestRaw, *col.NewestItem)
			newestIdx = append(newestIdx, i)
		}
	}
	var projected []core.Title
	if len(newestRaw) > 0 {
		projected = c.projectItems(ctx, rec, lib, newestRaw)
	}
	out := make([]frontend.FacetCollection, len(page.Items))
	for i, col := range page.Items {
		out[i] = frontend.FacetCollection{
			ID:        col.ID,
			Name:      col.Label,
			ItemCount: col.Count,
		}
	}
	for j, bucketIdx := range newestIdx {
		if j < len(projected) {
			t := projected[j]
			out[bucketIdx].NewestItem = &t
		}
	}
	return frontend.FacetCollectionPage{Items: out, More: page.More}, nil
}

// FacetPeople implements experience.PluginPresentationLibraries.
func (c *KindCatalog) FacetPeople(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (experience.PluginFacetPeoplePage, error) {
	return c.FacetPeopleAtLevel(ctx, lib, "", field, order, query, offset, limit)
}

// FacetPeopleAtLevel implements experience.PluginLevelFacetLibraries.
func (c *KindCatalog) FacetPeopleAtLevel(ctx context.Context, lib core.Library, level, field, order, query string, offset, limit int) (experience.PluginFacetPeoplePage, error) {
	if c == nil || c.items == nil {
		return experience.PluginFacetPeoplePage{Items: []experience.PluginFacetPerson{}}, nil
	}
	rec, err := c.items.GetKindByLibrary(ctx, lib.ID)
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	targetLevel, ok := effectiveDeclLevel(rec.Decl, level)
	if !ok {
		return experience.PluginFacetPeoplePage{Items: []experience.PluginFacetPerson{}}, nil
	}
	policy, err := c.items.LibraryPolicy(ctx, lib)
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	page, err := c.items.FacetPeople(ctx, pluginitems.FacetPeopleQuery{
		LibraryID: lib.ID,
		Level:     targetLevel,
		Policy:    policy,
		FieldKey:  field,
		Order:     order,
		Query:     query,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return experience.PluginFacetPeoplePage{}, err
	}
	var sampleRaw []core.KindItem
	sampleIdx := make([]int, 0, len(page.Items))
	for i, p := range page.Items {
		if p.SampleItem != nil {
			sampleRaw = append(sampleRaw, *p.SampleItem)
			sampleIdx = append(sampleIdx, i)
		}
	}
	var projected []core.Title
	if len(sampleRaw) > 0 {
		projected = c.projectItems(ctx, rec, lib, sampleRaw)
	}
	out := make([]experience.PluginFacetPerson, len(page.Items))
	for i, p := range page.Items {
		out[i] = experience.PluginFacetPerson{
			PersonID: p.PersonID,
			Name:     p.Name,
			Count:    p.Count,
			HasThumb: p.Thumb != "",
		}
	}
	for j, personIdx := range sampleIdx {
		if j < len(projected) {
			t := projected[j]
			out[personIdx].SampleTitle = &t
		}
	}
	return experience.PluginFacetPeoplePage{Items: out, Total: page.Total, More: page.More}, nil
}
