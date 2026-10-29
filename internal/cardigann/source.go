package cardigann

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/safefetch"
)

// catalogTTL is how long a fetched definitions-source listing is reused.
const catalogTTL = time.Hour

// ErrNoSource reports that no definitions source is configured.
var ErrNoSource = errors.New("no definitions source is set")

// CatalogEntry is one definition a source offers.
type CatalogEntry struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Installed bool   `json:"installed"`
	// UpdateAvailable is set when the installed copy came from this source
	// at a different revision.
	UpdateAvailable bool   `json:"updateAvailable,omitempty"`
	URL             string `json:"-"`
	SHA             string `json:"-"`
}

type catalogCache struct {
	mu      sync.Mutex
	source  string
	entries []CatalogEntry
	at      time.Time
}

// listingURL turns the owner's source URL into the URL that lists the
// definitions: GitHub folder ("github.com/o/r/tree/ref/dir") and raw
// folder ("raw.githubusercontent.com/o/r/ref/dir/") URLs map to the
// GitHub contents API; anything else is fetched as given.
func listingURL(source string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("definitions source %q is not an http(s) URL", source)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	api := func(owner, repo, ref string, dir []string) string {
		v := url.Values{}
		if ref != "" {
			v.Set("ref", ref)
		}
		out := "https://api.github.com/repos/" + owner + "/" + repo + "/contents/" + strings.Join(dir, "/")
		if len(v) > 0 {
			out += "?" + v.Encode()
		}
		return out
	}
	switch strings.ToLower(u.Host) {
	case "github.com", "www.github.com":
		if len(segs) >= 4 && segs[2] == "tree" {
			return api(segs[0], segs[1], segs[3], segs[4:]), nil
		}
	case "raw.githubusercontent.com":
		if len(segs) >= 3 {
			ref, rest := segs[2], segs[3:]
			if ref == "refs" && len(segs) >= 5 {
				ref, rest = segs[4], segs[5:]
			}
			return api(segs[0], segs[1], ref, rest), nil
		}
	}
	return u.String(), nil
}

// sourceTrustedURL returns the listing URL of the owner-configured
// definitions source ("" when none is usable). Its origin is the one
// destination a source fetch may reach on a private address.
func (s *Service) sourceTrustedURL() string {
	listing, err := listingURL(strings.TrimSpace(s.siteSettings().DefinitionsURL))
	if err != nil {
		return ""
	}
	return listing
}

// fetchSource downloads one source document through the safefetch
// guard. The trust boundary: the source URL itself is configured by the
// owner, so its own origin (trusted, usually the listing URL) may be a
// LAN host; everything the remote listing hands back is untrusted data
// — absolute download_url entries, and relative ones too, since a
// network-path reference ("//host/...") or a redirect can still leave
// the source origin. The guard therefore validates every destination at
// dial time: only the trusted origin may resolve privately, every other
// host (and every redirect hop away from the origin) must be public.
// A relative download_url that stays on the source origin inherits only
// that already owner-chosen destination, never a new one.
func (s *Service) fetchSource(ctx context.Context, rawURL string, limit int64, trusted string) ([]byte, error) {
	if trusted == "" {
		trusted = s.sourceTrustedURL()
	}
	req, err := http.NewRequestWithContext(safefetch.WithTrustedOrigin(ctx, trusted), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Ether")
	req.Header.Set("Accept", "application/json, text/plain, application/x-yaml, */*")
	resp, err := s.sourceHC.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("definitions source unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("definitions source returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("definitions source document exceeds %d bytes", limit)
	}
	return data, nil
}

// parseListing reads a source listing: a GitHub contents API array, a JSON
// array of file names or URLs, or plain text with one per line.
func parseListing(data []byte, base *url.URL) []CatalogEntry {
	add := func(out []CatalogEntry, name, link, sha string) []CatalogEntry {
		file := path.Base(strings.SplitN(name, "?", 2)[0])
		ext := strings.ToLower(path.Ext(file))
		if ext != ".yml" && ext != ".yaml" {
			return out
		}
		if link == "" {
			link = name
		}
		ref, err := url.Parse(link)
		if err != nil {
			return out
		}
		return append(out, CatalogEntry{ID: strings.TrimSuffix(file, path.Ext(file)), File: file, URL: base.ResolveReference(ref).String(), SHA: sha})
	}
	var out []CatalogEntry
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var objs []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			SHA         string `json:"sha"`
			DownloadURL string `json:"download_url"`
		}
		if json.Unmarshal(trimmed, &objs) == nil {
			for _, o := range objs {
				if o.Type != "" && o.Type != "file" {
					continue
				}
				out = add(out, o.Name, o.DownloadURL, o.SHA)
			}
			return out
		}
		var names []string
		if json.Unmarshal(trimmed, &names) == nil {
			for _, n := range names {
				out = add(out, n, "", "")
			}
			return out
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = add(out, line, "", "")
	}
	return out
}

// Catalog lists the definitions the configured source offers, marking
// installed ones. refresh bypasses the hour-long cache.
func (s *Service) Catalog(ctx context.Context, refresh bool) ([]CatalogEntry, error) {
	entries, err := s.sourceEntries(ctx, refresh)
	if err != nil {
		return nil, err
	}
	installed, err := s.defs.list(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]storedDefinition{}
	for _, d := range installed {
		byID[d.ID] = d
	}
	out := make([]CatalogEntry, len(entries))
	for i, e := range entries {
		if d, ok := byID[e.ID]; ok {
			e.Installed = true
			e.UpdateAvailable = d.Origin != "" && e.SHA != "" && d.SHA != "" && d.SHA != e.SHA
		}
		out[i] = e
	}
	return out, nil
}

func (s *Service) sourceEntries(ctx context.Context, refresh bool) ([]CatalogEntry, error) {
	source := strings.TrimSpace(s.siteSettings().DefinitionsURL)
	if source == "" {
		return nil, ErrNoSource
	}
	s.mu.Lock()
	cache := s.catalog
	s.mu.Unlock()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !refresh && cache.source == source && s.now().Sub(cache.at) < catalogTTL && cache.entries != nil {
		return cache.entries, nil
	}
	listing, err := listingURL(source)
	if err != nil {
		return nil, err
	}
	data, err := s.fetchSource(ctx, listing, 8<<20, listing)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(listing)
	entries := parseListing(data, base)
	if len(entries) == 0 {
		return nil, errors.New("the definitions source lists no .yml definitions")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	cache.source, cache.entries, cache.at = source, entries, s.now()
	return entries, nil
}

// InstallFromSource downloads one definition from the source and installs it.
func (s *Service) InstallFromSource(ctx context.Context, id string) (Summary, error) {
	entries, err := s.sourceEntries(ctx, false)
	if err != nil {
		return Summary{}, err
	}
	var entry *CatalogEntry
	for i := range entries {
		if entries[i].ID == id {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return Summary{}, fmt.Errorf("the definitions source does not offer %q", id)
	}
	data, err := s.fetchSource(ctx, entry.URL, maxDefinitionBytes, "")
	if err != nil {
		return Summary{}, err
	}
	return s.Install(ctx, data, entry.URL, entry.SHA)
}

// UpdateReport summarises "Update definitions".
type UpdateReport struct {
	Updated   []string       `json:"updated"`
	Unchanged int            `json:"unchanged"`
	Failed    []UpdateFailed `json:"failed"`
}

// UpdateFailed is a definition that could not be refreshed.
type UpdateFailed struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Update refreshes every definition installed from a source. Uploaded
// definitions are left alone.
func (s *Service) Update(ctx context.Context) (UpdateReport, error) {
	rep := UpdateReport{Updated: []string{}, Failed: []UpdateFailed{}}
	installed, err := s.defs.list(ctx)
	if err != nil {
		return rep, err
	}
	// The listing (when reachable) tells which files changed and where
	// they live now; otherwise each definition is re-fetched from its origin.
	byID := map[string]CatalogEntry{}
	if entries, err := s.sourceEntries(ctx, true); err == nil {
		for _, e := range entries {
			byID[e.ID] = e
		}
	}
	for _, d := range installed {
		if d.Origin == "" {
			continue
		}
		link, sha := d.Origin, ""
		if e, ok := byID[d.ID]; ok {
			link, sha = e.URL, e.SHA
			if sha != "" && sha == d.SHA {
				rep.Unchanged++
				continue
			}
		}
		data, err := s.fetchSource(ctx, link, maxDefinitionBytes, "")
		if err != nil {
			rep.Failed = append(rep.Failed, UpdateFailed{ID: d.ID, Error: err.Error()})
			continue
		}
		if bytes.Equal(data, d.YAML) {
			rep.Unchanged++
			if sha != "" && sha != d.SHA {
				d.SHA = sha
				_ = s.defs.put(ctx, d)
			}
			continue
		}
		def, _, err := Parse(data)
		if err != nil {
			rep.Failed = append(rep.Failed, UpdateFailed{ID: d.ID, Error: err.Error()})
			continue
		}
		if def.ID != d.ID {
			rep.Failed = append(rep.Failed, UpdateFailed{ID: d.ID, Error: fmt.Sprintf("the source now serves definition %q", def.ID)})
			continue
		}
		if _, err := s.Install(ctx, data, link, sha); err != nil {
			rep.Failed = append(rep.Failed, UpdateFailed{ID: d.ID, Error: err.Error()})
			continue
		}
		rep.Updated = append(rep.Updated, d.ID)
	}
	return rep, nil
}
