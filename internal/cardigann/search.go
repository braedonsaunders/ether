package cardigann

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// Reserver reserves one unit of an indexer's daily budget ("hit" or
// "grab") before a request that counts against it.
type Reserver func(ctx context.Context, kind string) error

func noReserve(context.Context, string) error { return nil }

// query is one search attempt.
type query struct {
	mode     string // search, movie-search, tv-search
	q        string // title (and year for movies)
	keywords string // q plus the episode string for TV
	season   int
	ep       int
	year     int
	imdb     string // "tt0133093"
	tmdb     int64
	tvdb     int64
	wanted   []int // Newznab categories wanted
	idSearch bool
	test     bool
}

// sanitizeTitle prepares a title for a site search: apostrophes are
// dropped and other punctuation becomes spaces.
func sanitizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\'' || r == '’':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (r *Runner) modeParams(mode string) ([]string, bool) {
	p, ok := r.def.Caps.Modes[mode]
	return p, ok
}

func hasParam(params []string, p string) bool {
	for _, x := range params {
		if x == p {
			return true
		}
	}
	return false
}

// attempts builds the ordered queries for a request: an identifier search
// when the definition supports one, then a keyword search.
func (r *Runner) attempts(req core.SearchRequest, movieCats, tvCats []int) []query {
	isTV := req.Type == core.TypeEpisode || req.Type == core.TypeShow || req.Season > 0
	base := query{wanted: movieCats, year: req.Year, mode: "movie-search"}
	if isTV {
		base = query{wanted: tvCats, year: req.Year, mode: "tv-search", season: req.Season, ep: req.Episode}
	}
	if len(base.wanted) == 0 {
		base.wanted = []int{2000}
		if isTV {
			base.wanted = []int{5000}
		}
	}
	params, ok := r.modeParams(base.mode)
	if !ok {
		base.mode = "search"
		params, _ = r.modeParams("search")
	}
	epString := ""
	if isTV && req.Season > 0 {
		epString = fmt.Sprintf("S%02d", req.Season)
		if req.Episode > 0 {
			epString += fmt.Sprintf("E%02d", req.Episode)
		}
	}
	var out []query
	idq := base
	idq.idSearch = true
	if imdb := strings.TrimSpace(req.IDs.IMDB); imdb != "" && hasParam(params, "imdbid") {
		if !strings.HasPrefix(imdb, "tt") {
			imdb = "tt" + imdb
		}
		idq.imdb = imdb
	}
	if req.IDs.TMDB > 0 && hasParam(params, "tmdbid") {
		idq.tmdb = req.IDs.TMDB
	}
	if req.IDs.TVDB > 0 && hasParam(params, "tvdbid") {
		idq.tvdb = req.IDs.TVDB
	}
	if idq.imdb != "" || idq.tmdb > 0 || idq.tvdb > 0 {
		idq.keywords = epString
		out = append(out, idq)
	}
	if title := sanitizeTitle(req.Title); title != "" {
		kq := base
		kq.q = title
		// Episode release names normally omit the series premiere year.
		// Keep Year as an explicit query variable and retain the canonical
		// IDs for validation, but do not AND an absent year into keywords.
		if req.Year > 0 && !isTV {
			kq.q = fmt.Sprintf("%s %d", title, req.Year)
		}
		kq.keywords = strings.TrimSpace(kq.q + " " + epString)
		out = append(out, kq)
	}
	return out
}

// searchVars builds the variables for one query.
func (r *Runner) searchVars(q query, cats []string) (vars, error) {
	v := r.vars()
	itoa := func(n int) string {
		if n <= 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	i64 := func(n int64) string {
		if n <= 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	qtype := "search"
	switch q.mode {
	case "movie-search":
		qtype = "movie"
	case "tv-search":
		qtype = "tvsearch"
	}
	qm := map[string]string{
		"Type": qtype, "Q": q.q, "Keywords": q.keywords, "Season": itoa(q.season), "Ep": itoa(q.ep),
		"Episode": itoa(q.ep), "Year": itoa(q.year), "IMDBID": q.imdb, "IMDBIDShort": strings.TrimPrefix(q.imdb, "tt"),
		"TMDBID": i64(q.tmdb), "TVDBID": i64(q.tvdb), "Limit": "100", "Offset": "0", "Extended": "1",
		"Categories": strings.Join(cats, ","),
	}
	v["Query"] = qm
	kw, err := applyFilters(q.keywords, r.def.Search.KeywordsFilters, r.env(v))
	if err != nil {
		return nil, fmt.Errorf("keywords filters: %w", err)
	}
	v["Keywords"] = kw
	v["Categories"] = cats
	return v, nil
}

// pathApplies reports whether a search path serves the requested site
// categories ("!" as the first entry inverts the list).
func pathApplies(pathCats, cats []string) bool {
	if len(pathCats) == 0 {
		return true
	}
	invert := pathCats[0] == "!"
	list := pathCats
	if invert {
		list = pathCats[1:]
	}
	hit := false
	for _, c := range cats {
		for _, p := range list {
			if c == p {
				hit = true
			}
		}
	}
	if invert {
		return !hit
	}
	return hit
}

// fallbackAttempts builds secondary queries tried only when the primary
// attempts return no results (without error): OriginalTitle, structural
// aliases, and season-pack / complete-series queries for TV episodes on
// torrent trackers.
func (r *Runner) fallbackAttempts(req core.SearchRequest, movieCats, tvCats []int) []query {
	isTV := req.Type == core.TypeEpisode || req.Type == core.TypeShow || req.Season > 0
	base := query{wanted: movieCats, year: req.Year, mode: "movie-search"}
	if isTV {
		base = query{wanted: tvCats, year: req.Year, mode: "tv-search", season: req.Season, ep: req.Episode}
	}
	if len(base.wanted) == 0 {
		base.wanted = []int{2000}
		if isTV {
			base.wanted = []int{5000}
		}
	}
	if _, ok := r.modeParams(base.mode); !ok {
		base.mode = "search"
	}
	epString := ""
	if isTV && req.Season > 0 {
		epString = fmt.Sprintf("S%02d", req.Season)
		if req.Episode > 0 {
			epString += fmt.Sprintf("E%02d", req.Episode)
		}
	}
	primarySanitized := strings.ToLower(sanitizeTitle(req.Title))
	seenTitle := map[string]bool{primarySanitized: true}
	var altTitles []string
	addTitle := func(raw string) {
		s := sanitizeTitle(raw)
		k := strings.ToLower(s)
		if s == "" || seenTitle[k] {
			return
		}
		seenTitle[k] = true
		altTitles = append(altTitles, s)
	}
	if req.OriginalTitle != "" {
		addTitle(req.OriginalTitle)
	}
	for _, a := range releaseparse.TitleAliases(req.Title) {
		addTitle(a)
	}
	if req.OriginalTitle != "" {
		for _, a := range releaseparse.TitleAliases(req.OriginalTitle) {
			addTitle(a)
		}
	}
	var out []query
	for _, t := range altTitles {
		kq := base
		kq.q = t
		if req.Year > 0 && !isTV {
			kq.q = fmt.Sprintf("%s %d", t, req.Year)
		}
		kq.keywords = strings.TrimSpace(kq.q + " " + epString)
		out = append(out, kq)
	}
	// Anime releases are numbered from the series start ("Show - 1071").
	if abs := releaseparse.AnimeSearchEpisode(req); abs > 0 && isTV {
		allTitles := []string{}
		if primary := sanitizeTitle(req.Title); primary != "" {
			allTitles = append(allTitles, primary)
		}
		for _, t := range append(allTitles, altTitles...) {
			aq := base
			aq.season, aq.ep = 0, 0
			aq.q = t
			aq.keywords = fmt.Sprintf("%s %02d", t, abs)
			out = append(out, aq)
		}
	}
	// Torrent season-pack / complete-series rescue when an episode search
	// returned 0 single-episode rows.
	if isTV && req.Season > 0 && req.Episode > 0 && !strings.EqualFold(r.def.Protocol, "usenet") {
		allTitles := []string{}
		if primary := sanitizeTitle(req.Title); primary != "" {
			allTitles = append(allTitles, primary)
		}
		allTitles = append(allTitles, altTitles...)
		seasonStr := fmt.Sprintf("S%02d", req.Season)
		for _, t := range allTitles {
			sq := base
			sq.ep = 0
			sq.q = t
			sq.keywords = strings.TrimSpace(t + " " + seasonStr)
			out = append(out, sq)
			sqWord := sq
			sqWord.keywords = strings.TrimSpace(fmt.Sprintf("%s Season %d", t, req.Season))
			out = append(out, sqWord)
		}
	}
	return out
}

// Search runs a request's attempts in order and returns the first
// non-empty result set.
func (r *Runner) Search(ctx context.Context, req core.SearchRequest, movieCats, tvCats []int, reserve Reserver) ([]core.Release, error) {
	if reserve == nil {
		reserve = noReserve
	}
	attempts := r.attempts(req, movieCats, tvCats)
	if len(attempts) == 0 {
		return nil, errors.New("search request has no title or supported identifier")
	}
	var firstErr error
	for _, q := range attempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := r.search(ctx, q, reserve)
		if err != nil {
			if errors.Is(err, core.ErrBudgetExhausted) {
				return nil, err
			}
			if firstErr == nil {
				firstErr = err
			}
			r.log.Debug("cardigann search attempt failed", "indexer", r.id, "mode", q.mode, "err", err)
			continue
		}
		if len(res) > 0 {
			return res, nil
		}
	}
	if firstErr == nil {
		fallbacks := r.fallbackAttempts(req, movieCats, tvCats)
		if len(fallbacks) > 0 && r.def.RequestDelay <= 0 {
			const burstWidth = 4
			for base := 0; base < len(fallbacks); base += burstWidth {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				batch := fallbacks[base:min(base+burstWidth, len(fallbacks))]
				type slot struct {
					res []core.Release
					err error
				}
				slots := make([]slot, len(batch))
				var wg sync.WaitGroup
				for i, q := range batch {
					wg.Add(1)
					go func(i int, q query) {
						defer wg.Done()
						slots[i].res, slots[i].err = r.search(ctx, q, reserve)
					}(i, q)
				}
				wg.Wait()
				var merged []core.Release
				seen := map[string]bool{}
				var budgetErr error
				for _, s := range slots {
					if s.err != nil {
						if errors.Is(s.err, core.ErrBudgetExhausted) && budgetErr == nil {
							budgetErr = s.err
						}
						continue
					}
					for _, rel := range s.res {
						k := rel.GUID
						if rel.InfoHash != "" {
							k = rel.InfoHash
						}
						if !seen[k] {
							seen[k] = true
							merged = append(merged, rel)
						}
					}
				}
				if len(merged) > 0 {
					return merged, nil
				}
				if budgetErr != nil {
					return nil, budgetErr
				}
			}
		} else {
			for _, q := range fallbacks {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				res, err := r.search(ctx, q, reserve)
				if err != nil {
					if errors.Is(err, core.ErrBudgetExhausted) {
						return nil, err
					}
					continue
				}
				if len(res) > 0 {
					return res, nil
				}
			}
		}
	}
	return nil, firstErr
}

// found is one parsed row before conversion.
type found struct {
	rel  core.Release
	cats []int
	imdb string
	tmdb int64
	tvdb int64
}

func (r *Runner) search(ctx context.Context, q query, reserve Reserver) ([]core.Release, error) {
	var cats []string
	if q.test {
		for _, m := range r.def.Caps.CategoryMappings {
			if m.Default {
				cats = append(cats, m.ID)
			}
		}
	} else if len(r.def.Caps.CategoryMappings) > 0 {
		cats = r.def.trackerCategories(q.wanted)
		if len(cats) == 0 {
			// The site carries nothing in the wanted categories.
			return nil, nil
		}
	}
	if err := r.ensureLogin(ctx); err != nil {
		return nil, err
	}
	v, err := r.searchVars(q, cats)
	if err != nil {
		return nil, err
	}
	var rows []found
	ran := false
	for _, p := range r.def.Search.Paths {
		if !pathApplies(p.Categories, cats) {
			continue
		}
		ran = true
		got, err := r.searchPath(ctx, p, v, reserve)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	if !ran {
		return nil, nil
	}
	return r.finish(rows, q), nil
}

// finish applies row filters and request matching, de-duplicating.
func (r *Runner) finish(rows []found, q query) []core.Release {
	seen := map[string]bool{}
	var out []core.Release
	for _, f := range rows {
		if !q.test && !q.idSearch {
			skip := false
			for _, flt := range r.def.Search.Rows.Filters {
				if flt.Name == "andmatch" {
					limit, _ := strconv.Atoi(arg(flt, 0))
					if !andMatch(f.rel.Name, q.keywords, limit) {
						skip = true
					}
				}
			}
			if skip {
				continue
			}
		}
		if !q.test && len(f.cats) > 0 && len(q.wanted) > 0 {
			ok := false
			for _, c := range f.cats {
				for _, w := range q.wanted {
					if c == w || family(c) == family(w) {
						ok = true
					}
				}
			}
			if !ok {
				continue
			}
		}
		if q.imdb != "" && f.imdb != "" && !strings.EqualFold(q.imdb, f.imdb) {
			continue
		}
		if q.tmdb > 0 && f.tmdb > 0 && q.mode == "movie-search" && q.tmdb != f.tmdb {
			continue
		}
		if q.tvdb > 0 && f.tvdb > 0 && q.tvdb != f.tvdb {
			continue
		}
		key := f.rel.GUID
		if f.rel.InfoHash != "" {
			key = f.rel.InfoHash
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f.rel)
	}
	return out
}

// searchPath issues one search request and parses its rows.
func (r *Runner) searchPath(ctx context.Context, p SearchPath, v vars, reserve Reserver) ([]found, error) {
	inputs := p.Inputs
	if p.InheritInputs {
		inputs = mergeInputs(r.def.Search.Inputs, p.Inputs)
	}
	form, raw, err := r.renderInputs(inputs, v, !r.def.Search.AllowEmptyInputs)
	if err != nil {
		return nil, err
	}
	path, err := render(p.Path, v, url.QueryEscape)
	if err != nil {
		return nil, err
	}
	target, err := r.resolve(path, nil)
	if err != nil {
		return nil, err
	}
	// The method may be templated ("{{ if .Keywords }}post{{ else }}get{{ end }}").
	m := p.Method
	if strings.Contains(m, "{{") {
		if m, err = render(m, v, nil); err != nil {
			return nil, err
		}
		m = strings.ToLower(strings.TrimSpace(m))
	}
	method := http.MethodGet
	switch m {
	case "", "get":
	case "post":
		method = http.MethodPost
	default:
		return nil, fmt.Errorf("%s: search method %q is not supported", r.def.Name, m)
	}
	req := request{method: method, url: target.String(), form: form, raw: raw, headers: r.headers(r.def.Search.Headers, v), follow: r.def.FollowRedirect || p.FollowRedirect}
	if err := reserve(ctx, "hit"); err != nil {
		return nil, err
	}
	_, version := r.session()
	resp, err := r.http.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if r.def.Login != nil {
		if needed, hard := r.loginNeeded(resp, p.Response.Type); needed {
			if !hard {
				// The page lacks the signed-in marker but may still list
				// results (some sites omit it on search pages): only sign in
				// again when nothing could be read.
				if rows, err := r.readResponse(resp, p, v); err == nil && len(rows) > 0 {
					return rows, nil
				}
			}
			if err := r.reauth(ctx, version); err != nil {
				return nil, err
			}
			if resp, err = r.http.do(ctx, req); err != nil {
				return nil, err
			}
			if needed, hard := r.loginNeeded(resp, p.Response.Type); needed && hard {
				return nil, fmt.Errorf("%s: %w: the site did not accept the session after signing in", r.def.Name, ErrLogin)
			}
		}
	}
	if resp.redirected() {
		// Not a login redirect: follow it once more by hand.
		if strings.HasPrefix(resp.location, "magnet:") {
			return nil, fmt.Errorf("%s: search redirected to a magnet link", r.def.Name)
		}
		next := req
		next.url, next.method, next.form, next.raw, next.follow = resp.location, http.MethodGet, nil, "", true
		if resp, err = r.http.do(ctx, next); err != nil {
			return nil, err
		}
	}
	return r.readResponse(resp, p, v)
}

// readResponse checks a search response's status and parses its rows.
func (r *Runner) readResponse(resp *response, p SearchPath, v vars) ([]found, error) {
	switch {
	case resp.status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%s: the site is rate limiting requests (HTTP 429)", r.def.Name)
	case resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden:
		return nil, fmt.Errorf("%s: %w: HTTP %d", r.def.Name, ErrLogin, resp.status)
	case resp.redirected():
		return nil, fmt.Errorf("%s: search was redirected", r.def.Name)
	case resp.status/100 != 2:
		return nil, fmt.Errorf("%s: search returned HTTP %d", r.def.Name, resp.status)
	}
	body := r.http.text(resp)
	if len(r.def.Search.PreprocessingFilters) > 0 {
		var err error
		if body, err = applyFilters(body, r.def.Search.PreprocessingFilters, r.env(v)); err != nil {
			return nil, err
		}
	}
	if msg := p.Response.NoResultsMessage; msg != "" && strings.TrimSpace(body) == strings.TrimSpace(msg) {
		return nil, nil
	}
	if strings.TrimSpace(body) == "" && p.Response.Type == "json" {
		return nil, nil
	}
	switch p.Response.Type {
	case "json":
		return r.parseJSON(body, resp.url, v)
	case "xml":
		doc, err := parseXMLDocument([]byte(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.def.Name, err)
		}
		return r.parseDocument(doc, resp.url, v)
	default:
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("%s: parse page: %w", r.def.Name, err)
		}
		if msg := p.Response.NoResultsMessage; msg != "" && strings.Contains(doc.Text(), msg) {
			return nil, nil
		}
		return r.parseDocument(doc, resp.url, v)
	}
}

// loginNeeded reports a response that shows the session is gone. hard
// marks an unfollowed redirect or an auth status; a soft sign is an HTML
// page lacking the login test selector.
func (r *Runner) loginNeeded(resp *response, respType string) (needed, hard bool) {
	if resp.redirected() && !strings.HasPrefix(resp.location, "magnet:") {
		return true, true
	}
	if resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
		return true, true
	}
	t := r.def.Login.Test
	if t == nil || t.Selector == "" || respType == "json" || respType == "xml" {
		return false, false
	}
	ct := strings.ToLower(resp.header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "html") {
		return false, false
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.http.text(resp)))
	if err != nil {
		return false, false
	}
	m, err := compileCSS(mustRender(t.Selector, r.vars()))
	if err != nil {
		return false, false
	}
	return doc.FindMatcher(m).Length() == 0, false
}

// parseDocument reads rows from an HTML or XML document.
func (r *Runner) parseDocument(doc *goquery.Document, base *url.URL, v vars) ([]found, error) {
	if err := r.checkDocErrors(doc, v); err != nil {
		return nil, err
	}
	rowsSel := r.def.Search.Rows
	sel, err := render(rowsSel.Selector, v, nil)
	if err != nil {
		return nil, err
	}
	m, err := compileCSS(sel)
	if err != nil {
		return nil, err
	}
	rows := doc.FindMatcher(m)
	if rowsSel.Remove != "" {
		rm, err := compileCSS(rowsSel.Remove)
		if err != nil {
			return nil, err
		}
		rows.FindMatcher(rm).Remove()
	}
	list := make([]*goquery.Selection, 0, rows.Length())
	rows.Each(func(_ int, s *goquery.Selection) { list = append(list, s) })
	if rowsSel.After > 0 {
		// Clamped: a huge "after" (an int converted from a float the
		// definition gave) would overflow i below and index out of range.
		after := min(rowsSel.After, len(list))
		var merged []*goquery.Selection
		for i := 0; i < len(list); i++ {
			cur := list[i]
			for j := 1; j <= after && i+j < len(list); j++ {
				cur.AppendSelection(list[i+j].Contents())
			}
			merged = append(merged, cur)
			i += after
		}
		list = merged
	}
	var out []found
	var firstErr error
	for _, row := range list {
		rv := v.clone()
		rv["Result"] = map[string]string{}
		f, err := r.parseRow(rv, base, func(s Selector, env filterEnv) (string, bool, error) {
			return htmlValue(s, row, env)
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r.log.Debug("cardigann: skipped row", "indexer", r.id, "err", err)
			continue
		}
		if rowsSel.DateHeaders != nil && f.rel.PubDate.IsZero() {
			if d, ok := r.dateHeader(row, *rowsSel.DateHeaders, rv); ok {
				f.rel.PubDate = d
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, fmt.Errorf("%s: no result row could be read: %w", r.def.Name, firstErr)
	}
	return out, nil
}

func (r *Runner) checkDocErrors(doc *goquery.Document, v vars) error {
	env := r.env(v)
	for _, e := range r.def.Search.Error {
		m, err := compileCSS(mustRender(e.Selector, v))
		if err != nil {
			return err
		}
		hit := doc.FindMatcher(m).First()
		if hit.Length() == 0 {
			continue
		}
		msg := normalizeSpace(hit.Text())
		if e.Message != nil {
			if val, ok, err := htmlValue(*e.Message, doc.Selection, env); err == nil && ok {
				msg = val
			}
		}
		return fmt.Errorf("%s: the site reported an error: %s", r.def.Name, abbreviate(msg))
	}
	return nil
}

// dateHeader finds the nearest preceding row matching the dateheaders
// selector (walking up to the parent's previous siblings when needed).
func (r *Runner) dateHeader(row *goquery.Selection, sel Selector, v vars) (time.Time, bool) {
	env := r.env(v)
	prev := row.Prev()
	if prev.Length() == 0 {
		prev = row.Parent().Prev()
	}
	for guard := 0; prev.Length() > 0 && guard < 500; guard++ {
		if val, ok, err := htmlValue(sel, prev, env); err == nil && ok && val != "" {
			if t, err := fuzzyTime(val, r.now()); err == nil {
				return t, true
			}
		}
		next := prev.Prev()
		if next.Length() == 0 {
			next = prev.Parent().Prev()
		}
		prev = next
	}
	return time.Time{}, false
}

// parseJSON reads rows from a JSON response.
func (r *Runner) parseJSON(body string, base *url.URL, v vars) ([]found, error) {
	root, err := decodeJSON([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("%s: response is not JSON: %w", r.def.Name, err)
	}
	rowsSel := r.def.Search.Rows
	env := r.env(v)
	if rowsSel.Count != nil {
		if val, ok, err := jsonValue(*rowsSel.Count, root, nil, env); err == nil && ok {
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n < 1 {
				return nil, nil
			}
		}
	}
	sel, err := render(rowsSel.Selector, v, nil)
	if err != nil {
		return nil, err
	}
	rows, ok, err := jsonRows(root, sel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.def.Name, err)
	}
	if !ok {
		if rowsSel.MissingAttributeEqualsNoResults {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: the response has no %q", r.def.Name, sel)
	}
	var out []found
	var firstErr error
	for _, row := range rows {
		items := []any{row}
		var parent any
		if rowsSel.Attribute != "" || rowsSel.Multiple {
			node := row
			if rowsSel.Attribute != "" {
				var has bool
				if node, has = jsonPath(row, rowsSel.Attribute); !has {
					continue
				}
			}
			if rowsSel.Multiple {
				parent = row
				switch x := node.(type) {
				case []any:
					items = x
				case map[string]any:
					items = objectValues(x)
				default:
					items = nil
				}
			} else {
				items = []any{node}
			}
		}
		for _, item := range items {
			rv := v.clone()
			rv["Result"] = map[string]string{}
			it, par := item, parent
			f, err := r.parseRow(rv, base, func(s Selector, env filterEnv) (string, bool, error) {
				return jsonValue(s, it, par, env)
			})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, fmt.Errorf("%s: no result row could be read: %w", r.def.Name, firstErr)
	}
	return out, nil
}

// optionalFields never fail a row when missing.
var optionalFields = map[string]bool{
	"imdb": true, "imdbid": true, "tmdbid": true, "rageid": true, "tvdbid": true, "tvmazeid": true,
	"traktid": true, "doubanid": true, "poster": true, "banner": true, "description": true, "genre": true,
}

type rowData struct {
	title, details, download, magnet, infohash string
	size                                       int64
	seeders, leechers, grabs                   int
	date                                       time.Time
	cats                                       []int
	imdb                                       string
	tmdb, tvdb                                 int64
}

// parseRow evaluates the fields of one row in order; each value is visible
// to later fields as .Result.<name>.
func (r *Runner) parseRow(v vars, base *url.URL, eval func(Selector, filterEnv) (string, bool, error)) (found, error) {
	res := v.result()
	env := r.env(v)
	var d rowData
	for _, f := range r.def.Search.Fields {
		optional := f.Selector.Optional || optionalFields[f.Name] || hasMod(f.Modifiers, "optional")
		val, ok, err := eval(f.Selector, env)
		if err != nil {
			if optional {
				res[f.Name] = ""
				continue
			}
			return found{}, fmt.Errorf("field %s: %w", f.Name, err)
		}
		if !ok || strings.TrimSpace(val) == "" {
			if f.Selector.HasDefault {
				val = mustRender(f.Selector.Default, v)
			}
			if !ok && val == "" && !optional {
				return found{}, fmt.Errorf("field %s: %w", f.Name, errNotFound)
			}
			if val == "" {
				res[f.Name] = ""
				continue
			}
		}
		val, err = r.applyField(&d, f, val, base)
		if err != nil {
			if optional {
				continue
			}
			return found{}, fmt.Errorf("field %s: %w", f.Name, err)
		}
		res[f.Name] = val
	}
	return r.toFound(d)
}

func hasMod(mods []string, m string) bool {
	for _, x := range mods {
		if strings.EqualFold(x, m) {
			return true
		}
	}
	return false
}

var digitsRE = regexp.MustCompile(`\d+`)
var imdbRE = regexp.MustCompile(`(?i)(?:tt)?(\d{5,10})`)

func (r *Runner) applyField(d *rowData, f Field, val string, base *url.URL) (string, error) {
	switch f.Name {
	case "title":
		if hasMod(f.Modifiers, "append") {
			d.title += val
		} else {
			d.title = val
		}
		return d.title, nil
	case "download":
		if strings.HasPrefix(strings.ToLower(val), "magnet:") {
			d.magnet = val
			return val, nil
		}
		u, err := r.resolve(val, base)
		if err != nil {
			return val, err
		}
		d.download = u.String()
		return d.download, nil
	case "magnet":
		d.magnet = val
	case "infohash":
		d.infohash = val
	case "details", "comments":
		u, err := r.resolve(val, base)
		if err != nil {
			return val, err
		}
		if f.Name == "details" || d.details == "" {
			d.details = u.String()
		}
		return u.String(), nil
	case "category":
		cats := r.def.mapSiteCategory(val)
		if hasMod(f.Modifiers, "noappend") {
			d.cats = cats
		} else {
			d.cats = appendUnique(d.cats, cats)
		}
	case "categorydesc":
		cats := r.def.mapSiteCategoryDesc(val)
		if hasMod(f.Modifiers, "noappend") {
			d.cats = cats
		} else {
			d.cats = appendUnique(d.cats, cats)
		}
	case "size":
		d.size = parseSize(val)
	case "seeders":
		d.seeders = coerceInt(val)
	case "leechers":
		d.leechers = coerceInt(val)
	case "grabs":
		d.grabs = coerceInt(val)
	case "date":
		t, err := fuzzyTime(val, r.now())
		if err != nil {
			r.log.Debug("cardigann: unreadable date", "indexer", r.id, "value", abbreviate(val))
		} else {
			d.date = t
		}
	case "imdb", "imdbid":
		if m := imdbRE.FindStringSubmatch(val); m != nil {
			id := m[1]
			for len(id) < 7 {
				id = "0" + id
			}
			d.imdb = "tt" + id
		}
	case "tmdbid":
		if m := digitsRE.FindString(val); m != "" {
			d.tmdb, _ = strconv.ParseInt(m, 10, 64)
		}
	case "tvdbid":
		if m := digitsRE.FindString(val); m != "" {
			d.tvdb, _ = strconv.ParseInt(m, 10, 64)
		}
	case "poster":
		if u, err := r.resolve(val, base); err == nil {
			return u.String(), nil
		}
	}
	return val, nil
}

func appendUnique(list, add []int) []int {
	for _, a := range add {
		dup := false
		for _, x := range list {
			if x == a {
				dup = true
			}
		}
		if !dup {
			list = append(list, a)
		}
	}
	return list
}

// toFound converts row data into a release.
func (r *Runner) toFound(d rowData) (found, error) {
	title := strings.TrimSpace(d.title)
	if title == "" {
		return found{}, errors.New("row has no title")
	}
	rel := core.Release{Name: title, Size: d.size, PubDate: d.date, Grabs: d.grabs, Category: bestCategory(d.cats), TitleIDs: core.IDs{IMDB: d.imdb, TMDB: d.tmdb, TVDB: d.tvdb}}
	if r.def.Protocol == "usenet" {
		if d.download == "" {
			return found{}, errors.New("row has no NZB download link")
		}
		rel.NZBURL = d.download
	} else {
		rel.Protocol = core.ProtocolTorrent
		rel.Seeders, rel.Peers = d.seeders, d.seeders+d.leechers
		rel.NZBURL = d.download
		rel.MagnetURL = d.magnet
		rel.InfoHash = newznab.NormalizeInfoHash(d.infohash)
		if rel.InfoHash == "" && d.magnet != "" {
			rel.InfoHash = newznab.InfoHashFromMagnet(d.magnet)
		}
		if rel.MagnetURL == "" && rel.InfoHash != "" {
			rel.MagnetURL = newznab.MagnetFor(rel.InfoHash, title, nil)
		}
		if rel.NZBURL == "" && rel.MagnetURL == "" {
			return found{}, errors.New("row has no download link, magnet or info hash")
		}
	}
	switch {
	case d.details != "":
		rel.GUID = d.details
	case d.download != "":
		rel.GUID = d.download
	case rel.InfoHash != "":
		rel.GUID = rel.InfoHash
	default:
		rel.GUID = rel.MagnetURL
	}
	return found{rel: rel, cats: d.cats, imdb: d.imdb, tmdb: d.tmdb, tvdb: d.tvdb}, nil
}
