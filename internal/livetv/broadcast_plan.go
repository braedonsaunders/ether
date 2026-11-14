package livetv

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type programmeSourceState struct {
	excluded map[core.RatingKey][]string
	key      core.RatingKey
	release  string
}

// scheduleBroadcastPlanner snapshots the channel recipe and owns bounded
// release exclusions for its currently prepared airings.
type scheduleBroadcastPlanner struct {
	sourceMu sync.Mutex
	sources  map[string]*programmeSourceState

	// standby memoizes the look's cached standby clip: the frame loop asks
	// every step, and resolving it re-clones and re-hashes the scene
	// document (about half a core per channel, with 250 ms frame-clock
	// slips on production).
	standbyMu   sync.Mutex
	standbyLook string
	standbyRef  core.ClipRef
	standbyOK   bool
	standbyAt   time.Time

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

func (p *scheduleBroadcastPlanner) noteHold(kind string, dur time.Duration) {
	if p.s != nil {
		p.s.noteHoldComplete(p.channel.ID, kind, dur)
	}
}

func (p *scheduleBroadcastPlanner) noteHandoff(key core.RatingKey, offset time.Duration, at time.Time) {
	if p.s == nil || key == 0 {
		return
	}
	wk := warmTarget{channelID: p.channel.ID, key: key, offsetSec: int64(offset / time.Second)}
	p.s.warmMu.Lock()
	warmedAt, ok := p.s.warmed[wk]
	p.s.warmMu.Unlock()
	lead := time.Duration(0)
	if ok && at.After(warmedAt) {
		lead = at.Sub(warmedAt)
	}
	p.s.noteProgrammeHandoff(p.channel.ID, lead)
}

func (p *scheduleBroadcastPlanner) startPlanner(parent context.Context) {
	p.workerCtx, p.workerCancel = context.WithCancel(parent)
	p.requests = make(chan time.Time, 1)
	p.workerDone = make(chan struct{})
	// Seed audience/rating identity and the bounded opening broadcast pod
	// before the encoder can publish its first frame.
	now := time.Now()
	c := p.s.personalChannel(p.workerCtx, p.channel, &p.user)
	if c.Personal != nil {
		p.window, _ = p.s.programmes(p.workerCtx, c, now, now.Add(12*time.Minute))
	} else {
		// Seed the actual broadcast pod, including title moments, before the
		// encoder can publish. Guide rows alone omit these opening elements.
		p.window = p.s.broadcastSchedule(p.workerCtx, p.s.plannedChannel(p.s.graphicsChannel(c)), now, now.Add(12*time.Minute), true)
	}
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
			p.s.triggerBreakWarm(c.ID, ps, now)
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
		lead := broadcastPrewarm
		if programme.Item.Key != 0 {
			lead = programmePrewarm
		}
		imminent := !programme.Start.After(now.Add(lead))
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
	isInterstitial := interstitial(programme.Item)
	liveMoment := false
	if isInterstitial && programme.Item.ReelClipID == "" && p.s != nil {
		_, liveMoment = p.s.o.Graphics.(liveMomentSource)
	}
	plan := broadcastPlan{mediaKey: programme.Item.Key, sourceOffset: programme.sourceOffset, interstitial: isInterstitial, liveMoment: liveMoment, fallback: programme.graphicsFallback, element: element, policy: programme.policyTag, programme: programme.Item.Key != 0, id: broadcastProgrammeID(programme), start: programme.Start, end: programme.End}
	if plan.programme {
		plan.failed = func(error) time.Time {
			retry := p.s.o.Now().Add(15 * time.Second)
			p.sourceMu.Lock()
			if state := p.sources[plan.id]; state != nil && state.release != "" {
				state.excluded[state.key] = append(state.excluded[state.key], state.release)
				state.release = ""
				retry = p.s.o.Now().Add(100 * time.Millisecond)
			}
			p.sourceMu.Unlock()
			return retry
		}
	}
	if isInterstitial {
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
			reelID := programme.Item.ReelID
			if reelID == "" && programme.graphicsRequest != nil {
				reelID = programme.graphicsRequest.ReelID
			}
			path := p.s.openReelClipPath(c, reelID, programme.Item.ReelClipID)
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
			loop := programme.Item.Slate == offAirSlate || programme.Item.Slate == "standby"
			if !loop && programme.Item.Slate == "break" {
				if dur, ok := c.momentDuration("break"); ok && dur > 0 && programme.End.Sub(programme.Start) > dur {
					loop = true
				}
			}
			decoder, release, err := p.s.openGraphicsDecoder(ctx, p.runner, request, p.format, loop)
			if err != nil && programme.Item.Slate == offAirSlate {
				return p.s.openSignalDecoder(ctx, p.s.screenRequest(c, offAirSlate), p.format)
			}
			return decoder, release, err
		}

		return p.openProgrammeSources(ctx, c, programme, plan, plannedAt)
	}
	return plan
}

func (p *scheduleBroadcastPlanner) broadcastLogger() *slog.Logger {
	return p.s.o.Logger.With("channel", p.channel.ID)
}

// Runtime preparation and retry identity includes the media and full interval.
// Guide IDs remain unchanged; an edited item can never inherit another input's
// prepared decoder, failure deadline, or latched fallback decision.
func broadcastProgrammeID(p Programme) string {
	identity := fmt.Sprintf("%s/%s/%d/%s/%s/%s/%d/%d/%d", p.SourceID, p.Item.SourceID, p.Item.Key, p.Item.Slate, p.Item.ReelID, p.Item.ReelClipID, p.sourceOffset, p.Start.UnixNano(), p.End.UnixNano())
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x-%s", hash[:8], p.ID)
}

// The first picture is part of source admission, then transferred exactly once
// to the channel clock without restarting the decoder or losing opening audio.
type primedProgrammeDecoder struct {
	transcode.Decoder
	first *transcode.RawFrame
}

func (d *primedProgrammeDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if d.first != nil {
		f := *d.first
		d.first = nil
		return f, nil
	}
	return d.Decoder.Next(ctx)
}

func (d *primedProgrammeDecoder) Diagnostics() string {
	if source, ok := d.Decoder.(interface{ Diagnostics() string }); ok {
		return source.Diagnostics()
	}
	return ""
}
