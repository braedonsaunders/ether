package silo

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Browse lists authorized version selectors only. Opening a leaf source
// belongs to playback, so a detail page never plans or probes.
func (i *instance) sourceVersions(r *http.Request, scope frontend.VersionScope, detail frontend.ItemDetail) ([]frontend.Version, []frontend.SourceOffer, error) {
	if i.host.SourceOffers == nil {
		return nil, nil, frontend.ErrSourceGone
	}
	versions := make([]frontend.Version, 0, len(detail.Versions))
	releases := make([]string, 0, len(detail.Versions))
	for _, version := range detail.Versions {
		if version.ID != "" {
			versions = append(versions, version)
			releases = append(releases, version.ID)
		}
	}
	if len(versions) > 128 {
		return nil, nil, frontend.ErrSourceGone
	}
	targets := make([]frontend.SourceOfferTarget, 0, len(versions))
	if len(versions) > 0 {
		batch, ok := i.host.VersionHandles.(frontend.VersionHandleBatch)
		if !ok {
			return nil, nil, frontend.ErrSourceGone
		}
		handles, err := batch.IssueBatch(r.Context(), scope, detail.Title.Key, releases)
		if err != nil {
			return nil, nil, err
		}
		for _, version := range versions {
			if handles[version.ID] == "" {
				return nil, nil, frontend.ErrSourceGone
			}
			targets = append(targets, frontend.SourceOfferTarget{Item: detail.Title.Key, VersionHandle: handles[version.ID]})
		}
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}
	offers, err := i.host.SourceOffers.IssueBatch(r.Context(), scope, targets)
	if err != nil {
		return nil, nil, err
	}
	if len(offers) != len(versions) {
		return nil, nil, frontend.ErrSourceGone
	}
	return versions, offers, nil
}
