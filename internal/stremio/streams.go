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
	"github.com/braedonsaunders/ether/internal/egress"
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
	kind, id       string
	packs          bool
	entryEpisode   int
	specialEpisode int
}

// animeDatabases are the anime id prefixes Ether can map TMDB titles to,
// in the order they are tried.
var animeDatabases = []string{animemap.Kitsu, animemap.MAL, animemap.AniList, animemap.AniDB}

// lookupID chooses how to ask the addon for req (LookupStreams). Without
// a readable manifest the addon is asked by IMDb id, as before prefixes
// were read.
func (b *Backend) lookupID(ctx context.Context, req core.SearchRequest) (idLookup, bool) {
	m := b.currentManifest(ctx)
	if m == nil {
		m = &Manifest{Resources: Resources{{Name: ResourceStream, Types: []string{TypeMovie, TypeSeries}, IDPrefixes: []string{"tt"}}}}
	}
	var anime AnimeIDs
	if req.Anime {
		anime = b.svc.animeIDs()
	}
	look, ok := LookupStreams(ctx, *m, req, anime, b.svc.specials)
	return idLookup{kind: look.Type, id: look.ID, packs: look.Packs, entryEpisode: look.EntryEpisode, specialEpisode: look.SpecialEpisode}, ok
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
	return FetchNZB(ctx, b.svc.nzb, b.origin(), r, reserve)
}

// FetchNZB downloads an add-on Usenet stream's NZB with client, trusting
// only the add-on's own origin (see Backend.FetchNZB). Installed add-ons
// (internal/plugins/adapters) fetch their NZBs through it too.
func FetchNZB(ctx context.Context, client *http.Client, origin string, r core.Release, reserve func(context.Context, string) error) ([]byte, error) {
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
	req, err := http.NewRequestWithContext(safefetch.WithTrustedOriginFor(ctx, origin, r.NZBURL), http.MethodGet, r.NZBURL, nil)
	if err != nil {
		return nil, fmt.Errorf("stremio: NZB from %s: invalid request", link.Host)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
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

// DefaultNZBClient is the guarded client add-on NZBs are downloaded with.
func DefaultNZBClient() *http.Client {
	return (&safefetch.Guard{}).Client(egress.Client(egress.CapStremio, nzbTimeout))
}

// AnimeIDs is the service's anime id mapping, loaded the first time an
// add-on needs it; installed add-ons share it (internal/plugins/adapters).
func (s *Service) AnimeIDs() AnimeIDs { return lazyAnime{s} }

// LookupCour maps a multi-cour TMDB season episode to its 1-based cour (part)
// number within that season and the episode's 1-based number inside that cour.
func (s *Service) LookupCour(ctx context.Context, req core.SearchRequest) (int, int, bool, error) {
	if cl, ok := s.animeIDs().(interface {
		LookupCour(context.Context, core.SearchRequest) (int, int, bool, error)
	}); ok {
		return cl.LookupCour(ctx, req)
	}
	return 0, 0, false, nil
}

type lazyAnime struct{ s *Service }

func (l lazyAnime) Lookup(ctx context.Context, req core.SearchRequest, db string) (int64, int, bool, error) {
	return l.s.animeIDs().Lookup(ctx, req, db)
}
