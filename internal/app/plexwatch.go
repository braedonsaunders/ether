package app

import (
	"context"
	"database/sql"
	"errors"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexwatch"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

// buildPlexWatch attaches the optional per-viewer account sync. Each viewer
// syncs with their own Plex account token; titles are identified by the
// verified plex:// GUIDs of the Plex front end's resolver.
func (a *App) buildPlexWatch() error {
	if a.plexWatch != nil || a.db == nil || a.state == nil || a.plexGUID == nil || a.cfg == nil {
		return nil
	}
	svc, err := plexwatch.New(plexwatch.Options{
		DB:       a.db,
		State:    a.state,
		Sealer:   a.cfg,
		GUID:     a.plexGUID,
		NodeID:   a.clusterNodeID(),
		ClientID: plexguid.ClientIdentifier(a.cfg.Get().Plex.ClientIdentifier, a.cfg.Get().Server.MachineIdentifier),
		Logger:   a.log.With("component", "plexwatch"),
		Watchlist: func(ctx context.Context, userID int64) ([]core.RatingKey, error) {
			return watchlist.ExportKeys(ctx, a.db, userID)
		},
		// Continue Watching removals read from a viewer's Plex account also
		// apply to the Discover rows' own removal list, as the Plex front
		// end's removeFromContinueWatching does.
		Continue: func(ctx context.Context, userID int64, key core.RatingKey, removed bool) error {
			personal := a.discoveryPersonal
			if personal == nil {
				return nil
			}
			var err error
			if removed {
				err = personal.RemoveContinue(ctx, userID, key)
			} else {
				err = personal.RestoreContinue(ctx, userID, key)
			}
			if errors.Is(err, core.ErrNotFound) {
				return nil
			}
			return err
		},
	})
	if err != nil {
		return err
	}
	a.plexWatch = svc
	a.closers = append(a.closers, func() { _ = svc.Close() })
	a.state.SetJournalGate(func(ctx context.Context, tx *sql.Tx, userID int64) (int64, bool, error) {
		var enabled, gen int64
		err := tx.QueryRowContext(ctx, `SELECT a.direction, f.generation
			FROM plexwatch_account a JOIN plexwatch_fence f ON f.user_id=a.user_id
			WHERE a.user_id=? AND a.revoked=0 AND a.token<>'' AND a.token_generation=f.generation`, userID).Scan(&enabled, &gen)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return gen, enabled&1 != 0, nil
	})
	return nil
}

// plexWatchAdmin adapts the service to the admin interface and maps
// identity errors onto admin sentinels.
type plexWatchAdmin struct{ svc *plexwatch.Service }

func (p plexWatchAdmin) status(st plexwatch.Status) admin.PlexWatchStatus {
	return admin.PlexWatchStatus{
		Connected: st.Connected, PlexID: st.PlexID, Username: st.Username,
		Direction: st.Direction, ImportInterval: st.ImportInterval,
		LastImport: st.LastImport, LastFullImport: st.LastFullImport, LastSend: st.LastSend, NextImport: st.NextImport,
		Problem: st.Problem, NeedsReconnect: st.NeedsReconnect,
		Pending: st.Pending, Failed: st.Failed, Restricted: st.Restricted,
		PINCode: st.PINCode, PINURL: st.PINURL, PINExpires: st.PINExpires,
		AccountMatch: st.AccountMatch,
	}
}

func mapPlexWatchErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, plexwatch.ErrAccountMismatch):
		return admin.ErrPlexAccountMismatch
	case errors.Is(err, plexwatch.ErrRevoked):
		return admin.ErrPlexWatchRevoked
	case errors.Is(err, plexwatch.ErrNotConnected):
		return admin.ErrPlexWatchNotConnected
	case errors.Is(err, plexwatch.ErrSendingOff), errors.Is(err, plexwatch.ErrReceivingOff):
		return admin.ErrPlexWatchDirectionOff
	default:
		return err
	}
}

func (p plexWatchAdmin) Status(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.Status(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) BeginPIN(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.BeginPIN(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) FinishPIN(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.FinishPIN(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) SetDirection(ctx context.Context, userID, plexID int64, direction, interval int) (admin.PlexWatchStatus, error) {
	st, err := p.svc.SetDirection(ctx, userID, plexID, direction, interval)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) Disconnect(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.Disconnect(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) Retry(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.Retry(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) ExportCurrent(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.ExportCurrent(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}
func (p plexWatchAdmin) ImportCurrent(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.ImportCurrent(ctx, userID, plexID)
	return p.status(st), mapPlexWatchErr(err)
}

// buildPlexServers attaches watch sync with owner-run Plex Media Servers.
// It does not depend on the Plex front end: plex:// GUIDs, used only to
// find a server's copy of an Ether title, are read once the front end has
// built its resolver.
func (a *App) buildPlexServers() error {
	if a.plexServers != nil || a.db == nil || a.state == nil || a.cfg == nil {
		return nil
	}
	opts := plexwatch.ServersOptions{
		Enabled:  func() bool { return a.cfg.Get().PlexServers.Enabled },
		DB:       a.db,
		State:    a.state,
		Sealer:   a.cfg,
		GUID:     lazyPlexGUID{a},
		ClientID: plexguid.ClientIdentifier(a.cfg.Get().Plex.ClientIdentifier, a.cfg.Get().Server.MachineIdentifier),
		Logger:   a.log.With("component", "plexservers"),
		ViewerExists: func(ctx context.Context, id int64) (bool, error) {
			_, err := auth.StoredIdentity(ctx, a.db, id)
			if errors.Is(err, core.ErrNotFound) {
				return false, nil
			}
			return err == nil, err
		},
	}
	if a.catalog != nil {
		opts.Catalog = a.catalog
	}
	svc, err := plexwatch.NewServers(opts)
	if err != nil {
		return err
	}
	a.plexServers = svc
	a.closers = append(a.closers, func() { _ = svc.Close() })
	return nil
}

// lazyPlexGUID reads the Plex front end's GUID resolver when it exists.
type lazyPlexGUID struct{ a *App }

func (g lazyPlexGUID) service() *plexguid.Service {
	g.a.plexBuildMu.Lock()
	defer g.a.plexBuildMu.Unlock()
	return g.a.plexGUID
}

func (g lazyPlexGUID) Resolve(ctx context.Context, key core.RatingKey) (string, error) {
	if s := g.service(); s != nil {
		return s.Resolve(ctx, key)
	}
	return "", core.ErrNotFound
}

func (g lazyPlexGUID) ExactKey(ctx context.Context, guid string) (core.RatingKey, error) {
	if s := g.service(); s != nil {
		return s.ExactKey(ctx, guid)
	}
	return 0, core.ErrNotFound
}

func (a *App) plexServersAPI() admin.PlexServers {
	if a.plexServers == nil {
		return nil
	}
	return a.plexServers
}
