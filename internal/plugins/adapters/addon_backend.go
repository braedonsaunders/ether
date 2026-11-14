package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/stremio"
)

// An installed add-on's streams are searched like every other indexer: an
// IndexerKindAddon row in the indexer list names the add-on, and
// addonBackend is its newznab.Backend, so it gets the search cache, daily
// budgets, rate-limit pauses, rescue, de-duplication and events every
// indexer gets. Plain Stremio add-ons and Ether source plugins share this
// one path; the protocol work (ids, stream decoding) is internal/stremio's.

const (
	// skipWarnInterval throttles the skipped-streams warning per add-on.
	skipWarnInterval = time.Hour
)

// Backend returns the search backend of an add-on indexer row
// (newznab.Options.Backend). Backends are kept per row so throttling state
// survives between searches.
func (s *StreamSources) Backend(ix config.Indexer) (newznab.Backend, error) {
	if !ix.IsAddon() || ix.Plugin == "" {
		return nil, fmt.Errorf("indexer %q: not an installed add-on", ix.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backends == nil {
		s.backends = map[string]*addonBackend{}
	}
	key := ix.ID + "\x00" + ix.Plugin
	b := s.backends[key]
	if b == nil {
		b = &addonBackend{s: s, plugin: ix.Plugin, instance: ix.ID}
		s.backends[key] = b
	}
	return b, nil
}

// SetAnimeIDs wires the anime id mapping used for add-ons that look anime
// up by Kitsu, MAL, AniList or AniDB ids.
func (s *StreamSources) SetAnimeIDs(a stremio.AnimeIDs) {
	s.mu.Lock()
	s.anime = a
	s.mu.Unlock()
}

// SetNZBClient overrides the client that downloads add-on NZBs (tests).
func (s *StreamSources) SetNZBClient(c *http.Client) {
	s.mu.Lock()
	s.nzb = c
	s.mu.Unlock()
}

type addonBackend struct {
	s        *StreamSources
	plugin   string
	instance string

	mu           sync.Mutex
	lastSkipWarn time.Time
	skipWarned   bool
}

var _ newznab.Backend = (*addonBackend)(nil)

func (b *addonBackend) record(ctx context.Context) (*plugins.Manager, plugins.Record, error) {
	b.s.mu.RLock()
	mgr := b.s.mgr
	b.s.mu.RUnlock()
	if mgr == nil {
		return nil, plugins.Record{}, fmt.Errorf("add-on %s: %w", b.plugin, core.ErrSourceUnavailable)
	}
	rec, err := mgr.Get(ctx, b.plugin)
	if err != nil {
		return nil, plugins.Record{}, fmt.Errorf("add-on %s: %w", b.plugin, core.ErrSourceUnavailable)
	}
	return mgr, rec, nil
}

// streamManifest is the Stremio manifest a stream lookup reads. An Ether
// source plugin that declares no stream resource is asked like an add-on
// whose stream resource inherits the manifest's types and prefixes.
func streamManifest(man plugins.Manifest) stremio.Manifest {
	m := man.Stremio()
	if man.Ether != nil && !m.HasResource(stremio.ResourceStream) {
		m.Resources = append(append(stremio.Resources(nil), m.Resources...), stremio.Resource{Name: stremio.ResourceStream, Short: true})
		if len(m.Types) == 0 {
			m.Types = []string{stremio.TypeMovie, stremio.TypeSeries}
		}
	}
	return m
}

// Search implements newznab.Backend.
func (b *addonBackend) Search(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error) {
	mgr, rec, err := b.record(ctx)
	if err != nil {
		return nil, err
	}
	if !rec.Enabled || !rec.Manifest.HasCapability(plugins.CapStreamSource) {
		return nil, nil
	}
	b.s.mu.RLock()
	anime := b.s.anime
	specials := b.s.specials
	b.s.mu.RUnlock()
	look, ok := stremio.LookupStreams(ctx, streamManifest(rec.Manifest), req, anime, specials)
	if !ok {
		return nil, nil
	}
	if err := reserve(ctx, "hit"); err != nil {
		return nil, err
	}
	if err := b.s.spendSearch(ctx, rec); err != nil {
		return nil, err
	}
	call := plugins.Request{Method: http.MethodGet, Path: look.Route()}
	if rec.Manifest.Ether != nil {
		// Ether source plugins may read search hints; third-party add-ons
		// get exactly the Stremio request.
		call.Query = searchHints(req)
	}
	resp, err := mgr.Call(ctx, rec.ID, call)
	if err != nil {
		b.s.noteFailure(rec.ID)
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		b.s.noteFailure(rec.ID)
		he := &stremio.HTTPError{Status: resp.Status, RetryAfter: header(resp.Headers, "Retry-After")}
		if he.RateLimited() {
			return nil, &newznab.RateLimitError{RetryAfter: he.RetryAfter, Err: he}
		}
		return nil, he
	}
	var answer struct {
		Streams []stremio.Stream `json:"streams"`
	}
	if err := json.Unmarshal(resp.Body, &answer); err != nil {
		b.s.noteFailure(rec.ID)
		return nil, errors.New("the add-on answered with invalid stream JSON")
	}
	return b.convert(ctx, rec, req, look, answer.Streams), nil
}

// etherStream is the optional `ether` block of a stream an Ether source
// plugin answers (SPEC §5.1.4).
type etherStream struct {
	ID          string   `json:"id,omitempty"`
	ReleaseName string   `json:"releaseName,omitempty"`
	Size        int64    `json:"size,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	Lazy        bool     `json:"lazy,omitempty"`
	File        string   `json:"file,omitempty"`
}

// convert turns an answer into releases in the add-on's order. Ordinary
// streams go through stremio.Convert; an Ether plugin's lazy and local-file
// streams become direct releases opened at play time.
func (b *addonBackend) convert(ctx context.Context, rec plugins.Record, req core.SearchRequest, look stremio.Lookup, streams []stremio.Stream) []core.Release {
	origin := ""
	if u, err := stremio.NormalizeURL(rec.SourceURL); err == nil {
		origin = stremio.Origin(u)
	}
	type extra struct {
		langs []string
	}
	ordinary := make([]stremio.Stream, 0, len(streams))
	ordinaryAt := make([]int, 0, len(streams))
	extras := map[int]extra{}
	direct := map[int]core.Release{}
	for i := range streams {
		st := streams[i]
		var eb etherStream
		if rec.Manifest.Ether != nil && len(st.Ether) > 0 {
			_ = json.Unmarshal(st.Ether, &eb)
		}
		if eb.Lazy || strings.TrimSpace(eb.File) != "" {
			if rel, ok := b.s.directRelease(ctx, rec, req, eb); ok {
				direct[i] = rel
			}
			continue
		}
		if eb.ReleaseName != "" {
			st.BehaviorHints.Filename = eb.ReleaseName
			if st.Description == "" && st.Title == "" {
				st.Title = eb.ReleaseName
			}
		}
		if eb.Size > 0 {
			st.BehaviorHints.VideoSize = eb.Size
		}
		if len(eb.Languages) > 0 {
			extras[len(ordinary)] = extra{langs: eb.Languages}
		}
		ordinary = append(ordinary, st)
		ordinaryAt = append(ordinaryAt, i)
	}
	conv := stremio.Convert(ordinary, req, look, stremio.ConvertOptions{Origin: origin, Instance: b.instance})
	b.logSkipped(conv.SkippedNoHash, conv.SkippedAmbiguous)
	byStream := map[int]core.Release{}
	for j, rel := range conv.Releases {
		src := conv.Source[j]
		if ex, ok := extras[src]; ok {
			rel.Info.Languages = mergeLanguages(rel.Info.Languages, ex.langs)
		}
		rel.TitleIDs = req.IDs
		rel.TitleIDsEchoed = true
		byStream[ordinaryAt[src]] = rel
	}
	for i, rel := range direct {
		byStream[i] = rel
	}
	out := make([]core.Release, 0, len(byStream))
	for i := range streams {
		if rel, ok := byStream[i]; ok {
			out = append(out, rel)
		}
	}
	return out
}

func (b *addonBackend) logSkipped(noHash, ambiguous int) {
	if noHash == 0 && ambiguous == 0 {
		return
	}
	now := time.Now()
	b.mu.Lock()
	throttled := b.skipWarned && now.Sub(b.lastSkipWarn) < skipWarnInterval
	if !throttled {
		b.lastSkipWarn, b.skipWarned = now, true
	}
	b.mu.Unlock()
	level := slog.LevelWarn
	if throttled {
		level = slog.LevelDebug
	}
	slog.Default().Log(context.Background(), level, "add-on streams skipped without a usable torrent, NZB or HTTP link",
		"addon", b.plugin, "skippedNoHash", noHash, "skippedAmbiguous", ambiguous)
}

// ResolveTorrent implements newznab.Backend: add-on results already carry
// their info hash and magnet.
func (b *addonBackend) ResolveTorrent(_ context.Context, r core.Release, _ func(context.Context, string) error) (core.Release, error) {
	if r.InfoHash == "" {
		return r, fmt.Errorf("add-on release %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
	}
	return r, nil
}

// FetchNZB implements newznab.Backend for an add-on's Usenet streams,
// exactly as for an add-on entered on the Indexers page.
func (b *addonBackend) FetchNZB(ctx context.Context, r core.Release, reserve func(context.Context, string) error) ([]byte, error) {
	_, rec, err := b.record(ctx)
	if err != nil {
		return nil, err
	}
	origin := ""
	if u, err := stremio.NormalizeURL(rec.SourceURL); err == nil {
		origin = stremio.Origin(u)
	}
	return stremio.FetchNZB(ctx, b.s.nzbClient(), origin, r, func(ctx context.Context, kind string) error {
		if err := reserve(ctx, kind); err != nil {
			return err
		}
		return b.s.spendGrab(ctx, rec.ID)
	})
}

func (s *StreamSources) nzbClient() *http.Client {
	s.mu.RLock()
	c := s.nzb
	s.mu.RUnlock()
	if c != nil {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nzb == nil {
		s.nzb = stremio.DefaultNZBClient()
	}
	return s.nzb
}

func searchHints(req core.SearchRequest) map[string]string {
	q := map[string]string{}
	if req.Title != "" {
		q["title"] = req.Title
	}
	if req.Year > 0 {
		q["year"] = strconv.Itoa(req.Year)
	}
	if req.Season > 0 || (req.Type == core.TypeEpisode && req.Episode > 0) {
		q["season"] = strconv.Itoa(req.Season)
	}
	if req.Episode > 0 {
		q["episode"] = strconv.Itoa(req.Episode)
	}
	if req.IDs.IMDB != "" {
		q["imdbId"] = req.IDs.IMDB
	}
	if req.IDs.TMDB > 0 {
		q["tmdbId"] = strconv.FormatInt(req.IDs.TMDB, 10)
	}
	return q
}

func header(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// directRelease builds the release of an Ether plugin's lazy or local-file
// stream. Its GUID carries what FetchNZB needs to open it at play time.
func (s *StreamSources) directRelease(ctx context.Context, rec plugins.Record, req core.SearchRequest, eb etherStream) (core.Release, bool) {
	name := strings.TrimSpace(eb.ReleaseName)
	file := strings.TrimSpace(eb.File)
	if name == "" && file != "" {
		name = file[strings.LastIndexAny(file, `/\`)+1:]
	}
	if name == "" {
		return core.Release{}, false
	}
	cat := 2000
	if req.Type != core.TypeMovie {
		cat = 5000
	}
	rel := core.Release{Name: name, Size: eb.Size, Category: cat, Info: releaseparse.Parse(name), TitleIDs: req.IDs, TitleIDsEchoed: true, Protocol: "direct"}
	if len(eb.Languages) > 0 {
		rel.Info.Languages = mergeLanguages(rel.Info.Languages, eb.Languages)
	}
	switch {
	case file != "":
		var resolved string
		var err error
		if rec.Manifest.RuntimeKind() == "remote" {
			var f remoteFile
			f, err = s.validateRemoteFile(ctx, rec.ID, file)
			resolved = f.full
		} else {
			resolved, _, err = s.validateLocalFile(ctx, rec.ID, file)
		}
		if err != nil {
			return core.Release{}, false
		}
		// A file in an owner-granted folder is this title: name it so and
		// rank it first even when it is named plainly.
		if rel.Info.Title == "" || (req.Title != "" && !releaseparse.TitleMatches(rel.Info, req.Title)) {
			rel.Info.Title = req.Title
		}
		if rel.Info.Year == 0 && req.Year > 0 {
			rel.Info.Year = req.Year
		}
		if req.Type == core.TypeEpisode {
			if len(rel.Info.Seasons) == 0 && req.Season > 0 {
				rel.Info.Seasons = []int{req.Season}
			}
			if len(rel.Info.Episodes) == 0 && req.Episode > 0 {
				rel.Info.Episodes = []int{req.Episode}
			}
		}
		if rel.Info.Height == 0 {
			rel.Info.Height = 2160
		}
		if rel.Info.Source == core.SourceUnknown {
			rel.Info.Source = core.SourceRemux
		}
		rel.PubDate = s.now()
		rel.Grabs = 1_000_000
		rel.GUID = fmt.Sprintf("plugin:%s:file:%s", rec.ID, resolved)
	case eb.Lazy:
		id := strings.TrimSpace(eb.ID)
		if id == "" {
			return core.Release{}, false
		}
		rel.GUID = fmt.Sprintf("plugin:%s:lazy:%s", rec.ID, id)
	}
	return rel, true
}

// directStream reads a direct release's GUID back: the plugin, and the
// lazy stream id or the file path.
func directStream(guid string) (plugin, kind, ref string, ok bool) {
	rest, ok := strings.CutPrefix(guid, "plugin:")
	if !ok {
		return "", "", "", false
	}
	for _, k := range []string{":lazy:", ":file:"} {
		if i := strings.Index(rest, k); i > 0 {
			return rest[:i], strings.Trim(k, ":"), rest[i+len(k):], true
		}
	}
	return "", "", "", false
}

// ExpandAddonIndexers is the indexer list the running search manager
// searches: the owner's rows, then one row per installed plugin that serves
// streams (an Ether source plugin, or a Stremio add-on installed from
// Plugins with its streams on). Plugin rows are never saved: the plugin is
// their configuration, and the Indexers page stays the owner's own list.
func ExpandAddonIndexers(ctx context.Context, mgr *plugins.Manager, rows []config.Indexer) []config.Indexer {
	out := make([]config.Indexer, 0, len(rows))
	for _, ix := range rows {
		if !ix.IsAddon() {
			out = append(out, ix)
		}
	}
	if mgr == nil {
		return out
	}
	recs, err := mgr.List(ctx)
	if err != nil {
		return out
	}
	for _, r := range recs {
		if !r.Enabled || !r.Manifest.HasCapability(plugins.CapStreamSource) {
			continue
		}
		out = append(out, config.Indexer{
			ID: "plugin:" + r.ID, Name: r.Name, Kind: config.IndexerKindAddon, Plugin: r.ID,
			URL: r.SourceURL, Protocol: "torrent", Enabled: true, Priority: 1 << 20,
		})
	}
	return out
}
