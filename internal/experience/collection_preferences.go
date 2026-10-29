package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
)

func sliceBounds(total, offset, limit int) (int, int) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	start := min(max(offset, 0), total)
	end := total
	if limit > 0 && limit < total-start {
		end = start + limit
	}
	return start, end
}

// Collections filters member visibility before calculating totals and paging.
func (s *Service) Collections(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.CollectionPage{}, err
	}
	if s.d.Collections == nil {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	requested := map[core.MediaType]bool{}
	for _, typ := range q.Types {
		if typ != core.TypeMovie && typ != core.TypeShow {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		requested[typ] = true
	}
	complete, ok := s.d.Collections.(interface {
		ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
	})
	if !ok {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	seenTypes := map[core.MediaType]bool{}
	seenKeys := map[core.RatingKey]bool{}
	var visible []core.Collection
	for _, lib := range s.d.Libraries {
		typ := lib.Type
		if (typ != core.TypeMovie && typ != core.TypeShow) || !SectionAllowed(v, lib.ID) || (len(requested) > 0 && !requested[typ]) || seenTypes[typ] {
			continue
		}
		seenTypes[typ] = true
		collections, e := complete.ListComplete(ctx, v, typ)
		if e != nil {
			return frontend.CollectionPage{}, e
		}
		for _, c := range collections {
			if c.Type != typ || seenKeys[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
				continue
			}
			members, e := s.collectionItems(ctx, v, c.Key)
			if e != nil {
				return frontend.CollectionPage{}, e
			}
			if len(members) == 0 {
				continue
			}
			c.ChildCount = len(members)
			seenKeys[c.Key] = true
			visible = append(visible, c)
		}
	}
	start, end := sliceBounds(len(visible), q.Offset, q.Limit)
	return frontend.CollectionPage{Items: visible[start:end], Total: len(visible)}, nil
}

func (s *Service) notInterested(ctx context.Context, v *core.User) ([]frontend.NotInterestedEntry, error) {
	if s.d.NotInterestedSource == nil {
		return nil, core.ErrNotAvailable
	}
	entries, err := s.d.NotInterestedSource(ctx, v)
	if err != nil {
		return nil, err
	}
	result := make([]frontend.NotInterestedEntry, 0, len(entries))
	for _, entry := range entries {
		if _, err := s.item(ctx, v, entry.Key); err == nil {
			result = append(result, entry)
		}
	}
	return result, nil
}
func (s *Service) NotInterested(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.PageQuery) (frontend.NotInterestedPage, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	start, end := sliceBounds(len(entries), q.Offset, q.Limit)
	return frontend.NotInterestedPage{Items: entries[start:end], Total: len(entries)}, nil
}
func (s *Service) NotInterestedItem(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey) (frontend.NotInterestedEntry, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.NotInterestedEntry{}, core.ErrNotFound
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	for _, entry := range entries {
		if entry.Key == key {
			return entry, nil
		}
	}
	return frontend.NotInterestedEntry{}, core.ErrNotFound
}
