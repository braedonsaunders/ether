package newznab

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// searchAttemptsFor dispatches to the query builder for the resolved
// identity: the ether identity keeps the established attempt list, while a
// persona sends the shape its real client sends.
func (c *Client) searchAttemptsFor(req core.SearchRequest, caps *Caps, ident resolvedIdentity) []url.Values {
	if ident.profile == profileCurrent {
		return c.searchAttempts(req, caps)
	}
	return c.personaSearchAttempts(req, caps, ident)
}

// rescueAttemptsFor dispatches the broader second-pass queries.
func (c *Client) rescueAttemptsFor(req core.SearchRequest, caps *Caps, ident resolvedIdentity) []url.Values {
	if ident.profile == profileCurrent {
		return c.rescueAttempts(req, caps)
	}
	return c.personaRescueAttempts(req, caps, ident)
}

// personaBase is the common part of a persona query: the verb, categories,
// extended results, and an offset even on the first page.
func (c *Client) personaBase(isTV bool, verb string) url.Values {
	cfg := c.Config()
	v := url.Values{"t": {verb}, "extended": {"1"}, "offset": {"0"}, "limit": {strconv.Itoa(defaultSearchLimit)}}
	cats := cfg.MovieCategories
	if isTV {
		cats = cfg.TVCategories
		if len(cats) == 0 {
			cats = []int{5000}
		}
	} else if len(cats) == 0 {
		cats = []int{2000}
	}
	v.Set("cat", joinCats(cats))
	return v
}

// personaSearchAttempts builds one identifier query plus a text fallback.
// Prowlarr combines every supported identifier with the text query; the
// servarr clients send one identifier, then fall back to a text search.
func (c *Client) personaSearchAttempts(req core.SearchRequest, caps *Caps, ident resolvedIdentity) []url.Values {
	isTV := req.Type == core.TypeEpisode || req.Season > 0 || req.Type == core.TypeShow
	verb := "movie"
	if isTV {
		verb = "tvsearch"
	}
	supported := func(param string) bool {
		if caps == nil {
			return true
		}
		if isTV {
			return !caps.TVAvail || hasParam(caps.TVParams, param)
		}
		return !caps.MovieAvail || hasParam(caps.MovieParams, param)
	}
	type idParam struct{ name, value string }
	var ids []idParam
	imdb := strings.TrimPrefix(req.IDs.IMDB, "tt")
	if isTV {
		if req.IDs.TVDB > 0 && supported("tvdbid") {
			ids = append(ids, idParam{"tvdbid", strconv.FormatInt(req.IDs.TVDB, 10)})
		}
		if req.IDs.TMDB > 0 && supported("tmdbid") {
			ids = append(ids, idParam{"tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)})
		}
		if imdb != "" && supported("imdbid") {
			ids = append(ids, idParam{"imdbid", imdb})
		}
	} else {
		if imdb != "" && supported("imdbid") {
			ids = append(ids, idParam{"imdbid", imdb})
		}
		if req.IDs.TMDB > 0 && supported("tmdbid") {
			ids = append(ids, idParam{"tmdbid", strconv.FormatInt(req.IDs.TMDB, 10)})
		}
	}

	title := strings.TrimSpace(req.Title)
	textQuery := title
	if isTV {
		switch {
		case req.Season > 0 && req.Episode > 0:
			textQuery = fmt.Sprintf("%s S%02dE%02d", title, req.Season, req.Episode)
		case req.Season > 0:
			textQuery = fmt.Sprintf("%s S%02d", title, req.Season)
		case req.Episode > 0:
			textQuery = fmt.Sprintf("%s E%02d", title, req.Episode)
		}
	} else if req.Year > 0 {
		textQuery = fmt.Sprintf("%s %d", title, req.Year)
	}
	text := func() url.Values {
		v := c.personaBase(isTV, "search")
		v.Set("q", textQuery)
		return v
	}

	if ident.profile == profileProwlarr {
		if len(ids) == 0 {
			return []url.Values{text()}
		}
		v := c.personaBase(isTV, verb)
		for _, p := range ids {
			v.Set(p.name, p.value)
		}
		if isTV {
			if req.Season > 0 {
				v.Set("season", strconv.Itoa(req.Season))
			}
			if req.Episode > 0 {
				v.Set("ep", strconv.Itoa(req.Episode))
			}
		}
		v.Set("q", title)
		return []url.Values{v}
	}

	var out []url.Values
	if len(ids) > 0 {
		v := c.personaBase(isTV, verb)
		v.Set(ids[0].name, ids[0].value)
		if isTV {
			if req.Season > 0 {
				v.Set("season", strconv.Itoa(req.Season))
			}
			if req.Episode > 0 {
				v.Set("ep", strconv.Itoa(req.Episode))
			}
		}
		out = append(out, v)
		// Older TV on torrent indexers often exists only as a season pack.
		if isTV && req.Season > 0 && req.Episode > 0 && c.Config().IsTorrent() {
			pack := c.personaBase(isTV, verb)
			pack.Set(ids[0].name, ids[0].value)
			pack.Set("season", strconv.Itoa(req.Season))
			out = append(out, pack)
		}
	}
	return append(out, text())
}

// personaRescueAttempts widens a persona search to season-pack and plain
// keyword queries; sorting and matching still run on the results. Queries
// Search already spent are skipped so rescue broadens instead of repeating.
func (c *Client) personaRescueAttempts(req core.SearchRequest, caps *Caps, ident resolvedIdentity) []url.Values {
	tried := map[string]bool{}
	primary := c.personaSearchAttempts(req, caps, ident)
	// The primary attempt always runs; fallbacks may not have. Skip only the
	// primary so rescue never repeats a query Search certainly spent.
	if len(primary) > 0 {
		tried[primary[0].Encode()] = true
	}
	isTV := req.Type == core.TypeEpisode || req.Season > 0 || req.Type == core.TypeShow
	title := strings.TrimSpace(req.Title)
	var out []url.Values
	add := func(v url.Values) {
		enc := v.Encode()
		if enc == "" || tried[enc] {
			return
		}
		tried[enc] = true
		out = append(out, v)
	}
	if isTV {
		if req.Season > 0 {
			v := c.personaBase(true, "search")
			v.Set("q", fmt.Sprintf("%s S%02d", title, req.Season))
			add(v)
		}
		v := c.personaBase(true, "search")
		v.Set("q", title+" Complete")
		add(v)
		return out
	}
	v := c.personaBase(false, "search")
	v.Set("q", title)
	add(v)
	if req.Year > 0 {
		vy := c.personaBase(false, "search")
		vy.Set("q", fmt.Sprintf("%s %d", title, req.Year))
		add(vy)
	}
	return out
}
