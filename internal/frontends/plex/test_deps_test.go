package plex

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type testHomeCache struct {
	Revision       func(context.Context, *frontend.User) (frontend.HomeRevision, error)
	NoProviderWait func(context.Context) context.Context
	Layout         func() uint64
	Config         func() uint64
	Catalog        func() uint64
}

type testServerDeps struct {
	Deps
	Experience          frontend.Experience
	HomeCache           *testHomeCache
	Catalog             frontend.Catalog
	Collections         frontend.Collections
	State               frontend.State
	Playlists           frontend.Playlists
	Recommender         frontend.Recommender
	VibeSearch          frontend.VibeSearcher
	Search              frontend.SearchSources
	FavoriteShows       func(context.Context, *frontend.User) (map[frontend.RatingKey]bool, error)
	FilterAvailable     func(context.Context, []frontend.Title) []frontend.Title
	JudgedOnFacts       func(context.Context) func(frontend.Title) bool
	PrioritizeAvailable func(context.Context, []frontend.Title) []frontend.Title
	EpisodeOrder        frontend.EpisodeOrder
	Discovery           *frontend.DiscoveryFeatures
	Dashboard           frontend.Dashboard
	Resources           frontend.ResourceStatistics
	LibraryTitles       func() (movies, shows string)
}

func hostOf[T any](d *T) *frontend.Host {
	if d == nil {
		return nil
	}
	switch v := any(d).(type) {
	case *Deps:
		return &v.Host
	case *testServerDeps:
		return &v.Deps.Host
	default:
		panic("unsupported deps pointer")
	}
}

func (d testServerDeps) experienceService() frontend.Experience {
	if d.Experience != nil {
		return d.Experience
	}
	var rev func(context.Context, *frontend.User) (frontend.HomeRevision, error)
	var noWait func(context.Context) context.Context
	if d.HomeCache != nil {
		rev = d.HomeCache.Revision
		noWait = d.HomeCache.NoProviderWait
	}
	return experience.NewDirectService(experience.ServiceDeps{
		Catalog:             d.Catalog,
		Collections:         d.Collections,
		State:               d.State,
		Playlists:           d.Playlists,
		Recommender:         d.Recommender,
		VibeSearch:          d.VibeSearch,
		SearchSources:       d.Search,
		FavoriteShows:       d.FavoriteShows,
		FilterAvailable:     d.FilterAvailable,
		JudgedOnFacts:       d.JudgedOnFacts,
		PrioritizeAvailable: d.PrioritizeAvailable,
		EpisodeOrder:        d.EpisodeOrder,
		Discovery:           d.Discovery,
		Dashboard:           d.Dashboard,
		Resources:           d.Resources,
		LibraryTitles:       d.LibraryTitles,
		HomeRevision:        rev,
		NoProviderWait:      noWait,
		Now:                 d.Deps.Host.Now,
	})
}

func (d testServerDeps) pageFreshnessFunc() func() frontend.PageFreshness {
	if d.HomeCache == nil {
		return nil
	}
	hc := d.HomeCache
	return func() frontend.PageFreshness {
		var f frontend.PageFreshness
		if hc.Layout != nil {
			f.Layout = hc.Layout()
		}
		if hc.Config != nil {
			f.Config = hc.Config()
		}
		if hc.Catalog != nil {
			f.Catalog = hc.Catalog()
		}
		return f
	}
}

// testStores returns the real sessions-backed store factory for tests.
func testStores() *sessions.LocalRuntime {
	return sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})
}

// testRuntime is the session runtime production hands to the Plex server: a
// real sessions.LocalRuntime with replaceable playback, transcoder and DB.
// Production wires NewServer(Deps{Host: host}) and reaches everything through
// host.Sessions; the fixture does the same, so a handler that reads raw
// s.d.Playback / s.d.Transcoder / s.d.DB fails here exactly as it would in
// production.
type testRuntime struct {
	*sessions.LocalRuntime
	playback   atomic.Value // core.Playback
	transcoder atomic.Value // core.Transcoder
	db         atomic.Value // *sql.DB
}

func newTestRuntime() *testRuntime {
	return &testRuntime{LocalRuntime: sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})}
}

func (r *testRuntime) setPlayback(p core.Playback) {
	if r == nil {
		return
	}
	r.playback.Store(&p)
}
func (r *testRuntime) setTranscoder(t core.Transcoder) {
	if r == nil {
		return
	}
	r.transcoder.Store(&t)
}
func (r *testRuntime) setDB(db *sql.DB) {
	if r == nil {
		return
	}
	r.db.Store(&db)
}

func (r *testRuntime) Playback() frontend.Playback {
	if p, ok := r.playback.Load().(*core.Playback); ok && p != nil && *p != nil {
		return *p
	}
	return r.LocalRuntime.Playback()
}
func (r *testRuntime) Transcoder() frontend.Transcoder {
	if t, ok := r.transcoder.Load().(*core.Transcoder); ok && t != nil && *t != nil {
		return *t
	}
	return r.LocalRuntime.Transcoder()
}
func (r *testRuntime) DB() *sql.DB {
	if db, ok := r.db.Load().(**sql.DB); ok && db != nil && *db != nil {
		return *db
	}
	return r.LocalRuntime.DB()
}

// currentPlayback and currentTranscoder return what the runtime holds without
// re-entering the broker, so fixture wrappers do not recurse through the
// surface that wraps them.
func (r *testRuntime) currentPlayback() core.Playback {
	if p, ok := r.playback.Load().(*core.Playback); ok && p != nil {
		return *p
	}
	return nil
}
func (r *testRuntime) currentTranscoder() core.Transcoder {
	if t, ok := r.transcoder.Load().(*core.Transcoder); ok && t != nil {
		return *t
	}
	return nil
}

func withTestEncoder(enc frontend.SegmentedEncoder) func(*testServerDeps) {
	return func(d *testServerDeps) {
		rt := newTestRuntime()
		rt.LocalRuntime = sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{Encoder: enc})
		d.Host.Sessions = rt
	}
}

// testDeps fills the required host factories in d, leaving explicitly set
// fields alone. Playback, transcoder and DB live only on the session runtime,
// never on Deps, mirroring production wiring.
func testDeps(d Deps) Deps {
	if d.Host.Sessions == nil {
		d.Host.Sessions = newTestRuntime()
	}
	if d.Host.Experience == nil {
		d.Host.Experience = experience.NewDirectService(experience.ServiceDeps{Now: d.Host.Now})
	}
	return d
}

func newTestServer(d testServerDeps) *Server {
	if d.Deps.Host.Sessions == nil {
		rt := newTestRuntime()
		rt.setPlayback(d.Playback)
		rt.setTranscoder(d.Transcoder)
		rt.setDB(d.DB)
		d.Deps.Host.Sessions = rt
	} else if rt, ok := d.Host.Sessions.(*testRuntime); ok {
		if d.Playback != nil {
			rt.setPlayback(d.Playback)
		}
		if d.Transcoder != nil {
			rt.setTranscoder(d.Transcoder)
		}
		if d.DB != nil {
			rt.setDB(d.DB)
		}
	} else if lr, ok := d.Host.Sessions.(*sessions.LocalRuntime); ok {
		lr.FillDefaults(sessions.LocalRuntimeDeps{
			Playback:   d.Playback,
			Transcoder: d.Transcoder,
			DB:         d.DB,
		})
	}
	if d.Deps.Host.Experience == nil {
		d.Deps.Host.Experience = d.experienceService()
	}
	if pf := d.pageFreshnessFunc(); pf != nil {
		d.Deps.Host.PageFreshness = pf
	}
	d.Deps = testDeps(d.Deps)
	return NewServer(d.Deps)
}

// setPlayback replaces the playback implementation the server sees through its
// session runtime, so the change is visible exactly where production reads it.
func (s *Server) setPlayback(p core.Playback) {
	if s == nil {
		return
	}
	if rt := s.testRuntime(); rt != nil {
		rt.setPlayback(p)
	}
}

// setTranscoder replaces the transcoder implementation through the runtime.
func (s *Server) setTranscoder(t core.Transcoder) {
	if s == nil {
		return
	}
	if rt := s.testRuntime(); rt != nil {
		rt.setTranscoder(t)
	}
}

// setDB replaces the durable store the server sees through the runtime.
func (s *Server) setDB(db *sql.DB) {
	if s == nil {
		return
	}
	if rt := s.testRuntime(); rt != nil {
		rt.setDB(db)
	}
}

// testRuntime returns the fixture session runtime from the bound host. It stays
// reachable there even after a broker mount swaps the runtime the server
// resolves its playback and transcoder through.
func (s *Server) testRuntime() *testRuntime {
	if s == nil {
		return nil
	}
	rt, _ := s.getHost().Sessions.(*testRuntime)
	return rt
}

func (s *Server) currentTestDeps() testServerDeps {
	if s == nil {
		return testServerDeps{}
	}
	deps := s.d
	deps.Host = s.getHost()
	deps.Host.Experience = nil
	var libTitles func() (string, string)
	if m, sh := s.exp().LibraryTitles(); m != "" || sh != "" {
		libTitles = func() (string, string) { return m, sh }
	}
	return testServerDeps{
		Deps:                deps,
		Catalog:             s.catalog(),
		Collections:         s.collections(),
		State:               s.state(),
		Playlists:           s.playlists(),
		Recommender:         s.recommender(),
		VibeSearch:          s.vibeSearch(),
		Search:              s.searchSources(),
		FavoriteShows:       s.favoriteShows(),
		FilterAvailable:     s.filterAvailable(),
		JudgedOnFacts:       s.judgedOnFacts(),
		PrioritizeAvailable: s.prioritizeAvailable(),
		EpisodeOrder:        s.episodeOrder(),
		Discovery:           s.discovery(),
		Dashboard:           s.dashboard(),
		Resources:           s.resources(),
		LibraryTitles:       libTitles,
	}
}

func (s *Server) updateTestDeps(fn func(*testServerDeps)) {
	d := s.currentTestDeps()
	fn(&d)
	s.bindHost(frontend.Host{
		Experience:    d.experienceService(),
		PageFreshness: d.pageFreshnessFunc(),
	})
}

func (s *Server) setCatalog(v frontend.Catalog) {
	s.updateTestDeps(func(d *testServerDeps) { d.Catalog = v })
}
func (s *Server) setCollections(v frontend.Collections) {
	s.updateTestDeps(func(d *testServerDeps) { d.Collections = v })
}
func (s *Server) setState(v frontend.State) {
	s.updateTestDeps(func(d *testServerDeps) { d.State = v })
}
func (s *Server) setRecommender(v frontend.Recommender) {
	s.updateTestDeps(func(d *testServerDeps) { d.Recommender = v })
}
func (s *Server) setFilterAvailable(v func(context.Context, []frontend.Title) []frontend.Title) {
	s.updateTestDeps(func(d *testServerDeps) { d.FilterAvailable = v })
}
func (s *Server) setEpisodeOrder(v frontend.EpisodeOrder) {
	s.updateTestDeps(func(d *testServerDeps) { d.EpisodeOrder = v })
}
func (s *Server) setDiscovery(v *frontend.DiscoveryFeatures) {
	s.updateTestDeps(func(d *testServerDeps) { d.Discovery = v })
}
func (s *Server) setDashboard(v frontend.Dashboard) {
	s.updateTestDeps(func(d *testServerDeps) { d.Dashboard = v })
}
func (s *Server) setResources(v frontend.ResourceStatistics) {
	s.updateTestDeps(func(d *testServerDeps) { d.Resources = v })
}
func (s *Server) setLibraryTitles(v func() (string, string)) {
	s.updateTestDeps(func(d *testServerDeps) { d.LibraryTitles = v })
}

func (s *Server) setImageURL(fn frontend.ImageURLFunc) {
	if fn == nil {
		fn = func(path, size string) string { return "https://image.tmdb.org/t/p/" + size + path }
	}
	s.mutateServices(func(svc *hostServices) { svc.imageURL = fn })
}

func (s *Server) setImages(cache frontend.ArtworkImageCache) {
	s.mutateServices(func(svc *hostServices) { svc.images = cache })
}

func (s *Server) setUltra(cache frontend.ArtworkPaletteCache) {
	s.mutateServices(func(svc *hostServices) { svc.ultra = cache })
}

func (s *Server) setGeneratedPosters(g frontend.TitlePosterRenderer) {
	s.mutateServices(func(svc *hostServices) { svc.generatedPosters = g })
}

func (s *Server) setLiveTV(ltv frontend.LiveTV) {
	s.mutateServices(func(svc *hostServices) { svc.liveTV = ltv })
}

func (s *Server) setSupport(rec frontend.DiagnosticRecorder) {
	s.mutateServices(func(svc *hostServices) { svc.support = rec })
}

func (s *Server) setClientSeen(fn func(string)) {
	s.mutateServices(func(svc *hostServices) { svc.clientSeen = fn })
}

func testServer(d Deps) *Server {
	d = testDeps(d)
	rt, _ := d.Host.Sessions.(frontend.SessionRuntime)
	return &Server{
		d:             d,
		host:          d.Host,
		runtime:       rt,
		streamRuntime: rt,
		experience:    d.Host.Experience,
		log:           slog.Default(),
	}
}

// updateTestDeps mutates the bound host services on a running server, keeping
// the session runtime that carries playback, transcoder and DB untouched.
func setDepsConfig[T any](d *T, fn func() config.Config) {
	h := hostOf(d)
	if h == nil {
		return
	}
	h.HostConfig = fn
	if fn == nil {
		h.Config = nil
		return
	}
	h.Config = func() frontend.Settings {
		return frontend.Settings{"serverName": fn().Server.FriendlyName}
	}
}

func depsConfig[T any](d *T) func() config.Config {
	h := hostOf(d)
	if h == nil {
		return nil
	}
	return h.HostConfig
}

func (s *Server) setTestConfig(fn func() config.Config) {
	s.mutateHost(func(h *frontend.Host) {
		h.HostConfig = fn
		if fn == nil {
			h.Config = nil
			return
		}
		h.Config = func() frontend.Settings {
			return frontend.Settings{"serverName": fn().Server.FriendlyName}
		}
	})
}

func (s *Server) testConfig() func() config.Config {
	if s == nil {
		return nil
	}
	return s.getHost().HostConfig
}

func setDepsNow[T any](d *T, fn func() time.Time) {
	if h := hostOf(d); h != nil {
		h.Now = fn
	}
}

func (s *Server) setTestNow(fn func() time.Time) {
	s.mutateHost(func(h *frontend.Host) { h.Now = fn })
}

func setDepsIdentity[T any](d *T, id Identity) {
	h := hostOf(d)
	if h == nil {
		return
	}
	if id.MachineIdentifier != "" {
		h.ServerIdentity.MachineID = id.MachineIdentifier
	}
	if id.FriendlyName != "" {
		h.ServerIdentity.FriendlyName = id.FriendlyName
	}
	if id.Version != "" {
		h.ServerIdentity.Version = id.Version
	}
	if id.Platform != "" {
		h.ServerIdentity.Platform = id.Platform
	}
	if id.PlatformVersion != "" {
		h.ServerIdentity.PlatformVersion = id.PlatformVersion
	}
	if id.MyPlexSubscription || len(id.OwnerFeatures) > 0 {
		sub, feats := id.MyPlexSubscription, append([]string(nil), id.OwnerFeatures...)
		h.OwnerSubscription = func() (bool, []string) { return sub, feats }
	}
}

func (s *Server) setTestSubscription(sub bool) {
	s.mutateHost(func(h *frontend.Host) {
		var feats []string
		if h.OwnerSubscription != nil {
			_, feats = h.OwnerSubscription()
		}
		h.OwnerSubscription = func() (bool, []string) { return sub, feats }
	})
}

func (s *Server) setTestPlatformVersion(v string) {
	s.mutateHost(func(h *frontend.Host) { h.ServerIdentity.PlatformVersion = v })
}

func setDepsOwnerSubscription[T any](d *T, fn func() (bool, []string)) {
	if h := hostOf(d); h != nil {
		h.OwnerSubscription = fn
	}
}

func (s *Server) setTestRepoFileURL(fn func(string) string) {
	s.mutateHost(func(h *frontend.Host) { h.RepoFileURL = fn })
}

func (s *Server) setTestPublicURL(fn func() string) {
	s.mutateHost(func(h *frontend.Host) { h.PublicOrigin = fn })
}

func setDepsAuth[T any](d *T, a core.Auth) {
	if h := hostOf(d); h != nil {
		h.Identity = frontend.BindAuth(a)
	}
}

func (s *Server) setTestAuth(a core.Auth) {
	s.mutateHost(func(h *frontend.Host) { h.Identity = frontend.BindAuth(a) })
}

func (s *Server) testAuth() *fakeAuth {
	if u, ok := s.getHost().Identity.(interface{ UnwrapAuth() core.Auth }); ok {
		fa, _ := u.UnwrapAuth().(*fakeAuth)
		return fa
	}
	return nil
}

func setDepsPolicy[T any](d *T, p frontend.ViewerPolicy) {
	if h := hostOf(d); h != nil {
		h.ViewerPolicy = p
	}
}

func (s *Server) setTestPolicy(p frontend.ViewerPolicy) {
	s.mutateHost(func(h *frontend.Host) { h.ViewerPolicy = p })
}

func setDepsHTTPClient[T any](d *T, c *http.Client) {
	if h := hostOf(d); h != nil {
		h.MetadataHTTP = c
	}
}

func TestNewServerRequiresSessionStores(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewServer without SessionStores did not panic")
		}
	}()
	NewServer(Deps{})
}
