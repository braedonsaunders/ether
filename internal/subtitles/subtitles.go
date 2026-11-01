// Package subtitles fetches missing subtitle languages from OpenSubtitles
// and serves them as external (sidecar) tracks.
//
// Flow: Sidecars is called with the tracks embedded in the chosen release;
// for each of the user's preferred languages not already embedded it
// searches OpenSubtitles, ranks candidates (trusted uploaders, download
// count, release-name match — a moviehash match is impossible before the
// file exists), downloads the best, normalises it to UTF-8 SRT + WebVTT and
// caches both on disk. The returned core.StreamInfo carries an ExternalURL
// of the form "ether-subtitle:<id>"; the Plex front end resolves it with
// Open / OpenFormat.
package subtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/flight"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// URLPrefix prefixes the ExternalURL of sidecar streams from this package.
const URLPrefix = "ether-subtitle:"

// ErrQuota is returned when OpenSubtitles refuses a download because the
// account's (or anonymous IP's) daily download quota is used up.
var ErrQuota = errors.New("opensubtitles: download quota exhausted")

const defaultBaseURL = "https://api.opensubtitles.com/api/v1"

// Options construct a Service. Config is a snapshot; call UpdateConfig on
// config changes if desired.
type Options struct {
	Config     config.OpenSubtitles
	CacheDir   string // data dir subfolder for downloaded subtitles
	BaseURL    string // override for tests
	HTTPClient *http.Client
	Logger     *slog.Logger
	UserAgent  string // OpenSubtitles requires an app User-Agent
	// NegativeTTL is how long "no subtitle found" for a title+language is
	// remembered before searching again (default 6h).
	NegativeTTL time.Duration
	// MaxRetryAfter caps a 429 Retry-After wait (default 3s).
	MaxRetryAfter time.Duration
	Now           func() time.Time
	// AlignLanguages are tried in order for dialogue timeline alignment
	// (default ["en"]: the most widely subtitled language).
	AlignLanguages []string
}

// Service is the OpenSubtitles client + disk cache.
type Service struct {
	cfg     config.OpenSubtitles
	dir     string
	base    string
	http    *http.Client
	log     *slog.Logger
	agent   string
	negTTL  time.Duration
	maxWait time.Duration
	now     func() time.Time

	mu        sync.Mutex
	token     string // login token, cached
	flights   flight.Group[string, string]
	negative  map[string]time.Time // title+lang -> when nothing was found
	quotaTill time.Time            // downloads refused until then

	// pickMu serialises writes of a title's picked subtitle: concurrent
	// picks of one file rename onto the same path, which Windows refuses
	// while another rename of it is in progress.
	pickMu sync.Mutex

	alignLanguages []string
	aligner        *Aligner
	alignOnce      sync.Once

	// Viewer-driven search results awaiting a pick (see ondemand.go).
	offersMu sync.Mutex
	offers   map[offerKey]offer

	correctFlights flightGroup
	speechSem      chan struct{}
}

// New builds a Service. A disabled service returns no sidecars.
func New(opts Options) *Service {
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.Client(egress.CapMetadata, 30*time.Second)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Ether v1"
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = 6 * time.Hour
	}
	if opts.MaxRetryAfter <= 0 {
		opts.MaxRetryAfter = 3 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	alignLangs := []string{"en"}
	if len(opts.AlignLanguages) > 0 {
		alignLangs = alignLangs[:0]
		seenLang := map[string]bool{}
		for _, l := range opts.AlignLanguages {
			if n := langtag.Normalize(l); n != "" && !seenLang[n] && len(alignLangs) < 3 {
				seenLang[n] = true
				alignLangs = append(alignLangs, n)
			}
		}
		if len(alignLangs) == 0 {
			alignLangs = []string{"en"}
		}
	}
	return &Service{
		cfg: opts.Config, dir: opts.CacheDir, base: strings.TrimSuffix(opts.BaseURL, "/"),
		http: opts.HTTPClient, log: opts.Logger, agent: opts.UserAgent,
		negative: map[string]time.Time{}, negTTL: opts.NegativeTTL, maxWait: opts.MaxRetryAfter, now: opts.Now,
		alignLanguages: alignLangs,
		speechSem:      make(chan struct{}, 2),
	}
}

// send performs a request built by mk, retrying once after a 429 (honouring
// Retry-After up to MaxRetryAfter). mk is called per attempt so bodies can
// be re-read.
func (s *Service) send(ctx context.Context, mk func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := mk()
		if err != nil {
			return nil, err
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt > 0 {
			return resp, nil
		}
		wait, ok := retryafter.Parse(resp.Header.Get("Retry-After"), time.Now(), s.maxWait)
		if !ok {
			wait = min(time.Second, s.maxWait)
		}
		resp.Body.Close()
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
}

// UpdateConfig swaps the OpenSubtitles settings (e.g. from config.Store.Subscribe).
func (s *Service) UpdateConfig(cfg config.OpenSubtitles) {
	s.mu.Lock()
	s.cfg = cfg
	s.token = ""
	s.mu.Unlock()
}

// Enabled reports whether sidecar fetching is configured.
func (s *Service) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Without a cache dir there is nowhere to serve sidecars from.
	return s.cfg.Enabled && s.cfg.APIKey != "" && s.dir != ""
}

// Candidate is one OpenSubtitles search result.
type Candidate struct {
	FileID        int64
	FileName      string
	Language      string
	Release       string
	DownloadCount int
	Ratings       float64
	Uploader      string
	UploaderRank  string
	HearingImp    bool
	AITranslated  bool
	HashMatch     bool
	// Forced marks a real forced track: OpenSubtitles'
	// foreign_parts_only and NOT hearing_impaired (a HI
	// foreign-parts track is SDH, not forced; HearingImp
	// carries that signal separately).
	Forced bool
	Score  float64 // filled by Rank
}

type searchResponse struct {
	Data []struct {
		Attributes struct {
			HashMatch     bool    `json:"moviehash_match"`
			Language      string  `json:"language"`
			DownloadCount int     `json:"download_count"`
			Ratings       float64 `json:"ratings"`
			Release       string  `json:"release"`
			HearingImp    bool    `json:"hearing_impaired"`
			AITranslated  bool    `json:"ai_translated"`
			ForeignOnly   bool    `json:"foreign_parts_only"`
			Uploader      struct {
				Name string `json:"name"`
				Rank string `json:"rank"`
			} `json:"uploader"`
			Files []struct {
				FileID   int64  `json:"file_id"`
				FileName string `json:"file_name"`
			} `json:"files"`
		} `json:"attributes"`
	} `json:"data"`
}

func imdbNumeric(id string) string {
	s := strings.TrimPrefix(strings.TrimSpace(id), "tt")
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return s
}

// searchQuery builds OpenSubtitles search params for a title + language.
func searchQuery(title *core.Title, lang string) url.Values {
	q := url.Values{}
	q.Set("languages", queryLang(lang))
	q.Set("order_by", "download_count")
	q.Set("order_direction", "desc")
	switch title.Type {
	case core.TypeEpisode:
		showTMDB := title.ShowIDs.TMDB
		if showTMDB == 0 && title.Key.Kind() == core.KindEpisode {
			showTMDB = title.Key.TMDB() // episode keys encode the show's TMDB id
		}
		if title.ShowIDs.IMDB != "" {
			q.Set("parent_imdb_id", imdbNumeric(title.ShowIDs.IMDB))
		}
		if showTMDB != 0 {
			q.Set("parent_tmdb_id", strconv.FormatInt(showTMDB, 10))
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
		q.Set("type", "episode")
	default:
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
		q.Set("type", "movie")
	}
	return q
}

// Search returns ranked candidates for a title and language.
func (s *Service) Search(ctx context.Context, title *core.Title, lang, releaseName string) ([]Candidate, error) {
	return s.SearchWithHash(ctx, title, lang, releaseName, "")
}

// SearchWithHash prefers exact cached-source matches; empty hash uses release ranking.
func (s *Service) SearchWithHash(ctx context.Context, title *core.Title, lang, releaseName, movieHash string) ([]Candidate, error) {
	if title == nil {
		return nil, fmt.Errorf("nil title")
	}
	// OpenSubtitles redirects unless parameters are sorted and lower-case;
	// url.Values.Encode sorts keys.
	q := searchQuery(title, lang)
	if movieHash != "" {
		if len(movieHash) != 16 {
			return nil, fmt.Errorf("invalid moviehash")
		}
		for _, c := range movieHash {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return nil, fmt.Errorf("invalid moviehash")
			}
		}
		q.Set("moviehash", movieHash)
	}
	u := s.base + "/subtitles?" + q.Encode()
	resp, err := s.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		s.setHeaders(req, false)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opensubtitles search: %s: %s", resp.Status, truncate(string(body), 200))
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("opensubtitles search decode: %w", err)
	}
	var out []Candidate
	for _, d := range sr.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		if lang != "" && a.Language != "" && langtag.MatchTier(lang, a.Language) == 0 {
			continue // the API matched a different language variant
		}
		out = append(out, Candidate{
			HashMatch: a.HashMatch, FileID: a.Files[0].FileID, FileName: a.Files[0].FileName, Language: a.Language,
			Release: a.Release, DownloadCount: a.DownloadCount, Ratings: a.Ratings,
			Uploader: a.Uploader.Name, UploaderRank: a.Uploader.Rank,
			HearingImp: a.HearingImp, AITranslated: a.AITranslated, Forced: a.ForeignOnly && !a.HearingImp,
		})
	}
	Rank(out, releaseName)
	return out, nil
}

var tokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range tokenSplit.Split(strings.ToLower(s), -1) {
		if len(p) > 1 {
			out[p] = true
		}
	}
	return out
}

// Rank scores candidates in place, best first. There is no file yet, so no
// moviehash: download count (log-scaled), trusted uploaders, ratings and
// release-name token overlap carry the signal; AI-translated and
// hearing-impaired tracks are penalised (HI only slightly — sometimes it is
// the only option).
func Rank(cands []Candidate, releaseName string) {
	want := tokens(releaseName)
	for i := range cands {
		c := &cands[i]
		score := math.Log10(float64(c.DownloadCount)+1) * 10
		score += c.Ratings // 0-10
		rank := strings.ToLower(c.UploaderRank)
		switch {
		case strings.Contains(rank, "trusted") || strings.Contains(rank, "platinum") || strings.Contains(rank, "administrator"):
			score += 15
		case strings.Contains(rank, "gold"):
			score += 10
		case strings.Contains(rank, "silver") || strings.Contains(rank, "vip"):
			score += 6
		case strings.Contains(rank, "bronze"):
			score += 3
		}
		if len(want) > 0 {
			have := tokens(c.Release + " " + c.FileName)
			hit := 0
			for w := range want {
				if have[w] {
					hit++
				}
			}
			score += 30 * float64(hit) / float64(len(want))
		}
		if c.AITranslated {
			score -= 20
		}
		if c.HearingImp {
			score -= 2
		}
		if HasMojibake(c.FileName) || HasMojibake(c.Release) {
			score -= 8
		}
		if c.HashMatch {
			score += 1000
		}
		c.Score = score
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].FileID < cands[j].FileID
	})
}

func (s *Service) setHeaders(req *http.Request, withToken bool) {
	s.mu.Lock()
	key, tok := s.cfg.APIKey, s.token
	s.mu.Unlock()
	req.Header.Set("Api-Key", key)
	req.Header.Set("User-Agent", s.agent)
	req.Header.Set("Accept", "application/json")
	if withToken && tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// login exchanges the optional account credentials for a download token
// (logged-in users get a larger daily download quota).
func (s *Service) login(ctx context.Context) error {
	s.mu.Lock()
	user, pass, tok := s.cfg.Username, s.cfg.Password, s.token
	s.mu.Unlock()
	if tok != "" || user == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := s.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/login", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		s.setHeaders(req, false)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opensubtitles login: %s", resp.Status)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &lr); err != nil || lr.Token == "" {
		return fmt.Errorf("opensubtitles login: no token")
	}
	s.mu.Lock()
	s.token = lr.Token
	s.mu.Unlock()
	return nil
}

// requestLink asks /download for a temporary file link. A 401 with a cached
// login token drops the token and retries once (tokens expire).
func (s *Service) requestLink(ctx context.Context, c Candidate) (string, error) {
	s.mu.Lock()
	quota := s.quotaTill
	s.mu.Unlock()
	if s.now().Before(quota) {
		return "", ErrQuota
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.login(ctx); err != nil {
			// Anonymous downloads may still work within the IP quota.
			s.log.Debug("opensubtitles login failed", "err", err)
		}
		body, _ := json.Marshal(map[string]any{"file_id": c.FileID, "sub_format": "srt"})
		resp, err := s.send(ctx, func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/download", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			s.setHeaders(req, true)
			req.Header.Set("Content-Type", "application/json")
			return req, nil
		})
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			s.mu.Lock()
			had := s.token != ""
			s.token = ""
			s.mu.Unlock()
			if had {
				continue
			}
			return "", fmt.Errorf("opensubtitles download: %s", resp.Status)
		case resp.StatusCode == http.StatusNotAcceptable:
			// Quota exhausted; the body says when it resets.
			var qr struct {
				ResetTimeUTC time.Time `json:"reset_time_utc"`
			}
			_ = json.Unmarshal(b, &qr)
			till := qr.ResetTimeUTC
			if till.IsZero() || till.Before(s.now()) {
				till = s.now().Add(time.Hour)
			}
			s.mu.Lock()
			s.quotaTill = till
			s.mu.Unlock()
			s.log.Warn("opensubtitles download quota exhausted", "resets", till.UTC().Format(time.RFC3339))
			return "", ErrQuota
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("opensubtitles download: %s: %s", resp.Status, truncate(string(b), 200))
		}
		var dr struct {
			Link      string `json:"link"`
			Remaining int    `json:"remaining"`
		}
		if err := json.Unmarshal(b, &dr); err != nil || dr.Link == "" {
			return "", fmt.Errorf("opensubtitles download: no link")
		}
		s.log.Debug("opensubtitles download link", "file", c.FileID, "remaining", dr.Remaining)
		return dr.Link, nil
	}
	return "", fmt.Errorf("opensubtitles download: unauthorized")
}

// download fetches the raw subtitle file bytes for a candidate.
func (s *Service) download(ctx context.Context, c Candidate) ([]byte, error) {
	link, err := s.requestLink(ctx, c)
	if err != nil {
		return nil, err
	}
	resp, err := s.send(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", s.agent)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subtitle file fetch: %s", resp.Status)
	}
	if len(raw) > maxSubtitleBytes {
		return nil, fmt.Errorf("subtitle download too large")
	}
	return unzipSubtitleCandidate(raw, c)
}

// maxSubtitleBytes bounds one subtitle entry/file; anything larger is an
// explicit error, never a silently truncated subtitle.
const maxSubtitleBytes = 16 << 20

// zipJunk reports macOS archive noise that must never be picked as the
// subtitle: __MACOSX/ resource forks, ._* AppleDouble files, .DS_Store
// and directory entries.
func zipJunk(f *zip.File) bool {
	if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
		return true
	}
	for _, seg := range strings.Split(f.Name, "/") {
		if seg == "__MACOSX" {
			return true
		}
	}
	base := path.Base(f.Name)
	return base == ".DS_Store" || strings.HasPrefix(base, "._")
}

// Aggregate budget for trying zip entries: a zip bomb must not cost
// dozens of seconds of decompression/parsing, so trying stops after
// maxZipTriedEntries entries or maxZipExpandedBytes expanded in total,
// whichever comes first.
const (
	maxZipExpandedBytes = 32 << 20
	maxZipTriedEntries  = 16
)

// zipEntryHints recognises only suffix labels, avoiding language codes in titles.
// Unlabelled entries inherit the provider's candidate metadata. Explicit labels
// conflicting with it are never returned under that metadata.
func zipEntryHints(name string, c Candidate) (languageRank, styleRank int, compatible bool) {
	base := strings.TrimSuffix(strings.ToLower(path.Base(name)), strings.ToLower(path.Ext(name)))
	parts := strings.FieldsFunc(base, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == ' ' || r == '[' || r == ']' || r == '(' || r == ')'
	})
	forced, sdh, normal := false, false, false
	// Recognise a bounded vocabulary at the end, in either language/style order.
	// "it" is intentionally omitted: it is a common title word.
	codes := map[string]string{"en": "en", "eng": "en", "fr": "fr", "fra": "fr", "fre": "fr", "es": "es", "spa": "es", "de": "de", "deu": "de", "ger": "de", "pt": "pt", "por": "pt", "pob": "pt-br", "ptbr": "pt-br", "ptpt": "pt-pt", "zh": "zh", "zho": "zh", "chi": "zh", "ja": "ja", "jpn": "ja", "ita": "it", "nl": "nl", "nld": "nl", "ru": "ru", "rus": "ru", "ko": "ko", "kor": "ko"}
	foundLanguage := false
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		switch last {
		case "forced":
			forced = true
		case "sdh", "hi", "cc":
			sdh = true
		case "normal", "full":
			normal = true
		default:
			if foundLanguage {
				goto done
			}
			code, ok := codes[last]
			if len(parts) > 2 && (last == "br" || last == "pt" || last == "cn" || last == "tw") {
				prev := parts[len(parts)-2]
				if prev == "pt" || prev == "zh" {
					code, ok = prev+"-"+last, true
					parts = parts[:len(parts)-1]
				}
			}
			if !ok {
				goto done
			}
			tier := langtag.MatchTier(c.Language, code)
			if tier == 0 {
				return 0, 0, false
			}
			languageRank = tier
			foundLanguage = true
		}
		parts = parts[:len(parts)-1]
	}
done:
	if forced && !c.Forced || sdh && !c.HearingImp || normal && (c.Forced || c.HearingImp) || normal && (forced || sdh) {
		return 0, 0, false
	}
	if forced == c.Forced && sdh == c.HearingImp && (forced || sdh || normal) {
		styleRank = 1
	}

	return languageRank, styleRank, true
}

// unzipSubtitle is retained for callers without candidate style metadata.
func unzipSubtitle(raw []byte, lang string) ([]byte, error) {
	return unzipSubtitleCandidate(raw, Candidate{Language: lang})
}

// unzipSubtitleCandidate chooses only entries compatible with known filename
// labels, preferring explicit language and style matches before cue count.
// Expansion and parsing are bounded even when an entry is corrupt.
func unzipSubtitleCandidate(raw []byte, candidate Candidate) ([]byte, error) {
	lang := candidate.Language
	if len(raw) < 4 || raw[0] != 'P' || raw[1] != 'K' {
		return raw, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	type entry struct {
		f                       *zip.File
		prio                    int // 0: srt/vtt, 1: ass/ssa
		languageRank, styleRank int
	}
	var entries []entry
	for _, f := range zr.File {
		if zipJunk(f) {
			continue
		}
		prio := -1
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".srt", ".vtt":
			prio = 0
		case ".ass", ".ssa":
			prio = 1
		}
		if prio >= 0 {
			lr, sr, ok := zipEntryHints(f.Name, candidate)
			if ok {
				entries = append(entries, entry{f: f, prio: prio, languageRank: lr, styleRank: sr})
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].languageRank != entries[j].languageRank {
			return entries[i].languageRank > entries[j].languageRank
		}
		if entries[i].styleRank != entries[j].styleRank {
			return entries[i].styleRank > entries[j].styleRank
		}
		return entries[i].prio < entries[j].prio
	})
	type usable struct {
		data                    []byte
		cues                    int
		languageRank, styleRank int
	}
	var best *usable
	var tooLarge, lastErr error
	expanded, tried := 0, 0
	budgetExhausted := false
	for _, e := range entries {
		if tried >= maxZipTriedEntries || expanded >= maxZipExpandedBytes {
			budgetExhausted = true
			break
		}
		tried++
		if e.f.UncompressedSize64 > maxSubtitleBytes {
			tooLarge = fmt.Errorf("zip subtitle %q too large: %d bytes exceeds %d byte limit", e.f.Name, e.f.UncompressedSize64, maxSubtitleBytes)
			continue
		}
		rc, err := e.f.Open()
		if err != nil {
			lastErr = err
			continue
		}
		remaining := maxZipExpandedBytes - expanded
		limit := min(maxSubtitleBytes+1, remaining)
		data, err := io.ReadAll(io.LimitReader(rc, int64(limit)))
		rc.Close()
		expanded += len(data)
		if len(data) == remaining {
			budgetExhausted = true
			break
		}
		if err != nil {
			lastErr = err
			continue
		}
		if len(data) > maxSubtitleBytes {
			tooLarge = fmt.Errorf("zip subtitle %q too large: exceeds %d byte limit", e.f.Name, maxSubtitleBytes)
			continue
		}
		srt, _, err := NormalizeLang(data, lang)
		if err != nil {
			lastErr = err
		} else if parsed, perr := ParseCues(srt); perr != nil {
			// Count parsed cues from the SRT NormalizeLang rendered
			// (it already dispatches ParseCues/ParseASS), never raw
			// "-->" occurrences: arrows inside dialogue text would
			// inflate the count and crown a stub over a full track.
			lastErr = perr
		} else if len(parsed) == 0 {
			lastErr = fmt.Errorf("zip subtitle %q has no cues", e.f.Name)
		} else if cand := (&usable{data, len(parsed), e.languageRank, e.styleRank}); best == nil ||
			(cand.languageRank > best.languageRank) ||
			(cand.languageRank == best.languageRank && cand.styleRank > best.styleRank) ||
			(cand.languageRank == best.languageRank && cand.styleRank == best.styleRank && cand.cues > best.cues) {
			best = cand
		}
		if expanded >= maxZipExpandedBytes {
			budgetExhausted = true
			break
		}
	}
	if best != nil {
		return best.data, nil
	}
	if budgetExhausted {
		err := fmt.Errorf("zip subtitle budget exhausted after %d entries tried, %d bytes expanded (limits %d entries, %d bytes)", tried, expanded, maxZipTriedEntries, maxZipExpandedBytes)
		if lastErr != nil {
			return nil, fmt.Errorf("%w: %w", err, lastErr)
		}
		return nil, err
	}
	if tooLarge != nil {
		return nil, tooLarge
	}
	return nil, fmt.Errorf("zip contains no usable .srt/.vtt/.ass/.ssa subtitle")
}

func cacheID(title *core.Title, lang string, fileID int64) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%d|%s|%d", int64(title.Key), lang, fileID)))
	return hex.EncodeToString(h[:10])
}

func (s *Service) cached(id, format string) bool {
	if s.dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(s.dir, id+"."+format))
	return err == nil
}

// fetchAndCache downloads + normalises a candidate, single-flighted per id.
// Downloads count against the account's quota, so the work stays with the
// caller that started it.
func (s *Service) fetchAndCache(ctx context.Context, id string, c Candidate, key core.RatingKey) (string, error) {
	if s.dir != "" && s.cached(id, "srt") && s.cached(id, "vtt") {
		return id, nil
	}
	return s.flights.Do(ctx, id, func(ctx context.Context) (string, error) {
		raw, err := s.download(ctx, c)
		if err != nil {
			return "", err
		}
		srt, vtt, err := NormalizeLang(raw, c.Language)
		if err != nil {
			return "", err
		}
		if s.dir != "" {
			if err := os.MkdirAll(s.dir, 0o755); err != nil {
				return "", err
			}
			meta, _ := json.Marshal(map[string]any{"titleKey": key.String(), "fileId": c.FileID, "language": c.Language, "release": c.Release, "fetchedAt": s.now().UTC()})
			// .json first and .srt last: cached() checks srt+vtt, so a crash
			// mid-write never leaves a half-visible entry.
			for _, w := range []struct{ name, data string }{{id + ".json", string(meta)}, {id + ".vtt", vtt}, {id + ".srt", srt}} {
				if err := writeFileAtomic(filepath.Join(s.dir, w.name), []byte(w.data)); err != nil {
					return "", err
				}
			}
		}
		return id, nil
	})
}

// queryLang maps a normalised language to the code OpenSubtitles expects in
// the languages parameter (it splits Portuguese and Chinese by region).
func queryLang(l string) string {
	switch n := langtag.Normalize(l); n {
	case "pt":
		return "pt-pt"
	case "zh":
		return "zh-cn"
	default:
		return n
	}
}

// MissingLanguages returns the user's preferred languages (in preference
// order) that no embedded subtitle track provides.
func MissingLanguages(user *core.User, embedded []core.StreamInfo) []string {
	if user == nil {
		return nil
	}
	var have []string
	for _, st := range embedded {
		if st.Type == "subtitle" && st.Language != "" {
			have = append(have, langtag.Normalize(st.Language))
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range user.SubtitleLanguages {
		n := langtag.Normalize(l)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		// Directional coverage (langtag.MatchTier): a regional embedded
		// track satisfies a generic want (tier 2), but a generic embedded
		// track does not satisfy a regional want (tier 1) — download it.
		covered := false
		for _, h := range have {
			if langtag.MatchTier(n, h) >= 2 {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, n)
		}
	}
	return out
}

// Preferences is the viewer's subtitle mode/style (trackprefs vocabulary)
// for automatic fetching. Empty means no preference: best-ranked wins.
type Preferences struct {
	Mode  string // off, forced-only, foreign-only, always
	Style string // normal, sdh, forced
}

func firstPrefs(prefs []Preferences) Preferences {
	if len(prefs) > 0 {
		return prefs[0]
	}
	return Preferences{}
}

// prefSig identifies a preference set for the negative cache, so a
// "nothing found" for one mode/style never blocks another.
func prefSig(p Preferences) string { return p.Mode + "/" + p.Style }

// selectionSatisfies reports whether a persisted selection may serve as
// the fast path for p: forced-only and style forced need a real forced
// track, style sdh needs an SDH track. Unconstrained prefs accept any.
func selectionSatisfies(sel selection, p Preferences) bool {
	if p.Mode == "forced-only" {
		return sel.Forced
	}
	switch p.Style {
	case "sdh":
		return sel.SDH
	case "forced":
		return sel.Forced
	case "normal":
		return !sel.Forced && !sel.SDH
	}
	return true
}

// Embedded tracks cover a preference only when their kind is suitable.
// Keep language coverage in MissingLanguages so shared language matching remains
// authoritative. SDH is represented in StreamInfo by its title, as in trackprefs.
func missingForPreferences(user *core.User, embedded []core.StreamInfo, p Preferences) []string {
	var suitable []core.StreamInfo
	for _, st := range embedded {
		title := strings.ToLower(st.Title)
		hi := strings.Contains(title, "sdh") || strings.Contains(title, "hearing impaired") || strings.Contains(title, "[cc]") || title == "cc"
		if selectionSatisfies(selection{Forced: st.Forced, SDH: hi}, p) {
			suitable = append(suitable, st)
		}
	}
	return MissingLanguages(user, suitable)
}

// candidatesForFetch applies Preferences to ranked candidates: forced-only
// requires a real forced track (never a full or HI track), style sdh/forced
// prefers matching tracks via a stable partition that keeps Rank order
// inside each group. The input slice is not mutated.
func candidatesForFetch(cands []Candidate, p Preferences) []Candidate {
	if p.Mode == "forced-only" {
		out := make([]Candidate, 0, len(cands))
		for _, c := range cands {
			if c.Forced {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return nil
		}
		cands = out
	}
	var match func(Candidate) bool
	switch p.Style {
	case "sdh":
		match = func(c Candidate) bool { return c.HearingImp }
	case "forced":
		match = func(c Candidate) bool { return c.Forced }
	case "normal":
		match = func(c Candidate) bool { return !c.Forced && !c.HearingImp }
	default:
		return cands
	}
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if match(c) {
			out = append(out, c)
		}
	}
	for _, c := range cands {
		if !match(c) {
			out = append(out, c)
		}
	}
	return out
}

// Sidecars returns external subtitle streams already cached for the
// user's preferred languages missing from the embedded tracks. It never
// searches or downloads; a file is fetched only when the viewer picks
// one (DownloadOnDemand). It returns (nil, nil) when the service is
// disabled or nothing cached is missing.
func (s *Service) Sidecars(ctx context.Context, title *core.Title, user *core.User, embedded []core.StreamInfo, prefs ...Preferences) ([]core.StreamInfo, error) {
	return s.SidecarsForRelease(ctx, title, user, embedded, "", prefs...)
}

// selection is the persisted choice for one title+language+release+preference, so
// later playbacks skip the search and download entirely.
type selection struct {
	ID       string    `json:"id"`
	FileID   int64     `json:"fileId"`
	Language string    `json:"language"`
	Release  string    `json:"release"`
	At       time.Time `json:"at"`
	// Forced is set for a real forced track (foreign-parts-only, not HI).
	Forced bool `json:"forced,omitempty"`
	// SDH marks a hearing-impaired track; it is surfaced in the sidecar
	// title so trackprefs' sdh() detection can match it. Selections
	// persisted before this field existed load with SDH=false and keep
	// serving as non-SDH until re-downloaded — SDH is never invented
	// from a release name.
	SDH        bool   `json:"sdh,omitempty"`
	Preference string `json:"preference,omitempty"`
}

func releaseSig(release string) string {
	toks := tokens(release)
	if len(toks) == 0 {
		return "any"
	}
	words := make([]string, 0, len(toks))
	for w := range toks {
		words = append(words, w)
	}
	sort.Strings(words)
	h := sha1.Sum([]byte(strings.Join(words, " ")))
	return hex.EncodeToString(h[:6])
}

func (s *Service) indexPath(title *core.Title, lang, release string, prefs ...Preferences) string {
	sig := releaseSig(release)
	if p := firstPrefs(prefs); p != (Preferences{}) {
		h := sha1.Sum([]byte(prefSig(p)))
		sig += "-" + hex.EncodeToString(h[:8])
	}
	return filepath.Join(s.dir, "index", fmt.Sprintf("%d.%s.%s.json", int64(title.Key), lang, sig))
}

func (s *Service) loadSelection(title *core.Title, lang, release string, prefs ...Preferences) (selection, bool) {
	if s.dir == "" {
		return selection{}, false
	}
	b, err := os.ReadFile(s.indexPath(title, lang, release, prefs...))
	if err != nil {
		return selection{}, false
	}
	var sel selection
	if json.Unmarshal(b, &sel) != nil || sel.ID == "" || !s.cached(sel.ID, "srt") || !s.cached(sel.ID, "vtt") {
		return selection{}, false
	}
	return sel, true
}

func (s *Service) saveSelection(title *core.Title, lang, release string, sel selection, prefs ...Preferences) {
	if s.dir == "" {
		return
	}
	p := s.indexPath(title, lang, release, prefs...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.log.Debug("subtitle index dir", "err", err)
		return
	}
	b, _ := json.Marshal(sel)
	if err := writeFileAtomic(p, b); err != nil {
		s.log.Debug("subtitle index write", "err", err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func negKey(title *core.Title, lang, release string, p Preferences) string {
	return fmt.Sprintf("%d|%s|%s|%s", int64(title.Key), lang, releaseSig(release), prefSig(p))
}

func sidecarTitle(lang, release string) string {
	if release != "" {
		return fmt.Sprintf("%s (OpenSubtitles: %s)", strings.ToUpper(lang), release)
	}
	return strings.ToUpper(lang) + " (OpenSubtitles)"
}

// SidecarsForRelease is Sidecars with the chosen release's name, so a
// selection remembered for that release is the one listed. It does not
// contact OpenSubtitles.
func (s *Service) SidecarsForRelease(ctx context.Context, title *core.Title, user *core.User, embedded []core.StreamInfo, releaseName string, prefs ...Preferences) ([]core.StreamInfo, error) {
	return s.SidecarsWithHash(ctx, title, user, embedded, releaseName, "", prefs...)
}

// SidecarsWithHash uses an already-computed cached-source hash for exact release matching.
func (s *Service) SidecarsWithHash(ctx context.Context, title *core.Title, user *core.User, embedded []core.StreamInfo, releaseName, movieHash string, prefs ...Preferences) ([]core.StreamInfo, error) {
	pref := firstPrefs(prefs)
	cacheRelease := releaseName
	if movieHash != "" {
		cacheRelease += " hash-" + movieHash
	}
	if title == nil {
		return nil, fmt.Errorf("nil title")
	}
	if !s.Enabled() {
		return nil, nil
	}
	// Tracks a viewer picked through Plex's subtitle search win over
	// automatic choices, and are listed even when the release embeds
	// that language (the viewer asked for this file).
	manual := s.manualSelections(title.Key)
	missing := missingForPreferences(user, embedded, pref)
	if len(missing) == 0 && len(manual) == 0 {
		return nil, nil
	}
	nextIndex := 0
	for _, st := range embedded {
		if st.Index >= nextIndex {
			nextIndex = st.Index + 1
		}
	}
	var out []core.StreamInfo
	add := func(lang string, sel selection) {
		out = append(out, sidecarStream(nextIndex, lang, sel))
		nextIndex++
	}
	for _, lang := range missing {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if sel, ok := manual[lang]; ok {
			add(lang, sel)
			delete(manual, lang)
			continue
		}
		// A remembered selection for this release, when it still matches
		// the requested preference. Nothing is downloaded to fill a gap.
		if sel, ok := s.loadSelection(title, lang, cacheRelease, pref); ok && (sel.Preference == prefSig(pref) || selectionSatisfies(sel, pref)) {
			add(lang, sel)
		}
	}
	for _, lang := range sortedLanguages(manual) {
		add(lang, manual[lang])
	}
	return out, nil
}

// Open resolves a sidecar id (or a full ExternalURL) to its SRT content.
// It returns the reader and the format ("srt").
func (s *Service) Open(id string) (io.ReadCloser, string, error) {
	return s.OpenFormat(id, "srt")
}

// OpenFormat resolves a sidecar id in the requested format ("srt" or "vtt").
func (s *Service) OpenFormat(id, format string) (io.ReadCloser, string, error) {
	id = strings.TrimPrefix(id, URLPrefix)
	format = strings.ToLower(strings.TrimPrefix(format, "."))
	if format != "srt" && format != "vtt" {
		return nil, "", fmt.Errorf("unsupported subtitle format %q", format)
	}
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return nil, "", core.ErrNotFound
	}
	return s.OpenFormatFrom(id, format, "")
}

// OpenFormatFrom is OpenFormat applying a cached fit only for this
// release/source. An empty source serves the original sidecar.
func (s *Service) OpenFormatFrom(id, format, source string) (io.ReadCloser, string, error) {
	id = strings.TrimPrefix(id, URLPrefix)
	format = strings.ToLower(strings.TrimPrefix(format, "."))
	if format != "srt" && format != "vtt" {
		return nil, "", fmt.Errorf("unsupported subtitle format %q", format)
	}
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return nil, "", core.ErrNotFound
	}
	if s.dir == "" {
		return nil, "", core.ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.dir, id+"."+format))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", core.ErrNotFound
		}
		return nil, "", err
	}
	if source == "" {
		return f, format, nil
	}
	if corr, ok := s.loadCorrection(id, source); ok && corr.Applied {
		raw, err := io.ReadAll(io.LimitReader(f, 32<<20))
		f.Close()
		if err != nil {
			return nil, "", err
		}
		cues, err := ParseCues(string(raw))
		if err != nil {
			return io.NopCloser(strings.NewReader(string(raw))), format, nil
		}
		cues = applyCorrection(cues, corr)
		text := RenderSRT(cues)
		if format == "vtt" {
			text = RenderVTT(cues)
		}
		return io.NopCloser(strings.NewReader(text)), format, nil
	}
	return f, format, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
