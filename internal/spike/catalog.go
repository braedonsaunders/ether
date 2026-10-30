package spike

import (
	"context"
	"hash/fnv"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
)

// localFile is one configured file.
type localFile struct {
	path  string
	title core.Title
	info  core.MediaInfo
	mtime time.Time
	size  int64
}

// fileKey is a stable rating key for a path. Ids stay below 10^12 so the
// key (9*10^15 + id) remains a safe JavaScript integer.
func fileKey(path string) core.RatingKey {
	h := fnv.New64a()
	h.Write([]byte(path))
	return core.LocalKey(int64(h.Sum64()%1_000_000_000_000) + 1)
}

var (
	yearRe     = regexp.MustCompile(`^(19|20)\d{2}$`)
	releaseTag = regexp.MustCompile(`(?i)^(2160p|1080p|720p|480p|4k|uhd|hdr|hdr10|dv|bluray|blu-ray|bdrip|brrip|web-?dl|webrip|web|hdtv|dvdrip|remux|x264|x265|h264|h265|hevc|avc|aac|ac3|eac3|dts|truehd|atmos|proper|repack|extended|unrated|imax)$`)
)

// ParseFileName turns "The.Signal.1999.1080p.BluRay.x264.mkv" into
// ("The Signal", 1999).
func ParseFileName(path string) (string, int) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	words := strings.FieldsFunc(base, func(r rune) bool {
		return r == '.' || r == '_' || r == ' ' || r == '(' || r == ')' || r == '[' || r == ']'
	})
	// The year is the last year-like word before the first release tag, so
	// "Blade.Runner.2049.2017.2160p" is ("Blade Runner 2049", 2017).
	end := len(words)
	for i, w := range words {
		if i > 0 && releaseTag.MatchString(w) {
			end = i
			break
		}
	}
	yearAt := -1
	for i := end - 1; i > 0; i-- {
		if yearRe.MatchString(words[i]) {
			yearAt = i
			break
		}
	}
	year := 0
	title := words[:end]
	if yearAt > 0 {
		year, _ = strconv.Atoi(words[yearAt])
		title = words[:yearAt]
	}
	t := strings.TrimSpace(strings.Join(title, " "))
	t = strings.TrimRight(t, " -")
	if t == "" {
		t = base
	}
	return t, year
}

func sortTitle(t string) string {
	lt := strings.ToLower(t)
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(lt, a) && len(t) > len(a) {
			return t[len(a):]
		}
	}
	return t
}

// catalog implements core.Catalog over the local files.
type catalog struct{ lib *Library }

func (c catalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	f, ok := c.lib.file(k)
	if !ok {
		return nil, core.ErrNotFound
	}
	t := f.title
	return &t, nil
}

func (c catalog) Children(context.Context, core.RatingKey) ([]core.Title, error) {
	return nil, core.ErrNotFound
}

func (c catalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return nil, core.ErrNotFound
}

func matchFilter(t core.Title, f core.Filter) bool {
	if f.Type != "" && f.Type != t.Type {
		return false
	}
	if f.YearFrom > 0 && t.Year < f.YearFrom || f.YearTo > 0 && t.Year > f.YearTo {
		return false
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(f.Query)) {
		return false
	}
	if len(f.Genres) > 0 || len(f.Countries) > 0 || len(f.ContentRatings) > 0 || f.Collection != 0 || f.Person != 0 || f.Studio != "" {
		return false // local files carry no such metadata
	}
	if f.MaxRuntime > 0 && t.Runtime > f.MaxRuntime || f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	return true
}

func (c catalog) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	var all []core.Title
	for _, f := range c.lib.files() {
		if matchFilter(f.title, q.Filter) && (!q.Filter.KnownAddedDate || (!f.title.Released.IsZero() && !f.title.Released.After(time.Now()))) {
			all = append(all, f.title)
		}
	}
	less := func(i, j int) bool {
		a, b := all[i], all[j]
		switch q.Sort {
		case core.SortReleased:
			if a.Year != b.Year {
				return a.Year < b.Year
			}
		case core.SortAdded:
			if !a.Released.Equal(b.Released) {
				return a.Released.Before(b.Released)
			}
		}
		return strings.ToLower(sortTitle(a.Title)) < strings.ToLower(sortTitle(b.Title))
	}
	sort.SliceStable(all, func(i, j int) bool {
		if q.Desc {
			return less(j, i)
		}
		return less(i, j)
	})
	total := len(all)
	lo := min(max(q.Offset, 0), total)
	hi := total
	if q.Limit > 0 {
		hi = min(lo+q.Limit, total)
	}
	return core.Page{Items: all[lo:hi], Total: total}, nil
}

func (c catalog) Search(_ context.Context, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	want := false
	for _, t := range types {
		if t == core.TypeMovie {
			want = true
		}
	}
	if !want && len(types) > 0 {
		return nil, nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var out []core.Title
	for _, f := range c.lib.files() {
		if q == "" || strings.Contains(strings.ToLower(f.title.Title), q) {
			out = append(out, f.title)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c catalog) List(_ context.Context, _ string, t core.MediaType, limit int) ([]core.Title, error) {
	if t != "" && t != core.TypeMovie {
		return nil, nil
	}
	p, _ := c.Browse(context.Background(), core.BrowseQuery{Sort: core.SortAdded, Desc: true, Limit: limit})
	return p.Items, nil
}

func (c catalog) Similar(_ context.Context, k core.RatingKey, limit int) ([]core.Title, error) {
	var out []core.Title
	for _, f := range c.lib.files() {
		if f.title.Key != k {
			out = append(out, f.title)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (c catalog) Collection(context.Context, int64) (string, []core.Title, error) {
	return "", nil, core.ErrNotFound
}

func (c catalog) Facet(_ context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	if t != "" && t != core.TypeMovie {
		return nil, nil
	}
	counts := map[string]int{}
	for _, f := range c.lib.files() {
		switch facet {
		case "year":
			if f.title.Year > 0 {
				counts[strconv.Itoa(f.title.Year)]++
			}
		case "decade":
			if f.title.Year > 0 {
				counts[strconv.Itoa(f.title.Year/10*10)]++
			}
		case "firstCharacter":
			r := []rune(strings.ToUpper(sortTitle(f.title.Title)))
			k := "#"
			if len(r) > 0 && unicode.IsLetter(r[0]) {
				k = string(r[0])
			}
			counts[k]++
		case "genre", "contentRating", "studio", "country":
			// local files carry none
		default:
			return nil, core.ErrNotFound
		}
	}
	out := make([]core.FacetValue, 0, len(counts))
	for k, n := range counts {
		out = append(out, core.FacetValue{Key: k, Title: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (c catalog) NextEpisode(context.Context, core.RatingKey) (*core.Title, error) {
	return nil, core.ErrNotFound
}

func (c catalog) ByExternalID(context.Context, string, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}
