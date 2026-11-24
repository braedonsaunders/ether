// Package plex is Ether's Plex Media Server front end: the HTTP API that
// official Plex apps (Web, iOS, Apple TV, Android, TVs) talk to. Everything
// behind it is reached through internal/frontend host interfaces.
package plex

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend/kit/plextimeline"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
)

// DefaultPMSVersion is reported when Identity.Version is empty. Clients gate
// features on the server version, so it must look like a current PMS build.
const DefaultPMSVersion = "1.43.4.10903-e5521bd8c"

// Identity is how the server presents itself. Empty fields fall back to the
// config snapshot (friendly name, machine identifier) or sensible defaults.
type Identity struct {
	MachineIdentifier  string
	FriendlyName       string
	Version            string
	Platform           string
	PlatformVersion    string
	MyPlexSubscription bool
	OwnerFeatures      []string
}

// Section is a library section exposed to clients.
type Section struct {
	ID       int
	Type     frontend.MediaType // native base: movie, show, artist, photo
	Title    string
	UUID     string // stable; derived from the machine identifier when empty
	Language string
	// LibraryID is the stable integer identity in the libraries store.
	LibraryID frontend.LibraryID
	// PluginID and KindKey are set for plugin-provided libraries (empty for
	// built-in and filter libraries).
	PluginID     string
	KindKey      string
	Subset       bool
	PluginSorts  []string
	PluginFacets []frontend.ViewFacet
}

// SessionToucher is optionally implemented by frontend.Playback to keep a play
// session alive on timeline updates.
type SessionToucher interface {
	Touch(sessionID string)
}

// Deps are the server's dependencies. Host.Experience, Host.Identity and Host.Sessions are
// required; the rest degrade gracefully when nil.
type Deps struct {
	Host       frontend.Host
	Playback   frontend.Playback
	Transcoder frontend.Transcoder

	// Sections overrides the default Movies (1) and TV Shows (2) sections.
	Sections []Section
	// DB persists artwork data and ordered shelf-generation keys. Clustered
	// nodes share this store so continuations survive failover and restarts.
	// Optional: without it, these caches remain process-local.
	DB *sql.DB
	// DisableEnrichment turns off the background title-detail and
	// UltraBlurColors work (the spike test library has nothing to fetch).
	DisableEnrichment bool
	// TracePlayback logs HLS/transcode requests when ETHER_TRACE_PLAYBACK=1.
	// It does not change responses.
	TracePlayback bool
}

type runtimeState struct {
	runtime        frontend.SessionRuntime
	playbackBroker frontend.Sessions
	streamRuntime  frontend.SessionRuntime
}

type expHolder struct {
	exp frontend.Experience
}

type hostServices struct {
	support          frontend.DiagnosticRecorder
	liveTV           frontend.LiveTV
	clientSeen       func(string)
	remoteClientSeen func(string)
	images           frontend.ArtworkImageCache
	ultra            frontend.ArtworkPaletteCache
	generatedPosters frontend.TitlePosterRenderer
	imageURL         frontend.ImageURLFunc
	trackPrefs       frontend.PlaybackTrackPreferences
	extras           frontend.ExtraStreams
	prerolls         frontend.Prerolls
	subtitles        frontend.SubtitleCatalog
	variants         frontend.Editions
}

// Server is the PMS API handler. It is an http.Handler; call Close on
// shutdown to stop background work and notification streams.
type Server struct {
	timelineStore timeline.Store
	bindMu        sync.Mutex
	rtState       atomic.Pointer[runtimeState]
	hostPtr       atomic.Pointer[frontend.Host]
	svcPtr        atomic.Pointer[hostServices]
	expState      atomic.Pointer[expHolder]
	sectionsPtr   atomic.Pointer[[]Section]
	titlesPtr     atomic.Pointer[titleCache]
	homePtr       atomic.Pointer[homeCache]

	activityMu       sync.Mutex
	activities       map[string]recordedActivity
	emittedHubMu     sync.Mutex
	emittedHubs      map[string]emittedHub
	emittedHubBytes  int
	shelfMu          sync.Mutex
	shelfCuts        map[string]shelfCut
	shelfGenerations map[string]shelfGeneration
	shelfBytes       int
	shelfSerial      uint64
	shelfWriter      *shelfPersistence
	plexEncoderMu    sync.RWMutex
	plexEncoder      SegmentedEncoder
	homeSharedMu     sync.Mutex
	homeShared       map[string]*homeSharedEntry
	shelfRefMu       sync.Mutex
	shelfRefs        map[string]shelfRef
	shelfRefRebuilt  time.Time
	shelfRebuild     chan struct{}
	guidResolveMu    sync.Mutex
	liveLogoKeyOnce  sync.Once
	liveLogoKey      []byte
	guidResolving    map[string]bool
	home             *homeCache

	d              Deps
	host           frontend.Host
	runtime        frontend.SessionRuntime
	streamRuntime  frontend.SessionRuntime
	initPlayback   frontend.Playback
	initTranscoder frontend.Transcoder
	experience     frontend.Experience
	log            *slog.Logger
	router         chi.Router

	sections []Section

	imageURL         frontend.ImageURLFunc
	generatedPosters frontend.TitlePosterRenderer
	liveTV           frontend.LiveTV
	support          frontend.DiagnosticRecorder
	clientSeen       func(string)
	remoteClientSeen func(string)

	images            frontend.ArtworkImageCache
	channelLogos      frontend.UntrustedImageFetcher
	channelRaster     frontend.SVGRasterizer
	trackPrefs        frontend.PlaybackTrackPreferences
	markers           frontend.MarkerRanges
	trickplay         frontend.TrickplayProvider
	extras            frontend.ExtraStreams
	prerolls          frontend.Prerolls
	subtitles         frontend.SubtitleCatalog
	variants          frontend.Editions
	plexGUID          frontend.LibraryGUIDs
	plexMeta          frontend.LibraryMetadata
	variantIDs        *variantIDRegistry
	playIDs           *playIdentities
	versionFlows      *versionFlowStore
	variantWatch      variantWatches
	plezyReasks       plezyReaskStore
	downloads         frontend.DownloadQueueStore
	queues            frontend.PlayQueues
	sessions          *sessionActivities
	playbackBroker    frontend.Sessions
	playbackOwners    frontend.PlaybackOwners
	playbackLifecycle frontend.Lifecycle
	transcodes        *transcodeTable
	progressiveStops  progressiveStopGuard
	readCaps          *readCapabilityStore
	deliveryGrants    *deliveryGrantStore
	recentClients     recentPartClients
	liveManifests     liveManifestRefetches
	extraPlay         *extraPlayStore
	subtitleOffers    subtitleOffers
	subtitlePicks     subtitlePicks
	selections        *selectionStore
	notifier          *notifier
	facets            *facetCache
	cl                *clusterState // nil on a standalone server

	// Background enrichment (nil when disabled).
	titles    *titleCache
	ultra     frontend.ArtworkPaletteCache
	titleJobs *bgQueue
	blurJobs  *bgQueue

	// Owner stream kills, delivered on the client's next timeline; the
	// janitor drops those whose client never reports again.
	termMu       sync.Mutex
	terms        map[string]termination
	decisionArms frontend.DecisionArms

	// Stalled-start watches and Dolby Vision base-layer fallbacks.
	starts frontend.StartGuard

	liveDiscovery liveDiscoveryCache
	liveGuideDay  liveGuideDayCache

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// NewServer builds the PMS API handler.
func NewServer(d Deps) *Server {
	host := d.Host
	if host.TracePlayback {
		d.TracePlayback = true
	}
	log := host.Log
	if log == nil {
		log = slog.Default()
	}
	httpClient := host.MetadataHTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt, ok := host.Sessions.(frontend.SessionRuntime)
	if !ok || rt == nil {
		panic("plex: Host.Sessions (implementing frontend.SessionRuntime) is required")
	}
	spikeEnabled := host.HostConfig != nil && host.HostConfig().Spike.Enabled
	db := d.DB
	if db == nil && !spikeEnabled {
		db = rt.DB()
	}
	s := &Server{
		activities:        map[string]recordedActivity{},
		d:                 d,
		host:              host,
		runtime:           rt,
		streamRuntime:     rt,
		initPlayback:      d.Playback,
		initTranscoder:    d.Transcoder,
		log:               log.With("component", "plex"),
		imageURL:          func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path },
		images:            frontend.NewArtworkRemoteCache(128<<20, httpClient),
		channelLogos:      frontend.NewUntrustedImageFetcher(nil),
		channelRaster:     frontend.NewSVGRasterizer(),
		variantIDs:        newVariantIDRegistry(rt, db),
		playIDs:           newPlayIdentities(),
		downloads:         rt.NewDownloadQueueStore(),
		queues:            rt.NewPlayQueues(),
		sessions:          newSessionActivities(),
		playbackOwners:    rt.PlaybackOwners(),
		playbackLifecycle: rt.LifecycleGates(),
		transcodes:        newTranscodeTable(),
		readCaps:          newReadCapabilityStore(),
		deliveryGrants:    newDeliveryGrantStore(),
		extraPlay:         newExtraPlayStore(newDirectHTTPClient(host.DirectProxy)),
		selections:        newSelectionStore(),
		notifier:          newNotifier(),
		facets:            newFacetCache(),
		ctx:               ctx,
		cancel:            cancel,
	}
	s.usePlaybackBroker(host.Sessions)
	initHost := host
	s.hostPtr.Store(&initHost)
	initSvc := s.svc()
	s.svcPtr.Store(&initSvc)
	s.shelfWriter = &shelfPersistence{pending: map[string]shelfWrite{}, active: map[string]bool{}, wake: make(chan struct{}, 1)}
	s.transcodes.load = s.loadDistributedBinding
	initSecs := s.buildSections()
	s.sectionsPtr.Store(&initSecs)
	s.queues.Configure(s.now, nil)
	cluster := s.clusterNode()
	if db != nil && cluster == nil {
		// Standalone queues share the durable store without starting cluster sessions.
		s.queues.Configure(s.now, newClusterState(s, db, frontend.StandaloneClusterNode("standalone-queues")).queuePersistence())
	}
	if cluster != nil && db != nil {
		s.cl = newClusterState(s, db, cluster)
		s.versionFlows = newVersionFlowStore(s, db)
		s.queues.Configure(s.now, s.cl.queuePersistence())
	}
	node := "standalone"
	if s.cl != nil {
		node = s.cl.self
	}
	s.starts = rt.NewStartGuard()
	s.router = s.routes()
	s.bindHost(host)
	s.wg.Add(1)
	go s.writeShelves()
	s.variantIDs.start(ctx, &s.wg)
	s.decisionArms = rt.NewDecisionArms(s.ctx, db, s.now, node, s.log, &s.wg)
	s.clearOwnDecisionArms()
	s.loadDownloads()
	s.wg.Add(1)
	go s.writeDownloads(s.downloadPersistence())
	s.scheduleDownloads()
	s.wg.Add(1)
	go s.janitor()
	if s.cl != nil {
		s.cl.start()
	}
	return s
}

func (s *Server) loadRuntimeState() runtimeState {
	if s == nil {
		return runtimeState{}
	}
	if p := s.rtState.Load(); p != nil {
		return *p
	}
	return runtimeState{
		runtime:        s.runtime,
		playbackBroker: s.playbackBroker,
		streamRuntime:  s.streamRuntime,
	}
}

func (s *Server) sessionRuntime() frontend.SessionRuntime { return s.loadRuntimeState().runtime }
func (s *Server) broker() frontend.Sessions               { return s.loadRuntimeState().playbackBroker }
func (s *Server) streams() frontend.SessionRuntime        { return s.loadRuntimeState().streamRuntime }

func (s *Server) getHost() frontend.Host {
	if s == nil {
		return frontend.Host{}
	}
	if p := s.hostPtr.Load(); p != nil {
		return *p
	}
	return s.host
}

func (s *Server) mutateHost(fn func(*frontend.Host)) {
	if s == nil {
		return
	}
	s.bindMu.Lock()
	cur := s.getHost()
	fn(&cur)
	s.hostPtr.Store(&cur)
	s.bindMu.Unlock()
}

func (s *Server) svc() hostServices {
	if s == nil {
		return hostServices{}
	}
	if p := s.svcPtr.Load(); p != nil {
		return *p
	}
	return hostServices{
		support:          s.support,
		liveTV:           s.liveTV,
		clientSeen:       s.clientSeen,
		remoteClientSeen: s.remoteClientSeen,
		images:           s.images,
		ultra:            s.ultra,
		generatedPosters: s.generatedPosters,
		imageURL:         s.imageURL,
		trackPrefs:       s.trackPrefs,
		extras:           s.extras,
		prerolls:         s.prerolls,
		subtitles:        s.subtitles,
		variants:         s.variants,
	}
}

func (s *Server) mutateServices(fn func(*hostServices)) {
	if s == nil {
		return
	}
	s.bindMu.Lock()
	cur := s.svc()
	fn(&cur)
	s.svcPtr.Store(&cur)
	s.bindMu.Unlock()
}

func (s *Server) tracePlaybackEnabled() bool {
	return s != nil && (s.getHost().TracePlayback || s.d.TracePlayback)
}

func (s *Server) getSections() []Section {
	return s.getSectionsCtx(context.Background())
}

func (s *Server) getSectionsCtx(ctx context.Context) []Section {
	if s == nil {
		return nil
	}
	if reqmemo.Active(ctx) {
		secs, _ := reqmemo.Do(ctx, "plex:sections", func() ([]Section, error) {
			return s.loadSections(ctx), nil
		})
		return secs
	}
	return s.loadSections(s.ctx)
}

func (s *Server) loadSections(ctx context.Context) []Section {
	if ctx == nil {
		ctx = s.ctx
	}
	// Stored publication is authoritative even when the last library was deleted.
	if secs := s.publishedKindSections(ctx); secs != nil {
		for i := range secs {
			secs[i] = s.sectionDefaults(secs[i])
		}
		return secs
	}
	if p := s.sectionsPtr.Load(); p != nil {
		return *p
	}
	return s.sections
}

func (s *Server) titleDetails() *titleCache {
	if s == nil {
		return nil
	}
	if p := s.titlesPtr.Load(); p != nil {
		return p
	}
	return s.titles
}

func (s *Server) homeCache() *homeCache {
	if s == nil {
		return nil
	}
	if p := s.homePtr.Load(); p != nil {
		return p
	}
	return s.home
}

func newDirectHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if proxy == nil {
		return nil
	}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok && tr != nil {
		cloned := tr.Clone()
		cloned.Proxy = proxy
		return &http.Client{Transport: cloned}
	}
	return &http.Client{Transport: &http.Transport{Proxy: proxy}}
}

// bindHost binds or updates the generic frontend host services on s.
// Non-nil services in h replace existing ones so a partial test mount
// preserves services configured at construction time.
func (s *Server) bindHost(h frontend.Host) {
	if s == nil {
		return
	}
	s.bindMu.Lock()
	curHost := s.getHost()
	curSvc := s.svc()
	if h.Support != nil {
		curHost.Support = h.Support
		curSvc.support = h.Support
	}
	if h.TracePlayback {
		curHost.TracePlayback = true
	}
	if h.LiveTV != nil {
		curHost.LiveTV = h.LiveTV
		curSvc.liveTV = h.LiveTV
	}
	if h.ClientSeen != nil {
		curHost.ClientSeen = h.ClientSeen
		curSvc.clientSeen = h.ClientSeen
	}
	if h.RemoteClientSeen != nil {
		curHost.RemoteClientSeen = h.RemoteClientSeen
		curSvc.remoteClientSeen = h.RemoteClientSeen
	}
	if h.Artwork != nil {
		curHost.Artwork = h.Artwork
		if rc := h.Artwork.RemoteCache(); rc != nil {
			curSvc.images = rc
		}
		if pc := h.Artwork.PaletteCache(); pc != nil {
			curSvc.ultra = pc
		}
		if pr := h.Artwork.PosterRenderer(); pr != nil {
			curSvc.generatedPosters = pr
		}
		if fn := h.Artwork.ImageURLFunc(); fn != nil {
			curSvc.imageURL = fn
		}
	}
	if h.Images != nil {
		curHost.Images = h.Images
	}
	if h.ImageGrants != nil {
		curHost.ImageGrants = h.ImageGrants
	}
	if h.Events != nil {
		curHost.Events = h.Events
	}
	if h.DirectProxy != nil {
		curHost.DirectProxy = h.DirectProxy
		if s.extraPlay != nil {
			s.extraPlay.setClient(newDirectHTTPClient(h.DirectProxy))
		}
	}
	if h.MetadataHTTP != nil {
		curHost.MetadataHTTP = h.MetadataHTTP
	}
	if h.ViewerPolicy != nil {
		curHost.ViewerPolicy = h.ViewerPolicy
	}
	if h.Capabilities != nil {
		curHost.Capabilities = h.Capabilities
	}
	if h.Identity != nil {
		curHost.Identity = h.Identity
	}
	if h.Log != nil {
		curHost.Log = h.Log
	}
	if h.ClientIP != nil {
		curHost.ClientIP = h.ClientIP
	}
	if h.SurfaceURL != nil {
		curHost.SurfaceURL = h.SurfaceURL
	}
	if h.PublicOrigin != nil {
		curHost.PublicOrigin = h.PublicOrigin
	}
	if h.RepoFileURL != nil {
		curHost.RepoFileURL = h.RepoFileURL
	}
	if h.Config != nil {
		curHost.Config = h.Config
	}
	if h.HostConfig != nil {
		curHost.HostConfig = h.HostConfig
	}
	if h.Now != nil {
		curHost.Now = h.Now
	}
	if h.ServerIdentity != (frontend.ServerIdentity{}) {
		curHost.ServerIdentity = h.ServerIdentity
	}
	if h.OwnerSubscription != nil {
		curHost.OwnerSubscription = h.OwnerSubscription
	}
	if h.MetadataLanguage != nil {
		curHost.MetadataLanguage = h.MetadataLanguage
	}
	if h.PersonalHomes != nil {
		curHost.PersonalHomes = h.PersonalHomes
	}
	if h.PageFreshness != nil {
		curHost.PageFreshness = h.PageFreshness
	}
	if h.TrackPreferences != nil {
		curHost.TrackPreferences = h.TrackPreferences
		curSvc.trackPrefs = h.TrackPreferences
	}
	if h.ExtraStreams != nil {
		curHost.ExtraStreams = h.ExtraStreams
		curSvc.extras = h.ExtraStreams
	}
	if h.Prerolls != nil {
		curHost.Prerolls = h.Prerolls
		curSvc.prerolls = h.Prerolls
	}
	if h.Subtitles != nil {
		curHost.Subtitles = h.Subtitles
		curSvc.subtitles = h.Subtitles
	}
	if h.Editions != nil {
		curHost.Editions = h.Editions
		curSvc.variants = h.Editions
	}
	if h.Experience != nil {
		curHost.Experience = h.Experience
	}
	if h.LazyStreams != nil {
		curHost.LazyStreams = h.LazyStreams
	}
	if h.LANStreams != nil {
		curHost.LANStreams = h.LANStreams
	}
	if h.Tickets != nil {
		curHost.Tickets = h.Tickets
	}
	if h.Sessions != nil {
		curHost.Sessions = h.Sessions
	}
	s.hostPtr.Store(&curHost)
	s.svcPtr.Store(&curSvc)
	if h.Experience != nil {
		s.attachExperienceLocked(h.Experience, &curSvc)
	}
	s.bindMu.Unlock()
	if h.Sessions != nil {
		s.usePlaybackBroker(h.Sessions)
	}
}

func (s *Server) attachExperienceLocked(exp frontend.Experience, curSvc *hostServices) {
	if s == nil || exp == nil {
		return
	}
	s.expState.Store(&expHolder{exp: exp})
	cat := exp.CatalogSource()
	titles := s.titleDetails()
	if !s.d.DisableEnrichment && cat != nil && titles == nil {
		s.titleJobs = newBGQueue(s.ctx, &s.wg, titleFetchWorkers, titleFetchQueueLen)
		s.blurJobs = newBGQueue(s.ctx, &s.wg, ultraBlurWorkers, ultraBlurQueueLen)
		titles = newTitleCache(s.titleJobs, s.now, s.log, s.enrichFetch)
		s.titlesPtr.Store(titles)
		if curSvc.ultra == nil {
			curSvc.ultra = frontend.NewArtworkPaletteCache(s.runtimeDB(), s.blurJobs.submit, s.now, s.log, func(ctx context.Context, key frontend.RatingKey, path string) (frontend.ArtworkPalette, error) {
				var art frontend.ArtworkCollectionArt
				if c, ok := s.collections().(frontend.ArtworkCollectionArt); ok {
					art = c
				}
				svc := s.svc()
				return frontend.ComputeArtworkItemPalette(ctx, svc.images, svc.imageURL, key, path, s.now(), art)
			})
			cp := *curSvc
			s.svcPtr.Store(&cp)
		}
	}
	if titles != nil {
		if pre, ok := cat.(frontend.StoredTitlePreloader); ok {
			titles.setPrefetch(pre.PreloadStored)
		} else {
			titles.setPrefetch(nil)
		}
	}
	if p, ok := cat.(frontend.ArtworkPersistenceCatalog); ok && curSvc.ultra != nil {
		curSvc.ultra.PersistFor(p.PersistsArtwork)
	}
	if s.homeCache() == nil {
		if _, hasRev, _ := exp.HomeRevision(s.ctx, nil); hasRev {
			hc := newHomeCache()
			s.homePtr.Store(hc)
			s.wg.Add(1)
			go s.homeRefresher()
		}
	}
	secs := s.buildSections()
	s.sectionsPtr.Store(&secs)
}

func (s *Server) policy() frontend.AccessPolicy {
	if s == nil {
		return nil
	}
	return s.getHost().ViewerPolicy
}

func (s *Server) listUsers(ctx context.Context) ([]frontend.User, error) {
	if s == nil {
		return nil, nil
	}
	id := s.getHost().Identity
	if id == nil {
		return nil, nil
	}
	return id.Users(ctx)
}

func (s *Server) exp() frontend.Experience {
	if s != nil {
		if p := s.expState.Load(); p != nil && p.exp != nil {
			return p.exp
		}
		if s.experience != nil {
			return s.experience
		}
	}
	return frontend.NoopExperience()
}
func (s *Server) libraryGUIDs() frontend.LibraryGUIDs {
	if s == nil {
		return nil
	}
	if s.plexGUID != nil {
		return s.plexGUID
	}
	return s.exp().LibraryGUIDs()
}
func (s *Server) libraryMetadata() frontend.LibraryMetadata {
	if s == nil {
		return nil
	}
	if s.plexMeta != nil {
		return s.plexMeta
	}
	return s.exp().LibraryMetadata()
}
func (s *Server) catalog() frontend.Catalog         { return s.exp().CatalogSource() }
func (s *Server) collections() frontend.Collections { return s.exp().CollectionsSource() }
func (s *Server) state() frontend.State             { return s.exp().StateSource() }
func (s *Server) playlists() frontend.Playlists     { return s.exp().PlaylistsSource() }
func (s *Server) recommender() frontend.Recommender { return s.exp().RecommenderSource() }
func (s *Server) vibeSearch() frontend.VibeSearcher { return s.exp().VibeSearchSource() }
func (s *Server) searchSources() frontend.SearchSources {
	return s.exp().SearchSources()
}
func (s *Server) favoriteShows() func(context.Context, *frontend.User) (map[frontend.RatingKey]bool, error) {
	return s.exp().FavoriteShowsFunc()
}
func (s *Server) filterAvailable() func(context.Context, []frontend.Title) []frontend.Title {
	return s.exp().FilterAvailableFunc()
}
func (s *Server) judgedOnFacts() func(context.Context) func(frontend.Title) bool {
	return s.exp().JudgedOnFactsFunc()
}
func (s *Server) prioritizeAvailable() func(context.Context, []frontend.Title) []frontend.Title {
	return s.exp().PrioritizeAvailableFunc()
}
func (s *Server) episodeOrder() frontend.EpisodeOrder    { return s.exp().EpisodeOrderSource() }
func (s *Server) discovery() *frontend.DiscoveryFeatures { return s.exp().DiscoveryFeatures() }
func (s *Server) dashboard() frontend.Dashboard {
	if d := s.exp().DashboardSource(); d != nil {
		return d
	}
	return noopDashboard{}
}
func (s *Server) resources() frontend.ResourceStatistics {
	if r := s.exp().ResourcesSource(); r != nil {
		return r
	}
	return noopResources{}
}
func (s *Server) libraryTitles() (movies, shows string) { return s.exp().LibraryTitles() }

func (s *Server) playback() frontend.Playback {
	if s == nil {
		return nil
	}
	if s.d.Playback != nil && s.d.Playback != s.initPlayback {
		return s.d.Playback
	}
	if rt := s.sessionRuntime(); rt != nil {
		if p := rt.Playback(); p != nil {
			return p
		}
	}
	if s.d.Playback != nil {
		return s.d.Playback
	}
	return s.initPlayback
}

func (s *Server) transcoder() frontend.Transcoder {
	if s == nil {
		return nil
	}
	if s.d.Transcoder != nil && s.d.Transcoder != s.initTranscoder {
		return s.d.Transcoder
	}
	if rt := s.sessionRuntime(); rt != nil {
		if t := rt.Transcoder(); t != nil {
			return t
		}
	}
	if s.d.Transcoder != nil {
		return s.d.Transcoder
	}
	return s.initTranscoder
}

func (s *Server) runtimeDB() *sql.DB {
	if s == nil {
		return nil
	}
	if s.cfg().Spike.Enabled {
		return s.d.DB
	}
	if rt := s.sessionRuntime(); rt != nil {
		if db := rt.DB(); db != nil {
			return db
		}
	}
	return s.d.DB
}

func (s *Server) clusterNode() frontend.ClusterNode {
	if s == nil {
		return nil
	}
	rt := s.sessionRuntime()
	if rt == nil {
		return nil
	}
	return rt.Cluster()
}

func (s *Server) sessionHeartbeat() time.Duration {
	if s == nil {
		return 0
	}
	rt := s.sessionRuntime()
	if rt == nil {
		return 0
	}
	return rt.SessionHeartbeat()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.getHost(); h.ServerIdentityFor != nil {
		if id, ok := h.ServerIdentityFor(r); ok && id.MachineID != "" {
			r = r.WithContext(context.WithValue(r.Context(), identityOverrideKey{}, id))
		}
	}
	s.router.ServeHTTP(w, r)
}

// identityOverrideKey carries the server identity one request is for when
// Ether presents several Plex servers on one port.
type identityOverrideKey struct{}

// identityCtx is identity as the request in ctx sees it: an extra Plex
// server's machine identifier and name when the request is for one.
func (s *Server) identityCtx(ctx context.Context) Identity {
	id := s.identity()
	if ctx == nil {
		return id
	}
	if o, ok := ctx.Value(identityOverrideKey{}).(frontend.ServerIdentity); ok {
		if o.MachineID != "" {
			id.MachineIdentifier = o.MachineID
		}
		if o.FriendlyName != "" {
			id.FriendlyName = o.FriendlyName
		}
		if o.Account != nil {
			// The extra server's own account: never the main owner's Plex Pass.
			id.MyPlexSubscription = o.Account.PlexPass
			id.OwnerFeatures = o.Account.OwnerFeatures
		}
	}
	return id
}

// Closed reports whether Close has begun.
func (s *Server) Closed() bool {
	return s == nil || s.ctx.Err() != nil
}

// Done is closed when the server is closed.
func (s *Server) Done() <-chan struct{} { return s.ctx.Done() }

// Close stops background goroutines and disconnects notification clients.
func (s *Server) Close() error {
	s.once.Do(func() {
		if s.runtimeDB() != nil {
			s.stopDownloadsForRestart()
		} else {
			s.releaseAllDownloads()
		}
		s.liveDiscovery.mu.Lock()
		s.liveDiscovery.closed = true
		if s.liveDiscovery.idleTimer != nil {
			s.liveDiscovery.idleTimer.Stop()
		}
		s.liveDiscovery.entries = nil
		s.liveDiscovery.mu.Unlock()
		s.liveGuideDay.close()
		s.cancel()

		s.notifier.close()
		s.wg.Wait()
		s.clearOwnDecisionArms()
		if err := s.saveDownloads(); err != nil {
			s.log.Warn("persist downloads on close", "err", err)
		}
		if s.cl != nil {
			s.cl.close()
		}
	})
	return nil
}

func (s *Server) settings() frontend.Settings {
	if s == nil {
		return nil
	}
	h := s.getHost()
	if h.Config == nil {
		return nil
	}
	return h.Config()
}

func (s *Server) hasConfig() bool {
	return s != nil && s.getHost().HostConfig != nil
}

func (s *Server) now() time.Time {
	if s != nil {
		if fn := s.getHost().Now; fn != nil {
			return fn()
		}
	}
	return time.Now()
}

func (s *Server) cfg() frontend.HostConfig {
	if s == nil {
		return frontend.HostConfig{}
	}
	h := s.getHost()
	if h.HostConfig == nil {
		return frontend.HostConfig{}
	}
	return h.HostConfig()
}

// identity resolves the effective identity.
func (s *Server) identity() Identity {
	var id Identity
	if s != nil {
		h := s.getHost()
		id = Identity{
			MachineIdentifier: h.ServerIdentity.MachineID,
			FriendlyName:      h.ServerIdentity.FriendlyName,
			Version:           h.ServerIdentity.Version,
			Platform:          h.ServerIdentity.Platform,
			PlatformVersion:   h.ServerIdentity.PlatformVersion,
		}
		if h.OwnerSubscription != nil {
			id.MyPlexSubscription, id.OwnerFeatures = h.OwnerSubscription()
		}
	}
	c := s.cfg()
	if id.MachineIdentifier == "" {
		id.MachineIdentifier = c.Server.MachineIdentifier
	}
	if id.MachineIdentifier == "" {
		id.MachineIdentifier = "ether"
	}
	if name := strings.TrimSpace(c.Server.FriendlyName); name != "" {
		id.FriendlyName = name
	} else if strings.TrimSpace(id.FriendlyName) == "" {
		if st := s.settings(); st != nil {
			id.FriendlyName = frontend.AdvertisedName(st)
		} else {
			id.FriendlyName = "Ether"
		}
	}
	if c.Plex.CompatibilityVersion != "" {
		id.Version = c.Plex.CompatibilityVersion
	} else if c.Plex.TrackLatestVersion && c.Plex.LatestCompatibilityVersion != "" {
		id.Version = c.Plex.LatestCompatibilityVersion
	}
	if id.Version == "" {
		id.Version = DefaultPMSVersion
	}
	if id.Platform == "" {
		switch runtime.GOOS {
		case "darwin":
			id.Platform = "MacOSX"
		case "windows":
			id.Platform = "Windows"
		default:
			id.Platform = "Linux"
		}
	}
	return id
}

// LibrarySections returns the canonical section identities for account
// registration. Titles follow the live config so renames publish to plex.tv.
// The copy prevents publishers from changing local libraries.
func (s *Server) LibrarySections() []Section {
	return s.withLibraryTitles(s.getSections())
}

func (s *Server) buildSections() []Section {
	if secs := s.publishedKindSections(s.ctx); secs != nil {
		for i := range secs {
			secs[i] = s.sectionDefaults(secs[i])
		}
		return secs
	}
	secs := s.d.Sections
	if len(secs) == 0 {
		secs = []Section{{ID: 1, Type: frontend.TypeMovie}}
		if !s.cfg().Spike.Enabled {
			secs = append(secs, Section{ID: 2, Type: frontend.TypeShow})
		}
	}
	out := make([]Section, len(secs))
	for i, sec := range secs {
		if sec.LibraryID == 0 && (sec.ID == int(frontend.LibraryMovies) || sec.ID == int(frontend.LibraryShows)) {
			sec.LibraryID = frontend.LibraryID(sec.ID)
		}
		out[i] = s.sectionDefaults(sec)
	}
	// Published plugin and filter libraries join the same section list
	// (spec §6.4). Each keeps its own stable UUID derived from its library id.
	kindSecs := s.publishedKindSections(s.ctx)
	for i := range kindSecs {
		kindSecs[i] = s.sectionDefaults(kindSecs[i])
	}
	out = mergeKindSections(out, kindSecs)
	return s.withLibraryTitles(out)
}

// sectionDefaults fills the language and the stable, library-id-derived UUID
// every section carries. The UUID is derived from the section id, which for
// every Plex-published library is the library id (spec §6.4), so existing
// built-in sections keep their UUIDs and every client keeps its state.
func (s *Server) sectionDefaults(sec Section) Section {
	if sec.Language == "" {
		sec.Language = "en-US"
	}
	if sec.UUID == "" {
		sum := sha1.Sum([]byte(fmt.Sprintf("%s/section/%d", s.identity().MachineIdentifier, sec.ID)))
		h := hex.EncodeToString(sum[:16])
		sec.UUID = h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	}
	return sec
}

// librarySectionTitle is the Movies / TV Shows name Ether advertises to
// Plex, from libraries 1/2 via Deps.LibraryTitles.
func librarySectionTitle(t frontend.MediaType, movies, shows string) string {
	switch t {
	case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
		if n := strings.TrimSpace(shows); n != "" {
			return n
		}
		return "TV Shows"
	default:
		if n := strings.TrimSpace(movies); n != "" {
			return n
		}
		return "Movies"
	}
}

// withLibraryTitles copies secs and applies the library names.
// IDs and UUIDs are not rewritten; UUIDs stay derived from section id only.
// Empty library names already resolve to Movies and TV Shows.
func (s *Server) withLibraryTitles(secs []Section) []Section {
	movies, shows := s.libraryTitles()
	out := append([]Section(nil), secs...)
	for i := range out {
		id := out[i].LibraryID
		if id == 0 && (out[i].ID == int(frontend.LibraryMovies) || out[i].ID == int(frontend.LibraryShows)) {
			id = frontend.LibraryID(out[i].ID)
		}
		switch id {
		case frontend.LibraryMovies:
			out[i].Title = librarySectionTitle(frontend.TypeMovie, movies, shows)
		case frontend.LibraryShows:
			out[i].Title = librarySectionTitle(frontend.TypeShow, movies, shows)
		}
	}
	return out
}

// sectionByID returns the section with the given id.
func (s *Server) sectionByID(id int) (Section, bool) {
	for _, sec := range s.withLibraryTitles(s.getSections()) {
		if sec.ID == id {
			return sec, true
		}
	}
	return Section{}, false
}

// sectionFor returns the section an item of type t lives in.
func (s *Server) sectionFor(t frontend.MediaType) (Section, bool) {
	return s.sectionForCtx(context.Background(), t)
}

func (s *Server) sectionForCtx(ctx context.Context, t frontend.MediaType) (Section, bool) {
	sec, ok := s.untitledSectionForCtx(ctx, t)
	if !ok {
		return Section{}, false
	}
	return s.withLibraryTitles([]Section{sec})[0], true
}

// untitledSectionFor is sectionFor without the configured title. A
// visibility check needs only the section id, and runs for every title a
// page shows, so it reads no configuration.
func (s *Server) untitledSectionFor(t frontend.MediaType) (Section, bool) {
	return s.untitledSectionForCtx(context.Background(), t)
}

func (s *Server) untitledSectionForCtx(ctx context.Context, t frontend.MediaType) (Section, bool) {
	want := frontend.TypeMovie
	switch t {
	case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
		want = frontend.TypeShow
	}
	secs := s.getSectionsCtx(ctx)
	for _, sec := range secs {
		if !isKindSection(sec) && sec.Type == want {
			return sec, true
		}
	}
	for _, sec := range secs {
		if sec.Type == want {
			return sec, true
		}
	}
	return Section{}, false
}

// userSections lists the sections a user may see.
func (s *Server) userSections(u *frontend.User) []Section {
	secs := s.withLibraryTitles(s.getSections())
	if u == nil || len(u.AllowedLibraries) == 0 || u.IsOwner {
		return secs
	}
	var out []Section
	for _, sec := range secs {
		if frontend.SectionAllowed(u, sec.ID) {
			out = append(out, sec)
		}
	}
	return out
}

func (s *Server) canSeeSection(u *frontend.User, id int) bool {
	for _, sec := range s.getSections() {
		if sec.ID == id {
			return u == nil || len(u.AllowedLibraries) == 0 || u.IsOwner || frontend.SectionAllowed(u, sec.ID)
		}
	}
	return false
}

func (s *Server) expireTranscodes(now time.Time, ttl time.Duration) {
	for _, id := range s.transcodes.expire(now, ttl) {
		s.revokeReadCapability(id)
	}
}

// janitor expires idle play queues, sessions and transcode bindings.
func (s *Server) janitor() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			now := s.now()

			s.expireDownloads(now)
			s.queues.Expire(now, 6*time.Hour)
			s.playIDs.expire(now, s.playIdentityAlive)
			s.expireTranscodes(now, 2*time.Hour)
			s.playbackOwners.Expire(now, 2*time.Hour)
			s.extraPlay.expire(now, 2*time.Hour)
			s.expireTerminations(now, terminationTTL)
			s.refreshDecisionArms(now)
			s.starts.Expire(now)
			expired := s.expirePlaybackActivities(now, 3*time.Minute)
			for _, e := range expired {
				s.notifyPlaying(e, "stopped")
				// No timelines or media fetches remain: release playback.
				s.endExpiredEntry(e)
			}
			if len(expired) > 0 {
				s.cl.poke()
			}
		}
	}
}

func trimSlash(p string) string { return strings.TrimSuffix(p, "/") }

// releaseMountedPlayback stops encoders, trickplay, and downloads this server
// started. The HTTP server itself stays up so an in-flight request can finish
// and a later mount can reuse it.
func (s *Server) releaseMountedPlayback() {
	if s == nil {
		return
	}
	s.usePlaybackBroker(nil)
	var ids []string
	if s.transcodes != nil {
		ids = s.transcodes.ids()
	}
	for _, id := range ids {
		if s.transcoder() != nil {
			s.stopPlexHLS(id)
		}
		s.dropTranscode(id)
	}

	if s.runtimeDB() != nil {
		s.stopDownloadsForRestart()
		s.stageDownloads()
	} else {
		s.releaseAllDownloads()
	}
}

type noopDashboard struct{}

func (noopDashboard) SeeDevice(string, string, string) frontend.StatisticsDevice {
	return frontend.StatisticsDevice{}
}
func (noopDashboard) Devices() []frontend.StatisticsDevice { return nil }
func (noopDashboard) Device(int64) (frontend.StatisticsDevice, bool) {
	return frontend.StatisticsDevice{}, false
}
func (noopDashboard) Bandwidth(frontend.StatisticsQuery) frontend.StatisticsPage {
	return frontend.StatisticsPage{}
}
func (noopDashboard) Media(frontend.StatisticsQuery) frontend.StatisticsPage {
	return frontend.StatisticsPage{}
}
func (noopDashboard) RecordBytes(int64, string, string, string, string, bool, int64) {}
func (noopDashboard) RecordPlay(int64, string, string, int, int64)                   {}

type noopResources struct{}

func (noopResources) Query(int, int64, int64, int64, bool, bool, bool) []frontend.ResourceSample {
	return nil
}

// accountCtx is the plex.tv owner and claim state of the server the request
// in ctx is for: an extra Plex server's own account, else the main one.
func (s *Server) accountCtx(ctx context.Context) (owner string, claimed bool) {
	if ctx != nil {
		if o, ok := ctx.Value(identityOverrideKey{}).(frontend.ServerIdentity); ok && o.Account != nil {
			return o.Account.OwnerUsername, o.Account.Claimed
		}
	}
	c := s.cfg()
	return c.Plex.OwnerUsername, c.Plex.Claimed || c.Plex.Token != ""
}
