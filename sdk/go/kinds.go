package etherplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Content-kind contract (Ether v0.3.8): a plugin declares media kinds in
// its manifest and answers /ether/v1/kinds/{kind}/* routes. These types
// mirror the host's wire shapes exactly; the shared fixtures under
// sdk/conformance/kinds pin all three SDKs to the same JSON.

// ItemUnavailable explains why an item cannot currently be played.
type ItemUnavailable struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// KindItem is the standardised plugin item envelope. Ether assigns the
// stored identity; the plugin only ever sees its own external ID.
type KindItem struct {
	Unavailable *ItemUnavailable           `json:"unavailable,omitempty"`
	ID          string                     `json:"id"`
	Level       string                     `json:"level"`
	ParentID    string                     `json:"parentId,omitempty"`
	Index       int                        `json:"index,omitempty"`
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sortTitle,omitempty"`
	Summary     string                     `json:"summary,omitempty"`
	Year        int                        `json:"year,omitempty"`
	Released    int64                      `json:"released,omitempty"` // unix seconds
	Rating      float64                    `json:"rating,omitempty"`
	Playable    bool                       `json:"playable,omitempty"`
	ChildCount  int                        `json:"childCount,omitempty"`
	LeafCount   int                        `json:"leafCount,omitempty"`
	Art         []KindArtRef               `json:"art,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"`
	Hints       json.RawMessage            `json:"hints,omitempty"` // opaque JSON object, echoed to the resolver
	ETag        string                     `json:"etag,omitempty"`
}

// KindArtRef is one artwork reference on a KindItem.
type KindArtRef struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// KindPage is one page of kind items; an empty NextCursor ends the walk.
type KindPage struct {
	Items      []KindItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Total      *int       `json:"total,omitempty"`
}

// KindBrowseRequest is Ether's browse/search request for one kind level.
type KindBrowseRequest struct {
	Kind     string              `json:"-"` // filled from the route, never on the wire
	ParentID string              `json:"parentId,omitempty"`
	Level    string              `json:"level,omitempty"`
	Cursor   string              `json:"cursor,omitempty"`
	Limit    int                 `json:"limit,omitempty"`
	Sort     string              `json:"sort,omitempty"`
	Facets   map[string][]string `json:"facets,omitempty"`
	Query    string              `json:"query,omitempty"`
}

// KindChangePage is one delta page for pull sync.
type KindChangePage struct {
	Upserts    []KindItem `json:"upserts,omitempty"`
	Deletes    []string   `json:"deletes,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Complete   bool       `json:"complete"`
}

// KindChangesRequest is Ether's delta-feed request.
type KindChangesRequest struct {
	Kind   string `json:"-"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// SourceQuery asks Ether to find sources (Host.ResolveSources).
type SourceQuery struct {
	Kind  string `json:"kind"` // indexer_search|debrid_check|debrid_acquire
	Query string `json:"query,omitempty"`
	// Category accepts comma-separated Newznab IDs or standard family names.
	Category   string   `json:"category,omitempty"`
	InfoHashes []string `json:"infoHashes,omitempty"`
	InfoHash   string   `json:"infoHash,omitempty"`
	Magnet     string   `json:"magnet,omitempty"`
}

// SourceCandidate is one Ether-found release for a resolver to rank.
type SourceCandidate struct {
	InfoHash string `json:"infoHash,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
	NZBURL   string `json:"nzbUrl,omitempty"`
	// FileIndex selects a zero-based entry in the torrent provider file list; nil uses video selection.
	FileIndex *int   `json:"fileIndex,omitempty"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Cached    bool   `json:"cached,omitempty"`

	// Ref names this candidate in match scores. Ether derives it from the
	// infohash/NZB when empty; matchers echo it verbatim.
	Ref string `json:"-"`
}

// CandidateRef returns the stable ref matchers report for a candidate.
func (c SourceCandidate) CandidateRef() string {
	switch {
	case c.Ref != "":
		return c.Ref
	case c.InfoHash != "":
		return "infohash:" + strings.ToLower(c.InfoHash)
	case c.Magnet != "":
		return "magnet:" + c.Magnet
	case c.NZBURL != "":
		return "nzb:" + c.NZBURL
	default:
		return c.Title
	}
}

// KindMatchRequest asks a matcher to score candidates for one item.
type KindMatchRequest struct {
	Kind       string            `json:"-"`
	Item       KindItem          `json:"item"`
	Candidates []SourceCandidate `json:"candidates"`
	Settings   map[string]any    `json:"settings,omitempty"`
}

// KindMatchScore is one candidate score (0..1) with plain-language reasons.
type KindMatchScore struct {
	CandidateRef string   `json:"candidateRef"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons,omitempty"`
}

// KindMatchResponse is a matcher's answer, best first.
type KindMatchResponse struct {
	Matches []KindMatchScore `json:"matches"`
}

// PlayableSource is one candidate way to acquire an item's bytes. URL may
// carry {itemIndex}, {pageNumber}, {byteStart}, {byteEnd} and {byteRange}
// placeholders that Ether substitutes.
type PlayableSource struct {
	Path        string            `json:"path,omitempty"`
	ID          string            `json:"id"`
	Label       string            `json:"label,omitempty"`
	Pattern     string            `json:"pattern"` // range|download|direct|container|release|file
	MIME        string            `json:"mime"`
	Size        int64             `json:"size,omitempty"`
	DurationSec float64           `json:"durationSec,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	BlobRef     string            `json:"blobRef,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	MaxBytes    int64             `json:"maxBytes,omitempty"`
	ExpiresAt   int64             `json:"expiresAt,omitempty"`
	Container   *ContainerRef     `json:"container,omitempty"`
	Release     *ReleaseHint      `json:"release,omitempty"`
}

// ContainerRef describes an archive member inside ordered volumes.
type ContainerRef struct {
	Format         string      `json:"format,omitempty"` // "" (sniff)|rar|7z|zip
	Volumes        []VolumeRef `json:"volumes"`
	Member         string      `json:"member,omitempty"`
	MemberIndex    *int        `json:"memberIndex,omitempty"`
	PasswordSecret string      `json:"passwordSecret,omitempty"`
}

// VolumeRef locates one archive volume by URL or blob cache reference.
type VolumeRef struct {
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	BlobRef string            `json:"blobRef,omitempty"`
	Size    int64             `json:"size"`
}

// ReleaseHint hands a release to Ether's Usenet/debrid/torrent pipeline.
type ReleaseHint struct {
	// FileIndex overrides candidate.fileIndex; out-of-range indices fail without fallback.
	FileIndex *int             `json:"fileIndex,omitempty"`
	NZBURL    string           `json:"nzbUrl,omitempty"`
	Magnet    string           `json:"magnet,omitempty"`
	InfoHash  string           `json:"infoHash,omitempty"`
	Candidate *SourceCandidate `json:"candidate,omitempty"`
	// File selects an exact torrent path or unique basename, including non-video files.
	File           string `json:"file,omitempty"`
	FileHint       string `json:"fileHint,omitempty"`
	PasswordSecret string `json:"passwordSecret,omitempty"`
}

// KindResolveRequest asks a resolver for playable sources for one item.
type KindResolveRequest struct {
	Kind       string         `json:"-"`
	LibraryKey string         `json:"libraryKey"`
	Item       KindItem       `json:"item"`
	PartIndex  *int           `json:"partIndex,omitempty"`
	Client     map[string]any `json:"client,omitempty"`
}

// KindResolveResponse is a resolver's ordered sources.
type KindResolveResponse struct {
	Sources []PlayableSource `json:"sources"`
	TTLSec  int              `json:"ttlSec,omitempty"`
}

// SetupTestRequest is Ether's setup-step test call: the step key plus the
// saved (non-secret) values of the step's fields.
type SetupTestRequest struct {
	Step   string         `json:"step"`
	Fields map[string]any `json:"fields,omitempty"`
}

// SetupTestResult is a setup test's answer.
type SetupTestResult struct {
	OK     bool              `json:"ok"`
	Detail string            `json:"detail,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Reserved plugin error codes Ether understands on kind routes.
var (
	ErrKindNotFound     = &RouteError{Status: http.StatusNotFound, Code: "not_found", Message: "Item not found."}
	ErrKindNotAvailable = &RouteError{Status: http.StatusOK, Code: "not_available", Message: "No source is available right now."}
)

// CatalogProvider serves browse and detail for one kind. Implement
// ChangesProvider too for delta pull sync; without it Ether re-walks.
type CatalogProvider interface {
	Browse(ctx *Context, req KindBrowseRequest) (KindPage, error)
	Detail(ctx *Context, kind, id string) (KindItem, error)
}

// ChangesProvider is the optional delta feed for pull sync.
type ChangesProvider interface {
	Changes(ctx *Context, req KindChangesRequest) (KindChangePage, error)
}

// SearchProvider is the optional provider-side search route.
type SearchProvider interface {
	Search(ctx *Context, req KindBrowseRequest) (KindPage, error)
}

// KindResolver turns a playable item into ordered playable sources.
type KindResolver interface {
	Resolve(ctx *Context, req KindResolveRequest) (KindResolveResponse, error)
}

// KindMatcher scores source candidates for one item.
type KindMatcher interface {
	Match(ctx *Context, req KindMatchRequest) (KindMatchResponse, error)
}

// FrontendAdapter is a plugin-provided protocol surface (OPDS, Subsonic,
// custom). Ether authenticates the viewer first and forwards the request
// under /ether/v1/frontend/{key}; the handler sees X-Ether-Viewer headers
// and never a credential.
type FrontendAdapter interface {
	Handler() http.Handler
}

// SetupTester answers setup-step test calls.
type SetupTester interface {
	TestSetup(ctx *Context, req SetupTestRequest) (SetupTestResult, error)
}

type kindHandlers struct {
	catalog  map[string]CatalogProvider
	resolve  map[string]KindResolver
	match    map[string]KindMatcher
	frontend map[string]FrontendAdapter
	setup    SetupTester
}

func (p *Plugin) kinds() *kindHandlers {
	if p.kindHandlers == nil {
		p.kindHandlers = &kindHandlers{
			catalog: map[string]CatalogProvider{}, resolve: map[string]KindResolver{},
			match: map[string]KindMatcher{}, frontend: map[string]FrontendAdapter{},
		}
	}
	return p.kindHandlers
}

// OnCatalog registers the catalogue provider for one declared kind.
func (p *Plugin) OnCatalog(kind string, c CatalogProvider) { p.kinds().catalog[kind] = c }

// OnResolve registers the resolver for one declared kind.
func (p *Plugin) OnResolve(kind string, r KindResolver) { p.kinds().resolve[kind] = r }

// OnMatch registers the matcher for one declared kind.
func (p *Plugin) OnMatch(kind string, m KindMatcher) { p.kinds().match[kind] = m }

// OnFrontend registers one declared frontend adapter.
func (p *Plugin) OnFrontend(key string, a FrontendAdapter) { p.kinds().frontend[key] = a }

// OnSetupTest registers the handler for /ether/v1/setup/{step}/test.
func (p *Plugin) OnSetupTest(t SetupTester) { p.kinds().setup = t }

// StreamURL mints a short-lived, scoped ticket for one of this plugin's own
// kind items and returns the Ether stream URL to embed in a frontend adapter
// response (spec §6.5). The viewer is taken from the request's X-Ether-Viewer
// header, which Ether injects after authenticating; the plugin never sees a
// credential. Bytes are served by Ether from the ticket, never by the plugin.
func (c *Context) StreamURL(itemRef string) (string, error) {
	if c.Ether == nil {
		return "", errors.New("stream url: no host service available")
	}
	viewerID, err := strconv.ParseInt(strings.TrimSpace(c.Headers["X-Ether-Viewer"]), 10, 64)
	if err != nil || viewerID <= 0 {
		return "", errors.New("stream url: no authenticated viewer on this request")
	}
	return c.Ether.StreamURL(c.Context, viewerID, itemRef)
}

// dispatchKinds handles the /ether/v1/kinds, /ether/v1/setup and
// /ether/v1/frontend route families. It reports false when the path is
// none of them so Dispatch can fall through to its 404.
func (p *Plugin) dispatchKinds(pctx *Context, method, path string, req RawRequest) (RawResponse, bool) {
	if !strings.HasPrefix(path, "/ether/v1/") {
		return RawResponse{}, false
	}
	rest := strings.TrimPrefix(path, "/ether/v1/")
	parts := strings.Split(rest, "/")
	// Frontend adapters are protocol surfaces (WebDAV, Subsonic, OPDS, …) and
	// see every method token. The kind and setup routes stay JSON GET/POST.
	if parts[0] == "frontend" {
		if len(parts) >= 2 {
			key, _ := url.PathUnescape(parts[1])
			return p.dispatchFrontend(pctx, method, key, "/"+strings.Join(parts[2:], "/"), req), true
		}
		return RawResponse{}, false
	}
	if method != http.MethodPost && method != http.MethodGet {
		return errorJSON(http.StatusMethodNotAllowed, "method_not_allowed", "Kind routes use GET or POST."), true
	}
	switch {
	case len(parts) >= 2 && parts[0] == "kinds":
		kind, _ := url.PathUnescape(parts[1])
		return p.dispatchKind(pctx, kind, parts[2:], req), true
	case len(parts) == 3 && parts[0] == "setup" && parts[2] == "test":
		if p.kindHandlers == nil || p.kindHandlers.setup == nil {
			return errorJSON(http.StatusNotFound, "not_found", "Setup test handler is not registered."), true
		}
		var in SetupTestRequest
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), true
			}
		}
		if in.Step == "" {
			in.Step, _ = url.PathUnescape(parts[1])
		}
		res, err := p.kindHandlers.setup.TestSetup(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err), true
		}
		return okJSON(res, http.StatusOK), true
	}
	return RawResponse{}, false
}

func (p *Plugin) dispatchKind(pctx *Context, kind string, tail []string, req RawRequest) RawResponse {
	h := p.kindHandlers
	decode := func(v any) (RawResponse, bool) {
		if len(req.Body) == 0 {
			return RawResponse{}, true
		}
		if err := json.Unmarshal(req.Body, v); err != nil {
			return errorJSON(http.StatusBadRequest, "invalid_json", "Request body must be valid JSON."), false
		}
		return RawResponse{}, true
	}
	switch {
	case len(tail) == 1 && (tail[0] == "browse" || tail[0] == "search"):
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		var in KindBrowseRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		if in.Limit <= 0 {
			in.Limit = 200
		}
		if tail[0] == "search" {
			sp, isSearch := prov.(SearchProvider)
			if !isSearch {
				return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider does not search.")
			}
			page, err := sp.Search(pctx, in)
			return kindPageResponse(page, err, in.Limit)
		}
		page, err := prov.Browse(pctx, in)
		return kindPageResponse(page, err, in.Limit)
	case len(tail) == 2 && tail[0] == "items":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		if prov == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No catalogue provider is registered for kind %q.", kind))
		}
		id, _ := url.PathUnescape(tail[1])
		item, err := prov.Detail(pctx, kind, id)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		return okJSON(item, http.StatusOK)
	case len(tail) == 1 && tail[0] == "changes":
		prov := lookup(h, func(k *kindHandlers) CatalogProvider { return k.catalog[kind] })
		cp, isChanges := prov.(ChangesProvider)
		if prov == nil || !isChanges {
			// 404/501 tells Ether to fall back to a re-walk diff.
			return errorJSON(http.StatusNotImplemented, "not_implemented", "This provider has no changes feed.")
		}
		var in KindChangesRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		page, err := cp.Changes(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if page.Upserts == nil {
			page.Upserts = []KindItem{}
		}
		return okJSON(page, http.StatusOK)
	case len(tail) == 1 && tail[0] == "resolve":
		var r KindResolver
		if h != nil {
			r = h.resolve[kind]
		}
		if r == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No resolver is registered for kind %q.", kind))
		}
		var in KindResolveRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := r.Resolve(pctx, in)
		if err != nil {
			if errors.Is(err, ErrKindNotAvailable) {
				return okJSON(KindResolveResponse{Sources: []PlayableSource{}}, http.StatusOK)
			}
			return toRouteErrorResponse(err)
		}
		if res.Sources == nil {
			res.Sources = []PlayableSource{}
		}
		return okJSON(res, http.StatusOK)
	case len(tail) == 1 && tail[0] == "match":
		var m KindMatcher
		if h != nil {
			m = h.match[kind]
		}
		if m == nil {
			return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No matcher is registered for kind %q.", kind))
		}
		var in KindMatchRequest
		if resp, ok := decode(&in); !ok {
			return resp
		}
		in.Kind = kind
		res, err := m.Match(pctx, in)
		if err != nil {
			return toRouteErrorResponse(err)
		}
		if res.Matches == nil {
			res.Matches = []KindMatchScore{}
		}
		return okJSON(res, http.StatusOK)
	}
	return errorJSON(http.StatusNotFound, "not_found", "No kind route matched.")
}

func lookup(h *kindHandlers, get func(*kindHandlers) CatalogProvider) CatalogProvider {
	if h == nil {
		return nil
	}
	return get(h)
}

// kindPageResponse enforces the page contract: never more than limit
// items (Ether treats an oversized page as a contract error).
func kindPageResponse(page KindPage, err error, limit int) RawResponse {
	if err != nil {
		return toRouteErrorResponse(err)
	}
	if limit > 0 && len(page.Items) > limit {
		page.Items = page.Items[:limit]
	}
	if page.Items == nil {
		page.Items = []KindItem{}
	}
	return okJSON(page, http.StatusOK)
}

const maxFrontendBody = 10 << 20

func (p *Plugin) dispatchFrontend(pctx *Context, method, key, subPath string, req RawRequest) RawResponse {
	var a FrontendAdapter
	if p.kindHandlers != nil {
		a = p.kindHandlers.frontend[key]
	}
	if a == nil || a.Handler() == nil {
		return errorJSON(http.StatusNotFound, "not_found", fmt.Sprintf("No frontend adapter is registered for %q.", key))
	}
	if subPath == "/" || subPath == "" {
		subPath = "/"
	}
	// Protocol adapters (Subsonic, OPDS, WebDAV) carry their arguments in the
	// query string; the server relays them, so rebuild the request URL with
	// the original query. The server flattens repeated keys to the first
	// value, so a query map holds one value per key.
	_, query := parsePathAndQuery(req.Path, req.Query)
	target := "http://ether.invalid" + subPath
	if len(query) > 0 {
		target += "?" + encodeQuery(query)
	}
	httpReq := httptest.NewRequestWithContext(pctx.Context, method, target, bytes.NewReader(req.Body))
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httpReq)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxFrontendBody+1))
	if err != nil {
		return errorJSON(http.StatusBadGateway, "adapter_error", err.Error())
	}
	if len(body) > maxFrontendBody {
		return errorJSON(http.StatusBadGateway, "adapter_error", "Frontend adapter response is larger than 10 MiB; serve media through Ether stream URLs.")
	}
	hdrs := map[string]string{}
	for k := range res.Header {
		hdrs[k] = res.Header.Get(k)
	}
	// A HEAD reply carries headers only, exactly as a real server would.
	if method == http.MethodHead {
		body = nil
	}
	return RawResponse{Status: res.StatusCode, Headers: hdrs, BinaryBody: body}
}

// encodeQuery renders a query map back into a raw query string, preserving
// key order is unnecessary; values are escaped so adapters see what the
// viewer sent.
func encodeQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(query[k]))
	}
	return strings.Join(parts, "&")
}
