package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// HomeSelection obtains ranked shelves from the configured source. Policy-aware
// sources filter before ranking; finite custom sources are checked afterwards.
type HomeSelection struct {
	Source     core.Recommender
	Visibility Visibility
	Restricted bool
}

func (s HomeSelection) Rows(ctx context.Context, viewer *core.User, hc core.HubContext, typ core.MediaType, count int) ([]core.Hub, error) {
	if s.Source == nil {
		return nil, nil
	}
	var hubs []core.Hub
	var err error
	if source, ok := s.Source.(core.PolicyHomeCandidateSource); ok && s.Restricted {
		if typ == "" {
			hubs, err = source.HomeCandidatesWithPolicy(ctx, viewer, hc, count, s.Visibility.Visible)
		} else {
			hubs, err = source.SectionCandidatesWithPolicy(ctx, viewer, typ, hc, count, s.Visibility.Visible)
		}

		if err != nil {
			return nil, err
		}
	} else if typ == "" {
		if source, ok := s.Source.(core.HomeCandidateSource); ok {
			hubs, err = source.HomeCandidatesFor(ctx, viewer, hc, count)
		} else if source, ok := s.Source.(core.ContextualRecommender); ok {
			hubs, err = source.HomeHubsFor(ctx, viewer, hc, count)
		} else {
			hubs, err = s.Source.HomeHubs(ctx, viewer, count)
		}
	} else {
		if source, ok := s.Source.(core.SectionCandidateSource); ok {
			hubs, err = source.SectionCandidatesFor(ctx, viewer, typ, hc, count)
		} else if source, ok := s.Source.(core.ContextualRecommender); ok {
			hubs, err = source.SectionHubsFor(ctx, viewer, typ, hc, count)
		} else {
			hubs, err = s.Source.SectionHubs(ctx, viewer, typ, count)
		}
	}
	if err != nil || !s.Restricted {
		return hubs, err
	}
	return s.Filter(ctx, viewer, hubs), nil
}
func (s HomeSelection) Filter(ctx context.Context, viewer *core.User, hubs []core.Hub) []core.Hub {
	out := make([]core.Hub, len(hubs))
	copy(out, hubs)
	for i := range out {
		items := make([]core.Title, 0, len(out[i].Items))
		for _, item := range out[i].Items {
			if s.Visibility.Visible(ctx, viewer, &item) {
				items = append(items, item)
			}
		}
		out[i].Items = items
	}
	return out
}

// ArrangeHome selects original producer indices. Providers use their declared
// layout contract directly; rendering sidecars never enter a neutral layout.
func ArrangeHome(ctx context.Context, source core.Recommender, viewer *core.User, hc core.HubContext, section string, rows []core.Hub, candidates []core.NeutralHomeRow) ([]int, error) {
	if neutral, ok := source.(core.NeutralHomeLayoutSource); ok {
		situation := core.HomeSituation{Device: hc.Device, Daypart: hc.Daypart, Weekend: hc.Weekend, Now: hc.Now, LibraryID: hc.SectionID, Product: hc.Product, Platform: hc.Platform, SessionID: hc.SessionID, ClientID: hc.ClientID}
		return ArrangeNeutralHome(ctx, neutral, viewer, situation, section, candidates)
	}
	if layout, ok := source.(core.HomeLayoutSource); ok {
		input := make([]LayoutCandidate, len(rows))
		for i, row := range rows {
			input[i] = LayoutCandidate{ID: row.ID}
		}
		return ArrangeLayout(input, func([]LayoutCandidate) ([]string, error) {
			ordered, err := layout.FinalizeHome(ctx, viewer, hc, section, rows)
			if err != nil {
				return nil, err
			}
			ids := make([]string, len(ordered))
			for i, row := range ordered {
				ids[i] = row.ID
			}
			return ids, nil
		})
	}
	out := make([]int, len(rows))
	for i := range rows {
		out[i] = i
	}
	return out, nil
}
