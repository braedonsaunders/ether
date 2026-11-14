// Package adapters bridges installed Ether plugins (internal/plugins) into
// the core media, catalogue, live TV, subtitle, marker, notification and
// ranking interfaces (SPEC §5.1.3, WS9).
package adapters

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/stremio"
)

const (
	localPayloadMagic = "ETHER-LOCAL/1\n"
	openDeadline      = 3 * time.Second
	movieCategory     = 2000
	tvCategory        = 5000
	maxPluginStreams  = 250
)

// LocalPayload describes a local file stream authorized through a plugin's
// folders: read grant (SPEC §5.1.4). Permission and symlink containment are
// re-verified on every CheckRelease and Open call so revoking a folder grant
// takes effect immediately.
type LocalPayload struct {
	PluginID  string `json:"pluginId"`
	ReleaseID string `json:"releaseId"`
	FilePath  string `json:"filePath"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Origin    string `json:"origin,omitempty"`
	Remote    bool   `json:"remote,omitempty"`
}

// IsLocalPayload reports whether b is an encoded LocalPayload.
func IsLocalPayload(b []byte) bool {
	return bytes.HasPrefix(b, []byte(localPayloadMagic))
}

// Encode serializes p with the local payload prefix.
func (p LocalPayload) Encode() []byte {
	body, _ := json.Marshal(p)
	return append([]byte(localPayloadMagic), body...)
}

// DecodeLocalPayload parses an encoded LocalPayload.
func DecodeLocalPayload(b []byte) (LocalPayload, error) {
	if !IsLocalPayload(b) {
		return LocalPayload{}, errors.New("plugins: not a local file payload")
	}
	var p LocalPayload
	if err := json.Unmarshal(b[len(localPayloadMagic):], &p); err != nil {
		return LocalPayload{}, fmt.Errorf("plugins: corrupt local payload: %w", err)
	}
	if p.PluginID == "" || p.FilePath == "" {
		return LocalPayload{}, errors.New("plugins: incomplete local payload")
	}
	return p, nil
}

type openResponse struct {
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expiresAt,omitempty"`
	File      string            `json:"file,omitempty"`
}

type sourceLimits struct {
	SearchPerHour int `json:"searchPerHour,omitempty"`
	GrabPerHour   int `json:"grabPerHour,omitempty"`
	DailySearches int `json:"dailySearches,omitempty"`
	DailyGrabs    int `json:"dailyGrabs,omitempty"`
	Searches      int `json:"searches,omitempty"`
	Grabs         int `json:"grabs,omitempty"`
}

// SourceStats reports per-plugin stream-source activity for the Sources -> Usage tab.
type SourceStats struct {
	PluginID string `json:"pluginId"`
	Name     string `json:"name"`
	Class    string `json:"class"`
	Searches int64  `json:"searches"`
	Grabs    int64  `json:"grabs"`
	Failures int64  `json:"failures"`
}

type pluginUsage struct {
	searchTimes []time.Time
	grabTimes   []time.Time
	searches    int64
	grabs       int64
	failures    int64
}

// StreamSources adapts enabled `stream_source` plugins into a core.Indexer
// (and core.StreamingIndexer / core.RescueIndexer) alongside an optional
// inner indexer (typically *newznab.Manager).
type StreamSources struct {
	inner       core.Indexer
	usenetReady func() bool

	mu       sync.RWMutex
	mgr      *plugins.Manager
	debrid   *debrid.Service
	order    []string
	usage    map[string]*pluginUsage
	now      func() time.Time
	backends map[string]*addonBackend
	anime    stremio.AnimeIDs
	specials stremio.SpecialEpisodes
	nzb      *http.Client
}

var (
	_ core.Indexer                = (*StreamSources)(nil)
	_ core.StreamingIndexer       = (*StreamSources)(nil)
	_ core.RescueIndexer          = (*StreamSources)(nil)
	_ core.StreamingRescueIndexer = (*StreamSources)(nil)
)

// NewStreamSources constructs a StreamSources adapter wrapping inner.
func NewStreamSources(inner core.Indexer) *StreamSources {
	return &StreamSources{
		inner: inner,
		usage: map[string]*pluginUsage{},
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// Bind attaches the plugin manager and debrid service once they are built.
func (s *StreamSources) Bind(mgr *plugins.Manager, deb *debrid.Service) {
	s.mu.Lock()
	s.mgr = mgr
	s.debrid = deb
	s.mu.Unlock()
}

// SetUsenetReady wires the Usenet readiness check so non-plugin Usenet
// releases still require an enabled Usenet provider when plugin sources are active.
func (s *StreamSources) SetUsenetReady(fn func() bool) {
	s.mu.Lock()
	s.usenetReady = fn
	s.mu.Unlock()
}

// SetClock overrides the clock used for rate-limit windows in tests.
func (s *StreamSources) SetClock(now func() time.Time) {
	s.mu.Lock()
	if now != nil {
		s.now = now
	}
	s.mu.Unlock()
}

// SetOrder sets the preferred plugin source order (plugin IDs).
func (s *StreamSources) SetOrder(ids []string) {
	s.mu.Lock()
	s.order = append([]string(nil), ids...)
	s.mu.Unlock()
}

// Order returns the current preferred plugin source order.
func (s *StreamSources) Order() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Stats returns per-plugin search and grab counters for enabled stream_source plugins.
func (s *StreamSources) Stats(ctx context.Context) []SourceStats {
	recs := s.enabledPlugins(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SourceStats, 0, len(recs))
	for _, r := range recs {
		var class string
		if r.Manifest.Ether != nil && r.Manifest.Ether.Contributes.Source != nil {
			class = r.Manifest.Ether.Contributes.Source.Class
		}
		st := SourceStats{PluginID: r.ID, Name: r.Name, Class: class}
		if u := s.usage[r.ID]; u != nil {
			st.Searches = u.searches
			st.Grabs = u.grabs
			st.Failures = u.failures
		}
		out = append(out, st)
	}
	return out
}

// HasEnabledPlugins reports whether at least one stream_source plugin is enabled.
func (s *StreamSources) HasEnabledPlugins(ctx context.Context) bool {
	return len(s.enabledPlugins(ctx)) > 0
}

// MissingSources implements the debrid/resolver sourceInventory contract.
func (s *StreamSources) MissingSources() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.HasEnabledPlugins(ctx) {
		return nil
	}
	if inv, ok := s.inner.(interface{ MissingSources() error }); ok {
		return inv.MissingSources()
	}
	return nil
}

// HasTorrentIndexer implements the debrid sourceInventory contract.
func (s *StreamSources) HasTorrentIndexer() bool {
	if inv, ok := s.inner.(interface{ HasTorrentIndexer() bool }); ok && inv.HasTorrentIndexer() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.HasEnabledPlugins(ctx)
}

// ResolveTorrent implements debrid.TorrentResolver.
func (s *StreamSources) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	if r.InfoHash != "" {
		return r, nil
	}
	if tr, ok := s.inner.(debrid.TorrentResolver); ok {
		return tr.ResolveTorrent(ctx, r)
	}
	return r, fmt.Errorf("plugins: torrent %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
}

// Name implements core.Indexer.
func (s *StreamSources) Name() string {
	if s.inner != nil {
		return s.inner.Name()
	}
	return "plugins"
}

// Search implements core.Indexer.
func (s *StreamSources) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return s.SearchWith(ctx, req, nil)
}

// SearchWith implements core.StreamingIndexer. Add-on streams are
// searched as indexer rows inside inner (see Backend).
func (s *StreamSources) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if s.inner == nil {
		return nil, core.ErrNoSources
	}
	if st, ok := s.inner.(core.StreamingIndexer); ok && arrived != nil {
		return st.SearchWith(ctx, req, arrived)
	}
	rels, err := s.inner.Search(ctx, req)
	if arrived != nil && len(rels) > 0 {
		arrived(rels)
	}
	return rels, err
}

// SearchRescue implements core.RescueIndexer.
func (s *StreamSources) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return s.SearchRescueWith(ctx, req, nil)
}

// SearchRescueWith implements core.StreamingRescueIndexer.
func (s *StreamSources) SearchRescueWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if s.inner == nil {
		return nil, nil
	}
	if sr, ok := s.inner.(core.StreamingRescueIndexer); ok && arrived != nil {
		return sr.SearchRescueWith(ctx, req, arrived)
	}
	if r, ok := s.inner.(core.RescueIndexer); ok {
		rels, err := r.SearchRescue(ctx, req)
		if arrived != nil && len(rels) > 0 {
			arrived(rels)
		}
		return rels, err
	}
	return nil, nil
}

// FetchNZB implements core.Indexer. An Ether plugin's lazy and local-file
// streams are opened here at play time (their GUID names the plugin and
// the stream); everything else is the inner indexer's.
func (s *StreamSources) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	plugin, kind, ref, direct := directStream(r.GUID)
	if !direct {
		s.mu.RLock()
		usenetReady := s.usenetReady
		s.mu.RUnlock()
		if r.Protocol == core.ProtocolUsenet && usenetReady != nil && !usenetReady() {
			return nil, fmt.Errorf("usenet release %q: %w", r.Name, core.ErrNotAvailable)
		}
		if s.inner == nil {
			return nil, fmt.Errorf("release %q: %w", r.Name, core.ErrNotAvailable)
		}
		return s.inner.FetchNZB(ctx, r)
	}
	if err := s.spendGrab(ctx, plugin); err != nil {
		return nil, err
	}
	name := plugin
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr != nil {
		if rec, err := mgr.Get(ctx, plugin); err == nil {
			name = rec.Name
		}
	}
	if kind == "file" {
		return s.prepareLocalFile(ctx, r, plugin, name, ref)
	}
	return s.openLazyStream(ctx, r, plugin, name, ref)
}

func (s *StreamSources) openLazyStream(ctx context.Context, r core.Release, pluginID, pluginName, streamID string) ([]byte, error) {
	origin := ""
	if mgr := s.manager(); mgr != nil {
		if rec, err := mgr.Get(ctx, pluginID); err == nil {
			origin = pluginOrigin(rec.SourceURL)
		}
	}
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr == nil {
		return nil, core.ErrSourceUnavailable
	}
	if streamID == "" {
		return nil, fmt.Errorf("plugin %s lazy stream missing id: %w", pluginID, core.ErrSourceUnavailable)
	}

	openCtx, cancel := context.WithTimeout(ctx, openDeadline)
	defer cancel()

	resp, err := mgr.Call(openCtx, pluginID, plugins.Request{
		Method: http.MethodGet,
		Path:   "/ether/open/" + url.PathEscape(streamID),
	})
	if err != nil {
		s.noteFailure(pluginID)
		return nil, fmt.Errorf("plugin %s open %s: %w", pluginID, streamID, core.ErrSourceUnavailable)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		s.noteFailure(pluginID)
		return nil, fmt.Errorf("plugin %s open %s status %d: %w", pluginID, streamID, resp.Status, core.ErrSourceUnavailable)
	}

	var opened openResponse
	if err := json.Unmarshal(resp.Body, &opened); err != nil {
		s.noteFailure(pluginID)
		return nil, fmt.Errorf("plugin %s open %s invalid json: %w", pluginID, streamID, core.ErrSourceUnavailable)
	}
	if opened.File != "" {
		return s.prepareLocalFile(ctx, r, pluginID, pluginName, opened.File)
	}
	if opened.URL != "" {
		return s.prepareDirectURL(ctx, r, opened.URL, opened.Headers, origin)
	}
	s.noteFailure(pluginID)
	return nil, fmt.Errorf("plugin %s open %s returned no url or file: %w", pluginID, streamID, core.ErrSourceUnavailable)
}

func (s *StreamSources) prepareDirectURL(ctx context.Context, r core.Release, rawURL string, headers map[string]string, origin string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("plugin stream %q invalid url: %w", r.Name, core.ErrSourceUnavailable)
	}
	s.mu.RLock()
	deb := s.debrid
	s.mu.RUnlock()

	httpRel := r
	httpRel.Protocol = core.ProtocolHTTP
	httpRel.StreamURL = u.String()
	httpRel.StreamHeaders = headers
	httpRel.StreamOrigin = origin
	if deb != nil {
		return deb.DirectPayload(ctx, httpRel)
	}
	size := r.Size
	if size <= 0 {
		size = 100 << 20
	}
	p := debrid.Payload{
		Key:       r.ID,
		ReleaseID: r.ID,
		Provider:  "direct",
		Kind:      debrid.KindHTTP,
		Path:      r.Name,
		Size:      size,
		Link:      u.String(),
		URL:       u.String(),
		Headers:   headers,
		Origin:    origin,
		Release:   r.Name,
	}
	return p.Encode(), nil
}

func (s *StreamSources) prepareLocalFile(ctx context.Context, r core.Release, pluginID, pluginName, rawPath string) ([]byte, error) {
	if s.isRemote(ctx, pluginID) {
		f, err := s.validateRemoteFile(ctx, pluginID, rawPath)
		if err != nil {
			return nil, err
		}
		size, err := s.remoteFileSize(ctx, pluginID, f.full)
		if err != nil {
			return nil, err
		}
		origin := pluginName
		if origin == "" {
			origin = pluginID
		}
		return (LocalPayload{PluginID: pluginID, ReleaseID: r.ID, FilePath: f.full, Name: f.name, Size: size, Origin: origin, Remote: true}).Encode(), nil
	}
	resolved, info, err := s.validateLocalFile(ctx, pluginID, rawPath)
	if err != nil {
		s.noteFailure(pluginID)
		return nil, err
	}
	origin := pluginName
	if origin == "" {
		origin = pluginID
	}
	p := LocalPayload{
		PluginID:  pluginID,
		ReleaseID: r.ID,
		FilePath:  resolved,
		Name:      filepath.Base(resolved),
		Size:      info.Size(),
		Origin:    origin,
	}
	return p.Encode(), nil
}

func (s *StreamSources) manager() *plugins.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mgr
}

func (s *StreamSources) isRemote(ctx context.Context, id string) bool {
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr == nil {
		return false
	}
	rec, err := mgr.Get(ctx, id)
	return err == nil && rec.Manifest.Ether != nil && rec.Manifest.Ether.Runtime.Kind == "remote"
}

// validateRemoteFile checks a sidecar path lexically against the plugin's
// owner-granted roots (see remotePath). The sidecar must also enforce
// containment when opening the file, including symlinks; Ether cannot
// inspect its filesystem.
func (s *StreamSources) validateRemoteFile(ctx context.Context, id, raw string) (remoteFile, error) {
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr == nil {
		return remoteFile{}, plugins.ErrUnavailable
	}
	rec, err := mgr.Get(ctx, id)
	if err != nil {
		return remoteFile{}, err
	}
	if !rec.Enabled {
		return remoteFile{}, plugins.ErrDisabled
	}
	if rec.Manifest.Ether == nil || rec.Manifest.Ether.Runtime.Kind != "remote" || rec.Manifest.Ether.Permissions.Folders != "read" {
		return remoteFile{}, plugins.ErrPermissionDenied
	}
	g, err := mgr.GrantsOf(ctx, id)
	if err != nil {
		return remoteFile{}, err
	}
	for _, revoked := range g.Revoked {
		if revoked == "folders" {
			return remoteFile{}, plugins.ErrPermissionDenied
		}
	}
	f, ok := resolveRemoteFile(g.Folders, raw)
	if !ok {
		return remoteFile{}, plugins.ErrPermissionDenied
	}
	return f, nil
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func (s *StreamSources) remoteRange(ctx context.Context, id, file string, start, end int64) (plugins.Response, io.ReadCloser, error) {
	f, err := s.validateRemoteFile(ctx, id, file)
	if err != nil {
		return plugins.Response{}, nil, err
	}
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	return mgr.CallStream(ctx, id, plugins.Request{Method: http.MethodGet, Path: "/ether/file/" + f.rel, Headers: map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end), "X-Ether-File-Root": f.root}})
}

func parseRemoteRange(h string) (start, end, total int64, err error) {
	if _, err = fmt.Sscanf(h, "bytes %d-%d/%d", &start, &end, &total); err != nil || start < 0 || end < start || total <= end || h != fmt.Sprintf("bytes %d-%d/%d", start, end, total) {
		return 0, 0, 0, errors.New("plugins: invalid remote content range")
	}
	return
}
func (s *StreamSources) remoteFileSize(ctx context.Context, id, file string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, body, err := s.remoteRange(ctx, id, file, 0, 0)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	if resp.Status == http.StatusRequestedRangeNotSatisfiable && headerValue(resp.Headers, "Content-Range") == "bytes */0" {
		return 0, nil
	}
	start, end, total, err := parseRemoteRange(headerValue(resp.Headers, "Content-Range"))
	if err != nil || resp.Status != http.StatusPartialContent || start != 0 || end != 0 {
		return 0, errors.New("plugins: remote file does not support byte ranges")
	}
	var b [2]byte
	n, err := io.ReadFull(body, b[:1])
	if n != 1 || err != nil {
		return 0, io.ErrUnexpectedEOF
	}
	return total, nil
}

// validateLocalFile verifies that pluginID is enabled, declares folders: read,
// has a non-revoked folders grant, and that rawPath resolves (after
// filepath.EvalSymlinks on both root and target) to a regular file inside one
// of the owner-granted folder roots.
func (s *StreamSources) validateLocalFile(ctx context.Context, pluginID, rawPath string) (string, os.FileInfo, error) {
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr == nil {
		return "", nil, plugins.ErrUnavailable
	}
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", nil, fmt.Errorf("%w: empty file path", plugins.ErrPermissionDenied)
	}
	for _, seg := range strings.FieldsFunc(rawPath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", nil, fmt.Errorf("%w: path traversal is not allowed", plugins.ErrPermissionDenied)
		}
	}

	rec, err := mgr.Get(ctx, pluginID)
	if err != nil {
		return "", nil, err
	}
	if !rec.Enabled {
		return "", nil, plugins.ErrDisabled
	}
	if rec.Manifest.Ether == nil || rec.Manifest.RuntimeKind() != "hosted" || rec.Manifest.Ether.Permissions.Folders != "read" {
		return "", nil, fmt.Errorf("%w: plugin does not declare folders: read permission", plugins.ErrPermissionDenied)
	}
	grants, err := mgr.GrantsOf(ctx, pluginID)
	if err != nil {
		return "", nil, err
	}
	for _, rev := range grants.Revoked {
		if rev == "folders" {
			return "", nil, fmt.Errorf("%w: folders permission is revoked", plugins.ErrPermissionDenied)
		}
	}
	if len(grants.Folders) == 0 {
		return "", nil, fmt.Errorf("%w: no folders granted to plugin", plugins.ErrPermissionDenied)
	}

	for _, root := range grants.Folders {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		evalRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		var candidate string
		if filepath.IsAbs(rawPath) {
			candidate = filepath.Clean(rawPath)
		} else {
			candidate = filepath.Join(evalRoot, rawPath)
		}
		evalTarget, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		prefix := evalRoot
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if !strings.HasPrefix(evalTarget, prefix) {
			continue
		}
		rootFS, err := os.OpenRoot(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(root), evalTarget)
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// An owner-picked root may itself be a symlink.
			rel, err = filepath.Rel(evalRoot, evalTarget)
		}
		if err != nil {
			_ = rootFS.Close()
			continue
		}
		f, err := rootFS.Open(rel)
		_ = rootFS.Close()
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		_ = f.Close()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		return evalTarget, fi, nil
	}
	return "", nil, fmt.Errorf("%w: file is outside the folders granted to %s", plugins.ErrPermissionDenied, pluginID)
}

// openGrantedLocalFile opens through the granted directory descriptor rather
// than trusting a path checked earlier. Symlink changes cannot escape it.
func (s *StreamSources) openGrantedLocalFile(ctx context.Context, pluginID, resolved string) (*os.File, error) {
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	if mgr == nil {
		return nil, plugins.ErrUnavailable
	}
	rec, err := mgr.Get(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	if !rec.Enabled || rec.Manifest.Ether == nil || rec.Manifest.RuntimeKind() != "hosted" || rec.Manifest.Ether.Permissions.Folders != "read" {
		return nil, plugins.ErrPermissionDenied
	}
	grants, err := mgr.GrantsOf(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	for _, revoked := range grants.Revoked {
		if revoked == "folders" {
			return nil, plugins.ErrPermissionDenied
		}
	}
	for _, root := range grants.Folders {
		realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(realRoot, resolved)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		fs, err := os.OpenRoot(root)
		if err != nil {
			continue
		}
		f, err := fs.Open(rel)
		_ = fs.Close()
		if err == nil {
			return f, nil
		}
	}
	return nil, plugins.ErrPermissionDenied
}

func (s *StreamSources) enabledPlugins(ctx context.Context) []plugins.Record {
	s.mu.RLock()
	mgr := s.mgr
	order := append([]string(nil), s.order...)
	s.mu.RUnlock()
	if mgr == nil {
		return nil
	}
	recs, err := mgr.List(ctx)
	if err != nil {
		return nil
	}
	var out []plugins.Record
	for _, r := range recs {
		if !r.Enabled {
			continue
		}
		if r.Manifest.EffectiveEther().Kind != plugins.KindSource || !r.Manifest.HasCapability(plugins.CapStreamSource) {
			continue
		}
		out = append(out, r)
	}
	if len(order) > 0 {
		pos := make(map[string]int, len(order))
		for i, id := range order {
			pos[id] = i
		}
		sort.SliceStable(out, func(i, j int) bool {
			pi, oki := pos[out[i].ID]
			pj, okj := pos[out[j].ID]
			switch {
			case oki && okj:
				return pi < pj
			case oki:
				return true
			case okj:
				return false
			default:
				return false
			}
		})
	}
	return out
}

func (s *StreamSources) spendSearch(ctx context.Context, rec plugins.Record) error {
	if err := core.SpendBackground(ctx, "search", rec.ID, 1); err != nil {
		return err
	}
	lim := parseLimits(rec.Manifest)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	u := s.usageForLocked(rec.ID)
	u.searchTimes = pruneTimes(u.searchTimes, now.Add(-24*time.Hour))
	hourly := countSince(u.searchTimes, now.Add(-time.Hour))
	daily := len(u.searchTimes)
	if (lim.SearchPerHour > 0 && hourly >= lim.SearchPerHour) ||
		(lim.Searches > 0 && hourly >= lim.Searches) ||
		(lim.DailySearches > 0 && daily >= lim.DailySearches) {
		return core.ErrBudgetExhausted
	}
	u.searchTimes = append(u.searchTimes, now)
	u.searches++
	return nil
}

func (s *StreamSources) spendGrab(ctx context.Context, pluginID string) error {
	if pluginID == "" {
		return nil
	}
	if err := core.SpendBackground(ctx, "grab", pluginID, 1); err != nil {
		return err
	}
	s.mu.RLock()
	mgr := s.mgr
	s.mu.RUnlock()
	var lim sourceLimits
	if mgr != nil {
		if rec, err := mgr.Get(ctx, pluginID); err == nil {
			lim = parseLimits(rec.Manifest)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	u := s.usageForLocked(pluginID)
	u.grabTimes = pruneTimes(u.grabTimes, now.Add(-24*time.Hour))
	hourly := countSince(u.grabTimes, now.Add(-time.Hour))
	daily := len(u.grabTimes)
	if (lim.GrabPerHour > 0 && hourly >= lim.GrabPerHour) ||
		(lim.Grabs > 0 && hourly >= lim.Grabs) ||
		(lim.DailyGrabs > 0 && daily >= lim.DailyGrabs) {
		return core.ErrBudgetExhausted
	}
	u.grabTimes = append(u.grabTimes, now)
	u.grabs++
	return nil
}

func (s *StreamSources) noteFailure(pluginID string) {
	if pluginID == "" {
		return
	}
	s.mu.Lock()
	u := s.usageForLocked(pluginID)
	u.failures++
	s.mu.Unlock()
}

func (s *StreamSources) usageForLocked(pluginID string) *pluginUsage {
	u := s.usage[pluginID]
	if u == nil {
		u = &pluginUsage{}
		s.usage[pluginID] = u
	}
	return u
}

func parseLimits(man plugins.Manifest) sourceLimits {
	if man.Ether == nil || man.Ether.Contributes.Source == nil || len(man.Ether.Contributes.Source.Limits) == 0 {
		return sourceLimits{}
	}
	var lim sourceLimits
	_ = json.Unmarshal(man.Ether.Contributes.Source.Limits, &lim)
	return lim
}

func pruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

func countSince(ts []time.Time, cutoff time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(cutoff) {
			n++
		}
	}
	return n
}

func pluginOrigin(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func mergeLanguages(existing, extra []string) []string {
	seen := make(map[string]bool, len(existing)+len(extra))
	var out []string
	for _, list := range [][]string{existing, extra} {
		for _, l := range list {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			key := strings.ToLower(l)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Engine & HealthChecker wrapper for local file streams (core.LocalFileSource)

// Engine wraps an inner core.Engine (and core.ReleaseHealthChecker) so
// ETHER-LOCAL/1 payloads open granted local files directly via os.Open and
// satisfy core.LocalFileSource.
type Engine struct {
	sources *StreamSources
	inner   core.Engine
	innerHC core.HealthChecker
}

var (
	_ core.Engine               = (*Engine)(nil)
	_ core.ReleaseHealthChecker = (*Engine)(nil)
)

// WrapEngine wraps inner with local-file payload support backed by s.
func (s *StreamSources) WrapEngine(inner core.Engine) *Engine {
	var hc core.HealthChecker
	if h, ok := inner.(core.HealthChecker); ok {
		hc = h
	}
	return &Engine{sources: s, inner: inner, innerHC: hc}
}

// Check implements core.HealthChecker.
func (e *Engine) Check(ctx context.Context, payload []byte) (bool, error) {
	return e.CheckRelease(ctx, "", payload)
}

// CheckRelease implements core.ReleaseHealthChecker.
func (e *Engine) CheckRelease(ctx context.Context, releaseID string, payload []byte) (bool, error) {
	if IsLocalPayload(payload) {
		lp, err := DecodeLocalPayload(payload)
		if err != nil {
			return false, err
		}
		if e.sources == nil {
			return false, plugins.ErrUnavailable
		}
		if lp.Remote {
			size, err := e.sources.remoteFileSize(ctx, lp.PluginID, lp.FilePath)
			return size > 0, err
		}
		_, fi, err := e.sources.validateLocalFile(ctx, lp.PluginID, lp.FilePath)
		if err != nil {
			return false, err
		}
		return fi.Size() > 0, nil
	}
	if e.innerHC == nil {
		return true, nil
	}
	return core.CheckReleaseHealth(ctx, e.innerHC, releaseID, payload)
}

// Open implements core.Engine.
func (e *Engine) Open(ctx context.Context, releaseID string, payload []byte) (core.MediaSource, error) {
	if IsLocalPayload(payload) {
		lp, err := DecodeLocalPayload(payload)
		if err != nil {
			return nil, err
		}
		if e.sources == nil {
			return nil, plugins.ErrUnavailable
		}
		if lp.Remote {
			size, err := e.sources.remoteFileSize(ctx, lp.PluginID, lp.FilePath)
			if err != nil {
				return nil, err
			}
			relID := releaseID
			if relID == "" {
				relID = lp.ReleaseID
			}
			return &remoteFileSource{sources: e.sources, plugin: lp.PluginID, file: lp.FilePath, size: size, name: lp.Name, releaseID: relID, origin: lp.Origin}, nil
		}
		resolved, _, err := e.sources.validateLocalFile(ctx, lp.PluginID, lp.FilePath)
		if err != nil {
			return nil, err
		}
		f, err := e.sources.openGrantedLocalFile(ctx, lp.PluginID, resolved)
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			_ = f.Close()
			return nil, plugins.ErrPermissionDenied
		}
		relID := releaseID
		if relID == "" {
			relID = lp.ReleaseID
		}
		name := lp.Name
		if name == "" {
			name = filepath.Base(resolved)
		}
		return &localFileSource{
			file:      f,
			size:      fi.Size(),
			name:      name,
			releaseID: relID,
			origin:    lp.Origin,
		}, nil
	}
	if e.inner == nil {
		return nil, errors.New("plugins: no inner stream engine")
	}
	return e.inner.Open(ctx, releaseID, payload)
}

// Prefetch implements core.Engine.
func (e *Engine) Prefetch(ctx context.Context, releaseID string, payload []byte, offset, length int64) error {
	if IsLocalPayload(payload) {
		return nil
	}
	if e.inner == nil {
		return nil
	}
	return e.inner.Prefetch(ctx, releaseID, payload, offset, length)
}

type localFileSource struct {
	file      *os.File
	size      int64
	name      string
	releaseID string
	origin    string
}

var (
	_ core.MediaSource     = (*localFileSource)(nil)
	_ core.LocalFileSource = (*localFileSource)(nil)
	_ core.OriginSource    = (*localFileSource)(nil)
)

func (l *localFileSource) ReadAt(p []byte, off int64) (int, error) {
	return l.file.ReadAt(p, off)
}

func (l *localFileSource) Size() int64         { return l.size }
func (l *localFileSource) Name() string        { return l.name }
func (l *localFileSource) ReleaseID() string   { return l.releaseID }
func (l *localFileSource) Close() error        { return l.file.Close() }
func (l *localFileSource) LocalFile() *os.File { return l.file }
func (l *localFileSource) Origin() string      { return l.origin }

// remoteFileSource rechecks grants and requests only the requested byte window
// for every ReadAt. It cannot expose a local file handle on this host.
type remoteFileSource struct {
	sources                               *StreamSources
	plugin, file, name, releaseID, origin string
	size                                  int64
	closed                                atomic.Bool
}

func (s *remoteFileSource) ReadAt(p []byte, off int64) (int, error) {
	if s.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, errors.New("plugins: negative file offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if want > s.size-off {
		want = s.size - off
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, body, err := s.sources.remoteRange(ctx, s.plugin, s.file, off, off+want-1)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	start, end, total, err := parseRemoteRange(headerValue(resp.Headers, "Content-Range"))
	if err != nil || resp.Status != http.StatusPartialContent || start != off || end != off+want-1 || total != s.size {
		return 0, errors.New("plugins: invalid remote range response")
	}
	n, err := io.ReadFull(body, p[:int(want)])
	if err != nil {
		return n, err
	}
	if want < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}
func (s *remoteFileSource) Size() int64       { return s.size }
func (s *remoteFileSource) Name() string      { return s.name }
func (s *remoteFileSource) ReleaseID() string { return s.releaseID }
func (s *remoteFileSource) Origin() string    { return s.origin }
func (s *remoteFileSource) Close() error      { s.closed.Store(true); return nil }
