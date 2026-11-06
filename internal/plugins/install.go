package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/plugins/pkgdigest"
	"github.com/braedonsaunders/ether/internal/stremio"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/safefetch"
)

// Source describes where an install comes from.
type Source struct {
	Kind string // url|github|pkg|studio|catalogue
	URL  string
	// ID and Version select an entry from the signed catalogue (kind "catalogue").
	ID      string
	Version string
	// SHA256 is the expected lowercase hex SHA-256 of the downloaded artifact.
	SHA256   string
	reviewed *CatalogueArtifact
	// Pkg is the .etherpkg bytes (kind "pkg") or a Studio package.
	Pkg []byte
	// Manifest and Files install directly (Studio with loose files).
	Manifest json.RawMessage
	Files    map[string][]byte
	// Verified is set only by the signed catalogue (WS12).
	Verified bool
	// LAN allows a private-network manifest URL (owner confirmed).
	LAN bool
}

// Preview is what the install sheet shows before the owner confirms.
type Preview struct {
	Manifest         Manifest `json:"manifest"`
	PermissionLines  []string `json:"permissions"`
	NeedsViewers     bool     `json:"needsViewers"`
	NeedsFolders     bool     `json:"needsFolders"`
	Verified         bool     `json:"verified"`
	StremioOnly      bool     `json:"stremioOnly"`
	AlreadyInstalled bool     `json:"alreadyInstalled,omitempty"`
	InstalledVersion string   `json:"installedVersion,omitempty"`
	AddedPermissions []string `json:"addedPermissions,omitempty"`
	source           Source
	files            map[string][]byte
	manifestRaw      json.RawMessage
	resolvedSettings json.RawMessage
	resolvedStrings  map[string]map[string]string
}

// IsGitHubURL classifies a github.com repo URL.
func IsGitHubURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Hostname(), "github.com") &&
		len(strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })) >= 2 &&
		!strings.HasSuffix(u.Path, "/manifest.json")
}

func (m *Manager) fetch(ctx context.Context, rawURL string, lan bool, max int64) ([]byte, error) {
	if lan {
		ctx = safefetch.WithTrustedOrigin(ctx, rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("plugins: invalid fetch URL")
	}
	req.Header.Set("User-Agent", "Ether plugin host")
	client := *m.client
	priorRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		next.Header.Del("Referer")
		if priorRedirect != nil {
			return priorRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("plugins: too many redirects")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("plugins: fetch failed")
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugins: fetch returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("plugins: fetch is larger than %d bytes", max)
	}
	return body, nil
}

func verifyArtifactDigest(data []byte, wantHex string) error {
	want := strings.ToLower(strings.TrimSpace(wantHex))
	if len(want) != 64 {
		return fmt.Errorf("%w: invalid sha256 %q", ErrDigestMismatch, wantHex)
	}
	got := digestOf(data)
	if got != want {
		return fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, want)
	}
	return nil
}

func (m *Manager) resolveCatalogueSource(ctx context.Context, src Source) (Source, error) {
	if src.Kind != "catalogue" {
		return src, nil
	}
	if cat := m.getCatalogue(); cat != nil {
		key := src.ID
		if key == "" {
			key = src.URL
		}
		art, err := cat.Resolve(ctx, key, src.Version)
		if err != nil {
			return src, err
		}
		src.ID = art.ID
		src.Version = art.Version
		src.URL = art.URL
		src.SHA256 = art.SHA256
		src.reviewed = &art
		src.Verified = true
		return src, nil
	}
	return src, fmt.Errorf("plugins: no signed catalogue is configured")
}

// FetchManifest resolves a source to a manifest and its package files.
// GitHub repo URLs read the manifest from the latest release's .etherpkg
// asset, else manifest.json on the default branch.
func (m *Manager) loadSource(ctx context.Context, src Source) (raw json.RawMessage, files map[string][]byte, err error) {
	files = map[string][]byte{}
	switch src.Kind {
	case "pkg", "studio":
		if len(src.Manifest) > 0 {
			for k, v := range src.Files {
				files[k] = v
			}
			files["manifest.json"] = src.Manifest
			return src.Manifest, files, nil
		}
		if src.SHA256 != "" {
			if err := verifyArtifactDigest(src.Pkg, src.SHA256); err != nil {
				return nil, nil, err
			}
		}
		return readPkg(src.Pkg)
	case "url", "github", "catalogue", "":
		if src.Kind == "url" && len(src.Manifest) > 0 {
			// A link whose manifest the caller already holds (a migrated
			// add-on recorded while its host was unreachable); the hourly
			// refresh replaces it with the add-on's own.
			files["manifest.json"] = src.Manifest
			return src.Manifest, files, nil
		}
		if len(src.Pkg) > 0 {
			if src.SHA256 != "" {
				if err := verifyArtifactDigest(src.Pkg, src.SHA256); err != nil {
					return nil, nil, err
				}
			}
			return readPkg(src.Pkg)
		}
		if src.Kind == "catalogue" || strings.HasSuffix(src.URL, ".etherpkg") {
			b, err := m.fetch(ctx, src.URL, src.LAN, 64<<20)
			if err != nil {
				return nil, nil, err
			}
			if src.SHA256 != "" {
				if err := verifyArtifactDigest(b, src.SHA256); err != nil {
					return nil, nil, err
				}
			}
			return readPkg(b)
		}
		if IsGitHubURL(src.URL) {
			return m.loadGitHub(ctx, src)
		}
		// Install links are Stremio-style: stremio://, a configure page
		// or a manifest URL, with configuration in the path or query.
		base, err := stremio.NormalizeURL(src.URL)
		if err != nil {
			return nil, nil, err
		}
		manURL := stremio.ManifestURL(base)
		b, err := m.fetch(ctx, manURL, src.LAN, 1<<20)
		if err != nil {
			return nil, nil, err
		}
		if src.SHA256 != "" {
			if err := verifyArtifactDigest(b, src.SHA256); err != nil {
				return nil, nil, err
			}
		}
		files["manifest.json"] = b
		// Sibling files the manifest points at (strings, schema, entry)
		// are fetched best-effort from the same directory.
		return b, m.fetchSiblings(ctx, manURL, b, files, src.LAN), nil
	default:
		return nil, nil, fmt.Errorf("plugins: unknown source kind %q", src.Kind)
	}
}

func (m *Manager) fetchSiblings(ctx context.Context, manURL string, manRaw []byte, files map[string][]byte, lan bool) map[string][]byte {
	man, err := ParseManifest(manRaw)
	if err != nil || man.Ether == nil {
		return files
	}
	base := strings.TrimSuffix(manURL, "manifest.json")
	var paths []string
	if icon := man.Ether.Icon; icon != "" && !strings.HasPrefix(icon, "http://") && !strings.HasPrefix(icon, "https://") {
		paths = append(paths, icon)
	}
	if man.Ether.Runtime.Entry != "" {
		paths = append(paths, man.Ether.Runtime.Entry)
	}
	if man.Ether.Runtime.Module != "" {
		paths = append(paths, man.Ether.Runtime.Module)
	}
	for _, raw := range man.Ether.Strings {
		var p string
		if json.Unmarshal(raw, &p) == nil && p != "" {
			paths = append(paths, p)
		}
	}
	if ref := settingsRef(man.Ether.Settings); ref != "" {
		paths = append(paths, ref)
	}
	for _, p := range paths {
		if unsafePath(p) {
			continue
		}
		if b, err := m.fetch(ctx, base+p, lan, 32<<20); err == nil {
			files[p] = b
		}
	}
	return files
}

func (m *Manager) loadGitHub(ctx context.Context, src Source) (json.RawMessage, map[string][]byte, error) {
	u, _ := url.Parse(src.URL)
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	if b, err := m.fetch(ctx, api, false, 1<<20); err == nil {
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if json.Unmarshal(b, &rel) == nil {
			for _, a := range rel.Assets {
				if strings.HasSuffix(a.Name, ".etherpkg") {
					pkg, err := m.fetch(ctx, a.URL, false, 64<<20)
					if err != nil {
						return nil, nil, err
					}
					return readPkg(pkg)
				}
			}
		}
	}
	branch := "main"
	if b, err := m.fetch(ctx, fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo), false, 1<<20); err == nil {
		var repoInfo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if json.Unmarshal(b, &repoInfo) == nil && repoInfo.DefaultBranch != "" {
			branch = repoInfo.DefaultBranch
		}
	}
	manURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/manifest.json", owner, repo, branch)
	b, err := m.fetch(ctx, manURL, false, 1<<20)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{"manifest.json": b}
	return b, m.fetchSiblings(ctx, manURL, b, files, false), nil
}

// readPkg reads an .etherpkg: a zip holding manifest.json, compiled JS or
// WASM, assets and strings. A `digest` entry (hex SHA-256 of the sorted
// path=digest lines) is verified when present.
func readPkg(pkg []byte) (json.RawMessage, map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return nil, nil, fmt.Errorf("plugins: package: %w", err)
	}
	files := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if f.FileInfo().IsDir() || unsafePath(name) || strings.HasPrefix(name, "__MACOSX") {
			continue
		}
		if _, exists := files[name]; exists {
			return nil, nil, fmt.Errorf("plugins: duplicate package path")
		}
		if f.UncompressedSize64 > 64<<20 {
			return nil, nil, fmt.Errorf("plugins: package entry is too large")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, (64<<20)+1))
		rc.Close() //nolint:errcheck,gosec
		if err != nil {
			return nil, nil, err
		}
		if len(b) > 64<<20 {
			return nil, nil, fmt.Errorf("plugins: package entry is too large")
		}
		total += int64(len(b))
		if total > 128<<20 {
			return nil, nil, fmt.Errorf("plugins: package is too large")
		}
		files[name] = b
	}
	man, ok := files["manifest.json"]
	if !ok {
		return nil, nil, fmt.Errorf("plugins: package has no manifest.json")
	}
	if d, ok := files["digest"]; ok {
		if strings.TrimSpace(string(d)) != pkgDigest(files) {
			return nil, nil, fmt.Errorf("plugins: package digest does not match its contents")
		}
	}
	return man, files, nil
}

// PkgDigest computes an .etherpkg digest over its files (excluding the
// digest entry itself). pack (WS11) writes it; install verifies it.
func PkgDigest(files map[string][]byte) string { return pkgDigest(files) }

func pkgDigest(files map[string][]byte) string { return pkgdigest.Sum(files) }

func settingsRef(raw json.RawMessage) string {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) == nil {
		if ref, ok := doc["$ref"].(string); ok {
			return ref
		}
	}
	return ""
}

// resolve fills the preview's settings schema and strings from the
// package files, and validates the resolved schema (SPEC §5.5).
func (p *Preview) resolve() error {
	if err := validateSegmentFiles(p.Manifest, p.files); err != nil {
		return err
	}
	man := p.Manifest
	if man.Ether == nil {
		return nil
	}
	p.resolvedSettings = man.Ether.Settings
	if ref := settingsRef(man.Ether.Settings); ref != "" {
		b, ok := p.files[ref]
		if !ok {
			return &ValidationError{[]FieldError{{"ether.settings.$ref", "the settings file is missing from the package"}}}
		}
		p.resolvedSettings = b
	}
	if err := LintSettingsSchema(p.resolvedSettings); err != nil {
		return err
	}
	p.resolvedStrings = map[string]map[string]string{}
	for loc, raw := range man.Ether.Strings {
		var path string
		var doc map[string]string
		if json.Unmarshal(raw, &path) == nil {
			b, ok := p.files[path]
			if !ok {
				return &ValidationError{[]FieldError{{"ether.strings." + loc, "the strings file is missing from the package"}}}
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				return &ValidationError{[]FieldError{{"ether.strings." + loc, "the strings file is not a JSON object of text"}}}
			}
		} else if err := json.Unmarshal(raw, &doc); err != nil {
			return &ValidationError{[]FieldError{{"ether.strings." + loc, "must be a file path or an object of text"}}}
		}
		p.resolvedStrings[loc] = doc
	}
	if man.Ether.Runtime.Kind == "hosted" {
		entry := man.Ether.Runtime.Entry
		if entry == "" {
			entry = man.Ether.Runtime.Module
		}
		if p.source.Kind == "pkg" || p.source.Kind == "catalogue" {
			if _, ok := p.files[entry]; !ok {
				return &ValidationError{[]FieldError{{"ether.runtime", "the entry file is missing from the package"}}}
			}
		}
	}
	return nil
}

// PreviewInstall fetches and validates a source without installing it.
func (m *Manager) PreviewInstall(ctx context.Context, src Source) (*Preview, error) {
	resolved, err := m.resolveCatalogueSource(ctx, src)
	if err != nil {
		return nil, err
	}
	src = resolved
	raw, files, err := m.loadSource(ctx, src)
	if err != nil {
		return nil, err
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if man.IsStremioOnly() && src.URL != "" {
		// One record per configured add-on: the same add-on installed with
		// two configurations (two debrid accounts) is two records, and the
		// same link pasted on another page is the same record. The record id
		// comes from the link, so an add-on's own id may be anything Stremio
		// accepts ("tmdb-addon", "stremio.addons.mediafusion|elfhosted").
		id, base, err := StremioInstallID(src.URL)
		if err != nil {
			return nil, err
		}
		man.ID, src.URL = id, base
	}
	if err := man.Validate(); err != nil {
		return nil, err
	}
	if src.reviewed != nil {
		reviewed := src.reviewed
		if man.Ether == nil || man.Ether.Kind != reviewed.Kind {
			return nil, fmt.Errorf("plugins: manifest kind does not match the catalogue review")
		}
		approved := Manifest{Ether: &EtherBlock{Permissions: reviewed.Permissions, Capabilities: reviewed.Capabilities}}
		if added := PermissionDiff(approved, man); len(added) != 0 {
			return nil, fmt.Errorf("plugins: manifest requests permissions beyond the catalogue review: %s", strings.Join(added, "; "))
		}
	}
	if src.ID != "" && man.ID != src.ID {
		return nil, fmt.Errorf("plugins: manifest id %q does not match catalogue entry %q", man.ID, src.ID)
	}
	if src.Version != "" && man.Version != src.Version {
		return nil, fmt.Errorf("plugins: manifest version %q does not match catalogue version %q", man.Version, src.Version)
	}
	p := &Preview{Manifest: man, source: src, files: files, manifestRaw: raw, Verified: src.Verified, StremioOnly: man.IsStremioOnly()}
	p.PermissionLines, p.NeedsViewers, p.NeedsFolders = PermissionSheet(man)
	if err := p.resolve(); err != nil {
		return nil, err
	}
	// The conformance kit checks packages Ether runs; a plain Stremio add-on
	// is a remote service whose manifest was validated above.
	if m.deps.CheckPackage != nil && !man.IsStremioOnly() {
		if err := m.deps.CheckPackage(ctx, raw, files); err != nil {
			return nil, err
		}
	}
	if cur, err := m.store.record(ctx, man.ID); err == nil {
		p.AlreadyInstalled = true
		p.InstalledVersion = cur.Version
		p.AddedPermissions = PermissionDiff(cur.Manifest, man)
	}
	return p, nil
}

// InstallOptions carries the owner's install-time picks.
type InstallOptions struct {
	Grants     Grants
	Settings   map[string]any
	AutoUpdate bool
	Shadow     bool
	// AcceptAddedPermissions confirms an update that asks for more.
	AcceptAddedPermissions bool
}

// Install installs (or updates to) a previewed source.
func (m *Manager) Install(ctx context.Context, p *Preview, opts InstallOptions) (Record, error) {
	man := p.Manifest
	if cat := m.getCatalogue(); cat != nil {
		if p.source.Kind == "catalogue" {
			current, err := cat.Resolve(ctx, man.ID, man.Version)
			if err != nil {
				return Record{}, err
			}
			if p.source.reviewed == nil || current.URL != p.source.URL || !strings.EqualFold(current.SHA256, p.source.SHA256) {
				return Record{}, fmt.Errorf("plugins: catalogue entry changed since the package was reviewed")
			}
			approved := Manifest{Ether: &EtherBlock{Permissions: current.Permissions, Capabilities: current.Capabilities}}
			if man.Ether == nil || man.Ether.Kind != current.Kind || len(PermissionDiff(approved, man)) != 0 {
				return Record{}, fmt.Errorf("plugins: catalogue review changed since the package was reviewed")
			}
		} else if _, err := cat.Resolve(ctx, man.ID, ""); err != nil && errors.Is(err, ErrRevoked) {
			return Record{}, err
		}
	} else if p.source.Kind == "catalogue" {
		return Record{}, fmt.Errorf("plugins: no signed catalogue is configured")
	}
	cur, curErr := m.store.record(ctx, man.ID)
	existing := curErr == nil
	if existing {
		if CompareSemVer(man.Version, cur.Version) <= 0 && man.Version != cur.Version {
			// Same-version reinstall is allowed (Studio); older is not.
			if CompareSemVer(man.Version, cur.Version) < 0 {
				return Record{}, fmt.Errorf("plugins: version %s is older than the installed %s; use rollback", man.Version, cur.Version)
			}
		}
		added := PermissionDiff(cur.Manifest, man)
		if len(added) > 0 && !opts.AcceptAddedPermissions {
			return Record{}, &PermissionIncreaseError{Added: added}
		}
	}
	if man.Ether != nil && man.Ether.Runtime.Kind == "data" {
		if m.deps.ImportStylePack == nil {
			return Record{}, fmt.Errorf("plugins: style import is unavailable")
		}
		if err := m.deps.ImportStylePack(ctx, man.ID, p.files); err != nil {
			return Record{}, err
		}
	}
	if err := m.store.putFiles(ctx, man.ID, man.Version, p.files); err != nil {
		return Record{}, err
	}
	r := Record{
		ID: man.ID, Name: man.Name, Version: man.Version, Manifest: man, ManifestRaw: p.manifestRaw,
		SourceKind: sourceKindOf(p.source), SourceURL: p.source.URL, Enabled: true,
		Verified: p.source.Verified, AutoUpdate: opts.AutoUpdate, Shadow: opts.Shadow,
	}
	if man.Ether != nil {
		r.Kind = string(man.Ether.Kind)
	}
	if existing {
		r.InstalledAt = cur.InstalledAt
		r.Enabled = cur.Enabled
		if cur.Version != man.Version {
			r.PrevVersion = cur.Version
		} else {
			r.PrevVersion = cur.PrevVersion
		}
	}
	if err := m.store.upsert(ctx, r); err != nil {
		return Record{}, err
	}
	// Grants: keep existing picks on update unless new ones were given.
	g := opts.Grants
	if existing && g.Viewers == nil && g.Folders == nil && g.Revoked == nil {
		if old, err := m.store.grants(ctx, man.ID); err == nil {
			g = old
		}
	}
	if p.source.LAN {
		g.LAN = true
	}
	if err := m.store.putGrants(ctx, man.ID, g); err != nil {
		return Record{}, err
	}
	row, _ := m.store.settings(ctx, man.ID)
	if opts.Settings != nil {
		row.Settings = opts.Settings
	}
	row.Strings = p.resolvedStrings
	row.Schema = p.resolvedSettings
	if err := m.store.putSettings(ctx, man.ID, row); err != nil {
		return Record{}, err
	}
	if h, err := m.store.health(ctx, man.ID); err == nil && h.Status == "" {
		_ = m.store.putHealth(ctx, man.ID, Health{Status: "ok", Observed: map[string]int{}})
	}
	m.closeRuntime(man.ID)
	saved, err := m.store.record(ctx, man.ID)
	if err != nil {
		return Record{}, err
	}
	m.syncDescriptors(ctx, saved)
	m.broadcast(ctx)
	return saved, nil
}

func sourceKindOf(s Source) string {
	if s.Kind != "" {
		return s.Kind
	}
	if IsGitHubURL(s.URL) {
		return "github"
	}
	return "url"
}

// PermissionIncreaseError reports an update that asks for more access.
type PermissionIncreaseError struct{ Added []string }

func (e *PermissionIncreaseError) Error() string {
	return "plugins: this update asks for more access: " + strings.Join(e.Added, "; ")
}

// CheckUpdate fetches the plugin's source again. When a newer version
// exists it is stored as a pending update; auto-update applies it at once
// only for verified plugins whose permissions did not grow (SPEC §5.4).
func (m *Manager) CheckUpdate(ctx context.Context, id string) (*Preview, bool, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if (r.SourceURL == "" && r.SourceKind != "catalogue") || r.SourceKind == "studio" {
		// A Studio build's link (an edited add-on's) is not an update source.
		return nil, false, fmt.Errorf("plugins: %s has no update source", id)
	}
	g, _ := m.store.grants(ctx, id)
	src := Source{Kind: r.SourceKind, URL: r.SourceURL, LAN: g.LAN, Verified: r.Verified}
	if r.SourceKind == "catalogue" {
		src.ID = r.ID
	}
	p, err := m.PreviewInstall(ctx, src)
	if err != nil {
		return nil, false, err
	}
	if r.Manifest.IsStremioOnly() && p.Manifest.IsStremioOnly() {
		// Same link, same add-on record, whatever id its manifest names.
		p.Manifest.ID = r.ID
		// A Stremio add-on changes its catalogs and resources without
		// versioning them, and asks for no permissions: its current
		// manifest always applies.
		if bytes.Equal(p.manifestRaw, r.ManifestRaw) {
			return p, false, nil
		}
		_, err := m.Install(ctx, p, InstallOptions{AutoUpdate: r.AutoUpdate, Shadow: r.Shadow})
		return p, true, err
	}
	if CompareSemVer(p.Manifest.Version, r.Version) <= 0 {
		return p, false, nil
	}
	added := PermissionDiff(r.Manifest, p.Manifest)
	if r.AutoUpdate && r.Verified && len(added) == 0 {
		if _, err := m.Install(ctx, p, InstallOptions{AutoUpdate: r.AutoUpdate, Shadow: r.Shadow}); err != nil {
			return p, true, err
		}
		return p, true, nil
	}
	r.UpdateVersion = p.Manifest.Version
	r.updateManifest = string(p.manifestRaw)
	filesRaw, err := json.Marshal(p.files)
	if err != nil {
		return nil, false, err
	}
	r.updateFiles = string(filesRaw)
	if err := m.store.upsert(ctx, r); err != nil {
		return nil, false, err
	}
	return p, true, nil
}

// ApplyUpdate installs a pending update previously stored by CheckUpdate.
func (m *Manager) ApplyUpdate(ctx context.Context, id string, acceptAdded bool) (Record, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if r.UpdateVersion == "" || r.updateManifest == "" {
		return Record{}, fmt.Errorf("plugins: no update is waiting for %s", id)
	}
	if r.SourceKind == "catalogue" {
		g, _ := m.store.grants(ctx, id)
		p, err := m.PreviewInstall(ctx, Source{Kind: "catalogue", ID: id, Version: r.UpdateVersion, LAN: g.LAN})
		if err != nil {
			return Record{}, err
		}
		return m.Install(ctx, p, InstallOptions{AutoUpdate: r.AutoUpdate, Shadow: r.Shadow, AcceptAddedPermissions: acceptAdded})
	}
	man, err := ParseManifest([]byte(r.updateManifest))
	if err != nil {
		return Record{}, err
	}
	man.ID = r.ID
	var files map[string][]byte
	if err := json.Unmarshal([]byte(r.updateFiles), &files); err != nil {
		return Record{}, err
	}
	p := &Preview{Manifest: man, files: files, manifestRaw: json.RawMessage(r.updateManifest),
		source: Source{Kind: r.SourceKind, URL: r.SourceURL, Verified: r.Verified}}
	if err := p.resolve(); err != nil {
		return Record{}, err
	}
	saved, err := m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: acceptAdded, AutoUpdate: r.AutoUpdate, Shadow: r.Shadow})
	if err != nil {
		return Record{}, err
	}
	saved.UpdateVersion, saved.updateManifest, saved.updateFiles = "", "", ""
	_ = m.store.upsert(ctx, saved)
	return saved, nil
}

// Rollback returns a plugin to its previous version (one step).
func (m *Manager) Rollback(ctx context.Context, id string) (Record, error) {
	r, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if r.PrevVersion == "" {
		return Record{}, fmt.Errorf("plugins: %s has no earlier version to go back to", id)
	}
	manRaw, ok := m.store.file(ctx, id, r.PrevVersion, "manifest.json")
	if !ok {
		return Record{}, fmt.Errorf("plugins: the earlier version of %s is no longer stored", id)
	}
	man, err := ParseManifest(manRaw)
	if err != nil {
		return Record{}, err
	}
	man.ID = r.ID
	prev := r.PrevVersion
	r.Version, r.Manifest, r.ManifestRaw, r.Name = prev, man, manRaw, man.Name
	if man.Ether != nil {
		r.Kind = string(man.Ether.Kind)
	}
	r.PrevVersion = ""
	r.UpdateVersion, r.updateManifest, r.updateFiles = "", "", ""
	if err := m.store.upsert(ctx, r); err != nil {
		return Record{}, err
	}
	// Strings and schema come from the rolled-back package files.
	p := &Preview{Manifest: man, files: map[string][]byte{}, source: Source{Kind: r.SourceKind}}
	// Reload known files for the target version so resolution works.
	for _, name := range artifactPaths(man) {
		if b, ok := m.store.file(ctx, id, prev, name); ok {
			p.files[name] = b
		}
	}
	if err := p.resolve(); err == nil {
		row, _ := m.store.settings(ctx, id)
		row.Strings, row.Schema = p.resolvedStrings, p.resolvedSettings
		_ = m.store.putSettings(ctx, id, row)
	}
	m.closeRuntime(id)
	saved, err := m.store.record(ctx, id)
	if err != nil {
		return Record{}, err
	}
	m.syncDescriptors(ctx, saved)
	m.broadcast(ctx)
	return saved, nil
}

func artifactPaths(man Manifest) []string {
	if man.Ether == nil {
		return nil
	}
	var out []string
	if man.Ether.Runtime.Entry != "" {
		out = append(out, man.Ether.Runtime.Entry)
	}
	if man.Ether.Runtime.Module != "" {
		out = append(out, man.Ether.Runtime.Module)
	}
	if ref := settingsRef(man.Ether.Settings); ref != "" {
		out = append(out, ref)
	}
	for _, raw := range man.Ether.Strings {
		var p string
		if json.Unmarshal(raw, &p) == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- configure, test, options, health ---

const savedSecret = "Saved"

// SettingsSchema returns the resolved settings schema for a plugin.
func (m *Manager) SettingsSchema(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := m.store.record(ctx, id); err != nil {
		return nil, err
	}
	row, err := m.store.settings(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(row.Schema) == 0 {
		return json.RawMessage("{}"), nil
	}
	return row.Schema, nil
}

// secretFieldNames walks a settings schema for secret-kind field names.
func secretFieldNames(schema json.RawMessage) map[string]bool {
	out := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if t["kind"] == "secret" {
				if name, ok := firstString(t, "key", "name", "id", "field"); ok {
					out[name] = true
				}
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	var doc any
	if json.Unmarshal(schema, &doc) == nil {
		walk(doc)
	}
	return out
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// GetSettings returns settings with secrets replaced by "Saved".
func (m *Manager) GetSettings(ctx context.Context, id string) (map[string]any, json.RawMessage, error) {
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	row, err := m.store.settings(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	out := NonSecretSettings(row.Settings, row.Schema, settingsSecrets(rec))
	for name := range row.Secrets {
		if row.Secrets[name] != "" {
			out[name] = savedSecret
		}
	}
	return out, row.Schema, nil
}

// mergedSettings returns the full settings document (secrets opened) for
// delivery to the plugin through /ether/configure. Never logged.
func (m *Manager) mergedSettings(ctx context.Context, id string) (map[string]any, error) {
	rec, err := m.store.record(ctx, id)
	if err != nil {
		return nil, err
	}
	row, err := m.store.settings(ctx, id)
	if err != nil {
		return nil, err
	}
	out := NonSecretSettings(row.Settings, row.Schema, settingsSecrets(rec))
	for name, stored := range row.Secrets {
		plain, err := m.store.openSecret(id, name, stored)
		if err != nil {
			return nil, err
		}
		out[name] = plain
	}
	return out, nil
}

// Configure validates incoming settings against the schema, stores them
// (secrets sealed per plugin; a "Saved" value keeps the stored secret),
// then calls the plugin's /ether/configure and surfaces its field errors.
func (m *Manager) Configure(ctx context.Context, id string, incoming map[string]any) (map[string]string, error) {
	lock := m.notificationLock(id)
	lock.Lock()
	defer lock.Unlock()
	r, err := m.store.record(ctx, id)
	if err != nil {
		return nil, err
	}
	row, err := m.store.settings(ctx, id)
	if err != nil {
		return nil, err
	}
	withDefaults := NonSecretSettings(nil, row.Schema, settingsSecrets(r))
	for key, value := range incoming {
		withDefaults[key] = value
	}
	incoming = withDefaults
	if err := validateSettingsValues(row.Schema, incoming); err != nil {
		return nil, err
	}
	secretNames := secretFieldNames(row.Schema)
	if r.Manifest.Ether != nil {
		for _, s := range r.Manifest.Ether.Permissions.Secrets {
			secretNames[s] = true
		}
	}
	settings := map[string]any{}
	secrets := map[string]string{}
	for k, v := range row.Secrets {
		secrets[k] = v
	}
	for k, v := range incoming {
		if secretNames[k] {
			s, _ := v.(string)
			if s == savedSecret {
				continue
			}
			sealed, err := m.store.sealSecret(id, k, s)
			if err != nil {
				return nil, err
			}
			if s == "" {
				delete(secrets, k)
			} else {
				secrets[k] = sealed
			}
			continue
		}
		settings[k] = v
	}
	row.Settings, row.Secrets = settings, secrets
	if err := m.store.putSettings(ctx, id, row); err != nil {
		return nil, err
	}
	// Deliver to the plugin. No runtime yet (WS8) is not an error: the
	// settings are stored and will be delivered on first start.
	merged, err := m.mergedSettings(ctx, id)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(merged)
	resp, err := m.Call(ctx, id, Request{Method: "POST", Path: "/ether/configure", Body: body})
	if err != nil {
		if err == ErrNoRuntime || err == ErrDisabled {
			return nil, nil
		}
		return nil, err
	}
	var out struct {
		Errors map[string]string `json:"errors"`
	}
	if len(resp.Body) > 0 {
		_ = json.Unmarshal(resp.Body, &out)
	}
	resolved := map[string]string{}
	for k, v := range out.Errors {
		if strings.HasPrefix(v, "$t:") {
			if s, ok := m.ResolveText(ctx, id, strings.TrimPrefix(v, "$t:"), "en"); ok {
				v = s
			}
		}
		resolved[k] = v
	}
	if len(resolved) == 0 && r.Manifest.HasCapability(CapDebridProvider) {
		m.syncDescriptors(ctx, r)
	}
	return resolved, nil
}

// validateSettingsValues does server-side structural checks: required
// fields present, no json-kind fields, value types roughly match kinds.
func validateSettingsValues(schema json.RawMessage, values map[string]any) error {
	var f []FieldError
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			name, hasName := firstString(t, "key", "name", "id", "field")
			kind, _ := t["kind"].(string)
			if hasName && kind != "" && kind != "group" {
				if req, _ := t["required"].(bool); req {
					if v, ok := values[name]; !ok || v == "" {
						f = append(f, FieldError{"/" + name, "is required"})
					}
				}
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	var doc any
	if json.Unmarshal(schema, &doc) == nil {
		walk(doc)
	}
	if len(f) > 0 {
		return &ValidationError{f}
	}
	return nil
}

// TestResult is the plugin's /ether/test answer.
type TestResult struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message,omitempty"`
	Details []string `json:"details,omitempty"`
}

// Test calls the plugin's /ether/test.
func (m *Manager) Test(ctx context.Context, id string) (TestResult, error) {
	resp, err := m.Call(ctx, id, Request{Method: "POST", Path: "/ether/test"})
	if err != nil {
		return TestResult{}, err
	}
	var out TestResult
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &out); err != nil {
			return TestResult{}, fmt.Errorf("plugins: test answer from %s was not understood", id)
		}
	}
	if out.OK {
		m.recordOK(ctx, id)
	}
	return out, nil
}

// Options calls /ether/options/{field} for runtime field options.
func (m *Manager) Options(ctx context.Context, id, field string) (json.RawMessage, error) {
	resp, err := m.Call(ctx, id, Request{Method: "GET", Path: "/ether/options/" + field})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CheckHealth calls /ether/health once and updates the health record.
// A failing call feeds the failure policy through Call already; a plugin
// reporting attention or error is recorded without counting as a crash.
func (m *Manager) CheckHealth(ctx context.Context, id string) (Health, error) {
	resp, err := m.Call(ctx, id, Request{Method: "GET", Path: "/ether/health"})
	if err != nil {
		h, herr := m.store.health(ctx, id)
		if herr != nil {
			return Health{}, herr
		}
		return h, nil
	}
	var out struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if len(resp.Body) > 0 {
		_ = json.Unmarshal(resp.Body, &out)
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	h, err := m.store.health(ctx, id)
	if err != nil {
		return Health{}, err
	}
	switch out.Status {
	case "attention", "error":
		h.Status, h.Reason = out.Status, out.Reason
	default:
		okUnlessSkipping(&h)
		h.LastOK = m.now()
	}
	if err := m.store.putHealth(ctx, id, h); err != nil {
		return Health{}, err
	}
	return h, nil
}

// StremioInstallID is the record id of a plain Stremio add-on installed
// from link, and the normalised base URL it is reached at. The id depends
// on the link alone: the same link pasted on any page is the same add-on,
// even when its manifest changes, and two configurations of one add-on
// (two debrid accounts) are two add-ons.
func StremioInstallID(link string) (id, base string, err error) {
	u, err := stremio.NormalizeURL(link)
	if err != nil {
		return "", "", err
	}
	return "addon." + stremio.Fingerprint(u)[:16], u.String(), nil
}
