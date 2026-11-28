package livetv

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/store"
)

func (s *Service) catchUpChannels(ctx context.Context, u *core.User, first bool) (context.Context, []Channel, error) {
	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil || pol.Disabled {
		return ctx, nil, err
	}
	archived, err := s.archiveChannels(ctx)
	if err != nil {
		return ctx, nil, err
	}
	var out []Channel
	for _, ch := range archived {
		if s.allowedWithLookup(ctx, u, ch, nil, nil, false) {
			out = append(out, ch)
			if first {
				break
			}
		}
	}
	return ctx, out, nil
}

func (s *Service) archiveChannels(ctx context.Context) ([]Channel, error) {
	_, snap, err := s.channelSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	return snap.archived, nil
}

// CatchUpMaxDays reports the archive window of the viewer's visible channels.
// Only IPTV sources can supply archives, so this capability read never needs
// to prepare a virtual channel or load guide data for the rest of the lineup.
func (s *Service) CatchUpMaxDays(ctx context.Context, u *core.User) (int, error) {
	archived, err := s.archiveChannels(ctx)
	if err != nil || len(archived) == 0 {
		return 0, err
	}
	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return 0, err
	}
	channels, err := s.filterAllowedChannels(ctx, u, pol, archived)
	days := 0
	for _, ch := range channels {
		days = max(days, ch.ArchiveDays())
	}
	return days, err
}

func (h liveTVHost) CatchUpLibraryVisible(ctx context.Context, u *frontend.Viewer) (bool, error) {
	_, channels, err := h.s.catchUpChannels(ctx, u, true)
	return len(channels) > 0, err
}

func catchUpLibraryItem(p Programme, kind frontend.MediaType) frontend.CatchUpItem {
	out := frontend.CatchUpItem{
		Type: kind, Programme: toFrontendProgramme(p),
		ShowID:   frontend.CatchUpGroupID(frontend.TypeShow, p.Item.Title),
		SeasonID: frontend.CatchUpGroupID(frontend.TypeSeason, p.Item.Title),
	}
	switch kind {
	case frontend.TypeShow:
		out.ID = out.ShowID
	case frontend.TypeSeason:
		out.ID = out.SeasonID
	case frontend.TypeEpisode:
		out.ID = frontend.CatchUpEpisodeID(p.ID)
	}
	return out
}

func (h liveTVHost) CatchUpItem(ctx context.Context, u *frontend.Viewer, id string) (frontend.CatchUpItem, error) {
	s := h.s
	if key, start, ok := frontend.CatchUpGUIDAiring(id); ok {
		_, snapshot, err := s.channelSnapshot(ctx)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		snapshot.mu.Lock()
		channel := snapshot.archiveIDs[key]
		snapshot.mu.Unlock()
		if channel == "" {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		id = frontend.CatchUpEpisodeID(fmt.Sprintf("%s-%d", channel, start.UnixMilli()))
	}
	_, groupKind := frontend.CatchUpGUIDGroup(id)
	if req, ok := frontend.CatchUpEpisodeRequest(id); ok {
		ctx, policy, err := s.withPolicy(ctx, u)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		if policy.Disabled {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		ch, err := s.Channel(ctx, req.ChannelID)
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		stamp, _ := strconv.ParseInt(strings.TrimPrefix(req.ProgrammeID, ch.ID+"-"), 10, 64)
		now := s.clockNow()
		if ch.ArchiveDays() <= 0 || time.UnixMilli(stamp).Before(now.Add(-time.Duration(ch.ArchiveDays())*24*time.Hour)) ||
			!s.allowedWithLookup(ctx, u, ch, nil, nil, false) {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		p, found, err := s.programmeAt(ctx, ch, time.UnixMilli(stamp))
		if err != nil {
			return frontend.CatchUpItem{}, err
		}
		eligible, _ := frontend.ArchiveFlags(ch.ArchiveDays(), p.Start, p.End, now)
		if !found || p.ID != req.ProgrammeID || !eligible || !s.allowedProgramme(ctx, u, ch, &p) {
			return frontend.CatchUpItem{}, core.ErrNotFound
		}
		item := []frontend.CatchUpItem{catchUpLibraryItem(p, frontend.TypeEpisode)}
		if err := s.fillCatchUpWatch(ctx, u, item, nil); err != nil {
			return frontend.CatchUpItem{}, err
		}
		return item[0], nil
	}
	if groupKind == "" && !strings.HasPrefix(id, "catchup-show-") && !strings.HasPrefix(id, "catchup-season-") {
		return frontend.CatchUpItem{}, core.ErrNotFound
	}
	page, _, err := h.catchUpItems(ctx, u, frontend.CatchUpQuery{Limit: 1}, id)
	if err != nil {
		return frontend.CatchUpItem{}, err
	}
	if len(page.Items) == 0 {
		return frontend.CatchUpItem{}, core.ErrNotFound
	}
	return page.Items[0], nil
}

func (h liveTVHost) CatchUpItems(ctx context.Context, u *frontend.Viewer, q frontend.CatchUpQuery) (frontend.CatchUpPage, error) {
	page, view, err := h.catchUpItems(ctx, u, q, "")
	if err != nil {
		return page, err
	}
	// The roll-up reads the airings of the listed shows and seasons only. A
	// search lists only matching airings, so its counts would be partial; it
	// gets none.
	var members map[string][]string
	if q.Rollup && strings.TrimSpace(q.Search) == "" {
		var groups []string
		for _, item := range page.Items {
			if item.Type != frontend.TypeEpisode {
				groups = append(groups, item.ID)
			}
		}
		if members, err = h.catchUpMembers(ctx, u, view, groups); err != nil {
			return page, err
		}
	}
	return page, h.s.fillCatchUpWatch(ctx, u, page.Items, members)
}

// sortCatchUpItems reorders a newest-first listing. Equal titles keep their
// newest-first order.
func sortCatchUpItems(items []frontend.CatchUpItem, order frontend.CatchUpSort) {
	switch order {
	case frontend.CatchUpSortOldest:
		slices.Reverse(items)
	case frontend.CatchUpSortTitle, frontend.CatchUpSortTitleDesc:
		slices.SortStableFunc(items, func(a, b frontend.CatchUpItem) int {
			c := cmp.Compare(catchUpSortTitle(a), catchUpSortTitle(b))
			if order == frontend.CatchUpSortTitleDesc {
				c = -c
			}
			return c
		})
	}
}

func catchUpSortTitle(item frontend.CatchUpItem) string {
	title := item.Programme.Item.Title
	if item.Type == frontend.TypeEpisode && item.Programme.Item.EpisodeTitle != "" {
		title += " " + item.Programme.Item.EpisodeTitle
	}
	return strings.ToLower(title)
}

// catchUpItems lists one page. It also returns the viewer's catch-up view it
// read (nil when the listing had none), for a roll-up of the page.
func (h liveTVHost) catchUpItems(ctx context.Context, u *core.User, q frontend.CatchUpQuery, exact string) (frontend.CatchUpPage, *catchUpView, error) {
	var out frontend.CatchUpPage
	var search string
	if raw := strings.TrimSpace(q.Search); raw != "" {
		if utf8.RuneCountInString(raw) > 256 {
			return out, nil, nil
		}
		search = catchUpSearchKey(raw)
		if search == "" {
			return out, nil, nil
		}
	}
	ctx, view, err := h.catchUpViewOf(ctx, u)
	if err != nil || view == nil {
		return out, nil, err
	}
	channels, policy, ratings, maxDays := view.channels, view.policy, view.ratings, view.maxDays
	parent, parentKind := frontend.CatchUpGUIDGroup(q.Parent)
	exactGUID, exactKind := frontend.CatchUpGUIDGroup(exact)
	show := parentKind == frontend.TypeShow || strings.HasPrefix(q.Parent, "catchup-show-")
	season := parentKind == frontend.TypeSeason || strings.HasPrefix(q.Parent, "catchup-season-")
	if q.Parent != "" && q.Parent != frontend.CatchUpLibraryID && !show && !season {
		return out, nil, nil
	}
	kind := frontend.TypeShow
	switch {
	case q.Recursive || season:
		kind = frontend.TypeEpisode
	case show || exactKind == frontend.TypeSeason || strings.HasPrefix(exact, "catchup-season-"):
		kind = frontend.TypeSeason
	}
	limit := min(q.Limit, 1000)
	if limit <= 0 {
		limit = 100
	}
	// Sorted and filtered pages can share one complete archive window.
	// Plain newest-first browsing keeps its early-return path.
	fullWindow := q.Sort != frontend.CatchUpSortRecent || search != ""
	offset := max(q.Offset, 0)
	now := h.s.clockNow()
	var key catchUpListKey
	var cacheable bool
	if fullWindow {
		if revision, err := h.s.GuideRevision(ctx, u); err == nil {
			ids := make([]string, 0, len(channels))
			for _, ch := range channels {
				ids = append(ids, ch.ID)
			}
			key = catchUpListKey{
				revision: stableID(revision, view.policy.MaxRating, fmt.Sprint(view.ratings), fmt.Sprint(ids)),
				parent:   q.Parent, search: search, exact: exact, kind: kind, order: q.Sort,
			}
			if cached, ok := h.s.cachedCatchUpList(key, now); ok {
				return catchUpListPage(cached, offset, limit), view, nil
			}
			cacheable = true
		}
	}
	seen := map[string]bool{}
	earliest := now.Add(-time.Duration(maxDays) * 24 * time.Hour)
	// Each batch is a disjoint start-time interval. Process the newest
	// interval first so small pages do not read the entire archive.
	for until := now.Add(time.Millisecond); maxDays > 0 && until.After(earliest); {
		from := until.Add(-12 * time.Hour)
		if from.Before(earliest) {
			from = earliest
		}
		rows, err := h.s.catchUpWindow(ctx, u, policy, channels, ratings, from, until, now, search)
		if err != nil {
			return frontend.CatchUpPage{}, nil, err
		}
		for _, p := range rows {
			item := catchUpLibraryItem(p, kind)
			if exact != "" && item.ID != exact && (exactGUID == "" || frontend.CatchUpGroupGUID(item.ID) != exactGUID) ||
				show && item.ShowID != q.Parent && (parent == "" || frontend.CatchUpGroupGUID(item.ShowID) != parent) ||
				season && item.SeasonID != q.Parent && (parent == "" || frontend.CatchUpGroupGUID(item.SeasonID) != parent) {
				continue
			}
			if kind != frontend.TypeEpisode {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			if fullWindow || out.Total >= offset && len(out.Items) < limit {
				out.Items = append(out.Items, item)
				// Linked titles have additional policy checks outside the
				// guide revision, so keep reading those lists live.
				if p.Item.Key != 0 && h.s.o.Catalog != nil {
					cacheable = false
				}
			}
			out.Total++
			if !fullWindow && (exact != "" || kind == frontend.TypeSeason && q.Parent != "" || len(out.Items) == limit && out.Total-offset > limit) {
				return out, view, nil
			}
		}
		until = from
	}
	if fullWindow {
		sortCatchUpItems(out.Items, q.Sort)
		if cacheable && len(out.Items) <= catchUpListMaxItems {
			h.s.storeCatchUpList(key, out.Items, channels, now)
			return catchUpListPage(out.Items, offset, limit), view, nil
		}
		out.Items = paging.Slice(out.Items, offset, limit)
	}
	return out, view, nil
}

const (
	catchUpListTTL        = 5 * time.Second
	catchUpListMaxEntries = 16
	catchUpListMaxItems   = 4000
)

type catchUpListKey struct {
	revision, parent, search, exact string
	kind                            frontend.MediaType
	order                           frontend.CatchUpSort
}

type catchUpListEntry struct {
	items         []frontend.CatchUpItem
	at, expiresAt time.Time
}

func catchUpListPage(items []frontend.CatchUpItem, offset, limit int) frontend.CatchUpPage {
	page := slices.Clone(paging.Slice(items, offset, limit))
	for i := range page {
		page[i].Programme.Item.Genres = slices.Clone(page[i].Programme.Item.Genres)
	}
	return frontend.CatchUpPage{Items: page, Total: len(items)}
}

func (s *Service) cachedCatchUpList(key catchUpListKey, now time.Time) ([]frontend.CatchUpItem, bool) {
	s.catchUpListMu.Lock()
	defer s.catchUpListMu.Unlock()
	entry, ok := s.catchUpLists[key]
	return entry.items, ok && !now.Before(entry.at) && now.Before(entry.expiresAt)
}

func (s *Service) storeCatchUpList(key catchUpListKey, items []frontend.CatchUpItem, channels []Channel, now time.Time) {
	expiresAt := now.Add(catchUpListTTL)
	archiveDays := make(map[string]int, len(channels))
	for _, ch := range channels {
		archiveDays[ch.ID] = min(ch.ArchiveDays(), 30)
	}
	for _, item := range items {
		p := item.Programme
		expiresAt = minTime(expiresAt, p.Start.Add(time.Duration(archiveDays[p.ChannelID])*24*time.Hour))
	}
	if !now.Before(expiresAt) {
		return
	}

	s.catchUpListMu.Lock()
	defer s.catchUpListMu.Unlock()
	if s.catchUpLists == nil {
		s.catchUpLists = make(map[catchUpListKey]catchUpListEntry)
	}
	var oldestKey catchUpListKey
	var oldestAt time.Time
	for k, entry := range s.catchUpLists {
		if !now.Before(entry.expiresAt) {
			delete(s.catchUpLists, k)
			continue
		}
		if oldestAt.IsZero() || entry.at.Before(oldestAt) {
			oldestKey, oldestAt = k, entry.at
		}
	}
	if previous, exists := s.catchUpLists[key]; exists {
		if previous.at.After(now) {
			return
		}
	} else if len(s.catchUpLists) >= catchUpListMaxEntries {
		delete(s.catchUpLists, oldestKey)
	}
	s.catchUpLists[key] = catchUpListEntry{items: items, at: now, expiresAt: expiresAt}
}

// catchUpView is what every catch-up read checks airings against for one
// viewer: the archive channels they may watch, the policy, each channel's
// content rating when a rating limit applies, and the longest archive
// (capped at 30 days).
type catchUpView struct {
	channels []Channel
	policy   Policy
	ratings  map[string]string
	maxDays  int
}

// catchUpViewOf reads the viewer's catch-up view; it is nil when they have no
// archive channels.
func (h liveTVHost) catchUpViewOf(ctx context.Context, u *core.User) (context.Context, *catchUpView, error) {
	ctx, channels, err := h.s.catchUpChannels(ctx, u, false)
	if err != nil || len(channels) == 0 {
		return ctx, nil, err
	}
	_, policy, err := h.s.withPolicy(ctx, u)
	if err != nil {
		return ctx, nil, err
	}
	v := &catchUpView{channels: channels, policy: policy, ratings: make(map[string]string)}
	for _, ch := range channels {
		if ch.GuideID == "" {
			continue
		}
		v.maxDays = max(v.maxDays, min(ch.ArchiveDays(), 30))
		if u.MaxContentRating != "" || policy.MaxRating != "" {
			v.ratings[ch.ID] = h.s.channelContentRating(ctx, ch, nil)
		}
	}
	return ctx, v, nil
}

func (s *Service) catchUpWindow(ctx context.Context, u *core.User, policy Policy, channels []Channel, ratings map[string]string, from, until, now time.Time, search string) ([]Programme, error) {
	var out []Programme
	byID := make(map[string]Channel, len(channels))
	for batch := range slices.Chunk(channels, 100) {
		values := make([]string, 0, len(batch))
		args := make([]any, 0, 4*len(batch)+2)
		for _, ch := range batch {
			earliest := now.Add(-time.Duration(ch.ArchiveDays()) * 24 * time.Hour)
			if ch.GuideID == "" || !earliest.Before(until) {
				continue
			}
			values = append(values, "(CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT))")
			args = append(args, ch.ID, guideSource(ch), ch.GuideID, max(from.UnixMilli(), earliest.UnixMilli()))
			byID[ch.ID] = ch
		}
		if len(values) == 0 {
			continue
		}
		args = append(args, until.UnixMilli(), now.UnixMilli())
		columns := "archive.id, p.data"
		if search != "" {
			dialect := store.DialectOf(s.o.DB)
			item := store.JSONText(dialect, "p.data", "item")
			columns += ", COALESCE(" + store.JSONText(dialect, item, "title") + ",''), COALESCE(" + store.JSONText(dialect, item, "episodeTitle") + ",'')"
		}
		rows, err := s.o.DB.QueryContext(ctx, `WITH archive(id, source, guide, earliest) AS (VALUES `+strings.Join(values, ",")+`)
			SELECT `+columns+` FROM archive JOIN livetv_programmes p
			ON p.source_id=archive.source AND p.channel_id=archive.guide AND p.start_ms>=archive.earliest
			WHERE p.start_ms<? AND p.end_ms<=? AND p.end_ms>p.start_ms`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var raw []byte
			if search != "" {
				var title, episode string
				if err = rows.Scan(&id, &raw, &title, &episode); err != nil {
					break
				}
				if !strings.Contains(catchUpSearchKey(title), search) && !strings.Contains(catchUpSearchKey(episode), search) {
					continue
				}
			} else if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var p Programme
			var ok bool
			if p, ok, err = catchUpRow(byID[id], raw, now); ok {
				out = append(out, p)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return s.catchUpAllowed(ctx, u, policy, ratings, byID, out)
}

// catchUpRow decodes one guide row of an archive channel into its airing,
// and reports whether it is a past airing the archive still holds.
func catchUpRow(ch Channel, raw []byte, now time.Time) (Programme, bool, error) {
	var p Programme
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, false, err
	}
	eligible, _ := frontend.ArchiveFlags(ch.ArchiveDays(), p.Start, p.End, now)
	if !eligible || p.Item.Title == "" {
		return p, false, nil
	}
	p.ChannelID = ch.ID
	p.ID = fmt.Sprintf("%s-%d", ch.ID, p.Start.UnixMilli())
	return p, true, nil
}

// catchUpAllowed keeps the airings this viewer may play (byID holds their
// channels), newest first.
func (s *Service) catchUpAllowed(ctx context.Context, u *core.User, policy Policy, ratings map[string]string, byID map[string]Channel, out []Programme) ([]Programme, error) {
	lookup, err := s.catchUpTitleFacts(ctx, out)
	if err != nil {
		return nil, err
	}
	out = slices.DeleteFunc(out, func(p Programme) bool {
		ch := byID[p.ChannelID]
		rating := resolveChannelRating(ch, &p, "", ratings[ch.ID], false)
		return !s.allowedProgrammeContent(ctx, u, ch, &p, policy, rating, lookup, s.o.Catalog != nil)
	})
	slices.SortFunc(out, func(a, b Programme) int {
		if n := b.Start.Compare(a.Start); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Imported airings carry their own policy facts. A linked airing also needs
// its local title and parent facts, loaded in batches for the current window.
func (s *Service) catchUpTitleFacts(ctx context.Context, programmes []Programme) (func(context.Context, core.RatingKey) (*core.Title, error), error) {
	catalog, ok := s.o.Catalog.(interface {
		CachedGetMany(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	})
	if !ok {
		return nil, nil
	}
	var keys []core.RatingKey
	seen := make(map[core.RatingKey]bool)
	for _, p := range programmes {
		if p.Item.Key != 0 && !seen[p.Item.Key] {
			keys = append(keys, p.Item.Key)
			seen[p.Item.Key] = true
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	titles := make(map[core.RatingKey]core.Title, len(keys))
	fill := func(keys []core.RatingKey) error {
		for batch := range slices.Chunk(keys, 200) {
			found, err := catalog.CachedGetMany(ctx, batch)
			if err != nil {
				return err
			}
			for key, title := range found {
				titles[key] = title
			}
		}
		return nil
	}
	if err := fill(keys); err != nil {
		return nil, err
	}
	var parents []core.RatingKey
	for _, title := range titles {
		if title.ShowKey != 0 && !seen[title.ShowKey] {
			parents = append(parents, title.ShowKey)
			seen[title.ShowKey] = true
		}
	}
	if err := fill(parents); err != nil {
		return nil, err
	}
	return func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		title, ok := titles[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}, nil
}

func (h liveTVHost) FindCatchUp(ctx context.Context, viewer *frontend.Viewer, req CatchUpRequest, caps frontend.ClientCaps, clientSession string) (CatchUpInfo, bool) {
	if viewer == nil || clientSession == "" {
		return CatchUpInfo{}, false
	}
	h.s.mu.Lock()
	var candidate CatchUpInfo
	for _, ss := range h.s.sessions {
		ss.mu.Lock()
		if !ss.closed && ss.catchUp != nil && ss.info.UserID == viewer.ID &&
			ss.info.ClientSessionID == clientSession && ss.caps.ClientID == caps.ClientID && ss.caps.Protocol == caps.Protocol {
			info := ss.catchUp.info
			if info.ChannelID == req.ChannelID && info.ProgrammeID == req.ProgrammeID && info.DASH == req.DASH && !info.StartOver {
				candidate = info
			}
		}
		ss.mu.Unlock()
		if candidate.ID != "" {
			break
		}
	}
	h.s.mu.Unlock()
	if candidate.ID == "" {
		return CatchUpInfo{}, false
	}
	_, err := h.s.getSession(ctx, candidate.ID, viewer)
	return candidate, err == nil
}

func (h liveTVHost) LibraryCatchUpSession(access frontend.SessionAccess, id string) (CatchUpInfo, bool) {
	if access.Viewer == nil {
		return CatchUpInfo{}, false
	}
	h.s.mu.Lock()
	ss := h.s.sessions[id]
	h.s.mu.Unlock()
	if ss == nil {
		return CatchUpInfo{}, false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed || ss.catchUp == nil || ss.info.UserID != access.Viewer.ID || ss.caps.Protocol != access.Protocol ||
		(access.Device.ID != "" && ss.caps.ClientID != access.Device.ID) {
		return CatchUpInfo{}, false
	}
	return ss.catchUp.info, true
}

// catchUpSearchKey folds a title or query to lower-case letters and digits,
// so "world news tonight", "World-News Tonight" and "worldnews" all match.
func catchUpSearchKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}
