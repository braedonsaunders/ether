package watchlist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

const (
	// householdUserID stores the unattributed "Friends' watchlists" set as
	// watchlist-owned rows without associating them with any individual viewer.
	householdUserID int64 = 0

	sourcePlexFriendsRSS = "plex-friends-rss"
	sourcePlexOwnerRSS   = "plex-rss"

	metaKeyFriendsRSS = "rss_friends"
	metaKeyOwnerRSS   = "rss_owner"

	rssProblemAuth        = "auth"
	rssProblemRateLimited = "rate_limited"
	rssProblemInvalidFeed = "invalid_feed"
	rssProblemMetadata    = "metadata"
	rssProblemNetwork     = "network"
)

// RSSFeedStatus reports the latest poll state of a configured Plex watchlist RSS feed.
type RSSFeedStatus struct {
	Configured      bool   `json:"configured"`
	LastRead        int64  `json:"lastRead"`
	Imported        int    `json:"imported"`
	UnmatchedTitles int    `json:"unmatchedTitles"`
	Problem         string `json:"problem,omitempty"`
	Error           string `json:"error,omitempty"`
}

type rssMeta struct {
	URLHash         string `json:"urlHash,omitempty"`
	OwnerID         int64  `json:"ownerId,omitempty"`
	ETag            string `json:"etag,omitempty"`
	LastModified    string `json:"lastModified,omitempty"`
	LastRead        int64  `json:"lastRead,omitempty"`
	Imported        int    `json:"imported"`
	UnmatchedTitles int    `json:"unmatchedTitles"`
	Problem         string `json:"problem,omitempty"`
	Error           string `json:"error,omitempty"`
}

type rssFeedXML struct {
	XMLName xml.Name     `xml:""`
	Channel rssChannel   `xml:"channel"`
	Items   []rssXMLItem `xml:"item"`
	Entries []rssAtom    `xml:"entry"`
}

type rssChannel struct {
	Items []rssXMLItem `xml:"item"`
}

type rssXMLItem struct {
	Author     string        `xml:"author"`
	Title      string        `xml:"title"`
	Guids      []rssGUID     `xml:"guid"`
	Links      []rssLink     `xml:"link"`
	Categories []rssCategory `xml:"category"`
}

type rssAtom struct {
	Title      string        `xml:"title"`
	ID         string        `xml:"id"`
	Links      []rssLink     `xml:"link"`
	Categories []rssCategory `xml:"category"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink string `xml:"isPermaLink,attr"`
}

type rssLink struct {
	Value string `xml:",chardata"`
	Href  string `xml:"href,attr"`
	Rel   string `xml:"rel,attr"`
}

type rssCategory struct {
	Value string `xml:",chardata"`
	Term  string `xml:"term,attr"`
}

type rssItem struct {
	Author     string
	Title      string
	Guids      []string
	Links      []string
	Categories []rssCategory
}

type traktWriter interface {
	SetWatchlist(ctx context.Context, userID int64, title core.Title, add bool) (bool, error)
}

type typedExternalCatalog interface {
	ByExternalIDForType(ctx context.Context, source, id string, typ core.MediaType) (*core.Title, error)
}

func validateRSSURL(raw string) error {
	if raw == "" {
		return nil
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return errors.New("RSS URL must be a valid http or https URL")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("RSS URL must be a valid http or https URL")
	}
	return nil
}

func rssHTTPClient(base *http.Client) *http.Client {
	var c http.Client
	if base != nil {
		c = *base
	} else {
		c.Timeout = 20 * time.Second
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("unsupported redirect scheme")
		}
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing https to http redirect")
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[len(via)-1].URL.Host) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("X-Plex-Token")
		}
		return nil
	}
	return &c
}

// SyncRSS polls configured Plex watchlist RSS feeds: the household "Friends'
// watchlists" set (unattributed) and the owner's "Your Watchlist" feed.
func (s *Service) SyncRSS(ctx context.Context) error {
	cfg, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || (cfg.PlexFriendsRSSURL == "" && cfg.PlexOwnerRSSURL == "") {
		return nil
	}
	var us []core.User
	if s.o.Users != nil {
		us, err = s.o.Users(ctx)
		if err != nil {
			return err
		}
	}
	return s.syncRSSUsers(ctx, us)
}

func (s *Service) syncRSSUsers(ctx context.Context, us []core.User) error {
	s.rssMu.Lock()
	defer s.rssMu.Unlock()
	cfg, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || (cfg.PlexFriendsRSSURL == "" && cfg.PlexOwnerRSSURL == "") {
		return nil
	}
	var errs []error
	if cfg.PlexFriendsRSSURL != "" {
		if e := s.pollRSSFeed(ctx, cfg.PlexFriendsRSSURL, metaKeyFriendsRSS, sourcePlexFriendsRSS, true, nil, us); e != nil {
			errs = append(errs, e)
		}
	}
	if cfg.PlexOwnerRSSURL != "" {
		var owner *core.User
		for i := range us {
			if us[i].IsOwner {
				owner = &us[i]
				break
			}
		}
		if e := s.pollRSSFeed(ctx, cfg.PlexOwnerRSSURL, metaKeyOwnerRSS, sourcePlexOwnerRSS, false, owner, us); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) pollRSSFeed(ctx context.Context, feedURL, metaKey, source string, friendsFeed bool, owner *core.User, us []core.User) error {
	if s.o.Catalog == nil {
		return errors.New("Plex watchlist RSS requires a catalog")
	}
	urlHash := sha256Hex(feedURL)
	meta := s.readRSSMeta(ctx, metaKey)
	if meta.URLHash != urlHash {
		meta = rssMeta{URLHash: urlHash}
	}
	var ownerID int64
	if owner != nil {
		ownerID = owner.ID
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		meta.Problem = rssProblemInvalidFeed
		meta.Error = "Invalid feed URL"
		s.writeRSSMeta(ctx, metaKey, meta)
		return errors.New("Plex watchlist RSS URL invalid")
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9, application/json;q=0.5, */*;q=0.1")
	req.Header.Set("User-Agent", "Ether")
	canConditional := friendsFeed || (ownerID > 0 && meta.OwnerID == ownerID)
	if canConditional {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	resp, err := rssHTTPClient(s.o.Client).Do(req)
	if err != nil {
		// Never return or log *url.Error raw: it embeds the secret feed URL.
		meta.Problem = rssProblemNetwork
		s.writeRSSMeta(ctx, metaKey, meta)
		return errors.New("Plex watchlist RSS request failed")
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	resp.Body.Close()
	if readErr != nil || len(data) > 4<<20 {
		meta.Problem = rssProblemInvalidFeed
		meta.Error = "Response unreadable or larger than 4 MiB"
		s.writeRSSMeta(ctx, metaKey, meta)
		return errors.New("Plex watchlist RSS response unreadable or too large")
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		meta.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		meta.LastRead = s.o.Now().Unix()
		meta.Problem = ""
		meta.Error = ""
		if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
			meta.ETag = etag
		}
		if lm := strings.TrimSpace(resp.Header.Get("Last-Modified")); lm != "" {
			meta.LastModified = lm
		}
		meta.OwnerID = ownerID
		s.writeRSSMeta(ctx, metaKey, meta)
		return nil
	case http.StatusOK:
		// Handled below.
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		meta.Problem = rssProblemAuth
		s.writeRSSMeta(ctx, metaKey, meta)
		return fmt.Errorf("Plex watchlist RSS: HTTP %d", resp.StatusCode)
	case http.StatusTooManyRequests:
		meta.Problem = rssProblemRateLimited
		s.writeRSSMeta(ctx, metaKey, meta)
		return errors.New("Plex watchlist RSS rate limited")
	default:
		meta.Problem = rssProblemNetwork
		s.writeRSSMeta(ctx, metaKey, meta)
		return fmt.Errorf("Plex watchlist RSS: HTTP %d", resp.StatusCode)
	}

	items, err := parseRSSItems(data)
	if err != nil {
		meta.Problem = rssProblemInvalidFeed
		meta.Error = err.Error()
		s.writeRSSMeta(ctx, metaKey, meta)
		return err
	}

	var resolved []core.Title
	byAuthor := map[string][]core.Title{}
	unmatchedTitles := 0
	for _, it := range items {
		title, e := s.resolveRSSItem(ctx, it)
		if e != nil {
			// Preserve the last good snapshot and conditional headers on a transient
			// catalogue/TMDB outage so the next pass re-fetches and retries cleanly.
			meta.Problem = rssProblemMetadata
			s.writeRSSMeta(ctx, metaKey, meta)
			return fmt.Errorf("Plex watchlist RSS metadata: %w", e)
		}
		if title == nil {
			unmatchedTitles++
			continue
		}
		resolved = append(resolved, *title)
		if friendsFeed && it.Author != "" {
			byAuthor[it.Author] = append(byAuthor[it.Author], *title)
		}
	}

	var imported int
	if friendsFeed {
		imported, err = s.replaceHouseholdFeedSnapshot(ctx, source, resolved)
		if err == nil && len(byAuthor) > 0 && len(us) > 0 {
			s.attributeFriendsRSSItems(ctx, byAuthor, us)
		}
	} else {
		imported, err = s.replaceOwnerFeedSnapshot(ctx, source, owner, resolved)
	}
	if err != nil {
		meta.Problem = rssProblemMetadata
		s.writeRSSMeta(ctx, metaKey, meta)
		return err
	}
	if friendsFeed {
		s.notifyFriendsRSSChanged(ctx)
	}

	meta.URLHash = urlHash
	meta.OwnerID = ownerID
	meta.ETag = strings.TrimSpace(resp.Header.Get("ETag"))
	meta.LastModified = strings.TrimSpace(resp.Header.Get("Last-Modified"))
	meta.LastRead = s.o.Now().Unix()
	meta.Imported = imported
	meta.UnmatchedTitles = unmatchedTitles
	meta.Problem = ""
	meta.Error = ""
	s.writeRSSMeta(ctx, metaKey, meta)
	return nil
}

func parseRSSItems(data []byte) ([]rssItem, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex watchlist RSS response empty")
	}
	if trimmed[0] == '{' {
		return parsePlexRSSJSON(trimmed)
	}
	var doc rssFeedXML
	if err := xml.Unmarshal(trimmed, &doc); err != nil {
		return nil, fmt.Errorf("Plex watchlist RSS XML: %w", err)
	}
	local := strings.ToLower(doc.XMLName.Local)
	if local != "rss" && local != "rdf" && local != "feed" {
		return nil, errors.New("Plex watchlist RSS response invalid")
	}
	xmlItems := doc.Channel.Items
	if len(xmlItems) == 0 && len(doc.Items) > 0 {
		xmlItems = doc.Items
	}
	if len(xmlItems)+len(doc.Entries) > 2000 {
		return nil, errors.New("Plex watchlist RSS exceeds 2000-item safety limit; previous snapshot preserved")
	}
	out := make([]rssItem, 0, len(xmlItems)+len(doc.Entries))
	for _, x := range xmlItems {
		it := rssItem{
			Author:     strings.TrimSpace(x.Author),
			Title:      strings.TrimSpace(x.Title),
			Categories: x.Categories,
		}
		for _, g := range x.Guids {
			if v := strings.TrimSpace(g.Value); v != "" {
				it.Guids = append(it.Guids, v)
			}
		}
		for _, l := range x.Links {
			if v := strings.TrimSpace(l.Value); v != "" {
				it.Links = append(it.Links, v)
			} else if h := strings.TrimSpace(l.Href); h != "" {
				it.Links = append(it.Links, h)
			}
		}
		out = append(out, it)
	}
	for _, e := range doc.Entries {
		it := rssItem{
			Title:      strings.TrimSpace(e.Title),
			Categories: e.Categories,
		}
		if id := strings.TrimSpace(e.ID); id != "" {
			it.Guids = append(it.Guids, id)
		}
		for _, l := range e.Links {
			if h := strings.TrimSpace(l.Href); h != "" {
				it.Links = append(it.Links, h)
			} else if v := strings.TrimSpace(l.Value); v != "" {
				it.Links = append(it.Links, v)
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func parsePlexRSSJSON(data []byte) ([]rssItem, error) {
	var envelope struct {
		Items []struct {
			Title    string   `json:"title"`
			Link     string   `json:"link"`
			Guids    []string `json:"guids"`
			Category string   `json:"category"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Items == nil {
		return nil, errors.New("Plex watchlist RSS response invalid")
	}
	if len(envelope.Items) > 2000 {
		return nil, errors.New("Plex watchlist RSS exceeds 2000-item safety limit; previous snapshot preserved")
	}
	out := make([]rssItem, 0, len(envelope.Items))
	for _, x := range envelope.Items {
		it := rssItem{
			Title: strings.TrimSpace(x.Title),
			Guids: x.Guids,
		}
		if l := strings.TrimSpace(x.Link); l != "" {
			it.Links = append(it.Links, l)
		}
		if cat := strings.TrimSpace(x.Category); cat != "" {
			it.Categories = append(it.Categories, rssCategory{Value: cat})
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *Service) attributeFriendsRSSItems(ctx context.Context, byAuthor map[string][]core.Title, us []core.User) {
	statuses := map[string]FriendFeedStatus{}
	for _, st := range s.friendStatuses(ctx) {
		statuses[st.ID] = st
	}
	for authorID, titles := range byAuthor {
		st, ok := statuses[authorID]
		if !ok {
			st = FriendFeedStatus{ID: authorID}
		}
		u := matchCommunityFriend(plextv.CommunityFriend{ID: st.ID, Username: st.Username}, us)
		if u == nil {
			continue
		}
		src := "plex-community:" + st.ID
		for _, t := range titles {
			if !s.allowed(u, &t) {
				continue
			}
			var exists int
			isNew := s.db.QueryRowContext(ctx, `SELECT 1 FROM watchlist_items WHERE user_id=? AND item_key=? AND source=?`, u.ID, t.Key, src).Scan(&exists) != nil
			if err := s.upsert(ctx, u.ID, src, t); err == nil && isNew {
				s.queueRSSCheck(*u)
			}
		}
	}
}

type externalRef struct {
	source string         // "tmdb", "imdb", "tvdb"
	id     string         // numeric id or "tt..."
	typ    core.MediaType // optional TypeMovie / TypeShow
}

func (s *Service) resolveRSSItem(ctx context.Context, it rssItem) (*core.Title, error) {
	itemType := rssItemMediaType(it)
	refs := extractRSSExternalRefs(it, itemType)
	if len(refs) == 0 {
		return nil, nil
	}
	typedCat, hasTyped := s.o.Catalog.(typedExternalCatalog)
	for _, ref := range refs {
		typ := ref.typ
		if typ == "" {
			typ = itemType
		}
		switch ref.source {
		case "tmdb":
			id, err := strconv.ParseInt(ref.id, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			var keys []core.RatingKey
			switch typ {
			case core.TypeMovie:
				keys = []core.RatingKey{core.MovieKey(id)}
			case core.TypeShow:
				keys = []core.RatingKey{core.ShowKey(id)}
			default:
				keys = []core.RatingKey{core.MovieKey(id), core.ShowKey(id)}
			}
			for _, k := range keys {
				t, e := s.o.Catalog.Get(ctx, k)
				if e != nil && !errors.Is(e, core.ErrNotFound) {
					return nil, e
				}
				if e == nil && t != nil && (t.Type == core.TypeMovie || t.Type == core.TypeShow) {
					return t, nil
				}
			}
		case "imdb", "tvdb":
			var t *core.Title
			var e error
			if hasTyped && (typ == core.TypeMovie || typ == core.TypeShow) {
				t, e = typedCat.ByExternalIDForType(ctx, ref.source, ref.id, typ)
				if errors.Is(e, core.ErrNotFound) && ref.source == "imdb" {
					t, e = s.o.Catalog.ByExternalID(ctx, ref.source, ref.id)
				}
			} else {
				t, e = s.o.Catalog.ByExternalID(ctx, ref.source, ref.id)
			}
			if e != nil && !errors.Is(e, core.ErrNotFound) {
				return nil, e
			}
			if e == nil && t != nil && (t.Type == core.TypeMovie || t.Type == core.TypeShow) {
				return t, nil
			}
		}
	}
	return nil, nil
}

func rssItemMediaType(it rssItem) core.MediaType {
	for _, c := range it.Categories {
		val := strings.TrimSpace(c.Value)
		if val == "" {
			val = strings.TrimSpace(c.Term)
		}
		switch strings.ToLower(val) {
		case "movie", "movies", "film":
			return core.TypeMovie
		case "show", "shows", "tv", "series":
			return core.TypeShow
		}
	}
	for _, raw := range append(append([]string(nil), it.Links...), it.Guids...) {
		low := strings.ToLower(raw)
		switch {
		case strings.Contains(low, "/movie/") || strings.HasPrefix(low, "plex://movie/"):
			return core.TypeMovie
		case strings.Contains(low, "/show/") || strings.Contains(low, "/tv/") || strings.Contains(low, "/series/") || strings.HasPrefix(low, "plex://show/"):
			return core.TypeShow
		}
	}
	return ""
}

func extractRSSExternalRefs(it rssItem, fallbackType core.MediaType) []externalRef {
	var out []externalRef
	seen := map[string]bool{}
	add := func(source, id string, typ core.MediaType) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if source == "imdb" {
			id = strings.ToLower(id)
			if !strings.HasPrefix(id, "tt") || len(id) < 3 {
				return
			}
			for _, ch := range id[2:] {
				if ch < '0' || ch > '9' {
					return
				}
			}
		} else {
			for _, ch := range id {
				if ch < '0' || ch > '9' {
					return
				}
			}
		}
		key := source + ":" + id + ":" + string(typ)
		if !seen[key] {
			seen[key] = true
			out = append(out, externalRef{source: source, id: id, typ: typ})
		}
	}

	parseOne := func(raw string) {
		s := strings.TrimSpace(raw)
		if s == "" {
			return
		}
		low := strings.ToLower(s)
		switch {
		case strings.HasPrefix(low, "imdb://"), strings.HasPrefix(low, "imdb:"):
			id := strings.TrimPrefix(strings.TrimPrefix(low, "imdb://"), "imdb:")
			add("imdb", strings.Trim(id, "/"), fallbackType)
			return
		case strings.HasPrefix(low, "tmdb://"), strings.HasPrefix(low, "tmdb:"), strings.HasPrefix(low, "themoviedb://"):
			rest := low
			for _, p := range []string{"themoviedb://", "tmdb://", "tmdb:"} {
				rest = strings.TrimPrefix(rest, p)
			}
			rest = strings.Trim(rest, "/")
			switch {
			case strings.HasPrefix(rest, "movie/"):
				add("tmdb", strings.TrimPrefix(rest, "movie/"), core.TypeMovie)
			case strings.HasPrefix(rest, "show/"):
				add("tmdb", strings.TrimPrefix(rest, "show/"), core.TypeShow)
			case strings.HasPrefix(rest, "tv/"):
				add("tmdb", strings.TrimPrefix(rest, "tv/"), core.TypeShow)
			default:
				add("tmdb", rest, fallbackType)
			}
			return
		case strings.HasPrefix(low, "tvdb://"), strings.HasPrefix(low, "tvdb:"), strings.HasPrefix(low, "thetvdb://"):
			rest := low
			for _, p := range []string{"thetvdb://", "tvdb://", "tvdb:"} {
				rest = strings.TrimPrefix(rest, p)
			}
			rest = strings.Trim(rest, "/")
			switch {
			case strings.HasPrefix(rest, "movie/"), strings.HasPrefix(rest, "movies/"):
				add("tvdb", rest[strings.IndexByte(rest, '/')+1:], core.TypeMovie)
			case strings.HasPrefix(rest, "show/"), strings.HasPrefix(rest, "series/"):
				add("tvdb", rest[strings.IndexByte(rest, '/')+1:], core.TypeShow)
			default:
				add("tvdb", rest, fallbackType)
			}
			return
		}

		u, err := url.Parse(s)
		if err != nil {
			return
		}
		host := strings.ToLower(u.Hostname())
		segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
		switch {
		case host == "imdb.com" || strings.HasSuffix(host, ".imdb.com"):
			for i := 0; i+1 < len(segs); i++ {
				if strings.EqualFold(segs[i], "title") {
					add("imdb", segs[i+1], fallbackType)
				}
			}
		case host == "themoviedb.org" || strings.HasSuffix(host, ".themoviedb.org"):
			for i := 0; i+1 < len(segs); i++ {
				num := leadingDigits(segs[i+1])
				switch strings.ToLower(segs[i]) {
				case "movie":
					add("tmdb", num, core.TypeMovie)
				case "tv":
					add("tmdb", num, core.TypeShow)
				}
			}
		case host == "thetvdb.com" || strings.HasSuffix(host, ".thetvdb.com"):
			for i := 0; i+1 < len(segs); i++ {
				num := leadingDigits(segs[i+1])
				switch strings.ToLower(segs[i]) {
				case "series", "shows":
					add("tvdb", num, core.TypeShow)
				case "movies", "movie":
					add("tvdb", num, core.TypeMovie)
				}
			}
			if qid := u.Query().Get("id"); qid != "" {
				add("tvdb", qid, fallbackType)
			}
		}
		q := u.Query()
		if v := q.Get("imdb"); v != "" {
			add("imdb", v, fallbackType)
		}
		if v := q.Get("tmdb"); v != "" {
			add("tmdb", v, fallbackType)
		}
		if v := q.Get("tvdb"); v != "" {
			add("tvdb", v, fallbackType)
		}
	}

	for _, g := range it.Guids {
		parseOne(g)
	}
	for _, l := range it.Links {
		parseOne(l)
	}
	return out
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// replaceHouseholdFeedSnapshot atomically reconciles the shared household
// "Friends' watchlists" set (user_id=0, source="plex-friends-rss").
func (s *Service) replaceHouseholdFeedSnapshot(ctx context.Context, source string, titles []core.Title) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	keys := make(map[core.RatingKey]bool, len(titles))
	imported := 0
	changed := false
	now := s.o.Now().Unix()

	for _, t := range titles {
		if t.Adult || keys[t.Key] {
			continue
		}
		keys[t.Key] = true
		added := now - int64(imported)
		imported++
		raw, err := json.Marshal(t)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(?,?,?,?,?) ON CONFLICT(user_id,item_key,source) DO UPDATE SET title=excluded.title`, householdUserID, t.Key, source, string(raw), added); err != nil {
			return 0, err
		}
		changed = true
	}

	rows, err := tx.QueryContext(ctx, `SELECT user_id,item_key FROM watchlist_items WHERE source=?`, source)
	if err != nil {
		return 0, err
	}
	type pair struct {
		uid int64
		key core.RatingKey
	}
	var removed []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.uid, &p.key); err != nil {
			rows.Close()
			return 0, err
		}
		if p.uid != householdUserID || !keys[p.key] {
			removed = append(removed, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, p := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_items WHERE user_id=? AND source=? AND item_key=?`, p.uid, source, p.key); err != nil {
			return 0, err
		}
		if err := forgetEpisodes(ctx, tx, p.uid, p.key); err != nil {
			return 0, err
		}
		changed = true
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if changed {
		s.gen.Add(1)
	}
	return imported, nil
}

// replaceOwnerFeedSnapshot atomically reconciles the owner's "Your Watchlist"
// RSS rows (user_id=owner.ID, source="plex-rss") and pushes newly added titles
// to the owner's linked Trakt account when available.
func (s *Service) replaceOwnerFeedSnapshot(ctx context.Context, source string, owner *core.User, titles []core.Title) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	keys := map[core.RatingKey]bool{}
	var toPush []core.Title
	imported := 0
	changed := false
	now := s.o.Now().Unix()

	if owner != nil {
		for _, t := range titles {
			if keys[t.Key] || !s.allowed(owner, &t) {
				continue
			}
			keys[t.Key] = true
			added := now - int64(imported)
			imported++

			var existed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM watchlist_items WHERE user_id=? AND item_key=?)`, owner.ID, t.Key).Scan(&existed); err != nil {
				return 0, err
			}
			raw, err := json.Marshal(t)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO watchlist_items(user_id,item_key,source,title,added) VALUES(?,?,?,?,?) ON CONFLICT(user_id,item_key,source) DO UPDATE SET title=excluded.title`, owner.ID, t.Key, source, string(raw), added); err != nil {
				return 0, err
			}
			changed = true
			if !existed {
				toPush = append(toPush, t)
			}
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT user_id,item_key FROM watchlist_items WHERE source=?`, source)
	if err != nil {
		return 0, err
	}
	type pair struct {
		uid int64
		key core.RatingKey
	}
	var removed []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.uid, &p.key); err != nil {
			rows.Close()
			return 0, err
		}
		if owner == nil || p.uid != owner.ID || !keys[p.key] {
			removed = append(removed, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, p := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_items WHERE user_id=? AND source=? AND item_key=?`, p.uid, source, p.key); err != nil {
			return 0, err
		}
		if err := forgetEpisodes(ctx, tx, p.uid, p.key); err != nil {
			return 0, err
		}
		changed = true
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if changed {
		s.gen.Add(1)
	}

	if owner != nil && source == sourcePlexOwnerRSS && len(toPush) > 0 {
		if writer, ok := s.o.Trakt.(traktWriter); ok {
			for _, t := range toPush {
				if ctx.Err() != nil {
					break
				}
				pushCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				_, pushErr := writer.SetWatchlist(pushCtx, owner.ID, t, true)
				cancel()
				if pushErr != nil {
					s.o.Logger.Warn("watchlist rss trakt push failed", "user", owner.ID, "item", t.Key, "err", pushErr)
				}
			}
		}
	}
	return imported, nil
}

func (s *Service) clearFeedSource(ctx context.Context, source, metaKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT user_id,item_key FROM watchlist_items WHERE source=?`, source)
	if err != nil {
		return err
	}
	type pair struct {
		uid int64
		key core.RatingKey
	}
	var removed []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.uid, &p.key); err != nil {
			rows.Close()
			return err
		}
		removed = append(removed, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_items WHERE source=?`, source); err != nil {
		return err
	}
	for _, p := range removed {
		if err := forgetEpisodes(ctx, tx, p.uid, p.key); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watchlist_meta WHERE k=?`, metaKey); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(removed) > 0 {
		s.gen.Add(1)
	}
	if source == sourcePlexFriendsRSS {
		s.notifyFriendsRSSChanged(ctx)
	}
	return nil
}

func (s *Service) notifyFriendsRSSChanged(ctx context.Context) {
	if s.o.OnFriendsRSSChanged != nil {
		s.o.OnFriendsRSSChanged(ctx)
	}
}

func (s *Service) resetFeedMeta(ctx context.Context, metaKey string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM watchlist_meta WHERE k=?`, metaKey)
}

func (s *Service) readRSSMeta(ctx context.Context, metaKey string) rssMeta {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT v FROM watchlist_meta WHERE k=?`, metaKey).Scan(&raw); err != nil || raw == "" {
		return rssMeta{}
	}
	var m rssMeta
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func (s *Service) writeRSSMeta(ctx context.Context, metaKey string, m rssMeta) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO watchlist_meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, metaKey, string(raw)); err != nil {
		s.o.Logger.Warn("watchlist: record rss status failed", "key", metaKey, "err", err)
	}
}

func (s *Service) feedStatus(ctx context.Context, feedURL, metaKey string) *RSSFeedStatus {
	if feedURL == "" {
		return nil
	}
	m := s.readRSSMeta(ctx, metaKey)
	if m.URLHash != "" && m.URLHash != sha256Hex(feedURL) {
		m = rssMeta{}
	}
	return &RSSFeedStatus{
		Configured:      true,
		LastRead:        m.LastRead,
		Imported:        m.Imported,
		UnmatchedTitles: m.UnmatchedTitles,
		Problem:         m.Problem,
		Error:           m.Error,
	}
}

func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
