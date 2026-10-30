package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// DetailRelated owns source authorization and title selection for detail
// shelves. Parent and availability checks during rendering retain their
// established dialect semantics; postplay uses its explicit selection policy.
type DetailRelated struct {
	Source     core.Recommender
	Visibility Visibility
	Get        func(context.Context, core.RatingKey) (*core.Title, error)
}

func (s DetailRelated) SourceVisible(ctx context.Context, viewer *core.User, key core.RatingKey) bool {
	switch key.Kind() {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode:
	default:
		return true
	}
	title, err := s.Get(ctx, key)
	return err == nil && s.Visibility.Visible(ctx, viewer, title)
}
func (s DetailRelated) Rows(ctx context.Context, viewer *core.User, key core.RatingKey, count int) ([]core.Hub, error) {
	if s.Source == nil {
		return nil, nil
	}
	hubs, err := s.Source.Related(ctx, viewer, key, count)
	out := make([]core.Hub, len(hubs))
	copy(out, hubs)
	for i := range out {
		items := make([]core.Title, 0, len(out[i].Items))
		for _, item := range out[i].Items {
			if s.Visibility.TitleAllowed(viewer, &item) {
				items = append(items, item)
			}
		}
		out[i].Items = items
	}
	return out, err
}
