package plugins

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/safefetch"
)

// host implements Host for one plugin. Grants are re-read on every call,
// so a revoke takes effect mid-call. Writes are journalled with their
// inverse; in shadow mode they are recorded without applying.
type host struct {
	m      *Manager
	plugin string
}

type readOnlyToolContextKey struct{}

func (h *host) requireWritableToolContext(ctx context.Context) error {
	if readOnly, _ := ctx.Value(readOnlyToolContextKey{}).(bool); readOnly {
		h.noteDenied("read-only tool")
		return fmt.Errorf("%w: read-only tool cannot write", ErrPermissionDenied)
	}
	return nil
}

func (h *host) record(ctx context.Context) (Record, Grants, error) {
	r, err := h.m.store.record(ctx, h.plugin)
	if err != nil {
		return Record{}, Grants{}, err
	}
	if !r.Enabled {
		return Record{}, Grants{}, ErrDisabled
	}
	g, err := h.m.store.grants(ctx, h.plugin)
	return r, g, err
}

func (h *host) deny(r Record, g Grants, key, level string, allowed func(string) bool) error {
	if g.revoked(key) || !allowed(level) {
		h.noteDenied(key)
		return fmt.Errorf("%w: %s", ErrPermissionDenied, key)
	}
	return nil
}

func (h *host) noteDenied(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if hh, err := h.m.store.health(ctx, h.plugin); err == nil && hh.Status == "ok" {
		hh.Status = "attention"
		hh.Reason = "A permission it needs was taken back: " + key
		_ = h.m.store.putHealth(ctx, h.plugin, hh)
	}
}

func viewerOK(g Grants, level string, viewerID int64) bool {
	switch level {
	case "all":
		return true
	case "granted_viewers":
		return g.viewerGranted(viewerID)
	}
	return false
}

func (h *host) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if _, _, err := h.record(ctx); err != nil {
		return nil, false, err
	}
	return h.m.store.kvGet(ctx, h.plugin, key)
}

func (h *host) KVPut(ctx context.Context, key string, value []byte) error {
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	return h.m.store.kvPut(ctx, h.plugin, key, value)
}

func (h *host) KVDelete(ctx context.Context, key string) error {
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	return h.m.store.kvDelete(ctx, h.plugin, key)
}

func (h *host) KVList(ctx context.Context, prefix string) ([]string, error) {
	if _, _, err := h.record(ctx); err != nil {
		return nil, err
	}
	return h.m.store.kvList(ctx, h.plugin, prefix)
}

// Settings returns the plugin's stored non-secret settings.
func (h *host) Settings(ctx context.Context) (map[string]any, error) {
	r, _, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	row, err := h.m.store.settings(ctx, h.plugin)
	if err != nil {
		return nil, err
	}
	return NonSecretSettings(row.Settings, row.Schema, settingsSecrets(r)), nil
}

func (h *host) SecretGet(ctx context.Context, name string) (string, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	declared := false
	for _, s := range r.Manifest.Ether.Permissions.Secrets {
		if s == name {
			declared = true
		}
	}
	if err := h.deny(r, g, "secrets", "", func(string) bool { return declared }); err != nil {
		return "", err
	}
	row, err := h.m.store.settings(ctx, h.plugin)
	if err != nil {
		return "", err
	}
	stored, ok := row.Secrets[name]
	if !ok {
		return "", fmt.Errorf("%w: secret %s is not saved", ErrUnavailable, name)
	}
	return h.m.store.openSecret(h.plugin, name, stored)
}

// AllowNetworkHost reports whether pluginID is enabled, still holds its
// network grant, and declared hostName in permissions.network.
func (m *Manager) AllowNetworkHost(ctx context.Context, pluginID, hostName string) bool {
	h := &host{m: m, plugin: pluginID}
	r, g, err := h.record(ctx)
	if err != nil || r.Manifest.Ether == nil {
		return false
	}
	if err := h.deny(r, g, "network", "", func(string) bool {
		return hostAllowed(r.Manifest.Ether.Permissions.Network, hostName)
	}); err != nil {
		return false
	}
	return true
}

// hostAllowed matches a host against the manifest network list.
func hostAllowed(patterns []string, host string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if strings.HasPrefix(p, "*.") {
			if strings.HasSuffix(host, p[1:]) && host != p[2:] {
				return true
			}
			continue
		}
		if p == host {
			return true
		}
	}
	return false
}

// ErrFetchFailed is returned when a plugin's outbound request fails. Its
// message names only the host and a category: request URLs can carry
// plugin secrets (API keys in the query), and net/http errors include the
// full URL, so neither the URL nor the underlying error text is exposed.
var ErrFetchFailed = errors.New("plugins: request failed")

func fetchError(host string, err error) error {
	reason := "network error"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "timed out"
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case errors.Is(err, ErrPermissionDenied):
		// A redirect left the plugin's network grant.
		return fmt.Errorf("%w: a redirect from %s left the granted hosts", ErrPermissionDenied, host)
	case errors.Is(err, safefetch.ErrBlocked):
		return fmt.Errorf("%w: %s is not allowed (%w)", ErrFetchFailed, host, safefetch.ErrBlocked)
	}
	return fmt.Errorf("%w: %s: %s", ErrFetchFailed, host, reason)
}

func (h *host) openFetch(ctx context.Context, req FetchRequest) (*http.Response, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%w: bad url", ErrPermissionDenied)
	}
	if err := h.deny(r, g, "network", "", func(string) bool {
		return hostAllowed(r.Manifest.Ether.Permissions.Network, u.Hostname())
	}); err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	hr, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	// Recheck the current grant before every redirect, as well as the SSRF
	// guard's own redirect policy. A redirect is another network request.
	client := *h.m.client
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		current, grants, err := h.record(next.Context())
		if err != nil {
			return err
		}
		if err := h.deny(current, grants, "network", "", func(string) bool {
			return hostAllowed(current.Manifest.Ether.Permissions.Network, next.URL.Hostname())
		}); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("plugins: stopped after 10 redirects")
		}
		return nil
	}
	resp, err := client.Do(hr)
	if err != nil {
		return nil, fetchError(u.Hostname(), err)
	}
	return resp, nil
}

func (h *host) HTTPFetch(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	resp, err := h.openFetch(ctx, req)
	if err != nil {
		return FetchResponse{}, err
	}
	u, _ := url.Parse(req.URL)
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBytes+1))
	if err != nil {
		return FetchResponse{}, fetchError(u.Hostname(), err)
	}
	if len(body) > MaxFetchBytes {
		return FetchResponse{}, fmt.Errorf("plugins: response larger than %d bytes", MaxFetchBytes)
	}
	h.noteFetchTraffic(ctx, req, int64(len(body)))
	out := FetchResponse{Status: resp.StatusCode, Headers: map[string]string{}, Body: body}
	for k := range resp.Header {
		out.Headers[k] = resp.Header.Get(k)
	}
	return out, nil
}

func (h *host) catalogCheck(ctx context.Context) (Record, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return Record{}, err
	}
	return r, h.deny(r, g, "catalog", normLevel(r.Manifest.Ether.Permissions.Catalog), func(l string) bool { return l == "read" })
}

func (h *host) CatalogSearch(ctx context.Context, query string, limit, offset int) (json.RawMessage, error) {
	if _, err := h.catalogCheck(ctx); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.CatalogSearch == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return h.m.deps.Backends.CatalogSearch(ctx, query, limit, offset)
}

func (h *host) CatalogGet(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := h.catalogCheck(ctx); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.CatalogGet == nil {
		return nil, ErrUnavailable
	}
	return h.m.deps.Backends.CatalogGet(ctx, id)
}

func (h *host) ViewerGranted(ctx context.Context) (json.RawMessage, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	p := r.Manifest.Ether.Permissions.Viewers
	allowed := (!g.revoked("viewers.watch_history") && normLevel(p.WatchHistory) != "none") || (!g.revoked("viewers.identity") && normLevel(p.Identity) != "none")
	if !allowed || len(g.Viewers) == 0 {
		return json.RawMessage(`[]`), nil
	}
	if h.m.deps.Backends.ViewerNames == nil {
		return nil, ErrUnavailable
	}
	return h.m.deps.Backends.ViewerNames(ctx, g.Viewers)
}

func (h *host) ViewerHistory(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.deny(r, g, "viewers.watch_history", normLevel(r.Manifest.Ether.Permissions.Viewers.WatchHistory),
		func(l string) bool { return viewerOK(g, l, viewerID) }); err != nil {
		return nil, err
	}
	if h.m.deps.Backends.ViewerHistory == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return h.m.deps.Backends.ViewerHistory(ctx, viewerID, limit, offset)
}

// journalWrite records a write (and, unless shadow, has already applied
// it; callers pass the inverse they obtained).
func (h *host) journal(ctx context.Context, r Record, op, detail string, inverse any) error {
	raw, err := json.Marshal(inverse)
	if err != nil {
		return err
	}
	_, err = h.m.store.journal(ctx, JournalEntry{Plugin: h.plugin, Op: op, Detail: detail, Inverse: raw, Shadow: r.Shadow})
	return err
}

type watchInverse struct {
	Op       string `json:"op"`
	ViewerID int64  `json:"viewerId"`
	TitleID  string `json:"titleId"`
	Watched  bool   `json:"watched"`
}

type collectionInverse struct {
	Op       string          `json:"op"`
	Previous json.RawMessage `json:"previous,omitempty"` // empty means delete
	Doc      json.RawMessage `json:"doc,omitempty"`
}

func (h *host) SetWatchState(ctx context.Context, viewerID int64, titleID string, watched bool) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	if err := h.deny(r, g, "viewers.write_watch_state", normLevel(r.Manifest.Ether.Permissions.Viewers.WriteWatchState),
		func(l string) bool { return viewerOK(g, l, viewerID) }); err != nil {
		return err
	}
	if r.Shadow {
		return h.journal(ctx, r, "viewers.setWatchState", titleID, watchInverse{Op: "viewers.setWatchState", ViewerID: viewerID, TitleID: titleID, Watched: !watched})
	}
	if h.m.deps.Backends.SetWatchState == nil {
		return ErrUnavailable
	}
	prev, err := h.m.deps.Backends.SetWatchState(ctx, viewerID, titleID, watched)
	if err != nil {
		return err
	}
	return h.journal(ctx, r, "viewers.setWatchState", titleID, watchInverse{Op: "viewers.setWatchState", ViewerID: viewerID, TitleID: titleID, Watched: prev})
}

func (h *host) CollectionsUpsert(ctx context.Context, doc json.RawMessage) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return err
	}
	if err := h.deny(r, g, "collections", normLevel(r.Manifest.Ether.Permissions.Collections),
		func(l string) bool { return l == "write" }); err != nil {
		return err
	}
	if r.Shadow {
		return h.journal(ctx, r, "collections.upsert", "", collectionInverse{Op: "collections.upsert", Doc: doc})
	}
	if h.m.deps.Backends.CollectionsUpsert == nil {
		return ErrUnavailable
	}
	prev, err := h.m.deps.Backends.CollectionsUpsert(collectionContext(ctx, h.plugin), doc)
	if err != nil {
		return err
	}
	return h.journal(ctx, r, "collections.upsert", "", collectionInverse{Op: "collections.upsert", Previous: prev, Doc: doc})
}

// UndoAll replays a plugin's live journal inverses, newest first, and
// returns how many entries were undone.
func (m *Manager) UndoAll(ctx context.Context, pluginID string) (int, error) {
	return m.UndoSince(ctx, pluginID, 0)
}

// UndoSince reverses the plugin's changes back to and including entry
// since (0 means all of them), newest first, so an older change is never
// reversed underneath a later one.
func (m *Manager) UndoSince(ctx context.Context, pluginID string, since int64) (int, error) {
	entries, err := m.store.journalList(ctx, pluginID, 10000, true)
	if err != nil {
		return 0, err
	}
	undone := 0
	for _, e := range entries { // journalList is newest first
		if e.ID < since {
			break
		}
		if err := m.applyInverse(ctx, e); err != nil {
			return undone, err
		}
		if err := m.store.markUndone(ctx, e.ID); err != nil {
			return undone, err
		}
		undone++
	}
	return undone, nil
}

func (m *Manager) applyInverse(ctx context.Context, e JournalEntry) error {
	switch e.Op {
	case "viewers.setWatchState":
		var inv watchInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		if m.deps.Backends.SetWatchState == nil {
			return ErrUnavailable
		}
		_, err := m.deps.Backends.SetWatchState(ctx, inv.ViewerID, inv.TitleID, inv.Watched)
		return err
	case "collections.upsert":
		var inv collectionInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		if len(inv.Previous) > 0 {
			if m.deps.Backends.CollectionsUpsert == nil {
				return ErrUnavailable
			}
			_, err := m.deps.Backends.CollectionsUpsert(collectionContext(ctx, e.Plugin), inv.Previous)
			return err
		}
		if m.deps.Backends.CollectionsDelete == nil {
			return ErrUnavailable
		}
		return m.deps.Backends.CollectionsDelete(collectionContext(ctx, e.Plugin), inv.Doc)
	case "kinds.upsert":
		var inv kindInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		b := m.kindBackends()
		if len(inv.Previous) > 0 {
			if b.KindUpsert == nil {
				return ErrUnavailable
			}
			if _, err := b.KindUpsert(ctx, e.Plugin, inv.Kind, inv.Previous); err != nil {
				return err
			}
		}
		if len(inv.Items) > 0 {
			if b.KindDelete == nil {
				return ErrUnavailable
			}
			if err := b.KindDelete(ctx, e.Plugin, inv.Kind, inv.Items); err != nil {
				return err
			}
		}
		return nil
	case "kinds.delete":
		var inv kindInverse
		if err := json.Unmarshal(e.Inverse, &inv); err != nil {
			return err
		}
		b := m.kindBackends()
		if b.KindUpsert == nil {
			return ErrUnavailable
		}
		if len(inv.Previous) > 0 {
			_, err := b.KindUpsert(ctx, e.Plugin, inv.Kind, inv.Previous)
			return err
		}
		return nil
	default:
		return fmt.Errorf("plugins: no inverse for journal op %q", e.Op)
	}
}

func (h *host) folderCheck(ctx context.Context, root, path string) (string, error) {
	r, g, err := h.record(ctx)
	if err != nil {
		return "", err
	}
	if err := h.deny(r, g, "folders", normLevel(r.Manifest.Ether.Permissions.Folders),
		func(l string) bool { return l == "read" }); err != nil {
		return "", err
	}
	granted := false
	for _, f := range g.Folders {
		if f == root {
			granted = true
		}
	}
	if !granted {
		h.noteDenied("folders")
		return "", fmt.Errorf("%w: folder was not picked for this plugin", ErrPermissionDenied)
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, path))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path leaves the picked folder", ErrPermissionDenied)
	}
	return joined, nil
}

func (h *host) FoldersList(ctx context.Context, root, path string) ([]FolderEntry, error) {
	_, err := h.folderCheck(ctx, root, path)
	if err != nil {
		return nil, err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer fs.Close()
	name := path
	if name == "" {
		name = "."
	}
	dir, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(1000)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := make([]FolderEntry, 0, len(entries))
	for _, e := range entries {
		fe := FolderEntry{Name: e.Name(), Path: filepath.Join(path, e.Name()), Dir: e.IsDir()}
		if e.Type()&os.ModeSymlink != 0 {
			// Follow a link only when its target stays inside the grant;
			// the rooted Stat fails for one that escapes.
			info, err := fs.Stat(filepath.Join(name, e.Name()))
			if err != nil {
				continue
			}
			fe.Dir, fe.Size, fe.Mtime = info.IsDir(), info.Size(), info.ModTime().Unix()
			out = append(out, fe)
			if len(out) >= 1000 {
				break
			}
			continue
		}
		if info, err := e.Info(); err == nil {
			fe.Size, fe.Mtime = info.Size(), info.ModTime().Unix()
		}
		out = append(out, fe)
		if len(out) >= 1000 {
			break
		}
	}
	return out, nil
}

func (h *host) FoldersStat(ctx context.Context, root, path string) (FolderEntry, error) {
	_, err := h.folderCheck(ctx, root, path)
	if err != nil {
		return FolderEntry{}, err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return FolderEntry{}, err
	}
	defer fs.Close()
	name := path
	if name == "" {
		name = "."
	}
	info, err := fs.Stat(name)
	if err != nil {
		return FolderEntry{}, err
	}
	return FolderEntry{Name: info.Name(), Path: path, Dir: info.IsDir(), Size: info.Size(), Mtime: info.ModTime().Unix()}, nil
}

func (m *Manager) rateOK(bucket map[string][]time.Time, key string, limit int, window time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	kept := bucket[key][:0:0]
	for _, t := range bucket[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		bucket[key] = kept
		return false
	}
	bucket[key] = append(kept, now)
	return true
}

func (h *host) NotifyOwner(ctx context.Context, text string) error {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return err
	}
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	if !h.m.rateOK(h.m.notifyTimes, h.plugin, 5, time.Hour) {
		return fmt.Errorf("plugins: owner notices are limited to five an hour")
	}
	text = h.m.store.redactKnown(ctx, h.plugin, text)
	if h.m.deps.Backends.Notice != nil {
		h.m.deps.Backends.Notice(truncate(text, 500))
	}
	h.m.store.addLog(ctx, h.plugin, "notice: "+text)
	return nil
}

func (h *host) Log(ctx context.Context, level, msg string, attrs map[string]any) error {
	if _, _, err := h.record(ctx); err != nil {
		return err
	}
	if !h.m.rateOK(h.m.logTimes, h.plugin, 600, time.Minute) {
		return nil // drop, never fail the caller for logging
	}
	line := level + ": " + msg
	if len(attrs) > 0 {
		if raw, err := json.Marshal(attrs); err == nil {
			line += " " + redactJSON(raw)
		}
	}
	h.m.store.addLog(ctx, h.plugin, line)
	return nil
}

// FoldersHash keeps small hashes synchronous; large files require an explicit job.
func (h *host) FoldersHash(ctx context.Context, root, path string) (FolderHash, error) {
	return h.hashFile(ctx, root, path, false, nil)
}

func (h *host) hashFile(ctx context.Context, root, path string, background bool, progress func(int64)) (FolderHash, error) {
	const capBytes = 256 << 20
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return FolderHash{}, err
	}
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return FolderHash{}, ErrPermissionDenied
		}
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return FolderHash{}, err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(realRoot, path))
	if err != nil {
		return FolderHash{}, err
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil || filepath.IsAbs(path) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return FolderHash{}, ErrPermissionDenied
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return FolderHash{}, err
	}
	defer fs.Close()
	f, err := fs.Open(path)
	if err != nil {
		return FolderHash{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return FolderHash{}, err
	}
	if !info.Mode().IsRegular() {
		return FolderHash{}, fmt.Errorf("folders.hash requires a regular file")
	}
	if !background && info.Size() > capBytes {
		return FolderHash{}, fmt.Errorf("folders.hash requires a regular file of at most 256 MiB")
	}
	key := fmt.Sprintf("host.hash.%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s:%d:%d", realRoot, rel, info.Size(), info.ModTime().UnixNano()))))
	if raw, ok, err := h.m.store.hashCacheGet(ctx, h.plugin, key); err != nil {
		return FolderHash{}, err
	} else if ok {
		var cached FolderHash
		if json.Unmarshal(raw, &cached) == nil {
			if _, err := h.folderCheck(ctx, root, path); err != nil {
				return FolderHash{}, err
			}
			return cached, nil
		}
	}
	crc, md, sha := crc32.NewIEEE(), md5.New(), sha1.New()
	w := io.MultiWriter(crc, md, sha)
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return FolderHash{}, err
		}
		if _, err := h.folderCheck(ctx, root, path); err != nil {
			return FolderHash{}, err
		}
		n, err := f.Read(buf)
		total += int64(n)
		if !background && total > capBytes {
			return FolderHash{}, fmt.Errorf("folders.hash size limit exceeded")
		}
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if progress != nil {
				progress(total)
			}
			if background {
				timer := time.NewTimer(2 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return FolderHash{}, ctx.Err()
				case <-timer.C:
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return FolderHash{}, err
		}
	}
	after, err := f.Stat()
	if err != nil {
		return FolderHash{}, err
	}
	if total != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return FolderHash{}, fmt.Errorf("folders.hash file changed while hashing")
	}
	result := FolderHash{Size: total, ModTime: info.ModTime().Unix(), CRC32: fmt.Sprintf("%08x", crc.Sum32()), MD5: fmt.Sprintf("%x", md.Sum(nil)), SHA1: fmt.Sprintf("%x", sha.Sum(nil))}
	raw, _ := json.Marshal(result)
	if err := h.m.store.hashCachePut(ctx, h.plugin, key, raw); err != nil {
		return FolderHash{}, err
	}
	return result, nil
}

func (h *host) noteFetchTraffic(ctx context.Context, req FetchRequest, size int64) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return
	}
	h.m.healthMu.Lock()
	defer h.m.healthMu.Unlock()
	if hh, err := h.m.store.health(ctx, h.plugin); err == nil {
		hh.Observed[u.Hostname()]++
		hh.EgressBytes += size + int64(len(req.Body))
		_ = h.m.store.putHealth(ctx, h.plugin, hh)
	}
}
