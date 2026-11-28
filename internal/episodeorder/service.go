// Package episodeorder provides TV presentation and independent release preferences. Canonical TMDB
// keys and season/episode identity never change, including during playback.
package episodeorder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func init() {
	store.Register("episodeorder", 1, `CREATE TABLE episodeorder_preferences(user_id INTEGER NOT NULL,show_key INTEGER NOT NULL,group_id TEXT NOT NULL,specials TEXT NOT NULL DEFAULT 'last',PRIMARY KEY(user_id,show_key)); CREATE TABLE episodeorder_groups(id TEXT PRIMARY KEY,body TEXT NOT NULL,updated_at INTEGER NOT NULL);`)
	store.Register("episodeorder", 2, `CREATE TABLE episodeorder_release_preferences(show_key INTEGER PRIMARY KEY,mode TEXT NOT NULL,group_id TEXT NOT NULL);`)
}

type Source interface {
	EpisodeGroups(context.Context, int64) ([]tmdb.EpisodeGroupSummary, error)
	EpisodeGroup(context.Context, string) (*tmdb.EpisodeGroup, error)
}
type Preference struct {
	GroupID   string `json:"groupId"`
	Specials  string `json:"specials"` // first, last, hidden; included group specials retain their chosen position
	Inherited bool   `json:"inherited,omitempty"`
}
type cachedLayout struct {
	value   *layout
	expires time.Time
}
type Service struct {
	db         *sql.DB
	source     Source
	catalog    core.Catalog
	mu         sync.Mutex
	cache      map[string]cachedLayout
	generation uint64 // changes whenever preferences invalidate in-flight layouts

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
}

// SetBus connects the cluster bus (nil disconnects): preference changes on
// any node drop every node's cached episode layouts.
func (s *Service) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "episodeorder", nil, func(context.Context, []string, bool) { s.invalidate() })
	s.invalMu.Lock()
	old := s.inval
	s.inval = iv
	s.invalMu.Unlock()
	old.Close()
}

// changed drops cached layouts here and on the other nodes.
func (s *Service) changed() {
	s.invalidate()
	s.invalMu.Lock()
	iv := s.inval
	s.invalMu.Unlock()
	iv.Notify("")
}

func New(db *sql.DB, source Source, catalog core.Catalog) *Service {
	return &Service{db: db, source: source, catalog: catalog, cache: map[string]cachedLayout{}}
}

func (s *Service) groupsConfigured() bool {
	if keyed, ok := s.source.(interface{ HasKey() bool }); ok {
		return keyed.HasKey()
	}
	return s.source != nil
}

func (s *Service) groups(ctx context.Context, key core.RatingKey) ([]tmdb.EpisodeGroupSummary, error) {
	if !s.groupsConfigured() {
		return nil, nil
	}
	title, err := s.catalog.Get(ctx, key)
	if errors.Is(err, core.ErrNotFound) || (err == nil && (title == nil || title.IDs.TMDB <= 0)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	groups, err := s.source.EpisodeGroups(ctx, title.IDs.TMDB)
	if errors.Is(err, tmdb.ErrNoKey) {
		return nil, nil
	}
	return groups, err
}

func (s *Service) userPreferences(ctx context.Context, user int64) (map[core.RatingKey]Preference, error) {
	s.mu.Lock()
	gen := s.generation
	s.mu.Unlock()
	key := fmt.Sprintf("episodeorder:prefs:%p:%d:%d", s, gen, user)
	return reqmemo.Do(ctx, key, func() (map[core.RatingKey]Preference, error) {
		rows, err := s.db.QueryContext(ctx, `SELECT user_id,show_key,group_id,specials FROM episodeorder_preferences WHERE user_id IN (0,?) ORDER BY user_id ASC`, user)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[core.RatingKey]Preference{}
		for rows.Next() {
			var uid, show int64
			var p Preference
			if err := rows.Scan(&uid, &show, &p.GroupID, &p.Specials); err != nil {
				return nil, err
			}
			p.Inherited = user != 0 && uid == 0
			out[core.RatingKey(show)] = p
		}
		return out, rows.Err()
	})
}

func (s *Service) Preference(ctx context.Context, user int64, show core.RatingKey) (Preference, error) {
	p := Preference{Specials: "last"}
	// A list or detail page asks for many series' preferences; one indexed read
	// per request answers Preference and Grouped for every series.
	if reqmemo.Active(ctx) {
		prefs, err := s.userPreferences(ctx, user)
		if err != nil {
			return p, err
		}
		if got, ok := prefs[show]; ok {
			return got, nil
		}
		return p, nil
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id,group_id,specials FROM episodeorder_preferences WHERE show_key=? AND user_id IN (0,?) ORDER BY user_id DESC LIMIT 1`, int64(show), user).Scan(&id, &p.GroupID, &p.Specials)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	p.Inherited = user != 0 && id == 0
	return p, err
}

// Customized reports that the viewer's preferences, or the owner's
// defaults, change some series' episode set: an episode group, or hidden
// specials. Showing specials first or last only reorders them.
func (s *Service) Customized(ctx context.Context, u *core.User) (bool, error) {
	uid := userID(u)
	query := `SELECT EXISTS (SELECT 1 FROM episodeorder_preferences WHERE user_id IN (0,?) AND (group_id<>'' OR specials='hidden'))`
	if !s.groupsConfigured() {
		query = `SELECT EXISTS (SELECT 1 FROM episodeorder_preferences WHERE user_id IN (0,?) AND specials='hidden')`
	}
	if reqmemo.Active(ctx) {
		s.mu.Lock()
		gen := s.generation
		s.mu.Unlock()
		return reqmemo.Do(ctx, fmt.Sprintf("episodeorder:custom:%p:%d:%d:%t", s, gen, uid, s.groupsConfigured()), func() (bool, error) {
			var custom bool
			err := s.db.QueryRowContext(ctx, query, uid).Scan(&custom)
			return custom, err
		})
	}
	var custom bool
	err := s.db.QueryRowContext(ctx, query, uid).Scan(&custom)
	return custom, err
}

// Grouped reports which of the shows present an alternate episode group to
// the viewer: their own preference, else the owner's default. A show without
// one keeps the canonical season tree, so a list can render it from the
// catalogue without loading its layout title by title.
func (s *Service) Grouped(ctx context.Context, u *core.User, shows []core.RatingKey) (map[core.RatingKey]bool, error) {
	grouped := map[core.RatingKey]bool{}
	if len(shows) == 0 || !s.groupsConfigured() {
		return grouped, nil
	}
	if reqmemo.Active(ctx) {
		prefs, err := s.userPreferences(ctx, userID(u))
		if err != nil {
			return nil, err
		}
		for _, k := range shows {
			sk := showKey(k)
			if prefs[sk].GroupID != "" {
				grouped[sk] = true
			}
		}
		return s.effectiveViewerGroups(ctx, grouped)
	}
	for len(shows) > 0 {
		n := min(len(shows), 500)
		args := []any{userID(u)}
		for _, k := range shows[:n] {
			args = append(args, int64(showKey(k)))
		}
		shows = shows[n:]
		// Ascending, so the viewer's own row replaces the owner's default.
		rows, e := s.db.QueryContext(ctx, `SELECT show_key,group_id FROM episodeorder_preferences WHERE user_id IN (0,?) AND show_key IN (?`+strings.Repeat(",?", n-1)+`) ORDER BY user_id ASC`, args...)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var show int64
			var group string
			if e = rows.Scan(&show, &group); e != nil {
				rows.Close()
				return nil, e
			}
			grouped[core.RatingKey(show)] = group != ""
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return nil, e
		}
	}
	for k, ok := range grouped {
		if !ok {
			delete(grouped, k)
		}
	}
	return s.effectiveViewerGroups(ctx, grouped)
}

func (s *Service) effectiveViewerGroups(ctx context.Context, grouped map[core.RatingKey]bool) (map[core.RatingKey]bool, error) {
	if len(grouped) == 0 {
		return grouped, nil
	}
	cached, ok := s.catalog.(interface {
		CachedGetMany(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	})
	if !ok {
		return grouped, nil
	}
	keys := make([]core.RatingKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	shows, err := cached.CachedGetMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	for key, title := range shows {
		if !allowsViewerOrder(title) {
			delete(grouped, key)
		}
	}
	return grouped, nil
}
func (s *Service) Save(ctx context.Context, user int64, show core.RatingKey, p Preference) error {
	if user < 0 || show.Kind() != core.KindShow {
		return fmt.Errorf("invalid viewer or show")
	}
	if p.Specials != "first" && p.Specials != "last" && p.Specials != "hidden" {
		return fmt.Errorf("specials must be first, last, or hidden")
	}
	if p.GroupID != "" {
		groups, e := s.groups(ctx, show)
		if e != nil {
			return e
		}
		valid := false
		for _, g := range groups {
			if g.ID == p.GroupID {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("episode group does not belong to show")
		}
		if _, e = s.group(ctx, p.GroupID); e != nil {
			return e
		}
	}
	defer s.changed()
	_, e := s.db.ExecContext(ctx, `INSERT INTO episodeorder_preferences(user_id,show_key,group_id,specials) VALUES(?,?,?,?) ON CONFLICT(user_id,show_key) DO UPDATE SET group_id=excluded.group_id,specials=excluded.specials`, user, int64(show), p.GroupID, p.Specials)
	return e
}
func (s *Service) Reset(ctx context.Context, user int64, show core.RatingKey) error {
	defer s.changed()
	_, e := s.db.ExecContext(ctx, `DELETE FROM episodeorder_preferences WHERE user_id=? AND show_key=?`, user, int64(show))
	return e
}

// MetadataGroup reads the existing shared group cache after verifying show ownership.
func (s *Service) MetadataGroup(ctx context.Context, id int64, groupID string) (*tmdb.EpisodeGroup, error) {
	if s.source == nil || id <= 0 {
		return nil, core.ErrUnsupported
	}
	groups, err := s.source.EpisodeGroups(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if group.ID == groupID {
			return s.group(ctx, groupID)
		}
	}
	return nil, core.ErrNotFound
}

func (s *Service) group(ctx context.Context, id string) (*tmdb.EpisodeGroup, error) {
	var body string
	var at int64
	if e := s.db.QueryRowContext(ctx, `SELECT body,updated_at FROM episodeorder_groups WHERE id=?`, id).Scan(&body, &at); e == nil && !core.MetadataRefreshRequested(ctx) && time.Since(time.Unix(at, 0)) < 24*time.Hour {
		var g tmdb.EpisodeGroup
		if json.Unmarshal([]byte(body), &g) == nil {
			return &g, nil
		}
	}
	g, e := s.source.EpisodeGroup(ctx, id)
	if e != nil {
		var stale tmdb.EpisodeGroup
		if body != "" && json.Unmarshal([]byte(body), &stale) == nil {
			return &stale, nil
		}
		return nil, e
	}
	if len(g.Groups) > 999 {
		return nil, fmt.Errorf("too many episode groups")
	}
	b, e := json.Marshal(g)
	if e != nil {
		return nil, e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO episodeorder_groups(id,body,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,updated_at=excluded.updated_at`, id, string(b), time.Now().Unix())
	return g, e
}

// Presentation seasons use a reserved range; no synthetic episode key is ever
// created. Unlisted canonical episodes remain in an explicit Other episodes row.
const virtualBase = 9000
const remainder = 9999

type position struct {
	season, episode int
	name            string
}
type layout struct {
	positions map[core.RatingKey]position
	seasons   []core.Title
	episodes  []core.Title
}

func userID(u *core.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}
func showKey(k core.RatingKey) core.RatingKey {
	switch k.Kind() {
	case core.KindShow:
		return k
	case core.KindSeason:
		return k.Parent()
	case core.KindEpisode:
		return k.Parent().Parent()
	}
	return 0
}

func allowsViewerOrder(t core.Title) bool {
	return !t.Key.Allocated() && (t.EpisodeOrder.Source == "" ||
		t.EpisodeOrder.Source == core.MetadataTMDB && t.EpisodeOrder.Kind == core.OrderAired)
}

func (s *Service) overlayShow(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	return reqmemo.Do(ctx, fmt.Sprintf("episodeorder:overlay-show:%p:%d", s, key), func() (*core.Title, error) {
		if local, ok := s.catalog.(interface {
			CachedGet(context.Context, core.RatingKey) (*core.Title, error)
		}); ok {
			return local.CachedGet(ctx, key)
		}
		return s.catalog.Get(ctx, key)
	})
}
func (s *Service) layout(ctx context.Context, u *core.User, k core.RatingKey) (*layout, error) {
	l, _, err := s.layoutWithPref(ctx, u, k)
	return l, err
}
func (s *Service) layoutWithPref(ctx context.Context, u *core.User, k core.RatingKey) (*layout, Preference, error) {
	if k.Allocated() {
		return nil, Preference{Specials: "last"}, nil
	}
	show := showKey(k)
	if show == 0 || show.Allocated() {
		return nil, Preference{}, nil
	}
	if !s.groupsConfigured() {
		p, e := s.Preference(ctx, userID(u), show)
		return nil, p, e
	}
	cacheKey := fmt.Sprintf("%d:%d", userID(u), show)
	s.mu.Lock()
	cached, ok := s.cache[cacheKey]
	generation := s.generation
	s.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		st, err := s.overlayShow(ctx, show)
		if err != nil || st == nil || !allowsViewerOrder(*st) {
			return nil, Preference{}, err
		}
		return cached.value, Preference{}, nil
	}
	p, e := s.Preference(ctx, userID(u), show)
	if e != nil {
		return nil, p, e
	}
	if p.GroupID == "" {
		return nil, p, nil
	}
	st, e := s.overlayShow(ctx, show)
	if e != nil || st == nil || !allowsViewerOrder(*st) {
		return nil, Preference{}, e
	}
	g, e := s.group(ctx, p.GroupID)
	if errors.Is(e, tmdb.ErrNoKey) {
		return nil, p, nil
	}
	if e != nil {
		return nil, p, e
	}
	eps, e := s.catalog.Grandchildren(ctx, show)
	if e != nil {
		return nil, p, e
	}
	out := &layout{positions: map[core.RatingKey]position{}}
	byKey := map[core.RatingKey]core.Title{}
	for _, ep := range eps {
		byKey[ep.Key] = ep
	}
	groups := append([]tmdb.EpisodeSubgroup(nil), g.Groups...)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Order < groups[j].Order })
	addSeason := func(n int, name string, kids []core.Title) {
		if len(kids) == 0 {
			return
		}
		now := time.Now()
		var added time.Time
		for _, ep := range kids {
			if !ep.Released.After(now) && ep.Released.After(added) {
				added = ep.Released
			}
		}
		if added.IsZero() {
			added = st.DateAdded(now)
		}
		out.seasons = append(out.seasons, core.Title{Key: core.SeasonKey(show.TMDB(), n), Type: core.TypeSeason, ParentKey: show, ShowKey: show, ShowIDs: st.IDs, ShowTitle: st.Title, Title: name, SeasonTitle: name, Season: n, ChildCount: len(kids), LeafCount: len(kids), PosterPath: st.PosterPath, BackdropPath: st.BackdropPath, ContentRating: st.ContentRating, AddedAt: added})
		out.episodes = append(out.episodes, kids...)
	}
	for i, group := range groups {
		ordered := append([]tmdb.OrderedEpisode(nil), group.Episodes...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
		var kids []core.Title
		for _, ep := range ordered {
			key := core.EpisodeKey(show.TMDB(), ep.SeasonNumber, ep.EpisodeNumber)
			t, ok := byKey[key]
			if !ok {
				continue
			}
			if _, seen := out.positions[key]; seen {
				continue
			}
			if ep.SeasonNumber == 0 && p.Specials == "hidden" {
				continue
			}
			kids = append(kids, t)
			out.positions[key] = position{virtualBase + i, len(kids), group.Name}
		}
		addSeason(virtualBase+i, group.Name, kids)
	}
	var specials, other []core.Title
	for _, ep := range eps {
		if _, seen := out.positions[ep.Key]; seen {
			continue
		}
		if ep.Season == 0 {
			if p.Specials != "hidden" {
				specials = append(specials, ep)
			}
		} else {
			other = append(other, ep)
		}
	}
	for i, ep := range other {
		out.positions[ep.Key] = position{remainder, i + 1, "Other episodes"}
	}
	addSeason(remainder, "Other episodes", other)
	for _, ep := range specials {
		out.positions[ep.Key] = position{0, ep.Episode, "Specials"}
	}
	if len(specials) > 0 {
		addSeason(0, "Specials", specials)
		if p.Specials == "first" {
			last := out.seasons[len(out.seasons)-1]
			out.seasons = append([]core.Title{last}, out.seasons[:len(out.seasons)-1]...)
			out.episodes = append(specials, out.episodes[:len(out.episodes)-len(specials)]...)
		}
	}
	s.mu.Lock()
	if s.generation == generation {
		if len(s.cache) >= 64 {
			if _, exists := s.cache[cacheKey]; !exists {
				var victim string
				var earliest time.Time
				for ck, cv := range s.cache {
					if victim == "" || cv.expires.Before(earliest) {
						victim, earliest = ck, cv.expires
					}
				}
				if victim != "" {
					delete(s.cache, victim)
				}
			}
		}
		s.cache[cacheKey] = cachedLayout{out, time.Now().Add(time.Minute)}
	}
	s.mu.Unlock()
	return out, p, nil
}
func (s *Service) invalidate() {
	s.mu.Lock()
	s.generation++
	s.cache = map[string]cachedLayout{}
	s.mu.Unlock()
}
func (s *Service) Get(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
	if k.Kind() == core.KindSeason && k.Season() >= virtualBase {
		l, e := s.layout(ctx, u, k)
		if e != nil {
			return nil, e
		}
		if l != nil {
			for _, t := range l.seasons {
				if t.Key == k {
					return &t, nil
				}
			}
		}
		return nil, core.ErrNotFound
	}
	return s.catalog.Get(ctx, k)
}

// CachedVirtualSeason returns only an already-computed, unexpired layout for
// this viewer. It never refreshes preferences, the catalogue, or TMDB.
func (s *Service) CachedVirtualSeason(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
	if u == nil || u.ID <= 0 || k.Kind() != core.KindSeason || k.Season() < virtualBase || !s.groupsConfigured() {
		return nil, core.ErrNotFound
	}
	if local, ok := s.catalog.(interface {
		CachedGet(context.Context, core.RatingKey) (*core.Title, error)
	}); ok {
		st, err := local.CachedGet(ctx, showKey(k))
		if err != nil || st == nil || !allowsViewerOrder(*st) {
			return nil, core.ErrNotFound
		}
	}
	cacheKey := fmt.Sprintf("%d:%d", u.ID, showKey(k))
	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok := s.cache[cacheKey]
	if !ok || cached.value == nil || !time.Now().Before(cached.expires) {
		return nil, core.ErrNotFound
	}
	for _, season := range cached.value.seasons {
		if season.Key == k {
			return &season, nil
		}
	}
	return nil, core.ErrNotFound
}
func (s *Service) Children(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	if k.Kind() != core.KindShow && k.Kind() != core.KindSeason {
		return s.catalog.Children(ctx, k)
	}
	l, pref, e := s.layoutWithPref(ctx, u, k)
	if e != nil || l == nil {
		if e != nil {
			return nil, e
		}
		return s.canonicalChildren(ctx, k, pref)
	}
	if k.Kind() == core.KindShow {
		return l.seasons, nil
	}
	var out []core.Title
	for _, ep := range l.episodes {
		if l.positions[ep.Key].season == k.Season() {
			out = append(out, ep)
		}
	}
	return out, nil
}
func (s *Service) canonicalChildren(ctx context.Context, k core.RatingKey, p Preference) ([]core.Title, error) {
	items, e := s.catalog.Children(ctx, k)
	if e != nil {
		return nil, e
	}
	if k.Allocated() || (len(items) > 0 && !allowsViewerOrder(items[0])) {
		return items, nil
	}
	if k.Kind() == core.KindSeason && k.Season() == 0 && p.Specials == "hidden" {
		return nil, nil
	}
	if k.Kind() == core.KindShow {
		var special, normal []core.Title
		for _, t := range items {
			if t.Season == 0 {
				special = append(special, t)
			} else {
				normal = append(normal, t)
			}
		}
		if p.Specials == "hidden" {
			return normal, nil
		}
		if p.Specials == "first" {
			return append(special, normal...), nil
		}
		return append(normal, special...), nil
	}
	return items, nil
}
func (s *Service) Grandchildren(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	l, pref, e := s.layoutWithPref(ctx, u, k)
	if e != nil {
		return nil, e
	}
	if l != nil {
		return l.episodes, nil
	}
	items, e := s.catalog.Grandchildren(ctx, k)
	if e != nil {
		return nil, e
	}
	return orderUngroupedEpisodes(pref, items), nil
}

// OrderStoredGrandchildren presents an already loaded canonical episode set.
// It lets batched browse reads preserve Specials preferences without loading
// every show's full catalogue metadata again.
func (s *Service) OrderStoredGrandchildren(ctx context.Context, u *core.User, k core.RatingKey, items []core.Title) ([]core.Title, error) {
	l, pref, err := s.layoutWithPref(ctx, u, k)
	if err != nil {
		return nil, err
	}
	if l != nil {
		return l.episodes, nil
	}
	return orderUngroupedEpisodes(pref, items), nil
}

func orderUngroupedEpisodes(p Preference, items []core.Title) []core.Title {
	if len(items) > 0 && !allowsViewerOrder(items[0]) {
		return items
	}
	var special, normal []core.Title
	for _, t := range items {
		if t.Season == 0 {
			special = append(special, t)
		} else {
			normal = append(normal, t)
		}
	}
	if p.Specials == "hidden" {
		return normal
	}
	if p.Specials == "first" {
		return append(special, normal...)
	}
	return append(normal, special...)
}

// Position returns only presentation fields, leaving the catalogue title intact.
func (s *Service) Position(ctx context.Context, u *core.User, k core.RatingKey) (season, episode int, name string, ok bool) {
	if k.Kind() != core.KindEpisode {
		return
	}
	l, e := s.layout(ctx, u, k)
	if e != nil || l == nil {
		return
	}
	p, ok := l.positions[k]
	return p.season, p.episode, p.name, ok
}

// SearchNumbering reads only the independent owner release group.
func (s *Service) SearchNumbering(ctx context.Context, show core.RatingKey, season, episode int, cachedOnly bool) (int, int, error) {
	if show.Allocated() || !s.groupsConfigured() {
		return 0, 0, nil
	}
	p, err := s.ReleasePreference(ctx, show)
	if err != nil || p.Mode != "group" {
		return 0, 0, err
	}
	var g *tmdb.EpisodeGroup
	if cachedOnly {
		var body string
		if err := s.db.QueryRowContext(ctx, `SELECT body FROM episodeorder_groups WHERE id=?`, p.GroupID).Scan(&body); err != nil {
			return 0, 0, nil
		}
		g = &tmdb.EpisodeGroup{}
		if json.Unmarshal([]byte(body), g) != nil {
			return 0, 0, nil
		}
	} else {
		g, err = s.group(ctx, p.GroupID)
	}
	if err != nil {
		return 0, 0, err
	}
	seasonOffset := tmdb.GroupSeasonOffset(g)
	var found bool
	var sn, en int
	for _, group := range g.Groups {
		for _, ep := range group.Episodes {
			if ep.SeasonNumber == season && ep.EpisodeNumber == episode {
				if found {
					return 0, 0, nil
				} // ambiguous membership
				found = true
				sn, en = group.Order+seasonOffset, ep.Order+1
			}
		}
	}
	return sn, en, nil
}

// Generation changes when episode layout preferences are invalidated.
func (s *Service) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}
