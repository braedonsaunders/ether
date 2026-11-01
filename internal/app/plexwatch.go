package app

import (
	"context"
	"database/sql"
	"errors"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/plexwatch"
)

// buildPlexWatch attaches the optional per-viewer account sync. GUID lookup
// still uses the server metadata token inside plexguid. The watch client
// receives only the viewer's own sealed PIN token.
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
		ClientID: a.cfg.Get().Plex.ClientIdentifier,
		Logger:   a.log.With("component", "plexwatch"),
	})
	if err != nil {
		return err
	}
	a.plexWatch = svc
	a.closers = append(a.closers, func() { _ = svc.Close() })
	a.state.SetJournalGate(func(ctx context.Context, tx *sql.Tx, userID int64) (int64, bool, error) {
		var enabled, gen int64
		err := tx.QueryRowContext(ctx, `SELECT a.enabled, f.generation
			FROM plexwatch_account a JOIN plexwatch_fence f ON f.user_id=a.user_id
			WHERE a.user_id=? AND a.revoked=0 AND a.token<>'' AND a.token_generation=f.generation`, userID).Scan(&enabled, &gen)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return gen, enabled != 0, nil
	})
	return nil
}

// plexWatchAdmin adapts the service to the admin interface and maps
// identity errors onto admin sentinels.
type plexWatchAdmin struct{ svc *plexwatch.Service }

func (p plexWatchAdmin) status(st plexwatch.Status) admin.PlexWatchStatus {
	return admin.PlexWatchStatus{
		Connected: st.Connected, PlexID: st.PlexID, Username: st.Username,
		Enabled: st.Enabled, Eligibility: st.Eligibility, Revoked: st.Revoked,
		Pending: st.Pending, Held: st.Held, HeldLegacy: st.HeldLegacy,
		Dead: st.Dead, InFlight: st.InFlight, Skipped: st.Skipped, Restricted: st.Restricted,
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
	case errors.Is(err, plexwatch.ErrNotEligible):
		return admin.ErrPlexNotEligible
	case errors.Is(err, plexwatch.ErrRevoked):
		return admin.ErrPlexWatchRevoked
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
func (p plexWatchAdmin) SetEnabled(ctx context.Context, userID, plexID int64, on bool) (admin.PlexWatchStatus, error) {
	st, err := p.svc.SetEnabled(ctx, userID, plexID, on)
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
func (p plexWatchAdmin) Resolve(ctx context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	st, err := p.svc.Resolve(ctx, userID, plexID)
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
