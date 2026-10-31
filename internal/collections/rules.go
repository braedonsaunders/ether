package collections

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// ref is a TMDB entity referenced by a rule or source: by id, by name, or
// both once resolved.
type ref struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"` // person rules: cast|director|writer|crew
}

// rule is a compiled CollectionRule.
type rule struct {
	field string
	class fieldClass
	op    string
	label string

	tags []string  // lower-cased (classTags)
	refs []ref     // classRefs
	nums []float64 // classNumber: one value, a [lo,hi] pair, or a list
	// classDate: absolute bounds or a relative window resolved at
	// evaluation time.
	from, to   dateValue
	within     time.Duration
	boolVal    bool
	region     string // watchProvider rules
	ratingRank int    // contentRating gte/lte
}

// dateValue is an absolute date or an offset from "now".
type dateValue struct {
	set    bool
	abs    time.Time
	rel    bool
	offset time.Duration
	// last is the final second of the period an absolute date names
	// ("2019" ends 2019-12-31 23:59:59), used when it is an upper bound.
	last time.Time
}

// upper returns d as an inclusive upper bound.
func (d dateValue) upper() dateValue {
	if !d.rel && !d.last.IsZero() {
		d.abs = d.last
	}
	return d
}

func (d dateValue) at(now time.Time) time.Time {
	if d.rel {
		return now.Add(d.offset)
	}
	return d.abs
}

// personal reports whether the rule depends on the viewing user.
func (r *rule) personal() bool { return r.class == classBool }

// detailOnly reports whether the catalogue knows this field only for titles
// whose full TMDB details were fetched (so the engine pushes it down to
// TMDB discover to find matches it cannot see locally).
func (r *rule) detailOnly() bool {
	switch r.field {
	case "keyword", "person", "studio", "network", "runtime", "contentRating", "watchProvider":
		return true
	}
	return false
}

func compileRules(rs []core.CollectionRule) ([]*rule, error) {
	out := make([]*rule, 0, len(rs))
	for i, r := range rs {
		c, err := compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		out = append(out, c)
	}
	return out, nil
}

func compileRule(cr core.CollectionRule) (*rule, error) {
	class, ok := ruleFields[cr.Field]
	if !ok {
		return nil, fmt.Errorf("unknown field %q", cr.Field)
	}
	ratingRange := cr.Field == "contentRating" && (cr.Op == "gte" || cr.Op == "lte")
	if !classOps[class][cr.Op] && !ratingRange {
		return nil, fmt.Errorf("%s does not support %q", cr.Field, cr.Op)
	}
	r := &rule{field: cr.Field, class: class, op: cr.Op, label: cr.Label}
	raw := bytes.TrimSpace(cr.Value)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("%s needs a value", cr.Field)
	}
	switch class {
	case classTags:
		vals, err := stringList(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cr.Field, err)
		}
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" {
				r.tags = append(r.tags, strings.ToLower(v))
			}
		}
		if len(r.tags) == 0 {
			return nil, fmt.Errorf("%s needs a value", cr.Field)
		}
		if ratingRange {
			if len(r.tags) != 1 {
				return nil, fmt.Errorf("contentRating %s needs one rating", cr.Op)
			}
			if r.ratingRank = int(contentrating.LevelOf(r.tags[0])); r.ratingRank == 0 {
				return nil, fmt.Errorf("%q is not a rating Ether can order (try G, PG, PG-13, R, TV-14…)", r.tags[0])
			}
		} else if cr.Field == "contentRating" {
			// Ratings match by age level: "pg" also covers TV-PG and gb/PG,
			// and "nr" is titles without a rating. A label that names no
			// level matches nothing.
			for i, v := range r.tags {
				key, ok := contentrating.Canonical(v)
				if !ok {
					key = "\x00" + v
				}
				r.tags[i] = strings.ToLower(key)
			}
		}
	case classRefs:
		refs, region, err := refList(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cr.Field, err)
		}
		if len(refs) == 0 {
			return nil, fmt.Errorf("%s needs a value", cr.Field)
		}
		if len(refs) == 1 && refs[0].Name == "" && cr.Label != "" {
			refs[0].Name = cr.Label
		}
		r.refs, r.region = refs, strings.ToUpper(region)
	case classNumber:
		nums, err := numberList(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cr.Field, err)
		}
		switch cr.Op {
		case "between":
			if len(nums) != 2 {
				return nil, fmt.Errorf("%s between needs [from, to]", cr.Field)
			}
			if nums[0] > nums[1] {
				nums[0], nums[1] = nums[1], nums[0]
			}
		case "gte", "lte":
			if len(nums) != 1 {
				return nil, fmt.Errorf("%s %s needs one number", cr.Field, cr.Op)
			}
		default:
			if len(nums) == 0 {
				return nil, fmt.Errorf("%s needs a value", cr.Field)
			}
		}
		r.nums = nums
	case classDate:
		if cr.Op == "within" {
			d, err := parseWithin(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", cr.Field, err)
			}
			r.within = d
			break
		}
		vals, err := stringList(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cr.Field, err)
		}
		want := 1
		if cr.Op == "between" {
			want = 2
		}
		if len(vals) != want {
			return nil, fmt.Errorf("%s %s needs %d date(s)", cr.Field, cr.Op, want)
		}
		dv := make([]dateValue, len(vals))
		for i, v := range vals {
			d, err := parseDateValue(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", cr.Field, err)
			}
			dv[i] = d
		}
		switch cr.Op {
		case "gte":
			r.from = dv[0]
		case "lte":
			r.to = dv[0].upper()
		case "between":
			r.from, r.to = dv[0], dv[1].upper()
		}
	case classBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return nil, fmt.Errorf("%s needs true or false", cr.Field)
			}
			if b, err = strconv.ParseBool(s); err != nil {
				return nil, fmt.Errorf("%s needs true or false", cr.Field)
			}
		}
		r.boolVal = b
	}
	return r, nil
}

// stringList accepts "x", ["x","y"], numbers (as strings), and objects with
// a name/label/id.
func stringList(raw json.RawMessage) ([]string, error) {
	var one any
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("invalid value")
	}
	var out []string
	var add func(v any) error
	add = func(v any) error {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(x))
		case map[string]any:
			for _, k := range []string{"name", "label", "value", "key", "id"} {
				if s, ok := x[k]; ok {
					return add(s)
				}
			}
			return fmt.Errorf("object values need a name")
		case []any:
			for _, e := range x {
				if _, nested := e.([]any); nested {
					return fmt.Errorf("nested lists are not allowed")
				}
				if err := add(e); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported value")
		}
		return nil
	}
	if err := add(one); err != nil {
		return nil, err
	}
	return out, nil
}

// refList accepts 123, "123", "name", {"id":123,"name":"…","region":"US"}
// and lists of those.
func refList(raw json.RawMessage) ([]ref, string, error) {
	var one any
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, "", fmt.Errorf("invalid value")
	}
	var out []ref
	region := ""
	var add func(v any) error
	add = func(v any) error {
		switch x := v.(type) {
		case float64:
			if x <= 0 || x != math.Trunc(x) {
				return fmt.Errorf("ids are positive whole numbers")
			}
			out = append(out, ref{ID: int64(x)})
		case string:
			x = strings.TrimSpace(x)
			if x == "" {
				return nil
			}
			if n, err := strconv.ParseInt(x, 10, 64); err == nil && n > 0 {
				out = append(out, ref{ID: n})
			} else {
				out = append(out, ref{Name: x})
			}
		case map[string]any:
			var rf ref
			if id, ok := x["id"]; ok {
				switch n := id.(type) {
				case float64:
					rf.ID = int64(n)
				case string:
					rf.ID, _ = strconv.ParseInt(n, 10, 64)
				}
			}
			for _, k := range []string{"name", "label", "title"} {
				if s, ok := x[k].(string); ok && s != "" {
					rf.Name = strings.TrimSpace(s)
					break
				}
			}
			if s, ok := x["role"].(string); ok {
				rf.Role = strings.ToLower(strings.TrimSpace(s))
				if !personRoles[rf.Role] {
					return fmt.Errorf("role is cast, director, writer or crew")
				}
			}
			if s, ok := x["region"].(string); ok && s != "" {
				region = s
			}
			if rf.ID <= 0 && rf.Name == "" {
				return fmt.Errorf("pick an entry with an id or a name")
			}
			out = append(out, rf)
		case []any:
			for _, e := range x {
				if _, nested := e.([]any); nested {
					return fmt.Errorf("nested lists are not allowed")
				}
				if err := add(e); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported value")
		}
		return nil
	}
	if err := add(one); err != nil {
		return nil, "", err
	}
	return out, region, nil
}

func numberList(raw json.RawMessage) ([]float64, error) {
	vals, err := stringList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "s")) // "1980s"
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseWithin reads a look-back window: "30d", "6m", "2y", "3w", "48h", or
// a bare number of days.
func parseWithin(raw json.RawMessage) (time.Duration, error) {
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("within needs a positive window")
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf(`within needs a window like "30d" or "6m"`)
	}
	d, err := parsePeriod(strings.TrimPrefix(strings.TrimSpace(s), "-"))
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("within needs a positive window")
	}
	return d, nil
}

// parsePeriod reads "<n><unit>" with units h, d, w, m (30 days), y (365 days).
func parsePeriod(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0, fmt.Errorf("%q is not a period like 30d", s)
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a period like 30d", s)
	}
	day := 24 * time.Hour
	switch unit {
	case 'h':
		return time.Duration(n * float64(time.Hour)), nil
	case 'd':
		return time.Duration(n * float64(day)), nil
	case 'w':
		return time.Duration(n * float64(7*day)), nil
	case 'm':
		return time.Duration(n * float64(30*day)), nil
	case 'y':
		return time.Duration(n * float64(365*day)), nil
	}
	return 0, fmt.Errorf("%q is not a period like 30d", s)
}

// parseDateValue reads "2024-05-01", "2024", "now"/"today", or a signed
// offset "-30d" / "+3m".
func parseDateValue(s string) (dateValue, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "now", "today":
		return dateValue{set: true, rel: true}, nil
	}
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		d, err := parsePeriod(s[1:])
		if err != nil {
			return dateValue{}, err
		}
		if s[0] == '-' {
			d = -d
		}
		return dateValue{set: true, rel: true, offset: d}, nil
	}
	for _, p := range []struct {
		layout       string
		y, m, d      int
		exactInstant bool
	}{{"2006-01-02", 0, 0, 1, false}, {time.RFC3339, 0, 0, 0, true}, {"2006-01", 0, 1, 0, false}, {"2006", 1, 0, 0, false}} {
		if t, err := time.Parse(p.layout, s); err == nil {
			t = t.UTC()
			dv := dateValue{set: true, abs: t}
			if !p.exactInstant {
				dv.last = t.AddDate(p.y, p.m, p.d).Add(-time.Second)
			}
			return dv, nil
		}
	}
	return dateValue{}, fmt.Errorf("%q is not a date (use YYYY-MM-DD, now, or an offset like -30d)", s)
}

// evalCtx carries what rule evaluation needs beyond the entry itself.
type evalCtx struct {
	now time.Time
	// providers maps a watchProvider rule (by pointer) to the set of keys
	// streaming there, fetched from TMDB before evaluation.
	providers map[*rule]map[core.RatingKey]bool
	// status is per-user watch state for personal rules (nil = unknown).
	status map[core.RatingKey]core.WatchStatus
}

// match evaluates one rule against one entry. Unknown data (a title whose
// keywords were never fetched, say) fails positive conditions and passes
// negative ones.
func (r *rule) match(e *entry, ec *evalCtx) bool {
	switch r.class {
	case classTags:
		return r.matchTags(e)
	case classRefs:
		return r.matchRefs(e, ec)
	case classNumber:
		v, known := r.numberOf(e)
		return matchNumber(r.op, r.nums, v, known)
	case classDate:
		var ts int64
		if r.field == "released" {
			ts = e.released
		} else {
			ts = e.added
		}
		if ts == 0 {
			return false
		}
		t := time.Unix(ts, 0)
		if r.op == "within" {
			return !t.After(ec.now) && !t.Before(ec.now.Add(-r.within))
		}
		if r.from.set && t.Before(r.from.at(ec.now)) {
			return false
		}
		if r.to.set && t.After(r.to.at(ec.now)) {
			return false
		}
		return true
	case classBool:
		if ec.status == nil {
			return true // no user: personal rules are applied when serving
		}
		st := ec.status[e.key]
		var v bool
		if r.field == "watched" {
			v = st.ViewCount > 0
		} else {
			v = st.ViewOffset > 0 && st.ViewCount == 0
		}
		if r.op == "isNot" {
			return v != r.boolVal
		}
		return v == r.boolVal
	}
	return false
}

func (r *rule) matchTags(e *entry) bool {
	var have []string
	switch r.field {
	case "genre":
		have = e.genresLC
	case "country":
		have = e.countriesLC
	case "language":
		if e.lang != "" {
			have = []string{e.lang}
		}
	case "contentRating":
		if r.op == "gte" || r.op == "lte" {
			rank := int(contentrating.LevelOf(e.crating))
			if rank == 0 || r.ratingRank == 0 {
				return false
			}
			if r.op == "gte" {
				return rank >= r.ratingRank
			}
			return rank <= r.ratingRank
		}
		have = []string{strings.ToLower(contentrating.LevelOf(e.crating).Key())}
	}
	has := func(want string) bool {
		for _, h := range have {
			if h == want || (r.field == "country" && countryEqual(h, want)) {
				return true
			}
		}
		return false
	}
	return setOp(r.op, len(r.tags), func(i int) bool { return has(r.tags[i]) })
}

func (r *rule) matchRefs(e *entry, ec *evalCtx) bool {
	has := func(rf ref) bool {
		switch r.field {
		case "person":
			return e.hasPerson(rf)
		case "keyword":
			return containsFold(e.keywordsLC, rf.Name)
		case "studio", "network":
			return containsFold(e.studiosLC, rf.Name)
		case "collection":
			return rf.ID > 0 && e.collID == rf.ID
		case "watchProvider":
			set := ec.providers[r]
			return set != nil && set[e.key]
		}
		return false
	}
	if r.field == "watchProvider" {
		// One provider set per rule covers all of its refs ("any of").
		v := has(ref{})
		switch r.op {
		case "isNot", "notIn":
			return !v
		}
		return v
	}
	return setOp(r.op, len(r.refs), func(i int) bool { return has(r.refs[i]) })
}

// setOp applies is (all of), in (any of), isNot/notIn (none of).
func setOp(op string, n int, has func(i int) bool) bool {
	switch op {
	case "is":
		for i := 0; i < n; i++ {
			if !has(i) {
				return false
			}
		}
		return n > 0
	case "in":
		for i := 0; i < n; i++ {
			if has(i) {
				return true
			}
		}
		return false
	case "isNot", "notIn":
		for i := 0; i < n; i++ {
			if has(i) {
				return false
			}
		}
		return true
	}
	return false
}

func (r *rule) numberOf(e *entry) (float64, bool) {
	switch r.field {
	case "year":
		return float64(e.year), e.year > 0
	case "decade":
		return float64(e.year / 10 * 10), e.year > 0
	case "rating":
		return e.rating, e.votes > 0 || e.rating > 0
	case "votes":
		return float64(e.votes), true
	case "popularity":
		return e.pop, true
	case "runtime":
		return float64(e.runtime), e.runtime > 0
	}
	return 0, false
}

func matchNumber(op string, nums []float64, v float64, known bool) bool {
	eq := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	switch op {
	case "gte":
		return known && v >= nums[0]
	case "lte":
		return known && v <= nums[0]
	case "between":
		return known && v >= nums[0] && v <= nums[1]
	case "is", "in":
		if !known {
			return false
		}
		for _, n := range nums {
			if eq(v, n) {
				return true
			}
		}
		return false
	case "isNot", "notIn":
		if !known {
			return true
		}
		for _, n := range nums {
			if eq(v, n) {
				return false
			}
		}
		return true
	}
	return false
}

func containsFold(have []string, want string) bool {
	if want == "" {
		return false
	}
	w := strings.ToLower(want)
	for _, h := range have {
		if h == w {
			return true
		}
	}
	return false
}

// countryEqual compares a stored country (TMDB stores production-country
// names for movies and ISO codes for shows) with a wanted one in either form.
func countryEqual(have, want string) bool {
	if have == want {
		return true
	}
	if n, ok := countryNames[strings.ToUpper(have)]; ok && strings.ToLower(n) == want {
		return true
	}
	if n, ok := countryNames[strings.ToUpper(want)]; ok && strings.ToLower(n) == have {
		return true
	}
	return false
}

// countryNames maps ISO 3166-1 codes to the English names TMDB uses for
// production countries.
var countryNames = map[string]string{
	"US": "United States of America", "GB": "United Kingdom", "CA": "Canada", "AU": "Australia",
	"NZ": "New Zealand", "IE": "Ireland", "FR": "France", "DE": "Germany", "IT": "Italy", "ES": "Spain",
	"PT": "Portugal", "NL": "Netherlands", "BE": "Belgium", "CH": "Switzerland", "AT": "Austria",
	"SE": "Sweden", "NO": "Norway", "DK": "Denmark", "FI": "Finland", "IS": "Iceland", "PL": "Poland",
	"CZ": "Czech Republic", "HU": "Hungary", "RO": "Romania", "GR": "Greece", "TR": "Turkey",
	"RU": "Russia", "UA": "Ukraine", "IL": "Israel", "IR": "Iran", "IN": "India", "PK": "Pakistan",
	"CN": "China", "HK": "Hong Kong", "TW": "Taiwan", "JP": "Japan", "KR": "South Korea",
	"TH": "Thailand", "ID": "Indonesia", "PH": "Philippines", "VN": "Vietnam", "MY": "Malaysia",
	"SG": "Singapore", "MX": "Mexico", "BR": "Brazil", "AR": "Argentina", "CL": "Chile", "CO": "Colombia",
	"PE": "Peru", "ZA": "South Africa", "NG": "Nigeria", "EG": "Egypt", "MA": "Morocco",
	"SA": "Saudi Arabia", "AE": "United Arab Emirates", "LB": "Lebanon",
}
