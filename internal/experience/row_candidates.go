package experience

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RankedCandidate carries protocol-neutral selection and presentation data.
type RankedCandidate struct {
	ID        string
	Purpose   core.RowPurpose
	Title     string
	Type      core.MediaType
	Style     string
	Promoted  bool
	Items     []core.Title
	More      bool
	Key       core.RatingKey
	ItemCount int
}

// CandidateRanker assembles one ranked pool for a visit. A non-nil predicate
// must be applied to the generated pools before ranking and limiting.
type CandidateRanker interface {
	Home(context.Context, *core.User, core.HomeSituation, int, func(context.Context, *core.User, *core.Title) bool) ([]RankedCandidate, error)
	Library(context.Context, *core.User, core.MediaType, core.HomeSituation, int, func(context.Context, *core.User, *core.Title) bool) ([]RankedCandidate, error)
}

// ResumeCandidates supplies viewer-scoped continuation titles.
type ResumeCandidates interface {
	Continue(context.Context, *core.User, int) ([]core.Title, error)
}

// CandidateComposer selects one placement and incorporates continuation before
// the row service authorizes entries and issues interaction receipts.
type CandidateComposer struct {
	Ranker     CandidateRanker
	Resume     ResumeCandidates
	Libraries  []Library
	Visibility Visibility
	Restricted func(*core.User) bool
}

func (c CandidateComposer) Rows(ctx context.Context, req frontend.RowRequest, sit core.HomeSituation) ([]RankedCandidate, error) {
	if req.Viewer == nil || c.Ranker == nil {
		return nil, core.ErrNotFound
	}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || c.Restricted == nil || c.Restricted(req.Viewer))
	var allow func(context.Context, *core.User, *core.Title) bool
	if restricted {
		allow = c.Visibility.Visible
	}
	switch req.Placement {
	case frontend.PlacementHome:
		hubs, err := c.Ranker.Home(ctx, req.Viewer, sit, req.Limit, allow)
		if err != nil {
			return nil, err
		}
		if c.Resume == nil {
			return nil, core.ErrNotFound
		}
		resume, err := c.Resume.Continue(ctx, req.Viewer, req.Limit)
		if err != nil {
			return nil, err
		}
		inserted := false
		for i := range hubs {
			if hubs[i].ID == "home.continue" {
				hubs[i].Items = resume
				inserted = true
			}
		}
		if !inserted {
			hubs = append([]RankedCandidate{{ID: "home.continue", Purpose: core.RowPurpose("continue"), Title: "Continue Watching", Style: "hero", Promoted: true, Items: resume}}, hubs...)
		}
		return hubs, nil
	case frontend.PlacementLibrary:
		id, err := strconv.Atoi(req.ViewID)
		if err != nil {
			return nil, core.ErrNotFound
		}
		var kind core.MediaType
		for _, library := range c.Libraries {
			if library.ID == id {
				kind = library.Type
				break
			}
		}
		if kind == "" || !SectionAllowed(req.Viewer, id) {
			return nil, core.ErrNotFound
		}
		sit.LibraryID = id
		return c.Ranker.Library(ctx, req.Viewer, kind, sit, req.Limit, allow)
	default:
		return nil, core.ErrNotFound
	}
}
