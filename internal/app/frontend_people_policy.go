package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// validPeopleReadTimes bounds elapsed monotonic time when available and
// rejects an observed wall-clock reversal. Two samples cannot rule out an
// unobserved forward-and-back jump; no uninterrupted wall-clock truth is claimed.
func validPeopleReadTimes(started, checked time.Time) bool {
	elapsed := checked.Sub(started)
	return !checked.UTC().Before(started.UTC()) && elapsed >= 0 && elapsed <= 4*time.Second
}

type peoplePolicy struct {
	viewerID int64
	policy   safety.Policy
}

func (p peoplePolicy) Allowed(viewer *core.User, title *core.Title) bool {
	return viewer != nil && viewer.ID == p.viewerID && safety.AllowedWithPolicy(viewer, title, p.policy)
}

// beginPeopleRead takes no joint locks while reading metadata. The final epoch
// fence validates all durable facts read since the initial epoch, and checks the
// exact shared config version while the local config lease is still held.
// Playback hours do not restrict metadata navigation: this uses Allowed's rule
// body, not CanPlay. Release/home-release gates use one captured time and only
// become more permissive as time advances; a backwards clock fails closed.
func (a *App) beginPeopleRead(ctx context.Context, viewer *core.User) (experience.PeopleRead, error) {
	if viewer == nil || viewer.ID <= 0 || a.cfg == nil || a.catalog == nil || a.db == nil || a.safety == nil || a.users == nil {
		return experience.PeopleRead{}, core.ErrNotAvailable
	}
	lease, err := a.cfg.AcquirePolicySnapshot(ctx)
	if err != nil {
		return experience.PeopleRead{}, err
	}
	failed := true
	defer func() {
		if failed {
			lease.Close()
		}
	}()
	cfg := lease.Config()
	evidence, matches := a.catalog.CaptureConfigEvidence(cfg.Catalog)
	if !matches {
		return experience.PeopleRead{}, viewerpolicy.ErrChanged
	}
	epoch, err := viewerpolicy.Read(ctx, a.db)
	if err != nil {
		return experience.PeopleRead{}, err
	}
	owned, err := a.users.PolicyUser(ctx, viewer.ID)
	if err != nil {
		return experience.PeopleRead{}, err
	}
	if owned == nil || owned.ID != viewer.ID {
		return experience.PeopleRead{}, core.ErrUnauthorized
	}
	policy, err := a.safety.FreshPolicy(ctx, owned.ID)
	if err != nil {
		return experience.PeopleRead{}, err
	}
	started := time.Now()
	now := started.UTC()
	version, clustered := lease.SharedVersion()
	access := experience.PeopleRead{
		Viewer:     owned,
		SectionFor: experienceVisibility(nil, nil, nil, nil, serverLibraries(cfg)).SectionFor,
		Policy:     peoplePolicy{viewerID: owned.ID, policy: policy},
		Titles: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			return a.catalog.CachedCreditTitles(ctx, keys, cfg.Catalog, now)
		},
		Close: lease.Close,
		Finish: func(ctx context.Context) error {
			if !validPeopleReadTimes(started, time.Now()) || !evidence.Current() {
				return viewerpolicy.ErrChanged
			}
			tx, err := a.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := viewerpolicy.Fence(ctx, a.db, tx, epoch); err != nil {
				return err
			}
			if clustered {
				if err := viewerpolicy.CheckSharedConfigVersion(ctx, tx, version); err != nil {
					return err
				}
			}
			if !evidence.Current() {
				return viewerpolicy.ErrChanged
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			if !validPeopleReadTimes(started, time.Now()) || !evidence.Current() {
				return viewerpolicy.ErrChanged
			}
			return nil
		},
	}
	if a.availability != nil {
		access.FilterAvailable = a.availability.FilterDiscoveryCurrent
	}
	failed = false
	return access, nil
}
