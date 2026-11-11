package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/updater"
)

// ensurePlexAccountServices lazily initializes Plex account metadata and
// per-viewer watch-sync integrations once when the Plex surface is mounted.
// Leaving them unconstructed while Plex is disabled keeps the state journal
// gate closed and avoids idle metadata workers on Plex-free servers.
func (a *App) ensurePlexAccountServices() error {
	if a == nil || a.db == nil || a.cfg == nil {
		return nil
	}
	a.plexMetaMu.Lock()
	if a.plexGUID != nil {
		a.plexMetaMu.Unlock()
		return nil
	}
	clientID := func() string {
		cfg := a.cfg.Read()
		return plexguid.ClientIdentifier(cfg.Plex.ClientIdentifier, cfg.Server.MachineIdentifier)
	}
	plexToken := func() string {
		cfg := a.cfg.Read()
		if !cfg.Plex.Claimed || (cfg.Frontends != nil && !cfg.Frontends["plex"].Enabled) {
			return ""
		}
		return cfg.Plex.Token
	}
	guidService := plexguid.New(a.db, plexguid.Options{
		HTTP:             a.opts.plextvHTTP,
		Token:            plexToken,
		ClientIdentifier: clientID,
		Background:       true,
		Namesakes: func(ctx context.Context, k core.RatingKey) (bool, error) {
			if a.catalog == nil {
				return false, nil
			}
			return a.catalog.Namesakes(ctx, k)
		},
	})
	metadataService := plexmeta.New(a.db, plexmeta.Options{
		HTTP:             a.opts.plextvHTTP,
		Token:            plexToken,
		ClientIdentifier: clientID,
		GUID: func(ctx context.Context, k core.RatingKey) string {
			if guidService == nil {
				return ""
			}
			return guidService.Cached(ctx, k)
		},
		Enabled: func() bool {
			cfg := a.cfg.Read()
			return cfg.Plex.Claimed && cfg.Plex.UsePlexMetadata && (cfg.Frontends == nil || cfg.Frontends["plex"].Enabled)
		},
		Background: true,
	})
	a.plexGUID = guidService
	a.plexMeta = metadataService
	if err := a.buildPlexWatch(); err != nil {
		a.plexGUID = nil
		a.plexMeta = nil
		a.plexMetaMu.Unlock()
		guidService.Close()
		metadataService.Close()
		return fmt.Errorf("plex watch state: %w", err)
	}
	a.plexMetaClose = func() {
		guidService.Close()
		metadataService.Close()
	}
	a.plexMetaMu.Unlock()
	return nil
}

func (a *App) closePlexAccountServices() {
	if a == nil {
		return
	}
	a.plexMetaMu.Lock()
	fn := a.plexMetaClose
	a.plexMetaClose = nil
	a.plexMetaMu.Unlock()
	if fn != nil {
		fn()
	}
}

// publishLibrarySections pushes the live Movies / TV Shows identities
// (including configured titles) to plex.tv account registration.
func (a *App) publishLibrarySections() {
	if a == nil || a.frontends == nil {
		return
	}
	a.frontends.publishLibrarySections()
}

func plexPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOSX"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

// publicPlexOrigin is the origin of the first published public connection
// (secure first): a public IP or a plex.direct name for one. Names that would
// need a DNS lookup are skipped; this runs on request paths.
func publicPlexOrigin(connections []remote.Connection) string {
	for _, secure := range []bool{true, false} {
		for _, c := range connections {
			if c.Local || c.Relay {
				continue
			}
			u, err := url.Parse(c.URI)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme == "https") != secure || (u.Scheme != "https" && u.Scheme != "http") {
				continue
			}
			host := u.Hostname()
			ip := net.ParseIP(host)
			if ip == nil {
				ip, _, _ = remote.ParsePlexDirectHost(host)
			}
			if remote.IsPublicIP(ip) {
				return u.Scheme + "://" + u.Host
			}
		}
	}
	return ""
}

// repoFileURL serves files committed to Ether's public repository through
// jsDelivr, pinned to ref so a published URL never changes under a release.
func repoFileURL(ref string) func(string) string {
	return func(path string) string {
		if ref == "" {
			return ""
		}
		return "https://cdn.jsdelivr.net/gh/braedonsaunders/ether@" + ref + "/" + strings.TrimPrefix(path, "/")
	}
}

var commitRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// sourceRef names the source this build came from: a release's tag
// ("v0.3.6"), else the commit a release candidate, development, PR or local
// build was made from. "" when neither is known.
func sourceRef(version, commit, revision string) string {
	// Release candidates (0.3.7-rc.3) are not tagged on GitHub: only a
	// release version names a tag; anything else is pinned to its commit.
	if v := strings.TrimSpace(version); v != "dev" && !strings.ContainsAny(v, "+/ -") {
		if _, err := updater.ParseVersion(v); err == nil {
			return "v" + strings.TrimPrefix(v, "v")
		}
	}
	for _, c := range []string{commit, revision} {
		if c = strings.ToLower(strings.TrimSpace(c)); commitRef.MatchString(c) {
			return c
		}
	}
	return ""
}

// buildRevision is the VCS commit Go stamped into this binary, if any.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
