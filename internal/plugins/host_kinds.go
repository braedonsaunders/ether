package plugins

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
)

// KindBackends are the Ether services the kind host calls end in: the
// plugin-item store, the source resolver and the blob cache. Any field
// may be nil, in which case the matching host call returns
// ErrUnavailable. App wiring fills them (pluginitems, the indexer and
// debrid services, blobcache); the plugin id is always the caller's,
// so a plugin can only ever reach its own kinds and cache.
type KindBackends struct {
	KindUpsert     func(ctx context.Context, pluginID, kindKey string, items []core.KindItem) (accepted int, err error)
	KindDelete     func(ctx context.Context, pluginID, kindKey string, externalIDs []string) error
	KindItems      func(ctx context.Context, pluginID, kindKey, cursor string, limit int) (core.KindPage, error)
	ResolveSources func(ctx context.Context, pluginID string, q core.SourceQuery) ([]core.SourceCandidate, error)
	CachePut       func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (blobRef string, err error)
	CacheOpen      func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error)
	// StreamURL mints a scoped stream ticket for one of the plugin's own
	// kind items for one viewer and returns the Ether stream URL (spec
	// §6.5). The backend re-checks item ownership and library visibility.
	StreamURL func(ctx context.Context, pluginID string, viewerID int64, itemRef string) (string, error)
}

// SetKindBackends wires the kind host services into the Manager's
// Backends. Called once at startup by app wiring or in tests.
func (m *Manager) SetKindBackends(b KindBackends) {
	m.mu.Lock()
	m.deps.Backends.KindUpsert = b.KindUpsert
	m.deps.Backends.KindDelete = b.KindDelete
	m.deps.Backends.KindItems = b.KindItems
	m.deps.Backends.ResolveSources = b.ResolveSources
	m.deps.Backends.CachePut = b.CachePut
	m.deps.Backends.CacheOpen = b.CacheOpen
	m.deps.Backends.StreamURL = b.StreamURL
	m.mu.Unlock()
}

func (m *Manager) kindBackends() KindBackends {
	m.mu.Lock()
	defer m.mu.Unlock()
	return KindBackends{
		KindUpsert:     m.deps.Backends.KindUpsert,
		KindDelete:     m.deps.Backends.KindDelete,
		KindItems:      m.deps.Backends.KindItems,
		ResolveSources: m.deps.Backends.ResolveSources,
		CachePut:       m.deps.Backends.CachePut,
		CacheOpen:      m.deps.Backends.CacheOpen,
		StreamURL:      m.deps.Backends.StreamURL,
	}
}

// Per-call caps for kind host services (spec §3.3 sizes, host side).
const (
	MaxKindUpsertItems = 1000
	MaxKindHintsBytes  = 16 << 10
	MaxKindArtRoles    = 16
)

var externalIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// ValidExternalID reports whether id is a legal plugin external id
// (spec §2.2: no ':' or '/').
func ValidExternalID(id string) bool { return externalIDRe.MatchString(id) }

// CheckKindItem validates one item envelope against its kind
// declaration: identity, level, the string caps and the hints shape.
// Field-value typing against KindField is the item store's deeper
// check (spec §3.3); this one is shared by the host and the kit so a
// plugin is told the same thing everywhere. It returns a plain
// reason, or "" when the item passes.
func CheckKindItem(decl MediaKindMeta, it core.KindItem) string {
	switch {
	case !ValidExternalID(it.ID):
		return "id must start with a letter or digit and use letters, digits, dot, dash, underscore or tilde, at most 128 characters"
	case it.ParentID != "" && !ValidExternalID(it.ParentID):
		return "parentId must be a valid external id"
	case strings.TrimSpace(it.Title) == "":
		return "title is required"
	case utf8.RuneCountInString(it.Title) > 500:
		return "title is longer than 500 characters"
	case utf8.RuneCountInString(it.Summary) > 20000:
		return "summary is longer than 20000 characters"
	case len(it.Art) > MaxKindArtRoles:
		return "art lists more than 16 roles"
	case len(it.Hints) > MaxKindHintsBytes:
		return "hints are larger than 16 KiB"
	}
	lv, at, ok := decl.Level(it.Level)
	if !ok {
		return "level " + it.Level + " is not declared for this kind"
	}
	if at == 0 && it.ParentID != "" {
		return "a root-level item cannot name a parent"
	}
	if at > 0 && it.ParentID == "" {
		return "a " + it.Level + " item needs a parentId"
	}
	if it.Playable && !lv.Playable {
		return "level " + it.Level + " is not declared playable"
	}
	if len(it.Hints) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(it.Hints, &obj); err != nil || obj == nil {
			return "hints must be a JSON object"
		}
	}
	seenArt := map[string]bool{}
	for _, a := range it.Art {
		if a.Role == "" || a.URL == "" || seenArt[a.Role] {
			return "each art entry needs a distinct role and a url"
		}
		seenArt[a.Role] = true
	}
	for key := range it.Fields {
		if !kindFieldRe.MatchString(key) {
			return "field names must match the declared field pattern"
		}
	}
	return ""
}

// kindCheck resolves the caller's record and one declared kind,
// requiring one of caps. Roles the owner switched off are absent from
// the effective manifest, so they deny like a missing capability.
func (h *host) kindCheck(ctx context.Context, kindKey string, caps ...Capability) (Record, MediaKindMeta, error) {
	r, _, err := h.record(ctx)
	if err != nil {
		return Record{}, MediaKindMeta{}, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	allowed := false
	for _, c := range caps {
		if man.HasCapability(c) {
			allowed = true
		}
	}
	if !allowed {
		h.noteDenied(string(caps[0]))
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: %s", ErrPermissionDenied, caps[0])
	}
	decl, ok := man.MediaKindByKey(kindKey)
	if !ok {
		return Record{}, MediaKindMeta{}, fmt.Errorf("%w: kind %q is not declared", ErrUnavailable, kindKey)
	}
	return r, decl, nil
}

type kindInverse struct {
	Op       string          `json:"op"`
	Kind     string          `json:"kind"`
	Items    []string        `json:"items,omitempty"`    // external ids touched (or created on upsert)
	Previous []core.KindItem `json:"previous,omitempty"` // previous items before upsert or delete
}

// snapshotKindItems looks up any existing items for kindKey whose IDs
// are in ids so a live KindUpsert or KindDelete can record a real
// inverse in the journal.
func (h *host) snapshotKindItems(ctx context.Context, kindKey string, ids []string) (existing []core.KindItem, missing []string) {
	readFn := h.m.kindBackends().KindItems
	if readFn == nil || len(ids) == 0 {
		return nil, append([]string(nil), ids...)
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	found := make(map[string]core.KindItem, len(ids))
	cursor := ""
	seenCursor := map[string]bool{}
	for pageNum := 0; pageNum < 50 && len(found) < len(want); pageNum++ {
		seenCursor[cursor] = true
		page, err := readFn(ctx, h.plugin, kindKey, cursor, MaxKindUpsertItems)
		if err != nil {
			break
		}
		for _, it := range page.Items {
			if want[it.ID] {
				if _, dup := found[it.ID]; !dup {
					found[it.ID] = it
				}
			}
		}
		if page.NextCursor == "" || seenCursor[page.NextCursor] {
			break
		}
		cursor = page.NextCursor
	}
	seenID := map[string]bool{}
	for _, id := range ids {
		if seenID[id] {
			continue
		}
		seenID[id] = true
		if prev, ok := found[id]; ok {
			existing = append(existing, prev)
		} else {
			missing = append(missing, id)
		}
	}
	return existing, missing
}

func (h *host) KindUpsert(ctx context.Context, kindKey string, items []core.KindItem) (int, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return 0, err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return 0, err
	}
	if decl.Sync.Mode != KindSyncPush {
		return 0, fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(items) > MaxKindUpsertItems {
		return 0, fmt.Errorf("plugins: kind upsert batch larger than %d items", MaxKindUpsertItems)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if msg := CheckKindItem(decl, it); msg != "" {
			return 0, fmt.Errorf("plugins: kind item %q: %s", it.ID, msg)
		}
		ids = append(ids, it.ID)
	}
	if r.Shadow {
		return len(items), h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{Op: "kinds.upsert", Kind: kindKey, Items: ids})
	}
	backend := h.m.kindBackends().KindUpsert
	if backend == nil {
		return 0, ErrUnavailable
	}
	prev, created := h.snapshotKindItems(ctx, kindKey, ids)
	accepted, err := backend(ctx, h.plugin, kindKey, items)
	if err != nil {
		return 0, err
	}
	if err := h.journal(ctx, r, "kinds.upsert", kindKey, kindInverse{
		Op:       "kinds.upsert",
		Kind:     kindKey,
		Items:    created,
		Previous: prev,
	}); err != nil {
		return 0, err
	}
	return accepted, nil
}

func (h *host) KindDelete(ctx context.Context, kindKey string, externalIDs []string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, decl, err := h.kindCheck(ctx, kindKey, CapCatalogProvider)
	if err != nil {
		return err
	}
	if decl.Sync.Mode != KindSyncPush {
		return fmt.Errorf("%w: kind %s is pull-synced by Ether and takes no pushes", ErrPermissionDenied, kindKey)
	}
	if len(externalIDs) > MaxKindUpsertItems {
		return fmt.Errorf("plugins: kind delete batch larger than %d items", MaxKindUpsertItems)
	}
	for _, id := range externalIDs {
		if !ValidExternalID(id) {
			return fmt.Errorf("plugins: kind item id %q is not a valid external id", id)
		}
	}
	if r.Shadow {
		return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{Op: "kinds.delete", Kind: kindKey, Items: externalIDs})
	}
	backend := h.m.kindBackends().KindDelete
	if backend == nil {
		return ErrUnavailable
	}
	prev, _ := h.snapshotKindItems(ctx, kindKey, externalIDs)
	if err := backend(ctx, h.plugin, kindKey, externalIDs); err != nil {
		return err
	}
	return h.journal(ctx, r, "kinds.delete", kindKey, kindInverse{
		Op:       "kinds.delete",
		Kind:     kindKey,
		Items:    externalIDs,
		Previous: prev,
	})
}

func (h *host) KindItems(ctx context.Context, kindKey, cursor string, limit int) (core.KindPage, error) {
	if _, _, err := h.kindCheck(ctx, kindKey, CapCatalogProvider); err != nil {
		return core.KindPage{}, err
	}
	backend := h.m.kindBackends().KindItems
	if backend == nil {
		return core.KindPage{}, ErrUnavailable
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > MaxKindUpsertItems {
		limit = MaxKindUpsertItems
	}
	return backend(ctx, h.plugin, kindKey, cursor, limit)
}

func (h *host) ResolveSources(ctx context.Context, q core.SourceQuery) ([]core.SourceCandidate, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	man := r.Manifest
	man.Ether = man.EffectiveEther()
	if !man.HasCapability(CapKindResolver) {
		h.noteDenied(string(CapKindResolver))
		return nil, fmt.Errorf("%w: %s", ErrPermissionDenied, CapKindResolver)
	}
	need := "search"
	switch q.Kind {
	case "indexer_search":
		if strings.TrimSpace(q.Query) == "" {
			return nil, fmt.Errorf("plugins: indexer_search needs a query")
		}
	case "debrid_check":
		if len(q.InfoHashes) == 0 && q.InfoHash == "" {
			return nil, fmt.Errorf("plugins: debrid_check needs an info hash")
		}
	case "debrid_acquire":
		need = "acquire"
		if q.InfoHash == "" && q.Magnet == "" {
			return nil, fmt.Errorf("plugins: debrid_acquire needs an info hash or magnet")
		}
	default:
		return nil, fmt.Errorf("plugins: unknown source query kind %q", q.Kind)
	}
	level := normLevel(r.Manifest.Ether.Permissions.Sources)
	if err := h.deny(r, g, "sources", level, func(l string) bool {
		return l == "acquire" || (l == "search" && need == "search")
	}); err != nil {
		return nil, err
	}
	backend := h.m.kindBackends().ResolveSources
	if backend == nil {
		return nil, ErrUnavailable
	}
	return backend(ctx, h.plugin, q)
}

func (h *host) CachePut(ctx context.Context, libraryKey, name string, r io.Reader) (string, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return "", err
	}
	decl, err := h.cacheWriteCheck(ctx, libraryKey, name)
	if err != nil {
		return "", err
	}
	backend := h.m.kindBackends().CachePut
	if backend == nil {
		return "", ErrUnavailable
	}
	// A single blob can never exceed the whole per-library quota.
	limited := io.LimitReader(r, decl.Cache.EffectiveMaxBytes()+1)
	return backend(ctx, h.plugin, libraryKey, name, limited)
}

// viewerContextKey carries the authenticated viewer of a proxied request
// into a plugin call, so a host service can act as that viewer without the
// plugin passing (or being trusted with) the identity.
type viewerContextKey struct{}

// WithViewer attaches a viewer id to a plugin call context.
func WithViewer(ctx context.Context, viewerID int64) context.Context {
	return context.WithValue(ctx, viewerContextKey{}, viewerID)
}

// ViewerFromContext returns the viewer attached by WithViewer, if any.
func ViewerFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(viewerContextKey{}).(int64)
	return id, ok
}

// withViewerHeader reads the X-Ether-Viewer header the frontend adapter
// proxy injects and attaches that viewer to the context. A missing or
// malformed header leaves the context unchanged.
func withViewerHeader(ctx context.Context, headers map[string]string) context.Context {
	if headers == nil {
		return ctx
	}
	raw := headers["X-Ether-Viewer"]
	if raw == "" {
		return ctx
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return ctx
	}
	return WithViewer(ctx, id)
}

// StreamURL mints a scoped stream ticket for one of the plugin's own kind
// items and returns the Ether stream URL (spec §6.5). The viewer is the one
// the proxied request authenticated, carried on the call context as
// X-Ether-Viewer. The backend re-checks that the item belongs to this plugin
// and that the viewer may see its library.
func (h *host) StreamURL(ctx context.Context, itemRef string) (string, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	if man.Ether == nil || !man.HasCapability(CapFrontendAdapter) {
		h.noteDenied(string(CapFrontendAdapter))
		return "", fmt.Errorf("%w: %s", ErrPermissionDenied, CapFrontendAdapter)
	}
	viewerID, ok := ViewerFromContext(ctx)
	if !ok || viewerID <= 0 {
		return "", fmt.Errorf("%w: stream url needs an authenticated viewer", ErrPermissionDenied)
	}
	if strings.TrimSpace(itemRef) == "" {
		return "", fmt.Errorf("plugins: stream url needs an item ref")
	}
	backend := h.m.kindBackends().StreamURL
	if backend == nil {
		return "", ErrUnavailable
	}
	return backend(ctx, h.plugin, viewerID, itemRef)
}

func (h *host) CacheOpen(ctx context.Context, blobRef string) (io.ReadSeekCloser, error) {
	if _, _, err := h.record(ctx); err != nil {
		return nil, err
	}
	hexPart, ok := strings.CutPrefix(blobRef, "blob:")
	if !ok || len(hexPart) != 64 {
		return nil, fmt.Errorf("plugins: cache reference must be blob:<sha256>")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return nil, fmt.Errorf("plugins: cache reference must be blob:<sha256>")
	}
	backend := h.m.kindBackends().CacheOpen
	if backend == nil {
		return nil, ErrUnavailable
	}
	return backend(ctx, h.plugin, blobRef)
}

func (h *host) cacheWriteCheck(ctx context.Context, libraryKey, name string) (MediaKindMeta, error) {
	rec, _, err := h.record(ctx)
	if err != nil {
		return MediaKindMeta{}, err
	}
	man := rec.Manifest
	man.Ether = man.EffectiveEther()
	// The library key must name one of this plugin's own kinds; the
	// blob cache namespace follows from it, so a plugin can never
	// write into another library's cache.
	var decl MediaKindMeta
	found := false
	if man.Ether != nil {
		for _, k := range man.Ether.Contributes.MediaKinds {
			if core.PluginLibraryKey(h.plugin, k.Key) == libraryKey {
				decl, found = k, true
			}
		}
	}
	if !found || !(man.HasCapability(CapCatalogProvider) || man.HasCapability(CapKindResolver)) {
		h.noteDenied("cache")
		return MediaKindMeta{}, fmt.Errorf("%w: cache", ErrPermissionDenied)
	}
	if strings.TrimSpace(name) == "" {
		return MediaKindMeta{}, fmt.Errorf("plugins: cache put needs a name")
	}
	return decl, nil
}
