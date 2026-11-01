package silo

import (
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Browse lists authorized version selectors only. Opening a leaf source
// belongs to playback. A catalog item never plans or probes; watch and the
// versions list do when the catalogue has no probed copy.
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
	// A detail page does not open a resolver. Probed copies already on disk
	// are a catalogue read, and they are the only file a cold title has.
	if len(versions) == 0 && i.host.Editions != nil {
		listed, err := i.host.Editions.Editions(r.Context(), scope.Viewer, scope.Device, detail.Title.Key)
		if err != nil {
			return nil, nil, err
		}
		for _, version := range listed {
			if version.ID != "" {
				versions = append(versions, version)
				releases = append(releases, version.ID)
			}
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
		return i.probedVersion(r, scope, detail)
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

// detailOpensSource is an explicit file request. Catalog item browse stays on
// catalogue editions. Watch and the versions list each open one leaf, because
// playback needs that file id before the client can start.
func detailOpensSource(path string) bool {
	return strings.HasSuffix(path, "/versions") || strings.HasPrefix(path, "/api/v2/watch/")
}

// probedVersion opens one source for watch and the versions list. Item browse
// stays on catalogue data; a leaf open belongs to an explicit file request.
func (i *instance) probedVersion(r *http.Request, scope frontend.VersionScope, detail frontend.ItemDetail) ([]frontend.Version, []frontend.SourceOffer, error) {
	planner, ok := i.host.Sessions.(frontend.DetailPlanner)
	if !ok || i.host.SourceOffers == nil || !detailOpensSource(r.URL.Path) {
		return nil, nil, nil
	}
	plan, err := planner.DetailPlan(r.Context(), frontend.PlanRequest{
		Viewer:   scope.Viewer,
		Device:   scope.Device,
		Protocol: scope.Protocol,
		Item:     detail.Title.Key,
		Caps:     scope.Caps,
		Intent:   frontend.PlayIntent{Options: frontend.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1}},
	})
	if err != nil || plan.DetailSource == "" || plan.ETag == "" {
		return nil, nil, nil
	}
	offers, err := i.host.SourceOffers.IssueBatch(r.Context(), scope, []frontend.SourceOfferTarget{{
		Item: detail.Title.Key, DetailSource: plan.DetailSource, ETag: plan.ETag,
	}})
	if err != nil {
		return nil, nil, err
	}
	if len(offers) != 1 {
		return nil, nil, frontend.ErrSourceGone
	}
	return []frontend.Version{{ID: plan.DetailSource, Media: plan.Media}}, offers, nil
}
