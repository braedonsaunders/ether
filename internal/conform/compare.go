package conform

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Difference kinds. Missing, extra, type, status, header, parse and
// request_error differences are blocking; value differences (including child
// order, reported at ".../#order" paths) are
// informational (the two servers hold different titles, so many values
// legitimately differ).
const (
	KindMissing      = "missing"
	KindExtra        = "extra"
	KindType         = "type"
	KindValue        = "value"
	KindStatus       = "status"
	KindHeader       = "header"
	KindParse        = "parse"
	KindRequestError = "request_error"
)

// Difference describes one structural or protocol mismatch at one schema
// path. Array indices are collapsed, so a path stands for every element of
// a repeated list ("/MediaContainer/Video/@title",
// "$/MediaContainer/Metadata[]/title").
type Difference struct {
	// Client, Format and Endpoint identify the request (filled by the
	// caller that ran it; Compare leaves them empty).
	Client   string `json:"client,omitempty"`
	Format   string `json:"format,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`

	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Reference any    `json:"reference,omitempty"`
	Target    any    `json:"target,omitempty"`
	// Count is how many times the path occurs on the side that has it (the
	// reference for missing/type/value, the target for extra).
	Count int `json:"count,omitempty"`
	// Nested is how many descendant paths of a missing or extra element
	// were folded into this difference.
	Nested   int  `json:"nested,omitempty"`
	Blocking bool `json:"blocking"`
}

// Report is the result of comparing responses.
type Report struct {
	Differences      []Difference      `json:"differences"`
	Discovery        []string          `json:"discovery,omitempty"`
	ReferenceDefects []ReferenceDefect `json:"reference_defects,omitempty"`
	// Endpoints lists every endpoint compared, so summaries can show
	// endpoints without differences too.
	Endpoints []string `json:"endpoints,omitempty"`
}

// Blocking counts the blocking differences.
func (r Report) Blocking() int {
	n := 0
	for _, d := range r.Differences {
		if d.Blocking {
			n++
		}
	}
	return n
}

// WithRequest stamps every difference with the request it came from.
func (r Report) WithRequest(client, format, endpoint string) Report {
	for i := range r.Differences {
		r.Differences[i].Client = client
		r.Differences[i].Format = format
		r.Differences[i].Endpoint = endpoint
	}
	return r
}

func blockingKind(kind string) bool {
	return kind != KindValue
}

// Compare checks status, selected headers and the response schema. Bodies
// are reduced to schemas (paths with array indices collapsed, the set of
// value types and distinct values seen at each path, and child element
// order), so two listings of different titles compare by shape: an
// attribute that PMS emits on some items but the target never emits is
// "missing", the reverse is "extra", and a JSON number where PMS sends a
// string is a "type" mismatch. Values that legitimately differ between
// servers (titles, ids, keys, timestamps, counts, versions, redacted
// fields) are compared by type only.
func Compare(a, b Response) Report {
	return CompareHeaders(a, b, []string{"Content-Type", "Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods"})
}

// CompareHeaders compares responses using protocol-supplied headers.
func CompareHeaders(a, b Response, headers []string) Report {
	r := Report{}
	add := func(d Difference) {
		d.Blocking = blockingKind(d.Kind)
		r.Differences = append(r.Differences, d)
	}
	if a.Status != b.Status {
		add(Difference{Path: "$status", Kind: KindStatus, Reference: a.Status, Target: b.Status})
	}
	for _, k := range headers {
		av, bv := a.Headers.Get(k), b.Headers.Get(k)
		if av != bv && !(k == "Access-Control-Allow-Origin" && redacted(av)) {
			add(Difference{Path: "$headers." + k, Kind: KindHeader, Reference: av, Target: bv})
		}
	}
	as, ae := buildSchema(a.Body)
	bs, be := buildSchema(b.Body)
	if ae != nil || be != nil {
		add(Difference{Path: "$body", Kind: KindParse, Reference: errString(ae), Target: errString(be)})
		return r
	}
	for _, d := range diffSchemas(as, bs) {
		add(d)
	}
	return r
}

func errString(e error) any {
	if e != nil {
		return e.Error()
	}
	return nil
}

// maxValues bounds the distinct values remembered per path.
const maxValues = 16

type schemaNode struct {
	count    int
	kinds    map[string]int
	values   map[string]int
	overflow bool
	element  bool // XML element / JSON object or array
}

func (n *schemaNode) add(kind, value string) {
	n.count++
	n.kinds[kind]++
	if kind == "element" || kind == "object" || kind == "array" {
		n.element = true
		return
	}
	if _, ok := n.values[value]; ok || len(n.values) < maxValues {
		n.values[value]++
	} else {
		n.overflow = true
	}
}

// singleValue returns the only value seen at a path.
func (n *schemaNode) singleValue() (string, bool) {
	if n.overflow || len(n.values) != 1 {
		return "", false
	}
	for v := range n.values {
		return v, true
	}
	return "", false
}

func (n *schemaNode) sample() string {
	if n.element {
		return ""
	}
	vals := make([]string, 0, len(n.values))
	for v := range n.values {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	if len(vals) == 0 {
		return ""
	}
	s := vals[0]
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func (n *schemaNode) kindList() string {
	ks := make([]string, 0, len(n.kinds))
	for k := range n.kinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, "|")
}

type schema struct {
	xml   bool
	nodes map[string]*schemaNode
	// before[parent] holds child-name pairs (a, b) where a appeared before
	// b inside one instance of parent.
	before map[string]map[[2]string]bool
}

func newSchema(isXML bool) *schema {
	return &schema{xml: isXML, nodes: map[string]*schemaNode{}, before: map[string]map[[2]string]bool{}}
}

func (s *schema) add(path, kind, value string) {
	n := s.nodes[path]
	if n == nil {
		n = &schemaNode{kinds: map[string]int{}, values: map[string]int{}}
		s.nodes[path] = n
	}
	n.add(kind, value)
}

func (s *schema) order(path string, children []string) {
	var seen []string
	have := map[string]bool{}
	for _, c := range children {
		if !have[c] {
			have[c] = true
			seen = append(seen, c)
		}
	}
	if len(seen) < 2 {
		return
	}
	m := s.before[path]
	if m == nil {
		m = map[[2]string]bool{}
		s.before[path] = m
	}
	for i := range seen {
		for j := i + 1; j < len(seen); j++ {
			m[[2]string{seen[i], seen[j]}] = true
		}
	}
}

func buildSchema(body []byte) (*schema, error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return newSchema(false), nil
	}
	if t[0] == '{' || t[0] == '[' {
		v, e := decodeJSON(t)
		if e != nil {
			// Fixtures recorded before Redact kept JSON valid can hold
			// bare redaction markers ("multiuser":[REDACTED]); quote them.
			var e2 error
			if v, e2 = decodeJSON(repairRedactedJSON(t)); e2 != nil {
				return nil, e
			}
		}
		s := newSchema(false)
		walkJSON(s, "$", v)
		return s, nil
	}
	if t[0] != '<' {
		return nil, fmt.Errorf("body is neither XML nor JSON: %q", truncate(string(t), 40))
	}
	s := newSchema(true)
	type frame struct {
		path     string
		children []string
	}
	dec := xml.NewDecoder(bytes.NewReader(t))
	var stack []frame
	for {
		tok, e := dec.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		switch x := tok.(type) {
		case xml.StartElement:
			name := x.Name.Local
			path := "/" + name
			if len(stack) > 0 {
				p := &stack[len(stack)-1]
				p.children = append(p.children, name)
				path = p.path + "/" + name
			}
			stack = append(stack, frame{path: path})
			s.add(path, "element", "")
			for _, a := range x.Attr {
				s.add(path+"/@"+a.Name.Local, xmlKind(a.Value), a.Value)
			}
		case xml.CharData:
			if v := strings.TrimSpace(string(x)); v != "" && len(stack) > 0 {
				s.add(stack[len(stack)-1].path+"/#text", xmlKind(v), v)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				f := stack[len(stack)-1]
				s.order(f.path, f.children)
				stack = stack[:len(stack)-1]
			}
		}
	}
	return s, nil
}

var (
	bareRedactionRE      = regexp.MustCompile(`(:\s*)(\[REDACTED[A-Z_]*\])`)
	swallowedRedactionRE = regexp.MustCompile(`(:\s*)(\[REDACTED[A-Z_]*\])(\s*")`)
)

// repairRedactedJSON quotes bare redaction markers and restores the comma
// an older redaction pattern swallowed together with the value
// ("multiuser":[REDACTED]"musicAnalysis":2).
func repairRedactedJSON(b []byte) []byte {
	b = swallowedRedactionRE.ReplaceAll(b, []byte(`$1"$2",$3`))
	return bareRedactionRE.ReplaceAll(b, []byte(`$1"$2"`))
}

func decodeJSON(b []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	err := dec.Decode(&v)
	return v, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func walkJSON(s *schema, p string, v any) {
	switch x := v.(type) {
	case map[string]any:
		s.add(p, "object", "")
		for k, val := range x {
			walkJSON(s, p+"/"+k, val)
		}
	case []any:
		s.add(p, "array", "")
		for _, val := range x {
			walkJSON(s, p+"[]", val)
		}
	case nil:
		s.add(p, "null", "null")
	case bool:
		s.add(p, "boolean", strconv.FormatBool(x))
	case json.Number:
		s.add(p, "number", x.String())
	case string:
		s.add(p, "string", x)
	}
}

// xmlKind infers a value type from an XML attribute.
func xmlKind(s string) string {
	if _, e := strconv.ParseInt(s, 10, 64); e == nil {
		return "number"
	}
	if _, e := strconv.ParseFloat(s, 64); e == nil && !strings.ContainsAny(s, "xXnN") {
		return "number"
	}
	if s == "true" || s == "false" {
		return "boolean"
	}
	return "string"
}

// leafName returns the attribute or key name at the end of a path.
func leafName(path string) string {
	leaf := path[strings.LastIndex(path, "/")+1:]
	leaf = strings.TrimPrefix(leaf, "@")
	return strings.TrimSuffix(leaf, "[]")
}

// volatileRE matches names whose values differ between servers and
// libraries: they are compared by type (and URL shape) only.
var volatileRE = regexp.MustCompile(`(?i)^(.*title.*|.*summary|tagline|.*name|machineIdentifier|.*token|.*timestamp|.*At|username|email|.*guid|.*ratingKey|.*key|id|.*ID|.*Id|.*thumb|.*art|banner|.*logo|.*theme|.*url|uri|size|totalSize|offset|.*count|.*year|duration|.*index|.*rating|viewOffset|.*version|platform.*|.*slug|alt|.*studio|tag|file|uuid|.*bitrate|width|height|address|.*port|host|composite|filter|ownerFeatures|countryCode|score|.*Image|image|value|default|location|path|.*Color|topLeft|topRight|bottomLeft|bottomRight|originallyAvailableAt|contentRating|role|.*Rating|reason.*|.*Text|.*Code|.*Size|.*Length|.*Offset|.*Count|.*Bitrates|.*Qualities|.*Resolutions|.*Sessions|subscription.*|.*features|.*Features|.*Address|.*Port|minYear|maxYear|tagKey|.*ItemID|playQueue.*|sessionKey|throttled|progress|speed|frameRate|aspectRatio|channels|samplingRate|bitDepth|level|refFrames|codec|.*Codec|profile|.*Profile|container|videoResolution|videoFrameRate|audioChannels|audioChannelLayout|chroma.*|color.*|language.*|languageTag|languageCode|displayTitle|extendedDisplayTitle|scanType|DOVI.*|has64bitOffsets|optimizedForStreaming|selected|default|forced|hidden|canDownload.*|.*Lock|loudness.*|peak|gain|lra|indexes|hearingImpaired|original|external|userID|librarySectionID|collectionSort|enumValues|leafCount|childCount|viewCount|skipCount|viewedLeafCount|myPlex.*|pushNotifications|transcoderActive.*|livetv|readOnlyLibraries|audienceRatingImage|ratingImage|primaryExtraKey|chapterSource|subtype|extraType|parentStudio|grandparentStudio|context)$`)

func volatile(path string) bool {
	return volatileRE.MatchString(leafName(path))
}

// freeTextRE matches names whose XML values are free text (a title can be
// "1917"), so XML type inference is meaningless for them.
var freeTextRE = regexp.MustCompile(`(?i)^(videoResolution|.*title.*|.*summary|tagline|tag|.*name|studio|key|.*key|value|default|enumValues|contentRating|filter|role|.*slug|alt|top.*|bottom.*|.*studio|.*contentRating|path|file|uri|.*Text|label|hubIdentifier|id|guid|.*guid)$`)

func redacted(v string) bool { return strings.Contains(v, "[REDACTED") }

func (n *schemaNode) anyRedacted() bool {
	for v := range n.values {
		if redacted(v) {
			return true
		}
	}
	return false
}

func diffSchemas(ref, tgt *schema) []Difference {
	paths := map[string]bool{}
	for p := range ref.nodes {
		paths[p] = true
	}
	for p := range tgt.nodes {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)

	var out []Difference
	// Fold descendants of a missing/extra element into that element.
	var foldMissing, foldExtra string
	var foldMissingAt, foldExtraAt = -1, -1
	under := func(p, root string) bool {
		return root != "" && (strings.HasPrefix(p, root+"/") || strings.HasPrefix(p, root+"[]"))
	}
	for _, p := range ordered {
		x, xok := ref.nodes[p]
		y, yok := tgt.nodes[p]
		switch {
		case !yok:
			if under(p, foldMissing) {
				out[foldMissingAt].Nested++
				continue
			}
			out = append(out, Difference{Path: p, Kind: KindMissing, Reference: x.sample(), Count: x.count})
			if x.element {
				foldMissing, foldMissingAt = p, len(out)-1
			}
		case !xok:
			if under(p, foldExtra) {
				out[foldExtraAt].Nested++
				continue
			}
			out = append(out, Difference{Path: p, Kind: KindExtra, Target: y.sample(), Count: y.count})
			if y.element {
				foldExtra, foldExtraAt = p, len(out)-1
			}
		default:
			if d, ok := typeDiff(p, x, y, ref.xml); ok {
				out = append(out, d)
				continue
			}
			if d, ok := valueDiff(p, x, y); ok {
				out = append(out, d)
			}
		}
	}
	out = append(out, orderDiffs(ref, tgt)...)
	return out
}

func typeDiff(p string, x, y *schemaNode, isXML bool) (Difference, bool) {
	if x.anyRedacted() || y.anyRedacted() {
		return Difference{}, false
	}
	refK, tgtK := map[string]bool{}, map[string]bool{}
	for k := range x.kinds {
		if k != "null" {
			refK[k] = true
		}
	}
	for k := range y.kinds {
		if k != "null" {
			tgtK[k] = true
		}
	}
	if len(refK) == 0 || len(tgtK) == 0 {
		return Difference{}, false
	}
	mismatch := false
	if isXML {
		if freeTextRE.MatchString(leafName(p)) {
			return Difference{}, false
		}
		mismatch = true
		for k := range tgtK {
			if refK[k] {
				mismatch = false
			}
		}
	} else {
		for k := range tgtK {
			if !refK[k] {
				mismatch = true
			}
		}
	}
	if !mismatch {
		return Difference{}, false
	}
	return Difference{Path: p, Kind: KindType, Reference: x.kindList(), Target: y.kindList(), Count: x.count}, true
}

func valueDiff(p string, x, y *schemaNode) (Difference, bool) {
	if x.element || y.element || x.anyRedacted() {
		return Difference{}, false
	}
	xv, xok := x.singleValue()
	yv, yok := y.singleValue()
	if !xok || !yok || xv == yv {
		return Difference{}, false
	}
	leaf := leafName(p)
	if volatile(p) {
		// Keys and URLs are still compared by shape when PMS always uses
		// one ("/library/metadata/{id}/children").
		if strings.HasPrefix(xv, "/") && strings.HasPrefix(yv, "/") && regexp.MustCompile(`(?i)(key|url|thumb|art)$`).MatchString(leaf) {
			if xs, ys := urlShape(xv), urlShape(yv); xs != ys {
				return Difference{Path: p, Kind: KindValue, Reference: xs, Target: ys, Count: x.count}, true
			}
		}
		return Difference{}, false
	}
	if boolEquivalent(xv, yv) {
		return Difference{}, false
	}
	return Difference{Path: p, Kind: KindValue, Reference: truncate(xv, 80), Target: truncate(yv, 80), Count: x.count}, true
}

func boolEquivalent(x, y string) bool {
	norm := func(s string) string {
		switch s {
		case "1", "true":
			return "1"
		case "0", "false":
			return "0"
		}
		return s
	}
	return norm(x) == norm(y)
}

var digitsRE = regexp.MustCompile(`/[0-9]+`)

func urlShape(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '/'); j >= 0 {
			s = s[j:]
		}
	}
	if j := strings.IndexAny(s, "?#"); j >= 0 {
		s = s[:j]
	}
	return digitsRE.ReplaceAllString(s, "/{id}")
}

// orderDiffs reports child elements the target emits in the opposite order
// to PMS (only for pairs PMS always orders one way).
func orderDiffs(ref, tgt *schema) []Difference {
	var out []Difference
	parents := make([]string, 0, len(tgt.before))
	for p := range tgt.before {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	for _, p := range parents {
		rb := ref.before[p]
		if rb == nil {
			continue
		}
		var bad []string
		for pair := range tgt.before[p] {
			a, b := pair[0], pair[1]
			if rb[[2]string{b, a}] && !rb[[2]string{a, b}] && !tgt.before[p][[2]string{b, a}] {
				bad = append(bad, b+" before "+a)
			}
		}
		if len(bad) == 0 {
			continue
		}
		sort.Strings(bad)
		out = append(out, Difference{Path: p + "/#order", Kind: KindValue, Reference: strings.Join(bad, "; "), Target: "reversed", Count: ref.nodes[p].count})
	}
	return out
}
