package silo

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// A detail request may resolve one leaf source. Alternatives retain their
// authorized exact version selectors; listing estimates never become probes.
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
	if planner, ok := i.host.Sessions.(frontend.DetailPlanner); ok {
		req := frontend.PlanRequest{Viewer: scope.Viewer, Device: scope.Device, Protocol: "silo", Item: detail.Title.Key, Caps: scope.Caps, Intent: frontend.PlayIntent{Options: frontend.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
		if len(targets) > 0 {
			req.Version, req.Intent.ExactVersion = targets[0].VersionHandle, true
		}
		plan, err := planner.DetailPlan(r.Context(), req)
		if err == nil && !plan.Estimated && !plan.Media.Estimated && plan.DetailSource != "" && plan.ETag != "" {
			target := frontend.SourceOfferTarget{Item: detail.Title.Key, DetailSource: plan.DetailSource, ETag: plan.ETag}
			if len(versions) == 0 {
				versions = append(versions, frontend.Version{Media: plan.Media})
				targets = append(targets, target)
			} else {
				versions[0].Media, targets[0] = plan.Media, target
			}
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
