// Package migrate performs read-only PMS discovery and reviewed, idempotent local imports.
package migrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	store.Register("migrate", 1, `CREATE TABLE migrate_plans(id TEXT PRIMARY KEY, document TEXT NOT NULL, created INTEGER NOT NULL, imported INTEGER NOT NULL DEFAULT 0); CREATE TABLE migrate_objects(source TEXT NOT NULL, viewer INTEGER NOT NULL, kind TEXT NOT NULL, external TEXT NOT NULL, local_id INTEGER NOT NULL, PRIMARY KEY(source,viewer,kind,external)); CREATE TABLE migrate_watchlist(viewer INTEGER NOT NULL,key INTEGER NOT NULL,PRIMARY KEY(viewer,key));`)
}

type Service struct {
	db      *sql.DB
	catalog core.Catalog
	client  *http.Client
	state   *state.State
	users   interface {
		User(context.Context, int64) (*core.User, error)
	}
	logger *slog.Logger
	jobsMu sync.Mutex
	jobs   map[string]*previewJob
}

type Options struct {
	State *state.State
	Users interface {
		User(context.Context, int64) (*core.User, error)
	}
	Logger *slog.Logger
}

func (s *Service) viewer(ctx context.Context, id int64) (*core.User, error) {
	if s.users != nil {
		return s.users.User(ctx, id)
	}
	return auth.StoredIdentity(ctx, s.db, id)
}

func New(db *sql.DB, catalog core.Catalog, opts ...Options) *Service {
	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.State == nil {
		o.State = state.New(db, state.Options{})
	}
	return &Service{db: db, catalog: catalog, state: o.State, users: o.Users, logger: o.Logger, client: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

type Viewer struct {
	LocalUserID     int64  `json:"localUserId"`
	PlexID          int64  `json:"plexId"`
	Name            string `json:"name"`
	Token           string `json:"token,omitempty"`
	SourceAccountID int64  `json:"sourceAccountId,omitempty"`
}
type Request struct {
	URL         string   `json:"url"`
	Token       string   `json:"token"`
	Viewers     []Viewer `json:"viewers"`
	HistoryOnly bool     `json:"historyOnly,omitempty"`
}
type Item struct {
	External  string         `json:"external"`
	Key       core.RatingKey `json:"key"`
	Title     string         `json:"title"`
	Viewer    int64          `json:"viewer"`
	Views     int            `json:"views"`
	Offset    int64          `json:"offsetMs"`
	Duration  int64          `json:"durationMs"`
	Rating    float64        `json:"rating"`
	ViewedAt  int64          `json:"viewedAt"`
	Watchlist bool           `json:"watchlist,omitempty"`
}
type List struct {
	External string           `json:"external"`
	Name     string           `json:"name"`
	Kind     string           `json:"kind"`
	Viewer   int64            `json:"viewer"`
	Keys     []core.RatingKey `json:"keys"`
}
type Plan struct {
	ID       string   `json:"id"`
	Source   string   `json:"source"`
	Items    []Item   `json:"items"`
	History  []Item   `json:"history"`
	Lists    []List   `json:"lists"`
	Viewers  []Viewer `json:"viewers"`
	Warnings []string `json:"warnings"`
	Imported bool     `json:"imported"`
}
type media struct {
	Key   string `xml:"ratingKey,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr"`
	GUID  string `xml:"guid,attr"`
	GUIDs []struct {
		ID string `xml:"id,attr"`
	} `xml:"Guid"`
	Views       int     `xml:"viewCount,attr"`
	Offset      int64   `xml:"viewOffset,attr"`
	Duration    int64   `xml:"duration,attr"`
	Rating      float64 `xml:"userRating,attr"`
	ViewedAt    int64   `xml:"lastViewedAt,attr"`
	HistoryAt   int64   `xml:"viewedAt,attr"`
	AccountID   int64   `xml:"accountID,attr"`
	Grandparent string  `xml:"grandparentRatingKey,attr"`
	Season      int     `xml:"parentIndex,attr"`
	Episode     int     `xml:"index,attr"`
}
type container struct {
	Accounts []struct {
		ID   int64  `xml:"id,attr"`
		Name string `xml:"name,attr"`
	} `xml:"Account"`
	Machine     string `xml:"machineIdentifier,attr"`
	Total       int    `xml:"totalSize,attr"`
	Size        int    `xml:"size,attr"`
	Directories []struct {
		Key       string `xml:"key,attr"`
		RatingKey string `xml:"ratingKey,attr"`
		GUIDs     []struct {
			ID string `xml:"id,attr"`
		} `xml:"Guid"`
		Type  string `xml:"type,attr"`
		Title string `xml:"title,attr"`
		GUID  string `xml:"guid,attr"`
	} `xml:"Directory"`
	Videos    []media `xml:"Video"`
	Playlists []media `xml:"Playlist"`
	Metadata  []media `xml:"Metadata"`
}

func (s *Service) get(ctx context.Context, base, token, path string) (container, error) {
	load := func() (container, error) { return s.getOnce(ctx, base, token, path) }
	if m := memoFrom(ctx); m != nil {
		// The key stays in memory only. It is never logged.
		return m.httpGet(token+"\n"+path, load)
	}
	return load()
}

func pmsErr(err error, fallback string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New(fallback)
}

func (s *Service) getOnce(ctx context.Context, base, token, path string) (container, error) {
	var c container
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if e != nil {
		return c, e
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/xml")
	// PMS accepts these pagination controls as headers. Some versions silently
	// ignore the identically named query parameters and return the whole library.
	for _, name := range []string{"X-Plex-Container-Start", "X-Plex-Container-Size"} {
		if value := req.URL.Query().Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	resp, e := s.client.Do(req)
	if e != nil {
		return c, pmsErr(e, "PMS connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return c, fmt.Errorf("PMS returned HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if e != nil {
		return c, pmsErr(e, "PMS response read failed")
	}
	if len(b) > 32<<20 {
		return c, errors.New("PMS response too large")
	}
	e = xml.Unmarshal(b, &c)
	return c, e
}
func (s *Service) mapMedia(ctx context.Context, base, token string, m media) (core.RatingKey, error) {
	guids := []string{m.GUID}
	for _, g := range m.GUIDs {
		guids = append(guids, g.ID)
	}
	for _, g := range guids {
		for _, prefix := range []string{"tmdb://", "com.plexapp.agents.themoviedb://", "imdb://", "com.plexapp.agents.imdb://", "tvdb://", "com.plexapp.agents.thetvdb://"} {
			if !strings.HasPrefix(g, prefix) {
				continue
			}
			id := strings.Split(strings.TrimPrefix(g, prefix), "?")[0]
			if strings.Contains(prefix, "tmdb") || strings.Contains(prefix, "themoviedb") {
				n, e := strconv.ParseInt(id, 10, 64)
				if e != nil || n <= 0 {
					continue
				}
				var k core.RatingKey
				if m.Type == "movie" {
					k = core.MovieKey(n)
				} else if m.Type == "show" {
					k = core.ShowKey(n)
				}
				if k != 0 {
					if s.catalog != nil {
						if _, e = s.catalogGet(ctx, k); e != nil {
							continue
						}
					}
					return k, nil
				}
			} else if s.catalog != nil {
				source := "imdb"
				if strings.Contains(prefix, "tvdb") {
					source = "tvdb"
				}
				t, e := s.externalTitle(ctx, source, id)
				if e == nil && t != nil && string(t.Type) == m.Type {
					return t.Key, nil
				}
			}
		}
	}
	if m.Type == "episode" && m.Grandparent != "" && base != "" {
		c, e := s.get(ctx, base, token, "/library/metadata/"+url.PathEscape(m.Grandparent))
		if e == nil {
			for _, d := range c.Directories {
				show := media{Type: "show", GUID: d.GUID, GUIDs: d.GUIDs}
				k, e := s.mapMedia(ctx, base, token, show)
				if e == nil {
					return core.EpisodeKey(k.TMDB(), m.Season, m.Episode), nil
				}
			}
		}
	}
	return 0, core.ErrNotFound
}
func (s *Service) pages(ctx context.Context, base, token, path string) ([]media, error) {
	out := []media{}
	for start := 0; start < 1000000; {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		c, e := s.get(ctx, base, token, path+sep+"includeGuids=1&X-Plex-Container-Start="+strconv.Itoa(start)+"&X-Plex-Container-Size=500")
		if e != nil {
			return nil, e
		}
		items := append(c.Videos, c.Metadata...)
		items = append(items, c.Playlists...)
		for _, d := range c.Directories {
			key := d.RatingKey
			if key == "" {
				key = d.Key
			}
			items = append(items, media{Key: key, Title: d.Title, Type: d.Type, GUID: d.GUID, GUIDs: d.GUIDs})
		}
		out = append(out, items...)
		addProgress(ctx, func(pr *PreviewProgress) { pr.Pages++ })
		if len(items) == 0 || c.Total == 0 || start+len(items) >= c.Total {
			return out, nil
		}
		start += len(items)
	}
	return nil, errors.New("PMS paging limit exceeded")
}
func (s *Service) Preview(ctx context.Context, q Request) (Plan, error) {
	ctx = withPreviewState(ctx)
	addProgress(ctx, func(pr *PreviewProgress) { pr.Phase = "connecting" })
	p := Plan{Items: []Item{}, Lists: []List{}, Warnings: []string{}, Viewers: []Viewer{}}
	if e := validatePreviewRequest(q); e != nil {
		return p, e
	}
	u, _ := url.Parse(q.URL)
	base := strings.TrimRight(u.String(), "/")
	ident, e := s.get(ctx, base, q.Token, "/identity")
	if e != nil {
		return p, e
	}
	if ident.Machine == "" {
		return p, errors.New("PMS identity missing machine identifier")
	}
	p.Source = ident.Machine
	sections, e := s.get(ctx, base, q.Token, "/library/sections")
	if e != nil {
		return p, e
	}
	for _, v := range q.Viewers {
		if v.LocalUserID <= 0 {
			return p, errors.New("viewer localUserId required")
		}
		if _, err := s.viewer(ctx, v.LocalUserID); err != nil {
			return p, errors.New("viewer does not exist locally; sync Plex sharing first")
		}
		token := v.Token
		if token == "" {
			if len(q.Viewers) > 1 {
				return p, errors.New("each viewer needs their own PMS access token")
			}
			token = q.Token
		}
		v.Token = ""
		// Account discovery requires the owner token. Use it for explicitly
		// account-scoped detailed history too; PMS permissions vary by version.
		// Personal library state must still use the selected viewer's token.
		accountID, accountErr := s.sourceAccount(ctx, base, q.Token, v)
		if accountErr != nil && q.HistoryOnly {
			return p, accountErr
		}
		v.SourceAccountID = accountID
		p.Viewers = append(p.Viewers, v)
		keyMap := map[string]core.RatingKey{}
		mapper := sourceMapper{service: s, base: base, token: token, keys: keyMap, parents: map[string]core.RatingKey{}, missing: map[string]bool{}}
		addProgress(ctx, func(pr *PreviewProgress) { pr.Phase = "library" })
		for _, section := range sections.Directories {
			if section.Type != "movie" && section.Type != "show" {
				continue
			}
			typ := "1"
			if section.Type == "show" {
				typ = "4"
			}
			sectionPath := "/library/sections/" + url.PathEscape(section.Key) + "/all?type=" + typ
			if q.HistoryOnly {
				sectionPath += "&unwatched=0"
			}
			ms, e := s.pages(ctx, base, token, sectionPath)
			if e != nil {
				return p, e
			}
			mapped := make([]mappedRow, len(ms))
			if e = s.eachMedia(ctx, ms, func(i int, m media) {
				addProgress(ctx, func(pr *PreviewProgress) { pr.Scanned++ })
				if q.HistoryOnly && m.Views == 0 {
					return
				}
				k, me := mapper.resolve(ctx, m, false)
				if me != nil {
					if errors.Is(me, context.Canceled) {
						return
					}
					mapped[i].warn = "Unmatched: " + m.Title
					addProgress(ctx, func(pr *PreviewProgress) { pr.Unmatched++ })
					return
				}
				addProgress(ctx, func(pr *PreviewProgress) { pr.Matched++ })
				if m.Views > 0 || m.Offset > 0 || m.Rating > 0 {
					mapped[i].keep = true
					mapped[i].item = Item{External: m.Key, Key: k, Title: m.Title, Viewer: v.LocalUserID, Views: m.Views, Offset: m.Offset, Duration: m.Duration, Rating: m.Rating, ViewedAt: m.ViewedAt}
				}
			}); e != nil {
				return p, e
			}
			for _, row := range mapped {
				if row.warn != "" {
					p.Warnings = append(p.Warnings, row.warn)
				}
				if row.keep {
					p.Items = append(p.Items, row.item)
				}
			}
			s.logProgress(ctx, "plex preview progress")
			if q.HistoryOnly {
				continue
			}
			cs, ce := s.pages(ctx, base, token, "/library/sections/"+url.PathEscape(section.Key)+"/collections")
			if ce != nil {
				p.Warnings = append(p.Warnings, "Collections unavailable for section "+section.Title)
			} else {
				for _, c := range cs {
					list, e := s.readList(ctx, base, token, c, "collection", v.LocalUserID, keyMap)
					if e != nil {
						p.Warnings = append(p.Warnings, "Collection unavailable: "+c.Title)
					} else {
						p.Lists = append(p.Lists, list)
					}
				}
			}
		}
		var history []media
		he := accountErr
		if he == nil {
			addProgress(ctx, func(pr *PreviewProgress) { pr.Phase = "history" })
			history, he = s.pages(ctx, base, q.Token, "/status/sessions/history/all?sort=viewedAt:asc&accountID="+strconv.FormatInt(accountID, 10))
		}
		if he != nil {
			p.Warnings = append(p.Warnings, "Detailed watch history unavailable for "+v.Name+"; last-viewed summaries will be imported")
		} else {
			mapped := make([]mappedRow, len(history))
			if e = s.eachMedia(ctx, history, func(i int, h media) {
				addProgress(ctx, func(pr *PreviewProgress) { pr.Scanned++ })
				// Defend against a server ignoring the account filter.
				if h.AccountID != accountID {
					return
				}
				k := mapper.cached(h.Key)
				var me error
				if k == 0 {
					k, me = mapper.resolve(ctx, h, false)
				}
				if errors.Is(me, context.Canceled) {
					return
				}
				at := h.HistoryAt
				if at == 0 {
					at = h.ViewedAt
				}
				if k == 0 {
					mapped[i].warn = "Unmatched history: " + h.Title
					addProgress(ctx, func(pr *PreviewProgress) { pr.Unmatched++ })
				}
				if k != 0 && at > 0 {
					mapped[i].keep = true
					mapped[i].item = Item{Key: k, Viewer: v.LocalUserID, ViewedAt: at, Views: 1, Title: h.Title}
					addProgress(ctx, func(pr *PreviewProgress) { pr.Matched++ })
				}
			}); e != nil {
				return p, e
			}
			for _, row := range mapped {
				if row.warn != "" {
					p.Warnings = append(p.Warnings, row.warn)
				}
				if row.keep {
					p.History = append(p.History, row.item)
				}
			}
			s.logProgress(ctx, "plex preview progress")
		}
		if q.HistoryOnly {
			continue
		}
		pls, pe := s.pages(ctx, base, token, "/playlists")
		if pe != nil {
			p.Warnings = append(p.Warnings, "Playlists unavailable for "+v.Name)
		} else {
			for _, pl := range pls {
				l, e := s.readList(ctx, base, token, pl, "playlist", v.LocalUserID, keyMap)
				if e != nil {
					p.Warnings = append(p.Warnings, "Playlist unavailable: "+pl.Title)
				} else {
					p.Lists = append(p.Lists, l)
				}
			}
		}
	}
	p.Warnings = append(p.Warnings, "PMS exposes state only for supplied viewer access tokens. Existing Ether users are mapped; sharing must be granted through plex.tv.")
	addProgress(ctx, func(pr *PreviewProgress) { pr.Phase = "saving" })
	return s.savePlan(ctx, p)
}
func (s *Service) readList(ctx context.Context, base, token string, m media, kind string, viewer int64, keys map[string]core.RatingKey) (List, error) {
	path := "/library/collections/" + url.PathEscape(m.Key) + "/children"
	if kind == "playlist" {
		path = "/playlists/" + url.PathEscape(m.Key) + "/items"
	}
	items, e := s.pages(ctx, base, token, path)
	l := List{External: m.Key, Name: m.Title, Kind: kind, Viewer: viewer, Keys: []core.RatingKey{}}
	if e != nil {
		return l, e
	}
	for _, item := range items {
		k := keys[item.Key]
		if k == 0 {
			k, _ = s.mapMedia(ctx, base, token, item)
		}
		if k != 0 {
			l.Keys = append(l.Keys, k)
		}
	}
	return l, nil
}
func (s *Service) savePlan(ctx context.Context, p Plan) (Plan, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return p, e
	}
	p.ID = hex.EncodeToString(b)
	doc, e := json.Marshal(p)
	if e != nil {
		return p, e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO migrate_plans(id,document,created) VALUES(?,?,?)`, p.ID, string(doc), time.Now().Unix())
	return p, e
}
func (s *Service) Import(ctx context.Context, id string) (Plan, error) {
	var p Plan
	var document string
	var imported bool
	if err := s.db.QueryRowContext(ctx, `SELECT document,imported FROM migrate_plans WHERE id=?`, id).Scan(&document, &imported); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(document), &p); err != nil {
		return p, err
	}
	if imported {
		p.Imported = true
		return p, nil
	}
	// Resolve snapshots before taking the write transaction: Catalog.Get may
	// use the same database connection or fetch remote metadata.
	titles, err := s.watchlistTitles(ctx, p.Items, strings.HasPrefix(p.Source, "csv:") && strings.HasSuffix(p.Source, ":watchlist"))
	if err != nil {
		return p, err
	}
	err = s.state.ImportTransaction(ctx, func(tx *sql.Tx) (state.ImportBatch, error) {
		var batch state.ImportBatch
		// A State's write mutex belongs to one node. Serialize the import
		// transaction in the shared database too: both the plan's imported
		// flag and source-to-list mappings use read-then-write decisions.
		if err := store.LockKey(ctx, store.DialectOf(s.db), tx, "migrate:import"); err != nil {
			return batch, err
		}
		var doc string
		var imported bool
		if err := tx.QueryRowContext(ctx, `SELECT document,imported FROM migrate_plans WHERE id=?`, id).Scan(&doc, &imported); err != nil {
			return batch, err
		}
		if err := json.Unmarshal([]byte(doc), &p); err != nil {
			return batch, err
		}
		if imported {
			p.Imported = true
			return batch, nil
		}
		watchlistOnly := strings.HasPrefix(p.Source, "csv:") && strings.HasSuffix(p.Source, ":watchlist")
		ratingsOnly := strings.HasPrefix(p.Source, "csv:") && strings.HasSuffix(p.Source, ":ratings")
		for _, it := range p.Items {
			if watchlistOnly || it.Watchlist {
				if err := watchlist.ImportTitleTx(ctx, tx, it.Viewer, titles[it.Key], time.Now().Unix()); err != nil {
					return batch, err
				}
			}
			if !watchlistOnly {
				r := stateImportRecord(it)
				r.RatingsOnly = ratingsOnly
				batch.Records = append(batch.Records, r)
			}
		}
		for _, it := range p.History {
			batch.History = append(batch.History, stateImportRecord(it))
		}
		for _, l := range p.Lists {
			if err := importList(ctx, tx, p.Source, l, time.Now().UnixNano()); err != nil {
				return batch, err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE migrate_plans SET imported=1 WHERE id=?`, id)
		return batch, err
	})
	p.Imported = err == nil
	return p, err
}

func stateImportRecord(it Item) state.ImportRecord {
	r := state.ImportRecord{UserID: it.Viewer, Key: it.Key, Views: it.Views, Offset: time.Duration(it.Offset) * time.Millisecond, Duration: time.Duration(it.Duration) * time.Millisecond, Rating: it.Rating}
	if it.ViewedAt > 0 {
		r.ViewedAt = time.Unix(it.ViewedAt, 0)
	}
	return r
}

func importList(ctx context.Context, tx *sql.Tx, source string, l List, now int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT local_id FROM migrate_objects WHERE source=? AND viewer=? AND kind=? AND external=?`, source, l.Viewer, l.Kind, l.External).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch l.Kind {
	case "playlist":
		id, err = state.ImportPlaylistTx(ctx, tx, id, l.Viewer, l.Name, l.Keys, now)
	case "collection":
		id, err = collections.ImportTitlesTx(ctx, tx, id, l.Viewer, l.Name, l.Keys, now)
	default:
		return errors.New("invalid imported list kind")
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migrate_objects(source,viewer,kind,external,local_id) VALUES(?,?,?,?,?) ON CONFLICT(source,viewer,kind,external) DO UPDATE SET local_id=excluded.local_id`, source, l.Viewer, l.Kind, l.External, id)
	return err
}
func fingerprint(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
