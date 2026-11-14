package onair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/braedonsaunders/ether/internal/faillog"
	"github.com/braedonsaunders/ether/internal/onair/encode"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/fonts"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"golang.org/x/image/font/opentype"
	"image"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Enabled func() bool

	// Showcase adds the decade looks and commercial reels used by the shipped showcase channels.
	Showcase bool
	// Changed runs after an owner style save has committed and locks are released.
	Changed        func(context.Context) error
	ReelsChanged   func(context.Context)
	StyleChannels  func(context.Context) (map[string][]string, error)
	ReelChannels   func(context.Context) (map[string][]string, error)
	PluginSegment  func(string) (PluginSegment, bool)
	StaticRequests func(context.Context, string) []core.ClipRequest
	DB             *sql.DB
	Chat           Chatter
	Dir            string
	FFmpeg         func() string
	Probe          func(context.Context, string) (*core.MediaInfo, error)
	Acquire        func(context.Context) (context.Context, func(), error)
	Bindings       func(context.Context, string, string, time.Time) (any, error)
	// Upcoming supplies requests for the next three hours of watched channels.
	Upcoming       func(context.Context) []core.ClipRequest
	Logger         *slog.Logger
	CacheBytes     int64
	LinkCacheBytes int64
	WorkingSize    func() (int, int)
	EncoderOptions func(int, int) ([]string, []string)
	Artwork        func(context.Context, string) (image.Image, error)
	// Render is a seam for verifying that Ensure never executes work inline.
	Render func(context.Context, scene.Document, raster.Bindings, string) error
	// RenderFrame is an optional seam for testing live frame rasterization speed gates.
	RenderFrame func(scene.Document, raster.Bindings, float64, int, int) *image.RGBA
	// NormalizeClip is a seam for normalizing downloaded link media in offline tests.
	NormalizeClip func(ctx context.Context, reel Reel, src, out string) (time.Duration, error)
	// ValidateURL, ArchiveOrigin, HTTPClient and Now are seams for offline unit tests.
	ValidateURL   func(context.Context, string) error
	ArchiveOrigin string
	HTTPClient    *http.Client
	Now           func() time.Time
	// AssetDir is the blob store for asset bytes (<data>/onair/assets);
	// empty uses Dir/assets.
	AssetDir string
	// NodeID identifies this node in onair_asset_replicas.
	NodeID string
	// Members lists every node sharing the database (cluster members seen
	// in the last day); nil means this node alone.
	Members func(context.Context) ([]Member, error)
	// AssetsChanged asks every node to run an asset pass (PokeAssets).
	AssetsChanged func(context.Context)
	// Downloader resolves and downloads video/audio pages via yt-dlp or the
	// owner's configured resolver command.
	Downloader *extras.Downloader
}
type entry struct {
	Ref  core.ClipRef
	Size int64
	Used time.Time
	Pin  time.Time
}
type renderJob struct {
	Boundary    time.Time
	Scene       StyleScene
	download    bool
	AssetDigest string
	Hash        string
	Look        string
	Doc         scene.Document
	Bind        raster.Bindings
	Request     core.ClipRequest
	prepare     func(context.Context) error
	convert     func(context.Context) error
}
type Service struct {
	liveJobs       []renderJob
	liveReady      chan struct{}
	upcomingWake   chan struct{}
	activeCancel   context.CancelFunc
	activeBoundary time.Time
	activeHash     string
	activeChannel  string
	// unwanted holds channels whose queued renders process must drop: a
	// disabled, deleted or no-longer-watched channel. A fresh request for
	// the channel clears it (enqueue).
	unwanted map[string]bool
	// hashChannel remembers which channel a pinned hash belongs to, so
	// cancelling the channel releases its pins. Entries die with their pin.
	hashChannel map[string]string
	// wantedChannels is the channel set of the last watchUpcoming tick;
	// channels that fall out of it are cancelled.
	wantedChannels map[string]bool
	resources      string
	reelReady      map[string]bool
	linkCache      map[string]linkEntry
	reelClipIndex  map[string]reelClipMeta
	linkChannels   map[string]map[string]bool
	linkCancels    map[string]context.CancelFunc
	manualLink     map[string]bool
	linkFailLog    *faillog.Tracker
	o              Options
	db             *sql.DB
	mu             sync.Mutex
	assetMu        sync.Mutex
	blobs          blobStore
	nodeID         string
	assetPoke      chan struct{}
	fetchMu        sync.Mutex
	fetching       map[string]chan struct{}
	looks          map[string]Look
	cache          map[string]entry
	pins           map[string]time.Time
	pending        map[string]bool
	liveSaving     map[string]bool
	liveSpeed      map[string]liveSpeedRecord
	vis            VisibilitySnapshot
	lastVisLog     time.Time
	lastVisMode    string
	// failed holds a retry time per content hash after a render error, so a
	// boundary that re-requests every tick cannot spin a failing render.
	failed        map[string]failure
	renderTimes   map[string]time.Duration
	progress      map[string]*Progress
	lookJobs      map[string][]string
	importCancels map[string]context.CancelFunc
	jobs          chan renderJob
	clipJobs      chan renderJob
	importJobs    chan renderJob
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	startOnce     sync.Once
	lifecycleMu   sync.Mutex
	running       bool
	initialized   bool
}

var _ core.ChannelGraphics = (*Service)(nil)

func New(o Options) (*Service, error) {
	if o.DB == nil {
		return nil, fmt.Errorf("onair: database required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 2 << 30
	}
	if o.LinkCacheBytes <= 0 {
		o.LinkCacheBytes = defaultLinkCacheBytes
	}
	if e := os.MkdirAll(o.Dir, 0750); e != nil {
		return nil, e
	}
	if o.AssetDir == "" {
		o.AssetDir = filepath.Join(o.Dir, "assets")
	}
	if o.NodeID == "" {
		o.NodeID = "local"
	}
	if o.Downloader == nil {
		o.Downloader = extras.NewDownloader(extras.DownloaderOptions{
			ToolsDir: filepath.Join(o.Dir, "tools"),
			FFmpeg:   o.FFmpeg,
		})
	}
	linkFail := faillog.New(o.Logger, "onair: link clip resolution")
	if o.Now != nil {
		linkFail.Now = o.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{blobs: blobStore{dir: o.AssetDir}, nodeID: o.NodeID, assetPoke: make(chan struct{}, 1), upcomingWake: make(chan struct{}, 1), fetching: map[string]chan struct{}{}, reelReady: map[string]bool{}, linkCache: map[string]linkEntry{}, reelClipIndex: map[string]reelClipMeta{}, linkChannels: map[string]map[string]bool{}, linkCancels: map[string]context.CancelFunc{}, manualLink: map[string]bool{}, linkFailLog: linkFail, resources: fonts.Version(), o: o, db: o.DB, looks: map[string]Look{}, cache: map[string]entry{}, pending: map[string]bool{}, liveSaving: map[string]bool{}, liveSpeed: map[string]liveSpeedRecord{}, renderTimes: map[string]time.Duration{}, pins: map[string]time.Time{}, progress: map[string]*Progress{}, lookJobs: map[string][]string{}, importCancels: map[string]context.CancelFunc{}, liveReady: make(chan struct{}, 1), jobs: make(chan renderJob, 128), clipJobs: make(chan renderJob, 64), importJobs: make(chan renderJob, 64), ctx: ctx, cancel: cancel}

	if o.Enabled == nil || o.Enabled() {
		if err := s.initialize(); err != nil {
			return nil, err
		}
	}
	// Strict demand-driven rule: zero background rendering at boot, showcase
	// install, or look save. Looks render only when an active viewer tunes a
	// channel using them (rendered live + saved on cold boundary or via
	// watchUpcoming for actively watched channels).
	return s, nil
}

func (s *Service) initialize() error {
	if s.initialized {
		return nil
	}

	if e := s.seed(s.ctx); e != nil {
		s.cancel()
		return e
	}
	if s.o.Showcase {
		if e := s.seedShowcase(s.ctx); e != nil {
			s.cancel()
			return e
		}
	}
	ls, e := s.Looks(s.ctx)
	if e != nil {
		s.cancel()
		return e
	}
	s.mu.Lock()
	for _, l := range ls {
		s.looks[l.ID] = l
	}
	s.mu.Unlock()
	s.loadCache()
	s.loadLinkCache()
	s.loadReels()
	for _, l := range ls {
		s.refreshLookHashes(l)
	}

	if s.o.Enabled == nil || s.o.Enabled() {
		s.resumeConversions()
		s.resumeImports()
	}
	s.initialized = true
	return nil
}

// WakeUpcoming signals watchUpcoming when viewer demand or look configuration starts.
func (s *Service) WakeUpcoming() {
	if s == nil || s.upcomingWake == nil {
		return
	}
	select {
	case s.upcomingWake <- struct{}{}:
	default:
	}
}

// Downloader returns the yt-dlp downloader shared with this service.
func (s *Service) Downloader() *extras.Downloader { return s.o.Downloader }

// Start begins workers after application dependencies have been constructed.
func (s *Service) Start() {
	if s.o.Enabled != nil && !s.o.Enabled() {
		return
	}
	s.startOnce.Do(func() {
		if s.ctx.Err() == nil {
			s.wg.Add(4)
			go s.run()
			go s.watchUpcoming()
			go s.backfillFacts()
			go s.runAssets()
			for range 2 {
				s.wg.Add(1)
				go s.runImports()
			}
		}
	})
}

// SetEnabled starts or drains all On Air workers with the shared Live TV switch.
func (s *Service) SetEnabled(enabled bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if enabled == s.running {
		return
	}
	if enabled {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		if err := s.initialize(); err != nil {
			s.o.Logger.Error("onair initialization failed", "error", err)
			return
		}
		s.startOnce = sync.Once{}
		s.Start()
	} else {
		s.cancel()
		s.wg.Wait()
	}
	s.running = enabled
}
func (s *Service) Close() { s.cancel(); s.wg.Wait() }
func (s *Service) Duration(id, m string) (time.Duration, bool) {
	if strings.HasPrefix(m, "plugin:") {
		if s.o.PluginSegment != nil {
			v, ok := s.o.PluginSegment(m)
			return time.Duration(v.Document.Duration * float64(time.Second)), ok
		}
		return 0, false
	}
	if strings.HasPrefix(m, "screen.") {
		switch m {
		case "screen.card", "screen.static", "screen.bars", "screen.brb", "screen.testcard":
			return 4 * time.Second, true
		}
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.looks[id]
	if !ok {
		return 0, false
	}
	if m == "standby" {
		if v, found := l.Roles["standby"]; found {
			for _, sc := range l.Scenes {
				if sc.Role == "standby" && sc.Enabled {
					return time.Duration(v.Duration * float64(time.Second)), true
				}
			}
		}
		for _, fallback := range []string{"break", "ident"} {
			for _, sc := range l.Scenes {
				if sc.Role == fallback && sc.Enabled {
					if _, valid := scene.StandbyBase(sc.Document); valid {
						dur := sc.Document.Duration
						if v, found := l.Roles[fallback]; found && v.Duration > 0 {
							dur = v.Duration
						}
						return time.Duration(dur * float64(time.Second)), true
					}
				}
			}
		}
		return 0, false
	}
	v, found := l.Roles[m]
	enabled := false
	for _, sc := range l.Scenes {
		if sc.Role == m && sc.Enabled {
			enabled = true
			break
		}
	}
	return time.Duration(v.Duration * float64(time.Second)), ok && found && enabled
}

// planRaw resolves the full scene document and full caller bindings (including
// live programme/viewer slots and overlay metadata).
func (s *Service) planRaw(r core.ClipRequest) (renderJob, bool) {
	if s.o.Enabled != nil && !s.o.Enabled() {
		return renderJob{}, false
	}
	var plugin PluginSegment
	isPlugin := strings.HasPrefix(r.Moment, "plugin:")
	if isPlugin {
		if s.o.PluginSegment == nil {
			return renderJob{}, false
		}
		var found bool
		plugin, found = s.o.PluginSegment(r.Moment)
		if !found || !time.Now().Before(plugin.Expires) {
			return renderJob{}, false
		}
		bind := make(map[string]string, len(r.Bind)+len(plugin.Slots)+1)
		for k, v := range r.Bind {
			bind[k] = v
		}
		for k, v := range plugin.Slots {
			bind[k] = v
		}
		bind["feed.version"] = plugin.Version
		r.Bind = bind
	}
	s.mu.Lock()
	l, ok := s.looks[r.LookID]
	s.mu.Unlock()
	if (isPlugin || r.AssetDigest != "") && r.LookID == "" {
		l = familyLook("clean", "Clean")
		ok = true
	}

	var selected StyleScene
	var d scene.Document
	f := ""
	if strings.HasPrefix(r.Moment, "screen.") {
		// The editor's Card and Be right back previews use the style's
		// off-air and break scenes. Playout must select those same scenes,
		// without depending on a rendered movie being ready.
		role := ""
		if r.Bind["screen.generic"] != "1" {
			switch r.Moment {
			case "screen.card":
				role = "offair"
			case "screen.brb":
				role = "break"
			}
		}
		styled := false
		if ok && role != "" {
			pick := r
			pick.Moment = role
			selected, styled = pickScene(l, pick)
		}
		if styled {
			d = selected.Document
		} else {
			f = "signal"
			var err error
			d, err = families.Get("signal", r.Moment)
			if err != nil {
				return renderJob{}, false
			}
		}
		ok = true
	} else if isPlugin {
		d = plugin.Document
	} else if r.AssetDigest != "" {
		// A channel bumper is independent of whether this style has the role.
		d = scene.Document{V: 1, Moment: r.Moment, Duration: 8, Poster: 0}
		if role, found := l.Roles[r.Moment]; found {
			d.Duration = role.Duration
		}
	} else if r.Moment == "standby" && ok {
		selected, ok = pickScene(l, r)
		if ok {
			d = selected.Document
		} else {
			for _, fallback := range []string{"break", "ident"} {
				pick := r
				pick.Moment = fallback
				if sc, found := pickScene(l, pick); found {
					if base, valid := scene.StandbyBase(sc.Document); valid {
						selected = sc
						selected.ID = sc.ID + "-standby"
						selected.Role = "standby"
						selected.Sounds = nil
						d = base
						ok = true
						break
					}
				}
			}
		}
		if !ok {
			return renderJob{}, false
		}
	} else {
		selected, ok = pickScene(l, r)
		if !ok {
			// A channel can enable "Upcoming shows" or "Up next" independent
			// of whether its look actually has that scene (e.g. a custom
			// look built before the role existed): fall back through the
			// same chain standby uses, rather than dropping the title card.
			for _, fallback := range momentFallbacks[r.Moment] {
				pick := r
				pick.Moment = fallback
				if sc, found := pickScene(l, pick); found {
					selected, ok = sc, true
					break
				}
			}
		}
		if !ok && r.AssetDigest == "" {
			return renderJob{}, false
		}
		d = selected.Document
	}
	if !ok {
		return renderJob{}, false
	}
	// Plans mutate their own document (durations, bug layout, bindings). Copy it
	// into a FRESH value: decoding into d would reuse the stored style's slice
	// backing arrays and write into shared scenes while other plans hash them.
	var own scene.Document
	raw, _ := json.Marshal(d)
	_ = json.Unmarshal(raw, &own)
	d = own
	if assetLoop(r.Moment) && r.AssetDigest == "" && (selected.Video == nil || selected.Video.Mode != "replace") {
		d = scene.ShortLoop(d)
	}
	assetDigest := ""
	if selected.Video != nil && selected.Video.Mode == "replace" {
		assetDigest = selected.Video.AssetDigest
	}
	if r.AssetDigest != "" {
		assetDigest = r.AssetDigest
		if r.AssetDuration > 0 {
			d.Duration = r.AssetDuration.Seconds()
		}
	}
	b := scene.Bindings{Tokens: map[string]any{}, Slots: map[string]string{}}
	if f == "signal" {
		b = families.Defaults("signal")
		for k := range b.Slots {
			b.Slots[k] = ""
		}
	}
	if f != "signal" && l.Name != "" {
		b.Slots["channel.name"] = l.Name
		if l.Brand.Tagline != "" {
			b.Slots["offair.message"] = l.Brand.Tagline
		}
	}
	for k, v := range r.Bind {
		if k != "studio.scene" && k != "slate.start" {
			b.Slots[k] = v
		}
	}
	if f != "signal" {
		b.Tokens["bg"] = l.Brand.Palette.Background
		b.Tokens["accent"] = l.Brand.Palette.Accent
		b.Tokens["text"] = l.Brand.Palette.Text
		b.Tokens["glow"] = l.Brand.Palette.Glow
		b.Tokens["type.display"] = normalizeFont(l.Brand.Type.Display)
		b.Tokens["type.body"] = normalizeFont(l.Brand.Type.Body)
		b.Tokens["type.mono"] = normalizeFont(l.Brand.Type.Body)
		b.Tokens["energy"] = l.Brand.Energy
		b.Tokens["words.tagline"] = l.Brand.Tagline
		b.Tokens["brand.logo"] = l.Brand.Logo
		b.Slots["brand.logo"] = l.Brand.Logo
		for _, v := range []string{"grain", "flicker", "scanlines", "vignette", "chroma"} {
			b.Tokens["textures."+v] = 0.0
		}
		for _, v := range l.Brand.Textures {
			b.Tokens["textures."+strings.ToLower(v)] = .07
		}
		for _, c := range l.Controls {
			b.Tokens["control."+c.ID] = c.Value
		}
	}
	if f == "signal" {
		switch r.Bind["screen.reason"] {
		case "busy":
			b.Slots["screen.line1"] = "Waiting for a free connection"
		case "offair":
			b.Slots["screen.line1"] = "Off air"
			if r.Bind["screen.back"] != "" {
				b.Slots["screen.line1"] = "Back at " + r.Bind["screen.back"]
			}
		default:
			b.Slots["screen.line1"] = "Reconnecting…"
		}
		if r.Bind["screen.stationId"] == "1" {
			b.Slots["station.id"] = "ETHER"
		}
	}
	if v := r.Bind["channel.accent"]; v != "" {
		b.Tokens["accent"] = v
	}
	if v := r.Bind["channel.tagline"]; v != "" {
		b.Tokens["words.tagline"] = v
	}

	cacheRequest := r
	cacheRequest.Bind = map[string]string{}
	for k, v := range r.Bind {
		if k != "studio.scene" && k != "slate.start" {
			cacheRequest.Bind[k] = v
		}
	}
	h := encode.Hash(raster.Version+":"+s.resources, d, struct {
		Brand Brand
		Scene StyleScene
		Asset string
	}{l.Brand, selected, assetDigest}, struct {
		Bindings scene.Bindings
		Request  core.ClipRequest
	}{b, cacheRequest}, r.Art, encode.Profile)
	return renderJob{Boundary: r.Boundary, Scene: selected, AssetDigest: assetDigest, Hash: h, Look: l.ID, Doc: d, Bind: raster.Bindings{Bindings: b, Assets: plugin.Images}, Request: r}, h != ""
}

// plan resolves the durable content-addressed master job for r:
//   - Static look moments (ident, break, back, offair, standby): 1 master per look.
//   - Split live-slot moments (upnext, tonight, welcome, etc.): 1 slot-free base
//     master per look; slot foreground layers composite live at air time.
//   - Unsplittable live-slot moments: never cached to disk (returns ok=false so
//     they render live per airing without growing the master cache).
func (s *Service) plan(r core.ClipRequest) (renderJob, bool) {
	j, ok := s.planRaw(r)
	if !ok {
		return renderJob{}, false
	}
	isPlugin := strings.HasPrefix(r.Moment, "plugin:")
	isScreen := strings.HasPrefix(r.Moment, "screen.")
	if isPlugin || isScreen || r.AssetDigest != "" || r.Moment == "bug" || r.Moment == "lowerthird" {
		return j, true
	}

	if scene.HasLiveSlots(j.Doc) {
		baseDoc, _, splittable := scene.SplitSlotLayers(j.Doc)
		if !splittable {
			return renderJob{}, false
		}
		j.Doc = baseDoc
	}

	s.mu.Lock()
	l := s.looks[j.Look]
	s.mu.Unlock()
	if l.ID == "" && j.Look == "" {
		l = familyLook("clean", "Clean")
	}

	// Normalize cache identity strictly to the look + selected scene + look brand,
	// excluding ChannelID, channel.name overrides, and per-programme slots/artwork.
	staticSlots := map[string]string{
		"brand.logo":   l.Brand.Logo,
		"channel.name": l.Name,
	}
	if l.Brand.Tagline != "" {
		staticSlots["offair.message"] = l.Brand.Tagline
	}
	j.Bind.Slots = staticSlots

	cacheReq := core.ClipRequest{
		LookID: j.Look,
		Moment: r.Moment,
	}
	if accent := r.Bind["channel.accent"]; accent != "" {
		cacheReq.Bind = map[string]string{"channel.accent": accent}
	}
	if tagline := r.Bind["channel.tagline"]; tagline != "" {
		if cacheReq.Bind == nil {
			cacheReq.Bind = map[string]string{}
		}
		cacheReq.Bind["channel.tagline"] = tagline
	}

	// Clear per-programme Art from j.Request copy used when executing base master
	// while keeping ChannelID and Boundary on j.Request for scheduling/cancel.
	execReq := r
	execReq.Art = nil
	j.Request = execReq

	j.Hash = encode.Hash(raster.Version+":"+s.resources, j.Doc, struct {
		Brand Brand
		Scene StyleScene
		Asset string
	}{l.Brand, j.Scene, j.AssetDigest}, struct {
		Bindings scene.Bindings
		Request  core.ClipRequest
	}{j.Bind.Bindings, cacheReq}, map[string]string(nil), encode.Profile)
	return j, j.Hash != ""
}
func (s *Service) Clip(_ context.Context, r core.ClipRequest) (core.ClipRef, bool) {
	if r.ReelClipID != "" {
		if path, dur, ok := s.PeekCachedReelClip(r.ReelID, r.ReelClipID); ok {
			return core.ClipRef{Hash: r.ReelClipID, Duration: dur, Path: path}, true
		}
		return core.ClipRef{}, false
	}
	j, ok := s.plan(r)
	if !ok {
		return core.ClipRef{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cache[j.Hash]
	if ok {
		v.Used = time.Now()
		v.Pin = time.Now().Add(3 * time.Hour)
		s.cache[j.Hash] = v
	}
	return v.Ref, ok
}

// RenderLead reflects measured 1080p master work, including slot contention.
// Every channel decodes these same masters; lower-resolution outputs inherit
// the conservative native-resolution estimate. Cached repeats need no work.
func (s *Service) RenderLead(look, moment string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	estimate := time.Duration(0)
	if moment != "" {
		estimate = s.renderTimes[look+"/"+moment]
	} else {
		for key, duration := range s.renderTimes {
			if look == "" || strings.HasPrefix(key, look+"/") {
				estimate = max(estimate, duration)
			}
		}
	}
	return max(3*time.Minute, 4*estimate)
}

func (s *Service) Ensure(_ context.Context, r core.ClipRequest) {
	s.WakeUpcoming()
	if r.ReelClipID != "" {
		s.ensureReelClip(r)
		return
	}
	s.mu.Lock()
	l := s.looks[r.LookID]
	s.mu.Unlock()
	variants := []core.ClipRequest{r}
	if r.AssetDigest == "" && !strings.HasPrefix(r.Moment, "screen.") && !strings.HasPrefix(r.Moment, "plugin:") && r.Moment != "standby" {
		variants = nil
		for _, sc := range l.Scenes {
			if sc.Enabled && sc.Role == r.Moment {
				v := r
				v.Bind = map[string]string{}
				for k, x := range r.Bind {
					v.Bind[k] = x
				}
				v.Bind["studio.scene"] = sc.ID
				variants = append(variants, v)
			}
		}
	}
	for _, v := range variants {
		if j, ok := s.plan(v); ok {
			s.mu.Lock()
			s.pins[j.Hash] = time.Now().Add(3 * time.Hour)
			s.mu.Unlock()
			s.enqueue(j)
		}
	}
}

// CancelChannel stops every queued and running render for a channel and
// releases its pins: the channel was disabled, deleted, or fell out of
// the requested set. The running render's context is cancelled, which
// also kills its ffmpeg. Work already in the job channels is dropped by
// process under the unwanted mark; a later request for the channel
// clears the mark in enqueue.
func (s *Service) CancelChannel(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	s.mu.Lock()
	if s.unwanted == nil {
		s.unwanted = map[string]bool{}
	}
	s.unwanted[channelID] = true
	kept := s.liveJobs[:0]
	for _, j := range s.liveJobs {
		if j.Request.ChannelID == channelID {
			delete(s.pending, j.Hash)
			delete(s.pins, j.Hash)
			delete(s.hashChannel, j.Hash)
			continue
		}
		kept = append(kept, j)
	}
	for i := len(kept); i < len(s.liveJobs); i++ {
		s.liveJobs[i] = renderJob{}
	}
	s.liveJobs = kept
	for h, ch := range s.hashChannel {
		if ch == channelID {
			delete(s.pins, h)
			delete(s.hashChannel, h)
			if v, ok := s.cache[h]; ok {
				v.Pin = time.Time{}
				s.cache[h] = v
			}
		}
	}
	linkCancels := s.cancelChannelLinksLocked(channelID)
	cancel := s.activeCancel != nil && s.activeChannel == channelID
	cancelFn := s.activeCancel
	s.mu.Unlock()
	if cancel {
		cancelFn()
	}
	for _, fn := range linkCancels {
		fn()
	}
}

func (s *Service) enqueue(j renderJob) bool {
	if s.o.Enabled != nil && !s.o.Enabled() {
		return false
	}
	imminent := !j.download && s.imminent(j)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := j.Request.ChannelID; ch != "" {
		delete(s.unwanted, ch)
		if s.hashChannel == nil {
			s.hashChannel = map[string]string{}
		}
		if j.scene() {
			s.hashChannel[j.Hash] = ch
		}
	}
	if s.liveSaving[j.Hash] {
		return true
	}
	if s.pending[j.Hash] {
		if imminent {
			return s.queueLiveLocked(j)
		}
		return true
	}
	if v, ok := s.cache[j.Hash]; ok {
		v.Pin = s.pins[j.Hash]
		s.cache[j.Hash] = v
		return true
	}
	if f, ok := s.failed[j.Hash]; ok && j.scene() && time.Now().Before(f.retry) {
		return true // backing off; the boundary falls back as it would on a miss
	}
	select {
	case <-s.ctx.Done():
		return false
	default:
	}
	if imminent {
		if !s.queueLiveLocked(j) {
			return false
		}
		s.pending[j.Hash] = true
		return true
	}
	queue := s.jobs
	if j.download {
		queue = s.importJobs
	} else if j.convert != nil {
		queue = s.clipJobs
	}
	select {
	case queue <- j:
		s.pending[j.Hash] = true
		return true
	default:
		return false
	}
}

// refreshLookHashes updates a look's content-addressed cache progress snapshot
// without queueing any background renders. Rendering happens strictly on demand
// when an active viewer watches a channel using the look.
func (s *Service) refreshLookHashes(l Look) {
	hashes := []string{}
	seen := map[string]bool{}
	for _, sc := range l.Scenes {
		if !sc.Enabled || sc.Role == "bug" || sc.Role == "lowerthird" {
			continue
		}
		r := core.ClipRequest{
			LookID: l.ID,
			Moment: sc.Role,
			Bind:   map[string]string{"studio.scene": sc.ID, "channel.name": l.Name},
		}
		if j, ok := s.plan(r); ok && !seen[j.Hash] {
			seen[j.Hash] = true
			hashes = append(hashes, j.Hash)
		}
	}
	s.mu.Lock()
	s.lookJobs[l.ID] = hashes
	s.refreshProgress(l.ID)
	s.mu.Unlock()
}

// refreshProgress is called under mu and counts only the current saved revision.
func (s *Service) refreshProgress(id string) {
	p := &Progress{Total: len(s.lookJobs[id])}
	for _, h := range s.lookJobs[id] {
		if _, ok := s.cache[h]; ok {
			p.Done++
		}
	}
	s.progress[id] = p
}

func (s *Service) imminent(j renderJob) bool {
	now := s.now()
	lead := s.RenderLead(j.Look, j.Doc.Moment)
	if strings.HasPrefix(j.Doc.Moment, "plugin:") {
		lead = max(lead, 15*time.Minute)
	}
	return j.Boundary.After(now) && !j.Boundary.After(now.Add(lead))
}

// Preparation order accounts for measured render cost. A long plugin master
// must start before short cards with later preparation deadlines, even when
// their airing boundaries are earlier. Within equal cost, boundaries order it.
func (s *Service) preparationAtLocked(j renderJob) time.Time {
	cost := s.renderTimes[j.Look+"/"+j.Doc.Moment]
	if strings.HasPrefix(j.Doc.Moment, "plugin:") {
		cost = max(cost, 30*time.Second)
	}
	return j.Boundary.Add(-2 * cost)
}

func (s *Service) liveJobBeforeLocked(a, b renderJob) bool {
	pa, pb := s.preparationAtLocked(a), s.preparationAtLocked(b)
	if pa.Equal(pb) {
		return a.Boundary.Before(b.Boundary)
	}
	return pa.Before(pb)
}

// Called under mu. Only ordinary work waiting for a background slot is preemptible.
func (s *Service) queueLiveLocked(j renderJob) bool {
	if s.activeHash == j.Hash && !s.activeBoundary.IsZero() {
		return true
	}

	for i, old := range s.liveJobs {
		if old.Hash == j.Hash {
			if !old.Boundary.After(time.Now()) || j.Boundary.Before(old.Boundary) {
				s.liveJobs[i] = j
				sort.SliceStable(s.liveJobs, func(i, j int) bool {
					return s.liveJobBeforeLocked(s.liveJobs[i], s.liveJobs[j])
				})
			}
			return true
		}
	}
	if len(s.liveJobs) >= 128 {
		if !s.liveJobBeforeLocked(j, s.liveJobs[len(s.liveJobs)-1]) {
			return false
		}
		delete(s.pending, s.liveJobs[len(s.liveJobs)-1].Hash)
		s.liveJobs = s.liveJobs[:len(s.liveJobs)-1]
	}
	s.liveJobs = append(s.liveJobs, j)
	sort.SliceStable(s.liveJobs, func(i, j int) bool {
		return s.liveJobBeforeLocked(s.liveJobs[i], s.liveJobs[j])
	})
	if s.activeCancel != nil && s.activeBoundary.IsZero() {
		s.activeCancel()
	}
	select {
	case s.liveReady <- struct{}{}:
	default:
	}
	return true
}

func (s *Service) watchUpcoming() {
	defer s.wg.Done()
	var t *time.Timer
	var timerCh <-chan time.Time
	arm := func(d time.Duration) {
		if t == nil {
			t = time.NewTimer(d)
		} else {
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(d)
		}
		timerCh = t.C
	}
	disarm := func() {
		if t != nil {
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		timerCh = nil
	}
	defer disarm()
	for {
		// On a standalone server, s.looks is loaded in New() and updated by
		// SaveLook/DeleteLook; only clustered servers need a database reload.
		if s.o.Members != nil {
			if ls, e := s.Looks(s.ctx); e == nil {
				snapshot := map[string]Look{}
				for _, l := range ls {
					snapshot[l.ID] = l
				}
				s.mu.Lock()
				s.looks = snapshot
				s.evict()
				s.evictLinksLocked("")
				s.mu.Unlock()
			}
		} else {
			s.mu.Lock()
			if len(s.pins) > 0 {
				s.evict()
			}
			s.evictLinksLocked("")
			s.mu.Unlock()
		}
		wanted := map[string]bool{}
		if s.o.Upcoming != nil {
			requests := s.o.Upcoming(s.ctx)
			for _, r := range requests {
				if r.ChannelID != "" {
					wanted[r.ChannelID] = true
				}
				if r.ReelClipID != "" {
					s.ensureReelClip(r)
					continue
				}
				j, ok := s.plan(r)
				if !ok {
					continue
				}
				s.mu.Lock()
				v := s.cache[j.Hash]
				s.pins[j.Hash] = time.Now().Add(3 * time.Hour)
				if s.hashChannel == nil {
					s.hashChannel = map[string]string{}
				}
				s.hashChannel[j.Hash] = r.ChannelID
				v.Pin = s.pins[j.Hash]
				if v.Ref.Hash != "" {
					s.cache[j.Hash] = v
				}
				s.mu.Unlock()
				s.enqueue(j)
			}
			// A channel that fell out of the request set (disabled,
			// deleted, or unwatched past its window) stops rendering:
			// queued and running work is cancelled, pins released.
			s.mu.Lock()
			var dropped []string
			for ch := range s.wantedChannels {
				if !wanted[ch] {
					dropped = append(dropped, ch)
				}
			}
			s.wantedChannels = wanted
			s.mu.Unlock()
			for _, ch := range dropped {
				s.CancelChannel(ch)
			}
		}
		s.mu.Lock()
		active := len(wanted) > 0 || s.o.Members != nil
		s.mu.Unlock()
		if active {
			arm(30 * time.Second)
		} else {
			disarm()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-timerCh:
		case <-s.upcomingWake:
		}
	}
}

func (s *Service) run() {
	defer s.wg.Done()
	clipsSinceRender := 0
	for {
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		var urgent *renderJob
		if len(s.liveJobs) > 0 {
			j := s.liveJobs[0]
			s.liveJobs[0] = renderJob{}
			s.liveJobs = s.liveJobs[1:]
			urgent = &j
		}
		s.mu.Unlock()
		if urgent != nil {
			s.process(*urgent)
			continue
		}
		if clipsSinceRender >= 3 {
			select {
			case j := <-s.jobs:
				s.process(j)
				clipsSinceRender = 0
				continue
			default:
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.liveReady:
		case j := <-s.clipJobs:
			clipsSinceRender++
			s.process(j)
		case j := <-s.jobs:
			clipsSinceRender = 0
			s.process(j)
		}
	}
}

func (s *Service) unwantedChannel(j renderJob) bool {
	ch := j.Request.ChannelID
	if ch == "" || !j.scene() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unwanted[ch]
}

func (s *Service) process(j renderJob) {
	// A cancelled channel's queued work leaves the lane without rendering.
	if s.unwantedChannel(j) {
		s.mu.Lock()
		delete(s.pending, j.Hash)
		delete(s.pins, j.Hash)
		delete(s.hashChannel, j.Hash)
		s.mu.Unlock()
		return
	}
	// Obsolete airings cannot consume a lane ahead of future work. A repeated
	// content hash will be admitted again with its next scheduled boundary.
	if !j.Boundary.IsZero() && !j.Boundary.After(s.now()) {
		s.mu.Lock()
		delete(s.pending, j.Hash)
		s.mu.Unlock()
		return
	}
	imminent := s.imminent(j)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.mu.Lock()
	if _, cached := s.cache[j.Hash]; cached || s.liveSaving[j.Hash] {
		delete(s.pending, j.Hash)
		s.mu.Unlock()
		return
	}
	if !j.download {
		s.activeCancel = cancel
		s.activeHash = j.Hash
		s.activeChannel = j.Request.ChannelID
		s.activeBoundary = time.Time{}
		if imminent {
			s.activeBoundary = j.Boundary
			ctx = WithBoundaryDeadline(ctx, j.Boundary)
		} else if len(s.liveJobs) > 0 {
			cancel()
		}
	} else if imminent {
		ctx = WithBoundaryDeadline(ctx, j.Boundary)
	}
	s.mu.Unlock()
	e := s.execute(ctx, j)
	s.mu.Lock()
	if !j.download {
		s.activeCancel = nil
		s.activeHash = ""
		s.activeChannel = ""
		s.activeBoundary = time.Time{}
	}
	delete(s.pending, j.Hash)
	s.refreshProgress(j.Look)
	s.mu.Unlock()
	// A preempted render re-queues; a channel-cancelled one does not.
	if errors.Is(e, context.Canceled) && s.ctx.Err() == nil && !j.download && !s.unwantedChannel(j) {
		s.enqueue(j)
	}
	if e != nil && s.ctx.Err() == nil && !errors.Is(e, context.Canceled) && j.scene() {
		s.mu.Lock()
		s.vis.MastersFailed++
		if s.failed == nil {
			s.failed = map[string]failure{}
		}
		if len(s.failed) > 1024 {
			for k, f := range s.failed {
				if time.Since(f.retry) > 10*time.Minute {
					delete(s.failed, k)
				}
			}
		}
		f := s.failed[j.Hash]
		f.delay = min(max(2*f.delay, 5*time.Second), 5*time.Minute)
		if errors.Is(e, context.DeadlineExceeded) {
			f.delay = 24 * time.Hour
		}
		f.retry = time.Now().Add(f.delay)
		s.failed[j.Hash] = f
		first := f.delay == 5*time.Second
		s.mu.Unlock()
		if first {
			s.o.Logger.Warn("onair job failed", "hash", j.Hash, "error", e)
		} else {
			s.o.Logger.Debug("onair job failed again", "hash", j.Hash, "error", e, "retry_in", f.delay)
		}
	} else if e != nil && s.ctx.Err() == nil && !errors.Is(e, context.Canceled) {
		s.o.Logger.Warn("onair job failed", "hash", j.Hash, "error", e)
	} else if e == nil {
		s.mu.Lock()
		delete(s.failed, j.Hash)
		s.mu.Unlock()
	}
}

// scene reports whether j renders a scene master, as opposed to a planning,
// import or reel conversion job; only scene renders are retried by boundaries.
func (j renderJob) scene() bool { return j.prepare == nil && j.convert == nil && !j.download }

// failure is a render hash's retry backoff after an error.
type failure struct {
	retry time.Time
	delay time.Duration
}

func (s *Service) execute(parent context.Context, j renderJob) (err error) {
	if j.prepare != nil {
		return j.prepare(parent)
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	queued := time.Now()
	if s.o.Acquire != nil {
		jobCtx, release, e := s.o.Acquire(ctx)
		if e != nil {
			return e
		}
		defer release()
		ctx = jobCtx
	}
	if j.scene() && j.AssetDigest == "" {
		// Waiting for a playback slot costs no rendering budget. Once acquired,
		// procedural masters have a bounded render budget independent of their
		// on-air boundary. A slower title may complete for the next tune.
		var stopRender context.CancelFunc
		ctx, stopRender = context.WithTimeout(ctx, 30*time.Second)
		defer stopRender()
	}
	started := time.Now()
	queueWait := started.Sub(queued)
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if j.convert != nil {
		return j.convert(ctx)
	}
	var completed *entry
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		elapsed := time.Since(started)
		speedX := 0.0
		if elapsed > 0 && j.Doc.Duration > 0 {
			speedX = j.Doc.Duration / elapsed.Seconds()
		}
		s.mu.Lock()
		key := j.Look + "/" + j.Doc.Moment
		if s.renderTimes == nil {
			s.renderTimes = map[string]time.Duration{}
		}
		previous := s.renderTimes[key]
		s.renderTimes[key] = max(elapsed, (previous*3+elapsed)/4)
		if err == nil {
			s.vis.MastersRendered++
			if speedX > 0 {
				s.vis.LastSpeedX = speedX
			}
		}
		if err == nil && completed != nil {
			completed.Pin = s.pins[j.Hash]
			s.cache[j.Hash] = *completed
			s.evict()
		}
		masters := s.vis.MastersRendered
		s.mu.Unlock()
		if err == nil {
			s.o.Logger.Info("onair master rendered", "look", j.Look, "moment", j.Doc.Moment, "duration", elapsed.Round(time.Millisecond), "render_speed_x", round2(speedX), "queue_wait", queueWait, "masters_rendered", masters)
		} else {
			s.o.Logger.Debug("onair master render ended", "look", j.Look, "moment", j.Doc.Moment, "duration", elapsed, "success", false, "error", err)
		}
	}()
	if e := s.loadAssets(ctx, &j); e != nil {
		return e
	}
	out := filepath.Join(s.o.Dir, j.Hash+".mp4")
	var e error
	audio := ""

	var sounds []encode.SoundInput
	// Scene media is read in place from the immutable asset file.
	prepare := func(digest, kind string) (string, error) {
		var stored string
		if err := s.db.QueryRowContext(ctx, "SELECT kind FROM onair_assets WHERE digest=?", digest).Scan(&stored); err != nil {
			return "", err
		}
		if stored != kind {
			return "", sql.ErrNoRows
		}
		return s.assetPath(ctx, digest)
	}
	for _, sound := range j.Scene.Sounds {
		path, err := prepare(sound.AssetDigest, "audio")
		if err != nil {
			return err
		}
		sounds = append(sounds, encode.SoundInput{Path: path, At: sound.At, Start: sound.Start, End: sound.End, Volume: sound.Volume, FadeIn: sound.FadeIn, FadeOut: sound.FadeOut})
	}
	var background *encode.VideoInput
	if v := j.Scene.Video; v != nil && v.Mode == "background" && j.Request.AssetDigest == "" {
		path, err := prepare(v.AssetDigest, "video")
		if err != nil {
			return err
		}
		background = &encode.VideoInput{Path: path, Fit: v.Fit, Start: v.Start}
	}
	if j.AssetDigest != "" {
		e = s.renderMomentVideo(ctx, j, out, sounds)
	} else if s.o.Render != nil {
		e = s.o.Render(ctx, j.Doc, j.Bind, out)
	} else {
		if s.o.FFmpeg == nil {
			return fmt.Errorf("ffmpeg unavailable")
		}
		opts := encode.Options{Workers: 1, Width: 640, Height: 360, Audio: j.Doc.Audio, Sounds: sounds, Background: background}
		renderFn := s.o.RenderFrame
		if renderFn == nil {
			renderFn = raster.Render
		}
		if s.o.EncoderOptions != nil {
			opts.InputArgs, opts.VideoArgs = s.o.EncoderOptions(opts.Width, opts.Height)
		}
		seq := raster.NewSequence(j.Doc, j.Bind, opts.Width, opts.Height)
		frames := func(i int) *image.RGBA {
			if s.o.RenderFrame != nil {
				return renderFn(j.Doc, j.Bind, float64(i)/30, opts.Width, opts.Height)
			}
			t := float64(i) / 30
			if assetLoop(j.Doc.Moment) {
				t = min(t, j.Doc.Duration-t)
			}
			return seq.Frame(t)
		}
		if j.Doc.Moment == "standby" {
			// A hold is needed immediately. Bound its raster work to one settled
			// background; the live Signal engine supplies motion on cold tunes.
			// Rendering the entire procedural loop can outlive the viewer.
			still := seq.Frame(max(1, j.Doc.Poster))
			frames = func(int) *image.RGBA { return still }
		}
		if strings.HasPrefix(j.Doc.Moment, "plugin:") {
			opts.MaxSeconds = 600
			// Long static data cards need one rasterization, not thousands.
			if staticPluginScene(j.Doc) {
				still := frames(0)
				frames = func(int) *image.RGBA { return still }
			}
		}
		e = encode.WithOptions(ctx, s.o.FFmpeg(), frames, int(j.Doc.Duration*30), 30, audio, out, opts)
		if e != nil && len(opts.VideoArgs) > 0 && ctx.Err() == nil {
			opts.InputArgs = nil
			opts.VideoArgs = nil
			e = encode.WithOptions(ctx, s.o.FFmpeg(), frames, int(j.Doc.Duration*30), 30, audio, out, opts)
		}
	}
	if e != nil {
		return e
	}
	st, e := os.Stat(out)
	if e != nil {
		return e
	}
	meta, _ := json.Marshal(j.Doc.Duration)
	if err := os.WriteFile(filepath.Join(s.o.Dir, j.Hash+".json"), meta, 0640); err != nil {
		return err
	}
	// Publish durability and its measured speed together, after the sidecar
	// is complete, so Clip cannot expose a partially committed master.
	completed = &entry{Ref: core.ClipRef{Hash: j.Hash, Duration: time.Duration(j.Doc.Duration * float64(time.Second)), Path: out}, Size: st.Size(), Used: time.Now()}
	return nil
}
func (s *Service) loadCache() {
	es, _ := os.ReadDir(s.o.Dir)
	for _, v := range es {
		if !strings.HasSuffix(v.Name(), ".mp4") {
			continue
		}
		h := strings.TrimSuffix(v.Name(), ".mp4")
		if len(h) != 64 {
			continue
		}
		b, e := os.ReadFile(filepath.Join(s.o.Dir, h+".json"))
		var d float64
		if e != nil || json.Unmarshal(b, &d) != nil {
			continue
		}
		st, e := v.Info()
		if e != nil {
			continue
		}
		s.mu.Lock()
		s.cache[h] = entry{Ref: core.ClipRef{Hash: h, Duration: time.Duration(d * float64(time.Second)), Path: filepath.Join(s.o.Dir, v.Name())}, Size: st.Size(), Used: st.ModTime()}
		s.mu.Unlock()
	}
}
func (s *Service) evict() {
	for h, until := range s.pins {
		if until.Before(time.Now()) {
			delete(s.pins, h)
			delete(s.hashChannel, h)
		}
	}
	var total int64
	keys := []string{}
	for k, v := range s.cache {
		total += v.Size
		if v.Pin.Before(time.Now()) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return s.cache[keys[i]].Used.Before(s.cache[keys[j]].Used) })
	for _, k := range keys {
		if total <= s.o.CacheBytes {
			break
		}
		v := s.cache[k]
		if os.Remove(v.Ref.Path) == nil {
			os.Remove(filepath.Join(s.o.Dir, k+".json"))
			delete(s.cache, k)
			total -= v.Size
		}
	}
}

func (s *Service) loadAssets(ctx context.Context, j *renderJob) error {
	assets := map[string]image.Image{}
	for k, v := range j.Bind.Assets {
		assets[k] = v
	}
	j.Bind.Assets = assets
	j.Bind.Fonts = map[string]*opentype.Font{}
	refs := map[string]string{}
	for k, d := range j.Request.Art {
		refs[k] = d
	}
	collectSceneAssets(j.Doc.Layers, j.Bind.Bindings, refs)
	for _, key := range []string{"type.display", "type.mono", "type.body", "logo", "brand.logo"} {
		d, _ := j.Bind.Tokens[key].(string)
		if len(d) == 64 {
			refs[d] = d
		}
	}
	for slot, d := range refs {
		if _, ok := j.Bind.Assets[d]; ok {
			continue
		}
		if im := showcaseBugImage(d); im != nil {
			j.Bind.Assets[d] = im
			j.Bind.Slots[slot] = d
			continue
		}
		if len(d) != 64 {
			if d != "" && s.o.Artwork != nil {
				im, e := s.o.Artwork(ctx, d)
				if e != nil {
					// Programme artwork is decoration: a poster or backdrop
					// that cannot load leaves its slot empty instead of
					// failing the whole scene into the generic fallback.
					if ctx.Err() != nil {
						return ctx.Err()
					}
					s.o.Logger.Debug("onair artwork unavailable", "slot", slot, "moment", j.Request.Moment, "error", e)
					continue
				}
				j.Bind.Assets[d] = im
				j.Bind.Slots[slot] = d
			}
			continue
		}
		var kind, mime string
		if e := s.db.QueryRowContext(ctx, "SELECT kind,mime FROM onair_assets WHERE digest=?", d).Scan(&kind, &mime); e != nil {
			return e
		}
		b, e := s.assetBytes(ctx, d)
		if e != nil {
			return e
		}
		if kind == "font" {
			f, e := opentype.Parse(b)
			if e != nil {
				return e
			}
			j.Bind.Fonts[d] = f
			continue
		}
		if kind == "image" {
			if mime == "image/svg+xml" {
				v, _, e := artwork.NewSVGRasterizer().Rasterize(ctx, d, b, artwork.RasterOptions{})
				if e != nil {
					return e
				}
				b = v
			}
			im, _, e := image.Decode(bytes.NewReader(b))
			if e != nil {
				return e
			}
			j.Bind.Assets[d] = im
			j.Bind.Slots[slot] = d
		}
	}
	return nil
}

// SegmentInfo supplies immutable policy metadata without accessing the database.
func (s *Service) SegmentInfo(moment string) (name, rating string, ok bool) {
	if s.o.PluginSegment == nil {
		return "", "", false
	}
	v, ok := s.o.PluginSegment(moment)
	return v.Name, v.Rating, ok
}

// Imports are network-bound. Two bounded workers isolate them from raster work;
// downloaded clips rejoin clipJobs for the existing conversion priority.
func (s *Service) runImports() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case j := <-s.importJobs:
			s.process(j)
		}

	}
}

// StyleScreens reads the cached owner style; it performs no rendering or database work.
func (s *Service) StyleScreens(id string) *config.LiveTVScreens {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.looks[id].Screens; v != nil {
		cp := *v
		return &cp
	}
	return nil
}

// momentFallbacks lets a channel enable a moment (e.g. "Upcoming shows" or
// "Up next") independent of whether the active look actually defines that
// role's scene, degrading to the next best title card instead of dropping it.
var momentFallbacks = map[string][]string{
	"tonight": {"upnext", "ident"},
	"upnext":  {"ident"},
	"back":    {"ident"},
}

func pickScene(l Look, r core.ClipRequest) (StyleScene, bool) {
	var choices []StyleScene
	total := 0
	for _, sc := range l.Scenes {
		if sc.Enabled && sc.Role == r.Moment && sc.Weight > 0 {
			if r.Bind["studio.scene"] == sc.ID {
				return sc, true
			}
			choices = append(choices, sc)
			total += sc.Weight
		}
	}
	if total == 0 {
		return StyleScene{}, false
	}
	sum := sha256.Sum256([]byte(l.ID + "\x00" + r.ChannelID + "\x00" + r.Bind["slate.start"]))
	n := int(binary.BigEndian.Uint64(sum[:8]) % uint64(total))
	for _, sc := range choices {
		n -= sc.Weight
		if n < 0 {
			return sc, true
		}
	}
	return StyleScene{}, false
}

func assetLoop(moment string) bool {
	return moment == "standby" || moment == "fallback" || moment == "offair"
}
