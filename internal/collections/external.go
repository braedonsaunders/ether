package collections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// ExternalBases are the origins external list sources are fetched from.
// Tests point them at httptest servers; the URLs owners paste are always
// validated against the real hosts.
type ExternalBases struct {
	TraktAPI   string // https://api.trakt.tv
	Letterboxd string // https://letterboxd.com
	MDBList    string // https://mdblist.com
	AniListAPI string // https://graphql.anilist.co
	AnimeIDs   string // the anime id mapping document (anilist -> TMDB/TVDB/IMDb)
	StevenLu   string // https://popular-movies-data.stevenlu.com
}

func defaultBases() ExternalBases {
	return ExternalBases{TraktAPI: "https://api.trakt.tv", Letterboxd: "https://letterboxd.com", MDBList: "https://mdblist.com",
		AniListAPI: "https://graphql.anilist.co",
		AnimeIDs:   "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json",
		StevenLu:   "https://popular-movies-data.stevenlu.com"}
}

// withDefaults fills origins left empty with the real ones.
func (b ExternalBases) withDefaults() ExternalBases {
	d := defaultBases()
	for _, f := range []struct {
		v *string
		d string
	}{
		{&b.TraktAPI, d.TraktAPI}, {&b.Letterboxd, d.Letterboxd}, {&b.MDBList, d.MDBList},
		{&b.AniListAPI, d.AniListAPI}, {&b.AnimeIDs, d.AnimeIDs}, {&b.StevenLu, d.StevenLu},
	} {
		if *f.v == "" {
			*f.v = f.d
		}
	}
	return b
}

// extRef is a title named by an external list.
type extRef struct {
	TMDB  int64
	IMDB  string
	TVDB  int64
	Type  string // "movie", "show" or "" (unknown)
	Title string
	Year  int
	Slug  string // Letterboxd film slug
}

// webClient fetches public pages politely: it identifies itself, spaces
// requests per host, and caches responses.
type webClient struct {
	http *http.Client
	ua   string
	now  func() time.Time
	ttl  time.Duration
	gaps map[string]time.Duration // minimum spacing per host

	mu    sync.Mutex
	last  map[string]time.Time
	cache map[string]webEntry
	hostL map[string]*sync.Mutex
}

type webEntry struct {
	body    []byte
	header  http.Header
	expires time.Time
}

func newWebClient(c *http.Client, ua string, now func() time.Time) *webClient {
	if c == nil {
		c = egress.Client(egress.CapMetadata, 30*time.Second)
	}
	return &webClient{http: c, ua: ua, now: now, ttl: 6 * time.Hour,
		gaps: map[string]time.Duration{"letterboxd.com": 1500 * time.Millisecond, "mdblist.com": time.Second, "api.trakt.tv": 500 * time.Millisecond,
			"graphql.anilist.co": 2 * time.Second, "popular-movies-data.stevenlu.com": time.Second, "raw.githubusercontent.com": time.Second},
		last: map[string]time.Time{}, cache: map[string]webEntry{}, hostL: map[string]*sync.Mutex{}}
}

// List services must not redirect a server-side request to another origin.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
		return fmt.Errorf("external list redirect changes origin")
	}
	return nil
}

var errHTTPNotFound = errors.New("not found")

func (w *webClient) get(ctx context.Context, rawURL string, hdr map[string]string) ([]byte, http.Header, error) {
	return w.do(ctx, http.MethodGet, rawURL, nil, hdr, nil)
}

// post sends body (a JSON query) and caches the reply by URL and body.
// APIs such as GraphQL report failures inside an HTTP 200 reply, so check
// vets a fresh reply first: a reply it rejects is returned as an error
// and not cached.
func (w *webClient) post(ctx context.Context, rawURL string, body []byte, hdr map[string]string, check func([]byte) error) ([]byte, http.Header, error) {
	return w.do(ctx, http.MethodPost, rawURL, body, hdr, check)
}

func (w *webClient) do(ctx context.Context, method, rawURL string, reqBody []byte, hdr map[string]string, check func([]byte) error) ([]byte, http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cacheKey := rawURL
	if reqBody != nil {
		cacheKey += "\x00" + string(reqBody)
	}
	w.mu.Lock()
	if e, ok := w.cache[cacheKey]; ok && w.now().Before(e.expires) {
		w.mu.Unlock()
		return e.body, e.header, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		w.mu.Unlock()
		return nil, nil, err
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	hl := w.hostL[host]
	if hl == nil {
		hl = &sync.Mutex{}
		w.hostL[host] = hl
	}
	w.mu.Unlock()

	// One request at a time per host, spaced by the host's gap.
	hl.Lock()
	defer hl.Unlock()
	w.mu.Lock()
	gap := w.gaps[host]
	wait := time.Until(w.last[host].Add(gap))
	w.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, nil, ctx.Err()
		case <-t.C:
		}
	}
	var rd io.Reader
	if reqBody != nil {
		rd = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", w.ua)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := *w.http
	if host == "graphql.anilist.co" || host == "popular-movies-data.stevenlu.com" {
		client.CheckRedirect = sameOriginRedirect
	}
	resp, err := client.Do(req)
	w.mu.Lock()
	w.last[host] = time.Now()
	w.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := time.Minute
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n >= 0 {
			if n > 86400 {
				n = 86400
			}
			delay = time.Duration(n) * time.Second
		} else if until, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			delay = time.Until(until)
		}
		if delay < 0 {
			delay = 0
		}
		if delay > 24*time.Hour {
			delay = 24 * time.Hour
		}
		w.mu.Lock()
		w.last[host] = time.Now().Add(delay - gap)
		w.mu.Unlock()
		return nil, nil, fmt.Errorf("%s: rate limited (%s)", u.Host, resp.Status)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil, fmt.Errorf("%s: %w", rawURL, errHTTPNotFound)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, nil, fmt.Errorf("%s: access denied (%s); is the list public?", u.Host, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("%s: %s", u.Host, resp.Status)
	}
	if check != nil {
		if err := check(body); err != nil {
			return nil, nil, err
		}
	}
	w.mu.Lock()
	if len(w.cache) > 2000 {
		w.cache = map[string]webEntry{}
	}
	ttl := w.ttl
	if host == "popular-movies-data.stevenlu.com" {
		ttl = 24 * time.Hour
	}
	w.cache[cacheKey] = webEntry{body: body, header: resp.Header.Clone(), expires: w.now().Add(ttl)}
	w.mu.Unlock()
	return body, resp.Header, nil
}

// ---------------------------------------------------------------------------
// URL parsing

func parseListURL(raw string, hosts ...string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("paste the list's URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("that is not a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("use an https URL")
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	for _, want := range hosts {
		if h == want {
			return u, nil
		}
	}
	return nil, fmt.Errorf("expected a %s URL", hosts[0])
}

var (
	traktUserList = regexp.MustCompile(`^/users/([^/]+)/lists/([^/]+)/?$`)
	traktList     = regexp.MustCompile(`^/lists/([^/]+)/?$`)
	lbList        = regexp.MustCompile(`^/([^/]+)/list/([^/]+)/?`)
	mdbList       = regexp.MustCompile(`^/lists/([^/]+)/([^/]+)/?`)
)

// parseTraktURL returns the API path of a public Trakt list's items.
func parseTraktURL(raw string) (string, error) {
	u, err := parseListURL(raw, "trakt.tv", "app.trakt.tv", "api.trakt.tv")
	if err != nil {
		return "", err
	}
	if m := traktUserList.FindStringSubmatch(u.Path); m != nil {
		return "/users/" + url.PathEscape(m[1]) + "/lists/" + url.PathEscape(m[2]) + "/items", nil
	}
	if m := traktList.FindStringSubmatch(u.Path); m != nil {
		return "/lists/" + url.PathEscape(m[1]) + "/items", nil
	}
	return "", fmt.Errorf("use a list URL like https://trakt.tv/users/<user>/lists/<list>")
}

// parseLetterboxdURL returns the canonical path of a Letterboxd list.
func parseLetterboxdURL(raw string) (string, error) {
	u, err := parseListURL(raw, "letterboxd.com")
	if err != nil {
		return "", err
	}
	m := lbList.FindStringSubmatch(u.Path)
	if m == nil {
		return "", fmt.Errorf("use a list URL like https://letterboxd.com/<user>/list/<list>/")
	}
	return "/" + m[1] + "/list/" + m[2] + "/", nil
}

// parseMDBListURL returns the path of an MDBList list.
func parseMDBListURL(raw string) (string, error) {
	u, err := parseListURL(raw, "mdblist.com")
	if err != nil {
		return "", err
	}
	m := mdbList.FindStringSubmatch(u.Path)
	if m == nil || m[2] == "json" {
		return "", fmt.Errorf("use a list URL like https://mdblist.com/lists/<user>/<list>")
	}
	return "/lists/" + m[1] + "/" + m[2], nil
}

// ---------------------------------------------------------------------------
// Trakt

const maxExternalPages = 10

func (s *Service) fetchTrakt(ctx context.Context, rawURL string) ([]extRef, error) {
	path, err := parseTraktURL(rawURL)
	if err != nil {
		return nil, err
	}
	clientID := ""
	if s.traktCfg != nil {
		clientID = strings.TrimSpace(s.traktCfg().ClientID)
	}
	if clientID == "" {
		return nil, fmt.Errorf("Trakt lists need a Trakt client ID (Settings → Trakt)")
	}
	hdr := map[string]string{"trakt-api-version": "2", "trakt-api-key": clientID, "Content-Type": "application/json"}
	var out []extRef
	for page := 1; page <= maxExternalPages; page++ {
		body, h, err := s.web.get(ctx, s.bases.TraktAPI+path+"?page="+strconv.Itoa(page)+"&limit=100", hdr)
		if err != nil {
			if page == 1 || ctx.Err() != nil {
				return nil, fmt.Errorf("trakt: %w", err)
			}
			return out, &partialListError{msg: fmt.Sprintf("Trakt list is incomplete: page %d failed: %v", page, err)}
		}
		var items []struct {
			Type  string `json:"type"`
			Movie *struct {
				Title string `json:"title"`
				Year  int    `json:"year"`
				IDs   struct {
					TMDB int64  `json:"tmdb"`
					IMDB string `json:"imdb"`
					TVDB int64  `json:"tvdb"`
				} `json:"ids"`
			} `json:"movie"`
			Show *struct {
				Title string `json:"title"`
				Year  int    `json:"year"`
				IDs   struct {
					TMDB int64  `json:"tmdb"`
					IMDB string `json:"imdb"`
					TVDB int64  `json:"tvdb"`
				} `json:"ids"`
			} `json:"show"`
		}
		if err := json.Unmarshal(body, &items); err != nil {
			if page > 1 && len(out) > 0 {
				return out, &partialListError{msg: fmt.Sprintf("Trakt list is incomplete: page %d has an invalid response: %v", page, err)}
			}
			return nil, fmt.Errorf("trakt: unexpected response: %w", err)
		}
		for _, it := range items {
			switch {
			case it.Movie != nil && (it.Type == "movie" || it.Type == ""):
				out = append(out, extRef{TMDB: it.Movie.IDs.TMDB, IMDB: it.Movie.IDs.IMDB, Type: "movie", Title: it.Movie.Title, Year: it.Movie.Year})
			case it.Show != nil:
				// Seasons and episodes of a show count as the show.
				out = append(out, extRef{TMDB: it.Show.IDs.TMDB, IMDB: it.Show.IDs.IMDB, TVDB: it.Show.IDs.TVDB, Type: "show", Title: it.Show.Title, Year: it.Show.Year})
			}
		}
		pages, _ := strconv.Atoi(h.Get("X-Pagination-Page-Count"))
		if pages <= page || len(items) == 0 {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Letterboxd

var (
	lbTagRe  = regexp.MustCompile(`<(?:div|li)\b[^>]*\bdata-(?:film|item)-slug="[^"]*"[^>]*>`)
	lbAttrRe = regexp.MustCompile(`\bdata-([a-z-]+)="([^"]*)"`)
	lbImgAlt = regexp.MustCompile(`<img\b[^>]*\balt="([^"]*)"`)
	lbNextRe = regexp.MustCompile(`<a\b[^>]*class="[^"]*\bnext\b[^"]*"[^>]*href="([^"]+)"`)
	lbYearRe = regexp.MustCompile(`^(.*?)\s*\((\d{4})\)\s*$`)
	lbTMDBRe = regexp.MustCompile(`data-tmdb-id="(\d+)"`)
	lbTypeRe = regexp.MustCompile(`data-tmdb-type="(movie|tv)"`)
	lbLinkRe = regexp.MustCompile(`themoviedb\.org/(movie|tv)/(\d+)`)
)

// parseLetterboxdList extracts films from one list page and the next
// page's path, handling both the classic poster markup and the newer
// LazyPoster components.
func parseLetterboxdList(page []byte) ([]extRef, string) {
	s := string(page)
	var out []extRef
	seen := map[string]bool{}
	locs := lbTagRe.FindAllStringIndex(s, -1)
	for i, loc := range locs {
		tag := s[loc[0]:loc[1]]
		attrs := map[string]string{}
		for _, m := range lbAttrRe.FindAllStringSubmatch(tag, -1) {
			attrs[m[1]] = html.UnescapeString(m[2])
		}
		slug := firstNonEmpty(attrs["film-slug"], attrs["item-slug"])
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		ref := extRef{Slug: slug, Type: "movie"}
		name := firstNonEmpty(attrs["item-name"], attrs["film-name"])
		if name == "" {
			end := len(s)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if m := lbImgAlt.FindStringSubmatch(s[loc[1]:end]); m != nil {
				name = html.UnescapeString(m[1])
			}
		}
		if m := lbYearRe.FindStringSubmatch(name); m != nil {
			name = m[1]
			ref.Year, _ = strconv.Atoi(m[2])
		}
		if y, err := strconv.Atoi(firstNonEmpty(attrs["film-release-year"], attrs["item-release-year"])); err == nil {
			ref.Year = y
		}
		ref.Title = strings.TrimSpace(name)
		out = append(out, ref)
	}
	next := ""
	if m := lbNextRe.FindStringSubmatch(s); m != nil {
		next = html.UnescapeString(m[1])
	}
	return out, next
}

func (s *Service) fetchLetterboxd(ctx context.Context, rawURL string) ([]extRef, error) {
	path, err := parseLetterboxdURL(rawURL)
	if err != nil {
		return nil, err
	}
	var out []extRef
	next := path
	for page := 1; page <= maxExternalPages && next != ""; page++ {
		if !strings.HasPrefix(next, "/") {
			if u, err := url.Parse(next); err == nil {
				next = u.Path
			}
		}
		body, _, err := s.web.get(ctx, s.bases.Letterboxd+next, nil)
		if err != nil {
			if page == 1 || ctx.Err() != nil {
				return nil, fmt.Errorf("letterboxd: %w", err)
			}
			return out, &partialListError{msg: fmt.Sprintf("Letterboxd list is incomplete: page %d failed: %v", page, err)}
		}
		refs, nxt := parseLetterboxdList(body)
		out = append(out, refs...)
		if nxt == next {
			break
		}
		next = nxt
	}
	return out, nil
}

// letterboxdFilmTMDB reads the TMDB id from a Letterboxd film page.
func (s *Service) letterboxdFilmTMDB(ctx context.Context, slug string) (int64, string, error) {
	body, _, err := s.web.get(ctx, s.bases.Letterboxd+"/film/"+url.PathEscape(slug)+"/", nil)
	if err != nil {
		return 0, "", err
	}
	typ := "movie"
	if m := lbTypeRe.FindSubmatch(body); m != nil {
		typ = string(m[1])
	}
	if m := lbTMDBRe.FindSubmatch(body); m != nil {
		id, _ := strconv.ParseInt(string(m[1]), 10, 64)
		return id, typ, nil
	}
	if m := lbLinkRe.FindSubmatch(body); m != nil {
		id, _ := strconv.ParseInt(string(m[2]), 10, 64)
		return id, string(m[1]), nil
	}
	return 0, "", errUnresolved
}

// ---------------------------------------------------------------------------
// MDBList

func (s *Service) fetchMDBList(ctx context.Context, rawURL string) ([]extRef, error) {
	path, err := parseMDBListURL(rawURL)
	if err != nil {
		return nil, err
	}
	body, _, err := s.web.get(ctx, s.bases.MDBList+path+"/json", map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, fmt.Errorf("mdblist: %w", err)
	}
	type item struct {
		ID          json.Number `json:"id"`
		TMDBID      json.Number `json:"tmdb_id"`
		IMDB        string      `json:"imdb_id"`
		TVDB        json.Number `json:"tvdb_id"`
		Title       string      `json:"title"`
		MediaType   string      `json:"mediatype"`
		ReleaseYear json.Number `json:"release_year"`
		Rank        int         `json:"rank"`
	}
	var items []item
	if err := json.Unmarshal(body, &items); err != nil {
		var obj struct {
			Movies []item `json:"movies"`
			Shows  []item `json:"shows"`
			Items  []item `json:"items"`
		}
		if err2 := json.Unmarshal(body, &obj); err2 != nil {
			return nil, fmt.Errorf("mdblist: unexpected response (is the list public?)")
		}
		for i := range obj.Movies {
			obj.Movies[i].MediaType = firstNonEmpty(obj.Movies[i].MediaType, "movie")
		}
		for i := range obj.Shows {
			obj.Shows[i].MediaType = firstNonEmpty(obj.Shows[i].MediaType, "show")
		}
		items = append(append(obj.Items, obj.Movies...), obj.Shows...)
	}
	out := make([]extRef, 0, len(items))
	for _, it := range items {
		r := extRef{IMDB: it.IMDB, Title: it.Title}
		if n, err := it.TMDBID.Int64(); err == nil && n > 0 {
			r.TMDB = n
		} else if n, err := it.ID.Int64(); err == nil && n > 0 {
			r.TMDB = n
		}
		r.TVDB, _ = it.TVDB.Int64()
		y, _ := it.ReleaseYear.Int64()
		r.Year = int(y)
		switch strings.ToLower(it.MediaType) {
		case "movie":
			r.Type = "movie"
		case "show", "tv":
			r.Type = "show"
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// StevenLu

// stevenLuLists are the published popular-movie feeds: the plain list and
// the variants filtered by critic or audience score.
var stevenLuLists = map[string]bool{
	"movies": true, "movies-imdb-min7": true, "movies-imdb-min8": true,
	"movies-metacritic-min50": true, "movies-metacritic-min60": true,
	"movies-metacritic-min70": true, "movies-metacritic-min80": true,
	"movies-rottentomatoes-min70": true, "movies-rottentomatoes-min80": true,
}

func stevenLuList(list string) (string, error) {
	if list == "" {
		return "movies", nil
	}
	if !stevenLuLists[list] {
		return "", fmt.Errorf("unknown StevenLu list %q", list)
	}
	return list, nil
}

func (s *Service) fetchStevenLu(ctx context.Context, list string) ([]extRef, error) {
	list, err := stevenLuList(list)
	if err != nil {
		return nil, err
	}
	body, _, err := s.web.get(ctx, s.bases.StevenLu+"/"+list+".json", map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, fmt.Errorf("stevenlu: %w", err)
	}
	var items []struct {
		Title  string      `json:"title"`
		TMDBID json.Number `json:"tmdb_id"`
		IMDB   string      `json:"imdb_id"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("stevenlu: unexpected response: %w", err)
	}
	out := make([]extRef, 0, len(items))
	for _, it := range items {
		r := extRef{IMDB: it.IMDB, Title: it.Title, Type: "movie"}
		if n, err := it.TMDBID.Int64(); err == nil && n > 0 {
			r.TMDB = n
		}
		out = append(out, r)
	}
	return out, nil
}
