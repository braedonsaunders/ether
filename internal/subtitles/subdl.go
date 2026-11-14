package subtitles

// SubDL (subdl.com) subtitle provider.
//
// Docs: https://subdl.com/api-doc
//   GET https://api.subdl.com/api/v1/subtitles
//     api_key (required) plus one of imdb_id / tmdb_id / film_name.
//     type=movie|tv, season_number, episode_number, year, languages
//     (comma-separated ISO codes), subs_per_page (default 10, max 30),
//     hi=0|1, full_season=0|1, unpack=1, client.
//     Response: {status, results[], subtitles[]} where each subtitle has
//     release_name, name, url ("/subtitle/<n_id>-<file>.zip"), season,
//     episode, hi, full_season, episode_from/end, unpack_files[].
//   Downloads: https://dl.subdl.com + subtitle.url. A zip holds one or
//     more .srt files; unzipSubtitleCandidate picks the matching entry.
//   Limits: free keys get 2,000 requests/day; anonymous downloads are
//     capped at 300/day per IP, paid keys use the account quota. The
//     response carries no Retry-After, so a 429 is a hard failure.
//
// SubDL is keyed on IMDb/TMDB ids and season/episode exactly like
// OpenSubtitles, so it fits the existing Candidate/Rank pipeline and its
// results merge with OpenSubtitles' in searchProviders.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/logredact"
)

const (
	subdlBaseURL     = "https://api.subdl.com/api/v1"
	subdlDownloadURL = "https://dl.subdl.com"
	// subdlClient identifies this integration, as SubDL asks.
	subdlClient = "custom_integration"
	// subdlMaxResults is the API's maximum subs_per_page.
	subdlMaxResults = 30
)

// subdlEnabled reports whether SubDL can search and download.
func (s *Service) subdlEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subdl.Enabled && s.subdl.APIKey != "" && s.dir != ""
}

// subdlQuery builds the SubDL search params for a title + language.
// SubDL filters on base ISO 639-1 codes, so a regional variant is reduced
// to its primary subtag (see subdlQueryLang).
func subdlQuery(title *core.Title, lang string) url.Values {
	q := url.Values{}
	q.Set("client", subdlClient)
	q.Set("subs_per_page", strconv.Itoa(subdlMaxResults))
	q.Set("languages", strings.ToUpper(subdlQueryLang(lang)))
	switch title.Type {
	case core.TypeEpisode:
		q.Set("type", "tv")
		// Episode searches want per-episode files, not season packs.
		q.Set("full_season", "0")
		showTMDB := title.ShowIDs.TMDB
		if showTMDB == 0 && title.Key.Kind() == core.KindEpisode {
			showTMDB = title.Key.TMDB()
		}
		if title.ShowIDs.IMDB != "" {
			q.Set("imdb_id", imdbNumeric(title.ShowIDs.IMDB))
		}
		if showTMDB != 0 {
			q.Set("tmdb_id", strconv.FormatInt(showTMDB, 10))
		} else if title.IDs.TMDB != 0 {
			q.Set("tmdb_id", strconv.FormatInt(title.IDs.TMDB, 10))
		}
		season, episode := title.Season, title.Episode
		if season == 0 && title.Key.Kind() == core.KindEpisode {
			season, episode = title.Key.Season(), title.Key.Episode()
		}
		if season > 0 {
			q.Set("season_number", strconv.Itoa(season))
		}
		if episode > 0 {
			q.Set("episode_number", strconv.Itoa(episode))
		}
	default:
		q.Set("type", "movie")
		tmdb := title.IDs.TMDB
		if tmdb == 0 && title.Key.Kind() == core.KindMovie {
			tmdb = title.Key.TMDB()
		}
		if title.IDs.IMDB != "" {
			q.Set("imdb_id", imdbNumeric(title.IDs.IMDB))
		}
		if tmdb != 0 {
			q.Set("tmdb_id", strconv.FormatInt(tmdb, 10))
		}
	}
	return q
}

// subdlQueryLang maps a normalised language to the code SubDL expects.
// SubDL filters on base ISO 639-1 codes, so a regional variant (pt-br,
// zh-cn) is reduced to its primary subtag.
func subdlQueryLang(l string) string {
	n := langtag.Normalize(l)
	if base, _, ok := strings.Cut(n, "-"); ok {
		return base
	}
	return n
}

type subdlResponse struct {
	Status    bool   `json:"status"`
	Error     string `json:"error"`
	Subtitles []struct {
		ReleaseName string `json:"release_name"`
		Name        string `json:"name"`
		URL         string `json:"url"`
		HI          bool   `json:"hi"`
		FullSeason  bool   `json:"full_season"`
	} `json:"subtitles"`
}

// subdlSearchRequest is shared by subtitle searches and the console connection
// test, so both authenticate using the API's required query parameter.
func subdlSearchRequest(ctx context.Context, base, key, agent string, q url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/subtitles", nil)
	if err != nil {
		return nil, logredact.TransportError(err, "subdl", nil)
	}
	params := req.URL.Query()
	for name, values := range q {
		params[name] = append([]string(nil), values...)
	}
	params.Set("api_key", strings.TrimSpace(key))
	req.URL.RawQuery = params.Encode()
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func readSubDLSearch(resp *http.Response) (subdlResponse, error) {
	var sr subdlResponse
	// Do not include provider response bodies in errors: they may echo the
	// authenticated URL. Transport errors must omit that URL as well.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return sr, fmt.Errorf("SubDL rejected the API key")
	}
	if resp.StatusCode != http.StatusOK {
		return sr, fmt.Errorf("subdl search: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return sr, err
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return sr, fmt.Errorf("subdl search decode: %w", err)
	}
	if !sr.Status {
		return sr, fmt.Errorf("subdl search: request rejected")
	}
	return sr, nil
}

// searchSubDL searches SubDL and maps its results onto Candidates. SubDL
// has no moviehash parameter, so release-name matching carries ranking.
func (s *Service) searchSubDL(ctx context.Context, title *core.Title, lang string) ([]Candidate, error) {
	if title == nil {
		return nil, fmt.Errorf("nil title")
	}
	q := subdlQuery(title, lang)
	cacheKey := q.Encode()
	s.mu.Lock()
	until, missed := s.negative[cacheKey]
	if missed && !s.now().Before(until) {
		delete(s.negative, cacheKey)
		missed = false
	}
	s.mu.Unlock()
	if missed {
		return nil, nil
	}
	s.mu.Lock()
	key := s.subdl.APIKey
	s.mu.Unlock()
	resp, err := s.send(ctx, func() (*http.Request, error) {
		return subdlSearchRequest(ctx, s.subdlBase, key, s.agent, q)
	})
	if err != nil {
		return nil, logredact.TransportError(err, "subdl", nil)
	}
	defer resp.Body.Close()
	sr, err := readSubDLSearch(resp)
	if err != nil {
		return nil, err
	}
	lang = langtag.Normalize(lang)
	var out []Candidate
	for _, d := range sr.Subtitles {
		if d.URL == "" {
			continue
		}
		// A full-season pack for an episode search would need per-file
		// unpacking; keep it only when no season/episode was requested.
		if d.FullSeason && (title.Type == core.TypeEpisode) {
			continue
		}
		release := d.ReleaseName
		if release == "" {
			release = strings.TrimSuffix(d.Name, ".zip")
		}
		out = append(out, Candidate{
			FileID:     subdlFileID(d.URL),
			FileName:   strings.TrimSuffix(d.Name, ".zip"),
			Language:   lang,
			Release:    release,
			HearingImp: d.HI,
			// SubDL has no uploader-rank or download-count signal; the
			// release-name token overlap and HI penalty in Rank carry
			// the ordering.
			Provider:    "SubDL",
			ProviderID:  config.SubtitleProviderSubDL,
			DownloadURL: s.subdlDownload + d.URL,
		})
	}
	if len(out) == 0 && ctx.Err() == nil {
		s.mu.Lock()
		now := s.now()
		for k, until := range s.negative {
			if !now.Before(until) {
				delete(s.negative, k)
			}
		}
		if len(s.negative) >= 4096 {
			var oldest string
			var expiry = now
			for k, until := range s.negative {
				if until.Before(expiry) {
					oldest, expiry = k, until
				}
			}
			delete(s.negative, oldest)
		}
		s.negative[cacheKey] = now.Add(s.negTTL)
		s.mu.Unlock()
	}
	return out, nil
}

// subdlFileID derives a stable numeric id from a SubDL download path so a
// candidate can be remembered as an offer and re-picked. The path is
// "/subtitle/<n_id>-<file>.zip"; both halves are hashed.
func subdlFileID(path string) int64 {
	p := strings.TrimPrefix(strings.TrimSuffix(path, ".zip"), "/subtitle/")
	var h uint64 = 1469598103934665603
	for i := 0; i < len(p); i++ {
		h ^= uint64(p[i])
		h *= 1099511628211
	}
	if h == 0 {
		return 1
	}
	return int64(h & 0x7fffffffffffffff)
}

// downloadSubDL fetches and unzips a SubDL candidate. Paid keys may
// authenticate the download link with the API key; the key is attached so
// the account quota applies instead of the anonymous per-IP cap.
func (s *Service) downloadSubDL(ctx context.Context, c Candidate) ([]byte, error) {
	if c.DownloadURL == "" {
		return nil, fmt.Errorf("subdl: no download url")
	}
	s.mu.Lock()
	key := s.subdl.APIKey
	s.mu.Unlock()
	link, err := url.Parse(c.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("subdl: invalid download url")
	}
	if key = strings.TrimSpace(key); key != "" {
		q := link.Query()
		q.Set("api_key", key)
		link.RawQuery = q.Encode()
	}
	resp, err := s.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
		if err != nil {
			return nil, logredact.TransportError(err, "subdl", nil)
		}
		req.Header.Set("User-Agent", s.agent)
		return req, nil
	})
	if err != nil {
		return nil, logredact.TransportError(err, "subdl", nil)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil {
		return nil, logredact.TransportError(err, "subdl", nil)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subdl download: HTTP %d", resp.StatusCode)
	}
	if len(raw) > maxSubtitleBytes {
		return nil, fmt.Errorf("subtitle download too large")
	}
	return unzipSubtitleCandidate(raw, c)
}
