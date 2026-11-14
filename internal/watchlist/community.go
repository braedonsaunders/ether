package watchlist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type communityScheduledKey struct{}
type FriendFeedStatus struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	RSSFeedStatus
}

func (s *Service) friendStatuses(ctx context.Context) []FriendFeedStatus {
	rows, err := s.db.QueryContext(ctx, `SELECT v FROM watchlist_meta WHERE k LIKE 'community:%' ORDER BY k`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []FriendFeedStatus
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) == nil {
			var st FriendFeedStatus
			if json.Unmarshal([]byte(raw), &st) == nil {
				out = append(out, st)
			}
		}
	}
	return out
}
func (s *Service) recordFriend(ctx context.Context, st FriendFeedStatus) {
	raw, _ := json.Marshal(st)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO watchlist_meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, "community:"+st.ID, string(raw)); err != nil {
		s.o.Logger.Warn("watchlist friend status failed", "err", err)
	}
}
func matchCommunityFriend(f plextv.CommunityFriend, us []core.User) *core.User {
	for i := range us {
		if us[i].PlexID > 0 && strconv.FormatInt(us[i].PlexID, 10) == f.ID {
			return &us[i]
		}
	}
	for i := range us {
		if f.Username != "" && strings.EqualFold(us[i].Username, f.Username) {
			return &us[i]
		}
	}
	return nil
}
func (s *Service) syncCommunityViewer(ctx context.Context, u *core.User) (int, error) {
	if err := s.syncCommunityFriends(ctx, []core.User{*u}, u); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT item_key) FROM watchlist_items WHERE user_id=? AND source LIKE 'plex-community:%'`, u.ID).Scan(&count)
	return count, err
}
func (s *Service) syncCommunityFriends(ctx context.Context, us []core.User, only *core.User) error {
	s.communityMu.Lock()
	defer s.communityMu.Unlock()
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled || !cfg.Plex {
		return err
	}
	if s.o.OwnerToken == nil {
		if only != nil {
			return errors.New("Plex owner authorization unavailable")
		}
		return nil
	}
	token, err := s.o.OwnerToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("Plex owner authorization unavailable")
	}
	friends, err := s.o.Community.Friends(ctx, token)
	if err != nil {
		s.recordFriend(ctx, FriendFeedStatus{ID: "friends", RSSFeedStatus: RSSFeedStatus{Configured: true, Problem: "upstream", Error: err.Error()}})
		return err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM watchlist_meta WHERE k='community:friends'`)
	previous := map[string]FriendFeedStatus{}
	for _, st := range s.friendStatuses(ctx) {
		previous[st.ID] = st
	}
	var errs []error
	if only == nil {
		active := map[string]bool{}
		for _, friend := range friends {
			active[friend.ID] = true
		}
		for _, old := range s.friendStatuses(ctx) {
			if old.ID != "friends" && !active[old.ID] {
				if e := s.clearFeedSource(ctx, "plex-community:"+old.ID, "community:"+old.ID); e != nil {
					errs = append(errs, e)
				}
			}
		}
	}
	found := false
	for _, f := range friends {
		u := matchCommunityFriend(f, us)
		if only != nil && (u == nil || u.ID != only.ID) {
			continue
		}
		found = true
		st := previous[f.ID]
		st.ID, st.Username, st.Configured = f.ID, f.Username, true
		st.Problem, st.Error = "", ""
		titles, unmatched, e := s.communityTitles(ctx, token, f.ID)
		source := "plex-community:" + f.ID

		if e == nil {
			// The source lease also covers household attribution, across nodes.
			// Re-read settings and viewer identity after the upstream fetch.
			lockID := "friend:" + f.ID
			e = s.withCommunityWriterLock(ctx, lockID, func() error {
				current, err := s.settings(ctx)
				if err != nil {
					return err
				}
				if !current.Enabled || !current.Plex {
					return errors.New("Plex watchlists are disabled")
				}
				if u == nil {
					st.Imported, err = s.replaceHouseholdFeedSnapshot(ctx, source, titles)
				} else {
					latest, lookupErr := s.user(ctx, u.ID)
					if lookupErr != nil {
						return lookupErr
					}
					if latest.PlexID != u.PlexID || latest.UUID != u.UUID {
						return errors.New("Viewer identity changed")
					}
					st.Imported, err = s.replaceOwnerFeedSnapshot(ctx, source, latest, titles)
				}
				return err
			})
		}

		st.UnmatchedTitles = unmatched
		if e != nil {
			st.Problem = "upstream"
			st.Error = e.Error()
			errs = append(errs, fmt.Errorf("friend %s: %w", f.Username, e))
		} else {
			st.LastRead = s.o.Now().Unix()
		}
		s.recordFriend(ctx, st)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if only != nil && !found {
		return errors.New("Viewer is not a visible Plex friend of the owner")
	}
	s.notifyFriendsRSSChanged(ctx)
	return errors.Join(errs...)
}

func (s *Service) preserveActiveFriendsRSSItems(ctx context.Context, tx *sql.Tx, userID int64, source string, titles []core.Title) ([]core.Title, error) {
	have := make(map[core.RatingKey]bool, len(titles))
	for _, t := range titles {
		have[t.Key] = true
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT wi.title
		FROM watchlist_items wi
		JOIN watchlist_items rss ON rss.user_id = 0 AND rss.source = ? AND rss.item_key = wi.item_key
		JOIN watchlist_rss_authors a ON a.item_key = wi.item_key AND a.friend_id = ?
		WHERE wi.user_id = ? AND wi.source = ?`, sourcePlexFriendsRSS, strings.TrimPrefix(source, "plex-community:"), userID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var t core.Title
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, err
		}
		if t.Key != 0 && !have[t.Key] {
			have[t.Key] = true
			titles = append(titles, t)
		}
	}
	return titles, rows.Err()
}

func (s *Service) communityTitles(ctx context.Context, token, id string) ([]core.Title, int, error) {
	var out []core.Title
	unmatched := 0
	cursor := ""
	seen := map[string]bool{}
	for total := 0; total < 500; {
		page, err := s.o.Community.WatchlistPage(ctx, token, id, cursor, min(100, 500-total))
		if err != nil {
			return nil, 0, err
		}
		if len(page.Nodes) > min(100, 500-total) {
			return nil, 0, errors.New("Plex community page exceeds requested limit")
		}
		for _, item := range page.Nodes {
			if item.ID == "" {
				return nil, 0, errors.New("Plex community item identity missing")
			}
			itemType := strings.ToLower(strings.TrimSpace(item.Type))
			if itemType != "movie" && itemType != "show" {
				unmatched++
				continue
			}
			var title *core.Title
			if strings.HasPrefix(item.ID, "plex://") || !strings.Contains(item.ID, "://") {
				guid := item.ID
				if !strings.HasPrefix(guid, "plex://") {
					guid = "plex://" + itemType + "/" + guid
				}
				if s.o.PlexLookup == nil {
					return nil, 0, errors.New("Plex identity lookup unavailable")
				}
				if e := s.o.Community.Wait(ctx); e != nil {
					return nil, 0, e
				}
				key, e := s.o.PlexLookup(ctx, guid)
				if e != nil && !errors.Is(e, core.ErrNotFound) {
					if strings.Contains(e.Error(), "ambiguous Plex identity") {
						unmatched++
						continue
					}
					return nil, 0, e
				}
				if e == nil {
					title, err = s.o.Catalog.Get(ctx, key)
				}
			} else {
				title, err = s.resolveRSSItem(ctx, rssItem{Guids: []string{item.ID}, Categories: []rssCategory{{Value: itemType}}})
			}
			if err != nil && !errors.Is(err, core.ErrNotFound) {
				return nil, 0, err
			}
			if title == nil {
				unmatched++
			} else {
				out = append(out, *title)
			}
		}
		total += len(page.Nodes)
		if !page.PageInfo.HasNextPage || total == 500 {
			return out, unmatched, nil
		}
		next := page.PageInfo.EndCursor
		if len(page.Nodes) == 0 || next == "" || next == cursor || seen[next] {
			return nil, 0, errors.New("Plex community pagination did not advance")
		}
		seen[next] = true
		cursor = next
	}
	return out, unmatched, nil
}
