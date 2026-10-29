package stremio

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/animemap"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

const (
	nzbTimeout = 60 * time.Second
	maxNZB     = 64 << 20
	// maxStreamHeaders bounds the request headers an addon may ask for.
	maxStreamHeaders = 16
)

// idLookup is the stream request for a search: the addon's content kind
// ("movie" or "series") and id, whether whole-season packs are wanted, and
// for an anime database id the episode's number inside that entry.
type idLookup struct {
	kind, id     string
	packs        bool
	entryEpisode int
}

// animeDatabases are the anime id prefixes Ether can map TMDB titles to,
// in the order they are tried.
var animeDatabases = []string{animemap.Kitsu, animemap.MAL, animemap.AniList, animemap.AniDB}

// lookupID chooses how to ask the addon for req: by IMDb id when the addon
// takes them, else by TMDB id, else (for anime) by an anime database id
// the community mapping knows unambiguously. ok is false when the addon
// cannot be asked, which skips it for this title.
func (b *Backend) lookupID(ctx context.Context, req core.SearchRequest) (idLookup, bool) {
	m := b.currentManifest(ctx)
	if m != nil {
		kind := "movie"
		if req.Type != core.TypeMovie {
			kind = "series"
		}
		if !m.serves(kind) {
			return idLookup{}, false
		}
	}
	takes := func(prefix string) bool { return m == nil && prefix == "tt" || m != nil && m.takesIDs(prefix) }
	movie := req.Type == core.TypeMovie
	ep, packs := req.Episode, false
	if !movie && ep <= 0 {
		ep, packs = 1, true
	}
	if imdb := imdbID(req.IDs.IMDB); imdb != "" && takes("tt") {
		if movie {
			return idLookup{kind: "movie", id: imdb}, true
		}
		return idLookup{kind: "series", id: fmt.Sprintf("%s:%d:%d", imdb, req.Season, ep), packs: packs}, true
	}
	if req.IDs.TMDB > 0 && takes("tmdb") {
		if movie {
			return idLookup{kind: "movie", id: fmt.Sprintf("tmdb:%d", req.IDs.TMDB)}, true
		}
		return idLookup{kind: "series", id: fmt.Sprintf("tmdb:%d:%d:%d", req.IDs.TMDB, req.Season, ep), packs: packs}, true
	}
	// Anime databases number each season or cour separately; whole-season
	// pack requests and non-anime titles never load the mapping.
	if !req.Anime || packs || m == nil {
		return idLookup{}, false
	}
	for _, db := range animeDatabases {
		if !m.takesIDs(db) {
			continue
		}
		id, episode, ok, err := b.svc.animeIDs().Lookup(ctx, req, db)
		if err != nil || !ok {
			continue
		}
		if movie {
			return idLookup{kind: "movie", id: fmt.Sprintf("%s:%d", db, id)}, true
		}
		return idLookup{kind: "series", id: fmt.Sprintf("%s:%d:%d", db, id, episode), entryEpisode: episode}, true
	}
	return idLookup{}, false
}

// origin is the addon's scheme://host[:port], without credentials.
func (b *Backend) origin() string {
	return (&url.URL{Scheme: b.base.Scheme, Host: b.base.Host}).String()
}

// httpLink returns raw when it is an absolute http(s) URL, else "".
func httpLink(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// requestHeaders are the headers the addon says its stream URL needs
// (behaviorHints.proxyHeaders.request), bounded and stripped of headers
// Ether controls itself.
func (s *stream) requestHeaders() map[string]string {
	var out map[string]string
	for k, v := range s.BehaviorHints.ProxyHeaders.Request {
		k = http.CanonicalHeaderKey(strings.TrimSpace(k))
		switch k {
		case "", "Range", "Host", "Content-Length", "Connection", "Transfer-Encoding", "Te", "Upgrade", "Proxy-Authorization", "Proxy-Connection":
			continue
		}
		if strings.ContainsAny(k+v, "\r\n\x00") || len(out) == maxStreamHeaders {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// linkIdentity identifies an HTTP or NZB stream within one configured
// addon without its URL, which can carry credentials and often changes
// between answers: the file's name and size, the debrid service the addon
// labels it with, its file index or requested episode, and the link's host.
func linkIdentity(name string, size int64, link string, s *stream, req core.SearchRequest) string {
	host := ""
	if u, err := url.Parse(link); err == nil {
		host = strings.ToLower(u.Host)
	}
	cached, uncached := debridHints(s.Name)
	h := sha1.Sum([]byte(strings.ToLower(name) + "|" + strconv.FormatInt(size, 10) + "|" + host + "|" +
		strings.Join(cached, ",") + "|" + strings.Join(uncached, ",") + "|" + guidSuffix(s, req, false)))
	return hex.EncodeToString(h[:10])
}

// streamLanguages adds the audio languages a stream's text declares
// outside its release name: flag emoji (as Torrentio prints them) and
// "Multi Audio" / "Dual Audio".
func streamLanguages(langs []string, text string) []string {
	add := func(l string) {
		for _, x := range langs {
			if x == l {
				return
			}
		}
		langs = append(langs, l)
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "multi audio") || strings.Contains(lower, "dual audio") {
		add(releaseparse.LanguageMulti)
	}
	for _, l := range releaseparse.FlagLanguages(text) {
		add(l)
	}
	return langs
}

// entryEpisodeIdentity reads an anime release numbered inside the
// database entry the addon was asked for ("Show Final Season Part 2 - 04"
// for that entry's fourth episode) as the requested episode. Only a
// season-less release whose own number is that entry episode is read so;
// every other name keeps its parsed identity.
func entryEpisodeIdentity(info core.ReleaseInfo, req core.SearchRequest, entryEpisode int) core.ReleaseInfo {
	if entryEpisode <= 0 || req.Type != core.TypeEpisode || len(info.Seasons) > 0 || info.FullSeason {
		return info
	}
	if !releaseparse.EpisodeMatchesAbsolute(info, entryEpisode) || entryEpisode == req.AbsoluteEpisode {
		return info
	}
	info.Seasons, info.Episodes = []int{req.Season}, []int{req.Episode}
	return info
}

// FetchNZB implements newznab.Backend for an addon's Usenet streams: it
// downloads the stream's NZB. The addon's own origin is trusted (a
// self-hosted addon on the LAN); any other host, and every redirect hop,
// must be public. Errors name the host only. Whatever the server answers
// is transient (core.ErrSourceUnavailable): a login page or an oversized
// body today says nothing permanent about the release. Only a release
// without a usable NZB link is rejected (core.ErrReleaseRejected).
func (b *Backend) FetchNZB(ctx context.Context, r core.Release, reserve func(context.Context, string) error) ([]byte, error) {
	if r.Protocol != core.ProtocolUsenet || r.NZBURL == "" {
		return nil, fmt.Errorf("stremio: %q has no NZB to fetch: %w", r.Name, core.ErrReleaseRejected)
	}
	link, err := url.Parse(r.NZBURL)
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" {
		return nil, fmt.Errorf("stremio: %q has an invalid NZB link: %w", r.Name, core.ErrReleaseRejected)
	}
	if err := reserve(ctx, "grab"); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(safefetch.WithTrustedOriginFor(ctx, b.origin(), r.NZBURL), http.MethodGet, r.NZBURL, nil)
	if err != nil {
		return nil, fmt.Errorf("stremio: NZB from %s: invalid request", link.Host)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := b.svc.nzb.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("stremio: NZB from %s: %w: %v", link.Host, core.ErrSourceUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("stremio: NZB from %s: HTTP %d: %w", link.Host, resp.StatusCode, core.ErrSourceUnavailable)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNZB+1))
	if err != nil {
		return nil, fmt.Errorf("stremio: NZB from %s: %w: %v", link.Host, core.ErrSourceUnavailable, err)
	}
	if len(body) > maxNZB {
		return nil, fmt.Errorf("stremio: NZB from %s is larger than %d bytes: %w", link.Host, maxNZB, core.ErrSourceUnavailable)
	}
	if !bytes.Contains(bytes.ToLower(body[:min(len(body), 64<<10)]), []byte("<nzb")) {
		return nil, fmt.Errorf("stremio: %s did not answer with an NZB: %w", link.Host, core.ErrSourceUnavailable)
	}
	return body, nil
}
