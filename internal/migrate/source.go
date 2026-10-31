package migrate

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// PMS's owner history endpoint includes every account unless explicitly scoped.
// The source's local account ID is distinct from a plex.tv account ID (the PMS
// owner, for example, is account 1). Never infer it from the destination row ID.
func (s *Service) sourceAccount(ctx context.Context, base, token string, v Viewer) (int64, error) {
	accounts, err := s.get(ctx, base, token, "/accounts")
	if err != nil {
		return 0, err
	}
	name := v.Name
	if name == "" {
		if user, err := s.viewer(ctx, v.LocalUserID); err == nil {
			name = user.Username
		}
	}
	for _, account := range accounts.Accounts {
		if account.ID <= 0 {
			continue
		}
		if v.SourceAccountID > 0 {
			if account.ID == v.SourceAccountID {
				return account.ID, nil
			}
		} else if (name != "" && strings.EqualFold(account.Name, name)) || (v.PlexID > 0 && account.ID == v.PlexID) {
			return account.ID, nil
		}
	}
	return 0, errors.New("could not identify the source Plex history account")
}

// One preview can contain thousands of episodes of the same show. Resolve each
// source show once, and reuse the identity for history entries lacking GUIDs.
// The maps are shared by the preview's worker pool.
type sourceMapper struct {
	service             *Service
	base, token         string
	mu                  sync.Mutex
	keys, parents       map[string]core.RatingKey
	showsByTitle        map[string]core.RatingKey
	titleToGrandparents map[string][]string
	libraryByKey        map[string]media
	moviesByTitleYear   map[string][]media
	moviesByTitle       map[string][]media
	missing             map[string]bool
	inflight            map[string]*mapCall
}

type mapCall struct {
	done chan struct{}
	key  core.RatingKey
	err  error
}

func foldTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func titleYearKey(title string, year int) string {
	return foldTitle(title) + "\x00" + strconv.Itoa(year)
}

func (m *sourceMapper) indexLibrary(items []media) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.libraryByKey == nil {
		m.libraryByKey = map[string]media{}
	}
	if m.titleToGrandparents == nil {
		m.titleToGrandparents = map[string][]string{}
	}
	if m.moviesByTitleYear == nil {
		m.moviesByTitleYear = map[string][]media{}
	}
	if m.moviesByTitle == nil {
		m.moviesByTitle = map[string][]media{}
	}
	for _, item := range items {
		if item.Key != "" {
			m.libraryByKey[item.Key] = item
		}
		switch item.Type {
		case "episode":
			m.noteShowParentLocked(item.GrandparentTitle, item.grandparentID())
		case "movie":
			ft := foldTitle(item.Title)
			if ft != "" {
				m.moviesByTitle[ft] = append(m.moviesByTitle[ft], item)
				if y := item.effectiveYear(); y > 0 {
					ty := titleYearKey(item.Title, y)
					m.moviesByTitleYear[ty] = append(m.moviesByTitleYear[ty], item)
				}
			}
		}
	}
}

func (m *sourceMapper) indexHistory(items []media, accountID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.titleToGrandparents == nil {
		m.titleToGrandparents = map[string][]string{}
	}
	for _, item := range items {
		if item.AccountID != accountID {
			continue
		}
		if item.Type == "episode" {
			m.noteShowParentLocked(item.GrandparentTitle, item.grandparentID())
		}
	}
}

func (m *sourceMapper) noteShowParent(title, gp string) {
	if title == "" || gp == "" {
		return
	}
	m.mu.Lock()
	if m.titleToGrandparents == nil {
		m.titleToGrandparents = map[string][]string{}
	}
	m.noteShowParentLocked(title, gp)
	m.mu.Unlock()
}

func (m *sourceMapper) noteShowParentLocked(title, gp string) {
	ft := foldTitle(title)
	if ft == "" || gp == "" {
		return
	}
	for _, existing := range m.titleToGrandparents[ft] {
		if existing == gp {
			return
		}
	}
	m.titleToGrandparents[ft] = append(m.titleToGrandparents[ft], gp)
}

func (m *sourceMapper) cacheShowTitle(title string, show core.RatingKey) {
	ft := foldTitle(title)
	if ft == "" || show == 0 {
		return
	}
	m.mu.Lock()
	if m.showsByTitle == nil {
		m.showsByTitle = map[string]core.RatingKey{}
	}
	if m.showsByTitle[ft] == 0 {
		m.showsByTitle[ft] = show
	}
	m.mu.Unlock()
}

func (m *sourceMapper) enrichFromLibrary(item media) media {
	if item.Key == "" {
		return item
	}
	m.mu.Lock()
	lib, ok := m.libraryByKey[item.Key]
	m.mu.Unlock()
	if !ok {
		return item
	}
	return mergeMedia(item, lib)
}

func mergeMedia(dst, src media) media {
	if dst.Type == "" {
		dst.Type = src.Type
	}
	if dst.Title == "" {
		dst.Title = src.Title
	}
	if dst.OrigTitle == "" {
		dst.OrigTitle = src.OrigTitle
	}
	if dst.Year == 0 {
		dst.Year = src.Year
	}
	if dst.AirDate == "" {
		dst.AirDate = src.AirDate
	}
	if dst.GUID == "" {
		dst.GUID = src.GUID
	}
	if len(dst.GUIDs) == 0 {
		dst.GUIDs = src.GUIDs
	}
	if dst.Grandparent == "" {
		dst.Grandparent = src.Grandparent
	}
	if dst.GrandparentKey == "" {
		dst.GrandparentKey = src.GrandparentKey
	}
	if dst.GrandparentTitle == "" {
		dst.GrandparentTitle = src.GrandparentTitle
	}
	if dst.ParentKey == "" {
		dst.ParentKey = src.ParentKey
	}
	if dst.Season == 0 {
		dst.Season = src.Season
	}
	if dst.Episode == 0 {
		dst.Episode = src.Episode
	}
	return dst
}

func (m *sourceMapper) cached(source string) core.RatingKey {
	if source == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[source]
}

func (m *sourceMapper) resolve(ctx context.Context, item media, fetched bool) (core.RatingKey, error) {
	if item.Key == "" {
		return m.resolveBody(ctx, item, fetched)
	}
	m.mu.Lock()
	if key := m.keys[item.Key]; key != 0 {
		m.mu.Unlock()
		return key, nil
	}
	if m.missing[item.Key] {
		m.mu.Unlock()
		return 0, core.ErrNotFound
	}
	if m.inflight == nil {
		m.inflight = map[string]*mapCall{}
	}
	if call := m.inflight[item.Key]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-call.done:
			return call.key, call.err
		}
	}
	call := &mapCall{done: make(chan struct{})}
	m.inflight[item.Key] = call
	m.mu.Unlock()

	key, err := m.resolveBody(ctx, item, fetched)
	m.mu.Lock()
	if err == nil {
		m.keys[item.Key] = key
	} else if !errors.Is(err, context.Canceled) {
		m.missing[item.Key] = true
	}
	delete(m.inflight, item.Key)
	call.key, call.err = key, err
	close(call.done)
	m.mu.Unlock()
	return key, err
}

func (m *sourceMapper) resolveBody(ctx context.Context, item media, fetched bool) (core.RatingKey, error) {
	item = m.enrichFromLibrary(item)
	var key core.RatingKey
	err := core.ErrNotFound
	if item.Type == "episode" {
		if gp := item.grandparentID(); gp != "" && episodeKeyFits(item.Season, item.Episode) {
			m.noteShowParent(item.GrandparentTitle, gp)
			show, ok := m.showFor(ctx, gp)
			if !ok && ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if ok {
				m.cacheShowTitle(item.GrandparentTitle, show)
				return core.EpisodeKey(show.TMDB(), item.Season, item.Episode), nil
			}
		}
	}
	key, err = m.service.mapMediaGUIDs(ctx, m.base, m.token, item)
	if err == nil && key != 0 {
		if item.Type == "episode" && item.GrandparentTitle != "" && key.Kind() == core.KindEpisode {
			m.cacheShowTitle(item.GrandparentTitle, core.ShowKey(key.TMDB()))
		}
		return key, nil
	}
	if errors.Is(err, context.Canceled) {
		return 0, err
	}
	if !fetched && item.Key != "" {
		c, e := m.service.get(ctx, m.base, m.token, "/library/metadata/"+url.PathEscape(item.Key)+"?includeGuids=1")
		if errors.Is(e, context.Canceled) {
			return 0, e
		}
		if e == nil {
			details := append(c.Videos, c.Metadata...)
			if len(details) > 0 {
				detail := mergeMedia(details[0], item)
				if detail.Type == "episode" {
					m.noteShowParent(detail.GrandparentTitle, detail.grandparentID())
				}
				// Resolve the fetched record directly so this worker never waits
				// on an in-flight key (even if PMS aliases two ratingKeys).
				key, err = m.resolveBody(ctx, detail, true)
				if err == nil && key != 0 {
					return key, nil
				}
				if errors.Is(err, context.Canceled) {
					return 0, err
				}
			}
		}
	}
	switch item.Type {
	case "episode":
		if validEpisodeNumbers(item.Season, item.Episode) && item.GrandparentTitle != "" {
			show, ok := m.showForTitle(ctx, item.GrandparentTitle, item.effectiveYear(), item.Season)
			if !ok && ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if ok {
				return core.EpisodeKey(show.TMDB(), item.Season, item.Episode), nil
			}
		}
	case "movie":
		if k, ok := m.movieFromLibrary(ctx, item.Title, item.effectiveYear(), item.Key); ok {
			return k, nil
		} else if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return m.service.mapMediaSearch(ctx, item)
	case "show":
		return m.service.mapMediaSearch(ctx, item)
	}
	return key, err
}

func (m *sourceMapper) movieFromLibrary(ctx context.Context, title string, year int, excludeKey string) (core.RatingKey, bool) {
	ft := foldTitle(title)
	if ft == "" {
		return 0, false
	}
	m.mu.Lock()
	var candidates []media
	if year > 0 {
		candidates = append(candidates, m.moviesByTitleYear[titleYearKey(title, year)]...)
	}
	if len(candidates) == 0 {
		all := m.moviesByTitle[ft]
		if year == 0 && len(all) == 1 {
			candidates = append(candidates, all[0])
		} else if year > 0 {
			for _, cand := range all {
				cy := cand.effectiveYear()
				if cy > 0 && (cy == year-1 || cy == year+1) {
					candidates = append(candidates, cand)
				}
			}
		}
	}
	m.mu.Unlock()

	var resolved core.RatingKey
	for _, cand := range candidates {
		if cand.Key != "" && cand.Key == excludeKey {
			continue
		}
		k := m.cached(cand.Key)
		if k == 0 {
			var err error
			k, err = m.service.mapMediaGUIDs(ctx, m.base, m.token, cand)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return 0, false
				}
				continue
			}
		}
		if k == 0 {
			continue
		}
		if resolved != 0 && resolved != k {
			return 0, false
		}
		resolved = k
	}
	return resolved, resolved != 0
}

func (m *sourceMapper) showForTitle(ctx context.Context, title string, maxAirYear, season int) (core.RatingKey, bool) {
	ft := foldTitle(title)
	if ft == "" {
		return 0, false
	}
	const prefix = "gpt\x00"
	id := prefix + ft
	m.mu.Lock()
	if m.showsByTitle == nil {
		m.showsByTitle = map[string]core.RatingKey{}
	}
	if show, seen := m.showsByTitle[ft]; seen {
		m.mu.Unlock()
		return show, show != 0
	}
	if m.inflight == nil {
		m.inflight = map[string]*mapCall{}
	}
	if call := m.inflight[id]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, false
		case <-call.done:
		}
		m.mu.Lock()
		show := m.showsByTitle[ft]
		m.mu.Unlock()
		return show, show != 0
	}
	call := &mapCall{done: make(chan struct{})}
	m.inflight[id] = call
	gps := append([]string(nil), m.titleToGrandparents[ft]...)
	m.mu.Unlock()

	var show core.RatingKey
	for _, gp := range gps {
		cand, ok := m.showFor(ctx, gp)
		if !ok {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if show != 0 && show != cand {
			show = 0
			break
		}
		show = cand
	}
	if show == 0 && ctx.Err() == nil {
		if t, err := m.service.searchShow(ctx, title, maxAirYear, season); err == nil && t != nil && t.Key != 0 {
			show = t.Key
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, id)
	close(call.done)
	if ctx.Err() != nil && show == 0 {
		return 0, false
	}
	m.showsByTitle[ft] = show
	return show, show != 0
}

func (m *sourceMapper) showFor(ctx context.Context, grandparent string) (core.RatingKey, bool) {
	const prefix = "gp\x00"
	id := prefix + grandparent
	m.mu.Lock()
	if show, seen := m.parents[grandparent]; seen {
		m.mu.Unlock()
		return show, show != 0
	}
	if m.inflight == nil {
		m.inflight = map[string]*mapCall{}
	}
	if call := m.inflight[id]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, false
		case <-call.done:
		}
		m.mu.Lock()
		show := m.parents[grandparent]
		m.mu.Unlock()
		return show, show != 0
	}
	call := &mapCall{done: make(chan struct{})}
	m.inflight[id] = call
	m.mu.Unlock()

	var show core.RatingKey
	var showTitle string
	c, e := m.service.get(ctx, m.base, m.token, "/library/metadata/"+url.PathEscape(grandparent)+"?includeGuids=1")
	if e == nil {
		for _, d := range c.Directories {
			show, _ = m.service.mapMedia(ctx, m.base, m.token, media{Type: "show", Title: d.Title, OrigTitle: d.OrigTitle, Year: d.Year, AirDate: d.AirDate, GUID: d.GUID, GUIDs: d.GUIDs})
			if show != 0 {
				showTitle = d.Title
				break
			}
		}
		if show == 0 {
			for _, item := range append(c.Videos, c.Metadata...) {
				item.Type = "show"
				show, _ = m.service.mapMedia(ctx, m.base, m.token, item)
				if show != 0 {
					showTitle = item.Title
					break
				}
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, id)
	close(call.done)
	if errors.Is(e, context.Canceled) && show == 0 {
		return 0, false
	}
	m.parents[grandparent] = show
	if show != 0 && showTitle != "" {
		if m.showsByTitle == nil {
			m.showsByTitle = map[string]core.RatingKey{}
		}
		if ft := foldTitle(showTitle); ft != "" && m.showsByTitle[ft] == 0 {
			m.showsByTitle[ft] = show
		}
	}
	return show, show != 0
}
