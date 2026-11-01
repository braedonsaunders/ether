package plex

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Plex metadata type numbers.
const (
	typeNumMovie   = 1
	typeNumShow    = 2
	typeNumSeason  = 3
	typeNumEpisode = 4
)

func typeNumber(t core.MediaType) int {
	switch t {
	case core.TypeMovie:
		return typeNumMovie
	case core.TypeShow:
		return typeNumShow
	case core.TypeSeason:
		return typeNumSeason
	case core.TypeEpisode:
		return typeNumEpisode
	}
	return 0
}

func typeFromParam(s string) core.MediaType {
	switch strings.ToLower(s) {
	case "1", "movie", "movies":
		return core.TypeMovie
	case "2", "show", "shows", "tv":
		return core.TypeShow
	case "3", "season":
		return core.TypeSeason
	case "4", "episode", "episodes":
		return core.TypeEpisode
	case "18", "collection":
		return "collection"
	}
	return ""
}

const viewModeList = 65592

// defaultUnpagedLimit bounds list responses for clients that don't page.
const defaultUnpagedLimit = 500

// sectionPrefs is /library/sections/{id}/prefs: the library settings the
// apps read (visibility, collection mode, episode sorting and ordering,
// season display), with the values Ether actually behaves by. PMS types
// default/value as strings in this endpoint's JSON, unlike /:/prefs.
// Prefs writes are not supported (Ether's libraries are generated). Library
// titles themselves are configured in Ether's admin UI, not via this endpoint.
func sectionPrefs(sec Section) []model.Setting {
	set := func(id, label, summary, typ, def, val, enum string) model.Setting {
		return model.Setting{ID: id, Label: label, Summary: summary, Type: typ, Default: def, Value: val, EnumValues: enum}
	}
	visibility := set("hidden", "Visibility", "Restrict where content from this library should appear.", "int", "0", "0",
		"0:Include in home screen and global search|1:Exclude from home screen|2:Exclude from home screen and global search")
	collections := set("collectionMode", "Collections", "How to display collections on the Library tab.", "int", "2", "2",
		"0:Hide collections but show their items|1:Hide items which are in collections|2:Show collections and their items")
	original := set("originalTitles", "Use original titles", "Use the original titles for all items regardless of the library language.", "bool", "false", "false", "")
	if sec.Type == core.TypeShow {
		return []model.Setting{
			visibility,
			set("episodeSort", "Episode sorting", "How to sort the episodes for shows.", "text", "-1", "-1", "0:Oldest first|1:Newest first"),
			set("showOrdering", "Episode ordering", "Determines the ordering of episodes.", "text", "tmdbAiring", "tmdbAiring", "tmdbAiring:The Movie Database"),
			set("useSeasonTitles", "Use season titles", "Use season titles when available.", "bool", "false", "false", ""),
			original,
			set("enableBIFGeneration", "Enable video preview thumbnails", "Generate video preview thumbnails for items in this library when enabled in server settings.", "bool", "true", "false", ""),
			collections,
			set("flattenSeasons", "Seasons", "Choose whether to show or hide seasons.", "int", "0", "0", "0:Show|2:Hide for single-season series|1:Hide"),
			set("enableIntroMarkerGeneration", "Enable intro detection", "Detect intros and allow intro skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
			set("enableCreditsMarkerGeneration", "Enable credits detection", "Detect credits and allow credits skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
		}
	}
	return []model.Setting{
		visibility,
		set("enableCinemaTrailers", "Enable Cinema Trailers", "Allow Cinema Trailers to play before items in this library.", "bool", "true", "false", ""),
		original,
		set("enableBIFGeneration", "Enable video preview thumbnails", "Generate video preview thumbnails for items in this library when enabled in server settings.", "bool", "true", "false", ""),
		collections,
		set("enableCreditsMarkerGeneration", "Enable credits detection", "Detect credits and allow credits skipping for items in this library when enabled in server settings.", "bool", "true", "false", ""),
	}
}

func sectionTypeTitle(sec Section) (typeTitle, all string) {
	if sec.Type == core.TypeShow {
		return "TV Shows", "All Shows"
	}
	return "Movies", "All Movies"
}

func (s *Server) sectionContainer(sec Section) *model.MediaContainer {
	mc := libraryContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	return mc
}

// sectionParam resolves {section} and checks the user's access.
func (s *Server) sectionParam(w http.ResponseWriter, r *http.Request) (Section, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "section"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return Section{}, false
	}
	sec, ok := s.sectionByID(id)
	if !ok || !s.canSeeSection(FromContext(r.Context()).User, id) {
		writeHTTPError(w, http.StatusNotFound)
		return Section{}, false
	}
	return sec, true
}

func (s *Server) handleLibraryRoot(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{AllowSync: model.Ptr(s.downloadsAllowed(FromContext(r.Context()).User)), Identifier: model.LibraryIdentifier, MediaTagPrefix: model.MediaTagPrefix, MediaTagVersion: model.MediaTagVersion, Title1: "Plex Library"}
	for _, d := range []struct{ key, title string }{{"sections", "Library Sections"}, {"recentlyAdded", "Recently Added Content"}, {"onDeck", "On Deck Content"}} {
		mc.Directory = append(mc.Directory, model.Directory{Key: d.key, Title: d.title})
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

func (s *Server) handleSections(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	mc := &model.MediaContainer{AllowSync: model.Ptr(s.downloadsAllowed(u)), Title1: "Plex Library"}
	now := s.now().Unix()
	for _, sec := range s.userSections(u) {
		agent, scanner := agentFor(sec.Type)
		mc.Directory = append(mc.Directory, model.Directory{
			AllowSync:        model.Ptr(s.downloadsAllowed(u)),
			Filters:          model.Ptr(true),
			Refreshing:       model.Ptr(false),
			Key:              strconv.Itoa(sec.ID),
			Type:             string(sec.Type),
			Title:            sec.Title,
			Agent:            agent,
			Scanner:          scanner,
			Language:         sec.Language,
			UUID:             sec.UUID,
			UpdatedAt:        now,
			CreatedAt:        now,
			ScannedAt:        now,
			Content:          model.Ptr(true),
			IsDirectory:      model.Ptr(true),
			ContentChangedAt: now,
			Hidden:           model.Ptr(0),
			Location:         []model.Location{{ID: sec.ID, Path: "/ether/" + strings.ToLower(strings.ReplaceAll(sec.Title, " ", "-"))}},
		})
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

// handleSection is /library/sections/{id}: the section's listing directory
// (and its Meta when includeDetails/includeMeta is set).
func (s *Server) handleSection(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	typeTitle, allTitle := sectionTypeTitle(sec)
	tn := strconv.Itoa(typeNumber(sec.Type))
	mc := libraryContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	mc.Content = "secondary"
	mc.ViewGroup = "secondary"
	sub := []struct {
		key, title string
		secondary  bool
	}{
		{"all", allTitle, false},
		{"unwatched", "Unwatched", false},
		{"newest", "Recently Released", false},
		{"recentlyAdded", "Recently Added", false},
		{"recentlyViewed", "Recently Viewed", false},
		{"onDeck", "Continue Watching", false},
		{"collection", "By Collection", true},
		{"genre", "By Genre", true},
		{"year", "By Year", true},
		{"decade", "By Decade", true},
		{"country", "By Country", true},
		{"contentRating", "By Content Rating", true},
		{"firstCharacter", "By First Letter", true},
	}
	for _, d := range sub {
		dir := model.Directory{Key: d.key, Title: d.title}
		if d.secondary {
			dir.Secondary = model.Ptr(true)
		}
		mc.Directory = append(mc.Directory, dir)
	}
	mc.Directory = append(mc.Directory, model.Directory{Key: "search?type=" + tn, Prompt: "Search " + typeTitle, Search: model.Ptr(true), Title: "Search..."})
	mc.Size = len(mc.Directory)
	q := r.URL.Query()
	if q.Get("includeDetails") == "1" || q.Get("includeMeta") == "1" {
		mc.Meta = s.meta(sec, sec.Type, "")
	}
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Meta: types, filters, sorts, fields

type sortDef struct {
	key, title, defDir string
}

var sortDefs = []sortDef{
	{"titleSort", "Title", "asc"},
	{"originallyAvailableAt", "Release Date", "desc"},
	{"rating", "Critic Rating", "desc"},
	{"audienceRating", "Audience Rating", "desc"},
	{"addedAt", "Date Added", "desc"},
	{"popularity", "Popularity", "desc"},
}

type filterDef struct {
	filter, filterType, title string
}

var filterDefs = []filterDef{
	{"genre", "string", "Genre"},
	{"year", "integer", "Year"},
	{"decade", "integer", "Decade"},
	{"contentRating", "string", "Content Rating"},
	{"collection", "string", "Collection"},
	{"country", "string", "Country"},
	{"studio", "string", "Studio"},
	{"unwatched", "boolean", "Unwatched"},
}

func (s *Server) metaType(sec Section, t core.MediaType, activeSort string) model.MetaType {
	id := strconv.Itoa(sec.ID)
	tn := strconv.Itoa(typeNumber(t))
	typeTitle, _ := sectionTypeTitle(sec)
	mt := model.MetaType{Key: "/library/sections/" + id + "/all?type=" + tn, Type: string(t), Title: typeTitle, Active: true}
	for _, f := range filterDefs {
		key := "/library/sections/" + id + "/" + f.filter
		mt.Filter = append(mt.Filter, model.Filter{Filter: f.filter, FilterType: f.filterType, Key: key, Title: f.title, Type: "filter"})
	}
	activeKey, activeDir := parseSortParam(activeSort)
	for i, sd := range sortDefs {
		so := model.Sort{DefaultDirection: sd.defDir, DescKey: sd.key + ":desc", Key: sd.key, Title: sd.title}
		if sd.key == "titleSort" {
			so.FirstCharacterKey = "/library/sections/" + id + "/firstCharacter"
		}
		if i == 0 {
			so.Default = sd.defDir
		}
		if (activeKey == "" && i == 0) || activeKey == sd.key {
			so.Active = model.Ptr(true)
			dir := activeDir
			if dir == "" {
				dir = sd.defDir
			}
			so.ActiveDirection = dir
		}
		mt.Sort = append(mt.Sort, so)
	}
	mt.Field = []model.Field{
		{Key: "title", Title: "Title", Type: "string"},
		{Key: "studio", Title: "Studio", Type: "string"},
		{Key: "contentRating", Title: "Content Rating", Type: "tag"},
		{Key: "year", Title: "Year", Type: "integer", SubType: "year"},
		{Key: "decade", Title: "Decade", Type: "integer", SubType: "decade"},
		{Key: "originallyAvailableAt", Title: "Release Date", Type: "date"},
		{Key: "duration", Title: "Duration", Type: "integer", SubType: "duration"},
		{Key: "genre", Title: "Genre", Type: "tag"},
		{Key: "collection", Title: "Collection", Type: "tag"},
		{Key: "director", Title: "Director", Type: "tag"},
		{Key: "writer", Title: "Writer", Type: "tag"},
		{Key: "actor", Title: "Actor", Type: "tag"},
		{Key: "country", Title: "Country", Type: "tag"},
		{Key: "audienceRating", Title: "Audience Rating", Type: "integer", SubType: "rating"},
		{Key: "unwatched", Title: "Unwatched", Type: "boolean"},
		{Key: "inProgress", Title: "In Progress", Type: "boolean"},
	}
	return mt
}

func (s *Server) meta(sec Section, t core.MediaType, activeSort string) *model.Meta {
	return &model.Meta{
		Type: []model.MetaType{s.metaType(sec, t, activeSort)},
		FieldType: []model.FieldType{
			{Type: "tag", Operator: []model.Operator{{Key: "=", Title: "is"}, {Key: "!=", Title: "is not"}}},
			{Type: "integer", Operator: []model.Operator{{Key: "=", Title: "is"}, {Key: "!=", Title: "is not"}, {Key: ">>=", Title: "is greater than"}, {Key: "<<=", Title: "is less than"}}},
			{Type: "string", Operator: []model.Operator{{Key: "=", Title: "contains"}, {Key: "!=", Title: "does not contain"}, {Key: "==", Title: "is"}, {Key: "!==", Title: "is not"}, {Key: "<=", Title: "begins with"}, {Key: ">=", Title: "ends with"}}},
			{Type: "boolean", Operator: []model.Operator{{Key: "=", Title: "is true"}, {Key: "!=", Title: "is false"}}},
			{Type: "date", Operator: []model.Operator{{Key: "<<=", Title: "is before"}, {Key: ">>=", Title: "is after"}}},
		},
	}
}

func parseSortParam(s string) (key, dir string) {
	first, _, _ := strings.Cut(s, ",")
	key, opts, _ := strings.Cut(first, ":")
	for _, o := range strings.Split(opts, ":") {
		if o == "desc" || o == "asc" {
			dir = o
		}
	}
	return key, dir
}

// ---------------------------------------------------------------------------
// Browse

type browseRequest struct {
	q          core.BrowseQuery
	unwatched  *bool // true = unwatched (ViewCount == 0), false = watched (ViewCount > 0)
	inProgress *bool // true = in progress (ViewOffset > 0), false = not in progress (ViewOffset == 0)
	limit      int   // "limit" param; 0 = none
}

func stripFieldTypePrefix(key string) string {
	if key == "title.query" {
		return key
	}
	if prefix, rest, ok := strings.Cut(key, "."); ok && rest != "" && typeFromParam(prefix) != "" {
		return rest
	}
	return key
}

// parseFilterKeyOp extracts the field name, comparison operator ("=", "!=",
// "==", "!==", "<=", ">=", "<<", ">>"), and value from a URL query entry.
// Plex media queries encode the operator before the "=" key-value delimiter
// (e.g. "genre!=Drama" parses as key "genre!", value "Drama"; "title==Foo"
// parses as key "title", value "=Foo"; "title!==Foo" parses as key "title!",
// value "=Foo").
func parseFilterKeyOp(rawKey, rawVal string) (key, op, val string) {
	if rawVal == "" {
		for _, sep := range []string{"!==", "==", ">>=", "<<=", ">=", "<=", "!=", ">>", "<<"} {
			if k, v, ok := strings.Cut(rawKey, sep); ok && k != "" {
				switch sep {
				case ">>=":
					sep = ">>"
				case "<<=":
					sep = "<<"
				}
				return stripFieldTypePrefix(k), sep, v
			}
		}
	}
	key, op, val = rawKey, "=", rawVal
	switch {
	case strings.HasSuffix(rawKey, "!=="):
		key, op = strings.TrimSuffix(rawKey, "!=="), "!=="
	case strings.HasSuffix(rawKey, "=="):
		key, op = strings.TrimSuffix(rawKey, "=="), "=="
	case strings.HasSuffix(rawKey, ">>="):
		key, op = strings.TrimSuffix(rawKey, ">>="), ">>"
	case strings.HasSuffix(rawKey, "<<="):
		key, op = strings.TrimSuffix(rawKey, "<<="), "<<"
	case strings.HasSuffix(rawKey, ">="):
		key, op = strings.TrimSuffix(rawKey, ">="), ">="
	case strings.HasSuffix(rawKey, "<="):
		key, op = strings.TrimSuffix(rawKey, "<="), "<="
	case strings.HasSuffix(rawKey, "!="):
		key, op = strings.TrimSuffix(rawKey, "!="), "!="
	case strings.HasSuffix(rawKey, ">>"):
		key, op = strings.TrimSuffix(rawKey, ">>"), ">>"
	case strings.HasSuffix(rawKey, "<<"):
		key, op = strings.TrimSuffix(rawKey, "<<"), "<<"
	case strings.HasSuffix(rawKey, ">"):
		key, op = strings.TrimSuffix(rawKey, ">"), ">="
	case strings.HasSuffix(rawKey, "<"):
		key, op = strings.TrimSuffix(rawKey, "<"), "<="
	case strings.HasSuffix(rawKey, "!"):
		key = strings.TrimSuffix(rawKey, "!")
		if strings.HasPrefix(rawVal, "=") {
			op, val = "!==", rawVal[1:]
		} else {
			op = "!="
		}
	case strings.HasSuffix(rawKey, "="):
		key, op = strings.TrimSuffix(rawKey, "="), "=="
	default:
		if strings.HasPrefix(rawVal, "=") {
			op, val = "==", rawVal[1:]
		}
	}
	return stripFieldTypePrefix(key), op, val
}

func parseBoolFilter(op, val string) (bool, bool) {
	v := strings.TrimSpace(val)
	var b bool
	switch {
	case v == "1" || strings.EqualFold(v, "true"):
		b = true
	case v == "0" || strings.EqualFold(v, "false"):
		b = false
	default:
		return false, false
	}
	if op == "!=" || op == "!==" {
		b = !b
	}
	return b, true
}

func parseDateFilter(op, raw string, now time.Time) (core.DateFilter, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return core.DateFilter{}, false
	}
	if strings.HasPrefix(v, "-") {
		rel := v[1:]
		for _, unit := range []string{"mon", "s", "m", "h", "d", "w", "y"} {
			if strings.HasSuffix(rel, unit) {
				n, err := strconv.Atoi(strings.TrimSuffix(rel, unit))
				if err != nil || n < 0 {
					return core.DateFilter{}, false
				}
				var ts int64
				switch unit {
				case "s":
					ts = now.Add(-time.Duration(n) * time.Second).Unix()
				case "m":
					ts = now.Add(-time.Duration(n) * time.Minute).Unix()
				case "h":
					ts = now.Add(-time.Duration(n) * time.Hour).Unix()
				case "d":
					ts = now.AddDate(0, 0, -n).Unix()
				case "w":
					ts = now.AddDate(0, 0, -7*n).Unix()
				case "mon":
					ts = now.AddDate(0, -n, 0).Unix()
				case "y":
					ts = now.AddDate(-n, 0, 0).Unix()
				}
				return core.DateFilter{Op: op, Unix: ts}, true
			}
		}
	}
	if tm, err := time.Parse("2006-01-02", v); err == nil {
		return core.DateFilter{Op: op, Unix: tm.UTC().Unix(), SpanDay: true}, true
	}
	if tm, err := time.Parse(time.RFC3339, v); err == nil {
		return core.DateFilter{Op: op, Unix: tm.UTC().Unix()}, true
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return core.DateFilter{Op: op, Unix: n}, true
	}
	return core.DateFilter{}, false
}

// parseBrowse converts Plex filter/sort params into a catalogue query.
func (s *Server) parseBrowse(ctx context.Context, sec Section, t core.MediaType, u *core.User, q url.Values) browseRequest {
	br := browseRequest{}
	f := core.Filter{Type: t}
	if u != nil {
		f.MaxContentRating = u.MaxContentRating
	}
	splitVals := func(v string) []string {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	atoi := func(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
	minPos := func(cur, n int) int {
		if cur == 0 || n < cur {
			return n
		}
		return cur
	}
	rawKeys := make([]string, 0, len(q))
	for k := range q {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)
	for _, rawKey := range rawKeys {
		for _, rawVal := range q[rawKey] {
			key, op, v := parseFilterKeyOp(rawKey, rawVal)
			isNeg := op == "!=" || op == "!=="
			switch key {
			case "genre":
				for _, g := range splitVals(v) {
					name := s.facetTitle(ctx, t, "genre", g)
					if isNeg {
						f.ExcludeGenres = append(f.ExcludeGenres, name)
					} else {
						f.Genres = append(f.Genres, name)
					}
				}
			case "country":
				for _, c := range splitVals(v) {
					name := s.facetTitle(ctx, t, "country", c)
					if isNeg {
						f.ExcludeCountries = append(f.ExcludeCountries, name)
					} else {
						f.Countries = append(f.Countries, name)
					}
				}
			case "contentRating":
				for _, c := range splitVals(v) {
					name := s.facetTitle(ctx, t, "contentRating", c)
					if isNeg {
						f.ExcludeContentRatings = append(f.ExcludeContentRatings, name)
					} else {
						f.ContentRatings = append(f.ContentRatings, name)
					}
				}
			case "studio":
				name := s.facetTitle(ctx, t, "studio", v)
				if op == "=" && f.Studio == "" && len(f.StudioFilters) == 0 {
					f.Studio = name
				}
				f.StudioFilters = append(f.StudioFilters, core.StringFilter{Op: op, Value: name})
			case "firstCharacter":
				f.FirstCharacter = v
			case "language":
				f.Languages = append(f.Languages, splitVals(v)...)
			case "year":
				switch op {
				case ">>":
					if n := atoi(v); n > 0 {
						f.YearFrom = max(f.YearFrom, n+1)
					}
				case ">=":
					if n := atoi(v); n > 0 {
						f.YearFrom = max(f.YearFrom, n)
					}
				case "<<":
					if n := atoi(v); n > 1 {
						f.YearTo = minPos(f.YearTo, n-1)
					}
				case "<=":
					if n := atoi(v); n > 0 {
						f.YearTo = minPos(f.YearTo, n)
					}
				case "!=", "!==":
					for _, y := range splitVals(v) {
						if n := atoi(y); n > 0 {
							f.ExcludeYears = append(f.ExcludeYears, n)
						}
					}
				default:
					var ns []int
					for _, y := range splitVals(v) {
						if n := atoi(y); n > 0 {
							ns = append(ns, n)
						}
					}
					if len(ns) == 1 && len(f.Years) == 0 && f.YearFrom == 0 && f.YearTo == 0 {
						f.YearFrom, f.YearTo = ns[0], ns[0]
					} else if len(ns) > 0 {
						if f.YearFrom > 0 && f.YearFrom == f.YearTo && len(f.Years) == 0 {
							f.Years = append(f.Years, f.YearFrom)
							f.YearFrom, f.YearTo = 0, 0
						}
						f.Years = append(f.Years, ns...)
					}
				}
			case "decade":
				switch op {
				case ">>":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearFrom = max(f.YearFrom, d+10)
					}
				case ">=":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearFrom = max(f.YearFrom, d)
					}
				case "<<":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearTo = minPos(f.YearTo, d-1)
					}
				case "<=":
					if d := (atoi(v) / 10) * 10; d > 0 {
						f.YearTo = minPos(f.YearTo, d+9)
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if d := (atoi(p) / 10) * 10; d > 0 {
							for y := d; y <= d+9; y++ {
								f.ExcludeYears = append(f.ExcludeYears, y)
							}
						}
					}
				default:
					var decs []int
					for _, p := range splitVals(v) {
						if d := (atoi(p) / 10) * 10; d > 0 {
							decs = append(decs, d)
						}
					}
					if len(decs) == 1 && len(f.Years) == 0 {
						f.YearFrom = max(f.YearFrom, decs[0])
						f.YearTo = minPos(f.YearTo, decs[0]+9)
					} else if len(decs) > 1 {
						for _, d := range decs {
							for y := d; y <= d+9; y++ {
								f.Years = append(f.Years, y)
							}
						}
					}
				}
			case "originallyAvailableAt":
				if df, ok := parseDateFilter(op, v, s.now()); ok {
					f.ReleasedFilters = append(f.ReleasedFilters, df)
				}
			case "collection":
				var ids []int64
				for _, p := range splitVals(v) {
					if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
						if rk := core.RatingKey(id); rk.Kind() == core.KindCollection {
							id = rk.TMDB()
						}
						ids = append(ids, id)
					}
				}
				if len(ids) > 0 {
					if isNeg {
						f.ExcludeCollections = append(f.ExcludeCollections, ids...)
					} else {
						if len(ids) == 1 && f.Collection == 0 && len(f.Collections) == 0 {
							f.Collection = ids[0]
						}
						f.Collections = append(f.Collections, ids...)
					}
				}
			case "actor", "director", "writer", "producer":
				kind := "cast"
				switch key {
				case "director":
					kind = "director"
				case "writer":
					kind = "writer"
				case "producer":
					kind = "crew"
				}
				var ids []int64
				for _, p := range splitVals(v) {
					if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
						ids = append(ids, id)
					}
				}
				if len(ids) > 0 {
					if isNeg {
						f.Credits = append(f.Credits, core.CreditFilter{People: ids, Kind: kind, Exclude: true})
					} else {
						if len(ids) == 1 && f.Person == 0 {
							f.Person = ids[0]
						}
						f.Credits = append(f.Credits, core.CreditFilter{People: ids, Kind: kind})
					}
				}
			case "title":
				if op == "=" && f.Query == "" && len(f.TitleFilters) == 0 {
					f.Query = v
				} else {
					f.TitleFilters = append(f.TitleFilters, core.StringFilter{Op: op, Value: v})
				}
			case "audienceRating", "rating":
				switch op {
				case ">>", ">=":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						f.MinRating = max(f.MinRating, x)
					}
				case "<<", "<=":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						if f.MaxRating == 0 || x < f.MaxRating {
							f.MaxRating = x
						}
					}
				case "=", "==":
					if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && x > 0 {
						f.MinRating = max(f.MinRating, x)
						if f.MaxRating == 0 || x < f.MaxRating {
							f.MaxRating = x
						}
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if x, err := strconv.ParseFloat(p, 64); err == nil && x > 0 {
							f.ExcludeRatings = append(f.ExcludeRatings, x)
						}
					}
				}
			case "duration":
				switch op {
				case "<<", "<=":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						if f.MaxRuntime == 0 || d < f.MaxRuntime {
							f.MaxRuntime = d
						}
					}
				case ">>", ">=":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						if d > f.MinRuntime {
							f.MinRuntime = d
						}
					}
				case "=", "==":
					if n := atoi(v); n > 0 {
						d := time.Duration(n) * time.Millisecond
						f.MinRuntime, f.MaxRuntime = d, d
					}
				case "!=", "!==":
					for _, p := range splitVals(v) {
						if n := atoi(p); n > 0 {
							f.ExcludeRuntimes = append(f.ExcludeRuntimes, time.Duration(n)*time.Millisecond)
						}
					}
				}
			case "unwatched":
				if want, ok := parseBoolFilter(op, v); ok {
					br.unwatched = &want
				}
			case "inProgress":
				if want, ok := parseBoolFilter(op, v); ok {
					br.inProgress = &want
				}
			case "viewCount":
				n := atoi(v)
				switch {
				case (op == "=" || op == "==" || op == "<=") && strings.TrimSpace(v) == "0", op == "<<" && n == 1:
					want := true
					br.unwatched = &want
				case (op == "!=" || op == "!==" || op == ">>") && strings.TrimSpace(v) == "0", (op == "=" || op == "==" || op == ">=" || op == ">>") && n > 0:
					want := false
					br.unwatched = &want
				}
			case "limit":
				br.limit = atoi(v)
			}
		}
	}
	key, dir := parseSortParam(q.Get("sort"))
	br.q.Sort, br.q.Desc = sortFor(key, dir)
	br.q.Filter = f
	return br
}

// sortFor maps a Plex sort key to a catalogue sort.
func sortFor(key, dir string) (core.SortOrder, bool) {
	var so core.SortOrder
	def := true // desc
	switch key {
	case "titleSort", "title", "":
		so, def = core.SortTitle, false
	case "originallyAvailableAt", "year", "releaseDate":
		so = core.SortReleased
	case "rating", "audienceRating", "userRating":
		so = core.SortRating
	case "addedAt", "lastViewedAt":
		so = core.SortAdded
	case "updatedAt":
		so = core.SortUpdated
	default:
		so = core.SortPopularity
	}
	switch dir {
	case "desc":
		return so, true
	case "asc":
		return so, false
	}
	return so, def
}

// maxPageSize bounds an explicit X-Plex-Container-Size handed to backends:
// far above any real client page, far below where offset+size overflows.
const maxPageSize = 10_000

// pageOf returns the page window the client asked for.
func (pr *Request) pageOf(limit int) (offset, size int) {
	offset, size = 0, defaultUnpagedLimit
	if pr.HasPage {
		offset = pr.Start
		if pr.Size >= 0 {
			size = min(pr.Size, maxPageSize)
		}
	}
	if limit > 0 {
		if offset >= limit {
			return offset, 0
		}
		size = min(size, limit-offset)
	}
	return offset, size
}

// browse runs a catalogue query honouring paging and watch-state filters.
func (s *Server) browse(ctx context.Context, pr *Request, br browseRequest) ([]core.Title, int, int, error) {
	offset, size := pr.pageOf(br.limit)
	if br.unwatched == nil && br.inProgress == nil {
		q := br.q
		q.Offset, q.Limit = offset, size
		if size == 0 {
			q.Limit = 1 // learn the total without rendering items
		}
		page, err := s.d.Catalog.Browse(ctx, q)
		if err != nil {
			return nil, 0, offset, err
		}
		items := page.Items
		if size == 0 {
			items = nil
		}
		total := page.Total
		if br.limit > 0 && total > br.limit {
			total = br.limit
		}
		return items, total, offset, nil
	}
	// Watch-state filter: scan the catalogue in chunks, filtering by watch state.
	const chunk, maxScan = 200, 5000
	var out []core.Title
	matched, scanned, total := 0, 0, 0
	for scanned < maxScan {
		q := br.q
		q.Offset, q.Limit = scanned, chunk
		page, err := s.d.Catalog.Browse(ctx, q)
		if err != nil {
			return nil, 0, offset, err
		}
		total = page.Total
		if len(page.Items) == 0 {
			break
		}
		st := s.stateFor(ctx, pr, page.Items)
		for _, t := range page.Items {
			ws := st[t.Key]
			if br.unwatched != nil {
				isUnwatched := ws.ViewCount == 0
				if isUnwatched != *br.unwatched {
					total--
					continue
				}
			}
			if br.inProgress != nil {
				isInProgress := ws.ViewOffset > 0
				if isInProgress != *br.inProgress {
					total--
					continue
				}
			}
			if matched >= offset && len(out) < size {
				out = append(out, t)
			}
			matched++
		}
		scanned += len(page.Items)
		if scanned >= page.Total || (len(out) >= size && matched > offset+size) {
			break
		}
	}
	if total < matched {
		total = matched
	}
	if br.limit > 0 && total > br.limit {
		total = br.limit
	}
	return out, total, offset, nil
}

// handleSectionSub dispatches /library/sections/{id}/{sub}.
func (s *Server) handleSectionSub(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	sub := chi.URLParam(r, "sub")
	switch sub {
	case "all", "unwatched", "newest", "recentlyAdded", "search", "autocomplete":
		s.sectionList(w, r, sec, sub)
	case "onDeck":
		s.sectionOnDeck(w, r, sec)
	case "recentlyViewed":
		s.sectionRecentlyViewed(w, r, sec)
	case "collections", "collection":
		s.sectionCollections(w, r, sec, sub)
	case "genre", "year", "decade", "contentRating", "country", "studio", "firstCharacter", "categories":
		s.sectionFacet(w, r, sec, sub)
	case "sorts":
		mt := s.metaType(sec, sec.Type, r.URL.Query().Get("sort"))
		mc := &model.MediaContainer{Size: len(mt.Sort), Sort: mt.Sort}
		s.write(w, r, mc)
	case "filters":
		mt := s.metaType(sec, sec.Type, "")
		var dirs []model.Directory
		for _, f := range mt.Filter {
			dirs = append(dirs, model.Directory{Key: f.Key, Title: f.Title, Type: "filter", Extra: []model.Attr{model.A("filter", f.Filter), model.A("filterType", f.FilterType)}})
		}
		s.write(w, r, &model.MediaContainer{Size: len(dirs), Directory: dirs})
	case "prefs":
		settings := sectionPrefs(sec)
		s.write(w, r, &model.MediaContainer{Size: len(settings), Setting: settings})
	case "refresh", "analyze", "emptyTrash":
		s.ownerOnly(s.handleOK)(w, r)
	default:
		s.handleUnknown(w, r)
	}
}

// handleSectionSubKey dispatches /library/sections/{id}/{sub}/{key} when a
// client traverses a secondary facet directory by relative key.
func (s *Server) handleSectionSubKey(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	sub := chi.URLParam(r, "sub")
	val, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		val = chi.URLParam(r, "key")
	}
	switch sub {
	case "genre", "year", "decade", "contentRating", "country", "studio", "firstCharacter":
		r2 := r.Clone(r.Context())
		q := r2.URL.Query()
		q.Set(sub, val)
		r2.URL.RawQuery = q.Encode()
		s.sectionList(w, r2, sec, "all")
	default:
		s.handleUnknown(w, r)
	}
}

func (s *Server) sectionList(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	if sub == "all" && r.URL.Query().Has("guid") {
		s.libraryByGUID(w, r, &sec)
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	t := typeFromParam(q.Get("type"))
	if t == "" {
		t = sec.Type
	}
	_, allTitle := sectionTypeTitle(sec)
	mc := s.sectionContainer(sec)
	mc.Title2 = allTitle
	mc.ViewGroup = string(t)
	if t == "collection" {
		s.sectionCollections(w, r, sec, "collections")
		return
	}
	// Seasons/episodes are not browsable as flat section lists.
	if t != sec.Type {
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	br := s.parseBrowse(ctx, sec, t, pr.User, q)
	switch sub {
	case "unwatched":
		want := true
		br.unwatched = &want
		mc.Title2 = "Unwatched"
	case "newest":
		br.q.Sort, br.q.Desc = core.SortReleased, true
		mc.Title2 = "Recently Released"
	case "recentlyAdded":
		br.q.Sort, br.q.Desc = core.SortAdded, true
		br.q.Filter.KnownAddedDate = true
		mc.Title2 = "Recently Added"
	case "search", "autocomplete":
		query := q.Get("query")
		if query == "" {
			query = q.Get("title")
		}
		if query == "" {
			query = q.Get("title.query")
		}
		s.sectionSearch(w, r, sec, t, query, mc)
		return
	}
	items, total, offset, err := s.browse(ctx, pr, br)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	if q.Get("includeMeta") == "1" {
		mc.Meta = s.meta(sec, t, q.Get("sort"))
	}
	s.write(w, r, mc)
}

func (s *Server) sectionSearch(w http.ResponseWriter, r *http.Request, sec Section, t core.MediaType, query string, mc *model.MediaContainer) {
	ctx := r.Context()
	pr := FromContext(ctx)
	var items []core.Title
	if strings.TrimSpace(query) != "" {
		limit := 50
		if n, _ := strconv.Atoi(r.URL.Query().Get("limit")); n > 0 {
			limit = min(n, 200)
		}
		search := s.searchExperience()
		res, err := search.Keyword(ctx, pr.User, query, []core.MediaType{t}, limit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items = res
	}
	mc.Title2 = "Search for '" + query + "'"
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// filterAllowed drops titles in sections the user cannot see.
func (s *Server) filterAllowed(u *core.User, ts []core.Title) []core.Title {
	return s.visibleTitles(u, ts)
}

func (s *Server) sectionFacet(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	ctx := r.Context()
	t := typeFromParam(r.URL.Query().Get("type"))
	if t == "" || t == "collection" {
		t = sec.Type
	}
	facet := sub
	if sub == "categories" {
		facet = "genre"
	}
	mc := libraryContainer()
	mc.Title1 = sec.Title
	mc.ViewMode = viewModeList
	mc.Content = "secondary"
	mc.ViewGroup = "secondary"
	titles := map[string]string{"genre": "By Genre", "year": "By Year", "decade": "By Decade", "contentRating": "By Content Rating", "country": "By Country", "studio": "By Studio", "firstCharacter": "By First Letter", "categories": ""}
	mc.Title2 = titles[sub]
	var (
		vals []core.FacetValue
		err  error
	)
	if sub == "firstCharacter" {
		pr := FromContext(ctx)
		br := s.parseBrowse(ctx, sec, t, pr.User, r.URL.Query())
		vals, err = s.firstCharacter(ctx, br.q.Filter)
	} else {
		vals, err = s.facet(ctx, t, facet)
	}
	if err != nil {
		// Catalogues may not support every facet (e.g. firstCharacter);
		// an empty list is what PMS returns for an empty library.
		s.log.Debug("facet unavailable", "facet", facet, "err", err)
		vals = nil
	}
	id := strconv.Itoa(sec.ID)
	for _, v := range vals {
		// Titles that sort after Z stay in the library, but they are not a
		// second jump-bar "#". Plex tvOS adds every directory titled "#" into
		// the offset of A–Z, so a trailing bucket titled "#" lands that many
		// titles past the letter. PMS lists one "#" (the prefix before "a").
		if sub == "firstCharacter" && v.Key == "~" {
			continue
		}
		var d model.Directory
		switch sub {
		case "firstCharacter":
			d = model.Directory{FastKey: "/library/sections/" + id + "/all?firstCharacter=" + url.QueryEscape(v.Key), Size: model.Ptr(v.Count), Key: url.PathEscape(v.Key), Title: v.Title}
		case "categories":
			d = model.Directory{Type: "directory", Key: "/library/sections/" + id + "/all?genre=" + url.QueryEscape(v.Key), Title: v.Title}
		default:
			d = model.Directory{FastKey: "/library/sections/" + id + "/all?" + facet + "=" + url.QueryEscape(v.Key), Key: v.Key, Title: v.Title, Type: facet}
		}
		mc.Directory = append(mc.Directory, d)
	}
	if sub == "firstCharacter" {
		if _, dir := parseSortParam(r.URL.Query().Get("sort")); dir == "desc" {
			for i, j := 0, len(mc.Directory)-1; i < j; i, j = i+1, j-1 {
				mc.Directory[i], mc.Directory[j] = mc.Directory[j], mc.Directory[i]
			}
		}
	}
	if sub == "categories" {
		mc.Meta = &model.Meta{Type: []model.MetaType{{Key: "/library/sections/" + id + "/categories", Type: "directory", Title: "Categories", Active: true}}}
	}
	if sub == "year" || sub == "decade" {
		sort.SliceStable(mc.Directory, func(i, j int) bool { return mc.Directory[i].Title > mc.Directory[j].Title })
	}
	mc.Size = len(mc.Directory)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// On deck, recently added, recently viewed

// inProgressTitles loads the user's in-progress items and next episodes.
func (s *Server) inProgressTitles(ctx context.Context, pr *Request, t core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	return s.continueExperience(pr).InProgress(ctx, pr.User, t, limit)
}

// getTitles fetches titles concurrently, preserving order and skipping
// failures.
func (s *Server) getTitles(ctx context.Context, keys []core.RatingKey) []core.Title {
	if ctx != nil && ctx.Value(storedPolicyKey{}) != nil {
		if cached, ok := s.d.Catalog.(interface {
			CachedGetMany(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
		}); ok {
			got, err := cached.CachedGetMany(ctx, keys)
			if err != nil || len(got) == 0 {
				return nil
			}
			out := make([]core.Title, 0, len(got))
			seen := map[core.RatingKey]bool{}
			for _, k := range keys {
				if seen[k] {
					continue
				}
				seen[k] = true
				if t, ok := got[k]; ok {
					out = append(out, t)
				}
			}
			return out
		}
	}
	res := make([]*core.Title, len(keys))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if t, err := s.d.Catalog.Get(ctx, k); err == nil && t != nil {
				res[i] = t
			}
		}()
	}
	wg.Wait()
	out := make([]core.Title, 0, len(keys))
	for _, t := range res {
		if t != nil {
			out = append(out, *t)
		}
	}
	return out
}

func (s *Server) sectionOnDeck(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	_, size := pr.pageOf(0)
	items, state := s.inProgressTitles(ctx, pr, sec.Type, min(size, 50))
	mc := s.sectionContainer(sec)
	mc.Title2 = "On Deck"
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.onDeckItems(ctx, pr, items, state)
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleOnDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	_, size := pr.pageOf(0)
	items, state := s.inProgressTitles(ctx, pr, "", min(size, 50))
	mc := libraryContainer()
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.onDeckItems(ctx, pr, items, state)
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// onDeckItems renders On Deck the way PMS does: movies as list items,
// episodes in full (tag ids, Guid, Rating, streams), every item led by its
// sync flag and section UUID.
func (s *Server) onDeckItems(ctx context.Context, pr *Request, items []core.Title, state map[core.RatingKey]core.WatchStatus) []model.Metadata {
	// Filter first: renderTitles drops hidden titles, and the loop below
	// pairs its output with items by index.
	items = s.visibleTitles(pr.User, items)
	md := s.renderTitles(ctx, pr, items, renderOpts{media: true, detailEpisodes: true, state: state})
	for i := range md {
		md[i].AllowSync = model.Ptr(s.downloadsAllowed(pr.User) && isPlayable(items[i].Type))
		if sec, ok := s.sectionFor(items[i].Type); ok {
			md[i].LibrarySectionUUID = sec.UUID
		}
	}
	return md
}

func (s *Server) sectionRecentlyViewed(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	mc.Title2 = "Recently Viewed"
	if s.d.State != nil && pr.User != nil {
		recent := experience.Recent{Visibility: s.visibility(), Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) }}
		items, err := recent.Viewed(ctx, pr.User, sec.ID, s.d.State.History, s.getTitles, func(t core.MediaType) (int, bool) {
			section, ok := s.sectionFor(t)
			return section.ID, ok
		})
		if err == nil {
			mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleRecentlyAdded(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	_, size := pr.pageOf(0)
	size = min(size, 50)
	var items []core.Title
	rq := r.URL.Query()
	// Plex leaves an unknown or combined type unfiltered, and it does not
	// apply contentDirectoryID or pinnedContentDirectoryID on this route.
	// sectionID still selects one library. KnownAddedDate drops titles whose
	// release or air date is missing or still in the future.
	wantType := typeFromParam(rq.Get("type"))
	wantSection, _ := strconv.Atoi(rq.Get("sectionID"))
	for _, sec := range s.userSections(pr.User) {
		if wantSection > 0 && sec.ID != wantSection || wantType != "" && wantType != sec.Type {
			continue
		}
		q := core.BrowseQuery{Filter: core.Filter{Type: sec.Type, KnownAddedDate: true}, Sort: core.SortAdded, Desc: true, Limit: size}
		if pr.User != nil {
			q.Filter.MaxContentRating = pr.User.MaxContentRating
		}
		page, err := s.d.Catalog.Browse(ctx, q)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		items = append(items, page.Items...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].DateAdded(s.now()).After(items[j].DateAdded(s.now()))
	})
	if len(items) > size {
		items = items[:size]
	}
	mc := libraryContainer()
	mc.Title1 = "Recently Added"
	mc.Mixed = model.Ptr(true)
	mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleLibraryAll is /library/all?type=N across sections.
func (s *Server) handleLibraryAll(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("guid") {
		s.libraryByGUID(w, r, nil)
		return
	}
	t := typeFromParam(q.Get("type"))
	sec, ok := s.sectionFor(t)
	if !ok || t == "" || !s.canSeeSection(FromContext(r.Context()).User, sec.ID) {
		s.write(w, r, libraryContainer())
		return
	}
	s.sectionList(w, r, sec, "all")
}
