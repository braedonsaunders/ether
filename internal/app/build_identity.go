package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/remote"
	"github.com/braedonsaunders/ether/internal/safety"
)

func (a *App) buildIdentity(ctx context.Context, cfg config.Config) error {
	log := a.log
	dataDir := a.opts.DataDir
	var err error
	// plex.tv, users, certificates, LAN discovery.
	if a.plextv, err = plextv.New(plextv.Options{BaseURL: parityPlexTVBaseURL(), HTTPClient: a.opts.plextvHTTP, Identity: plextv.ServerIdentity(cfg, ""), VersionSource: func() string { return plextv.PMSVersion(a.cfg.Get()) }}); err != nil {
		return fmt.Errorf("plex.tv client: %w", err)
	}
	if a.users, err = auth.New(context.Background(), a.db, auth.Options{
		Now:       parityClock(),
		Validator: auth.PlexTVValidator{Client: a.plextv, MachineID: cfg.Server.MachineIdentifier},
		Logger:    log.With("component", "auth"),
	}); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if cfg.Plex.Token != "" {
		if err := a.users.BootstrapOwner(context.Background(), cfg.Plex); err != nil {
			log.Warn("owner bootstrap", "err", err)
		}
	}
	if a.safety != nil {
		a.safety.SetPlexShare(func(u *core.User) safety.PlexShare {
			if a.users == nil || u == nil {
				return safety.PlexShare{}
			}
			d, ok := a.users.PlexDefaults(u.ID)
			if !ok {
				return safety.PlexShare{}
			}
			return safety.PlexShare{FilterMovies: d.FilterMovies, FilterTelevision: d.FilterTelevision, AllowSync: d.AllowSync}
		})
		a.safety.SetFriendsDownloads(func() bool {
			if a.cfg == nil {
				return false
			}
			return a.cfg.Get().Downloads.AllowFriends
		})
	}
	if a.certs, err = plextv.NewCertManager(plextv.CertManagerOptions{
		Dir:            filepath.Join(dataDir, "certs"),
		Client:         a.plextv,
		CustomCertFile: cfg.Plex.CertFile,
		CustomKeyFile:  cfg.Plex.KeyFile,
		Shared:         a.sharedCerts(),
		Logger:         log.With("component", "certs"),
	}); err != nil {
		return fmt.Errorf("certificates: %w", err)
	}
	ropts := plextv.RegistrarOptions{
		Config: a.cfg,
		Client: a.plextv,
		Users:  a.users,
		Certs:  a.certs,
		Logger: log.With("component", "registrar"),
	}
	if a.clustered() {
		ropts.PeerURIs = a.peers.get // publish the union of every live node
		ropts.NodeMapping = true     // every node maps its own port (runRegistrarAsLeader)
	}
	if a.registrar, err = plextv.NewRegistrar(ropts); err != nil {
		return fmt.Errorf("registrar: %w", err)
	}
	a.users.SetUnknownTokenRefresher(a.registrar.SyncFriendsOnUnknownToken)
	if cfg.Plex.Claimed && cfg.Plex.Token != "" && (cfg.Frontends == nil || cfg.Frontends["plex"].Enabled) {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := a.registrar.RefreshOwner(rctx); err != nil {
			log.Warn("plex.tv owner refresh", "err", err)
		}
		cancel()
	}
	a.gdm = a.newGDMResponder()

	return nil
}

func (a *App) newGDMResponder() *remote.GDMResponder {
	return remote.NewGDMResponder(remote.GDMOptions{
		Info: func() remote.GDMInfo {
			c := a.cfg.Get()
			return remote.GDMInfo{Name: c.Server.FriendlyName, MachineIdentifier: c.Server.MachineIdentifier, Port: c.Server.Port, Version: plextv.PMSVersion(c), UpdatedAt: time.Now()}
		},
		Logger: a.log.With("component", "gdm"),
	})
}
