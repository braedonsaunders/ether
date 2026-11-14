package collections

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
)

// SourceKind is one collection source kind: how it is described to the
// console, how its saved source is validated, and how it gathers titles.
// Gather runs on background build/refresh paths only, never on a request
// path (list sources included).
//
// To add a kind: implement SourceKind, then register it. Built-ins register
// in builtinSourceKinds below; runtime additions (plugin list sources) call
// Service.RegisterSourceKind, or register on a KindRegistry passed with
// WithKindRegistry. The kind then appears in Descriptors and is served to
// the console by extend; no console code names individual kinds.
type SourceKind interface {
	// ID is the kind persisted as CollectionSource.Kind.
	ID() string
	Describe() KindDescribe
	Validate(ve *ValidationError, path string, src core.CollectionSource)
	Gather(g *Gather)
}

// minimumSourceRefresh respects source-specific lower bounds on refresh cadence.
func (s *Service) minimumSourceRefresh(sp core.CollectionSpec) time.Duration {
	var min time.Duration
	for _, src := range sp.Sources {
		if k, ok := s.kindRegistry().Lookup(src.Kind); ok {
			if timed, ok := k.(interface{ MinRefresh() time.Duration }); ok && timed.MinRefresh() > min {
				min = timed.MinRefresh()
			}
		}
	}
	return min
}

// KindDescribe carries everything the console needs to offer a kind in the
// "Add source" picker and edit its params.
type KindDescribe struct {
	Label      extend.Text
	Hint       extend.Text
	Icon       string
	Order      int
	Editor     string
	Params     []extend.ParamField
	Defaults   string // JSON object merged into a new source
	MediaTypes []string
}

// Gather is the context one source gather runs with. Kinds append to Res.
type Gather struct {
	Strict bool // on-demand catalogue refreshes preserve old membership on upstream errors
	Ctx    context.Context
	Svc    *Service
	Spec   *core.CollectionSpec
	Src    core.CollectionSource
	Rules  []*rule
	Want   int
	Kinds  []string
	Res    sourceResult
}

// Fail ends a preview when a source cannot be evaluated.
func (g *Gather) Fail(err error) { g.Res.err = err }

func (g *Gather) Warn(format string, args ...any) {
	g.Res.warnings = append(g.Res.warnings, fmt.Sprintf(format, args...))
}

type previewContextKey struct{}

// IsPreview reports whether a gather belongs to an owner-initiated preview.
func IsPreview(ctx context.Context) bool { return ctx.Value(previewContextKey{}) == true }

// PreviewItems is how many items an owner preview of a remote list source
// reads (SPEC: preview is the first 20).
const PreviewItems = 20

// ExternalRef identifies one item from an external or plugin list source.
type ExternalRef struct {
	MetadataProvider string
	ProviderID       string
	TMDB             int64
	IMDB             string
	TVDB             int64
	Type             string // "movie" or "show"
	Title            string
	Year             int
}

// ResolveExternal resolves external references to catalogue titles and
// appends the matched ones to the gather result. unidentified counts items
// the source read but could not identify at all (no id Ether can match);
// they are reported with the references that did not resolve, as one
// "N of M items … could not be matched" warning plus its reasons. It
// returns the matched titles and the unmatched count.
func (g *Gather) ResolveExternal(sourceLabel string, refs []ExternalRef, unidentified int) ([]core.Title, int) {
	if sourceLabel == "" {
		sourceLabel = sourceName(g.Src)
	}
	var titles []core.Title
	unresolved, pending := 0, 0
	var details []string
	if len(refs) > 0 {
		ext := make([]extRef, len(refs))
		for i, r := range refs {
			ext[i] = extRef{
				MetadataProvider: r.MetadataProvider, ProviderID: r.ProviderID,
				TMDB:  r.TMDB,
				IMDB:  r.IMDB,
				TVDB:  r.TVDB,
				Type:  r.Type,
				Title: r.Title,
				Year:  r.Year,
			}
		}
		titles, unresolved, pending, details = g.Svc.resolveExternalDetailed(g.Ctx, g.Src.Kind, ext)
		g.Res.titles = append(g.Res.titles, titles...)
	}
	if unmatched := unresolved + unidentified; unmatched > 0 {
		g.Warn("%d of %d items in %s could not be matched to the catalogue", unmatched, len(refs)+unidentified, sourceLabel)
	}
	for _, detail := range details {
		g.Warn("%s: %s", sourceLabel, detail)
	}
	if pending > 0 {
		g.Warn("%d items in %s are still being matched; they will appear after the next refresh", pending, sourceLabel)
	}
	return titles, unresolved + unidentified + pending
}

// Titles returns the titles gathered so far.
func (g *Gather) Titles() []core.Title { return append([]core.Title(nil), g.Res.titles...) }

// Warnings returns the warnings recorded so far.
func (g *Gather) Warnings() []string { return append([]string(nil), g.Res.warnings...) }

// Add records a validation error on field if one is not already present.
func (e *ValidationError) Add(field, msg string) { e.add(field, msg) }

// NeedTMDB reports whether TMDB is available, recording a warning.
func (g *Gather) NeedTMDB() bool {
	if g.Svc.api == nil || g.Svc.api.c == nil {
		g.Warn("%s needs TMDB, which is not configured", sourceName(g.Src))
		return false
	}
	return true
}

// Discover runs a discover-backed query for one TMDB kind into the result.
func (g *Gather) Discover(kind string, q url.Values, what string) {
	g.Svc.discoverInto(g.Ctx, g.Spec, kind, q, g.Rules, g.Want, &g.Res, what)
}

// KindRegistry is the set of source kinds a Service can validate and run.
// Safe for concurrent use; plugins register kinds at runtime.
type KindRegistry struct {
	mu    sync.RWMutex
	kinds map[string]SourceKind
	order []string
}

// NewKindRegistry returns a registry holding the built-in kinds.
func NewKindRegistry() *KindRegistry {
	r := &KindRegistry{kinds: map[string]SourceKind{}}
	for _, k := range builtinSourceKinds() {
		if err := r.Register(k); err != nil {
			panic("collections: " + err.Error())
		}
	}
	return r
}

// Register adds a kind. IDs are unique; a duplicate keeps the original.
func (r *KindRegistry) Register(k SourceKind) error {
	if k == nil || k.ID() == "" {
		return fmt.Errorf("source kind needs an id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.kinds[k.ID()]; ok {
		return fmt.Errorf("source kind %q already registered", k.ID())
	}
	r.kinds[k.ID()] = k
	r.order = append(r.order, k.ID())
	return nil
}

// Unregister removes a runtime source kind by id.
func (r *KindRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.kinds[id]; !ok {
		return
	}
	delete(r.kinds, id)
	out := r.order[:0]
	for _, cur := range r.order {
		if cur != id {
			out = append(out, cur)
		}
	}
	r.order = out
}

// Lookup returns the kind with id, if registered.
func (r *KindRegistry) Lookup(id string) (SourceKind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[id]
	return k, ok
}

// Kinds returns every registered kind in registration order.
func (r *KindRegistry) Kinds() []SourceKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SourceKind, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.kinds[id])
	}
	return out
}

// IDs returns the registered kind ids, sorted.
func (r *KindRegistry) IDs() []string {
	r.mu.RLock()
	out := append([]string(nil), r.order...)
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Descriptors returns the collectionSource descriptors for every kind, for
// the extend registry. Built-ins register through
// internal/app/extend_collections.go.
func (r *KindRegistry) Descriptors() []extend.Descriptor {
	kinds := r.Kinds()
	out := make([]extend.Descriptor, 0, len(kinds))
	for _, k := range kinds {
		d := k.Describe()
		out = append(out, extend.Descriptor{
			Kind: extend.KindCollectionSource, ID: k.ID(), Owner: extend.Builtin,
			Label: d.Label, Hint: d.Hint, Icon: d.Icon, Order: d.Order,
		}.WithMeta(extend.CollectionSourceMeta{
			Params: d.Params, Editor: d.Editor,
			Defaults: []byte(d.Defaults), MediaTypes: d.MediaTypes,
		}))
	}
	return out
}

func (s *Service) kindRegistry() *KindRegistry {
	if s.kinds != nil {
		return s.kinds
	}
	return defaultKinds
}

func (s *Service) validate(sp *core.CollectionSpec) error { return validateWith(s.kindRegistry(), sp) }

// RegisterSourceKind adds a source kind to this service (plugin list
// sources, Stremio catalogs). The kind is then valid in specs, gathers on
// background builds, and should also be served as a descriptor: call
// KindRegistry.Descriptors (or Service.SourceDescriptors) and register the
// result with the extend registry.
func (s *Service) RegisterSourceKind(k SourceKind) error { return s.kindRegistry().Register(k) }

// UnregisterSourceKind removes a runtime source kind from this service.
func (s *Service) UnregisterSourceKind(id string) { s.kindRegistry().Unregister(id) }

// KindRegistry exposes this service's source-kind registry.
func (s *Service) KindRegistry() *KindRegistry { return s.kindRegistry() }

// SourceDescriptors returns extend descriptors for every registered kind.
func (s *Service) SourceDescriptors() []extend.Descriptor { return s.kindRegistry().Descriptors() }

// BuiltinDescriptors returns descriptors for the built-in kinds, without a
// Service (app startup registers these before services exist).
func BuiltinDescriptors() []extend.Descriptor { return defaultKinds.Descriptors() }

// defaultKinds validates specs before a Service exists (Validate). Service
// instances get their own registry from NewKindRegistry, so runtime
// registrations never mutate it.
var defaultKinds = NewKindRegistry()

// SourceKinds returns the built-in kinds (used by tests and NewKindRegistry).
func builtinSourceKinds() []SourceKind {
	return []SourceKind{
		catalogKind{kindDef: def("catalog", 10, extend.T("Whole catalogue"), extend.T("Everything browsable in Ether"), "library", "none", nil, `{}`)},
		tmdbListKind{},
		tmdbUserListKind{},
		titlesKind{},
		friendsWatchlistKind{},
		personKind{},
		lookupKind{id: "tmdbCollection", order: 50, label: extend.T("TMDB collection"), hint: extend.T("A franchise such as “The Lord of the Rings”"), icon: "layers", lookup: "collection", entity: "collection"},
		lookupKind{id: "tmdbKeyword", order: 60, label: extend.T("Keyword"), hint: extend.T("Titles tagged with a TMDB keyword"), icon: "tag", lookup: "keyword", entity: "keyword", param: "with_keywords"},
		lookupKind{id: "tmdbCompany", order: 70, label: extend.T("Studio"), hint: extend.T("Titles from a production company"), icon: "clapperboard", lookup: "company", entity: "company", param: "with_companies"},
		lookupKind{id: "tmdbNetwork", order: 80, label: extend.T("TV network"), hint: extend.T("Shows from a network such as HBO"), icon: "tv", lookup: "network", entity: "network", param: "with_networks"},
		watchProviderKind{},
		discoverKind{},
		externalKind{id: "traktList", order: 110, label: extend.T("Trakt list"), hint: extend.T("A public Trakt list URL"), icon: "list-video", placeholder: "https://trakt.tv/users/…/lists/…",
			validateURL: func(u string) error { _, err := parseTraktURL(u); return err },
			fetch: func(s *Service, ctx context.Context, src core.CollectionSource) ([]extRef, error) {
				return s.fetchTrakt(ctx, src.URL)
			}},
		externalKind{id: "letterboxdList", order: 120, label: extend.T("Letterboxd list"), hint: extend.T("A public Letterboxd list URL"), icon: "list-video", placeholder: "https://letterboxd.com/…/list/…",
			validateURL: func(u string) error { _, err := parseLetterboxdURL(u); return err },
			fetch: func(s *Service, ctx context.Context, src core.CollectionSource) ([]extRef, error) {
				return s.fetchLetterboxd(ctx, src.URL)
			}},
		externalKind{id: "mdblist", order: 130, label: extend.T("MDBList"), hint: extend.T("A public MDBList URL"), icon: "list-video", placeholder: "https://mdblist.com/lists/…",
			validateURL: func(u string) error { _, err := parseMDBListURL(u); return err },
			fetch: func(s *Service, ctx context.Context, src core.CollectionSource) ([]extRef, error) {
				return s.fetchMDBList(ctx, src.URL)
			}},
		externalKind{id: "anilistList", order: 140, label: extend.T("AniList list"), hint: extend.T("A public AniList anime list URL"), icon: "list-video", placeholder: "https://anilist.co/user/…/animelist/Completed",
			validateURL: func(u string) error { _, _, err := parseAniListURL(u); return err },
			fetch: func(s *Service, ctx context.Context, src core.CollectionSource) ([]extRef, error) {
				return s.fetchAniList(ctx, src.URL)
			}},
		stevenLuKind{},
	}
}

type kindDef struct {
	id    string
	order int
	label extend.Text
	hint  extend.Text
	icon  string
}

func def(id string, order int, label, hint extend.Text, icon, editor string, params []extend.ParamField, defaults string) kindDef {
	return kindDef{id: id, order: order, label: label, hint: hint, icon: icon}
}

func (d kindDef) ID() string { return d.id }

func lookupParams(lookup string) []extend.ParamField {
	return []extend.ParamField{
		{Name: "id", Kind: "number", Label: extend.T("Entry"), Required: true, Lookup: lookup},
		{Name: "label", Kind: "text", Label: extend.T("Name")},
	}
}

func validateLookup(ve *ValidationError, path string, src core.CollectionSource) {
	if src.ID <= 0 && src.Label == "" {
		ve.add(path+".id", "choose a TMDB entry (an id or a name to look up)")
	}
}

// ---- catalog ----

type catalogKind struct{ kindDef }

func (catalogKind) Describe() KindDescribe {
	return KindDescribe{Label: extend.T("Whole catalogue"), Hint: extend.T("Everything browsable in Ether"),
		Icon: "library", Order: 10, Editor: "none", Defaults: `{}`}
}
func (catalogKind) Validate(*ValidationError, string, core.CollectionSource) {}
func (catalogKind) Gather(g *Gather) {
	if topic := g.Src.Params["topic"]; topic != "" {
		// A saved descriptive collection keeps its topic as a bounded local
		// catalogue source rather than widening into the whole library.
		f := core.Filter{Topic: topic, LocalOnly: true}
		switch g.Spec.MediaType {
		case MediaMovie:
			f.Type = core.TypeMovie
		case MediaShow:
			f.Type = core.TypeShow
		}
		for offset := 0; offset < 3072; offset += 500 {
			page, err := g.Svc.cat.Browse(g.Ctx, core.BrowseQuery{Filter: f, Sort: core.SortPopularity, Desc: true, Offset: offset, Limit: 500, NoTotal: true})
			if err != nil {
				g.Fail(err)
				return
			}
			for _, title := range page.Items {
				g.Res.keys = append(g.Res.keys, title.Key)
			}
			if len(page.Items) < 500 {
				break
			}
		}
		return
	}
	g.Res.all = true
	if g.Spec.ReleaseMode != "upcoming" && g.Spec.ReleaseMode != "mixed" {
		return
	}
	if !g.NeedTMDB() {
		return
	}
	now := g.Svc.now().UTC()
	for _, kind := range g.Kinds {
		prefix := "primary_release_date"
		if kind == "tv" {
			prefix = "first_air_date"
		}
		q := url.Values{prefix + ".gte": {now.Format("2006-01-02")}, prefix + ".lte": {now.AddDate(0, 0, g.Spec.LookaheadDays).Format("2006-01-02")}}
		g.Discover(kind, q, "upcoming discovery")
	}
}

// ---- tmdbList ----

type tmdbListKind struct{}

func (tmdbListKind) ID() string { return "tmdbList" }
func (tmdbListKind) Describe() KindDescribe {
	both := []string{"movie", "show"}
	return KindDescribe{
		Label: extend.T("TMDB chart"), Hint: extend.T("Trending, popular, top rated, now playing…"),
		Icon: "trending-up", Order: 20, Editor: "tmdbChart", Defaults: `{"list":"trending_week","mediaType":"movie"}`,
		Params: []extend.ParamField{
			{Name: "mediaType", Kind: "select", Label: extend.T("Media"), Required: true, Options: []extend.ParamOption{
				{Value: "movie", Label: extend.T("Movies")}, {Value: "show", Label: extend.T("TV")},
			}},
			{Name: "list", Kind: "select", Label: extend.T("Chart"), Required: true, Options: []extend.ParamOption{
				{Value: "trending_day", Label: extend.T("Trending today"), Media: both},
				{Value: "trending_week", Label: extend.T("Trending this week"), Media: both},
				{Value: "popular", Label: extend.T("Popular"), Media: both},
				{Value: "top_rated", Label: extend.T("Top rated"), Media: both},
				{Value: "now_playing", Label: extend.T("Now in cinemas"), Media: []string{"movie"}},
				{Value: "upcoming", Label: extend.T("Upcoming"), Media: []string{"movie"}},
				{Value: "airing_today", Label: extend.T("Airing today"), Media: []string{"show"}},
				{Value: "on_the_air", Label: extend.T("On the air"), Media: []string{"show"}},
			}},
		},
	}
}
func (tmdbListKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if !tmdbLists[src.List] {
		ve.add(path+".list", "unknown TMDB list "+strconv.Quote(src.List))
	}
	if src.MediaType != "" && src.MediaType != MediaMovie && src.MediaType != MediaShow {
		ve.add(path+".mediaType", `must be "movie" or "show"`)
	}
}
func (tmdbListKind) Gather(g *Gather) {
	n := g.Want
	if n > 100 {
		n = 100
	}
	for _, kind := range g.Kinds {
		items, err := g.Svc.cat.List(core.WithCatalogCandidates(g.Ctx), g.Src.List, coreType(kind), n)
		if err != nil {
			g.Warn("TMDB list %s: %v", g.Src.List, err)
			continue
		}
		g.Res.titles = append(g.Res.titles, items...)
	}
}

// ---- titles ----

type titlesKind struct{}

func (titlesKind) ID() string { return "titles" }
func (titlesKind) Describe() KindDescribe {
	return KindDescribe{Label: extend.T("Hand-picked titles"), Hint: extend.T("Choose titles one by one, in your order"),
		Icon: "bookmark", Order: 30, Editor: "titles", Defaults: `{"keys":[]}`,
		Params: []extend.ParamField{{Name: "keys", Kind: "title", Label: extend.T("Titles"), Required: true}}}
}
func (titlesKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if len(src.Keys) == 0 {
		ve.add(path+".keys", "pick at least one title")
	}
	if len(src.Keys) > maxMembers {
		ve.add(path+".keys", fmt.Sprintf("at most %d titles", maxMembers))
	}
	for _, k := range src.Keys {
		rk, err := core.ParseRatingKey(k)
		if err != nil || (rk.Kind() != core.KindMovie && rk.Kind() != core.KindShow) {
			ve.add(path+".keys", "only movies and shows can be hand-picked ("+k+")")
			break
		}
	}
}
func (titlesKind) Gather(g *Gather) {
	for _, k := range g.Src.Keys {
		if rk, err := core.ParseRatingKey(k); err == nil {
			g.Res.keys = append(g.Res.keys, rk)
		}
	}
}

// ---- tmdbPerson ----

type personKind struct{}

func (personKind) ID() string { return "tmdbPerson" }
func (personKind) Describe() KindDescribe {
	params := append(lookupParams("person"), extend.ParamField{Name: "role", Kind: "select", Label: extend.T("Role"), Options: []extend.ParamOption{
		{Value: "", Label: extend.T("Every credit")}, {Value: "cast", Label: extend.T("Cast")},
		{Value: "director", Label: extend.T("Director")}, {Value: "writer", Label: extend.T("Writer")}, {Value: "crew", Label: extend.T("Crew")},
	}})
	return KindDescribe{Label: extend.T("Person's filmography"), Hint: extend.T("Everything an actor or director made"),
		Icon: "user-round", Order: 40, Editor: "lookup", Defaults: `{"id":0}`, Params: params}
}
func (personKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	validateLookup(ve, path, src)
	if !personRoles[src.Role] {
		ve.add(path+".role", "role is cast, director, writer or crew")
	}
}
func (personKind) Gather(g *Gather) {
	if !g.NeedTMDB() {
		return
	}
	r, err := g.Svc.names.resolve(g.Ctx, "person", ref{ID: g.Src.ID, Name: g.Src.Label}, "")
	if err != nil || r.ID <= 0 {
		g.Warn("couldn't find the person %q on TMDB", firstNonEmpty(g.Src.Label, strconv.FormatInt(g.Src.ID, 10)))
		return
	}
	items, name, err := g.Svc.api.personCredits(g.Ctx, r.ID, g.Src.Role)
	if err != nil {
		g.Warn("TMDB credits for %s: %v", firstNonEmpty(r.Name, g.Src.Label), err)
		return
	}
	g.Res.resolvedID, g.Res.resolvedLabel = r.ID, firstNonEmpty(name, r.Name)
	g.Res.titles = items
}

// ---- lookup kinds (collection, keyword, company, network) ----

type lookupKind struct {
	id     string
	order  int
	label  extend.Text
	hint   extend.Text
	icon   string
	lookup string
	entity string
	param  string // discover parameter; empty for tmdbCollection
}

func (k lookupKind) ID() string { return k.id }
func (k lookupKind) Describe() KindDescribe {
	return KindDescribe{Label: k.label, Hint: k.hint, Icon: k.icon, Order: k.order, Editor: "lookup",
		Defaults: `{"id":0}`, Params: lookupParams(k.lookup)}
}
func (k lookupKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	validateLookup(ve, path, src)
}
func (k lookupKind) Gather(g *Gather) {
	if k.entity == "collection" {
		id := g.Src.ID
		if id <= 0 {
			if !g.NeedTMDB() {
				return
			}
			r, err := g.Svc.names.resolve(g.Ctx, "collection", ref{Name: g.Src.Label}, "")
			if err != nil {
				g.Warn("couldn't find a TMDB collection called %q", g.Src.Label)
				return
			}
			id, g.Res.resolvedID, g.Res.resolvedLabel = r.ID, r.ID, r.Name
		}
		name, items, err := g.Svc.cat.Collection(g.Ctx, id)
		if err != nil {
			g.Warn("TMDB collection %d: %v", id, err)
			return
		}
		if g.Res.resolvedLabel == "" && g.Src.Label == "" {
			g.Res.resolvedID, g.Res.resolvedLabel = id, name
		}
		g.Res.titles = items
		return
	}
	if !g.NeedTMDB() {
		return
	}
	r, err := g.Svc.names.resolve(g.Ctx, k.entity, ref{ID: g.Src.ID, Name: g.Src.Label}, "")
	if err != nil || r.ID <= 0 {
		g.Warn("couldn't find the %s %q on TMDB", k.entity, firstNonEmpty(g.Src.Label, strconv.FormatInt(g.Src.ID, 10)))
		return
	}
	g.Res.resolvedID, g.Res.resolvedLabel = r.ID, r.Name
	kinds := g.Kinds
	if k.entity == "network" {
		kinds = []string{"tv"}
	}
	for _, kind := range kinds {
		g.Discover(kind, url.Values{k.param: {strconv.FormatInt(r.ID, 10)}}, k.entity+" "+strconv.Quote(r.Name))
	}
}

// ---- watchProvider ----

type watchProviderKind struct{}

func (watchProviderKind) ID() string { return "watchProvider" }
func (watchProviderKind) Describe() KindDescribe {
	params := append(lookupParams("watchProvider"), extend.ParamField{Name: "region", Kind: "text", Label: extend.T("Region")})
	return KindDescribe{Label: extend.T("Streaming service"), Hint: extend.T("What's on a service in your region"),
		Icon: "monitor-play", Order: 90, Editor: "lookup", Defaults: `{"id":0}`, Params: params}
}
func (watchProviderKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if src.ID <= 0 && src.Label == "" {
		ve.add(path+".id", "choose a streaming service")
	}
	if src.Region != "" && !regionRe.MatchString(src.Region) {
		ve.add(path+".region", "use a two-letter country code such as US or GB")
	}
}
func (watchProviderKind) Gather(g *Gather) {
	if !g.NeedTMDB() {
		return
	}
	region := g.Svc.region(g.Src.Region)
	r, err := g.Svc.names.resolve(g.Ctx, "watchProvider", ref{ID: g.Src.ID, Name: g.Src.Label}, region)
	if err != nil || r.ID <= 0 {
		g.Warn("couldn't find the streaming service %q in %s", firstNonEmpty(g.Src.Label, strconv.FormatInt(g.Src.ID, 10)), region)
		return
	}
	g.Res.resolvedID, g.Res.resolvedLabel = r.ID, r.Name
	for _, kind := range g.Kinds {
		g.Discover(kind, url.Values{
			"with_watch_providers":          {strconv.FormatInt(r.ID, 10)},
			"watch_region":                  {region},
			"with_watch_monetization_types": {"flatrate|free|ads"},
		}, "watch provider "+strconv.Quote(r.Name))
	}
}

// ---- tmdbDiscover ----

type discoverKind struct{}

func (discoverKind) ID() string { return "tmdbDiscover" }
func (discoverKind) Describe() KindDescribe {
	return KindDescribe{Label: extend.T("TMDB discover query"), Hint: extend.T("Raw TMDB discover parameters"),
		Icon: "search", Order: 100, Editor: "discover", Defaults: `{"params":{}}`,
		Params: []extend.ParamField{{Name: "params", Kind: "list", Label: extend.T("Parameters"), Required: true}}}
}
func (discoverKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if len(src.Params) == 0 {
		ve.add(path+".params", "add at least one discover parameter")
	}
	for k := range src.Params {
		if !discoverParamRe.MatchString(k) {
			ve.add(path+".params", "unsupported discover parameter "+strconv.Quote(k))
		}
	}
}
func (discoverKind) Gather(g *Gather) {
	if !g.NeedTMDB() {
		return
	}
	for _, kind := range g.Kinds {
		q, err := g.Svc.discoverParams(g.Ctx, kind, g.Src.Params)
		if err != nil {
			g.Warn("discover: %v", err)
			return
		}
		g.Discover(kind, q, "discover")
	}
}

// ---- external public lists ----

type externalKind struct {
	id          string
	order       int
	label       extend.Text
	hint        extend.Text
	icon        string
	placeholder string
	validateURL func(string) error
	fetch       func(*Service, context.Context, core.CollectionSource) ([]extRef, error)
}

func (k externalKind) ID() string { return k.id }
func (k externalKind) Describe() KindDescribe {
	return KindDescribe{Label: k.label, Hint: k.hint, Icon: k.icon, Order: k.order, Editor: "urlList", Defaults: `{"url":""}`,
		Params: []extend.ParamField{{Name: "url", Kind: "url", Label: extend.T("List URL"), Required: true, Placeholder: k.placeholder}}}
}
func (k externalKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if err := k.validateURL(src.URL); err != nil {
		ve.add(path+".url", err.Error())
	}
}
func (k externalKind) Gather(g *Gather) {
	refs, err := k.fetch(g.Svc, g.Ctx, g.Src)
	var partial *partialListError
	if errors.As(err, &partial) && len(refs) > 0 {
		g.Warn("%v", err)
	} else if err != nil {
		g.Warn("%v", err)
		return
	}
	if len(refs) == 0 {
		g.Warn("%s is empty", sourceName(g.Src))
		return
	}
	titles, unresolved, pending := g.Svc.resolveExternal(g.Ctx, g.Src.Kind, refs)
	g.Res.titles = titles
	if unresolved > 0 {
		g.Warn("%d of %d items in %s could not be matched to TMDB", unresolved, len(refs), sourceName(g.Src))
	}
	if pending > 0 {
		g.Warn("%d items in %s are still being matched; they will appear after the next refresh", pending, sourceName(g.Src))
	}
}

// ---- stevenlu ----

type stevenLuKind struct{}

func (stevenLuKind) ID() string { return "stevenlu" }
func (stevenLuKind) Describe() KindDescribe {
	opt := func(v, l string) extend.ParamOption {
		return extend.ParamOption{Value: v, Label: extend.Text{Source: l}}
	}
	return KindDescribe{Label: extend.T("StevenLu popular movies"), Hint: extend.T("Movies people are talking about"),
		Icon: "flame", Order: 150, Editor: "listSelect", Defaults: `{"list":"movies"}`,
		Params: []extend.ParamField{{Name: "list", Kind: "select", Label: extend.T("List"), Required: true, Options: []extend.ParamOption{
			{Value: "movies", Label: extend.T("All popular movies")},
			opt("movies-imdb-min7", "IMDb 7+"), opt("movies-imdb-min8", "IMDb 8+"),
			opt("movies-metacritic-min50", "Metacritic 50+"), opt("movies-metacritic-min60", "Metacritic 60+"),
			opt("movies-metacritic-min70", "Metacritic 70+"), opt("movies-metacritic-min80", "Metacritic 80+"),
			opt("movies-rottentomatoes-min70", "Rotten Tomatoes 70+"), opt("movies-rottentomatoes-min80", "Rotten Tomatoes 80+"),
		}}}}
}
func (stevenLuKind) Validate(ve *ValidationError, path string, src core.CollectionSource) {
	if _, err := stevenLuList(src.List); err != nil {
		ve.add(path+".list", err.Error())
	}
}
func (stevenLuKind) Gather(g *Gather) {
	refs, err := g.Svc.fetchStevenLu(g.Ctx, g.Src.List)
	var partial *partialListError
	if errors.As(err, &partial) && len(refs) > 0 {
		g.Warn("%v", err)
	} else if err != nil {
		g.Warn("%v", err)
		return
	}
	if len(refs) == 0 {
		g.Warn("%s is empty", sourceName(g.Src))
		return
	}
	titles, unresolved, pending := g.Svc.resolveExternal(g.Ctx, g.Src.Kind, refs)
	g.Res.titles = titles
	if unresolved > 0 {
		g.Warn("%d of %d items in %s could not be matched to TMDB", unresolved, len(refs), sourceName(g.Src))
	}
	if pending > 0 {
		g.Warn("%d items in %s are still being matched; they will appear after the next refresh", pending, sourceName(g.Src))
	}
}
