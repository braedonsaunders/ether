package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/kindstream"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// DebridSourceBackend is the subset of debrid.Service used by KindResolver for
// host-mediated debrid cache checks and torrent acquisition.
type DebridSourceBackend interface {
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, bool)
	Acquire(ctx context.Context, rel core.Release) ([]byte, error)
}

// KindResolverDeps configures a KindResolver.
type KindResolverDeps struct {
	Manager   *plugins.Manager
	Libraries *libraries.Service
	Items     *pluginitems.Service
	Blobs     *blobcache.Cache
	Catalog   *KindCatalog
	Indexer   core.Indexer
	Debrid    DebridSourceBackend
	Engine    core.Engine
	HTTP      *http.Client
	Authorize func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error
	Logger    *slog.Logger
}

type resolveFlight struct {
	done    chan struct{}
	sources []core.PlayableSource
	err     error
}

// KindResolver implements frontend.KindStreams for plugin content kinds and
// supplies the host-side ResolveSources callback for plugins.KindBackends.
type KindResolver struct {
	mgr       *plugins.Manager
	libs      *libraries.Service
	items     *pluginitems.Service
	blobs     *blobcache.Cache
	catalog   *KindCatalog
	indexer   core.Indexer
	debrid    DebridSourceBackend
	engine    core.Engine
	http      *http.Client
	authorize func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) error
	backoff   *kindstream.HostBackoff
	log       *slog.Logger

	flightMu sync.Mutex
	flights  map[core.ItemRef]*resolveFlight
}

var _ frontend.KindStreams = (*KindResolver)(nil)

// NewKindResolver constructs a KindResolver.
func NewKindResolver(deps KindResolverDeps) *KindResolver {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	hc := deps.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &KindResolver{
		mgr:       deps.Manager,
		libs:      deps.Libraries,
		items:     deps.Items,
		blobs:     deps.Blobs,
		catalog:   deps.Catalog,
		indexer:   deps.Indexer,
		debrid:    deps.Debrid,
		engine:    deps.Engine,
		http:      hc,
		authorize: deps.Authorize,
		backoff:   kindstream.NewHostBackoff(),
		log:       log.With("component", "kindresolver"),
		flights:   make(map[core.ItemRef]*resolveFlight),
	}
}

// OpenKind implements frontend.KindStreams.
func (r *KindResolver) OpenKind(ctx context.Context, viewer *core.Viewer, key core.RatingKey, accept []string) (*frontend.KindMedia, error) {
	if r == nil || r.items == nil || r.libs == nil || !key.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	item, lib, rec, err := r.loadAuthorized(ctx, viewer, key)
	if err != nil {
		return nil, err
	}
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(item.Level)
	if !ok {
		return nil, core.ErrNotFound
	}
	if lv.Playable && item.Playable {
		bs, err := r.openItemByteSource(ctx, rec, lib, item, nil, accept)
		if err != nil {
			return nil, err
		}
		return &frontend.KindMedia{
			ReaderAt: bs,
			Closer:   bs,
			Size:     bs.Size(),
			MIME:     bs.MIME(),
			Duration: extractItemDuration(item, rec.Decl),
			Name:     bs.Name(),
		}, nil
	}

	leaves, err := r.collectPlayableLeaves(ctx, rec, item)
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, core.ErrNotAvailable
	}
	seqLeaves := make([]kindstream.Leaf, len(leaves))
	var totalDur time.Duration
	for i, leaf := range leaves {
		leafItem := leaf
		totalDur += extractItemDuration(leafItem, rec.Decl)
		leafSize, leafMIME := r.resolveLeafSizeAndMIME(ctx, rec, lib, leafItem, accept)
		seqLeaves[i] = kindstream.Leaf{
			Name: leafItem.Title,
			MIME: leafMIME,
			Size: leafSize,
			Open: func(lctx context.Context) (kindstream.ByteSource, error) {
				return r.openItemByteSource(lctx, rec, lib, leafItem, nil, accept)
			},
		}
	}
	seq := kindstream.NewSequence(seqLeaves)
	return &frontend.KindMedia{
		ReaderAt: seq,
		Closer:   seq,
		Size:     seq.Size(),
		MIME:     seq.MIME(),
		Duration: totalDur,
		Name:     item.Title,
	}, nil
}

func (r *KindResolver) resolveLeafSizeAndMIME(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, leaf core.KindItem, accept []string) (int64, string) {
	size, mime := extractItemSizeAndMIME(leaf, rec.Decl)
	if size > 0 {
		return size, mime
	}
	if cachedSrc, ok, err := r.items.GetResolution(ctx, leaf.Ref); err == nil && ok && cachedSrc.Size > 0 {
		if cachedSrc.MIME != "" {
			mime = cachedSrc.MIME
		}
		return cachedSrc.Size, mime
	}
	bs, err := r.openItemByteSource(ctx, rec, lib, leaf, nil, accept)
	if err != nil {
		return 0, mime
	}
	size = bs.Size()
	if bs.MIME() != "" {
		mime = bs.MIME()
	}
	_ = bs.Close()
	return size, mime
}

// KindLeaves implements frontend.KindStreams.
func (r *KindResolver) KindLeaves(ctx context.Context, viewer *core.Viewer, parent core.RatingKey) ([]frontend.KindLeaf, error) {
	if r == nil || r.items == nil || r.libs == nil || !parent.IsPluginKey() {
		return nil, core.ErrNotFound
	}
	parentItem, _, rec, err := r.loadAuthorized(ctx, viewer, parent)
	if err != nil {
		return nil, err
	}
	leaves, err := r.collectPlayableLeaves(ctx, rec, parentItem)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.KindLeaf, len(leaves))
	for i, leaf := range leaves {
		out[i] = frontend.KindLeaf{
			Key:      leaf.Key,
			Index:    leaf.Index,
			Title:    leaf.Title,
			Duration: extractItemDuration(leaf, rec.Decl),
		}
	}
	return out, nil
}

// KeyForRef implements frontend.KindRefKeys. It maps a plugin ItemRef to the
// stored item's RatingKey, refusing hidden libraries and viewers who cannot
// open them.
func (r *KindResolver) KeyForRef(ctx context.Context, viewer *core.Viewer, ref core.ItemRef) (core.RatingKey, error) {
	if r == nil || r.items == nil || r.libs == nil || !ref.Valid() {
		return 0, core.ErrNotFound
	}
	item, libID, err := r.items.GetByRef(ctx, ref)
	if err != nil {
		return 0, core.ErrNotFound
	}
	lib, err := r.libs.Get(ctx, libID)
	if err != nil || lib.Hidden || viewer == nil || viewer.ID <= 0 || !viewer.AllowsLibrary(lib.ID) {
		return 0, core.ErrNotFound
	}
	if r.authorize != nil {
		if err := r.authorize(ctx, viewer, item.Key); err != nil {
			return 0, err
		}
	}
	return item.Key, nil
}

// RefsForKeys implements frontend.KindRefKeys. It returns the ItemRef of each
// key the viewer may see; unknown or unauthorized keys are omitted. The page
// is caller-bounded, so this is a handful of indexed reads, never a walk.
func (r *KindResolver) RefsForKeys(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.ItemRef, error) {
	out := make(map[core.RatingKey]core.ItemRef, len(keys))
	if r == nil || r.items == nil || r.libs == nil || viewer == nil || viewer.ID <= 0 {
		return out, nil
	}
	for _, key := range keys {
		if !key.IsPluginKey() {
			continue
		}
		item, libID, err := r.items.GetByKey(ctx, key)
		if err != nil {
			continue
		}
		lib, err := r.libs.Get(ctx, libID)
		if err != nil || lib.Hidden || !viewer.AllowsLibrary(lib.ID) {
			continue
		}
		out[key] = item.Ref
	}
	return out, nil
}

// ResolveSources implements plugins.KindBackends.ResolveSources for plugins
// granted permissions.sources ("search" or "acquire"). Capability and
// permission checks are enforced upstream in plugins/host_kinds.go.
func (r *KindResolver) ResolveSources(ctx context.Context, _ string, q core.SourceQuery) ([]core.SourceCandidate, error) {
	if r == nil {
		return nil, core.ErrNotAvailable
	}
	switch q.Kind {
	case "indexer_search":
		if _, err := core.SearchCategories(q.Category); err != nil {
			return nil, err
		}
		if r.indexer == nil {
			return []core.SourceCandidate{}, nil
		}
		rels, err := r.indexer.Search(ctx, core.SearchRequest{
			Title:    strings.TrimSpace(q.Query),
			Category: strings.TrimSpace(q.Category),
		})
		if err != nil {
			return nil, err
		}
		out := make([]core.SourceCandidate, 0, len(rels))
		for _, rel := range rels {
			out = append(out, core.SourceCandidate{
				InfoHash: strings.ToLower(strings.TrimSpace(rel.InfoHash)),
				Magnet:   rel.MagnetURL,
				NZBURL:   rel.NZBURL,
				Title:    rel.Name,
				Size:     rel.Size,
				Seeders:  rel.Seeders,
				Cached:   len(rel.DebridCached) > 0,
			})
		}
		return out, nil

	case "debrid_check":
		if r.debrid == nil {
			return []core.SourceCandidate{}, nil
		}
		var hashes []string
		seen := make(map[string]bool)
		addHash := func(h string) {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" && !seen[h] {
				seen[h] = true
				hashes = append(hashes, h)
			}
		}
		for _, h := range q.InfoHashes {
			addHash(h)
		}
		addHash(q.InfoHash)
		if len(hashes) == 0 {
			return []core.SourceCandidate{}, nil
		}
		cached, _ := r.debrid.CheckCached(ctx, hashes)
		out := make([]core.SourceCandidate, len(hashes))
		for i, h := range hashes {
			out[i] = core.SourceCandidate{
				InfoHash: h,
				Cached:   cached[h],
			}
		}
		return out, nil

	case "debrid_acquire":
		if r.debrid == nil {
			return nil, core.ErrNotAvailable
		}
		hash := strings.ToLower(strings.TrimSpace(q.InfoHash))
		rel := core.Release{
			ID:        "kind:" + firstNonEmpty(hash, q.Magnet),
			Name:      firstNonEmpty(hash, "plugin-kind-acquire"),
			Protocol:  core.ProtocolTorrent,
			InfoHash:  hash,
			MagnetURL: strings.TrimSpace(q.Magnet),
		}
		if _, err := r.debrid.Acquire(ctx, rel); err != nil {
			return nil, err
		}
		return []core.SourceCandidate{{
			InfoHash: hash,
			Magnet:   q.Magnet,
			Cached:   true,
		}}, nil

	default:
		return nil, fmt.Errorf("kindresolver: unknown source query kind %q", q.Kind)
	}
}

func (r *KindResolver) loadAuthorized(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (core.KindItem, core.Library, pluginitems.KindRecord, error) {
	if viewer == nil || viewer.ID <= 0 {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	if r.authorize != nil {
		if err := r.authorize(ctx, viewer, key); err != nil {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, err
		}
	}
	item, libID, err := r.items.GetByKey(ctx, key)
	if err != nil {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	lib, err := r.libs.Get(ctx, libID)
	if err != nil || lib.Hidden || !viewer.AllowsLibrary(lib.ID) {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	rec, err := r.items.GetKindByLibrary(ctx, libID)
	if err != nil {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	}
	if r.catalog != nil && !r.catalog.IsPluginLibraryActive(ctx, lib) {
		return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
	} else if r.catalog == nil && r.mgr != nil {
		pr, err := r.mgr.Get(ctx, rec.PluginID)
		if err != nil || !pr.Enabled {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
		}
	}
	if r.authorize == nil && viewer.MaxContentRating != "" {
		proj := pluginitems.Project(item, rec.Decl, lib.Present)
		cr := proj.ContentRating
		if cr == "" {
			cr = extractContentRating(item.Fields)
		}
		if cr != "" && contentrating.Exceeds(cr, viewer.MaxContentRating) {
			return core.KindItem{}, core.Library{}, pluginitems.KindRecord{}, core.ErrNotFound
		}
	}
	return item, lib, rec, nil
}

func (r *KindResolver) collectPlayableLeaves(ctx context.Context, rec pluginitems.KindRecord, parent core.KindItem) ([]core.KindItem, error) {
	if parent.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	lv, _, ok := rec.Decl.Level(parent.Level)
	if ok && lv.Playable && parent.Playable {
		return []core.KindItem{parent}, nil
	}
	var leaves []core.KindItem
	queue := []core.ItemRef{parent.Ref}
	const maxLeaves = 2000
	for len(queue) > 0 && len(leaves) < maxLeaves {
		cur := queue[0]
		queue = queue[1:]
		cursor := ""
		for {
			page, err := r.items.Children(ctx, cur, cursor, 200)
			if err != nil {
				return nil, err
			}
			for _, child := range page.Items {
				if child.Unavailable != nil {
					continue
				}
				clv, _, cok := rec.Decl.Level(child.Level)
				if cok && clv.Playable && child.Playable {
					leaves = append(leaves, child)
					if len(leaves) >= maxLeaves {
						break
					}
				} else {
					queue = append(queue, child.Ref)
				}
			}
			if page.NextCursor == "" || len(page.Items) == 0 || len(leaves) >= maxLeaves {
				break
			}
			cursor = page.NextCursor
		}
	}
	return leaves, nil
}

func (r *KindResolver) kindstreamDeps(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem) kindstream.Deps {
	var ns *blobcache.Namespace
	if r.blobs != nil && lib.Key != "" {
		var quota blobcache.Quota
		if r.catalog != nil {
			quota = r.catalog.QuotaFor(rec.PluginID, lib.Key)
		} else {
			quota = blobcache.Quota{
				MaxBytes: rec.Decl.Cache.MaxBytes,
				TTL:      parseKindDuration(rec.Decl.Cache.TTL, 0),
			}
		}
		r.blobs.BindPluginLibrary(rec.PluginID, lib.Key)
		ns = r.blobs.Namespace(lib.Key, quota)
	}
	var allowHost func(string) bool
	var secretFn func(context.Context, string) (string, error)
	if r.mgr != nil {
		allowHost = func(host string) bool {
			return r.mgr.AllowNetworkHost(ctx, rec.PluginID, host)
		}
		host := r.mgr.HostFor(rec.PluginID)
		secretFn = func(sctx context.Context, name string) (string, error) {
			return host.SecretGet(sctx, name)
		}
	}
	return kindstream.Deps{
		OpenFile: func(ctx context.Context, src core.PlayableSource) (kindstream.ByteSource, error) {
			s := &StreamSources{mgr: r.mgr}
			resolved, _, err := s.validateLocalFile(ctx, rec.PluginID, src.Path)
			if err != nil {
				return nil, err
			}
			f, err := s.openGrantedLocalFile(ctx, rec.PluginID, resolved)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			if !info.Mode().IsRegular() {
				f.Close()
				return nil, core.ErrSourceUnavailable
			}
			return &kindLocalFile{File: f, size: info.Size(), mime: src.MIME}, nil
		},
		HTTP:             r.http,
		Cache:            ns,
		OpenRelease:      r.openRelease,
		Secret:           secretFn,
		AllowHost:        allowHost,
		MaxDownloadBytes: rec.Decl.Cache.EffectiveMaxBytes(),
		ItemIndex:        item.Index,
		Backoff:          r.backoff,
	}
}

func (r *KindResolver) openItemByteSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) (kindstream.ByteSource, error) {
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	ksDeps := r.kindstreamDeps(ctx, rec, lib, item)

	// 1. Check persisted positive/negative resolution cache first so repeat
	// opens never call into the plugin runtime while the resolution is fresh.
	if partIdx == nil {
		cachedSrc, ok, err := r.items.GetResolution(ctx, item.Ref)
		if err != nil {
			return nil, err
		}
		if ok {
			bs, oerr := kindstream.Open(ctx, cachedSrc, ksDeps)
			if oerr == nil {
				if cachedSrc.Size <= 0 && bs.Size() > 0 {
					cachedSrc.Size = bs.Size()
					if cachedSrc.MIME == "" {
						cachedSrc.MIME = bs.MIME()
					}
					_ = r.items.SaveResolution(ctx, item.Ref, cachedSrc)
				}
				return bs, nil
			}
			// Cached source went stale upstream: invalidate and re-resolve once.
			_ = r.items.InvalidateResolution(ctx, item.Ref)
		}
	}

	// 2. Singleflight plugin /resolve call per item_ref when uncached.
	sources, err := r.resolveItemSources(ctx, rec, lib, item, partIdx, accept)
	if err != nil {
		return nil, err
	}
	bs, winning, err := kindstream.OpenSources(ctx, sources, ksDeps)
	if err != nil {
		return nil, err
	}
	if partIdx == nil {
		if winning.Size <= 0 && bs.Size() > 0 {
			winning.Size = bs.Size()
		}
		if winning.MIME == "" && bs.MIME() != "" {
			winning.MIME = bs.MIME()
		}
		_ = r.items.SaveResolution(ctx, item.Ref, winning)
	}
	return bs, nil
}

func (r *KindResolver) resolveItemSources(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) ([]core.PlayableSource, error) {
	if item.Unavailable != nil {
		return nil, core.ErrNotAvailable
	}
	if r.mgr == nil {
		return nil, core.ErrSourceUnavailable
	}
	if partIdx != nil {
		return r.callPluginResolve(ctx, rec, lib, item, partIdx, accept)
	}

	r.flightMu.Lock()
	fl, ok := r.flights[item.Ref]
	if !ok {
		fl = &resolveFlight{done: make(chan struct{})}
		r.flights[item.Ref] = fl
		go func() {
			defer func() {
				// Leave the map before waking waiters: a request arriving
				// after a waiter returns must start a new resolve, never
				// join this finished flight and reuse its result.
				r.flightMu.Lock()
				delete(r.flights, item.Ref)
				r.flightMu.Unlock()
				close(fl.done)
			}()
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			fl.sources, fl.err = r.callPluginResolve(rctx, rec, lib, item, nil, accept)
		}()
	}
	r.flightMu.Unlock()

	select {
	case <-fl.done:
		return fl.sources, fl.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *KindResolver) callPluginResolve(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, accept []string) ([]core.PlayableSource, error) {
	pr, err := r.mgr.Get(ctx, rec.PluginID)
	if err != nil || !pr.Enabled || !pr.Manifest.HasCapability(plugins.CapKindResolver) {
		return nil, core.ErrSourceUnavailable
	}
	var clientMap map[string]any
	if len(accept) > 0 {
		clientMap = map[string]any{"accept": accept}
	}
	body, err := json.Marshal(core.KindResolveRequest{
		LibraryKey: lib.Key,
		Item:       item,
		PartIndex:  partIdx,
		Client:     clientMap,
	})
	if err != nil {
		return nil, err
	}
	resp, err := r.mgr.Call(ctx, rec.PluginID, plugins.Request{
		Method: "POST",
		Path:   "/ether/v1/kinds/" + rec.KindKey + "/resolve",
		Body:   body,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrSourceUnavailable, err)
	}
	if resp.Status == http.StatusNotFound {
		if partIdx == nil {
			_ = r.items.SaveNegativeResolution(ctx, item.Ref)
		}
		return nil, core.ErrNotAvailable
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("%w: resolver status %d", core.ErrSourceUnavailable, resp.Status)
	}
	var resolved core.KindResolveResponse
	if err := json.Unmarshal(resp.Body, &resolved); err != nil {
		return nil, fmt.Errorf("%w: invalid resolver payload: %v", core.ErrSourceUnavailable, err)
	}
	for _, src := range resolved.Sources {
		if strings.EqualFold(strings.TrimSpace(src.Pattern), "file") {
			s := &StreamSources{mgr: r.mgr}
			if _, _, err := s.validateLocalFile(ctx, rec.PluginID, src.Path); err != nil {
				return nil, err
			}
		}
	}
	if len(resolved.Sources) == 0 {
		if partIdx == nil {
			_ = r.items.SaveNegativeResolution(ctx, item.Ref)
		}
		return nil, core.ErrNotAvailable
	}
	if err := r.validateNoRawSecretsInHeaders(ctx, pr.ID, pr.Manifest.Ether.Permissions.Secrets, resolved.Sources); err != nil {
		return nil, err
	}
	if resolved.TTLSec > 0 {
		exp := time.Now().Add(time.Duration(resolved.TTLSec) * time.Second).Unix()
		for i := range resolved.Sources {
			if resolved.Sources[i].ExpiresAt == 0 {
				resolved.Sources[i].ExpiresAt = exp
			}
		}
	}
	return resolved.Sources, nil
}

// validateNoRawSecretsInHeaders rejects resolver headers whose value equals or
// embeds a stored plugin secret, enforcing the {secret:name} placeholder rule.
func (r *KindResolver) validateNoRawSecretsInHeaders(ctx context.Context, pluginID string, secretNames []string, sources []core.PlayableSource) error {
	if r.mgr == nil || len(secretNames) == 0 {
		return nil
	}
	hasHeaders := false
	for _, src := range sources {
		if len(src.Headers) > 0 {
			hasHeaders = true
			break
		}
	}
	if !hasHeaders {
		return nil
	}
	host := r.mgr.HostFor(pluginID)
	var rawSecrets []string
	for _, name := range secretNames {
		if val, err := host.SecretGet(ctx, name); err == nil && val != "" {
			rawSecrets = append(rawSecrets, val)
		}
	}
	if len(rawSecrets) == 0 {
		return nil
	}
	for _, src := range sources {
		for hk, hv := range src.Headers {
			for _, raw := range rawSecrets {
				if hv == raw || strings.Contains(hv, raw) {
					return fmt.Errorf("%w: resolver header %q contains raw secret value; use {secret:name} placeholder", core.ErrSourceUnavailable, hk)
				}
			}
		}
	}
	return nil
}

func (r *KindResolver) openRelease(ctx context.Context, hint core.ReleaseHint) (core.MediaSource, error) {
	if r.engine == nil {
		return nil, core.ErrSourceUnavailable
	}
	var rel core.Release
	fileIndex := hint.FileIndex
	if fileIndex == nil && hint.Candidate != nil {
		fileIndex = hint.Candidate.FileIndex
	}
	if fileIndex != nil && *fileIndex < 0 {
		return nil, fmt.Errorf("release fileIndex must be non-negative")
	}
	if fileIndex != nil || strings.TrimSpace(hint.File) != "" {
		rel.Target = &core.ReleaseTarget{FileIndex: fileIndex, ExactFile: strings.TrimSpace(hint.File)}
	}
	if hint.Candidate != nil {
		rel.InfoHash = strings.ToLower(strings.TrimSpace(hint.Candidate.InfoHash))
		rel.MagnetURL = strings.TrimSpace(hint.Candidate.Magnet)
		rel.NZBURL = strings.TrimSpace(hint.Candidate.NZBURL)
		rel.Name = hint.Candidate.Title
		rel.Size = hint.Candidate.Size
	}
	if h := strings.ToLower(strings.TrimSpace(hint.InfoHash)); h != "" {
		rel.InfoHash = h
	}
	if m := strings.TrimSpace(hint.Magnet); m != "" {
		rel.MagnetURL = m
	}
	if u := strings.TrimSpace(hint.NZBURL); u != "" {
		rel.NZBURL = u
	}
	rel.ID = "kind:" + firstNonEmpty(rel.InfoHash, rel.NZBURL, rel.MagnetURL)
	if rel.Target != nil {
		selection, _ := json.Marshal(rel.Target)
		rel.ID += fmt.Sprintf(":file:%x", sha256.Sum256(selection))
	}
	if rel.Name == "" {
		rel.Name = firstNonEmpty(hint.FileHint, rel.InfoHash, "kind-release")
	}

	if rel.InfoHash != "" || rel.MagnetURL != "" {
		if r.debrid == nil {
			return nil, core.ErrSourceUnavailable
		}
		rel.Protocol = core.ProtocolTorrent
		payload, err := r.debrid.Acquire(ctx, rel)
		if err != nil {
			return nil, err
		}
		return r.engine.Open(ctx, rel.ID, payload)
	}
	if rel.NZBURL != "" {
		if rel.Target != nil {
			return nil, fmt.Errorf("explicit release file selection requires a torrent; use a container source for NZB members")
		}
		if r.indexer == nil {
			return nil, core.ErrSourceUnavailable
		}
		rel.Protocol = core.ProtocolUsenet
		nzb, err := r.indexer.FetchNZB(ctx, rel)
		if err != nil {
			return nil, err
		}
		return r.engine.Open(ctx, rel.ID, nzb)
	}
	return nil, core.ErrSourceUnavailable
}

func extractItemDuration(item core.KindItem, decl plugins.MediaKindMeta) time.Duration {
	if len(item.Fields) == 0 {
		return 0
	}
	parseNum := func(raw json.RawMessage) int64 {
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return int64(n)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if f, perr := strconv.ParseFloat(strings.TrimSpace(s), 64); perr == nil && f > 0 {
				return int64(f)
			}
		}
		return 0
	}
	for _, fd := range decl.Fields {
		if fd.Type == "duration" {
			if raw, ok := item.Fields[fd.Key]; ok {
				if sec := parseNum(raw); sec > 0 {
					return time.Duration(sec) * time.Second
				}
			}
		}
	}
	for _, key := range []string{"duration", "durationSec", "runtime"} {
		if raw, ok := item.Fields[key]; ok {
			if sec := parseNum(raw); sec > 0 {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return 0
}

func extractItemSizeAndMIME(item core.KindItem, decl plugins.MediaKindMeta) (int64, string) {
	if len(item.Fields) == 0 {
		return 0, ""
	}
	var size int64
	var mime string
	parseNum := func(raw json.RawMessage) int64 {
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
			return int64(n)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if v, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64); perr == nil && v > 0 {
				return v
			}
		}
		return 0
	}
	for _, fd := range decl.Fields {
		if fd.Type == "bytes" {
			if raw, ok := item.Fields[fd.Key]; ok {
				if v := parseNum(raw); v > 0 {
					size = v
					break
				}
			}
		}
	}
	if size == 0 {
		for _, key := range []string{"size", "sizeBytes", "byteSize"} {
			if raw, ok := item.Fields[key]; ok {
				if v := parseNum(raw); v > 0 {
					size = v
					break
				}
			}
		}
	}
	for _, key := range []string{"mime", "mimeType", "contentType"} {
		if raw, ok := item.Fields[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
				mime = strings.TrimSpace(s)
				break
			}
		}
	}
	return size, mime
}

type kindLocalFile struct {
	*os.File
	size int64
	mime string
}

func (f *kindLocalFile) Size() int64  { return f.size }
func (f *kindLocalFile) Name() string { return filepath.Base(f.File.Name()) }
func (f *kindLocalFile) MIME() string { return f.mime }
