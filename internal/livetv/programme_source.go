package livetv

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const programmeSourceDeadline = 12 * time.Second
const programmeSourcePreference = 4 * time.Second

type programmeSourceResult struct {
	decoder    transcode.Decoder
	release    func()
	candidate  Programme
	releaseID  string
	info       core.MediaInfo
	sourceWall time.Time
	err        error
}

func (r *programmeSourceResult) close() {
	if r.decoder != nil {
		r.decoder.Close()
	}
	if r.release != nil {
		r.release()
	}
}
func (p *scheduleBroadcastPlanner) programmeCaps() core.ClientCaps {
	caps := p.caps
	caps.BroadcastBitrateKbps = 3000
	if p.format.Height >= 1080 {
		caps.BroadcastBitrateKbps = 6000
	}
	if p.format.Height <= 480 {
		caps.BroadcastBitrateKbps = 1500
	}
	caps.MaxHeight, caps.MaxBitrateKbps = p.format.Height, caps.BroadcastBitrateKbps
	return caps
}

// Acquisition starts when the frame clock first opens the airing, alongside
// its title. Only two verified substitutes may race the scheduled programme;
// unknown titles never start provider discovery on this recovery path.
func (p *scheduleBroadcastPlanner) openProgrammeSources(parent context.Context, c Channel, programme Programme, plan broadcastPlan, plannedAt []time.Time) (transcode.Decoder, func(), error) {
	ctx, cancel := context.WithTimeout(parent, programmeSourceDeadline)
	defer cancel()
	p.sourceMu.Lock()
	if p.sources == nil {
		p.sources = map[string]*programmeSourceState{}
	}
	state := p.sources[plan.id]
	if state == nil {
		if len(p.sources) >= 16 {
			clear(p.sources)
		}
		state = &programmeSourceState{excluded: map[core.RatingKey][]string{}}
		p.sources[plan.id] = state
	}
	state.release = ""
	p.sourceMu.Unlock()
	results := make(chan programmeSourceResult, 3)
	active, attempts := 0, 0
	tried := map[core.RatingKey]bool{}
	caps := p.programmeCaps()
	ready, hasReady := p.s.o.Playback.(interface {
		ChannelReadyRelease(context.Context, *core.User, core.RatingKey, core.ClientCaps) (string, bool)
	})
	start := func(candidate Programme, ready string) {
		tried[candidate.Item.Key] = true
		p.sourceMu.Lock()
		cc := caps
		cc.BroadcastExclude = append([]string(nil), state.excluded[candidate.Item.Key]...)
		p.sourceMu.Unlock()
		cc.BroadcastReadyRelease = ready
		active++
		attempts++
		go func() { results <- p.openProgrammeCandidate(ctx, parent, c, programme, candidate, plan, plannedAt, cc) }()
	}
	pinned := programme.Item.BreakReleaseID != "" && len(programme.Item.BreakPointsMS) > 0
	if !p.s.knownUnavailable(ctx, programme.Item.Key) && p.s.allowed(ctx, &p.user, c, &programme) {
		id := ""
		if hasReady && !pinned {
			p.sourceMu.Lock()
			sc := caps
			sc.BroadcastExclude = append([]string(nil), state.excluded[programme.Item.Key]...)
			p.sourceMu.Unlock()
			id, _ = ready.ChannelReadyRelease(ctx, &p.user, programme.Item.Key, sc)
		}
		start(programme, id)
	}
	candidates := p.s.virtualCandidates(c, programme)

	// Readiness is queried only from bounded local evidence. A concurrently
	// finishing active-viewer validation may make a substitute eligible here.
	launchReady := func() {
		if pinned || !hasReady {
			return
		}
		for _, candidate := range candidates {
			if attempts >= 3 {
				return
			}
			if tried[candidate.Item.Key] || candidate.Item.Key == programme.Item.Key || !p.s.allowed(ctx, &p.user, c, &candidate) {
				continue
			}
			if _, ok := joinStandIn(programme, candidate.Item, p.s.o.Now()); !ok {
				continue
			}
			p.sourceMu.Lock()
			cc := caps
			cc.BroadcastExclude = append([]string(nil), state.excluded[candidate.Item.Key]...)
			p.sourceMu.Unlock()
			if id, ok := ready.ChannelReadyRelease(ctx, &p.user, candidate.Item.Key, cc); ok {
				start(candidate, id)
			}
		}
	}
	launchReady()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	prefer := time.NewTimer(programmeSourcePreference)
	defer prefer.Stop()
	preferDone := false
	var winner, backup *programmeSourceResult
	last := error(core.ErrNotAvailable)
	for winner == nil {
		if active == 0 && backup != nil {
			winner = backup
			backup = nil
			break
		}
		if active == 0 && (!hasReady || pinned) {
			break
		}
		select {
		case r := <-results:
			active--
			if r.err != nil {
				last = r.err
				if r.releaseID != "" && ctx.Err() == nil {
					p.sourceMu.Lock()
					state.excluded[r.candidate.Item.Key] = append(state.excluded[r.candidate.Item.Key], r.releaseID)
					p.sourceMu.Unlock()
				}
			} else if r.candidate.Item.Key == programme.Item.Key || preferDone {
				winner = &r
			} else if backup == nil {
				backup = &r
			} else {
				r.close()
			}
		case <-prefer.C:
			preferDone = true
			if backup != nil {
				winner = backup
				backup = nil
			}
		case <-poll.C:
			launchReady()
		case <-ctx.Done():
			last = ctx.Err()
			if backup != nil {
				winner = backup
				backup = nil
			}
			goto settled
		}
	}
settled:
	cancel()
	// Every loser is joined and closed; none can publish a late session after
	// the winner is handed to the continuous channel clock.
	for active > 0 {
		r := <-results
		active--
		r.close()
	}
	if backup != nil {
		backup.close()
	}
	if winner == nil {
		return nil, nil, last
	}
	p.sourceMu.Lock()
	state.key, state.release = winner.candidate.Item.Key, winner.releaseID
	p.sourceMu.Unlock()
	if winner.candidate.Item.Key != programme.Item.Key {
		p.s.latchProgramme(winner.candidate)
	}
	d := &overlayDecoder{policy: p.s.programmePolicy(parent, winner.candidate), Decoder: winner.decoder, wallStart: winner.sourceWall, format: p.format, overlays: p.prepareProgrammeOverlays(parent, programme, winner.info.Chapters)}
	return d, winner.release, nil
}

func (p *scheduleBroadcastPlanner) openProgrammeCandidate(ctx, parent context.Context, c Channel, programme, candidate Programme, plan broadcastPlan, plannedAt []time.Time, caps core.ClientCaps) (result programmeSourceResult) {
	result.candidate = candidate
	group := "broadcast-" + c.ID
	if c.Personal != nil {
		group += "-viewer-" + strconv.FormatInt(p.user.ID, 10)
	}
	caps.ClientID = "channel-input/" + plan.id + "/" + strconv.FormatUint(uint64(candidate.Item.Key), 10)
	var ps *core.PlaySession
	var err error
	if candidate.Item.BreakReleaseID != "" && len(candidate.Item.BreakPointsMS) > 0 {
		pinned, ok := p.s.o.Playback.(interface {
			PrepareChannelRelease(context.Context, *core.User, core.RatingKey, core.ClientCaps, string, string) (*core.PlaySession, error)
		})
		if !ok {
			result.err = core.ErrSourceChanged
			return
		}
		ps, err = pinned.PrepareChannelRelease(ctx, &p.user, candidate.Item.Key, caps, group, candidate.Item.BreakReleaseID)
		if err == nil {
			pin, ok := p.s.o.Playback.(core.PlaybackRevisionPin)
			if !ok {
				err = core.ErrSourceChanged
			} else {
				err = pin.PinSourceRevision(ctx, ps.ID, candidate.Item.BreakRevision)
			}
			if err != nil {
				p.s.o.Playback.Stop(ps.ID)
			}
		}
	} else if grouped, ok := p.s.o.Playback.(core.ChannelPreparer); ok {
		ps, err = grouped.PrepareChannel(ctx, &p.user, candidate.Item.Key, caps, group)
	} else {
		ps, err = p.s.o.Playback.Prepare(ctx, &p.user, candidate.Item.Key, caps)
	}
	if err != nil {
		result.err = err
		return
	}
	result.releaseID = ps.SourceReleaseID()
	life, stopLife := context.WithCancel(parent)
	result.release = func() { stopLife(); p.s.o.Playback.Stop(ps.ID) }
	defer func() {
		if result.err != nil {
			result.close()
			result.decoder = nil
			result.release = nil
		}
	}()
	if ctx.Err() != nil {
		result.err = ctx.Err()
		return
	}
	now := p.s.o.Now()
	if len(plannedAt) > 0 && plannedAt[0].After(now) {
		now = plannedAt[0]
	}
	if p.joinAt.After(now) {
		now = p.joinAt
	}
	var offset time.Duration
	if candidate.Item.Key != programme.Item.Key {
		sub, ok := joinStandIn(programme, candidate.Item, now)
		if !ok {
			result.err = core.ErrNotAvailable
			return
		}
		candidate = sub
		offset = sub.Offset(now)
	} else {
		at := now
		if at.Before(programme.Start) {
			at = programme.Start
		}
		offset = programme.Offset(at)
	}
	result.candidate = candidate
	if ps.Info.Duration > 0 && offset >= ps.Info.Duration {
		result.err = fmt.Errorf("programme offset %s exceeds source duration %s", offset, ps.Info.Duration)
		return
	}
	result.sourceWall = time.Now()
	if ahead := now.Sub(p.s.o.Now()); ahead > 0 {
		result.sourceWall = result.sourceWall.Add(ahead)
	}
	if programme.Start.After(now) {
		result.sourceWall = result.sourceWall.Add(programme.Start.Sub(now))
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
	if ctx.Err() != nil {
		result.err = ctx.Err()
		return
	}
	audio := p.s.programmeAudioTrack(ctx, &p.user, ps.Info, candidate.Item.Key)
	result.decoder, result.err = p.runner.StartDecoder(life, transcode.DecoderRequest{SessionID: group + "-" + candidate.ID + "-" + strconv.FormatUint(uint64(candidate.Item.Key), 10), InputURL: input, Offset: trim, Seeked: seeked, Format: p.format, AudioIndex: audio})
	if result.err != nil {
		return
	}
	first, err := result.decoder.Next(ctx)
	if err == nil && !validRawFrame(p.format, first) {
		err = fmt.Errorf("programme decoder returned an invalid first picture")
	}
	if err != nil {
		result.err = err
		return
	}
	result.decoder = &primedProgrammeDecoder{Decoder: result.decoder, first: &first}
	if programme.naturalBreaks && p.s.o.BreakAnalyzer != nil && candidate.Item.Key.Kind() == core.KindEpisode {
		input := markers.PlaybackBreakInput(ps)
		input.Info = core.MediaInfo{Duration: ps.Info.Duration, Size: ps.Info.Size, Estimated: ps.Info.Estimated}
		if input.Revision != "" {
			result.decoder = &breakTapDecoder{Decoder: result.decoder, tap: markers.NewBreakTap(ps.Info.Duration), input: input, format: p.format, at: offset, output: p.s.breakObserved}
		}
	}
	result.info = ps.Info
	return
}
