// Package animemap maps TMDB and TVDB titles to the anime databases (Kitsu,
// MyAnimeList, AniList, AniDB) whose ids anime-only Stremio addons and SeaDex
// look titles up by.
//
// The primary mapping is Fribb/anime-lists on GitHub, supplemented by
// Anime-Lists/anime-lists (XML) and anime-offline-database (JSON) to fill
// missing IDs and unmapped titles/seasons ("Fribb wins, others fill gaps").
// Datasets are read lazily at runtime only when needed; Ether ships no copy.
// All inputs are untrusted: downloads are size- and time-bounded, failures
// back off, and any ambiguity yields no mapping rather than a guess.
package animemap

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/httpclient"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// Default dataset URLs read when Options.URL is empty.
const (
	DefaultURL                = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"
	DefaultAnimeListsURL      = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-full.xml"
	DefaultOfflineDatabaseURL = "https://github.com/manami-project/anime-offline-database/releases/latest/download/anime-offline-database-minified.json"
)

// Databases an anime addon or ranking source may look titles up by.
const (
	Kitsu   = "kitsu"
	MAL     = "mal"
	AniList = "anilist"
	AniDB   = "anidb"
)

const (
	defaultMaxBytes  = 96 << 20 // anime-offline-database-minified.json is ~60 MiB
	defaultTimeout   = 90 * time.Second
	defaultTTL       = 24 * time.Hour
	failureBackoff   = time.Hour
	cacheFileVersion = 1
	cacheFileName    = "animemap-v1.json"
)

var allDatabases = []string{Kitsu, MAL, AniList, AniDB}

// ErrUnavailable reports that the dataset could not be loaded (or is
// backing off after a failure). Callers treat it as "no mapping".
var ErrUnavailable = errors.New("animemap: anime id mapping unavailable")

// Options configure a Service.
type Options struct {
	URL                string       // "" = DefaultURL
	AnimeListsURL      string       // "" = DefaultAnimeListsURL when URL is ""
	OfflineDatabaseURL string       // "" = DefaultOfflineDatabaseURL when URL is ""
	CacheDir           string       // "" = memory cache only
	Client             *http.Client // nil = a public-only safefetch client
	Logger             *slog.Logger
	Now                func() time.Time
	TTL                time.Duration // 0 = 24h
	MaxBytes           int64         // 0 = 96 MiB
	Timeout            time.Duration // 0 = 90s
}

// Service loads the datasets lazily and answers lookups from memory (backed
// by an optional durable cache in Options.CacheDir).
type Service struct {
	opts Options

	mu          sync.Mutex
	idx         *index
	loadedAt    time.Time
	failedAt    time.Time
	diskChecked bool
	loading     chan struct{}
}

// New returns a Service; nothing is fetched until the first lookup.
func New(opts Options) *Service {
	if opts.URL == "" {
		opts.URL = DefaultURL
		if opts.AnimeListsURL == "" {
			opts.AnimeListsURL = DefaultAnimeListsURL
		}
		if opts.OfflineDatabaseURL == "" {
			opts.OfflineDatabaseURL = DefaultOfflineDatabaseURL
		}
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
	IDs        map[string]int64 `json:"ids"`
	Kind       string           `json:"kind,omitempty"`       // TV, ONA, OVA, MOVIE, SPECIAL
	Season     int              `json:"season"`               // TMDB (or TVDB in tvdbShows) season; -1 when none
	Offset     int              `json:"offset,omitempty"`     // episodes before the entry's first episode
	TVDBSeason int              `json:"tvdbSeason"`           // TVDB season; -1 when none or non-numeric
	TVDBOffset int              `json:"tvdbOffset,omitempty"` // TVDB episodes before the entry's first episode
	Fribb      bool             `json:"fribb,omitempty"`      // true when authoritative from Fribb
}

type index struct {
	shows      map[int64][]entry  // TMDB tv id
	movies     map[int64][]entry  // TMDB movie id
	tvdbShows  map[int64][]entry  // TVDB show id (Season/Offset hold TVDB numbering)
	imdbMovies map[string][]entry // lower-case tt... IMDb id for films
}

type persistedIndex struct {
	Version    int                `json:"version"`
	SavedAt    int64              `json:"savedAt"`
	Shows      map[int64][]entry  `json:"shows,omitempty"`
	Movies     map[int64][]entry  `json:"movies,omitempty"`
	TVDBShows  map[int64][]entry  `json:"tvdbShows,omitempty"`
	IMDBMovies map[string][]entry `json:"imdbMovies,omitempty"`
}

// rawRecord is an intermediate dataset entry prior to multi-source merging.
type rawRecord struct {
	ids           map[string]int64
	kind          string
	tmdbTV        []int64
	tmdbMovies    []int64
	tvdb          []int64
	imdb          []string
	season        int
	hasSeason     bool
	offset        int
	hasOffset     bool
	tvdbSeason    int
	hasTVDBSeason bool
	tvdbOffset    int
	hasTVDBOffset bool
	fribb         bool
}

// rawEntry is the part of a Fribb dataset entry Ether reads. Numbers may come
// as numbers or numeric strings; TMDB movie ids as a number or a list;
// TVDB seasons may be non-numeric strings (such as "a"), which are ignored
// except when explicitly marking absolute whole-series numbering.
type rawEntry struct {
	Type    string          `json:"type"`
	Kitsu   json.Number     `json:"kitsu_id"`
	MAL     json.Number     `json:"mal_id"`
	AniList json.Number     `json:"anilist_id"`
	AniDB   json.Number     `json:"anidb_id"`
	TMDB    json.RawMessage `json:"themoviedb_id"`
	TVDB    json.RawMessage `json:"tvdb_id"`
	IMDB    json.RawMessage `json:"imdb_id"`
	Season  struct {
		TMDB *int            `json:"tmdb"`
		TVDB json.RawMessage `json:"tvdb"`
	} `json:"season"`
	Offset struct {
		TMDB *int            `json:"tmdb"`
		TVDB json.RawMessage `json:"tvdb"`
	} `json:"episode_offset"`
}

func optionalNonNegInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, err := strconv.Atoi(n.String())
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func parse(r io.Reader) (*index, error) {
	fribb, err := parseFribbRecords(r)
	if err != nil {
		return nil, err
	}
	return buildIndex(fribb, nil, nil), nil
}

func parseFribbRecords(r io.Reader) ([]*rawRecord, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, fmt.Errorf("animemap: dataset is not a JSON array")
	}
	var out []*rawRecord
	for dec.More() {
		var raw rawEntry
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("animemap: bad dataset entry: %w", err)
		}
		ids := map[string]int64{}
		for db, n := range map[string]json.Number{Kitsu: raw.Kitsu, MAL: raw.MAL, AniList: raw.AniList, AniDB: raw.AniDB} {
			if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil && v > 0 {
				ids[db] = v
			}
		}
		if len(ids) == 0 {
			continue
		}
		rec := &rawRecord{
			ids:        ids,
			kind:       strings.ToUpper(strings.TrimSpace(raw.Type)),
			season:     -1,
			tvdbSeason: -1,
			fribb:      true,
		}
		if len(raw.TMDB) > 0 {
			var tmdb struct {
				TV    json.RawMessage `json:"tv"`
				Movie json.RawMessage `json:"movie"`
			}
			if json.Unmarshal(raw.TMDB, &tmdb) == nil {
				rec.tmdbTV = numbers(tmdb.TV)
				rec.tmdbMovies = numbers(tmdb.Movie)
			}
		}
		rec.tvdb = numbers(raw.TVDB)
		rec.imdb = imdbList(raw.IMDB)
		if raw.Season.TMDB != nil && *raw.Season.TMDB >= 0 {
			rec.season = *raw.Season.TMDB
			rec.hasSeason = true
		}
		if raw.Offset.TMDB != nil && *raw.Offset.TMDB > 0 {
			rec.offset = *raw.Offset.TMDB
			rec.hasOffset = true
		}
		if v, ok := optionalNonNegInt(raw.Season.TVDB); ok {
			rec.tvdbSeason = v
			rec.hasTVDBSeason = true
		} else if isAbsoluteSeasonJSON(raw.Season.TVDB) {
			rec.tvdbSeason = -1
			rec.hasTVDBSeason = true
		}
		if v, ok := optionalNonNegInt(raw.Offset.TVDB); ok && v > 0 {
			rec.tvdbOffset = v
			rec.hasTVDBOffset = true
		}
		out = append(out, rec)
	}
	return out, nil
}

func isAbsoluteSeasonJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(strings.TrimSpace(s), "a")
}

// parseAnimeListsRecords streams Anime-Lists/anime-lists XML (<anime-list><anime .../>).
func parseAnimeListsRecords(r io.Reader) ([]*rawRecord, error) {
	dec := xml.NewDecoder(r)
	sawRoot := false
	var out []*rawRecord
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("animemap: bad anime-lists XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !sawRoot {
			if se.Name.Local != "anime-list" {
				return nil, fmt.Errorf("animemap: anime-lists XML root is %q, want anime-list", se.Name.Local)
			}
			sawRoot = true
			continue
		}
		if se.Name.Local != "anime" {
			if err := dec.Skip(); err != nil {
				return nil, fmt.Errorf("animemap: bad anime-lists XML element: %w", err)
			}
			continue
		}
		var anidbStr, tvdbStr, tvdbSeasonStr, tvdbOffsetStr, tmdbTVStr, tmdbSeasonStr, tmdbOffsetStr, tmdbMovieStr, imdbStr string
		for _, attr := range se.Attr {
			switch attr.Name.Local {
			case "anidbid":
				anidbStr = attr.Value
			case "tvdbid":
				tvdbStr = attr.Value
			case "defaulttvdbseason":
				tvdbSeasonStr = attr.Value
			case "episodeoffset":
				tvdbOffsetStr = attr.Value
			case "tmdbtv":
				tmdbTVStr = attr.Value
			case "tmdbseason":
				tmdbSeasonStr = attr.Value
			case "tmdboffset":
				tmdbOffsetStr = attr.Value
			case "tmdbid":
				tmdbMovieStr = attr.Value
			case "imdbid":
				imdbStr = attr.Value
			}
		}
		if err := dec.Skip(); err != nil {
			return nil, fmt.Errorf("animemap: bad anime-lists XML anime entry: %w", err)
		}
		anidb, err := strconv.ParseInt(strings.TrimSpace(anidbStr), 10, 64)
		if err != nil || anidb <= 0 {
			continue
		}
		rec := &rawRecord{
			ids:        map[string]int64{AniDB: anidb},
			season:     -1,
			tvdbSeason: -1,
		}
		rec.tmdbTV = csvPositiveInts(tmdbTVStr)
		for _, m := range csvPositiveInts(tmdbMovieStr) {
			if len(rec.tmdbTV) == 0 || !slices.Contains(rec.tmdbTV, m) {
				rec.tmdbMovies = append(rec.tmdbMovies, m)
			}
		}
		tvdbTrim := strings.TrimSpace(tvdbStr)
		if v, err := strconv.ParseInt(tvdbTrim, 10, 64); err == nil && v > 0 {
			rec.tvdb = []int64{v}
		} else {
			switch strings.ToLower(tvdbTrim) {
			case "movie":
				rec.kind = "MOVIE"
			case "ova":
				rec.kind = "OVA"
			case "ona":
				rec.kind = "ONA"
			case "tv special", "special":
				rec.kind = "SPECIAL"
			}
		}
		rec.imdb = csvIMDB(imdbStr)
		if s, ok := parseXMLSeason(tmdbSeasonStr); ok {
			rec.season = s
			rec.hasSeason = true
		}
		if off, err := strconv.Atoi(strings.TrimSpace(tmdbOffsetStr)); err == nil && off > 0 {
			rec.offset = off
			rec.hasOffset = true
		}
		if s, ok := parseXMLSeason(tvdbSeasonStr); ok {
			rec.tvdbSeason = s
			rec.hasTVDBSeason = true
		}
		if off, err := strconv.Atoi(strings.TrimSpace(tvdbOffsetStr)); err == nil && off > 0 {
			rec.tvdbOffset = off
			rec.hasTVDBOffset = true
		}
		if len(rec.tmdbTV) == 0 && len(rec.tmdbMovies) == 0 && len(rec.tvdb) == 0 && len(rec.imdb) == 0 {
			continue
		}
		out = append(out, rec)
	}
	if !sawRoot {
		return nil, fmt.Errorf("animemap: empty anime-lists XML")
	}
	return out, nil
}

func parseXMLSeason(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1, false
	}
	if strings.EqualFold(s, "a") {
		return -1, true
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return -1, false
	}
	return v, true
}

// parseOfflineDatabaseRecords streams anime-offline-database JSON ({"data":[...]}).
func parseOfflineDatabaseRecords(r io.Reader) ([]*rawRecord, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("animemap: offline database is not a JSON object")
	}
	sawData := false
	var out []*rawRecord
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("animemap: bad offline database key: %w", err)
		}
		key, _ := keyTok.(string)
		if key != "data" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("animemap: bad offline database field %q: %w", key, err)
			}
			continue
		}
		arrTok, err := dec.Token()
		if err != nil || arrTok != json.Delim('[') {
			return nil, fmt.Errorf("animemap: offline database data is not an array")
		}
		sawData = true
		for dec.More() {
			var item struct {
				Type    string   `json:"type"`
				Sources []string `json:"sources"`
			}
			if err := dec.Decode(&item); err != nil {
				return nil, fmt.Errorf("animemap: bad offline database entry: %w", err)
			}
			ids := parseSourceURLs(item.Sources)
			if len(ids) == 0 {
				continue
			}
			out = append(out, &rawRecord{
				ids:  ids,
				kind: strings.ToUpper(strings.TrimSpace(item.Type)),
			})
		}
		if endTok, err := dec.Token(); err != nil || endTok != json.Delim(']') {
			return nil, fmt.Errorf("animemap: truncated offline database data array")
		}
	}
	if !sawData {
		return nil, fmt.Errorf("animemap: offline database missing data array")
	}
	return out, nil
}

func parseSourceURLs(sources []string) map[string]int64 {
	var ids map[string]int64
	for _, raw := range sources {
		db, id := parseSourceURL(raw)
		if db == "" || id <= 0 {
			continue
		}
		if ids == nil {
			ids = make(map[string]int64, 4)
		}
		if existing, ok := ids[db]; ok && existing != id {
			// Conflicting IDs within one source list: drop that db rather than guess.
			ids[db] = -1
			continue
		}
		ids[db] = id
	}
	for db, id := range ids {
		if id <= 0 {
			delete(ids, db)
		}
	}
	return ids
}

func parseSourceURL(u string) (string, int64) {
	u = strings.TrimSpace(u)
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	u = strings.TrimPrefix(u, "www.")
	var db, rest string
	switch {
	case strings.HasPrefix(u, "anidb.net/anime/"):
		db, rest = AniDB, u[len("anidb.net/anime/"):]
	case strings.HasPrefix(u, "anilist.co/anime/"):
		db, rest = AniList, u[len("anilist.co/anime/"):]
	case strings.HasPrefix(u, "myanimelist.net/anime/"):
		db, rest = MAL, u[len("myanimelist.net/anime/"):]
	case strings.HasPrefix(u, "kitsu.app/anime/"):
		db, rest = Kitsu, u[len("kitsu.app/anime/"):]
	case strings.HasPrefix(u, "kitsu.io/anime/"):
		db, rest = Kitsu, u[len("kitsu.io/anime/"):]
	default:
		return "", 0
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", 0
	}
	v, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil || v <= 0 {
		return "", 0
	}
	return db, v
}

// buildIndex merges Fribb (authoritative) with Anime-Lists XML and
// anime-offline-database JSON ("Fribb wins, others fill gaps").
func buildIndex(fribb, xmlList, aodb []*rawRecord) *index {
	type dbKey struct {
		db string
		id int64
	}
	aodbByID := make(map[dbKey][]*rawRecord, len(aodb)*2)
	for _, a := range aodb {
		for db, id := range a.ids {
			if id > 0 {
				k := dbKey{db: db, id: id}
				aodbByID[k] = append(aodbByID[k], a)
			}
		}
	}
	xmlByAniDB := make(map[int64][]*rawRecord, len(xmlList))
	for _, x := range xmlList {
		if id := x.ids[AniDB]; id > 0 {
			xmlByAniDB[id] = append(xmlByAniDB[id], x)
		}
	}

	enrichFromAODB := func(rec *rawRecord) {
		if len(aodbByID) == 0 {
			return
		}
		var matches []*rawRecord
		seen := map[*rawRecord]bool{}
		addCompatible := func(cands []*rawRecord) {
			for _, c := range cands {
				if seen[c] {
					continue
				}
				conflict := false
				for db, existing := range rec.ids {
					if existing > 0 {
						if cid := c.ids[db]; cid > 0 && cid != existing {
							conflict = true
							break
						}
					}
				}
				if !conflict {
					seen[c] = true
					matches = append(matches, c)
				}
			}
		}
		if anidb := rec.ids[AniDB]; anidb > 0 {
			addCompatible(aodbByID[dbKey{db: AniDB, id: anidb}])
		}
		if len(matches) == 0 {
			for _, db := range []string{AniList, MAL, Kitsu} {
				if id := rec.ids[db]; id > 0 {
					addCompatible(aodbByID[dbKey{db: db, id: id}])
				}
			}
		}
		if len(matches) == 0 {
			return
		}
		for _, db := range allDatabases {
			if rec.ids[db] > 0 {
				continue // Existing ID wins.
			}
			var agreed int64
			for _, m := range matches {
				v := m.ids[db]
				if v <= 0 {
					continue
				}
				if agreed != 0 && agreed != v {
					agreed = -1
					break
				}
				agreed = v
			}
			if agreed > 0 {
				rec.ids[db] = agreed
			}
		}
		if rec.kind == "" || rec.kind == "UNKNOWN" {
			var kind string
			for _, m := range matches {
				if m.kind == "" || m.kind == "UNKNOWN" {
					continue
				}
				if kind != "" && kind != m.kind {
					kind = "?"
					break
				}
				kind = m.kind
			}
			if kind != "" && kind != "?" {
				rec.kind = kind
			}
		}
	}

	fribbAniDBTMDB := map[int64]bool{}
	fribbAniDBTVDB := map[int64]bool{}
	fribbAniDBIMDB := map[int64]bool{}

	for _, f := range fribb {
		enrichFromAODB(f)
		if anidb := f.ids[AniDB]; anidb > 0 {
			if xs := xmlByAniDB[anidb]; len(xs) == 1 {
				x := xs[0]
				if len(f.tmdbTV) == 0 && len(f.tmdbMovies) == 0 {
					f.tmdbTV = x.tmdbTV
					f.tmdbMovies = x.tmdbMovies
					if !f.hasSeason && x.hasSeason {
						f.season, f.hasSeason = x.season, true
					}
					if !f.hasOffset && x.hasOffset {
						f.offset, f.hasOffset = x.offset, true
					}
				}
				if len(f.tvdb) == 0 {
					f.tvdb = x.tvdb
				}
				if !f.hasTVDBSeason && x.hasTVDBSeason {
					f.tvdbSeason, f.hasTVDBSeason = x.tvdbSeason, true
				}
				if !f.hasTVDBOffset && x.hasTVDBOffset {
					f.tvdbOffset, f.hasTVDBOffset = x.tvdbOffset, true
				}
				if len(f.imdb) == 0 {
					f.imdb = x.imdb
				}
			}
			if len(f.tmdbTV) > 0 || len(f.tmdbMovies) > 0 {
				fribbAniDBTMDB[anidb] = true
			}
			if len(f.tvdb) > 0 {
				fribbAniDBTVDB[anidb] = true
			}
			if len(f.imdb) > 0 {
				fribbAniDBIMDB[anidb] = true
			}
		}
	}

	idx := &index{
		shows:      map[int64][]entry{},
		movies:     map[int64][]entry{},
		tvdbShows:  map[int64][]entry{},
		imdbMovies: map[string][]entry{},
	}

	addRecord := func(rec *rawRecord, allowTMDB, allowTVDB, allowIMDB bool) {
		if len(rec.ids) == 0 {
			return
		}
		e := entry{
			IDs:        rec.ids,
			Kind:       rec.kind,
			Season:     rec.season,
			Offset:     rec.offset,
			TVDBSeason: rec.tvdbSeason,
			TVDBOffset: rec.tvdbOffset,
			Fribb:      rec.fribb,
		}
		if allowTMDB {
			for _, tv := range rec.tmdbTV {
				idx.shows[tv] = append(idx.shows[tv], e)
			}
			for _, m := range rec.tmdbMovies {
				idx.movies[m] = append(idx.movies[m], e)
			}
		}
		if allowTVDB {
			tvdbEntry := e
			tvdbEntry.Season = rec.tvdbSeason
			tvdbEntry.Offset = rec.tvdbOffset
			for _, tvdb := range rec.tvdb {
				idx.tvdbShows[tvdb] = append(idx.tvdbShows[tvdb], tvdbEntry)
			}
		}
		if allowIMDB && (rec.kind == "MOVIE" || (len(rec.tmdbMovies) > 0 && len(rec.tmdbTV) == 0)) {
			for _, tt := range rec.imdb {
				idx.imdbMovies[tt] = append(idx.imdbMovies[tt], e)
			}
		}
	}

	for _, f := range fribb {
		addRecord(f, true, true, true)
	}
	for _, x := range xmlList {
		enrichFromAODB(x)
		if x.kind == "" || x.kind == "UNKNOWN" {
			switch {
			case len(x.tmdbMovies) > 0 && len(x.tmdbTV) == 0 && len(x.tvdb) == 0:
				x.kind = "MOVIE"
			case x.hasTVDBSeason && x.tvdbSeason == 0 && (!x.hasSeason || x.season == 0):
				x.kind = "SPECIAL"
			case len(x.tmdbTV) > 0 || len(x.tvdb) > 0:
				x.kind = "TV"
			case len(x.imdb) > 0:
				x.kind = "MOVIE"
			}
		}
		anidb := x.ids[AniDB]
		addRecord(x, !fribbAniDBTMDB[anidb], !fribbAniDBTVDB[anidb], !fribbAniDBIMDB[anidb])
	}
	return idx
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

func imdbList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []string{one}
	}
	var out []string
	for _, s := range list {
		if tt := normIMDB(s); tt != "" && !slices.Contains(out, tt) {
			out = append(out, tt)
		}
	}
	return out
}

func csvPositiveInts(s string) []int64 {
	if s == "" {
		return nil
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		v, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && v > 0 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func csvIMDB(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if tt := normIMDB(part); tt != "" && !slices.Contains(out, tt) {
			out = append(out, tt)
		}
	}
	return out
}

func normIMDB(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 3 || !strings.HasPrefix(s, "tt") {
		return ""
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return s
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

// LookupCached answers from the in-memory index or the durable local disk
// cache (even if past TTL) without ever performing network I/O.
func (s *Service) LookupCached(req core.SearchRequest, db string) (id int64, episode int, ok bool) {
	s.mu.Lock()
	if s.idx == nil && s.opts.CacheDir != "" && !s.diskChecked {
		s.diskChecked = true
		if idx, savedAt, err := s.readDiskCache(); err == nil {
			s.idx = idx
			s.loadedAt = savedAt
		}
	}
	idx := s.idx
	s.mu.Unlock()
	if idx == nil {
		return 0, 0, false
	}
	return idx.lookup(req, db)
}

// LookupCour maps an offset TMDB anime season episode to its 1-based cour
// (part) number within that season, its 1-based episode inside that cour, and
// its anime-lists TVDB season/episode numbering when present.
func (s *Service) LookupCour(ctx context.Context, req core.SearchRequest) (core.CourMapping, bool, error) {
	idx, err := s.load(ctx)
	if err != nil {
		return core.CourMapping{}, false, err
	}
	m, ok := idx.lookupCour(req)
	return m, ok, nil
}

func entryKindRank(kind string) int {
	switch kind {
	case "TV":
		return 0
	case "ONA", "OVA":
		return 1
	}
	return 2 // MOVIE, SPECIAL, unknown: never an episode of a season
}

func (idx *index) lookupCour(req core.SearchRequest) (core.CourMapping, bool) {
	if req.Type != core.TypeEpisode || req.IDs.TMDB <= 0 || req.Season <= 0 || req.Episode <= 1 {
		return core.CourMapping{}, false
	}
	entries := idx.shows[req.IDs.TMDB]
	hasFribbSeason := false
	for _, e := range entries {
		if e.Fribb && e.Season == req.Season && entryKindRank(e.Kind) < 2 {
			hasFribbSeason = true
			break
		}
	}
	return lookupCourEntries(entries, req, hasFribbSeason)
}

func lookupCourEntries(entries []entry, req core.SearchRequest, fribbOnly bool) (core.CourMapping, bool) {
	best, bestRank := -1, 3
	var bestEntries []entry
	offsets := map[int]bool{}
	seasonRank := 2
	for _, e := range entries {
		if fribbOnly && !e.Fribb {
			continue
		}
		if e.Season == req.Season && entryKindRank(e.Kind) < seasonRank {
			seasonRank = entryKindRank(e.Kind)
		}
	}
	for _, e := range entries {
		if fribbOnly && !e.Fribb {
			continue
		}
		r := entryKindRank(e.Kind)
		if e.Season != req.Season || r == 2 || r != seasonRank {
			continue
		}
		offsets[e.Offset] = true
		if e.Offset >= req.Episode {
			continue
		}
		switch {
		case e.Offset > best || (e.Offset == best && r < bestRank):
			best, bestRank, bestEntries = e.Offset, r, []entry{e}
		case e.Offset == best && r == bestRank:
			bestEntries = append(bestEntries, e)
		}
	}
	if best <= 0 || len(bestEntries) == 0 {
		return core.CourMapping{}, false
	}
	for _, db := range allDatabases {
		var seen int64
		for _, e := range bestEntries {
			if id := e.IDs[db]; id > 0 {
				if seen != 0 && seen != id {
					return core.CourMapping{}, false
				}
				seen = id
			}
		}
	}
	tvdbSeason, tvdbOffset := -1, 0
	for i, e := range bestEntries {
		if i == 0 {
			tvdbSeason, tvdbOffset = e.TVDBSeason, e.TVDBOffset
		} else if e.TVDBSeason != tvdbSeason || e.TVDBOffset != tvdbOffset {
			tvdbSeason, tvdbOffset = -1, 0
			break
		}
	}
	var out core.CourMapping
	if tvdbSeason > 0 {
		out.TVDBSeason = tvdbSeason
		out.TVDBEpisode = (req.Episode - best) + tvdbOffset
	}
	if offsets[0] {
		sorted := make([]int, 0, len(offsets))
		for off := range offsets {
			sorted = append(sorted, off)
		}
		slices.Sort(sorted)
		for i, off := range sorted {
			if off == best {
				out.Cour = i + 1
				out.Episode = req.Episode - best
				break
			}
		}
	}
	if out.Cour <= 0 && out.TVDBSeason <= 0 {
		return core.CourMapping{}, false
	}
	return out, true
}

func (idx *index) lookup(req core.SearchRequest, db string) (int64, int, bool) {
	switch req.Type {
	case core.TypeMovie:
		if req.IDs.TMDB > 0 {
			if entries := idx.movies[req.IDs.TMDB]; len(entries) > 0 {
				if id, ep, ok, covered := singleTiered(entries, db); covered {
					return id, ep, ok
				}
			}
		}
		if tt := normIMDB(req.IDs.IMDB); tt != "" {
			if id, ep, ok, _ := singleTiered(idx.imdbMovies[tt], db); ok {
				return id, ep, true
			}
		}
		return 0, 0, false
	case core.TypeEpisode:
		if req.Season <= 0 || req.Episode <= 0 {
			return 0, 0, false // specials and whole seasons have no mapping
		}
		if req.IDs.TMDB > 0 {
			if showEntries := idx.shows[req.IDs.TMDB]; len(showEntries) > 0 {
				return idx.episode(showEntries, req, db)
			}
			// If TMDB is completely unmapped across all datasets, allow TVDB
			// fallback only when TVDB has a single unsplit Season 1 (matching
			// req.Season == 1) or an explicit absolute episode number, so
			// diverging TMDB/TVDB season splits can never be guessed.
			if req.IDs.TVDB > 0 {
				if tvEntries := idx.tvdbShows[req.IDs.TVDB]; canFallbackTMDBToTVDB(tvEntries, req) {
					return idx.episode(tvEntries, req, db)
				}
			}
			return 0, 0, false
		}
		if req.IDs.TVDB > 0 {
			return idx.episode(idx.tvdbShows[req.IDs.TVDB], req, db)
		}
	}
	return 0, 0, false
}

func canFallbackTMDBToTVDB(entries []entry, req core.SearchRequest) bool {
	if len(entries) == 0 {
		return false
	}
	hasSeason1 := false
	hasWhole := false
	for _, e := range entries {
		if entryKindRank(e.Kind) == 2 || e.Season == 0 {
			continue
		}
		if e.Season < 0 {
			hasWhole = true
			continue
		}
		if e.Season != 1 || e.Offset > 0 {
			return false // multi-season or offset-split TVDB layout: need explicit TMDB season map
		}
		hasSeason1 = true
	}
	if hasSeason1 {
		return req.Season == 1 && !hasWhole
	}
	return hasWhole && (req.AbsoluteEpisode > 0 || req.Season == 1)
}

// singleTiered checks Fribb entries first, falling back to gap-filling entries
// only when no Fribb entry maps db. covered is true when entries held at least
// one candidate for db (including an ambiguous tie).
func singleTiered(entries []entry, db string) (int64, int, bool, bool) {
	if id, ok, covered := singleTier(entries, db, true); covered {
		return id, 0, ok, true
	}
	id, ok, covered := singleTier(entries, db, false)
	return id, 0, ok, covered
}

func singleTier(entries []entry, db string, fribb bool) (int64, bool, bool) {
	var id int64
	covered := false
	for _, e := range entries {
		if e.Fribb != fribb {
			continue
		}
		v := e.IDs[db]
		if v <= 0 {
			continue
		}
		covered = true
		if id != 0 && id != v {
			return 0, false, true
		}
		id = v
	}
	return id, id != 0, covered
}

// episode maps a season episode to an entry. Fribb entries take precedence over
// gap-filling datasets ("Fribb wins, others fill gaps"):
//  1. Fribb season-specific entries for (req.Season, db)
//  2. Fribb whole-series entries for db (when AbsoluteEpisode or Season 1 applies)
//  3. Secondary season-specific entries for (req.Season, db)
//  4. Secondary whole-series entries for db
func (idx *index) episode(entries []entry, req core.SearchRequest, db string) (int64, int, bool) {
	if id, ep, ok, covered := episodeSeasonTier(entries, req, db, true); covered {
		return id, ep, ok
	}
	abs := req.AbsoluteEpisode
	if abs <= 0 && req.Season == 1 {
		abs = req.Episode
	}
	if abs > 0 {
		if id, ok, covered := episodeWholeTier(entries, db, true); covered {
			if !ok {
				return 0, 0, false
			}
			return id, abs, true
		}
	}
	if id, ep, ok, covered := episodeSeasonTier(entries, req, db, false); covered {
		return id, ep, ok
	}
	if abs > 0 {
		if id, ok, covered := episodeWholeTier(entries, db, false); covered && ok {
			return id, abs, true
		}
	}
	return 0, 0, false
}

func episodeSeasonTier(entries []entry, req core.SearchRequest, db string, fribb bool) (int64, int, bool, bool) {
	best, bestRank := -1, 3
	var ids []int64
	for _, e := range entries {
		if e.Fribb != fribb {
			continue
		}
		v := e.IDs[db]
		r := entryKindRank(e.Kind)
		if v <= 0 || e.Season != req.Season || e.Offset >= req.Episode || r == 2 {
			continue
		}
		switch {
		case e.Offset > best || (e.Offset == best && r < bestRank):
			best, bestRank, ids = e.Offset, r, []int64{v}
		case e.Offset == best && r == bestRank && !slices.Contains(ids, v):
			ids = append(ids, v)
		}
	}
	if len(ids) == 1 {
		return ids[0], req.Episode - best, true, true
	}
	if len(ids) > 1 {
		return 0, 0, false, true
	}
	return 0, 0, false, false
}

func episodeWholeTier(entries []entry, db string, fribb bool) (int64, bool, bool) {
	var id int64
	covered := false
	for _, e := range entries {
		if e.Fribb != fribb {
			continue
		}
		v := e.IDs[db]
		if v <= 0 || e.Season >= 0 || entryKindRank(e.Kind) == 2 {
			continue
		}
		covered = true
		if id != 0 && id != v {
			return 0, false, true
		}
		id = v
	}
	return id, id != 0, covered
}

// load returns the index, fetching it when missing or stale. Concurrent
// callers share one fetch; a failed fetch is not retried for an hour (a
// stale index keeps answering meanwhile).
func (s *Service) load(ctx context.Context) (*index, error) {
	for {
		s.mu.Lock()
		now := s.opts.Now()
		if s.idx == nil && s.opts.CacheDir != "" && !s.diskChecked {
			s.diskChecked = true
			if diskIdx, savedAt, err := s.readDiskCache(); err == nil {
				s.idx = diskIdx
				s.loadedAt = savedAt
			}
		}
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
			stale := s.idx
			s.mu.Unlock()
			if stale != nil {
				return stale, nil
			}
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		s.loading = done
		stale := s.idx
		s.mu.Unlock()

		if stale != nil {
			// Serve the stale index immediately and refresh in the background so
			// live searches do not block on dataset downloads.
			go s.refresh(done)
			return stale, nil
		}

		idx, err := s.refresh(done)
		if err != nil {
			return nil, err
		}
		return idx, nil
	}
}

func (s *Service) refresh(done chan struct{}) (*index, error) {
	idx, err := s.fetch()
	savedAt := s.opts.Now()
	if err == nil && s.opts.CacheDir != "" {
		if werr := s.writeDiskCache(idx, savedAt); werr != nil {
			s.opts.Logger.Warn("anime id mapping cache write failed", "err", werr)
		}
	}

	s.mu.Lock()
	s.loading = nil
	close(done)
	if err != nil {
		s.failedAt = savedAt
		stale := s.idx
		s.mu.Unlock()
		s.opts.Logger.Warn("anime id mapping unavailable", "err", err)
		if stale != nil {
			return stale, nil
		}
		return nil, ErrUnavailable
	}
	s.idx, s.loadedAt, s.failedAt = idx, savedAt, time.Time{}
	s.mu.Unlock()
	return idx, nil
}

// fetch downloads and parses the configured datasets concurrently. It runs on
// its own deadline, not the first caller's, so a cancelled search does not
// waste the download others are waiting for. Fribb (opts.URL) is required; if
// a secondary gap-filling source fails, its warning is logged and Fribb still
// answers.
func (s *Service) fetch() (*index, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
	defer cancel()

	var (
		wg       sync.WaitGroup
		fribb    []*rawRecord
		fribbErr error
		xmlList  []*rawRecord
		xmlErr   error
		aodb     []*rawRecord
		aodbErr  error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		fribb, fribbErr = fetchAndParse(ctx, s.opts.Client, s.opts.URL, s.opts.MaxBytes, parseFribbRecords)
	}()
	if s.opts.AnimeListsURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			xmlList, xmlErr = fetchAndParse(ctx, s.opts.Client, s.opts.AnimeListsURL, s.opts.MaxBytes, parseAnimeListsRecords)
		}()
	}
	if s.opts.OfflineDatabaseURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			aodb, aodbErr = fetchAndParse(ctx, s.opts.Client, s.opts.OfflineDatabaseURL, s.opts.MaxBytes, parseOfflineDatabaseRecords)
		}()
	}
	wg.Wait()

	if fribbErr != nil {
		return nil, fribbErr
	}
	if xmlErr != nil {
		s.opts.Logger.Warn("secondary anime-lists XML mapping unavailable; continuing with remaining datasets", "err", xmlErr)
	}
	if aodbErr != nil {
		s.opts.Logger.Warn("secondary anime-offline-database mapping unavailable; continuing with remaining datasets", "err", aodbErr)
	}
	return buildIndex(fribb, xmlList, aodb), nil
}

func fetchAndParse[T any](ctx context.Context, client *http.Client, rawURL string, maxBytes int64, decode func(io.Reader) (T, error)) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set("User-Agent", httpclient.UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("animemap: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("animemap: dataset HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return zero, fmt.Errorf("animemap: dataset is %d bytes, over the %d byte limit", resp.ContentLength, maxBytes)
	}
	lr := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
	out, err := decode(lr)
	if lr.N <= 0 {
		return zero, fmt.Errorf("animemap: dataset exceeds the %d byte limit", maxBytes)
	}
	if err != nil {
		return zero, err
	}
	return out, nil
}

func (s *Service) cacheFilePath() string {
	return filepath.Join(s.opts.CacheDir, cacheFileName)
}

func (s *Service) readDiskCache() (*index, time.Time, error) {
	f, err := os.Open(s.cacheFilePath())
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: s.opts.MaxBytes + 1}
	var snap persistedIndex
	if err := json.NewDecoder(lr).Decode(&snap); err != nil {
		return nil, time.Time{}, err
	}
	if lr.N <= 0 || snap.Version != cacheFileVersion || snap.SavedAt <= 0 {
		return nil, time.Time{}, errors.New("animemap: invalid cached index")
	}
	if snap.Shows == nil {
		snap.Shows = map[int64][]entry{}
	}
	if snap.Movies == nil {
		snap.Movies = map[int64][]entry{}
	}
	if snap.TVDBShows == nil {
		snap.TVDBShows = map[int64][]entry{}
	}
	if snap.IMDBMovies == nil {
		snap.IMDBMovies = map[string][]entry{}
	}
	return &index{
		shows:      snap.Shows,
		movies:     snap.Movies,
		tvdbShows:  snap.TVDBShows,
		imdbMovies: snap.IMDBMovies,
	}, time.Unix(snap.SavedAt, 0), nil
}

func (s *Service) writeDiskCache(idx *index, savedAt time.Time) error {
	if err := os.MkdirAll(s.opts.CacheDir, 0o755); err != nil {
		return err
	}
	snap := persistedIndex{
		Version:    cacheFileVersion,
		SavedAt:    savedAt.Unix(),
		Shows:      idx.shows,
		Movies:     idx.movies,
		TVDBShows:  idx.tvdbShows,
		IMDBMovies: idx.imdbMovies,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.cacheFilePath(), data, 0o644)
}
