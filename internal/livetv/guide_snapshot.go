package livetv

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	virtualGuideRefreshEvery = 30 * time.Second
	virtualGuideIdleWindow   = 15 * time.Minute
	virtualGuideColdWait     = 5 * time.Second
)

type virtualGuideSnapshot struct {
	recipe               string
	built                time.Time
	revision             uint64
	epoch                int64
	channel              Channel
	playbackChannel      Channel
	from, until          time.Time
	raw, playback, guide *compactProgrammes
}

type virtualGuideFlight struct {
	done chan struct{}
	err  error
}

type virtualGuideCache struct {
	mu             sync.RWMutex
	buildMu        sync.Mutex
	epoch          int64
	revision       uint64
	lastDemand     time.Time
	timelineDemand time.Time
	timeline       map[string]*virtualGuideSnapshot
	entries        map[string]*virtualGuideSnapshot
	inflight       map[string]*virtualGuideFlight
	prepOnce       sync.Once
	prepSem        chan struct{}
	wake           chan struct{}
	builds         atomic.Int64 // snapshot builds, for idle-cost checks
}

func (s *Service) acquirePrepSlot(ctx context.Context) (func(), error) {
	s.virtualGuide.prepOnce.Do(func() {
		s.virtualGuide.prepSem = make(chan struct{}, 2)
	})
	select {
	case s.virtualGuide.prepSem <- struct{}{}:
		return func() { <-s.virtualGuide.prepSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) waitVirtualGuideInflight() {
	for {
		s.virtualGuide.mu.RLock()
		var flight *virtualGuideFlight
		for _, cur := range s.virtualGuide.inflight {
			flight = cur
			break
		}
		s.virtualGuide.mu.RUnlock()
		if flight == nil {
			return
		}
		<-flight.done
	}
}

func (s *Service) hasLiveActivity() bool {
	s.mu.Lock()
	activeSessions := len(s.sessions) > 0
	s.mu.Unlock()
	if activeSessions {
		return true
	}
	return s.broadcasts.hasActive()
}

func (s *Service) virtualGuideActive(now time.Time) bool {
	if s.hasLiveActivity() {
		return true
	}
	s.virtualGuide.mu.RLock()
	last := s.virtualGuide.lastDemand
	s.virtualGuide.mu.RUnlock()
	return !last.IsZero() && now.Sub(last) < virtualGuideIdleWindow
}

func (s *Service) timelineGuideActive(now time.Time) bool {
	s.virtualGuide.mu.RLock()
	last := s.virtualGuide.timelineDemand
	s.virtualGuide.mu.RUnlock()
	return !last.IsZero() && now.Sub(last) < virtualGuideIdleWindow
}

func (s *Service) noteVirtualGuideDemand(timeline bool) {
	now := s.o.Now()
	s.virtualGuide.mu.Lock()
	wakeGuide := s.virtualGuide.lastDemand.IsZero() || now.Sub(s.virtualGuide.lastDemand) >= virtualGuideIdleWindow
	s.virtualGuide.lastDemand = now
	if timeline {
		if s.virtualGuide.timelineDemand.IsZero() || now.Sub(s.virtualGuide.timelineDemand) >= virtualGuideIdleWindow {
			wakeGuide = true
		}
		s.virtualGuide.timelineDemand = now
	}
	s.virtualGuide.mu.Unlock()
	if wakeGuide {
		s.wakeVirtualGuide()
		s.wakeBreakLoop()
		s.wakePersonalLoop()
	}
}

func (s *Service) evictIdleVirtualGuide(now time.Time) bool {
	if s.hasLiveActivity() {
		return false
	}
	s.virtualGuide.mu.Lock()
	if !s.virtualGuide.lastDemand.IsZero() && now.Sub(s.virtualGuide.lastDemand) < virtualGuideIdleWindow {
		s.virtualGuide.mu.Unlock()
		return false
	}
	evicted := !s.virtualGuide.lastDemand.IsZero()
	if evicted {
		s.virtualGuide.entries = nil
		s.virtualGuide.timeline = nil
		s.virtualGuide.lastDemand = time.Time{}
		s.virtualGuide.timelineDemand = time.Time{}
	}
	s.virtualGuide.mu.Unlock()
	if evicted {
		s.searchVirtualMu.Lock()
		s.searchVirtualSnapshots = nil
		s.searchVirtualMu.Unlock()
	}
	return true
}

func (s *Service) wakeVirtualGuide() {
	if s.virtualGuide.wake != nil {
		select {
		case s.virtualGuide.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) virtualGuideLoop() {
	defer s.wg.Done()
	timer := s.newTimer(virtualGuideRefreshEvery)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()
	for {
		now := s.o.Now()
		if !s.virtualGuideActive(now) {
			s.evictIdleVirtualGuide(now)
			select {
			case <-s.serviceContext().Done():
				return
			case <-s.virtualGuide.wake:
				continue
			}
		}
		if err := s.refreshVirtualGuide(s.serviceContext()); err != nil && s.serviceContext().Err() == nil {
			s.o.Logger.Debug("virtual guide snapshot refresh failed", "error", err)
		}
		if s.timelineGuideActive(s.o.Now()) {
			if err := s.refreshTimelineGuide(s.serviceContext()); err != nil && s.serviceContext().Err() == nil {
				s.o.Logger.Debug("timeline snapshot refresh failed", "error", err)
			}
		}
		timer.Reset(virtualGuideRefreshEvery)
		select {
		case <-s.serviceContext().Done():
			return
		case <-timer.C:
		case <-s.virtualGuide.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func (s *Service) publishVirtualGuideEntry(id string, epoch int64, entry *virtualGuideSnapshot) {
	published := *entry
	published.epoch = epoch
	s.virtualGuide.mu.Lock()
	if s.virtualGuide.entries == nil {
		s.virtualGuide.entries = map[string]*virtualGuideSnapshot{}
	}
	if epoch > s.virtualGuide.epoch {
		s.virtualGuide.epoch = epoch
	}
	s.virtualGuide.entries[id] = &published
	s.virtualGuide.mu.Unlock()
	if err := s.indexVirtualSearchSnapshot(s.serviceContext(), id, &published); err != nil {
		s.o.Logger.Warn("virtual guide search index failed", "channel", id, "error", err)
	}
}

func (s *Service) startChannelPrep(c Channel, epoch int64, revision uint64, now time.Time, compiled *guideCompileCache) *virtualGuideFlight {
	s.virtualGuide.mu.Lock()
	if f := s.virtualGuide.inflight[c.ID]; f != nil {
		s.virtualGuide.mu.Unlock()
		return f
	}
	if !s.Enabled() || s.serviceContext() == nil || s.serviceContext().Err() != nil {
		s.virtualGuide.mu.Unlock()
		f := &virtualGuideFlight{done: make(chan struct{}), err: context.Canceled}
		close(f.done)
		return f
	}
	if s.virtualGuide.inflight == nil {
		s.virtualGuide.inflight = map[string]*virtualGuideFlight{}
	}
	f := &virtualGuideFlight{done: make(chan struct{})}
	s.virtualGuide.inflight[c.ID] = f
	s.virtualGuide.mu.Unlock()

	go func() {
		defer func() {
			s.virtualGuide.mu.Lock()
			if s.virtualGuide.inflight[c.ID] == f {
				delete(s.virtualGuide.inflight, c.ID)
			}
			close(f.done)
			s.virtualGuide.mu.Unlock()
		}()
		f.err = s.warmSingleVirtualChannel(s.serviceContext(), c, epoch, revision, now, compiled, nil)
	}()
	return f
}

func (s *Service) warmSingleVirtualChannel(ctx context.Context, c Channel, epoch int64, revision uint64, now time.Time, compiled *guideCompileCache, preloadedUsers []core.User) error {
	s.ensureChannelReels(ctx, c)
	if c.Personal == nil {
		if compiled == nil {
			compiled = &guideCompileCache{}
		}
		recipe, reuse := s.virtualGuideFingerprint(c, now, revision, compiled)
		if reuse {
			s.virtualGuide.mu.RLock()
			prior := s.virtualGuide.entries[c.ID]
			s.virtualGuide.mu.RUnlock()
			if prior != nil {
				updated := *prior
				updated.channel = c
				updated.playbackChannel = refreshSnapshotMetadata(prior.playbackChannel, c)
				s.publishVirtualGuideEntry(c.ID, epoch, &updated)
				if shareableGuide(c) {
					compiled.remember(recipe, &updated)
				}
				return nil
			}
		}
	}
	release, err := s.acquirePrepSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	if c.Personal != nil {
		if err := s.backfillPersonalIndex(ctx); err != nil {
			return err
		}
		users := preloadedUsers
		if users == nil && s.o.Users != nil && s.o.Picker != nil {
			users, err = s.o.Users.Users(ctx)
			if err != nil {
				return err
			}
		}
		for i := range users {
			user := &users[i]
			if s.allowed(ctx, user, c, nil) {
				if err := s.buildPersonal(ctx, s.personalChannel(ctx, c, user), user); err != nil {
					return err
				}
			}
		}
		s.publishVirtualGuideEntry(c.ID, epoch, &virtualGuideSnapshot{channel: c, revision: revision, built: now})
		return nil
	}

	if compiled == nil {
		compiled = &guideCompileCache{}
	}
	recipe, reuse := s.virtualGuideFingerprint(c, now, revision, compiled)
	if reuse {
		s.virtualGuide.mu.RLock()
		prior := s.virtualGuide.entries[c.ID]
		s.virtualGuide.mu.RUnlock()
		if prior != nil {
			updated := *prior
			updated.channel = c
			updated.playbackChannel = refreshSnapshotMetadata(prior.playbackChannel, c)
			s.publishVirtualGuideEntry(c.ID, epoch, &updated)
			if shareableGuide(c) {
				compiled.remember(recipe, &updated)
			}
			return nil
		}
	}
	entry, err := s.buildVirtualGuideSnapshot(ctx, c, now, revision, compiled)
	if err != nil {
		return err
	}
	s.publishVirtualGuideEntry(c.ID, epoch, entry)
	return nil
}

func (s *Service) refreshVirtualGuide(ctx context.Context) error {
	s.virtualGuide.buildMu.Lock()
	defer s.virtualGuide.buildMu.Unlock()
	started := time.Now()
	var epoch int64
	if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&epoch); err != nil {
		return err
	}
	cs, _, err := s.channelSnapshot(ctx)
	if err != nil {
		return err
	}
	var users []core.User
	hasPersonal := false
	for _, c := range cs {
		if c.Kind == "virtual" && c.Personal != nil {
			hasPersonal = true
			break
		}
	}
	if hasPersonal && s.o.Users != nil && s.o.Picker != nil {
		users, err = s.o.Users.Users(ctx)
		if err != nil {
			return err
		}
	}
	now := s.o.Now()
	s.virtualGuide.mu.Lock()
	revision := s.virtualGuide.revision
	s.virtualGuide.epoch = epoch
	if s.virtualGuide.entries == nil {
		s.virtualGuide.entries = map[string]*virtualGuideSnapshot{}
	}
	s.virtualGuide.mu.Unlock()
	var seenMu sync.Mutex
	seen := map[string]bool{}
	expected := map[string]bool{}
	compiled := &guideCompileCache{}
	warm := func(c Channel) error {
		s.virtualGuide.mu.RLock()
		existingFlight := s.virtualGuide.inflight[c.ID]
		s.virtualGuide.mu.RUnlock()
		if existingFlight != nil {
			select {
			case <-existingFlight.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := s.warmSingleVirtualChannel(ctx, c, epoch, revision, now, compiled, users); err != nil {
			return err
		}
		seenMu.Lock()
		seen[c.ID] = true
		seenMu.Unlock()
		return nil
	}
	jobs := make(chan Channel, len(cs))
	for _, c := range cs {
		if c.Kind == "virtual" {
			expected[c.ID] = true
			jobs <- c
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	var failureMu sync.Mutex
	var failures []error
	// A slow channel cannot serialize every other channel's first guide, and
	// large lineups cannot launch unbounded schedule/availability work.
	for range min(2, len(expected)) {
		workers.Go(func() {
			for c := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := warm(c); err != nil {
					failureMu.Lock()
					failures = append(failures, fmt.Errorf("channel %s: %w", c.ID, err))
					failureMu.Unlock()
				}
			}
		})
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.virtualGuide.mu.Lock()
	for id := range s.virtualGuide.entries {
		if !expected[id] {
			delete(s.virtualGuide.entries, id)
		}
	}
	s.virtualGuide.mu.Unlock()
	s.o.Logger.Debug("virtual guide snapshots ready", "channels", len(seen), "total", len(expected), "duration", time.Since(started))
	return errors.Join(failures...)
}

// A binary search selects just the requested time range. Request paths never
// expand schedules, gate release availability, or hydrate titles.
func programmeSlice(ps []Programme, from, until time.Time) []Programme {
	first := sort.Search(len(ps), func(i int) bool { return ps[i].End.After(from) })
	last := first + sort.Search(len(ps)-first, func(i int) bool { return !ps[first+i].Start.Before(until) })
	return append([]Programme(nil), ps[first:last]...)
}
func (s *Service) virtualProgrammes(c Channel, from, until time.Time, kind string) []Programme {
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	if entry == nil || from.Before(entry.from) || until.After(entry.until) {
		if s.serviceContext() != nil {
			s.noteVirtualGuideDemand(false)
		} else {
			s.wakeVirtualGuide()
		}
		return nil
	}
	ps := entry.playback
	if kind == "raw" {
		ps = entry.raw
	}
	if kind == "guide" {
		ps = entry.guide
	}
	out := ps.slice(c.ID, from, until)
	if kind == "guide" {
		s.mu.Lock()
		s.demandReadinessLocked(c.ID, 24*time.Hour)
		s.mu.Unlock()
	}
	return s.readyProgrammes(c, out)
}
func (s *Service) allowedProgramme(ctx context.Context, u *core.User, c Channel, p *Programme) bool {
	if p != nil && p.policyTag != nil {
		tag := p.policyTag
		var lookup func(context.Context, core.RatingKey) (*core.Title, error)
		if tag.catalog {
			lookup = func(context.Context, core.RatingKey) (*core.Title, error) { return tag.title, nil }
		}
		return s.allowedWithLookup(ctx, u, c, p, lookup, false)
	}
	return s.allowed(ctx, u, c, p)
}

type virtualGuideContextKey struct{}

func entryCoversNow(entry *virtualGuideSnapshot, c Channel, epoch int64, revision uint64, now time.Time) bool {
	if entry == nil || entry.epoch != epoch || entry.revision != revision {
		return false
	}
	if c.Personal == nil && (now.Before(entry.built) || entry.until.Before(now.Add(7*24*time.Hour+time.Hour))) {
		return false
	}
	return true
}

func (s *Service) prepareVirtualChannels(ctx context.Context, missing []Channel, epoch int64, now time.Time) error {
	s.virtualGuide.mu.RLock()
	revision := s.virtualGuide.revision
	s.virtualGuide.mu.RUnlock()
	compiled := &guideCompileCache{}
	flights := make([]*virtualGuideFlight, 0, len(missing))
	for _, c := range missing {
		flights = append(flights, s.startChannelPrep(c, epoch, revision, now, compiled))
	}
	s.noteVirtualGuideDemand(false)

	waitTimer := s.newTimer(virtualGuideColdWait)
	defer waitTimer.Stop()
	for _, f := range flights {
		select {
		case <-f.done:
			if f.err != nil {
				return core.ErrNotAvailable
			}
		case <-waitTimer.C:
			return core.ErrNotAvailable
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.virtualGuide.mu.RLock()
	ready := true
	for _, c := range missing {
		if !entryCoversNow(s.virtualGuide.entries[c.ID], c, epoch, s.virtualGuide.revision, now) {
			ready = false
			break
		}
	}
	s.virtualGuide.mu.RUnlock()
	if !ready {
		return core.ErrNotAvailable
	}
	return nil
}

func (s *Service) withVirtualGuide(ctx context.Context, cs []Channel) (context.Context, error) {
	s.virtualGuide.mu.RLock()
	hasVirtual := len(s.virtualGuide.entries) > 0
	s.virtualGuide.mu.RUnlock()
	needed := len(cs) == 0 && hasVirtual
	for _, c := range cs {
		if c.Kind == "virtual" && c.Enabled {
			needed = true
			break
		}
	}
	if !needed {
		return ctx, nil
	}
	epoch, checked := ctx.Value(virtualGuideContextKey{}).(int64)
	if !checked {
		if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&epoch); err != nil {
			return ctx, err
		}
	}
	now := s.o.Now()
	if s.serviceContext() != nil {
		s.evictIdleVirtualGuide(now)
	}
	s.virtualGuide.mu.RLock()
	var missing []Channel
	ready := len(cs) > 0 || s.virtualGuide.epoch == epoch
	for _, c := range cs {
		if c.Kind == "virtual" && c.Enabled {
			entry := s.virtualGuide.entries[c.ID]
			ok := entry != nil && entry.epoch == epoch && entry.revision == s.virtualGuide.revision
			if s.serviceContext() != nil {
				ok = entryCoversNow(entry, c, epoch, s.virtualGuide.revision, now)
			}
			if !ok {
				ready = false
				missing = append(missing, c)
			}
		}
	}
	s.virtualGuide.mu.RUnlock()
	if !ready {
		if s.serviceContext() != nil && len(missing) > 0 {
			if err := s.prepareVirtualChannels(ctx, missing, epoch, now); err != nil {
				return ctx, err
			}
			return context.WithValue(ctx, virtualGuideContextKey{}, epoch), nil
		}
		if s.serviceContext() != nil {
			s.noteVirtualGuideDemand(false)
		} else {
			s.wakeVirtualGuide()
		}
		return ctx, core.ErrNotAvailable
	}
	if s.serviceContext() != nil {
		s.noteVirtualGuideDemand(false)
	}
	// Reuse the epoch query within this request, but always check each requested
	// channel: one warm channel cannot grant readiness to a cold neighbour.
	return context.WithValue(ctx, virtualGuideContextKey{}, epoch), nil
}

// refreshVirtualGuideIfWarm updates snapshots when warm or actively demanded,
// and otherwise refreshes only the lightweight decoded channel cache.
func (s *Service) refreshVirtualGuideIfWarm(ctx context.Context) error {
	s.virtualGuide.mu.RLock()
	warm := len(s.virtualGuide.entries) > 0
	s.virtualGuide.mu.RUnlock()
	if !warm && (s.serviceContext() == nil || !s.virtualGuideActive(s.o.Now())) {
		_, _, err := s.channelSnapshot(ctx)
		return err
	}
	return s.refreshVirtualGuide(ctx)
}

// RefreshGuide is the owner-save hook for look and schedule changes. Viewer
// reads never call this method. When Live TV is cold and idle, bumping the
// revision invalidates any stale state without materializing snapshots.
func (s *Service) RefreshGuide(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	s.virtualGuide.mu.Lock()
	s.virtualGuide.revision++
	warm := len(s.virtualGuide.entries) > 0
	s.virtualGuide.mu.Unlock()
	if !warm && (s.serviceContext() == nil || !s.virtualGuideActive(s.o.Now())) {
		return nil
	}
	return s.refreshVirtualGuide(ctx)
}

// SnapshotView selects an existing immutable representation. All Programme
// fields (including source/block provenance) survive the time-indexed read.
type SnapshotView string

const (
	SnapshotRaw      SnapshotView = "raw"
	SnapshotPlayback SnapshotView = "playback"
	SnapshotGuide    SnapshotView = "guide"
	SnapshotTimeline SnapshotView = "timeline"
)

func (s *Service) ProgrammesSnapshot(ctx context.Context, u *core.User, id string, from, until time.Time, view SnapshotView) ([]Programme, error) {
	if !s.Enabled() {
		return nil, core.ErrNotAvailable
	}
	if !until.After(from) || until.Sub(from) > 8*24*time.Hour {
		return nil, fmt.Errorf("invalid snapshot window")
	}
	if view != SnapshotRaw && view != SnapshotPlayback && view != SnapshotGuide && view != SnapshotTimeline {
		return nil, fmt.Errorf("invalid snapshot view")
	}
	ctx, _, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, err
	}
	if s.serviceContext() != nil {
		s.evictIdleVirtualGuide(s.o.Now())
	}
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[id]
	epoch, revision := s.virtualGuide.epoch, s.virtualGuide.revision
	s.virtualGuide.mu.RUnlock()
	if entry == nil && s.serviceContext() != nil {
		if ch, chErr := s.Channel(ctx, id); chErr == nil && ch.Kind == "virtual" && ch.Enabled {
			if nextCtx, prepErr := s.withVirtualGuide(ctx, []Channel{ch}); prepErr == nil {
				ctx = nextCtx
				s.virtualGuide.mu.RLock()
				entry = s.virtualGuide.entries[id]
				epoch, revision = s.virtualGuide.epoch, s.virtualGuide.revision
				s.virtualGuide.mu.RUnlock()
			}
		}
	}
	extendedTimeline := false
	if view == SnapshotTimeline && entry != nil && entry.channel.Personal == nil && (from.Before(entry.from) || until.After(entry.until)) {
		extendedTimeline = true
		s.virtualGuide.mu.RLock()
		entry = s.virtualGuide.timeline[id]
		epoch, revision = s.virtualGuide.epoch, s.virtualGuide.revision
		s.virtualGuide.mu.RUnlock()
	}
	if entry == nil || (view == SnapshotTimeline && (entry.epoch != epoch || entry.revision != revision)) {
		if s.serviceContext() != nil {
			s.noteVirtualGuideDemand(extendedTimeline || view == SnapshotTimeline)
		} else {
			s.wakeVirtualGuide()
		}
		return nil, core.ErrNotAvailable
	}
	c := s.personalChannel(ctx, entry.channel, u)
	ctx, err = s.withVirtualGuide(ctx, []Channel{c})
	if err != nil {
		return nil, err
	}
	if !extendedTimeline {
		s.virtualGuide.mu.RLock()
		if latest := s.virtualGuide.entries[id]; latest != nil {
			entry = latest
		}
		s.virtualGuide.mu.RUnlock()
	}
	var ps []Programme
	if c.Personal != nil {
		ps, err = s.personalProgrammes(ctx, c, u, from, until)
	} else {
		if from.Before(entry.from) || until.After(entry.until) {
			if s.serviceContext() != nil && view == SnapshotTimeline {
				s.noteVirtualGuideDemand(true)
			}
			return nil, core.ErrNotAvailable
		}
		if view == SnapshotTimeline {
			ps = entry.playback.slice(c.ID, from, until)
		} else {
			ps = s.virtualProgrammes(c, from, until, string(view))
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]Programme, 0, len(ps))
	for _, p := range ps {
		if s.allowedProgramme(ctx, u, c, &p) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Service) checkVirtualWindow(c Channel, from, until time.Time) error {
	if c.Kind != "virtual" || c.Personal != nil || !c.Enabled {
		return nil
	}
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	if entry == nil || from.Before(entry.from) || until.After(entry.until) {
		if s.serviceContext() != nil {
			s.noteVirtualGuideDemand(false)
		} else {
			s.wakeVirtualGuide()
		}
		return core.ErrNotAvailable
	}
	return nil
}

// The frame planner reuses background availability gating too; polling its
// upcoming window must not turn a large recipe into a continuous SQL sweep.
func (s *Service) plannedChannel(c Channel) Channel {
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	if entry != nil && entry.channel.Personal == nil {
		return entry.playbackChannel
	}
	return c
}

func sameProgrammeStorage(a, b []Programme) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func refreshSnapshotMetadata(playback, current Channel) Channel {
	playback.ID = current.ID
	playback.Name = current.Name
	playback.Number = current.Number
	playback.Logo = current.Logo
	playback.LogoMatched = current.LogoMatched
	playback.Favorite = current.Favorite
	playback.Enabled = current.Enabled
	playback.Private = current.Private
	playback.Group = current.Group
	playback.CustomGroup = current.CustomGroup
	playback.Health = current.Health
	return playback
}

// buildVirtualGuideSnapshot compiles one channel without publishing it. A pass
// shares its thread-safe cache across workers, so identical recipes expand once.
func (s *Service) buildVirtualGuideSnapshot(ctx context.Context, c Channel, now time.Time, revision uint64, compiled *guideCompileCache) (*virtualGuideSnapshot, error) {
	return s.buildVirtualSnapshot(ctx, c, now, revision, compiled, false)
}

func (s *Service) buildVirtualSnapshot(ctx context.Context, c Channel, now time.Time, revision uint64, compiled *guideCompileCache, timeline bool) (*virtualGuideSnapshot, error) {
	if compiled == nil {
		compiled = &guideCompileCache{}
	}
	recipe := compiled.fingerprint(s, c, revision, now)
	build := func() (*virtualGuideSnapshot, error) {
		s.virtualGuide.builds.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		from, until := now.Add(-24*time.Hour), now.Add(8*24*time.Hour+time.Hour)
		if timeline {
			until = now.Add(28*24*time.Hour + time.Hour)
		}
		gated := s.guideChannel(ctx, c)
		entry := &virtualGuideSnapshot{channel: c, playbackChannel: gated, recipe: recipe, built: now, from: from, until: until, revision: revision}
		rawChannel := c
		if timeline {
			rawChannel = gated
		}
		raw := Schedule(rawChannel, from, until)
		playback := raw
		if !timeline && !reflect.DeepEqual(c, gated) {
			playback = Schedule(gated, from, until)
		}
		guide := playback
		if !timeline && gated.clipDuration != nil {
			guide = s.broadcastGuide(gated, from, until)
		}
		tags := map[struct {
			key           core.RatingKey
			rating, slate string
		}]*broadcastPolicyTag{}
		facts := map[core.RatingKey]*core.Title{}
		loaded := map[core.RatingKey]bool{}
		for _, ps := range [][]Programme{raw, playback, guide} {
			for i := range ps {
				p := ps[i]
				if p.Item.Key != 0 && !loaded[p.Item.Key] && s.o.Catalog != nil {
					title, err := s.o.Catalog.Get(ctx, p.Item.Key)
					if err == nil && title != nil {
						copy := *title
						facts[p.Item.Key] = &copy
					}
					loaded[p.Item.Key] = true
				}
				identity := struct {
					key           core.RatingKey
					rating, slate string
				}{p.Item.Key, p.Item.Rating, p.Item.Slate}
				if tags[identity] == nil {
					tags[identity] = &broadcastPolicyTag{programme: Programme{Item: Item{Key: p.Item.Key, Rating: p.Item.Rating, Slate: p.Item.Slate}}, title: facts[p.Item.Key], catalog: s.o.Catalog != nil}
				}
				ps[i].policyTag = tags[identity]
			}
		}
		items := &compactItemTable{}
		entry.raw = compactProgrammeSlice(c.ID, raw, items)
		entry.playback = entry.raw
		if !sameProgrammeStorage(raw, playback) {
			entry.playback = compactProgrammeSlice(c.ID, playback, items)
		}
		entry.guide = entry.playback
		if !sameProgrammeStorage(playback, guide) {
			entry.guide = compactProgrammeSlice(c.ID, guide, items)
		}
		items.index = nil
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return entry, nil
	}
	if !shareableGuide(c) {
		return build()
	}
	shared, err := compiled.build(ctx, recipe, build)
	if err != nil {
		return nil, err
	}
	if shared.channel.ID == c.ID {
		return shared, nil
	}
	entry := *shared
	entry.channel = c
	entry.playbackChannel = refreshSnapshotMetadata(shared.playbackChannel, c)
	return &entry, nil
}

// virtualGuideFingerprint makes the skip decision before a build is admitted.
// The stored fingerprint belongs to the immutable snapshot, not lineup epoch.
func (s *Service) virtualGuideFingerprint(c Channel, now time.Time, revision uint64, compiled *guideCompileCache) (string, bool) {
	recipe := compiled.fingerprint(s, c, revision, now)
	s.virtualGuide.mu.RLock()
	prior := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	return recipe, prior != nil && prior.recipe == recipe && !now.Before(prior.built) && !prior.until.Before(now.Add(8*24*time.Hour))
}

func shareableGuide(c Channel) bool {
	if c.Personal != nil || c.Look != nil || len(c.Clock) > 0 {
		return false
	}
	for _, b := range c.Blocks {
		if b.Breaks != nil {
			return false
		}
	}
	for _, p := range c.Pins {
		if p.Breaks != nil {
			return false
		}
	}
	return true
}
