// Package watchlist ingests authorized viewers' watchlists and lazily checks
// their release availability.
package watchlist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/schedule"
	"github.com/braedonsaunders/ether/internal/store"
)

const Redacted = "••••"

func init() {
	store.Register("watchlist", 1, `
CREATE TABLE watchlist_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);
CREATE TABLE watchlist_items(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,source TEXT NOT NULL,title TEXT NOT NULL,added INTEGER NOT NULL,checked INTEGER NOT NULL DEFAULT 0,available INTEGER NOT NULL DEFAULT 0,notified INTEGER NOT NULL DEFAULT 0,warmed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,item_key,source));
CREATE INDEX watchlist_viewer ON watchlist_items(user_id,available,added);
`)
	// Cluster-wide record of the last scheduled pass, so a new leader
	// continues the schedule rather than re-running at once.
	store.Register("watchlist", 2, `CREATE TABLE watchlist_meta(k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');`)
	store.Register("watchlist", 4, `CREATE TABLE watchlist_community_jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,user_id INTEGER NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL,started_at INTEGER NOT NULL DEFAULT 0,finished_at INTEGER NOT NULL DEFAULT 0,count INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',plex TEXT NOT NULL DEFAULT ''); CREATE UNIQUE INDEX watchlist_community_active ON watchlist_community_jobs(user_id) WHERE state IN ('queued','running'); CREATE INDEX watchlist_community_history ON watchlist_community_jobs(user_id,id);`)
	store.Register("watchlist", 5, `CREATE TABLE watchlist_community_locks(user_id TEXT PRIMARY KEY,touched INTEGER NOT NULL);`)

}

type Source interface {
	Watchlist(context.Context, int64, int) ([]core.Title, error)
}
type Notifier interface {
	TitleAvailable(context.Context, string) error
}

// Account-source names in settings and feed encryption contexts are persisted identities.
type Settings struct {
	PlexFriendsStatus    []FriendFeedStatus `json:"plexFriendsStatus,omitempty"`
	Enabled              bool               `json:"enabled"`
	Plex                 bool               `json:"plex"`
	PlexFriendsRSSURL    string             `json:"plexFriendsRssUrl"`
	PlexOwnerRSSURL      string             `json:"plexOwnerRssUrl"`
	PlexFriendsRSSStatus *RSSFeedStatus     `json:"plexFriendsRssStatus,omitempty"`
	PlexOwnerRSSStatus   *RSSFeedStatus     `json:"plexOwnerRssStatus,omitempty"`
	Trakt                bool               `json:"trakt"`
	Notify               bool               `json:"notify"`
	Prewarm              bool               `json:"prewarm"`
	CheckHours           int                `json:"checkHours"`
	NaturalSearch        bool               `json:"naturalSearch"`
	Recaps               bool               `json:"recaps"`
	SubtitleTranslate    bool               `json:"subtitleTranslate"`
	SpeechToText         bool               `json:"speechToText"`
	OverviewTranslate    bool               `json:"overviewTranslate"`
}
type Options struct {
	Community        *plextv.CommunityClient
	OwnerToken       func(context.Context) (string, error)
	PlexLookup       func(context.Context, string) (core.RatingKey, error)
	Logger           *slog.Logger
	Sealer           config.FieldSealer
	Catalog          core.Catalog
	Resolver         core.Resolver
	Engine           core.Engine
	Trakt            Source
	Tokens           func(context.Context, int64) (string, error)
	Users            func(context.Context) ([]core.User, error)
	Allowed          func(*core.User, *core.Title) bool
	Notifier         Notifier
	Client           *http.Client
	PlexBaseURL      string
	ClientIdentifier string
	Now              func() time.Time
	PrewarmBytes     int64
	// Cache records why an arrival was warmed when it supports provenance.
	Cache core.CacheProvenanceRecorder
	// SyncInterval is how often Run passes over every viewer (30m).
	SyncInterval time.Duration
	// RSSInterval is how often Run polls configured Plex watchlist RSS feeds (10s).
	RSSInterval time.Duration
	// ResumeSchedule makes Run wait until SyncInterval has passed since the
	// last pass recorded in the database (by any node) before its first
	// pass, instead of passing at once. A cluster sets it because Run
	// restarts on every leader handoff.
	ResumeSchedule bool
	// OnAvailable runs when a watchlist title first becomes playable.
	// The key is the movie, show, or newly released episode.
	OnAvailable func(context.Context, int64, core.RatingKey)
	// OnFriendsRSSChanged runs when the household "Friends' watchlists" set changes.
	OnFriendsRSSChanged func(context.Context)
}
type Service struct {
	db              *sql.DB
	o               Options
	mu              sync.Mutex
	rssMu           sync.Mutex
	communityMu     sync.Mutex
	rssCheckMu      sync.Mutex
	rssCheckPending map[int64]core.User
	nextCheck       time.Time
	gen             atomic.Uint64
}

// Generation changes when watchlist membership changes.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

func (s *Service) queueRSSCheck(u core.User) {
	s.rssCheckMu.Lock()
	if s.rssCheckPending == nil {
		s.rssCheckPending = make(map[int64]core.User)
	}
	s.rssCheckPending[u.ID] = u
	s.rssCheckMu.Unlock()
}

func (s *Service) drainRSSChecks() []core.User {
	s.rssCheckMu.Lock()
	defer s.rssCheckMu.Unlock()
	if len(s.rssCheckPending) == 0 {
		return nil
	}
	out := make([]core.User, 0, len(s.rssCheckPending))
	for _, u := range s.rssCheckPending {
		out = append(out, u)
	}
	clear(s.rssCheckPending)
	return out
}

type Item struct {
	Title       core.Title `json:"title"`
	Sources     []string   `json:"sources"`
	AddedAt     time.Time  `json:"addedAt"`
	CheckedAt   time.Time  `json:"checkedAt,omitempty"`
	AvailableAt time.Time  `json:"availableAt,omitempty"`
}

func New(db *sql.DB, o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Client == nil {
		o.Client = egress.Client(egress.CapMetadata, 20*time.Second)
		o.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	if o.Community == nil {
		o.Community = &plextv.CommunityClient{Client: o.Client}
	}
	if o.PlexBaseURL == "" {
		o.PlexBaseURL = "https://discover.provider.plex.tv"
	}
	if o.ClientIdentifier == "" {
		o.ClientIdentifier = plexguid.ClientIdentifier("", "")
	}
	if o.PrewarmBytes <= 0 {
		o.PrewarmBytes = 8 << 20
	}
	if o.SyncInterval <= 0 {
		o.SyncInterval = 30 * time.Minute
	}
	if o.RSSInterval <= 0 {
		o.RSSInterval = 10 * time.Second
	}
	return &Service{db: db, o: o}
}

func (s *Service) memoKey() string { return fmt.Sprintf("watchlist:settings:%p", s) }

func friendsRSSContext() []string { return []string{"watchlist_settings", "plexFriendsRssUrl", "1"} }
func ownerRSSContext() []string   { return []string{"watchlist_settings", "plexOwnerRssUrl", "1"} }

func (s *Service) settings(ctx context.Context) (Settings, error) {
	v := Settings{Plex: true, Trakt: true, CheckHours: 6}
	// Every item of a list asks: read the row once per request.
	row, err := reqmemo.Do(ctx, s.memoKey(), func() (*string, error) {
		var raw string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM watchlist_settings WHERE id=1`).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &raw, err
	})
	if err != nil {
		return v, err
	}
	if row == nil {
		return v, nil
	}
	if err = json.Unmarshal([]byte(*row), &v); err != nil {
		return v, err
	}
	if v.PlexFriendsRSSURL, _, err = config.OpenWith(s.o.Sealer, v.PlexFriendsRSSURL, friendsRSSContext()...); err != nil {
		return v, fmt.Errorf("watchlist: stored friends RSS URL: %w", err)
	}
	if v.PlexOwnerRSSURL, _, err = config.OpenWith(s.o.Sealer, v.PlexOwnerRSSURL, ownerRSSContext()...); err != nil {
		return v, fmt.Errorf("watchlist: stored owner RSS URL: %w", err)
	}
	v.PlexFriendsStatus = nil
	v.PlexFriendsRSSStatus = nil
	v.PlexOwnerRSSStatus = nil
	return v, nil
}

func (s *Service) encodeSettings(v Settings) (string, error) {
	var err error
	v.PlexFriendsStatus = nil
	v.PlexFriendsRSSStatus = nil
	v.PlexOwnerRSSStatus = nil
	if v.PlexFriendsRSSURL, err = config.SealWith(s.o.Sealer, v.PlexFriendsRSSURL, friendsRSSContext()...); err != nil {
		return "", fmt.Errorf("watchlist: seal friends RSS URL: %w", err)
	}
	if v.PlexOwnerRSSURL, err = config.SealWith(s.o.Sealer, v.PlexOwnerRSSURL, ownerRSSContext()...); err != nil {
		return "", fmt.Errorf("watchlist: seal owner RSS URL: %w", err)
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

// MigrateSecrets seals RSS URLs stored in plaintext; it returns 1 when it
// rewrote the settings row. Unreadable rows are left untouched.
func (s *Service) MigrateSecrets(ctx context.Context) (int, error) {
	if s == nil || s.db == nil || s.o.Sealer == nil {
		return 0, nil
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM watchlist_settings WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v Settings
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return 0, err
	}
	friendsPlain, friendsLegacy, err := config.OpenWith(s.o.Sealer, v.PlexFriendsRSSURL, friendsRSSContext()...)
	if err != nil {
		return 0, fmt.Errorf("watchlist: stored friends RSS URL left unchanged: %w", err)
	}
	ownerPlain, ownerLegacy, err := config.OpenWith(s.o.Sealer, v.PlexOwnerRSSURL, ownerRSSContext()...)
	if err != nil {
		return 0, fmt.Errorf("watchlist: stored owner RSS URL left unchanged: %w", err)
	}
	if !friendsLegacy && !ownerLegacy {
		return 0, nil
	}
	v.PlexFriendsRSSURL = friendsPlain
	v.PlexOwnerRSSURL = ownerPlain
	next, err := s.encodeSettings(v)
	if err != nil || next == raw {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE watchlist_settings SET value=? WHERE id=1 AND value=?`, next, raw)
	if err != nil {
		return 0, err
	}
	reqmemo.Forget(ctx, s.memoKey())
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	v, err := s.settings(ctx)
	if err != nil {
		return v, err
	}
	v.PlexFriendsStatus = s.friendStatuses(ctx)
	v.PlexFriendsRSSStatus = s.feedStatus(ctx, v.PlexFriendsRSSURL, metaKeyFriendsRSS)
	v.PlexOwnerRSSStatus = s.feedStatus(ctx, v.PlexOwnerRSSURL, metaKeyOwnerRSS)
	if v.PlexFriendsRSSURL != "" {
		v.PlexFriendsRSSURL = Redacted
	}
	if v.PlexOwnerRSSURL != "" {
		v.PlexOwnerRSSURL = Redacted
	}
	return v, nil
}

func (s *Service) SaveSettings(ctx context.Context, v Settings) error {
	reqmemo.Forget(ctx, s.memoKey())
	if v.CheckHours < 1 || v.CheckHours > 168 {
		return errors.New("check interval must be between 1 and 168 hours")
	}
	old, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if v.PlexFriendsRSSURL == Redacted {
		if old.PlexFriendsRSSURL == "" {
			return errors.New("enter the Friends' Watchlist RSS URL")
		}
		v.PlexFriendsRSSURL = old.PlexFriendsRSSURL
	} else {
		v.PlexFriendsRSSURL = strings.TrimSpace(v.PlexFriendsRSSURL)
		if err := validateRSSURL(v.PlexFriendsRSSURL); err != nil {
			return err
		}
	}
	if v.PlexOwnerRSSURL == Redacted {
		if old.PlexOwnerRSSURL == "" {
			return errors.New("enter Your Watchlist RSS URL")
		}
		v.PlexOwnerRSSURL = old.PlexOwnerRSSURL
	} else {
		v.PlexOwnerRSSURL = strings.TrimSpace(v.PlexOwnerRSSURL)
		if err := validateRSSURL(v.PlexOwnerRSSURL); err != nil {
			return err
		}
	}
	raw, err := s.encodeSettings(v)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO watchlist_settings(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, raw); err != nil {
		return err
	}
	reqmemo.Forget(ctx, s.memoKey())
	if v.PlexFriendsRSSURL != old.PlexFriendsRSSURL {
		if v.PlexFriendsRSSURL == "" {
			if err := s.clearFeedSource(ctx, sourcePlexFriendsRSS, metaKeyFriendsRSS); err != nil {
				return err
			}
		} else {
			s.resetFeedMeta(ctx, metaKeyFriendsRSS)
		}
	}
	if v.PlexOwnerRSSURL != old.PlexOwnerRSSURL {
		if v.PlexOwnerRSSURL == "" {
			if err := s.clearFeedSource(ctx, sourcePlexOwnerRSS, metaKeyOwnerRSS); err != nil {
				return err
			}
		} else {
			s.resetFeedMeta(ctx, metaKeyOwnerRSS)
		}
	}
	return nil
}

func (s *Service) allowed(u *core.User, t *core.Title) bool {
	if u == nil || t == nil || t.Adult || contentrating.Exceeds(t.ContentRating, u.MaxContentRating) {
		return false
	}
	section := core.LibraryMovies
	if t.Type != core.TypeMovie {
		section = core.LibraryShows
	}
	if len(u.AllowedLibraries) > 0 {
		ok := false
		for _, id := range u.AllowedLibraries {
			ok = ok || id == section
		}
		if !ok {
			return false
		}
	}
	return s.o.Allowed == nil || s.o.Allowed(u, t)
}

// allowedForSet applies viewer rules when u != nil, and household safety
// (non-adult) when u == nil (the shared Friends' watchlists set).
func (s *Service) allowedForSet(u *core.User, t *core.Title) bool {
	if u != nil {
		return s.allowed(u, t)
	}
	return t != nil && !t.Adult
}

func (s *Service) Remind(ctx context.Context, u *core.User, key core.RatingKey) error {
	t, err := s.o.Catalog.Get(ctx, key)
	if err != nil {
		return err
	}
	if !s.allowed(u, t) {
		return core.ErrNotFound
	}
	return s.upsert(ctx, u.ID, "manual", *t)
}

// Contains reports membership from any watchlist source for one viewer.
func (s *Service) Contains(ctx context.Context, userID int64, key core.RatingKey) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM watchlist_items WHERE user_id=? AND item_key=?)`, userID, key).Scan(&present)
	return present, err
}

// ContainsMany reads membership for a page in one query per chunk.
func (s *Service) ContainsMany(ctx context.Context, userID int64, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	for start := 0; start < len(keys); start += 400 {
		chunk := keys[start:min(start+400, len(keys))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		query := `SELECT item_key FROM watchlist_items WHERE user_id=? AND item_key IN (`
		for i, key := range chunk {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, key)
		}
		query += ")"
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key int64
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.RatingKey(key)] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
func (s *Service) Remove(ctx context.Context, userID int64, key core.RatingKey) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_items WHERE user_id=? AND item_key=?`, userID, key); err != nil {
		return err
	}
	// The show's handled episodes go with it (see forgetEpisodes).
	if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_episodes WHERE user_id=? AND item_key=?`, userID, key); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.gen.Add(1)
	return nil
}
func (s *Service) upsert(ctx context.Context, uid int64, source string, t core.Title) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(?,?,?,?,?) ON CONFLICT(user_id,item_key,source) DO UPDATE SET title=excluded.title`, uid, t.Key, source, string(raw), s.o.Now().Unix())
	if err == nil {
		s.gen.Add(1)
	}
	return err
}
func (s *Service) replace(ctx context.Context, u *core.User, source string, ts []core.Title) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keys := map[core.RatingKey]bool{}
	for _, t := range ts {
		if !s.allowed(u, &t) {
			continue
		}
		keys[t.Key] = true
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(?,?,?,?,?) ON CONFLICT(user_id,item_key,source) DO UPDATE SET title=excluded.title`, u.ID, t.Key, source, string(raw), s.o.Now().Unix()); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT item_key FROM watchlist_items WHERE user_id=? AND source=?`, u.ID, source)
	if err != nil {
		return err
	}
	var removed []core.RatingKey
	for rows.Next() {
		var k core.RatingKey
		if err = rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		if !keys[k] {
			removed = append(removed, k)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, k := range removed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM watchlist_items WHERE user_id=? AND source=? AND item_key=?`, u.ID, source, k); err != nil {
			return err
		}
		if err = forgetEpisodes(ctx, tx, u.ID, k); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.gen.Add(1)
	return nil
}

// SyncResult describes the Plex outcome of an explicit viewer sync.
// SyncResult names external account sources, independent of the requesting frontend.
type SyncResult struct {
	Plex  string `json:"plex"`
	Count int    `json:"count"`
}

// Sync preserves the last successful snapshot when an upstream fails.
func (s *Service) Sync(ctx context.Context, u *core.User) error {
	return s.syncViewer(ctx, u, &SyncResult{})
}
func (s *Service) syncViewer(ctx context.Context, u *core.User, result *SyncResult) error {
	cfg, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if u == nil {
		return core.ErrUnauthorized
	}
	var errs []error
	result.Plex = "disabled"
	if !cfg.Enabled {
		return nil
	}
	if cfg.Plex && s.o.Tokens != nil {
		token, e := s.o.Tokens(ctx, u.ID)
		switch {
		case errors.Is(e, core.ErrUnauthorized):
			result.Plex = "synced"
			count, syncErr := 0, error(nil)
			if ctx.Value(communityScheduledKey{}) != true {
				count, syncErr = s.syncCommunityViewer(ctx, u)
			}
			result.Count = count
			if syncErr != nil {
				result.Plex = "refused"
				errs = append(errs, syncErr)
			}
		case errors.Is(e, plextv.ErrHomePINRequired):
			result.Plex = "pin"
			errs = append(errs, e)
		case e != nil:
			result.Plex = "refused"
			errs = append(errs, e)
		case token != "":
			result.Plex = "synced"
			ts, e := s.plex(ctx, token)
			if e == nil {
				// External account source identity, independent of the requesting frontend.
				e = s.replace(ctx, u, "plex", ts)
				if e == nil {
					keys := map[core.RatingKey]bool{}
					for _, title := range ts {
						if s.allowed(u, &title) {
							keys[title.Key] = true
						}
					}
					result.Count = len(keys)
				}
			}
			if e != nil {
				result.Plex = "refused"
			}
			errs = append(errs, e)
		default:
			result.Plex = "refused"
		}
	}
	if cfg.Trakt && s.o.Trakt != nil {
		ts, e := s.o.Trakt.Watchlist(ctx, u.ID, 1000)
		if e == nil {
			e = s.replace(ctx, u, "trakt", ts)
		}
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}

func (s *Service) Items(ctx context.Context, u *core.User) ([]Item, error) {
	if u == nil {
		return []Item{}, core.ErrUnauthorized
	}
	return s.itemsForUser(ctx, u.ID, u)
}

// HouseholdItems returns released titles in the shared "Friends' watchlists"
// set (user_id=0) for availability monitoring and inspection.
func (s *Service) HouseholdItems(ctx context.Context) ([]Item, error) {
	return s.itemsForUser(ctx, householdUserID, nil)
}

func (s *Service) itemsForUser(ctx context.Context, userID int64, policyUser *core.User) ([]Item, error) {
	out := []Item{}
	rows, err := s.db.QueryContext(ctx, `SELECT title,source,added,checked,available FROM watchlist_items WHERE user_id=? ORDER BY added DESC,item_key`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[core.RatingKey]int{}
	for rows.Next() {
		var raw, source string
		var added, checked, available int64
		if err := rows.Scan(&raw, &source, &added, &checked, &available); err != nil {
			return nil, err
		}
		var t core.Title
		if json.Unmarshal([]byte(raw), &t) != nil || !t.IsReleased(s.o.Now()) || !s.allowedForSet(policyUser, &t) {
			continue
		}
		if i, ok := byKey[t.Key]; ok {
			out[i].Sources = append(out[i].Sources, source)
			if checked > 0 {
				if ct := time.Unix(checked, 0); out[i].CheckedAt.IsZero() || ct.After(out[i].CheckedAt) {
					out[i].CheckedAt = ct
				}
			}
			if available > 0 {
				if at := time.Unix(available, 0); out[i].AvailableAt.IsZero() || at.Before(out[i].AvailableAt) {
					out[i].AvailableAt = at
				}
			}
			continue
		}
		it := Item{Title: t, Sources: []string{source}, AddedAt: time.Unix(added, 0)}
		if checked > 0 {
			it.CheckedAt = time.Unix(checked, 0)
		}
		if available > 0 {
			it.AvailableAt = time.Unix(available, 0)
		}
		byKey[t.Key] = len(out)
		out = append(out, it)
	}
	return out, rows.Err()
}

// Check resolves only explicitly watched-for, released titles, never the whole
// catalogue. A healthy resolver result is required before marking available.
//
// A show (or season) becomes available through its first released episode.
// After that, each newer regular episode is resolved once it airs and, when
// a healthy release exists, announced and prewarmed once (see newEpisode).
func (s *Service) Check(ctx context.Context, u *core.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled || s.o.Resolver == nil {
		return err
	}
	items, err := s.Items(ctx, u)
	if err != nil {
		return err
	}
	return s.checkSet(ctx, cfg, u.ID, u, u, items)
}

// CheckHousehold checks availability and prefetches releases for the shared
// "Friends' watchlists" set within the same per-pass 20-title budget.
func (s *Service) CheckHousehold(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled || s.o.Resolver == nil {
		return err
	}
	items, err := s.HouseholdItems(ctx)
	if err != nil {
		return err
	}
	return s.checkSet(ctx, cfg, householdUserID, nil, s.ownerOrNil(ctx), items)
}

func orderChecks(items []Item) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].CheckedAt.Before(items[j].CheckedAt) })
}

func (s *Service) checkSet(ctx context.Context, cfg Settings, userID int64, policyUser, resolveUser *core.User, items []Item) error {
	orderChecks(items)
	if cfg.Notify {
		if err := s.retryAlerts(ctx); err != nil {
			return err
		}
	}
	checked := 0
	for _, it := range items {
		if checked >= 20 || ctx.Err() != nil {
			break
		}
		t := it.Title
		if fresh, e := s.o.Catalog.Get(ctx, t.Key); e == nil && fresh != nil {
			t = *fresh
		}
		if !s.allowedForSet(policyUser, &t) || !t.IsReleased(s.o.Now()) {
			continue
		}
		resolveTitle := &t
		var first, newest *core.Title
		isShow := t.Type == core.TypeShow || t.Type == core.TypeSeason
		if isShow {
			var e error
			if first, newest, e = s.episodeRange(ctx, policyUser, &t); e != nil || first == nil {
				continue
			}
			resolveTitle = first
		}
		pending := false // a newer episode of an available show awaits a release
		if isShow && !it.AvailableAt.IsZero() && newest != nil {
			known, e := s.episodeKnown(ctx, userID, t.Key, newest.Key)
			if e != nil {
				return e
			}
			pending = !known
		}
		interval := time.Duration(cfg.CheckHours) * time.Hour
		if pending && s.o.Now().Sub(newest.Released) < freshAiring {
			interval = min(interval, freshAiringCheck)
		}
		// A check before this episode aired says nothing about its release.
		newlyAired := pending && it.CheckedAt.Before(newest.Released)
		if !newlyAired && !it.CheckedAt.IsZero() && s.o.Now().Sub(it.CheckedAt) < interval {
			continue
		}
		// Share the scheduled pass budget with owner jobs; concurrency stays one.
		timer := time.NewTimer(max(time.Until(s.nextCheck), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		timer.Stop()
		s.nextCheck = time.Now().Add(3 * time.Second)
		checked++
		if pending {
			if err := s.newEpisodeForSet(ctx, cfg, userID, resolveUser, &t, newest); err != nil {
				return err
			}
			continue
		}
		wasAvailable := !it.AvailableAt.IsZero()
		res, e := s.resolve(ctx, resolveUser, resolveTitle)
		now := s.o.Now().Unix()
		// Transient resolver failures do not establish that a stream is absent.
		if e != nil && !errors.Is(e, core.ErrNotAvailable) {
			if cfg.CheckHours == 0 {
				return e
			}
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET checked=? WHERE user_id=? AND item_key=?`, now, userID, t.Key); err != nil {
			return err
		}
		if errors.Is(e, core.ErrNotAvailable) || (e == nil && res == nil) {
			if _, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET available=0,notified=0,warmed=0 WHERE user_id=? AND item_key=?`, userID, t.Key); err != nil {
				return err
			}
		}
		if e != nil || res == nil {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET available=CASE WHEN available=0 THEN ? ELSE available END,title=? WHERE user_id=? AND item_key=?`, now, mustTitle(t), userID, t.Key); err != nil {
			return err
		}
		if !wasAvailable && s.o.OnAvailable != nil {
			s.o.OnAvailable(ctx, userID, t.Key)
		}
		if newest != nil {
			// The show's episodes so far count as seen: only later ones are new.
			if err := s.recordEpisode(ctx, userID, t.Key, newest.Key); err != nil {
				return err
			}
		}
		var notified, warmed int
		if err := s.db.QueryRowContext(ctx, `SELECT MAX(notified),MAX(warmed) FROM watchlist_items WHERE user_id=? AND item_key=?`, userID, t.Key).Scan(&notified, &warmed); err != nil {
			return err
		}
		if cfg.Notify && notified == 0 && s.o.Notifier != nil {
			if e := s.o.Notifier.TitleAvailable(ctx, t.Title); e == nil {
				_, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET notified=1 WHERE user_id=? AND item_key=?`, userID, t.Key)
				if err != nil {
					return err
				}
			}
		}
		if cfg.Prewarm && warmed == 0 && s.o.Engine != nil {
			if s.prewarm(ctx, res) == nil {
				_, err := s.db.ExecContext(ctx, `UPDATE watchlist_items SET warmed=1 WHERE user_id=? AND item_key=?`, userID, t.Key)
				if err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

// resolve asks for a healthy release of title for u. It returns a nil
// Resolution, with a nil error, when nothing playable was found.
func (s *Service) resolve(ctx context.Context, u *core.User, title *core.Title) (*core.Resolution, error) {
	caps := core.ClientCaps{MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}, Containers: []string{"mkv", "mp4"}}
	job, cancel := context.WithTimeout(core.WithIndexerPurpose(ctx, core.IndexerPurposeWarmup), 45*time.Second)
	defer cancel()
	// Nobody waits on a watchlist check: fallback queries go one at a time.
	res, err := s.o.Resolver.Resolve(core.WithUnattendedSearch(job), core.ResolveRequest{Title: title, User: u, Client: caps})
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.NZB) == 0 || res.Release.ID == "" {
		return nil, nil
	}
	return res, nil
}

func (s *Service) prewarm(ctx context.Context, res *core.Resolution) error {
	job, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if s.o.Cache != nil {
		s.o.Cache.RecordProvenance(core.CacheProvenance{ReleaseID: res.Release.ID, Title: res.Release.Name, Source: res.Release.Name, Cause: core.CacheCauseWatchlist})
	}
	return s.o.Engine.Prefetch(job, res.Release.ID, res.NZB, 0, s.o.PrewarmBytes)
}

func mustTitle(t core.Title) string { raw, _ := json.Marshal(t); return string(raw) }
func (s *Service) Hubs(ctx context.Context, u *core.User, limit int) ([]core.Hub, error) {
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	items, err := s.Items(ctx, u)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 12
	}
	soon, ready := core.Hub{ID: "watchlist.coming", Title: "Waiting for a stream", Context: "hub.home.watchlist", Promoted: true}, core.Hub{ID: "watchlist.available", Title: "Now available", Context: "hub.home.watchlist.available", Promoted: true}
	for _, it := range items {
		if !it.AvailableAt.IsZero() {
			if len(ready.Items) < limit {
				ready.Items = append(ready.Items, it.Title)
			}
		} else if len(soon.Items) < limit {
			soon.Items = append(soon.Items, it.Title)
		}
	}
	var out []core.Hub
	if len(ready.Items) > 0 {
		out = append(out, ready)
	}
	if len(soon.Items) > 0 {
		out = append(out, soon)
	}
	return out, nil
}

// Run is cancelled by the app; individual viewers are isolated and bounded.
// Run passes over every viewer (watchlist sync and availability check) plus
// the shared household Friends' watchlists set at once and then every
// SyncInterval; with ResumeSchedule the first pass waits out the interval
// since the last recorded pass.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	if s.o.RSSInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if !schedule.Wait(ctx, s.o.Now, s.o.RSSInterval) {
					return
				}
				feedJob, feedCancel := context.WithTimeout(ctx, time.Minute)
				err := s.SyncRSS(feedJob)
				feedCancel()
				if err != nil {
					s.o.Logger.Warn("watchlist rss sync failed", "err", err)
					if !schedule.Wait(ctx, s.o.Now, time.Minute) {
						return
					}
				} else if users := s.drainRSSChecks(); len(users) > 0 && s.o.Resolver != nil {
					wg.Add(1)
					go func(users []core.User) {
						defer wg.Done()
						for i := range users {
							if ctx.Err() != nil {
								return
							}
							checkJob, checkCancel := context.WithTimeout(ctx, 2*time.Minute)
							if err := s.Check(checkJob, &users[i]); err != nil {
								s.o.Logger.Warn("watchlist rss availability check failed", "user", users[i].ID, "err", err)
							}
							checkCancel()
						}
					}(users)
				}
			}
		}()
	}
	defer wg.Wait()

	if !schedule.Wait(ctx, s.o.Now, s.untilDue(ctx)) {
		return
	}

	run := func() {
		defer s.recordPass(ctx)
		var us []core.User
		if s.o.Users != nil {
			var err error
			us, err = s.o.Users(ctx)
			if err != nil {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		feedJob, feedCancel := context.WithTimeout(ctx, 3*time.Minute)
		if err := s.syncRSSUsers(feedJob, us); err != nil {
			s.o.Logger.Warn("watchlist rss sync failed", "err", err)
		}
		feedCancel()
		communityJob, communityCancel := context.WithTimeout(ctx, 15*time.Minute)
		if err := s.syncCommunityFriends(communityJob, us, nil); err != nil {
			s.o.Logger.Warn("watchlist friends sync failed", "err", err)
		}
		communityCancel()
		for i := range us {
			if ctx.Err() != nil {
				return
			}
			job, cancel := context.WithTimeout(ctx, 3*time.Minute)
			if err := s.syncViewer(context.WithValue(job, communityScheduledKey{}, true), &us[i], &SyncResult{}); err != nil {
				s.o.Logger.Warn("watchlist sync failed", "user", us[i].ID, "err", err)
			}
			if err := s.Check(job, &us[i]); err != nil {
				s.o.Logger.Warn("watchlist check failed", "user", us[i].ID, "err", err)
			}
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		houseJob, houseCancel := context.WithTimeout(ctx, 3*time.Minute)
		if err := s.CheckHousehold(houseJob); err != nil {
			s.o.Logger.Warn("watchlist household check failed", "err", err)
		}
		houseCancel()
	}
	for ctx.Err() == nil {
		run()
		if !schedule.Wait(ctx, s.o.Now, s.o.SyncInterval) {
			return
		}
	}
}

// untilDue is how long Run waits before its first pass.
func (s *Service) untilDue(ctx context.Context) time.Duration {
	if !s.o.ResumeSchedule {
		return 0
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT v FROM watchlist_meta WHERE k='last_pass'`).Scan(&v); err != nil {
		return 0
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return min(s.o.SyncInterval, max(time.UnixMilli(ms).Add(s.o.SyncInterval).Sub(s.o.Now().Round(0)), 0))
}

// recordPass notes a finished (or attempted) pass for ResumeSchedule.
func (s *Service) recordPass(ctx context.Context) {
	if ctx.Err() != nil {
		return // interrupted (leadership lost): let the next leader redo it
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO watchlist_meta(k,v) VALUES('last_pass',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		strconv.FormatInt(s.o.Now().UnixMilli(), 10)); err != nil {
		s.o.Logger.Warn("watchlist: record pass failed", "err", err)
	}
}

func (s *Service) ownerOrNil(ctx context.Context) *core.User {
	if s.o.Users == nil {
		return nil
	}
	users, err := s.o.Users(ctx)
	if err != nil {
		return nil
	}
	for i := range users {
		if users[i].IsOwner {
			return &users[i]
		}
	}
	return nil
}

func (s *Service) user(ctx context.Context, id int64) (*core.User, error) {
	if s.o.Users == nil {
		return nil, core.ErrNotFound
	}
	users, err := s.o.Users(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].ID == id {
			return &users[i], nil
		}
	}
	return nil, fmt.Errorf("viewer: %w", core.ErrNotFound)
}
