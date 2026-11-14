package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// RefineVibe returns cached structured filters immediately. A miss starts a
// bounded background call; this request falls back to ordinary title search.
// Both positive and negative answers are cached, and failures back off.
func (s *Service) RefineVibe(ctx context.Context, u *core.User, query string) (core.VibeQuery, bool, error) {
	if !s.Enabled() || ctx.Err() != nil {
		return core.VibeQuery{}, false, nil
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 300 {
		return core.VibeQuery{}, false, nil
	}
	key := strings.ToLower(query)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, cached := s.vibeCache[key]
	fresh := cached && s.now().Sub(e.at) < s.ttl
	retry := !cached || e.failedAt.IsZero() || s.now().Sub(e.failedAt) >= s.backoff
	if !fresh && retry && !s.vibeFlights[key] && len(s.vibeFlights) < 2 {
		if s.vibeFlights == nil {
			s.vibeFlights = map[string]bool{}
		}
		s.vibeFlights[key] = true
		generation := s.generation
		go s.refreshVibe(key, query, generation)
	}
	return e.vibe, e.ok, nil
}

func (s *Service) refreshVibe(key, query string, generation uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	v, ok, err := s.computeVibe(ctx, query)
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return
	}
	delete(s.vibeFlights, key)
	if s.vibeCache == nil {
		s.vibeCache = map[string]vibeCacheEntry{}
	}
	e := s.vibeCache[key]
	if err != nil {
		e.failedAt = s.now()
	} else {
		e = vibeCacheEntry{vibe: v, ok: ok, at: s.now()}
	}
	if len(s.vibeCache) >= min(s.max, 512) {
		var oldest string
		var at time.Time
		for k, entry := range s.vibeCache {
			touched := entry.at
			if entry.failedAt.After(touched) {
				touched = entry.failedAt
			}
			if oldest == "" || touched.Before(at) {
				oldest, at = k, touched
			}
		}
		delete(s.vibeCache, oldest)
	}
	s.vibeCache[key] = e
}

func (s *Service) computeVibe(ctx context.Context, query string) (core.VibeQuery, bool, error) {
	prompt := fmt.Sprintf(`A viewer typed this into a movie/TV search bar: %q.
Decide whether it describes a browseable category (genres, era, mood, language, runtime, a person, a studio, or "like some title") rather than one specific title.
Reply with JSON only, exactly: {"vibe":true,"title":"short collection title","type":"movie|show|","genres":[],"yearFrom":0,"yearTo":0,"minRating":0,"maxRuntimeMin":0,"minRuntimeMin":0,"languages":[],"countries":[],"personName":"","studio":"","similarTo":"","sort":"popularity|rating|released|title"}
Use only these genres: %s. Languages are ISO 639-1 codes (up to six distinct languages); for South Indian include te, ta, ml and kn. Choose a useful broad category rather than adding unrequested rating or country restrictions. If it is a specific title or you are unsure, reply {"vibe":false}.`, query, strings.Join(vibeGenreNames, ", "))

	content, err := s.client.Chat(ctx, []Message{
		{Role: "system", Content: "You translate search phrases into structured catalogue filters. Reply with JSON only."},
		{Role: "user", Content: prompt},
	}, true)
	if err != nil {
		return core.VibeQuery{}, false, err
	}
	v, ok := parseVibeJSON(content)
	if ok {
		v.Query = query
	}
	return v, ok, nil
}

type vibeCacheEntry struct {
	vibe     core.VibeQuery
	ok       bool
	at       time.Time
	failedAt time.Time
}

// vibeGenreNames is the catalogue's genre vocabulary (TMDB movie and
// TV names).
var vibeGenreNames = []string{
	"Action", "Adventure", "Animation", "Comedy", "Crime", "Documentary",
	"Drama", "Family", "Fantasy", "History", "Horror", "Music", "Mystery",
	"Romance", "Science Fiction", "TV Movie", "Thriller", "War", "Western",
	"Action & Adventure", "Sci-Fi & Fantasy", "War & Politics", "Talk",
	"Soap", "Reality", "News", "Kids",
}

type vibeJSON struct {
	Vibe          bool     `json:"vibe"`
	Title         string   `json:"title"`
	Type          string   `json:"type"`
	Genres        []string `json:"genres"`
	YearFrom      int      `json:"yearFrom"`
	YearTo        int      `json:"yearTo"`
	MinRating     float64  `json:"minRating"`
	MaxRuntimeMin int      `json:"maxRuntimeMin"`
	MinRuntimeMin int      `json:"minRuntimeMin"`
	Languages     []string `json:"languages"`
	Countries     []string `json:"countries"`
	PersonName    string   `json:"personName"`
	Studio        string   `json:"studio"`
	SimilarTo     string   `json:"similarTo"`
	Sort          string   `json:"sort"`
}

// parseVibeJSON strictly validates model output into a VibeQuery.
// Anything out of vocabulary or out of range is dropped; a reply with
// no surviving constraint is not a vibe.
func parseVibeJSON(content string) (core.VibeQuery, bool) {
	js := extractJSON(content)
	if js == "" {
		return core.VibeQuery{}, false
	}
	dec := json.NewDecoder(strings.NewReader(js))
	dec.DisallowUnknownFields()
	var in vibeJSON
	if err := dec.Decode(&in); err != nil || !in.Vibe {
		return core.VibeQuery{}, false
	}
	allowed := map[string]string{}
	for _, g := range vibeGenreNames {
		allowed[strings.ToLower(g)] = g
	}
	var v core.VibeQuery
	v.Title = strings.TrimSpace(in.Title)
	if len(v.Title) > 80 {
		v.Title = strings.TrimSpace(v.Title[:80])
	}
	switch strings.ToLower(in.Type) {
	case "movie", "film":
		v.Filter.Type = core.TypeMovie
	case "show", "tv", "series":
		v.Filter.Type = core.TypeShow
	case "":
	default:
		return core.VibeQuery{}, false
	}
	for _, g := range in.Genres {
		if name, ok := allowed[strings.ToLower(strings.TrimSpace(g))]; ok {
			v.Filter.Genres = append(v.Filter.Genres, name)
		}
		if len(v.Filter.Genres) >= 4 {
			break
		}
	}
	if in.YearFrom >= 1900 && in.YearFrom <= 2035 {
		v.Filter.YearFrom = in.YearFrom
	}
	if in.YearTo >= 1900 && in.YearTo <= 2035 {
		v.Filter.YearTo = in.YearTo
	}
	if v.Filter.YearFrom > 0 && v.Filter.YearTo > 0 && v.Filter.YearFrom > v.Filter.YearTo {
		return core.VibeQuery{}, false
	}
	if in.MinRating > 0 && in.MinRating <= 10 {
		v.Filter.MinRating = in.MinRating
	}
	if in.MaxRuntimeMin >= 5 && in.MaxRuntimeMin <= 600 {
		v.Filter.MaxRuntime = time.Duration(in.MaxRuntimeMin) * time.Minute
	}
	if in.MinRuntimeMin >= 5 && in.MinRuntimeMin <= 600 {
		v.Filter.MinRuntime = time.Duration(in.MinRuntimeMin) * time.Minute
	}
	seenLanguages := map[string]bool{}
	for _, raw := range in.Languages {
		tag, ok := langtag.ParseCompatible(raw)
		if ok && len(tag.Primary) == 2 && !seenLanguages[tag.Primary] {
			v.Filter.Languages = append(v.Filter.Languages, tag.Primary)
			seenLanguages[tag.Primary] = true
		}
		if len(v.Filter.Languages) == 6 {
			break
		}
	}
	for _, c := range in.Countries {
		c = strings.TrimSpace(c)
		if c != "" && len(c) <= 60 {
			v.Filter.Countries = append(v.Filter.Countries, c)
		}
		if len(v.Filter.Countries) >= 4 {
			break
		}
	}
	v.PersonName = strings.TrimSpace(in.PersonName)
	if len(v.PersonName) > 80 {
		v.PersonName = ""
	}
	v.Filter.Studio = strings.TrimSpace(in.Studio)
	if len(v.Filter.Studio) > 80 {
		v.Filter.Studio = ""
	}
	v.SimilarSeed = strings.TrimSpace(in.SimilarTo)
	if len(v.SimilarSeed) > 120 {
		v.SimilarSeed = ""
	}
	switch core.SortOrder(strings.ToLower(in.Sort)) {
	case core.SortRating, core.SortReleased, core.SortTitle, core.SortPopularity:
		v.Sort = core.SortOrder(strings.ToLower(in.Sort))
	default:
		v.Sort = core.SortPopularity
	}
	v.Desc = v.Sort != core.SortTitle

	f := v.Filter
	constrained := len(f.Genres) > 0 || f.YearFrom > 0 || f.YearTo > 0 ||
		f.MinRating > 0 || f.MaxRuntime > 0 || f.MinRuntime > 0 ||
		len(f.Languages) > 0 || len(f.Countries) > 0 || f.Studio != "" ||
		v.PersonName != "" || v.SimilarSeed != ""
	if !constrained {
		return core.VibeQuery{}, false
	}
	if v.Title == "" {
		v.Title = "Generated Collection"
	}
	return v, true
}

var _ core.VibeRefiner = (*Service)(nil)
