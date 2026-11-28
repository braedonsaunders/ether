package playback

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// copySourceAtCapacity keeps the known-good primary available while checking
// ranked alternates for video copy. Only the normal source open verifies an
// estimate, and the primary remains usable if every alternate fails.
func (m *Manager) copySourceAtCapacity(ctx context.Context, s *session, profile core.QualityProfile, primary *generation, tried []string) *generation {
	selection, explicit := core.PlaybackSourceDecision(ctx)
	opts := selection.Options
	if !explicit || (!opts.DirectPlay && !opts.DirectStream) {
		return nil
	}
	// Source-specific track selections cannot be applied to another file.
	if opts.SubtitleStream >= 0 || opts.AudioStream >= 0 {
		return nil
	}
	budget, ok := m.deps.Transcoder.(interface {
		VideoEncoderBusy(group string) bool
	})
	if !ok || m.deps.FetchNZB == nil || s.exact || (s.prefer != "" && !isAutoVersionSelector(s.prefer)) ||
		s.caps.Download || s.caps.BroadcastBitrateKbps > 0 || core.IsBackgroundAnalysis(ctx) ||
		len(primary.res.Alternates) == 0 || !budget.VideoEncoderBusy(s.caps.MultiviewGroup) {
		return nil
	}

	decide := selection.Decide
	if decide == nil {
		decide = func(info core.MediaInfo, _ *core.Resolution) core.Decision {
			return m.deps.Transcoder.Decide(info, s.caps, opts)
		}
	}
	info := primary.infoSnapshot()
	d := decide(info, primary.res)
	// Subtitle burn-in is part of the requested output. Keep the selected
	// source when the encoder is needed to render its subtitle track.
	// A strict pin, or a preferred release that is playing, is not replaced.
	if d.Kind == "" || d.Kind == core.DecisionDirectPlay || d.BurnSubtitles || !d.EncodesVideo(info) || m.ownerChoiceCovers(s, primary.releaseID()) {
		return nil
	}
	canCopy := func(info core.MediaInfo, res *core.Resolution) bool {
		if v := info.Video(); v == nil || v.Codec == "" {
			return false
		}
		d := decide(info, res)
		return d.Kind == core.DecisionDirectPlay || (d.Kind != "" && d.VideoCopy)
	}

	ctx, cancel := context.WithTimeout(withDeferredGenerationFills(ctx), 3*time.Second)
	defer cancel()
	ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	estimator, _ := m.deps.Resolver.(interface {
		EstimateReleaseMedia(*core.Title, core.Release, []string) core.MediaInfo
	})
	attempts := 0
	for _, alt := range primary.res.Alternates {
		if ctx.Err() != nil || attempts >= 3 {
			break
		}
		if alt.ID == "" || alt.ID == primary.releaseID() || slices.Contains(tried, alt.ID) {
			continue
		}
		m.mu.Lock()
		failed := m.openFailures[s.ident.key][alt.ID]
		m.mu.Unlock()
		if failed {
			continue
		}
		facts, probed := m.CachedReleaseProbe(alt)
		if !probed {
			if estimator == nil {
				continue
			}
			facts = estimator.EstimateReleaseMedia(s.title, alt, s.user.AudioLanguages)
		}
		if !canCopy(facts, &core.Resolution{Release: alt}) || !releaseMatchesTitle(alt, s.title) {
			continue
		}
		if m.releaseBlocked(ctx, alt.ID) || m.titleReleaseRejected(ctx, s.ident.key, alt) ||
			m.releaseNamesakeRejected(ctx, s, alt) {
			continue
		}
		attempts++
		payload, err := m.deps.FetchNZB(ctx, alt)
		if err != nil || len(payload) == 0 {
			continue
		}
		res := core.Resolution{
			Title: primary.res.Title, VersionSelector: primary.res.VersionSelector,
			Release: alt, NZB: payload, ResolvedAt: m.now(),
			Alternates: []core.Release{primary.res.Release},
		}
		for _, candidate := range primary.res.Alternates {
			if candidate.ID != alt.ID {
				res.Alternates = append(res.Alternates, candidate)
			}
		}
		g, err := m.openGeneration(ctx, s, &res, func(size int64) core.MediaInfo {
			facts.Size = size
			return facts
		}, m.persistedProbe(ctx, s.ident.userID, s.ident.key, alt.ID, payload))
		if err != nil {
			continue
		}
		verified := g.infoSnapshot()
		if ctx.Err() == nil && !verified.Estimated && canCopy(verified, g.res) && !m.knownHardSubbed(ctx, g) &&
			probedRequiredAudioRejection(profile, s.title, alt, verified, false) == "" &&
			m.probedAudioAcceptable(s, profile, verified) {
			m.log.Info("selected video-copy alternate because the encoder budget is busy", "session", s.id, "release", alt.ID)
			return g
		}
		m.dropGeneration(g)
	}
	return nil
}
