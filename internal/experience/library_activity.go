package experience

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const activityScanLimit = 10000

func (s *Service) activityLibrary(v *core.User, viewID string) (*Library, error) {
	if viewID == "" {
		return nil, nil
	}
	id, err := strconv.Atoi(viewID)
	if err != nil || !SectionAllowed(v, id) {
		return nil, core.ErrNotFound
	}
	for i := range s.d.Libraries {
		if s.d.Libraries[i].ID == id {
			return &s.d.Libraries[i], nil
		}
	}
	return nil, core.ErrNotFound
}

func activityLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 500)
}

// LatestAdded returns the real added-time order for one library's roots.
// It deliberately does not pretend to supply grouped recent episodes.
func (s *Service) LatestAdded(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID string, limit int) ([]frontend.LatestItem, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	lib, err := s.activityLibrary(v, viewID)
	if err != nil {
		return nil, err
	}
	if s.d.Browse == nil || s.d.AddedTimes == nil {
		return nil, core.ErrNotAvailable
	}
	limit = activityLimit(limit)
	var items []core.Title
	libraries := s.d.Libraries
	if lib != nil {
		libraries = []Library{*lib}
	}
	for _, library := range libraries {
		if !SectionAllowed(v, library.ID) {
			continue
		}
		found := 0
		for offset := 0; offset < activityScanLimit; {
			page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: library.Type}, Sort: core.SortAdded, Desc: true, Offset: offset, Limit: 500})
			if err != nil {
				return nil, err
			}
			for i := range page.Items {
				if s.visible(ctx, v, &page.Items[i]) {
					items = append(items, page.Items[i])
					found++
					if found == limit {
						break
					}
				}
			}
			offset += len(page.Items)
			if found == limit || offset >= page.Total {
				break
			}
			if len(page.Items) == 0 {
				return nil, errors.New("experience: incomplete latest catalogue page")
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if offset >= activityScanLimit {
				return nil, errors.New("experience: latest catalogue scan limit reached")
			}
		}
	}
	keys := make([]core.RatingKey, len(items))
	for i := range items {
		keys[i] = items[i].Key
	}
	times, err := s.d.AddedTimes(ctx, keys)
	if err != nil {
		return nil, err
	}
	result := make([]frontend.LatestItem, 0, len(items))
	for _, title := range items {
		item := frontend.LatestItem{Title: title}
		if at, ok := times[title.Key]; ok && !at.IsZero() {
			value := at
			item.AddedAt = &value
		}
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].AddedAt == nil {
			return false
		}
		if result[j].AddedAt == nil {
			return true
		}
		return result[i].AddedAt.After(*result[j].AddedAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// Resume reads only in-progress state, without next-up or Home Designer data.
func (s *Service) Resume(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID string, types []core.MediaType, offset, limit int) (frontend.ActivityPage, error) {
	var page frontend.ActivityPage
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return page, err
	}
	lib, err := s.activityLibrary(v, viewID)
	if err != nil {
		return page, err
	}
	if s.d.State == nil || s.d.CachedGet == nil {
		return page, core.ErrNotAvailable
	}
	if offset < 0 || offset > activityScanLimit {
		return page, errors.New("experience: invalid resume offset")
	}
	limit = activityLimit(limit)
	visibility := s.d.Visibility
	visibility.GetTitle = s.d.CachedGet
	target := offset + limit + 1
	fetch := min(max(target*2, 50), activityScanLimit+1)
	processed := 0
	var items []core.Title
	for {
		states, err := s.d.State.InProgress(ctx, v.ID, fetch)
		if err != nil {
			return page, err
		}
		if len(states) < processed {
			return page, errors.New("experience: resume state changed during scan")
		}
		for _, status := range states[processed:] {
			if err = ctx.Err(); err != nil {
				return page, err
			}
			title, err := s.d.CachedGet(ctx, status.Key)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return page, err
			}
			if title == nil || title.Key != status.Key || visibility.SectionFor == nil {
				continue
			}
			section, ok := visibility.SectionFor(title.Type)
			if !ok || !SectionAllowed(v, section) || (lib != nil && section != lib.ID) || !visibility.Visible(ctx, v, title) {
				continue
			}
			if len(types) > 0 {
				matches := false
				for _, typ := range types {
					if typ == title.Type {
						matches = true
						break
					}
				}
				if !matches {
					continue
				}
			}
			items = append(items, *title)
			if len(items) == target {
				page.Items, page.Total, page.More = items[offset:offset+limit], len(items), true
				return page, nil
			}
		}
		processed = len(states)
		if len(states) < fetch {
			page.Total, page.TotalExact = len(items), true
			start := min(offset, len(items))
			page.Items = items[start:min(start+limit, len(items))]
			return page, nil
		}
		if fetch > activityScanLimit {
			return page, errors.New("experience: resume scan limit reached")
		}
		fetch = min(fetch*2, activityScanLimit+1)
	}
}

var _ frontend.LibraryActivity = (*Service)(nil)
