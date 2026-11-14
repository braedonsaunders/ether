package livetv

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/extend"
)

// Fetched is what a source kind yields on import or refresh: channels (with
// their ordered stream inputs) and guide data. ImportSource persists it
// atomically, preserving owner mapping, numbering and access rules.
type Fetched struct {
	Channels []Channel
	Guide    XMLGuide
	// AddonInputs marks channels whose inputs an add-on owns (ids carry
	// AddonInputPrefix); see addon_streams.go.
	AddonInputs bool
	// ProviderNumbers marks a lineup whose every number comes from the
	// provider. Channels imported before provider numbers were recorded
	// then take the provider's number instead of keeping a generated one.
	ProviderNumbers bool
}

// SourceKind is one Live TV source kind: how the console describes it, how
// a saved Source is recognised as this kind, and how it yields channels,
// inputs and guide data. Fetch runs on import/refresh only, never on a
// request path; playback reads stored channels.
//
// To add a kind: implement SourceKind and register it (built-ins in
// builtinSourceKinds; runtime kinds such as plugin channel sources, which
// include Stremio `tv` add-ons, via Service.RegisterSourceKind). Then serve
// the result of KindRegistry.Descriptors through the extend registry. A kind that needs
// its own Source fields beyond playlist/guide/auth must extend Source
// additively and classify in Matches before "m3u".
type SourceKind interface {
	ID() string
	Describe() SourceDescribe
	Matches(src Source) bool
	// Fetch fills src.Playlist/src.Guide from URLs as needed (so they are
	// stored for the next refresh) and returns the parsed result.
	Fetch(ctx context.Context, s *Service, src *Source) (Fetched, error)
}

// SourceDescribe carries the console-facing description of a source kind.
type SourceDescribe struct {
	Label          extend.Text
	Hint           extend.Text
	Icon           string
	Order          int
	Editor         string
	Params         []extend.ParamField
	YieldsChannels bool
	YieldsGuide    bool
}

// KindRegistry holds the Live TV source kinds. Safe for concurrent use.
type KindRegistry struct {
	mu           sync.RWMutex
	kinds        map[string]SourceKind
	order        []string
	streamOpener func(ctx context.Context, c Channel, src StreamSource) (string, http.Header, error)
}

// NewKindRegistry returns a registry holding the built-in kinds.
func NewKindRegistry() *KindRegistry {
	r := &KindRegistry{kinds: map[string]SourceKind{}}
	for _, k := range builtinSourceKinds() {
		if err := r.Register(k); err != nil {
			panic("livetv: " + err.Error())
		}
	}
	return r
}

func (r *KindRegistry) Register(k SourceKind) error {
	if k == nil || k.ID() == "" {
		return fmt.Errorf("live TV source kind needs an id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.kinds[k.ID()]; ok {
		return fmt.Errorf("live TV source kind %q already registered", k.ID())
	}
	r.kinds[k.ID()] = k
	if strings.HasPrefix(k.ID(), "plugin:") {
		if idx := slices.Index(r.order, "m3u"); idx >= 0 {
			r.order = slices.Insert(r.order, idx, k.ID())
			return nil
		}
	}
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

func (r *KindRegistry) Lookup(id string) (SourceKind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[id]
	return k, ok
}

func (r *KindRegistry) Kinds() []SourceKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SourceKind, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.kinds[id])
	}
	return out
}

// Classify returns the first registered kind that recognises src, in
// registration order (xtream and plugin kinds before m3u, so an Xtream
// login or plugin source is never mistaken for a plain playlist URL).
func (r *KindRegistry) Classify(src Source) (SourceKind, bool) {
	for _, k := range r.Kinds() {
		if k.Matches(src) {
			return k, true
		}
	}
	return nil, false
}

// Descriptors returns the liveTVSource descriptors for every kind.
func (r *KindRegistry) Descriptors() []extend.Descriptor {
	kinds := r.Kinds()
	out := make([]extend.Descriptor, 0, len(kinds))
	for _, k := range kinds {
		d := k.Describe()
		out = append(out, extend.Descriptor{
			Kind: extend.KindLiveTVSource, ID: k.ID(), Owner: extend.Builtin,
			Label: d.Label, Hint: d.Hint, Icon: d.Icon, Order: d.Order,
		}.WithMeta(extend.LiveTVSourceMeta{
			Params: d.Params, Editor: d.Editor,
			YieldsChannels: d.YieldsChannels, YieldsGuide: d.YieldsGuide, Test: true,
		}))
	}
	return out
}

var defaultKinds = NewKindRegistry()

// BuiltinDescriptors returns descriptors for the built-in kinds.
func BuiltinDescriptors() []extend.Descriptor { return defaultKinds.Descriptors() }

func (s *Service) kindRegistry() *KindRegistry {
	if s.kinds != nil {
		return s.kinds
	}
	return defaultKinds
}

// RegisterSourceKind adds a source kind to this service.
func (s *Service) RegisterSourceKind(k SourceKind) error { return s.kindRegistry().Register(k) }

// UnregisterSourceKind removes a runtime source kind from this service.
func (s *Service) UnregisterSourceKind(id string) { s.kindRegistry().Unregister(id) }

// KindRegistry exposes this service's source-kind registry.
func (s *Service) KindRegistry() *KindRegistry { return s.kindRegistry() }

// Now returns the service's clock reading.
func (s *Service) Now() time.Time { return s.o.Now() }

// SetStreamOpener installs a lazy stream resolver for plugin:// stream inputs.
func (s *Service) SetStreamOpener(fn func(ctx context.Context, c Channel, src StreamSource) (string, http.Header, error)) {
	r := s.kindRegistry()
	r.mu.Lock()
	r.streamOpener = fn
	r.mu.Unlock()
}

// SourceKindOf classifies a saved source ("xtream", "guideOnly", "m3u").
func (s *Service) SourceKindOf(src Source) string {
	if k, ok := s.kindRegistry().Classify(src); ok {
		return k.ID()
	}
	return ""
}

func builtinSourceKinds() []SourceKind {
	return []SourceKind{providerGuideKind{}, xtreamKind{}, guideOnlyKind{}, m3uKind{}, virtualKind{}}
}

func authParams() []extend.ParamField {
	return []extend.ParamField{
		{Name: "name", Kind: "text", Label: extend.T("Source name"), Required: true},
	}
}

type m3uKind struct{}

func (m3uKind) ID() string { return "m3u" }
func (m3uKind) Describe() SourceDescribe {
	return SourceDescribe{
		Label: extend.T("M3U playlist"), Hint: extend.T("An M3U playlist of live streams, with an optional XMLTV guide."),
		Icon: "list-video", Order: 20, Editor: "playlist", YieldsChannels: true, YieldsGuide: true,
		Params: append(authParams(),
			extend.ParamField{Name: "playlistUrl", Kind: "url", Label: extend.T("M3U playlist"), Placeholder: "https://…/playlist.m3u"},
			extend.ParamField{Name: "guideUrl", Kind: "url", Label: extend.T("XMLTV guide (optional)")},
			extend.ParamField{Name: "refreshHours", Kind: "number", Label: extend.T("Keep it fresh")},
		),
	}
}
func (m3uKind) Matches(src Source) bool {
	// A legacy Stremio source's link is an add-on, never a playlist, and a
	// plugin source whose plugin is off is not one either.
	return src.Kind != legacyStremioKind && !strings.HasPrefix(src.Kind, "plugin:") &&
		(strings.TrimSpace(src.Playlist) != "" || src.PlaylistURL != "")
}
func (m3uKind) Fetch(ctx context.Context, s *Service, src *Source) (Fetched, error) {
	return fetchPlaylistGuide(ctx, s, src, true)
}

type guideOnlyKind struct{}

func (guideOnlyKind) ID() string { return "guideOnly" }
func (guideOnlyKind) Describe() SourceDescribe {
	return SourceDescribe{
		Label: extend.T("Guide only (XMLTV)"), Hint: extend.T("An XMLTV guide on its own. Map any channel to it for listings."),
		Icon: "calendar-range", Order: 30, Editor: "guide", YieldsChannels: false, YieldsGuide: true,
		Params: append(authParams(),
			extend.ParamField{Name: "guideUrl", Kind: "url", Label: extend.T("XMLTV guide")},
			extend.ParamField{Name: "refreshHours", Kind: "number", Label: extend.T("Keep it fresh")},
		),
	}
}
func (guideOnlyKind) Matches(src Source) bool {
	return strings.TrimSpace(src.Playlist) == "" && src.PlaylistURL == "" &&
		(strings.TrimSpace(src.Guide) != "" || src.GuideURL != "")
}
func (guideOnlyKind) Fetch(ctx context.Context, s *Service, src *Source) (Fetched, error) {
	return fetchPlaylistGuide(ctx, s, src, false)
}

type xtreamKind struct{}

func (xtreamKind) ID() string { return "xtream" }
func (xtreamKind) Describe() SourceDescribe {
	return SourceDescribe{
		Label: extend.T("Xtream Codes login"), Hint: extend.T("Sign in to an IPTV provider's Xtream Codes panel. Ether imports the live channels and the panel's guide."),
		Icon: "key-round", Order: 10, Editor: "xtream", YieldsChannels: true, YieldsGuide: true,
		Params: append(authParams(),
			extend.ParamField{Name: "server", Kind: "url", Label: extend.T("Server"), Required: true},
			extend.ParamField{Name: "username", Kind: "text", Label: extend.T("Username"), Required: true},
			extend.ParamField{Name: "password", Kind: "secret", Label: extend.T("Password"), Required: true},
			extend.ParamField{Name: "output", Kind: "select", Label: extend.T("Stream format"), Options: []extend.ParamOption{
				{Value: "ts", Label: extend.Text{Source: "MPEG-TS"}}, {Value: "m3u8", Label: extend.Text{Source: "HLS"}},
			}},
			extend.ParamField{Name: "guideUrl", Kind: "url", Label: extend.T("XMLTV guide override (optional)")},
		),
	}
}
func (xtreamKind) Matches(src Source) bool { return src.Auth != nil && src.Auth.Type == "xtream" }
func (xtreamKind) Fetch(ctx context.Context, s *Service, src *Source) (Fetched, error) {
	// prepareSourceAuth has already derived the lineup and guide URLs.
	return fetchXtream(ctx, s, src)
}

// virtualKind describes virtual channels programmed from the catalogue.
// They are channels, not imported sources: ChannelStudio creates them and
// playout generates their guide, so Fetch never runs for them.
type virtualKind struct{}

func (virtualKind) ID() string { return "virtual" }
func (virtualKind) Describe() SourceDescribe {
	return SourceDescribe{
		Label: extend.T("Virtual channel"), Hint: extend.T("A channel programmed from your catalogue."),
		Icon: "tv", Order: 40, Editor: "virtual", YieldsChannels: true, YieldsGuide: true,
		Params: []extend.ParamField{{Name: "collection", Kind: "collection", Label: extend.T("Collection")}},
	}
}
func (virtualKind) Matches(Source) bool { return false }
func (virtualKind) Fetch(context.Context, *Service, *Source) (Fetched, error) {
	return Fetched{}, fmt.Errorf("virtual channels are programmed in Channel Studio, not imported")
}

// fetchPlaylistGuide is the shared M3U/XMLTV fetch: download the playlist,
// parse channels, then fetch and filter the guide. Behaviour is unchanged
// from the pre-registry ImportSource.
func fetchPlaylistGuide(ctx context.Context, s *Service, src *Source, withChannels bool) (Fetched, error) {
	var err error
	progress := importProgress(ctx)
	if progress != nil {
		progress.setPhase("playlist")
	}
	if src.PlaylistURL != "" {
		s.o.Logger.Info("Live TV source fetching", "sourceId", src.ID, "source", src.Name, "url", sourceOrigin(*src), "step", "playlist")
		src.Playlist, err = s.readURLWith(ctx, src.PlaylistURL, maxPlaylistBytes, src.Auth.header(), "playlist")
		if err != nil {
			if len(src.Logins) > 0 && withChannels {
				src.Account = &ProviderAccount{Status: "Disabled", CheckedAt: s.o.Now()}
				return s.cachedPrimaryLineup(ctx, src, fmt.Errorf("playlist: %w", err))
			}
			return Fetched{}, fmt.Errorf("playlist: %w", err)
		}
	}
	if strings.TrimSpace(src.Playlist) == "" && strings.TrimSpace(src.Guide) == "" && src.GuideURL == "" {
		return Fetched{}, fmt.Errorf("source needs a playlist or an XMLTV guide")
	}
	var cs []Channel
	if withChannels && strings.TrimSpace(src.Playlist) != "" {
		var stats m3uStats
		cs, stats, err = parseM3U(strings.NewReader(src.Playlist), src.ID)
		if err != nil {
			return Fetched{}, fmt.Errorf("playlist: %w", err)
		}
		src.SkippedVOD = stats.SkippedVOD
		cs = s.applyGroupRules(ctx, src, cs)
		if progress != nil {
			progress.setChannels(len(cs))
		}
		s.o.Logger.Info("Live TV source read", "sourceId", src.ID, "source", src.Name, "step", "playlist", "bytes", len(src.Playlist), "channels", len(cs), "skippedVod", stats.SkippedVOD)
	}
	g, err := fetchSourceGuide(ctx, s, src, cs, withChannels && strings.TrimSpace(src.Playlist) != "")
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Channels: cs, Guide: g}, nil
}

// fetchSourceGuide reads a source's guide, keeping only the channels it
// lists (all of them for a guide-only source).
func fetchSourceGuide(ctx context.Context, s *Service, src *Source, cs []Channel, hasChannels bool) (XMLGuide, error) {
	allow, err := s.guideAllow(ctx, *src, cs, hasChannels)
	if err != nil {
		return XMLGuide{}, err
	}
	filter := guideFilter{From: s.o.Now().Add(-guideLookback), Until: s.o.Now().Add(guideLookahead), IDs: allow}
	nameCandidateCount := 0
	if len(cs) > 0 {
		filter.Resolve = func(channels map[string]Channel, names map[string][]string) map[string]struct{} {
			candidates := nameCandidates(cs, channels, names)
			nameCandidateCount = len(candidates)
			return candidates
		}
	}
	progress := importProgress(ctx)
	if progress != nil {
		progress.setChannels(len(cs))
		progress.setPhase("guide")
	}
	g := XMLGuide{}
	if src.GuideURL != "" {
		s.o.Logger.Info("Live TV source fetching", "sourceId", src.ID, "source", src.Name, "url", sourceOrigin(*src), "step", "guide")
		g, err = s.readGuide(ctx, src.GuideURL, src.Auth.header(), filter)
		if err != nil {
			return XMLGuide{}, fmt.Errorf("guide: %w", err)
		}
	} else if src.Guide != "" {
		var dropped bool
		g, dropped, err = parseXMLTV(strings.NewReader(src.Guide), filter)
		if err != nil {
			return XMLGuide{}, err
		}
		if dropped {
			trimmed, err := formatXMLTV(g)
			if err != nil {
				return XMLGuide{}, err
			}
			src.Guide = trimmed
		}
	}
	g.Programmes = dedupeProgrammes(g.Programmes)
	if src.Guide != "" || src.GuideURL != "" {
		if progress != nil {
			progress.setProgrammes(len(g.Programmes))
		}
		s.o.Logger.Info("Live TV source read", "sourceId", src.ID, "source", src.Name, "step", "guide", "guideChannels", len(g.Channels), "nameCandidates", nameCandidateCount, "lateChannels", g.LateChannels, "programmes", len(g.Programmes))
	}
	return g, nil
}

// GuideFeatures describes upstream behavior without identifying a frontend.
type GuideFeatures struct{ Remote, ReviewedAliases bool }
type guideFeatureSource interface{ GuideFeatures() GuideFeatures }

func (providerGuideKind) GuideFeatures() GuideFeatures {
	return GuideFeatures{Remote: true, ReviewedAliases: true}
}
func (s *Service) sourceGuideFeatures(src Source) GuideFeatures {
	k, ok := s.kindRegistry().Classify(src)
	if ok {
		if p, ok := k.(guideFeatureSource); ok {
			return p.GuideFeatures()
		}
	}
	return GuideFeatures{}
}

func defaultGuideFeatures(src Source) GuideFeatures {
	s := &Service{}
	return s.sourceGuideFeatures(src)
}
