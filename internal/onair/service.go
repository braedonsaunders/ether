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
	"github.com/braedonsaunders/ether/internal/onair/encode"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/fonts"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"golang.org/x/image/font/opentype"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	// Changed runs after an owner style save has committed and locks are released.
	Changed        func(context.Context) error
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
	Upcoming    func(context.Context) []core.ClipRequest
	Logger      *slog.Logger
	CacheBytes  int64
	WorkingSize func() (int, int)
	Artwork     func(context.Context, string) (image.Image, error)
	// Render is a seam for verifying that Ensure never executes work inline.
	Render func(context.Context, scene.Document, raster.Bindings, string) error
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
	activeCancel   context.CancelFunc
	activeBoundary time.Time
	resources      string
	reelReady      map[string]bool
	o              Options
	db             *sql.DB
	mu             sync.Mutex
	assetMu        sync.Mutex
	looks          map[string]Look
	cache          map[string]entry
	pins           map[string]time.Time
	pending        map[string]bool
	renderTimes    map[string]time.Duration
	progress       map[string]*Progress
	lookJobs       map[string][]string
	jobs           chan renderJob
	clipJobs       chan renderJob
	importJobs     chan renderJob
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	startOnce      sync.Once
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
	if e := os.MkdirAll(o.Dir, 0750); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{reelReady: map[string]bool{}, resources: fonts.Version(), o: o, db: o.DB, looks: map[string]Look{}, cache: map[string]entry{}, pending: map[string]bool{}, renderTimes: map[string]time.Duration{}, pins: map[string]time.Time{}, progress: map[string]*Progress{}, lookJobs: map[string][]string{}, liveReady: make(chan struct{}, 1), jobs: make(chan renderJob, 128), clipJobs: make(chan renderJob, 64), importJobs: make(chan renderJob, 64), ctx: ctx, cancel: cancel}

	if e := s.seed(ctx); e != nil {
		cancel()
		return nil, e
	}
	ls, e := s.Looks(ctx)
	if e != nil {
		cancel()
		return nil, e
	}
	for _, l := range ls {
		s.looks[l.ID] = l
	}
	s.loadCache()
	s.loadReels()

	s.resumeConversions()
	s.resumeImports()
	for _, l := range ls {
		s.enqueueLook(l)
	}
	return s, nil
}

// Start begins workers after application dependencies have been constructed.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		if s.ctx.Err() == nil {
			s.wg.Add(3)
			go s.run()
			go s.watchUpcoming()
			go s.backfillFacts()
			for range 2 {
				s.wg.Add(1)
				go s.runImports()
			}
		}
	})
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
func (s *Service) plan(r core.ClipRequest) (renderJob, bool) {
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
		f = "signal"
		var err error
		d, err = families.Get("signal", r.Moment)
		if err != nil {
			return renderJob{}, false
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
	} else {
		selected, ok = pickScene(l, r)
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

	// Scene selection is represented by its document and media; the internal prewarm selector is not a cache input.
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
func (s *Service) Clip(_ context.Context, r core.ClipRequest) (core.ClipRef, bool) {
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
	s.mu.Lock()
	l := s.looks[r.LookID]
	s.mu.Unlock()
	variants := []core.ClipRequest{r}
	if r.AssetDigest == "" && !strings.HasPrefix(r.Moment, "screen.") && !strings.HasPrefix(r.Moment, "plugin:") {
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
func (s *Service) enqueue(j renderJob) bool {
	imminent := s.imminent(j)
	s.mu.Lock()
	defer s.mu.Unlock()
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
func (s *Service) enqueueLook(l Look) {
	if s.o.StaticRequests != nil {
		s.enqueue(renderJob{Hash: "plan-" + l.ID + "-" + newID(), prepare: func(ctx context.Context) error { s.queueLook(l, s.o.StaticRequests(ctx, l.ID)); return nil }})
		return
	}
	s.queueLook(l, nil)
}
func (s *Service) queueLook(l Look, requests []core.ClipRequest) {
	if len(requests) == 0 {
		ids := l.ChannelIDs
		if len(ids) == 0 {
			ids = []string{""}
		}
		moments := []string{}
		for role := range l.Roles {
			moments = append(moments, role)
		}
		for _, id := range ids {
			for _, m := range moments {
				requests = append(requests, core.ClipRequest{LookID: l.ID, Moment: m, ChannelID: id, Bind: map[string]string{"channel.name": l.Name}})
			}
		}
	}

	// Render progress covers every enabled scene, including roles whose live slots
	// will be populated only when a schedule request arrives.
	channels := map[string]core.ClipRequest{}
	seen := map[string]bool{}
	for _, r := range requests {
		channels[r.ChannelID] = r
		seen[r.ChannelID+"\x00"+r.Moment] = true
	}
	for channel, base := range channels {
		for _, sc := range l.Scenes {
			key := channel + "\x00" + sc.Role
			if sc.Enabled && !seen[key] {
				r := base
				r.Moment = sc.Role
				r.AssetDigest = ""
				r.Art = nil
				requests = append(requests, r)
				seen[key] = true
			}
		}
	}
	jobs := []renderJob{}
	hashes := []string{}
	for _, r := range requests {
		for _, sc := range l.Scenes {
			if !sc.Enabled || sc.Role != r.Moment {
				continue
			}
			bind := map[string]string{}
			for k, v := range r.Bind {
				bind[k] = v
			}
			bind["studio.scene"] = sc.ID
			r.Bind = bind
			if j, ok := s.plan(r); ok {
				jobs = append(jobs, j)
				hashes = append(hashes, j.Hash)
			}
		}
	}
	s.mu.Lock()
	s.lookJobs[l.ID] = hashes
	s.refreshProgress(l.ID)
	s.mu.Unlock()
	for _, j := range jobs {
		s.enqueue(j)
	}
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
	now := time.Now()
	lead := s.RenderLead(j.Look, j.Doc.Moment)
	if strings.HasPrefix(j.Doc.Moment, "plugin:") {
		lead = max(lead, 15*time.Minute)
	}
	return j.Boundary.After(now) && !j.Boundary.After(now.Add(lead))
}

// Called under mu. Imminent jobs are bounded and selected nearest-boundary
// first. They also interrupt an ordinary job waiting for a viewer-held slot.
func (s *Service) queueLiveLocked(j renderJob) bool {
	for i, old := range s.liveJobs {
		if old.Hash == j.Hash {
			if j.Boundary.Before(old.Boundary) {
				s.liveJobs[i] = j
				sort.SliceStable(s.liveJobs, func(i, j int) bool { return s.liveJobs[i].Boundary.Before(s.liveJobs[j].Boundary) })
			}
			return true
		}
	}
	if len(s.liveJobs) >= 128 {
		if !j.Boundary.Before(s.liveJobs[len(s.liveJobs)-1].Boundary) {
			return false
		}
		delete(s.pending, s.liveJobs[len(s.liveJobs)-1].Hash)
		s.liveJobs = s.liveJobs[:len(s.liveJobs)-1]
	}
	s.liveJobs = append(s.liveJobs, j)
	sort.SliceStable(s.liveJobs, func(i, j int) bool { return s.liveJobs[i].Boundary.Before(s.liveJobs[j].Boundary) })
	if s.activeCancel != nil && (s.activeBoundary.IsZero() || !s.activeBoundary.After(time.Now())) {
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
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if ls, e := s.Looks(s.ctx); e == nil {
			snapshot := map[string]Look{}
			for _, l := range ls {
				snapshot[l.ID] = l
			}
			s.mu.Lock()
			s.looks = snapshot
			s.evict()
			s.mu.Unlock()
		}
		if s.o.Upcoming != nil {
			for _, r := range s.o.Upcoming(s.ctx) {
				j, ok := s.plan(r)
				if !ok {
					continue
				}
				s.mu.Lock()
				v := s.cache[j.Hash]
				s.pins[j.Hash] = time.Now().Add(3 * time.Hour)
				v.Pin = s.pins[j.Hash]
				if v.Ref.Hash != "" {
					s.cache[j.Hash] = v
				}
				s.mu.Unlock()
				s.enqueue(j)
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
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

func (s *Service) process(j renderJob) {
	imminent := s.imminent(j)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.mu.Lock()
	if _, cached := s.cache[j.Hash]; cached {
		delete(s.pending, j.Hash)
		s.mu.Unlock()
		return
	}
	if !j.download {
		s.activeCancel = cancel
		s.activeBoundary = time.Time{}
		if imminent {
			s.activeBoundary = j.Boundary
			ctx = WithBoundaryDeadline(ctx, j.Boundary)
		} else if len(s.liveJobs) > 0 {
			cancel()
		}
	}
	s.mu.Unlock()
	e := s.execute(ctx, j)
	s.mu.Lock()
	if !j.download {
		s.activeCancel = nil
		s.activeBoundary = time.Time{}
	}
	delete(s.pending, j.Hash)
	s.refreshProgress(j.Look)
	s.mu.Unlock()
	if errors.Is(e, context.Canceled) && s.ctx.Err() == nil {
		s.enqueue(j)
	}
	if e != nil && s.ctx.Err() == nil {
		s.o.Logger.Warn("onair job failed", "hash", j.Hash, "error", e)
	}
}

func (s *Service) execute(parent context.Context, j renderJob) (err error) {
	if j.prepare != nil {
		return j.prepare(parent)
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	started := time.Now()
	if s.o.Acquire != nil {
		jobCtx, release, e := s.o.Acquire(ctx)
		if e != nil {
			return e
		}
		defer release()
		ctx = jobCtx
	}
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if j.convert != nil {
		return j.convert(ctx)
	}
	defer func() {
		elapsed := time.Since(started)
		s.mu.Lock()
		key := j.Look + "/" + j.Doc.Moment
		if s.renderTimes == nil {
			s.renderTimes = map[string]time.Duration{}
		}
		previous := s.renderTimes[key]
		s.renderTimes[key] = max(elapsed, (previous*3+elapsed)/4)
		s.mu.Unlock()
		s.o.Logger.Debug("onair master rendered", "look", j.Look, "moment", j.Doc.Moment, "duration", elapsed, "success", err == nil)
	}()
	if e := s.loadAssets(ctx, &j); e != nil {
		return e
	}
	out := filepath.Join(s.o.Dir, j.Hash+".mp4")
	var e error
	audio := ""

	var sounds []encode.SoundInput
	prepare := func(digest, kind string) (string, error) {
		var b []byte
		if err := s.db.QueryRowContext(ctx, "SELECT content FROM onair_assets WHERE digest=? AND kind=?", digest, kind).Scan(&b); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(s.o.Dir, ".scene-media-*")
		if err != nil {
			return "", err
		}
		_, err = f.Write(b)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(f.Name())
			return "", err
		}
		return f.Name(), nil
	}
	for _, sound := range j.Scene.Sounds {
		path, err := prepare(sound.AssetDigest, "audio")
		if err != nil {
			return err
		}
		defer os.Remove(path)
		sounds = append(sounds, encode.SoundInput{Path: path, At: sound.At, Start: sound.Start, End: sound.End, Volume: sound.Volume, FadeIn: sound.FadeIn, FadeOut: sound.FadeOut})
	}
	var background *encode.VideoInput
	if v := j.Scene.Video; v != nil && v.Mode == "background" && j.Request.AssetDigest == "" {
		path, err := prepare(v.AssetDigest, "video")
		if err != nil {
			return err
		}
		defer os.Remove(path)
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
		opts := encode.Options{Workers: 2, Audio: j.Doc.Audio, Sounds: sounds, Background: background}
		frames := func(i int) *image.RGBA { return raster.Render(j.Doc, j.Bind, float64(i)/30, 1920, 1080) }
		if strings.HasPrefix(j.Doc.Moment, "plugin:") {
			opts.MaxSeconds = 600
			// Long static data cards need one rasterization, not thousands.
			if staticPluginScene(j.Doc) {
				still := frames(0)
				frames = func(int) *image.RGBA { return still }
			}
		}
		e = encode.WithOptions(ctx, s.o.FFmpeg(), frames, int(j.Doc.Duration*30), 30, audio, out, opts)
	}
	if e != nil {
		return e
	}
	st, e := os.Stat(out)
	if e != nil {
		return e
	}
	s.mu.Lock()
	s.cache[j.Hash] = entry{core.ClipRef{Hash: j.Hash, Duration: time.Duration(j.Doc.Duration * float64(time.Second)), Path: out}, st.Size(), time.Now(), s.pins[j.Hash]}
	s.evict()
	s.mu.Unlock()
	meta, _ := json.Marshal(j.Doc.Duration)
	return os.WriteFile(filepath.Join(s.o.Dir, j.Hash+".json"), meta, 0640)
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
		s.cache[h] = entry{Ref: core.ClipRef{Hash: h, Duration: time.Duration(d * float64(time.Second)), Path: filepath.Join(s.o.Dir, v.Name())}, Size: st.Size(), Used: st.ModTime()}
	}
}
func (s *Service) evict() {
	for h, until := range s.pins {
		if until.Before(time.Now()) {
			delete(s.pins, h)
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
	for _, key := range []string{"type.display", "type.mono", "type.body", "logo"} {
		d, _ := j.Bind.Tokens[key].(string)
		if len(d) == 64 {
			refs[d] = d
		}
	}
	for slot, d := range refs {
		if _, ok := j.Bind.Assets[d]; ok {
			continue
		}
		if len(d) != 64 {
			if d != "" && s.o.Artwork != nil {
				im, e := s.o.Artwork(ctx, d)
				if e != nil {
					if j.Request.Moment == "bug" && slot == "channel.logo" {
						continue
					}
					return e
				}
				j.Bind.Assets[d] = im
				j.Bind.Slots[slot] = d
			}
			continue
		}
		var b []byte
		var kind, mime string
		if e := s.db.QueryRowContext(ctx, "SELECT content,kind,mime FROM onair_assets WHERE digest=?", d).Scan(&b, &kind, &mime); e != nil {
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
