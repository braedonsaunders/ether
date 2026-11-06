package livetv

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// scheduleBroadcastPlanner snapshots the channel recipe. An edited recipe gets
// a new planner when P4 replaces the channel output after lineup invalidation.
type scheduleBroadcastPlanner struct {
	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerDone   chan struct{}
	requests     chan time.Time
	lastRequest  time.Time
	window       []Programme
	overlayMu    sync.Mutex
	overlayCards map[string]*programmeOverlays
	overlayBug   *videoOverlay
	joinAt       time.Time
	caps         core.ClientCaps
	s            *Service
	channel      Channel
	user         core.User
	format       transcode.RawFormat
	runner       transcode.ContinuousRunner
	mu           sync.Mutex
}

func (p *scheduleBroadcastPlanner) startupLead() time.Duration { return 12 * time.Second }
func (p *providerBroadcastPlanner) startupLead() time.Duration { return 12 * time.Second }

func (p *scheduleBroadcastPlanner) startPlanner(parent context.Context) {
	p.workerCtx, p.workerCancel = context.WithCancel(parent)
	p.requests = make(chan time.Time, 1)
	p.workerDone = make(chan struct{})
	// Seed audience/rating identity before the first neutral startup frame.
	// These are already materialized time slices; no schedule or availability
	// expansion runs during a tune, even for a large/personal recipe.
	now := time.Now()
	c := p.s.personalChannel(p.workerCtx, p.channel, &p.user)
	p.window, _ = p.s.programmes(p.workerCtx, c, now, now.Add(12*time.Minute))
	go p.planWorker()
}
func (p *scheduleBroadcastPlanner) closePlanner() { p.workerCancel(); <-p.workerDone }
func (p *scheduleBroadcastPlanner) planWorker() {
	defer close(p.workerDone)
	for {
		select {
		case <-p.workerCtx.Done():
			return
		case now := <-p.requests:
			c := p.s.graphicsChannel(p.channel)
			c = p.s.personalChannel(p.workerCtx, c, &p.user)
			var ps []Programme
			if c.Personal != nil {
				ps, _ = p.s.programmes(p.workerCtx, c, now, now.Add(12*time.Minute))
				if welcome, ok := p.s.personalWelcome(c, now); ok {
					ps = append([]Programme{welcome}, ps...)
				}
			} else {
				ps = p.s.broadcastSchedule(p.workerCtx, p.s.plannedChannel(c), now, now.Add(12*time.Minute), true)
			}
			for i := range ps {
				ps[i].policyTag = p.s.programmePolicy(p.workerCtx, ps[i])
			}
			p.mu.Lock()
			p.window = ps
			p.mu.Unlock()
		}
	}
}
func (p *scheduleBroadcastPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastRequest.IsZero() || now.Sub(p.lastRequest) >= time.Second {
		select {
		case p.requests <- now:
			p.lastRequest = now
		default:
		}
	}
	current := broadcastPlan{id: "screen-" + p.channel.ID}
	var next *broadcastPlan
	for _, programme := range p.window {
		if current.id != "screen-"+p.channel.ID && programme.Start.Before(current.end) {
			continue
		}
		plan := p.programme(programme, now)
		if !now.Before(programme.Start) && now.Before(programme.End) {
			current = plan
		} else if programme.Start.After(now) {
			next = &plan
			break
		}
	}
	return current, next
}

// A short ident must not hide the programme after it from prewarming.
func (p *scheduleBroadcastPlanner) prewarmPlans(now time.Time) []broadcastPlan {
	p.mu.Lock()
	defer p.mu.Unlock()
	var plans []broadcastPlan
	inPod := false
	for _, programme := range p.window {
		if !now.Before(programme.Start) && now.Before(programme.End) {
			inPod = programme.Item.Key == 0 && programme.Item.Slate != offAirSlate
			break
		}
	}
	programmePrepared := false
	for _, programme := range p.window {
		if !programme.Start.After(now) {
			continue
		}
		imminent := !programme.Start.After(now.Add(broadcastPrewarm))
		// A pod's first programme is prepared from the beginning of the pod,
		// even when short intervening elements hide it beyond the usual lead.
		if (imminent && len(plans) < 7) || (inPod && !programmePrepared && programme.Item.Key != 0) {
			plans = append(plans, p.programme(programme))
		}
		if programme.Item.Key != 0 {
			programmePrepared = true
		}
	}
	return plans
}

func (p *scheduleBroadcastPlanner) programme(programme Programme, plannedAt ...time.Time) broadcastPlan {
	element := programme.Item.Slate
	if programme.Item.ReelClipID != "" {
		element = "reel"
	} else if programme.Item.Key != 0 {
		element = "programme"
	}
	plan := broadcastPlan{mediaKey: programme.Item.Key, sourceOffset: programme.sourceOffset, interstitial: interstitial(programme.Item), fallback: programme.graphicsFallback, element: element, policy: programme.policyTag, programme: programme.Item.Key != 0, id: broadcastProgrammeID(programme), start: programme.Start, end: programme.End}
	if interstitial(programme.Item) {
		plan.failed = func(err error) time.Time {
			p.s.recordGraphicsSkip(p.channel, programme, err)
			// Refresh the input decision without changing any schedule boundary.
			select {
			case p.requests <- p.s.o.Now():
			default:
			}
			return programme.End
		}
	}
	plan.open = func(ctx context.Context) (transcode.Decoder, func(), error) {
		c := p.s.graphicsChannel(p.s.personalChannel(ctx, p.channel, &p.user))
		if p.s.o.Playback == nil && programme.Item.Slate == "" {
			return nil, nil, core.ErrNotAvailable
		}
		if programme.Item.ReelClipID != "" {
			var path string
			if reel := c.reelForItem(programme.Item); reel != nil {
				for _, clip := range reel.Clips {
					if clip.ID == programme.Item.ReelClipID {
						path = clip.Path
						break
					}
				}
			}
			if path == "" {
				return nil, nil, core.ErrNotAvailable
			}
			decoder, err := p.runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: "reel-" + programme.Item.ReelClipID, InputURL: path, Format: p.format})
			return decoder, nil, err
		}
		if programme.Item.Slate != "" {
			if programme.Item.Slate == offAirSlate && (c.Look == nil || !c.Look.OffAir) {
				back := ""
				for _, next := range Schedule(p.s.graphicsChannel(c), programme.End, programme.End.Add(24*time.Hour)) {
					if next.Item.Key != 0 {
						back = next.Start.In(zoneOf(c.Timezone)).Format("3:04 PM")
						break
					}
				}
				return p.s.openSignalDecoder(ctx, p.s.screenRequestWithBack(c, offAirSlate, back), p.format)
			}
			var request core.ClipRequest
			if programme.graphicsRequest != nil {
				request = *programme.graphicsRequest
			} else {
				request = p.s.clipRequest(c, programme)
			}
			decoder, release, err := p.s.openGraphicsDecoder(ctx, p.runner, request, p.format, programme.Item.Slate == offAirSlate)
			if err != nil && programme.Item.Slate == offAirSlate {
				return p.s.openSignalDecoder(ctx, p.s.screenRequest(c, offAirSlate), p.format)
			}
			return decoder, release, err
		}

		now := p.s.o.Now()
		if len(plannedAt) > 0 && plannedAt[0].After(now) {
			now = plannedAt[0]
		}
		if p.joinAt.After(now) {
			now = p.joinAt
		}
		candidates := p.s.virtualCandidates(c, programme)
		if programme.Item.BreakReleaseID != "" && len(programme.Item.BreakPointsMS) > 0 {
			candidates = []Programme{programme}
		}
		var last error = core.ErrNotAvailable
		for attempt, candidate := range candidates {
			if attempt >= virtualMaxAttempts {
				break
			}
			if p.s.knownUnavailable(ctx, candidate.Item.Key) {
				continue
			}
			if !p.s.allowed(ctx, &p.user, c, &candidate) {
				continue
			}
			var ps *core.PlaySession
			var err error
			attempt, cancel := context.WithTimeout(ctx, virtualAttemptBudget)
			group := "broadcast-" + c.ID
			if c.Personal != nil {
				group += "-viewer-" + strconv.FormatInt(p.user.ID, 10)
			}
			if candidate.Item.BreakReleaseID != "" && len(candidate.Item.BreakPointsMS) > 0 {
				planned, ok := p.s.o.Playback.(interface {
					PrepareChannelRelease(context.Context, *core.User, core.RatingKey, core.ClientCaps, string, string) (*core.PlaySession, error)
				})
				if !ok {
					cancel()
					last = core.ErrSourceChanged
					continue
				}
				ps, err = planned.PrepareChannelRelease(attempt, &p.user, candidate.Item.Key, p.caps, group, candidate.Item.BreakReleaseID)
				if err == nil {
					pin, ok := p.s.o.Playback.(core.PlaybackRevisionPin)
					if !ok {
						err = core.ErrSourceChanged
					} else {
						err = pin.PinSourceRevision(attempt, ps.ID, candidate.Item.BreakRevision)
					}
					if err != nil {
						p.s.o.Playback.Stop(ps.ID)
					}
				}
			} else if grouped, ok := p.s.o.Playback.(core.ChannelPreparer); ok {
				ps, err = grouped.PrepareChannel(attempt, &p.user, candidate.Item.Key, p.caps, group)
			} else {
				ps, err = p.s.o.Playback.Prepare(attempt, &p.user, candidate.Item.Key, p.caps)
			}
			cancel()
			if err != nil {
				last = err
				continue
			}
			now = p.s.o.Now()
			if len(plannedAt) > 0 && plannedAt[0].After(now) {
				now = plannedAt[0]
			}
			if p.joinAt.After(now) {
				now = p.joinAt
			}
			offset := time.Duration(0)
			if candidate.ID == programme.ID {
				at := now
				if at.Before(programme.Start) {
					at = programme.Start
				}
				offset = programme.Offset(at)
			}
			sourceWall := time.Now()
			if candidate.ID == programme.ID && programme.Start.After(now) {
				sourceWall = sourceWall.Add(programme.Start.Sub(now))
			}
			p.s.o.Logger.Debug("broadcast programme decoder start", "channel", c.ID, "programme", programme.ID, "playback", ps.ID, "offset", offset)
			input := p.s.o.Playback.InputURL(ps.ID)
			seeked := false
			trim := offset
			if index, ok := p.s.o.Playback.(interface {
				IndexedInput(context.Context, string, time.Duration) (string, time.Duration, bool)
			}); ok {
				if url, remaining, ready := index.IndexedInput(ctx, input, offset); ready {
					input = url
					trim = remaining
					seeked = true
				}
			}
			decoder, err := p.runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: group + "-" + candidate.ID, InputURL: input, Offset: trim, Seeked: seeked, Format: p.format})
			if err != nil {
				p.s.o.Playback.Stop(ps.ID)
				last = err
				continue
			}
			if programme.naturalBreaks && p.s.o.BreakAnalyzer != nil && candidate.Item.Key.Kind() == core.KindEpisode {
				input := markers.PlaybackBreakInput(ps)
				input.Info = core.MediaInfo{Duration: ps.Info.Duration, Size: ps.Info.Size, Estimated: ps.Info.Estimated}
				if input.Revision != "" {
					decoder = &breakTapDecoder{Decoder: decoder, tap: markers.NewBreakTap(ps.Info.Duration), input: input, format: p.format, at: offset, output: p.s.breakObserved}
				}
			}
			return &overlayDecoder{policy: p.s.programmePolicy(ctx, candidate), Decoder: decoder, wallStart: sourceWall, format: p.format, overlays: p.prepareProgrammeOverlays(ctx, programme, ps.Info.Chapters)}, func() { p.s.o.Playback.Stop(ps.ID) }, nil
		}
		return nil, nil, last
	}
	return plan
}

// reserveBroadcastProvider takes the provider slot under the same Service.mu
// as ordinary tunes. A waiting output holds no slot. Legacy viewers sharing
// a channel are counted once, as are the new shared channel outputs.
func (s *Service) reserveBroadcastProvider(account, channel, id string, limit int) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broadcastProviders == nil {
		s.broadcastProviders = map[string]map[string]string{}
	}
	active := s.broadcastProviders[account]
	if active == nil {
		active = map[string]string{}
		s.broadcastProviders[account] = active
	}
	channels := map[string]bool{}
	for _, ss := range s.sessions {
		if ss.broadcast != nil || s.useBroadcast(ss.info.Channel) || !ss.ready {
			continue
		}
		source := ss.info.Channel.SourceID
		if ss.inputSource.AuthSourceID != "" {
			source = ss.inputSource.AuthSourceID
		}
		if source == account {
			channels[ss.info.Channel.ID] = true
		}
	}
	for _, ch := range active {
		channels[ch] = true
	}
	if _, exists := active[id]; exists {
		return nil, ErrProviderBusy
	}
	if limit > 0 && len(channels) >= limit {
		return nil, ErrProviderBusy
	}
	active[id] = channel
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); delete(s.broadcastProviders[account], id); s.mu.Unlock() }) }, nil
}

type providerBroadcastPlanner struct {
	guideCancel           context.CancelFunc
	guideDone             chan struct{}
	guideFrom, guideUntil time.Time
	guide                 []Programme
	s                     *Service
	channel               Channel
	format                transcode.RawFormat
	runner                transcode.ContinuousRunner
	mu                    sync.Mutex
	selected              StreamSource
	lastAired             map[string]time.Duration
}

func (p *providerBroadcastPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	p.mu.Lock()
	ready := !now.Before(p.guideFrom) && now.Before(p.guideUntil)
	var tag *broadcastPolicyTag
	if ready {
		for _, programme := range p.guide {
			if !now.Before(programme.Start) && now.Before(programme.End) {
				tag = programme.policyTag
				break
			}
		}
	}
	p.mu.Unlock()
	if !ready {
		// Hold on a neutral signal screen until the background EPG snapshot is
		// ready. A placeholder denial must never poison a published screen segment.
		// Changing identity also retires an active source if its policy expires.
		return broadcastPlan{id: "provider-policy-wait-" + p.channel.ID}, nil
	}
	return broadcastPlan{element: "programme", policy: tag, programme: true, id: "provider-" + p.channel.ID, open: p.open, failed: p.failed}, nil
}
func (p *providerBroadcastPlanner) failed(err error) time.Time {
	p.mu.Lock()
	src := p.selected
	p.mu.Unlock()
	if src.ID != "" {
		p.s.recordSourceHealth(p.channel.ID, src, err, false)
		return p.s.sourceStatus(p.channel.ID, src).RetryAfter
	}
	now := p.s.o.Now()
	retry := now.Add(15 * time.Second)
	c := p.channel
	if normalizeStreamSources(&c) == nil {
		for _, src := range c.StreamSources {
			at := p.s.sourceStatus(c.ID, src).RetryAfter
			if at.After(now) && at.Before(retry) {
				retry = at
			}
		}
	}
	return retry
}
func (p *providerBroadcastPlanner) open(ctx context.Context) (transcode.Decoder, func(), error) {
	p.mu.Lock()
	p.selected = StreamSource{}
	p.mu.Unlock()
	c := p.channel
	if err := normalizeStreamSources(&c); err != nil {
		return nil, nil, err
	}
	var last error = core.ErrNotAvailable
	for _, src := range c.StreamSources {
		if !src.Enabled || p.s.sourceStatus(c.ID, src).RetryAfter.After(p.s.o.Now()) {
			continue
		}
		account := src.AuthSourceID
		if account == "" {
			account = c.SourceID
		}
		entry, err := p.s.sourceEntry(ctx, account)
		if err != nil {
			last = err
			continue
		}
		release, err := p.s.reserveBroadcastProvider(account, c.ID, "broadcast-"+c.ID, entry.limit)
		if err != nil {
			last = err
			continue
		}
		attempt, cancelAttempt := context.WithTimeout(ctx, 5*time.Second)
		resolved, headers, err := p.s.ResolveStreamSource(attempt, c, src)
		cancelAttempt()
		if err != nil {
			release()
			p.s.recordSourceHealth(c.ID, src, err, false)
			last = err
			continue
		}
		inputCtx, cancel := context.WithCancel(ctx)
		ss := &session{ctx: inputCtx, cancel: cancel, info: SessionInfo{ID: "broadcast-" + c.ID, Channel: c}, inputSource: src, inputURL: resolved, inputHeaders: headers}
		grant := src
		grant.URL = resolved
		err = p.s.grantInput(ss, grant, headers)
		var input string
		if err == nil {
			input, err = p.s.relayInput(ss)
		}
		var decoder transcode.Decoder
		if err == nil {
			p.mu.Lock()
			_, aired := p.lastAired[src.ID]
			p.mu.Unlock()
			decoder, err = p.runner.StartDecoder(inputCtx, transcode.DecoderRequest{SessionID: ss.info.ID, InputURL: input, Format: p.format, Live: true, PrimeLive: !aired})
		}
		cleanup := func() { cancel(); p.s.revokeInput(ss); release() }
		if err != nil {
			cleanup()
			p.s.recordSourceHealth(c.ID, src, err, false)
			last = err
			continue
		}
		p.mu.Lock()
		p.selected = src
		p.mu.Unlock()
		return &providerBroadcastDecoder{Decoder: decoder, planner: p, sourceID: src.ID, ready: func() { p.s.recordSourceHealth(c.ID, src, nil, true) }}, cleanup, nil
	}
	return nil, nil, fmt.Errorf("broadcast provider: %w", last)
}

type providerBroadcastDecoder struct {
	transcode.Decoder
	planner  *providerBroadcastPlanner
	sourceID string
	once     sync.Once
	ready    func()
}

func (d *providerBroadcastDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	for {
		frame, err := d.Decoder.Next(ctx)
		if err != nil {
			return frame, err
		}
		if !frame.SourcePTSValid {
			return transcode.RawFrame{}, fmt.Errorf("live decoder did not report a source timestamp")
		}
		d.planner.mu.Lock()
		floor, seen := d.planner.lastAired[d.sourceID]
		d.planner.mu.Unlock()
		if seen && frame.SourcePTS <= floor {
			continue
		}
		d.once.Do(d.ready)
		return frame, nil
	}
}
func (d *providerBroadcastDecoder) Aired(frame transcode.RawFrame) {
	if !frame.SourcePTSValid {
		return
	}
	d.planner.mu.Lock()
	defer d.planner.mu.Unlock()
	if d.planner.lastAired == nil {
		d.planner.lastAired = map[string]time.Duration{}
	}
	last, seen := d.planner.lastAired[d.sourceID]
	if !seen || frame.SourcePTS > last {
		d.planner.lastAired[d.sourceID] = frame.SourcePTS
	}
}

func (p *scheduleBroadcastPlanner) broadcastLogger() *slog.Logger {
	return p.s.o.Logger.With("channel", p.channel.ID)
}
func (p *providerBroadcastPlanner) broadcastLogger() *slog.Logger {
	return p.s.o.Logger.With("channel", p.channel.ID)
}

func (d *providerBroadcastDecoder) Diagnostics() string {
	d.planner.mu.Lock()
	floor := d.planner.lastAired[d.sourceID]
	d.planner.mu.Unlock()
	details := ""
	if source, ok := d.Decoder.(interface{ Diagnostics() string }); ok {
		details = source.Diagnostics()
	}
	return fmt.Sprintf("lastAiredSourcePTS=%s %s", floor, details)
}

// IPTV outputs retain EPG policy at exact frame-clock boundaries as well.
// Fetching/hydrating the bounded upcoming window is background work.
func (p *providerBroadcastPlanner) startPlanner(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	p.guideCancel = cancel
	p.guideDone = make(chan struct{})
	go func() {
		defer close(p.guideDone)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			now := p.s.o.Now()
			until := now.Add(2 * time.Minute)
			ps, err := p.s.programmes(ctx, p.channel, now, until)
			if err == nil {
				for i := range ps {
					ps[i].policyTag = p.s.programmePolicy(ctx, ps[i])
				}
				p.mu.Lock()
				p.guideFrom, p.guideUntil, p.guide = now, until, ps
				p.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
func (p *providerBroadcastPlanner) closePlanner() { p.guideCancel(); <-p.guideDone }

// Runtime preparation and retry identity includes the media and full interval.
// Guide IDs remain unchanged; an edited item can never inherit another input's
// prepared decoder, failure deadline, or latched fallback decision.
func broadcastProgrammeID(p Programme) string {
	identity := fmt.Sprintf("%s/%s/%d/%s/%s/%s/%d/%d/%d", p.SourceID, p.Item.SourceID, p.Item.Key, p.Item.Slate, p.Item.ReelID, p.Item.ReelClipID, p.sourceOffset, p.Start.UnixNano(), p.End.UnixNano())
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x-%s", hash[:8], p.ID)
}
