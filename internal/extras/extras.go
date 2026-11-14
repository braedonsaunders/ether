// Package extras lists and resolves trailers and other clips attached to
// catalogue titles (Plex "Extras" and Cinema Trailers).
//
// TMDB only gives YouTube/Vimeo keys, which official Plex apps cannot
// play, so an extra is only advertised when a playable source exists: an
// owner-configured yt-dlp-compatible resolver command that turns a video
// page into a direct media URL. Ether ships no resolver binary and no content.
package extras

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// SourceCommand marks an Extra whose TMDB video is resolved by the owner's command.
const SourceCommand = "command"

// MaxExtras caps how many extras one title advertises.
const MaxExtras = 10

// Extra is one playable clip attached to a title.
type Extra struct {
	Key         core.RatingKey // KindExtra
	ParentKey   core.RatingKey
	ParentTitle string
	Title       string
	Subtype     string // Plex subtype: trailer, teaser, clip, featurette, behindTheScenes
	ExtraType   int    // Plex numeric extra type (1 = trailer)
	Site        string // YouTube, Vimeo (command source)
	SiteKey     string // site-specific video id
	Language    string // ISO 639-1
	Official    bool
	PublishedAt time.Time
	Size        int // vertical resolution when known
	Duration    time.Duration
	Source      string // SourceCommand

	identity string // registry identity (collision detection)
}

// IsTrailer reports whether the extra can serve as a Cinema Trailer.
func (e Extra) IsTrailer() bool { return e.Subtype == "trailer" || e.Subtype == "teaser" }

// Resolved is a playable location for an extra.
type Resolved struct {
	URL       string // direct media URL
	Headers   map[string]string
	Expires   time.Time
	Container string
}

// Input returns the ffmpeg input.
func (r Resolved) Input() string { return r.URL }

// VideoProvider is the TMDB videos surface the service needs
// (*tmdb.Client satisfies it).
type VideoProvider interface {
	MovieVideos(ctx context.Context, id int64, lang string) ([]tmdb.Video, error)
	TVVideos(ctx context.Context, id int64, lang string) ([]tmdb.Video, error)
	SeasonVideos(ctx context.Context, tvID int64, season int, lang string) ([]tmdb.Video, error)
	EpisodeVideos(ctx context.Context, tvID int64, season, episode int, lang string) ([]tmdb.Video, error)
}

// CommandRunner runs the resolver command and returns its stdout. Tests
// substitute a fake; the default is exec without a shell.
type CommandRunner func(ctx context.Context, name string, args []string) ([]byte, error)

// Options are the extras service dependencies.
type Options struct {
	CacheDir   string
	CacheHTTP  *http.Client // optional injected transport for trailer downloads
	Videos     VideoProvider
	Config     func() config.Extras
	Logger     *slog.Logger
	Now        func() time.Time
	CacheTTL   time.Duration // list cache; default 1h
	Runner     CommandRunner
	LookPath   func(string) (string, error)
	ToolsDir   string
	Downloader *Downloader
}

type listEntry struct {
	extras  []Extra
	expires time.Time
}

// Service lists, registers and resolves extras. Safe for concurrent use.
type Service struct {
	cacheDir        string
	cacheHTTP       *http.Client
	cacheMu         sync.Mutex
	cacheDownloadMu sync.Mutex
	videos          VideoProvider
	cfgFn           func() config.Extras
	log             *slog.Logger
	now             func() time.Time
	ttl             time.Duration
	runner          CommandRunner
	look            func(string) (string, error)
	downloader      *Downloader

	mu       sync.Mutex
	registry map[core.RatingKey]Extra
	lists    map[string]listEntry
	resolved map[string]Resolved
}

const (
	maxListEntries     = 2048
	maxRegistryEntries = 8192
	maxResolvedEntries = 2048
)

// New builds the extras service.
func New(o Options) *Service {
	if o.Config == nil {
		o.Config = func() config.Extras { return config.Extras{} }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = time.Hour
	}
	if o.Runner == nil {
		o.Runner = execRunner
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	dl := o.Downloader
	if dl == nil && o.ToolsDir != "" {
		dl = NewDownloader(DownloaderOptions{
			ToolsDir: o.ToolsDir,
			Config:   o.Config,
			LookPath: o.LookPath,
			Now:      o.Now,
		})
	}
	return &Service{
		cacheDir: o.CacheDir, cacheHTTP: o.CacheHTTP,
		videos: o.Videos, cfgFn: o.Config,
		log: o.Logger.With("component", "extras"), now: o.Now,
		ttl: o.CacheTTL, runner: o.Runner, look: o.LookPath,
		downloader: dl,
		registry:   map[core.RatingKey]Extra{}, lists: map[string]listEntry{},
		resolved: map[string]Resolved{},
	}
}

// Downloader returns the managed yt-dlp downloader shared with this service.
func (s *Service) Downloader() *Downloader { return s.downloader }

func (s *Service) cfg() config.Extras { return s.cfgFn() }

// DefaultResolverCommand is used when the owner enabled trailers but set
// no resolver command. It is resolved on PATH at use time and never
// written into the saved config.
const DefaultResolverCommand = "yt-dlp"

// effectiveCommand returns the command that would run for this config:
// the configured one, the verified managed yt-dlp binary in Ether's data dir,
// or the yt-dlp default when none is set. The second result reports whether
// it was found and verified.
func (s *Service) effectiveCommand(c config.Extras) (string, bool) {
	cmd := strings.TrimSpace(c.ResolverCommand)
	if cmd == "" {
		if s.downloader != nil {
			if path, _, ok := s.downloader.VerifiedManagedBinary(); ok {
				return path, true
			}
		}
		cmd = DefaultResolverCommand
	}
	_, err := s.look(cmd)
	return cmd, err == nil
}

// CommandAvailable reports whether the resolver command source can run.
func (s *Service) CommandAvailable() bool {
	_, ok := s.effectiveCommand(s.cfg())
	return ok
}

// Enabled reports whether any playable extras can be advertised.
func (s *Service) Enabled() bool {
	return s.cfg().Enabled && s.CommandAvailable()
}

// DisabledReason explains why extras are off, for a startup log line.
// Empty when enabled.
func (s *Service) DisabledReason() string {
	c := s.cfg()
	switch {
	case !c.Enabled:
		return "extras disabled in config (extras.enabled is false)"
	case s.Enabled():
		return ""
	case strings.TrimSpace(c.ResolverCommand) == "":
		return "extras enabled but trailers need yt-dlp installed (or set extras.resolverCommand to a yt-dlp-compatible tool)"
	default:
		return fmt.Sprintf("extras resolver command %q not found or not executable", c.ResolverCommand)
	}
}

// PrewarmBytes is the configured trailer pre-buffer size (32–64 MiB).
func (s *Service) PrewarmBytes() int64 { return s.cfg().PrewarmBytesOr() }

// ISO639 reduces a BCP-47 tag to its ISO 639-1 language.
func ISO639(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	return lang
}

// List returns the playable extras of a title, best first: official
// trailers in the viewer's language ahead of everything else. It returns
// nothing when extras are disabled or no source can play them.
func (s *Service) List(ctx context.Context, parent *core.Title, lang string) ([]Extra, error) {
	if parent == nil || !s.Enabled() {
		return nil, nil
	}
	iso := ISO639(lang)
	cacheKey := parent.Key.String() + "|" + iso
	s.mu.Lock()
	if e, ok := s.lists[cacheKey]; ok && s.now().Before(e.expires) {
		out := append([]Extra(nil), e.extras...)
		for i := range out {
			out[i] = s.registerLocked(out[i])
		}
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	var out []Extra
	fetchFailed := false
	if s.CommandAvailable() && s.videos != nil {
		vs, err := s.fetchVideos(ctx, parent, lang)
		if err != nil {
			fetchFailed = true
			s.log.Debug("tmdb videos", "key", parent.Key, "err", err)
		}
		out = append(out, videosToExtras(parent, vs)...)
	}
	out = dedupeExtras(out)
	sortExtras(out, iso)
	if len(out) > MaxExtras {
		out = out[:MaxExtras]
	}
	s.mu.Lock()
	s.pruneLocked()
	for i := range out {
		out[i] = s.registerLocked(out[i])
	}
	// A failed TMDB lookup is not "this title has no extras": caching it
	// would hide every trailer for the whole TTL.
	if !fetchFailed {
		if len(s.lists) >= maxListEntries {
			for k := range s.lists {
				delete(s.lists, k)
				break
			}
		}
		s.lists[cacheKey] = listEntry{extras: append([]Extra(nil), out...), expires: s.now().Add(s.ttl)}
	}
	s.mu.Unlock()
	return out, nil
}

func (s *Service) fetchVideos(ctx context.Context, parent *core.Title, lang string) ([]tmdb.Video, error) {
	tmdbID := parent.IDs.TMDB
	if tmdbID == 0 {
		tmdbID = parent.Key.TMDB()
	}
	if parent.Type == core.TypeEpisode || parent.Type == core.TypeSeason {
		if parent.ShowIDs.TMDB > 0 {
			tmdbID = parent.ShowIDs.TMDB
		} else {
			tmdbID = parent.Key.TMDB()
		}
	}
	if tmdbID <= 0 {
		return nil, nil
	}
	switch parent.Type {
	case core.TypeMovie:
		return s.videos.MovieVideos(ctx, tmdbID, lang)
	case core.TypeShow:
		return s.videos.TVVideos(ctx, tmdbID, lang)
	case core.TypeSeason:
		return s.videos.SeasonVideos(ctx, tmdbID, parent.Season, lang)
	case core.TypeEpisode:
		return s.videos.EpisodeVideos(ctx, tmdbID, parent.Season, parent.Episode, lang)
	}
	return nil, nil
}

// Lookup returns a registered extra by rating key.
func (s *Service) Lookup(key core.RatingKey) (Extra, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.registry[key]
	return e, ok
}

// registerLocked assigns the deterministic KindExtra key for an extra.
// The payload is a hash of the extra's identity, so the same extra keeps
// the same key across listings and restarts; hash collisions salt.
func (s *Service) registerLocked(e Extra) Extra {
	for salt := 0; ; salt++ {
		id := e.identity
		if salt > 0 {
			id = fmt.Sprintf("%s#%d", id, salt)
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(id))
		key := core.ExtraKey(int64(h.Sum64()%999_999_999_999_999) + 1)
		if key.IsPreroll() {
			continue // reserved for owner pre-rolls
		}
		if prev, ok := s.registry[key]; ok && prev.identity != e.identity {
			continue
		}
		e.Key = key
		if len(s.registry) >= maxRegistryEntries {
			for k := range s.registry {
				delete(s.registry, k)
				break
			}
		}
		s.registry[key] = e
		return e
	}
}

// pruneLocked removes expired cache records. The hard caps below also bound
// entries whose keys are never revisited and therefore never expire lazily.
func (s *Service) pruneLocked() {
	now := s.now()
	for k, v := range s.lists {
		if !now.Before(v.expires) {
			delete(s.lists, k)
		}
	}
	for k, v := range s.resolved {
		if !v.Expires.IsZero() && !now.Before(v.Expires) {
			delete(s.resolved, k)
		}
	}
}

// Resolve turns an extra into a playable input, caching command results
// until the returned URL expires.
func (s *Service) Resolve(ctx context.Context, e Extra) (Resolved, error) {
	if e.Source == SourceCommand {
		return s.resolveCommand(ctx, e)
	}
	return Resolved{}, fmt.Errorf("extras: unknown source %q", e.Source)
}

func (s *Service) resolveCommand(ctx context.Context, e Extra) (Resolved, error) {
	cfg := s.cfg()
	if !cfg.Enabled {
		return Resolved{}, fmt.Errorf("extras disabled")
	}
	cmd, ok := s.effectiveCommand(cfg)
	if !ok {
		return Resolved{}, fmt.Errorf("extras: resolver command unavailable")
	}
	cfg.ResolverCommand = cmd
	// A new command or argument template may choose a different format or
	// credentials. Do not reuse a URL produced by the previous settings.
	identity, _ := json.Marshal(struct {
		Command string
		Args    []string
	}{cfg.ResolverCommand, cfg.ResolverArgs})
	cacheKey := string(identity) + ":" + strings.ToLower(e.Site) + ":" + e.SiteKey
	s.mu.Lock()
	if r, ok := s.resolved[cacheKey]; ok && (r.Expires.IsZero() || s.now().Before(r.Expires)) {
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	r, err := runResolver(ctx, cfg, e, s.runner, s.now)
	if err != nil {
		return Resolved{}, err
	}
	s.mu.Lock()
	s.pruneLocked()
	s.resolved[cacheKey] = r
	if len(s.resolved) > maxResolvedEntries {
		for k := range s.resolved {
			if k != cacheKey {
				delete(s.resolved, k)
				break
			}
		}
	}
	s.mu.Unlock()
	return r, nil
}

// subtypeFor maps a TMDB video type to the Plex subtype and numeric
// extra type. Unknown types are treated as clips.
func subtypeFor(tmdbType string) (string, int) {
	switch tmdbType {
	case tmdb.VideoTrailer:
		return "trailer", 1
	case tmdb.VideoTeaser:
		return "teaser", 1
	case tmdb.VideoBehindTheScenes, tmdb.VideoBloopers:
		return "behindTheScenes", 3
	case tmdb.VideoFeaturette:
		return "featurette", 6
	default:
		return "clip", 4
	}
}

func videosToExtras(parent *core.Title, vs []tmdb.Video) []Extra {
	var out []Extra
	for _, v := range vs {
		if !ValidateSiteKey(v.Site, v.Key) {
			continue
		}
		sub, num := subtypeFor(v.Type)
		title := strings.TrimSpace(v.Name)
		if title == "" {
			title = parent.Title + " " + v.Type
		}
		out = append(out, Extra{
			ParentKey: parent.Key, ParentTitle: parent.Title, Title: title,
			Subtype: sub, ExtraType: num, Site: v.Site, SiteKey: v.Key,
			Language: ISO639(v.ISO639), Official: v.Official,
			PublishedAt: v.Published(), Size: v.Size, Source: SourceCommand,
			identity: fmt.Sprintf("%d|c|%s|%s", int64(parent.Key), strings.ToLower(v.Site), v.Key),
		})
	}
	return out
}

func dedupeExtras(in []Extra) []Extra {
	seen := map[string]int{}
	out := in[:0:0]
	for _, e := range in {
		k := e.identity
		if i, ok := seen[k]; ok {
			// Keep the better copy (official, then larger).
			if (e.Official && !out[i].Official) || e.Size > out[i].Size {
				out[i] = e
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, e)
	}
	return out
}

func typeRank(sub string) int {
	switch sub {
	case "trailer":
		return 0
	case "teaser":
		return 1
	case "clip":
		return 2
	case "featurette":
		return 3
	case "behindTheScenes":
		return 4
	}
	return 5
}

func langRank(e Extra, iso string) int {
	switch {
	case iso != "" && e.Language == iso:
		return 0
	case e.Language == "en":
		return 1
	case e.Language == "":
		return 2
	}
	return 3
}

// sortExtras orders by type, official first, viewer language,
// resolution, then newest.
func sortExtras(es []Extra, iso string) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if ra, rb := typeRank(a.Subtype), typeRank(b.Subtype); ra != rb {
			return ra < rb
		}
		oa, ob := a.Official, b.Official
		if oa != ob {
			return oa
		}
		if la, lb := langRank(a, iso), langRank(b, iso); la != lb {
			return la < lb
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		if !a.PublishedAt.Equal(b.PublishedAt) {
			return a.PublishedAt.After(b.PublishedAt)
		}
		return a.Title < b.Title
	})
}
