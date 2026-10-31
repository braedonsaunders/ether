// Package animemap maps TMDB titles to the anime databases (Kitsu,
// MyAnimeList, AniList, AniDB) whose ids anime-only Stremio addons look
// titles up by.
//
// The mapping is the community anime-lists dataset (Fribb/anime-lists on
// GitHub), read at runtime only when an addon needs it. Ether ships no copy.
// The dataset is untrusted input: the download is size- and time-bounded,
// failures back off, and any ambiguity yields no mapping rather than a
// guess, so callers keep their IMDb lookups and simply skip the addon.
package animemap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// DefaultURL is the dataset Ether reads by default.
const DefaultURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"

// Databases an anime addon may look titles up by (Stremio id prefixes).
const (
	Kitsu   = "kitsu"
	MAL     = "mal"
	AniList = "anilist"
	AniDB   = "anidb"
)

const (
	defaultMaxBytes = 32 << 20 // the dataset is ~6 MB
	defaultTimeout  = 90 * time.Second
	defaultTTL      = 24 * time.Hour
	failureBackoff  = time.Hour
)

// ErrUnavailable reports that the dataset could not be loaded (or is
// backing off after a failure). Callers treat it as "no mapping".
var ErrUnavailable = errors.New("animemap: anime id mapping unavailable")

// Options configure a Service.
type Options struct {
	URL      string       // "" = DefaultURL
	Client   *http.Client // nil = a public-only safefetch client
	Logger   *slog.Logger
	Now      func() time.Time
	TTL      time.Duration // 0 = 24h
	MaxBytes int64         // 0 = 32 MiB
	Timeout  time.Duration // 0 = 90s
}

// Service loads the dataset lazily and answers lookups from memory.
type Service struct {
	opts Options

	mu       sync.Mutex
	idx      *index
	loadedAt time.Time
	failedAt time.Time
	loading  chan struct{}
}

// New returns a Service; nothing is fetched until the first lookup.
func New(opts Options) *Service {
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	if opts.Client == nil {
		opts.Client = (&safefetch.Guard{}).Client(&http.Client{Transport: egress.Transport(egress.CapMetadata)})
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	return &Service{opts: opts}
}

// entry is one anime database entry (a season, cour, film or special).
type entry struct {
	ids     map[string]int64
	kind    string // TV, ONA, OVA, MOVIE, SPECIAL
	season  int    // TMDB season; -1 when the entry names none
	offset  int    // TMDB episodes before the entry's first episode
	movieOf []int64
}

type index struct {
	shows  map[int64][]entry // TMDB tv id
	movies map[int64][]entry // TMDB movie id
}

// rawEntry is the part of a dataset entry Ether reads. Numbers may come
// as numbers or numeric strings; TMDB movie ids as a number or a list.
type rawEntry struct {
	Type    string          `json:"type"`
	Kitsu   json.Number     `json:"kitsu_id"`
	MAL     json.Number     `json:"mal_id"`
	AniList json.Number     `json:"anilist_id"`
	AniDB   json.Number     `json:"anidb_id"`
	TMDB    json.RawMessage `json:"themoviedb_id"`
	Season  struct {
		TMDB *int `json:"tmdb"`
	} `json:"season"`
	Offset struct {
		TMDB *int `json:"tmdb"`
	} `json:"episode_offset"`
}

func parse(r io.Reader) (*index, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, fmt.Errorf("animemap: dataset is not a JSON array")
	}
	idx := &index{shows: map[int64][]entry{}, movies: map[int64][]entry{}}
	for dec.More() {
		var raw rawEntry
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("animemap: bad dataset entry: %w", err)
		}
		e := entry{ids: map[string]int64{}, kind: raw.Type, season: -1}
		for db, n := range map[string]json.Number{Kitsu: raw.Kitsu, MAL: raw.MAL, AniList: raw.AniList, AniDB: raw.AniDB} {
			if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil && v > 0 {
				e.ids[db] = v
			}
		}
		if len(e.ids) == 0 || len(raw.TMDB) == 0 {
			continue
		}
		var tmdb struct {
			TV    json.RawMessage `json:"tv"`
			Movie json.RawMessage `json:"movie"`
		}
		if json.Unmarshal(raw.TMDB, &tmdb) != nil {
			continue
		}
		if raw.Season.TMDB != nil && *raw.Season.TMDB >= 0 {
			e.season = *raw.Season.TMDB
		}
		if raw.Offset.TMDB != nil && *raw.Offset.TMDB > 0 {
			e.offset = *raw.Offset.TMDB
		}
		for _, tv := range numbers(tmdb.TV) {
			idx.shows[tv] = append(idx.shows[tv], e)
		}
		for _, m := range numbers(tmdb.Movie) {
			idx.movies[m] = append(idx.movies[m], e)
		}
	}
	return idx, nil
}

// numbers reads a JSON number, numeric string or list of either.
func numbers(raw json.RawMessage) []int64 {
	if len(raw) == 0 {
		return nil
	}
	var list []json.Number
	if json.Unmarshal(raw, &list) != nil {
		var one json.Number
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []json.Number{one}
	}
	var out []int64
	for _, n := range list {
		if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// Lookup maps a search request to db's id and, for an episode, the
// episode's number inside that entry. ok is false when the request has no
// unambiguous mapping. err is non-nil only when the dataset is unavailable.
func (s *Service) Lookup(ctx context.Context, req core.SearchRequest, db string) (id int64, episode int, ok bool, err error) {
	idx, err := s.load(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	id, episode, ok = idx.lookup(req, db)
	return id, episode, ok, nil
}

func (idx *index) lookup(req core.SearchRequest, db string) (int64, int, bool) {
	if req.IDs.TMDB <= 0 {
		return 0, 0, false
	}
	switch req.Type {
	case core.TypeMovie:
		return single(idx.movies[req.IDs.TMDB], db)
	case core.TypeEpisode:
		if req.Season <= 0 || req.Episode <= 0 {
			return 0, 0, false // specials and whole seasons have no mapping
		}
		return idx.episode(idx.shows[req.IDs.TMDB], req, db)
	}
	return 0, 0, false
}

// single is the one id db has for a film; several distinct ids are ambiguous.
func single(entries []entry, db string) (int64, int, bool) {
	var id int64
	for _, e := range entries {
		v, ok := e.ids[db]
		if !ok {
			continue
		}
		if id != 0 && id != v {
			return 0, 0, false
		}
		id = v
	}
	return id, 0, id != 0
}

// episode maps a TMDB season episode to an entry. An entry covering the
// season starts after its offset; the entry with the largest offset below
// the episode holds it. Regular series entries (TV, then ONA/OVA) win over
// films and specials filed under the same season. With no season entry, a
// whole-series entry numbers its episodes absolutely.
func (idx *index) episode(entries []entry, req core.SearchRequest, db string) (int64, int, bool) {
	rank := func(kind string) int {
		switch kind {
		case "TV":
			return 0
		case "ONA", "OVA":
			return 1
		}
		return 2 // MOVIE, SPECIAL, unknown: never an episode of a season
	}
	best, bestRank := -1, 3
	var ids []int64
	for _, e := range entries {
		v, ok := e.ids[db]
		if !ok || e.season != req.Season || e.offset >= req.Episode || rank(e.kind) == 2 {
			continue
		}
		r := rank(e.kind)
		switch {
		case e.offset > best || (e.offset == best && r < bestRank):
			best, bestRank, ids = e.offset, r, []int64{v}
		case e.offset == best && r == bestRank && ids[0] != v:
			ids = append(ids, v)
		}
	}
	if len(ids) == 1 {
		return ids[0], req.Episode - best, true
	}
	if len(ids) > 1 {
		return 0, 0, false
	}
	// A single entry for the whole series numbers episodes from its start.
	abs := req.AbsoluteEpisode
	if abs <= 0 && req.Season == 1 {
		abs = req.Episode
	}
	if abs <= 0 {
		return 0, 0, false
	}
	var id int64
	for _, e := range entries {
		v, ok := e.ids[db]
		if !ok || e.season >= 0 || rank(e.kind) == 2 {
			continue
		}
		if id != 0 && id != v {
			return 0, 0, false
		}
		id = v
	}
	if id == 0 {
		return 0, 0, false
	}
	return id, abs, true
}

// load returns the index, fetching it when missing or stale. Concurrent
// callers share one fetch; a failed fetch is not retried for an hour (a
// stale index keeps answering meanwhile).
func (s *Service) load(ctx context.Context) (*index, error) {
	for {
		s.mu.Lock()
		now := s.opts.Now()
		if s.idx != nil && now.Sub(s.loadedAt) < s.opts.TTL {
			idx := s.idx
			s.mu.Unlock()
			return idx, nil
		}
		if !s.failedAt.IsZero() && now.Sub(s.failedAt) < failureBackoff {
			idx := s.idx
			s.mu.Unlock()
			if idx != nil {
				return idx, nil
			}
			return nil, ErrUnavailable
		}
		if wait := s.loading; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		s.loading = done
		s.mu.Unlock()

		idx, err := s.fetch()
		s.mu.Lock()
		s.loading = nil
		close(done)
		if err != nil {
			s.failedAt = s.opts.Now()
			stale := s.idx
			s.mu.Unlock()
			s.opts.Logger.Warn("anime id mapping unavailable", "err", err)
			if stale != nil {
				return stale, nil
			}
			return nil, ErrUnavailable
		}
		s.idx, s.loadedAt, s.failedAt = idx, s.opts.Now(), time.Time{}
		s.mu.Unlock()
		return idx, nil
	}
}

// fetch downloads and parses the dataset. It runs on its own deadline,
// not the first caller's, so a cancelled search does not waste the
// download others are waiting for.
func (s *Service) fetch() (*index, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Ether")
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("animemap: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("animemap: dataset HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > s.opts.MaxBytes {
		return nil, fmt.Errorf("animemap: dataset is %d bytes, over the %d byte limit", resp.ContentLength, s.opts.MaxBytes)
	}
	lr := &io.LimitedReader{R: resp.Body, N: s.opts.MaxBytes + 1}
	idx, err := parse(lr)
	if lr.N <= 0 {
		return nil, fmt.Errorf("animemap: dataset exceeds the %d byte limit", s.opts.MaxBytes)
	}
	if err != nil {
		return nil, err
	}
	return idx, nil
}
