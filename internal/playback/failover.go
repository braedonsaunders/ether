package playback

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type deferGenerationFillsKey struct{}

func withDeferredGenerationFills(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferGenerationFillsKey{}, true)
}

func generationFillsDeferred(ctx context.Context) bool {
	v, _ := ctx.Value(deferGenerationFillsKey{}).(bool)
	return v
}

// errSourceBroken marks a release whose articles went missing while it was
// being opened or probed.
var errSourceBroken = fmt.Errorf("playback: source broken: %w", core.ErrArticleMissing)

// errRequireRemux reports a completed failover that swapped the session to
// a healthy release whose bytes cannot continue the in-flight direct-play
// response: the response must end and the next decision/start is a remux.
var errRequireRemux = errors.New("playback: failover requires remux")

// preparedResolution is the source a session can open without resolving:
// the warm entry, else the stored resume release when it was resolved for
// the same profile. A picked version (prefer) accepts only that release.
func (m *Manager) preparedResolution(ctx context.Context, s *session, profile core.QualityProfile, original, prefer string) (*core.Resolution, *probeCache, bool) {
	if s.caps.BroadcastReadyRelease != "" {
		return nil, nil, false
	}
	var res *core.Resolution
	var cached *probeCache
	fromResume := false
	if warm := m.takeWarmResolution(ctx, s.ident, profile, s.caps, prefer, original); warm != nil {
		res, cached = warm.res, warm.probe
		if m.titleReleaseRejected(ctx, s.ident.key, res.Release) {
			res, cached = nil, nil
		}
	}
	if res == nil {
		res = m.resumeResolution(ctx, s.ident.userID, s.ident.key, profile, s.caps, original)
		if res != nil {
			if prefer != "" && !isAutoVersionSelector(prefer) {
				if !res.MatchesVersion(prefer) {
					res = nil // a picked version wins over the remembered release
				}
			} else if core.IsExplicitVersionSlotSelector(res.VersionSelector) {
				// An explicit non-Auto slot pick stays behind its own version slot
				// instead of replacing Auto, unless promoted by a remembered choice.
				if reader, ok := m.deps.Resolver.(viewerChoiceReader); !ok || s.title == nil || reader.ActiveViewerChoiceRelease(core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user, Prefer: prefer}) != res.Release.ID {
					res = nil
				}
			}
		}
		if res != nil {
			fromResume = true
			if cached == nil {
				// A restart drops the in-memory warm probe, but the resume
				// record's persisted probe and keyframe index survive it.
				cached = m.persistedProbe(ctx, s.ident.userID, s.ident.key, res.Release.ID, res.NZB)
			}
			var info core.MediaInfo
			if cached != nil {
				info = cached.info
			}
			if exceedsRuleHeight(s.caps.QualityRuleMaxHeight, res.Release, info) ||
				(cached != nil && !rememberedInfoCodecCurrent(info, s.caps)) {
				res, cached, fromResume = nil, nil, false
			}
		}
	}
	// For a new quality request, look for an HD source matching the output
	// before reusing an oversized stored source. Active sessions are reused by
	// prepareOwnedWithin before this path; explicit picks remain authoritative.
	if res != nil && prefer == "" && s.caps.PreferredSourceHeight >= 720 && (res.Release.Info.Height == 0 || res.Release.Info.Height > s.caps.PreferredSourceHeight) {
		return nil, nil, false
	}
	if res != nil && prefer == "" && !s.caps.DefaultRemoteBitrate && s.caps.MaxBitrateKbps > 0 {
		if cached != nil && cached.info.BitrateKbps > 0 {
			if cached.info.BitrateKbps > s.caps.MaxBitrateKbps {
				return nil, nil, false
			}
		} else {
			dur := time.Duration(0)
			if cached != nil && cached.info.Duration > 0 {
				dur = cached.info.Duration
			} else if s.title != nil {
				dur = s.title.Runtime
			}
			if !sourceFitsBitrate(res.Release.Size, dur, 0, s.caps.MaxBitrateKbps) {
				return nil, nil, false
			}
		}
	}
	if res != nil && (prefer == "" || isAutoVersionSelector(prefer)) {
		if budget := m.clientDeliveryBudget(s); budget > 0 && !m.hasAuthoritativeAutoChoice(s, res.Release.ID) {
			var dur time.Duration
			var bitrate int
			if s.title != nil {
				dur = s.title.Runtime
			}
			if cached != nil {
				if cached.info.Duration > 0 {
					dur = cached.info.Duration
				}
				bitrate = cached.info.BitrateKbps
			}
			if !sourceFitsBitrate(res.Release.Size, dur, bitrate, budget) {
				return nil, nil, false
			}
		}
	}
	if res != nil && m.videoOff(s.caps) {
		if cached != nil {
			if _, needs := m.probedNeedsVideoTranscode(cached.info, s.caps); needs {
				return nil, nil, false
			}
		} else {
			capKbps := s.caps.MaxBitrateKbps
			if capKbps <= 0 && s.caps.Remote {
				capKbps = m.config().Transcode.RemoteMaxBitrateKbps
			}
			dur := time.Duration(0)
			if s.title != nil {
				dur = s.title.Runtime
			}
			if !rememberedCodecCurrent(res.Release, s.caps) || !sourceFitsBitrate(res.Release.Size, dur, 0, capKbps) {
				return nil, nil, false
			}
		}
	}
	// An eligible preferred release the session has not tried is resolved
	// before a warm or remembered source.
	if res != nil && (!m.ownerPinAllows(s, res.Release.ID) || !m.preferenceAllows(s, res.Release.ID, nil) || !m.rememberedChoiceAllows(s, res.Release.ID) || slices.Contains(s.caps.BroadcastExclude, res.Release.ID) || !m.channelSourceAllowed(s, res.Release) || m.titleReleaseRejected(ctx, s.ident.key, res.Release)) {
		return nil, nil, false
	}
	if res != nil && core.IsExplicitVersionSlotSelector(prefer) && m.isReleaseStale(s.ident.key, res.Release.ID) {
		return nil, nil, false
	}
	if res != nil && (!m.rememberedReleaseMatchesTitle(ctx, res.Release, s.title) || !payloadMatchesTitle(res.NZB, s.title)) {
		// An older stored/warmed selection may predate the series identity
		// gate. Do not reuse a same-named reboot for this show's episode.
		if s.prefer == res.Release.ID {
			s.prefer, s.exact = "", false
		}
		return nil, nil, false
	}
	if fromResume && res != nil && s.caps.Protocol == "vela" && isAutoVersionSelector(prefer) && !m.hasAuthoritativeAutoChoice(s, res.Release.ID) {
		copy := *res
		copy.AutoSelectionReason = core.AutoResumeSource
		res = &copy
	}
	return res, cached, fromResume
}

// openInitial finds and opens the first release of a session: the stored
// resume release when it was resolved for the same profile, else the
// resolver's answer, moving to the next candidate when a release fails.
func (m *Manager) openInitial(ctx context.Context, s *session, profile core.QualityProfile) (*generation, error) {
	original := ""
	if s.title != nil {
		original = s.title.Language
	}
	isSlot := core.IsExplicitVersionSlotSelector(s.prefer)
	auto := !s.exact && isAutoVersionSelector(s.prefer)
	if pref, _ := m.preferredRelease(s); true {
		s.mu.Lock()
		if s.preference != pref {
			s.preference, s.preferenceFailed = pref, false
		}
		s.mu.Unlock()
	}
	res, cached, fromResume := m.preparedResolution(ctx, s, profile, original, s.prefer)
	if res == nil && s.prefer != "" && !s.exact && !isSlot {
		if auto, facts, remembered := m.preparedResolution(ctx, s, profile, original, ""); auto != nil {
			// Resolve only the explicit pick. Resolving a fresh automatic
			// fallback would health-check a source we then discard in favour
			// of the already checked remembered source below.
			pick, err := m.deps.Resolver.Resolve(core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback), core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user, Prefer: s.prefer, PreferredOnly: true, PlayedRelease: auto.Release.ID})
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err == nil && pick != nil && pick.MatchesVersion(s.prefer) {
				res = pick
			} else if err == nil || errors.Is(err, core.ErrNotAvailable) {
				res, cached, fromResume = auto, facts, remembered
				reason := "picked version failed availability checks"
				if err != nil {
					reason = err.Error()
				}
				m.notePreferenceStale(ctx, s, auto.Release.ID, reason)
			} else {
				return nil, err
			}
		}
	}
	var (
		tried              []string
		audioRejected      []core.Release
		alternates         []core.Release
		lastErr            error
		fallbackGen        *generation
		fallbackBurned     bool
		wrongContentSkips  int
		unknownAudioProbes int
		// allUncached: every release that failed to open was a torrent
		// no debrid account could serve (uncached, blocked, add refused).
		allUncached = true
	)
	defer func() {
		if fallbackGen != nil {
			m.dropGeneration(fallbackGen)
		}
	}()
	rt := newRuntimeTracker(func(g *generation, res *core.Resolution) bool {
		info := g.infoSnapshot()
		if s.offlineOriginal == nil && m.videoOff(s.caps) {
			if _, needs := m.probedNeedsVideoTranscode(info, s.caps); needs {
				return false
			}
		}
		if s.caps.BroadcastBitrateKbps > 0 {
			file := res.Release
			file.Size, file.Info.FullSeason = info.Size, false
			if video := info.Video(); video != nil && video.Height > file.Info.Height {
				file.Info.Height = video.Height
			}
			if !m.channelSourceAllowed(s, file) {
				return false
			}
		}
		if m.knownHardSubbed(ctx, g) && m.hardSubtitleBlocked(ctx, res.Release) {
			return false
		}
		if auto && probedRequiredAudioRejection(profile, s.title, res.Release, info, false) != "" {
			return false
		}
		return true
	})
	defer rt.close(m)
	adoptInitial := func(g *generation, extraTried ...string) (*generation, error) {
		if s.offlineOriginal == nil {
			if alt := m.copySourceAtCapacity(ctx, s, profile, g, tried); alt != nil {
				m.dropGeneration(g)
				g = alt
			}
		}
		rt.commitPendingRejections(m.ctx, m, s)
		s.mu.Lock()
		s.tried = append(s.tried, tried...)
		s.tried = append(s.tried, extraTried...)
		// A session that fell back from its preferred release stays on its
		// fallback and never jumps back to the preference.
		s.notePreferenceLocked(s.preference, g.releaseID())
		s.mu.Unlock()
		if s.offlineOriginal == nil {
			m.startGenerationFills(g)
		}
		return g, nil
	}
	adoptRuntimeFallback := func(extraTried ...string) (*generation, error) {
		g, fRes, med, disagree, ok := rt.takeFallback()
		if !ok {
			return nil, nil
		}
		m.recordConsensusRuntime(m.ctx, s.title, med)
		for _, r := range disagree {
			if err := m.RejectRelease(m.ctx, s.title.Key, r); err != nil {
				m.log.Debug("persist runtime rejection failed", "key", s.title.Key, "release", r.ID, "err", err)
			}
			m.rememberOpenFailure(s, r, core.ErrReleaseRejected)
		}
		m.log.Info("probed runtime consensus overrides catalogue runtime",
			"session", s.id, "release", fRes.Release.Name, "consensus", med, "catalogue", s.title.Runtime, "fallback", true)
		return adoptInitial(g, extraTried...)
	}
	fallback := func(size int64) core.MediaInfo {
		info := EstimateInfo(s.title, s.caps, profile)
		info.Size = size
		return info
	}
	if res != nil {
		msg := "Reused prepared release " + releaseLabel(res.Release)
		if fromResume {
			msg = "Reused remembered release " + releaseLabel(res.Release)
		}
		m.recordTimeline(s, TimelineEvent{
			Kind:    "resolve",
			Level:   "info",
			Message: msg,
			Detail:  timelineCandidateDetail(res.Release, len(res.Alternates)),
		})
	}
	openCtx := withDeferredGenerationFills(ctx)
	if !s.exact {
		openCtx = withRuntimeTracker(openCtx, rt)
	}
	const maxMoreCandidatePasses = 8
	morePasses := 0
	for attempt := 0; m.attemptAllowed(attempt) && ctx.Err() == nil; attempt++ {
		if res == nil && attempt > 0 && s.offlineOriginal == nil && !s.exact && (s.prefer == "" || isSlot) && m.deps.FetchNZB != nil &&
			s.caps.BroadcastReadyRelease == "" && !s.isKind() && fallbackGen == nil && len(alternates) >= 2 {
			maxBudget := 0
			if m.opts.MaxAttempts > 0 {
				maxBudget = m.opts.MaxAttempts - attempt
			}
			out := m.raceInitialAlternates(openCtx, s, profile, tried, audioRejected, alternates, fallback, failoverRaceWidth, maxBudget)
			tried = append(tried, out.failed...)
			if isSlot {
				reason := "failed to open during picked version failover"
				if out.lastErr != nil {
					reason = out.lastErr.Error()
				}
				for _, failedID := range out.failed {
					m.noteStaleRelease(s.ident.key, failedID, s.title, "", reason)
				}
			}
			audioRejected = append(audioRejected, out.audioRejected...)
			alternates = out.remaining
			if out.g != nil {
				return adoptInitial(out.g)
			}
			if out.fallbackGen != nil {
				fallbackGen = out.fallbackGen
				fallbackBurned = out.fallbackBurned
			}
			if ctx.Err() != nil {
				break
			}
			if out.attempted > 0 {
				allUncached = allUncached && out.allUncached
				if out.lastErr != nil {
					lastErr = out.lastErr
				}
				attempt += out.attempted - 1
				continue
			}
		}
		if res == nil {
			fromResume = false
			var err error
			res, err = m.nextResolutionTracking(ctx, s, &tried, audioRejected, alternates)
			if err == nil && len(tried) == 0 && s.prefer != "" && !s.exact && !isSlot && !res.MatchesVersion(s.prefer) {
				// The picked version fell back to normal ranking. Play the
				// automatic choice the warm-up (or resume) already holds, so
				// the opening prefetch is not wasted on another release, and
				// stop asking for the pick on every open.
				if auto, autoCached, autoResume := m.preparedResolution(ctx, s, profile, original, ""); auto != nil {
					res, cached, fromResume = auto, autoCached, autoResume
				}
				m.notePreferenceStale(ctx, s, res.Release.ID, "picked version was unavailable; resolver selected Auto")
			}
			if err != nil {
				if errors.Is(err, core.ErrMoreCandidates) && ctx.Err() == nil && morePasses < maxMoreCandidatePasses {
					morePasses++
					alternates = nil
					attempt--
					continue
				}
				if !errors.Is(ctx.Err(), context.Canceled) && m.ctx.Err() == nil {
					if fallbackGen != nil {
						g := fallbackGen
						fallbackGen = nil
						return adoptInitial(g)
					}
					if g, aerr := adoptRuntimeFallback(); g != nil || aerr != nil {
						return g, aerr
					}
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				rt.commitPendingRejections(m.ctx, m, s)
				if errors.Is(lastErr, frontend.ErrTrackSourceUnavailable) {
					return nil, lastErr
				}
				if lastErr != nil && errors.Is(err, core.ErrNotAvailable) {
					if errors.Is(err, core.ErrVideoTranscodingDisabled) {
						return nil, err
					}
					var reqLang *core.RequiredAudioLanguageError
					if errors.As(err, &reqLang) {
						return nil, err
					}
					if errors.Is(lastErr, core.ErrVideoTranscodingDisabled) {
						return nil, fmt.Errorf("%w: %w", err, core.ErrVideoTranscodingDisabled)
					}
					if errors.As(lastErr, &reqLang) {
						return nil, fmt.Errorf("%w: %w", err, reqLang)
					}
					if errors.Is(lastErr, core.ErrDebridAuth) {
						return nil, fmt.Errorf("%w (last failure: %v): %w", err, lastErr, core.ErrDebridAuth)
					}
					if !errors.Is(err, core.ErrNotCached) && !errors.Is(err, core.ErrNoSources) {
						switch {
						case errors.Is(lastErr, context.DeadlineExceeded):
							return nil, fmt.Errorf("%w (last failure: %v): %w", err, lastErr, context.DeadlineExceeded)
						case errors.Is(lastErr, core.ErrSourceUnavailable), errors.Is(lastErr, core.ErrArticleMissing), errors.Is(lastErr, core.ErrArticleCorrupt), errors.Is(lastErr, core.ErrReleaseRejected):
							return nil, fmt.Errorf("%w (last failure: %v): %w", err, lastErr, core.ErrSourceUnavailable)
						}
					}
					return nil, fmt.Errorf("%w (last failure: %v)", err, lastErr)
				}
				return nil, err
			}
		}
		if s.caps.BroadcastReadyRelease != "" && cached == nil {
			cached = m.persistedProbe(ctx, s.ident.userID, s.ident.key, res.Release.ID, res.NZB)
		}
		if s.exact && !res.MatchesVersion(s.prefer) {
			return nil, fmt.Errorf("%w: %w", core.ErrNotAvailable, core.ErrSourceChanged)
		}
		s.setAlternateMeta(res.VersionSelector, res.QualityFallback)
		if len(res.NZB) > 0 {
			res.Release.SourceHash = fmt.Sprintf("%x", sha256.Sum256(res.NZB))
		}
		if m.titleReleaseRejected(ctx, s.ident.key, res.Release) || (auto && audioExcluded(audioRejected, res.Release)) {
			tried = append(tried, res.Release.ID)
			alternates = res.Alternates
			res, cached, fromResume = nil, nil, false
			continue
		}
		if m.releaseNamesakeRejected(ctx, s, res.Release) {
			// Reported identifiers name the namesake title, not this one.
			m.markBad(res.Release.ID, "release names the other title")
			tried = append(tried, res.Release.ID)
			alternates = res.Alternates
			res, cached, fromResume = nil, nil, false
			continue
		}
		if cached == nil {
			cached = m.persistedProbe(ctx, s.ident.userID, s.ident.key, res.Release.ID, res.NZB)
		}
		needsAudioProbe := false
		if !s.exact && !m.ownerChoiceCovers(s, res.Release.ID) {
			if langs, unverified := candidateNeedsAudioVerification(profile, s.title, res.Release); unverified {
				if unknownAudioProbes >= maxUnknownAudioProbes {
					if reqLang := normalizedProfileLanguage(profile); reqLang != "" && strings.EqualFold(langs[0], reqLang) {
						lastErr = &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
					} else {
						lastErr = fmt.Errorf("audio is not in the profile's language (%s)", langs[0])
					}
					allUncached = false
					tried = append(tried, res.Release.ID)
					alternates = res.Alternates
					res, cached, fromResume = nil, nil, false
					continue
				}
				unknownAudioProbes++
				needsAudioProbe = true
			}
		}
		g, err := m.openGeneration(openCtx, s, res, fallback, cached)
		if err == nil && s.offlineOriginal == nil && m.videoOff(s.caps) {
			if dec, needs := m.probedNeedsVideoTranscode(g.infoSnapshot(), s.caps); needs {
				reason := core.ErrVideoTranscodingDisabled.Error()
				if dec.Reason != "" {
					reason += " (" + dec.Reason + ")"
				}
				m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", reason)
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "info",
					Message: "Skipped candidate " + releaseLabel(res.Release),
					Detail:  reason,
				})
				m.dropGeneration(g)
				if s.exact {
					return nil, core.ErrVideoTranscodingDisabled
				}
				lastErr = core.ErrVideoTranscodingDisabled
				allUncached = false
				tried = append(tried, res.Release.ID)
				alternates = res.Alternates
				res, cached, fromResume = nil, nil, false
				continue
			}
		}
		if err == nil && fromResume && !s.exact && !rememberedInfoCodecCurrent(g.infoSnapshot(), s.caps) {
			m.dropGeneration(g)
			tried = append(tried, res.Release.ID)
			res, cached, fromResume = nil, nil, false
			continue
		}
		burned := false
		if err == nil {
			burned = m.knownHardSubbed(ctx, g)
		}
		if err == nil && burned && m.hardSubtitleBlocked(ctx, res.Release) {
			m.dropGeneration(g)
			tried = append(tried, res.Release.ID)
			alternates = res.Alternates
			res, cached, fromResume = nil, nil, false
			continue
		}
		if err == nil && auto {
			info := g.infoSnapshot()
			if reqReason := probedRequiredAudioRejection(profile, s.title, res.Release, info, fromResume); reqReason != "" {
				m.noteSelectionProbe(s.ident.key, res, info)
				m.log.Info("trying next candidate", "release", res.Release.Name, "reason", reqReason)
				m.dropGeneration(g)
				if reqLang := normalizedProfileLanguage(profile); reqLang != "" && strings.HasPrefix(reqReason, "audio is not in the profile's language (") {
					lastErr = &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
				} else {
					lastErr = errors.New(reqReason)
				}
				allUncached = false
				if needsAudioProbe && unknownAudioProbes < maxUnknownAudioProbes {
					attempt--
				}
				tried = append(tried, res.Release.ID)
				audioRejected = append(audioRejected, res.Release)
				alternates = res.Alternates
				res, cached, fromResume = nil, nil, false
				continue
			}
		}
		if err == nil && auto {
			info := g.infoSnapshot()
			audioOK := m.probedAudioAcceptable(s, profile, info)
			if !audioOK && m.hasAuthoritativeAutoChoice(s, res.Release.ID) && m.ownerPinAllows(s, res.Release.ID) && m.rememberedChoiceAllows(s, res.Release.ID) {
				audioOK = true
			}
			if burned || !audioOK {
				reason := "probed release lacks viewer audio language"
				if burned {
					reason = "burned-in subtitles detected"
				} else {
					m.noteSelectionProbe(s.ident.key, res, info)
				}
				m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", reason)
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "info",
					Message: "Skipped candidate " + releaseLabel(res.Release),
					Detail:  reason,
				})
				if fallbackGen == nil || fallbackBurned && !burned {
					if fallbackGen != nil {
						m.dropGeneration(fallbackGen)
					}
					fallbackGen = g
					fallbackBurned = burned
				} else {
					m.dropGeneration(g)
				}
				allUncached = false
				tried = append(tried, res.Release.ID)
				if !audioOK {
					audioRejected = append(audioRejected, res.Release)
				}
				alternates = res.Alternates
				res, cached, fromResume = nil, nil, false
				continue
			}
		}
		if err == nil {
			return adoptInitial(g)
		}
		if ctx.Err() != nil {
			if !s.exact && errors.Is(ctx.Err(), context.DeadlineExceeded) && m.ctx.Err() == nil {
				m.log.Warn("release failed to open", "session", s.id, "release", res.Release.Name, "err", err)
				if fallbackGen != nil {
					g := fallbackGen
					fallbackGen = nil
					return adoptInitial(g, res.Release.ID)
				}
				if g, aerr := adoptRuntimeFallback(res.Release.ID); g != nil || aerr != nil {
					return g, aerr
				}
				rt.commitPendingRejections(m.ctx, m, s)
			}
			return nil, ctx.Err()
		}
		lastErr = err
		if s.offlineOriginal != nil && errors.Is(err, frontend.ErrVersionUnavailable) {
			if s.exact {
				return nil, err
			}
			allUncached = false
			tried = append(tried, res.Release.ID)
			alternates = res.Alternates
			res, cached, fromResume = nil, nil, false
			continue
		}
		// Bound release-level skips separately from actual playback failures.
		if errors.Is(err, core.ErrWrongContent) && wrongContentSkips < 8 {
			wrongContentSkips++
			attempt--
		}
		if s.exact {
			if errors.Is(err, frontend.ErrTrackSourceUnavailable) {
				return nil, err
			}
			if errors.Is(err, core.ErrNotFound) {
				return nil, fmt.Errorf("%w: %w", core.ErrNotAvailable, core.ErrSourceChanged)
			}
			return nil, fmt.Errorf("%w: selected release could not be opened", core.ErrNotAvailable)
		}
		allUncached = allUncached && errors.Is(err, core.ErrNotCached)
		m.log.Warn("release failed to open", "session", s.id, "release", res.Release.Name, "err", err)
		if isSlot {
			m.noteStaleRelease(s.ident.key, res.Release.ID, s.title, "", err.Error())
		}
		tried = append(tried, res.Release.ID)
		alternates = res.Alternates
		res, cached, fromResume = nil, nil, false
	}
	if ctx.Err() != nil {
		if !s.exact && errors.Is(ctx.Err(), context.DeadlineExceeded) && m.ctx.Err() == nil {
			if fallbackGen != nil {
				g := fallbackGen
				fallbackGen = nil
				return adoptInitial(g)
			}
			if g, aerr := adoptRuntimeFallback(); g != nil || aerr != nil {
				return g, aerr
			}
			rt.commitPendingRejections(m.ctx, m, s)
		}
		return nil, ctx.Err()
	}
	if !errors.Is(ctx.Err(), context.Canceled) && m.ctx.Err() == nil {
		if fallbackGen != nil {
			g := fallbackGen
			fallbackGen = nil
			return adoptInitial(g)
		}
		if g, aerr := adoptRuntimeFallback(); g != nil || aerr != nil {
			return g, aerr
		}
		rt.commitPendingRejections(m.ctx, m, s)
	}
	return nil, releasesFailed(len(tried), lastErr, allUncached)
}

// releasesFailed is the error after every attempted release failed to open.
// It always wraps core.ErrNotAvailable and keeps the owner-facing cause
// classifiable: a debrid service rejecting its API key (a configuration
// problem) or every release uncached on the debrid accounts. Other release
// errors stay text only, so release-level sentinels (ErrNotFound and the
// like) cannot change how the failure is reported to the client.
func releasesFailed(n int, lastErr error, allUncached bool) error {
	var reqLang *core.RequiredAudioLanguageError
	switch {
	case errors.Is(lastErr, core.ErrVideoTranscodingDisabled):
		return fmt.Errorf("%d releases failed: %w", n, lastErr)
	case errors.As(lastErr, &reqLang):
		return fmt.Errorf("%d releases failed: %w", n, lastErr)
	case errors.Is(lastErr, frontend.ErrTrackSourceUnavailable):
		return fmt.Errorf("%d releases failed: %w", n, lastErr)
	case errors.Is(lastErr, core.ErrDebridAuth):
		return fmt.Errorf("%w: %d releases failed (last: %v): %w", core.ErrNotAvailable, n, lastErr, core.ErrDebridAuth)
	case allUncached && n > 0:
		// Keep the typed last cause, including queued/refused/limit and
		// UncachedError's settings, for the protocol adapter to explain.
		return fmt.Errorf("%d releases failed: %w", n, lastErr)
	case errors.Is(lastErr, context.DeadlineExceeded):
		return fmt.Errorf("%w: %d releases failed (last: %v): %w", core.ErrNotAvailable, n, lastErr, context.DeadlineExceeded)
	case errors.Is(lastErr, core.ErrSourceUnavailable), errors.Is(lastErr, core.ErrArticleMissing), errors.Is(lastErr, core.ErrArticleCorrupt), errors.Is(lastErr, core.ErrReleaseRejected):
		return fmt.Errorf("%w: %d releases failed (last: %v): %w", core.ErrNotAvailable, n, lastErr, core.ErrSourceUnavailable)
	}
	return fmt.Errorf("%w: %d releases failed (last: %v)", core.ErrNotAvailable, n, lastErr)
}

// nextResolution returns the next release to try: an untried alternate when
// NZBs can be fetched directly, else a fresh Resolve excluding tried ones.
func (m *Manager) nextResolution(ctx context.Context, s *session, tried []string, alternates []core.Release) (*core.Resolution, error) {
	return m.nextResolutionTracking(ctx, s, &tried, nil, alternates)
}

// nextResolutionTracking returns the next release to try and appends any
// alternates whose FetchNZB fails into *tried so subsequent Resolve calls
// exclude them instead of retrying dead links.
func (m *Manager) nextResolutionTracking(ctx context.Context, s *session, tried *[]string, audioRejected []core.Release, alternates []core.Release) (*core.Resolution, error) {
	ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposePlayback)
	exclude := append(slices.Clone(*tried), s.caps.BroadcastExclude...)
	isSlot := core.IsExplicitVersionSlotSelector(s.prefer)
	m.mu.Lock()
	for id := range m.openFailures[s.ident.key] {
		if !slices.Contains(exclude, id) {
			exclude = append(exclude, id)
		}
	}
	if isSlot {
		for _, id := range m.staleReleasesLocked(s.ident.key, m.now()) {
			if !slices.Contains(exclude, id) {
				exclude = append(exclude, id)
			}
		}
	}
	m.mu.Unlock()
	if m.deps.FetchNZB != nil {
		altSel, altFallback := s.alternateMeta()
		// Fetches run sequentially. A timed-out on-demand fetch has returned
		// and cancelled its request, so the next eligible source may open.
		var fetchErr error
		fetched := 0
		gate := m.preferenceGate(s, exclude)
		for i, alt := range alternates {
			if alt.ID == "" || !m.ownerPinAllows(s, alt.ID) || gate.blocks(alt.ID) || !m.channelSourceAllowed(s, alt) || m.titleReleaseRejected(ctx, s.ident.key, alt) || m.releaseNamesakeRejected(ctx, s, alt) || slices.Contains(exclude, alt.ID) || audioExcluded(audioRejected, alt) || !releaseMatchesTitle(alt, s.title) {
				continue
			}
			if m.opts.MaxAttempts > 0 && fetched >= m.opts.MaxAttempts {
				return nil, fetchErr
			}
			fetched++
			nzb, err := m.deps.FetchNZB(ctx, alt)
			if err != nil {
				if ctx.Err() != nil {
					if errors.Is(ctx.Err(), context.DeadlineExceeded) && m.ctx.Err() == nil {
						m.log.Warn("alternate nzb fetch failed", "session", s.id, "release", alt.Name, "err", err)
					}
					return nil, ctx.Err()
				}
				m.log.Warn("alternate nzb fetch failed", "session", s.id, "release", alt.Name, "err", err)
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "warn",
					Failing: true,
					Message: "Candidate failed: " + releaseLabel(alt),
					Detail:  OwnerError(err),
				})
				fetchErr = err
				if isSlot {
					m.noteStaleRelease(s.ident.key, alt.ID, s.title, "", err.Error())
				}
				// A fresh Resolve must not retry a link that just failed.
				exclude = append(exclude, alt.ID)
				if !slices.Contains(*tried, alt.ID) {
					*tried = append(*tried, alt.ID)
				}
				continue
			}
			m.recordTimeline(s, TimelineEvent{
				Kind:    "resolve",
				Level:   "info",
				Message: "Selected alternate " + releaseLabel(alt),
				Detail:  timelineCandidateDetail(alt, len(alternates)-i-1),
			})
			return &core.Resolution{
				VersionSelector: altSel,
				QualityFallback: altFallback,
				Title:           s.ident.key,
				Release:         alt,
				NZB:             nzb,
				Alternates:      slices.Clone(alternates[i+1:]),
				ResolvedAt:      m.now(),
			}, nil
		}
		if fetchErr != nil && m.opts.MaxAttempts > 0 && fetched >= m.opts.MaxAttempts {
			return nil, fetchErr
		}
	}
	u := m.effectiveResolveUser(ctx, s.user)
	if s.caps.BroadcastReadyRelease != "" {
		ready, ok := m.deps.Resolver.(interface {
			ResolveReadyChannel(context.Context, core.ResolveRequest) (*core.Resolution, error)
		})
		if !ok {
			return nil, core.ErrNotAvailable
		}
		req := core.ResolveRequest{Title: s.title, Client: s.caps, User: &u, Exclude: exclude, ExcludeReleases: slices.Clone(audioRejected), Prefer: s.caps.BroadcastReadyRelease}
		// A freshly verified source may already have its exact payload on
		// disk. Revalidate evidence, then avoid another indexer/provider grab.
		if evidence, ok := m.deps.Resolver.(interface {
			ReadyChannelRelease(context.Context, core.ResolveRequest) (core.Release, bool)
		}); ok && m.resume != nil {
			if r, live := evidence.ReadyChannelRelease(ctx, req); live && r.ID == req.Prefer {
				if rec, err := m.resume.get(ctx, u.ID, s.ident.key); err == nil && rec != nil && rec.releaseID == r.ID {
					if payload, err := m.resume.readResumeNZB(ctx, rec); err == nil {
						return &core.Resolution{Title: s.ident.key, Release: r, NZB: payload, ResolvedAt: m.now()}, nil
					}
				}
			}
		}
		return ready.ResolveReadyChannel(ctx, req)
	}
	res, err := m.deps.Resolver.Resolve(ctx, core.ResolveRequest{
		Title:              s.title,
		Client:             s.caps,
		User:               &u,
		Exclude:            slices.Clone(exclude),
		ExcludeReleases:    slices.Clone(audioRejected),
		Prefer:             s.prefer,
		ExactHEVCTranscode: s.exact && s.ident.surface != "" && allowsH264Transcode(s.caps),
	})
	if err != nil {
		if ctx.Err() == nil {
			m.recordTimeline(s, TimelineEvent{
				Kind:    "resolve",
				Level:   "warn",
				Failing: true,
				Message: "Release search failed",
				Detail:  OwnerError(err),
			})
		}
		return nil, err
	}
	if res == nil || res.Release.ID == "" || len(res.NZB) == 0 {
		return nil, fmt.Errorf("%w: resolver returned no usable release", core.ErrNotAvailable)
	}
	if id := res.PreferenceFailed; id != "" && !slices.Contains(*tried, id) {
		// The preferred release could not be used: later sequential and
		// raced attempts of this session must not return to it.
		*tried = append(*tried, id)
	}
	if isSlot && len(res.FailedReleases) > 0 {
		for _, failedID := range res.FailedReleases {
			m.noteStaleRelease(s.ident.key, failedID, s.title, res.Release.ID, "failed validation during picked version failover")
		}
	}
	if slices.Contains(exclude, res.Release.ID) {
		return nil, fmt.Errorf("%w: resolver returned an excluded release", core.ErrNotAvailable)
	}
	m.recordTimeline(s, TimelineEvent{
		Kind:    "resolve",
		Level:   "info",
		Message: "Resolved " + releaseLabel(res.Release),
		Detail:  timelineCandidateDetail(res.Release, len(res.Alternates)),
	})
	return res, nil
}

// This is only permission to inspect the selected source. The broker's
// decision still checks output delivery, audio, conditions and capacity.
func allowsH264Transcode(c core.ClientCaps) bool {
	if c.Download || c.Profiles == nil {
		return false
	}
	for _, p := range c.Profiles.Transcoding {
		if !strings.EqualFold(p.MediaType, "video") {
			continue
		}
		for _, codec := range p.VideoCodecs {
			if strings.EqualFold(codec, "h264") {
				return true
			}
		}
	}
	return false
}

// openGeneration opens a release through the engine, exposes it on the
// loopback server and probes it. A release whose articles are missing is
// marked bad. When the probe fails for another reason, fallback supplies the
// media info.
func (m *Manager) openGeneration(ctx context.Context, s *session, res *core.Resolution, fallback func(size int64) core.MediaInfo, cached ...*probeCache) (*generation, error) {
	if !m.ownerPinAllows(s, res.Release.ID) {
		return nil, fmt.Errorf("%w: owner pinned a different release", core.ErrNotAvailable)
	}
	if slices.Contains(s.caps.BroadcastExclude, res.Release.ID) || !m.channelSourceAllowed(s, res.Release) {
		return nil, fmt.Errorf("%w: source exceeds channel delivery budget", core.ErrNotAvailable)
	}
	if m.titleReleaseRejected(ctx, s.ident.key, res.Release) {
		return nil, core.ErrReleaseRejected
	}
	// Recheck at the shared open boundary, including failover and old
	// cached alternates. A mismatch is request-specific, not a bad release.
	if !releaseMatchesTitle(res.Release, s.title) || !payloadMatchesTitle(res.NZB, s.title) {
		return nil, fmt.Errorf("%w: release does not match requested episode", core.ErrWrongContent)
	}
	openStarted := time.Now()
	src, finish, err := m.openSource(ctx, s.sources, res)
	m.log.Debug("playback candidate open", "session", s.id, "release", res.Release.ID, "duration", time.Since(openStarted), "ok", err == nil)
	if err != nil {
		m.rememberOpenFailure(s, res.Release, err)
		if permanentSourceFailure(err) {
			m.markBad(res.Release.ID, "invalid source on open")
		}
		openErr := fmt.Errorf("open %s: %w", res.Release.Name, err)
		if ctx.Err() == nil {
			m.recordTimeline(s, TimelineEvent{
				Kind:    "candidate",
				Level:   "warn",
				Failing: true,
				Message: "Candidate failed: " + releaseLabel(res.Release),
				Detail:  OwnerError(openErr),
			})
		}
		return nil, openErr
	}
	if src.Size() <= 0 {
		finish(src.Close())
		openErr := fmt.Errorf("open %s: empty media file", res.Release.Name)
		if ctx.Err() == nil {
			m.recordTimeline(s, TimelineEvent{
				Kind:    "candidate",
				Level:   "warn",
				Failing: true,
				Message: "Candidate failed: " + releaseLabel(res.Release),
				Detail:  OwnerError(openErr),
			})
		}
		return nil, openErr
	}
	g, err := m.probeGeneration(ctx, s, res, src, finish, fallback, cached...)
	if err == nil && g != nil && s.caps.BroadcastBitrateKbps > 0 {
		// Resolve may only know a pack's size. Admit the selected file using
		// its actual byte size, never the aggregate pack or an unknown size.
		file := res.Release
		file.Size, file.Info.FullSeason = src.Size(), false
		if video := g.infoSnapshot().Video(); video != nil && video.Height > file.Info.Height {
			file.Info.Height = video.Height
		}
		if !m.channelSourceAllowed(s, file) {
			m.dropGeneration(g)
			budgetErr := fmt.Errorf("%w: selected file exceeds channel delivery budget", core.ErrNotAvailable)
			if ctx.Err() == nil {
				m.recordTimeline(s, TimelineEvent{
					Kind:    "candidate",
					Level:   "warn",
					Failing: true,
					Message: "Candidate failed: " + releaseLabel(res.Release),
					Detail:  OwnerError(budgetErr),
				})
			}
			return nil, budgetErr
		}
	}
	if !errors.Is(err, errRuntimeMismatchDeferred) && !errors.Is(err, frontend.ErrVersionUnavailable) {
		m.rememberOpenFailure(s, res.Release, err)
	}
	if err != nil {
		if ctx.Err() == nil {
			m.recordTimeline(s, TimelineEvent{
				Kind:    "candidate",
				Level:   "warn",
				Failing: true,
				Message: "Candidate failed: " + releaseLabel(res.Release),
				Detail:  OwnerError(err),
			})
		}
		return nil, err
	}
	if g != nil {
		m.recordTimeline(s, TimelineEvent{
			Kind:    "candidate",
			Level:   "info",
			Message: "Opened candidate " + releaseLabel(res.Release),
			Detail:  timelineOpenedDetail(g.infoSnapshot(), res.Release, src.Size()),
		})
	}
	return g, nil
}

func (m *Manager) probeGeneration(ctx context.Context, s *session, res *core.Resolution, src core.MediaSource, finish func(error), fallback func(int64) core.MediaInfo, cached ...*probeCache) (*generation, error) {
	inner, closeShared := unwrapShared(src)
	// Probes, revisions and optional source interfaces must see the backend,
	// not the refcount wrapper. Close stays on the wrapper.
	share := src
	src = inner
	if isNonAVDocumentSource(src) {
		if finish != nil {
			finish(core.ErrNoUsableFile)
		}
		if closeShared != nil {
			_ = closeShared()
		} else {
			_ = share.Close()
		}
		return nil, core.ErrNoUsableFile
	}
	openedRevision := core.SourceRevision(&core.PlaySession{Key: s.ident.key, Source: src})
	g := &generation{warmReads: new(core.WarmReadWindow), sess: s, n: s.nextGen(), token: randomID(16), src: src, share: share, sharedClose: closeShared, finishSource: finish, res: res, indexReady: make(chan struct{})}
	if len(cached) > 0 && cached[0] != nil && cached[0].warmReads != nil &&
		cached[0].releaseID == res.Release.ID && cached[0].info.Size == src.Size() &&
		(cached[0].nzbHash == "" || (len(res.NZB) > 0 && cached[0].nzbHash == nzbHash(res.NZB))) {
		g.prewarmReads = cached[0].warmReads
	}
	g.ctx, g.cancel = context.WithCancel(m.ctx)
	if core.IsBackgroundAnalysis(ctx) {
		g.ctx = core.WithBackgroundAnalysis(g.ctx)
	}
	m.mu.Lock()
	m.tokens[s.id+"/"+g.token] = g
	m.mu.Unlock()

	probeStarted := time.Now()
	pctx, cancel := context.WithTimeout(ctx, m.opts.ProbeTimeout)
	var info *core.MediaInfo
	var perr error
	var facts *probeCache
	prober := "cache"
	if len(cached) > 0 && cached[0] != nil && cached[0].releaseID == res.Release.ID && cached[0].info.Size == src.Size() && !cached[0].info.Estimated &&
		(s.caps.Protocol != "vela" || (cached[0].info.Duration > 0 && cached[0].info.HasPlayableMedia())) &&
		(cached[0].nzbHash == "" || (len(res.NZB) > 0 && cached[0].nzbHash == nzbHash(res.NZB))) {
		facts = cached[0]
		copy := facts.info
		info = &copy
	} else {
		info, prober, perr = m.probeMedia(pctx, g, src)
	}
	cancel()
	probeDuration := time.Since(probeStarted)
	var mi core.MediaInfo
	switch {
	case g.sourceFailure() != nil || g.articleMissing.Load():
		failure := g.sourceFailure()
		if failure == nil {
			failure = errSourceBroken
		}
		m.dropGeneration(g)
		if permanentSourceFailure(failure) {
			m.markBad(res.Release.ID, "invalid source while probing")
		}
		return nil, failure
	case perr != nil && ctx.Err() != nil:
		m.dropGeneration(g)
		return nil, ctx.Err()
	case prober == "native" && permanentSourceFailure(perr):
		// The native probe read the source directly, so the loopback
		// server never saw the failure.
		m.dropGeneration(g)
		m.markBad(res.Release.ID, "invalid source while probing")
		return nil, perr
	case perr != nil || info == nil:
		m.log.Warn("probe failed; using estimated media info", "session", s.id, "release", res.Release.Name, "err", perr)
		mi = core.CloneMediaInfo(fallback(src.Size()))
		mi.Estimated = true
		// An estimated catalogue runtime is not a container duration. A
		// guessed end can make part of a longer release unseekable.
		mi.Duration = 0
	default:
		mi = core.CloneMediaInfo(*info)
	}
	// Vela publishes stable source track identities in its initial plan. A
	// failed/incomplete probe cannot supply those identities. Let the bounded
	// candidate loop try another release, without blacklisting this one.
	if s.caps.Protocol == "vela" && (mi.Estimated || mi.Duration <= 0 || !mi.HasPlayableMedia() || len(mi.Streams) == 0) {
		m.log.Warn("source lacks verified playback facts", "session", s.id, "release", res.Release.Name, "sourceSize", src.Size(), "releaseSize", res.Release.Size, "duration", mi.Duration, "estimated", mi.Estimated, "probeError", perr)
		m.dropGeneration(g)
		return nil, fmt.Errorf("probe %s: %w", res.Release.Name, frontend.ErrTrackSourceUnavailable)
	}
	if s.offlineOriginal != nil && !s.offlineOriginal(mi) {
		m.dropGeneration(g)
		return nil, frontend.ErrVersionUnavailable
	}
	// Reused persisted probes pass through here too, including a remembered
	// playback_resume release played from zero with no requested offset.
	rt := runtimeTrackerFrom(ctx)
	effTitle := m.titleWithEffectiveRuntime(s.title)
	holdRuntimeFallback := false
	if !mi.Estimated && effTitle != nil {
		mismatch := releaseRuntimeMismatch(effTitle, res.Release, mi.Duration) ||
			(m.namesakeRequest(ctx, effTitle) && effTitle.Type != core.TypeEpisode &&
				namesakeDurationMismatch(effTitle.Runtime, mi.Duration))
		if !mismatch {
			if rt != nil && mi.Duration > 0 {
				rt.observeMatch(res.Release, mi.Duration)
			}
		} else if rt != nil {
			decision, consensusDur, disagree := rt.observeMismatch(res.Release, mi.Duration, runtimeConsensusEligible(effTitle, res.Release, mi.Duration))
			switch decision {
			case runtimeAcceptLive:
				m.recordConsensusRuntime(ctx, s.title, consensusDur)
				for _, r := range disagree {
					if err := m.RejectRelease(ctx, s.title.Key, r); err != nil {
						m.log.Debug("persist runtime rejection failed", "key", s.title.Key, "release", r.ID, "err", err)
					}
					m.rememberOpenFailure(s, r, core.ErrReleaseRejected)
				}
				m.log.Info("probed runtime consensus overrides catalogue runtime",
					"session", s.id, "release", res.Release.Name, "consensus", consensusDur, "catalogue", effTitle.Runtime)
			case runtimeHoldFallback:
				holdRuntimeFallback = true
			case runtimeDefer:
				m.dropGeneration(g)
				m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", "runtime doesn't match this title",
					"probed", mi.Duration, "catalogue", effTitle.Runtime)
				return nil, fmt.Errorf("probe %s: %w: runtime doesn't match this title", res.Release.Name, errRuntimeMismatchDeferred)
			default:
				m.dropGeneration(g)
				if err := m.RejectRelease(ctx, s.title.Key, res.Release); err != nil {
					return nil, fmt.Errorf("remember runtime rejection: %w", err)
				}
				m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", "runtime doesn't match this title",
					"probed", mi.Duration, "catalogue", effTitle.Runtime)
				return nil, fmt.Errorf("probe %s: %w: runtime doesn't match this title", res.Release.Name, core.ErrReleaseRejected)
			}
		} else {
			m.dropGeneration(g)
			if err := m.RejectRelease(ctx, s.title.Key, res.Release); err != nil {
				return nil, fmt.Errorf("remember runtime rejection: %w", err)
			}
			m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", "runtime doesn't match this title",
				"probed", mi.Duration, "catalogue", effTitle.Runtime)
			return nil, fmt.Errorf("probe %s: %w: runtime doesn't match this title", res.Release.Name, core.ErrReleaseRejected)
		}
	}
	if size := src.Size(); size > 0 {
		mi.FileSize = size
		// Keep the probe's size for release matching and version selection.
		if mi.Size <= 0 {
			mi.Size = size
		}
	}
	if mi.Container == "" {
		mi.Container = containerFromName(src.Name())
	}
	correctReversedSubtitleDispositions(mi.Streams)
	// A cached warm probe was already augmented when it was made; running
	// AugmentMedia again would put its cost back on the Play path. Facts
	// rebuilt from the persisted probe had their per-viewer sidecars
	// stripped before they reached disk, so they augment again.
	augmentStarted := time.Now()
	if s.offlineOriginal == nil && (facts == nil || facts.needsAugment) && m.deps.AugmentMedia != nil {
		m.deps.AugmentMedia(ctx, s.title, &s.user, res, src, &mi)
	}
	m.log.Debug("media preparation completed", "session", s.id, "release", res.Release.ID,
		"probe", probeDuration, "probeCached", facts != nil, "prober", prober, "estimated", mi.Estimated,
		"augment", time.Since(augmentStarted))
	var points []IndexPoint
	if facts != nil {
		points = facts.points
		if len(points) == 0 {
			points = indexPointsOf(facts.info.Keyframes)
		}
	} else if len(mi.Keyframes) > 0 {
		points = indexPointsOf(mi.Keyframes)
	}
	mi.Keyframes = keyframesOf(points)
	needIndex := len(points) == 0 && !mi.Estimated && mi.Duration > 0
	// A historical open observation is never fresh authorization. Validation
	// belongs to source opening/probing, outside manager/session locks; a path
	// replacement or unsupported identity leaves all source facts unknown.
	if openedRevision != core.SourceRevision(&core.PlaySession{Key: s.ident.key, Source: src}) {
		openedRevision = ""
	}
	observation := core.PlaybackSourceObservation{}
	if openedRevision != "" {
		observation.Revision, observation.ReleaseID = openedRevision, res.Release.ID
		if !mi.Estimated {
			if video := mi.Video(); video != nil {
				observation.VideoCodec, observation.Height = video.Codec, video.Height
				observation.HDR = observedHDR(*video)
			}
		}
	}
	// The loopback server may already hold g; publish under its lock.
	g.publishInfo(mi, observation, TimeMap{Size: src.Size(), Duration: mi.Duration, Points: points})
	// The running time turns the source's playback read-ahead from minutes
	// into bytes; it starts only once a reader actually plays.
	if ps, ok := src.(core.PlaybackDurationSetter); ok && mi.Duration > 0 {
		ps.SetPlaybackDuration(mi.Duration)
	}
	g.mu.Lock()
	g.needIndex = needIndex
	g.needTrailer = prober == "native" || (facts != nil && facts.needsAugment)
	g.mu.Unlock()
	if holdRuntimeFallback {
		if !rt.tryStashFallback(g, res, mi.Duration) {
			m.dropGeneration(g)
		}
		m.log.Info("trying next candidate", "session", s.id, "release", res.Release.Name, "reason", "runtime doesn't match this title",
			"probed", mi.Duration, "catalogue", effTitle.Runtime)
		return nil, fmt.Errorf("probe %s: %w: runtime doesn't match this title", res.Release.Name, errRuntimeMismatchDeferred)
	}
	if !generationFillsDeferred(ctx) {
		m.startGenerationFills(g)
	}
	return g, nil
}

// startGenerationFills starts the deferred keyframe-index and trailer fills
// once a probed generation has been chosen rather than set aside as a
// candidate-selection fallback.
func (m *Manager) startGenerationFills(g *generation) {
	if g == nil {
		return
	}
	g.mu.Lock()
	needIndex, needTrailer := g.needIndex, g.needTrailer
	g.needIndex, g.needTrailer = false, false
	g.mu.Unlock()
	if needIndex {
		// Play never waits for the container index: publish now and fill
		// TimeMap.Points / MediaInfo.Keyframes in the background.
		m.startIndexFill(g)
	} else {
		g.markIndexReady()
	}
	if needTrailer {
		// Persisted facts predate any tail fill; it only fills facts
		// that are still unknown, so a running decision never changes.
		m.startTrailerFill(g)
	}
}

// probeMedia reads stream parameters from the container headers when the
// native prober can vouch for them, and otherwise runs ffprobe through the
// loopback URL. A native probe costs no process and, for a validated Usenet
// source, usually no network read: the first article is already cached.
func (m *Manager) probeMedia(ctx context.Context, g *generation, src core.MediaSource) (*core.MediaInfo, string, error) {
	if g.res != nil && g.res.Release.IsAddonHTTPStream() {
		// The source is already open. Read its track table before optional
		// Matroska statistics near the tail or a process-based probe.
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := transcode.ProbeHeader(core.WithReadOrigin(probeCtx, "playback.probe", "container headers"), src, src.Size(), transcode.DefaultHeaderProbeBudget)
		cancel()
		if err == nil || permanentSourceFailure(err) {
			return info, "native", err
		}
	}
	if m.deps.NativeProbe != nil {
		started := time.Now()
		info, err := m.deps.NativeProbe(core.WithReadOrigin(ctx, "playback.probe", "container facts"), src)
		if err == nil && info != nil {
			if m.opts.VerifyNativeProbe && g.res != nil {
				m.startProbeVerify(g.sess.sources, g.res, *info)
			}
			return info, "native", nil
		}
		if ctx.Err() != nil || permanentSourceFailure(err) {
			return nil, "native", err
		}
		m.log.Debug("native probe deferred to ffprobe", "release", g.releaseID(), "took", time.Since(started), "err", err)
	}
	info, err := m.deps.Transcoder.Probe(ctx, m.urlFor(g))
	return info, "ffprobe", err
}

// startTrailerFill reads Matroska facts kept after the media (tail
// chapters, per-track statistics) off the Play path and merges only
// facts that were unknown, so a running decision never changes.
func (m *Manager) startTrailerFill(g *generation) {
	if core.IsBackgroundAnalysis(g.ctx) {
		return
	}
	if m.deps.NativeTrailer == nil || !g.acquire() {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.release()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		// Release can block in a backend Close. Leave the manager wait group
		// first so shutdown is not pinned behind that Close.
		defer g.release()
		defer m.wg.Done()
		// The trailer usually sits next to the Cues the index fill reads;
		// waiting lets it share that fetch instead of competing with it.
		select {
		case <-g.indexReady:
		case <-g.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(g.ctx, m.opts.IndexTimeout)
		defer cancel()
		info := g.infoSnapshot()
		if !m.deps.NativeTrailer(core.WithReadOrigin(ctx, "playback.trailer", "container facts"), g.src, &info) {
			return
		}
		g.mu.Lock()
		chapterRead := info.ChaptersProbed && !g.info.ChaptersProbed
		if chapterRead {
			g.info.Chapters = slices.Clone(info.Chapters)
			g.info.ChaptersProbed = true
		}
		streams := slices.Clone(g.info.Streams)
		for i := range streams {
			if streams[i].BitrateKbps == 0 && i < len(info.Streams) && info.Streams[i].Index == streams[i].Index {
				streams[i].BitrateKbps = info.Streams[i].BitrateKbps
			}
		}
		g.info.Streams = streams
		updated := core.CloneMediaInfo(g.info)
		g.mu.Unlock()
		if (chapterRead || len(updated.Chapters) > 0) && g.res != nil {
			m.persistReleaseProbe(ctx, g.res.Release, g.res.NZB, updated)
			if g.res.Title.IsPluginKey() {
				m.persistItemProbe(ctx, g.res.Title, updated)
			}
		}
	}()
}

// ProbeChapters resolves deferred chapter metadata for a client that is
// asking for the chapter timeline now. Ordinary Play never waits for it.
func (m *Manager) ProbeChapters(ctx context.Context, id, expectedRevision string) ([]core.Chapter, bool) {
	s := m.lookup(id)
	if s == nil || m.deps.NativeTrailer == nil {
		return nil, false
	}
	g := s.current()
	if g == nil || g.src == nil || !g.acquire() {
		return nil, false
	}
	defer g.release()
	if expectedRevision == "" || core.SourceRevision(&core.PlaySession{Key: s.ident.key, Source: g.src}) != expectedRevision {
		return nil, false
	}
	info := g.infoSnapshot()
	if !info.ChaptersProbed && info.Container == "mkv" && !info.Estimated {
		readCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		m.deps.NativeTrailer(core.WithReadOrigin(readCtx, "playback.trailer", "requested chapters"), g.src, &info)
		cancel()
	}
	g.mu.Lock()
	updated := info.ChaptersProbed && !g.info.ChaptersProbed
	if updated {
		g.info.Chapters, g.info.ChaptersProbed = slices.Clone(info.Chapters), true
	}
	ch := slices.Clone(g.info.Chapters)
	probed := g.info.ChaptersProbed
	g.mu.Unlock()
	if s.current() != g || !probed {
		return nil, false
	}
	if updated && g.res != nil {
		m.noteReleaseProbeMemory(g.res.Release, g.res.NZB, info)
	}
	return ch, true
}

// startProbeVerify runs ffprobe after a native probe and logs any
// decision-relevant disagreement. It is a soak-test aid, off by default.
// The check opens its own source and serves it on a private loopback
// listener, because the generation that was probed may be retired (a
// decision-only session) long before ffprobe finishes.
func (m *Manager) startProbeVerify(sources *sourceLifetime, res *core.Resolution, native core.MediaInfo) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*m.opts.ProbeTimeout)
		defer cancel()
		src, finish, err := m.openSource(ctx, sources, res)
		if err != nil {
			return
		}
		inner, closeShared := unwrapShared(src)
		verification := &generation{src: inner, sharedClose: closeShared, finishSource: finish}
		defer verification.retire()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !verification.acquire() {
				http.Error(w, "gone", http.StatusGone)
				return
			}
			defer verification.release()
			http.ServeContent(w, r, src.Name(), time.Time{}, io.NewSectionReader(src, 0, src.Size()))
		})}
		go srv.Serve(ln)
		defer srv.Close()
		ref, err := m.deps.Transcoder.Probe(ctx, "http://"+ln.Addr().String()+"/media")
		if err != nil || ref == nil {
			m.log.Debug("native probe verification skipped", "release", res.Release.Name, "err", err)
			return
		}
		if diff := probeDifference(native, *ref); diff != "" {
			m.log.Warn("native probe disagrees with ffprobe", "release", res.Release.Name, "diff", diff)
			return
		}
		m.log.Info("native probe verified", "release", res.Release.Name, "streams", len(native.Streams))
	}()
}

// probeDifference compares the facts playback decisions use.
func probeDifference(native, ref core.MediaInfo) string {
	var diffs []string
	add := func(format string, args ...any) { diffs = append(diffs, fmt.Sprintf(format, args...)) }
	if d := native.Duration - ref.Duration; d > time.Second || d < -time.Second {
		add("duration %v/%v", native.Duration, ref.Duration)
	}
	if len(native.Streams) != len(ref.Streams) {
		add("streams %d/%d", len(native.Streams), len(ref.Streams))
		return strings.Join(diffs, "; ")
	}
	for i, n := range native.Streams {
		r := ref.Streams[i]
		if r.ColorTransfer == "unknown" {
			r.ColorTransfer = ""
		}
		if n.Index != r.Index || n.Type != r.Type || n.Codec != r.Codec || n.Language != r.Language || n.Default != r.Default || n.Forced != r.Forced {
			add("stream %d identity %d/%s/%s/%s vs %d/%s/%s/%s", i, n.Index, n.Type, n.Codec, n.Language, r.Index, r.Type, r.Codec, r.Language)
		}
		switch n.Type {
		case "video":
			if !strings.EqualFold(n.Profile, r.Profile) || n.Level != r.Level || n.Width != r.Width || n.Height != r.Height || n.BitDepth != r.BitDepth ||
				n.ColorTransfer != r.ColorTransfer || n.DolbyVision != r.DolbyVision || n.DolbyVisionProfile != r.DolbyVisionProfile ||
				n.DolbyVisionCompatibilityID != r.DolbyVisionCompatibilityID || n.CodecString != r.CodecString {
				add("video %+v vs %+v", n, r)
			}
		case "audio":
			if n.Channels != r.Channels || n.SampleRate != r.SampleRate || (n.Profile != "" && !strings.EqualFold(n.Profile, r.Profile)) {
				add("audio %d %s %dch %dHz vs %s %dch %dHz", n.Index, n.Profile, n.Channels, n.SampleRate, r.Profile, r.Channels, r.SampleRate)
			}
		}
	}
	return strings.Join(diffs, "; ")
}

// startIndexFill reads the container keyframe index off the critical path
// and installs it on the generation (and the session's virtual map when
// this generation defines it) once read.
func (m *Manager) startIndexFill(g *generation) {
	if core.IsBackgroundAnalysis(g.ctx) {
		g.markIndexReady()
		return
	}
	if !g.acquire() {
		g.markIndexReady()
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.release()
		g.markIndexReady()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		// Release can block in a backend Close. Leave the manager wait group
		// first so shutdown is not pinned behind that Close.
		defer g.release()
		defer m.wg.Done()
		defer g.markIndexReady()
		ctx, cancel := context.WithTimeout(g.ctx, m.opts.IndexTimeout)
		defer cancel()
		points := containerIndex(core.WithReadOrigin(ctx, "playback.index", "container keyframes"), g.src, g.src.Size())
		if len(points) == 0 {
			return
		}
		g.setIndex(points)
		s := g.sess
		// Seeking uses the index at once; persisting it for later resumes
		// can take seconds (or wait out a database outage).
		s.mu.Lock()
		if s.virtualGen == g {
			s.virtual.Points = points
		}
		s.mu.Unlock()
		if m.resume != nil {
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			if err := m.resume.saveIndex(ctx, s.ident.userID, s.ident.key, g.releaseID(), keyframesOf(points)); err != nil {
				if store.IsUnavailable(err) {
					// Only an optimisation for later resumes; the stream
					// itself never needs it.
					m.log.Debug("save resume keyframe index skipped: database unreachable", "key", s.ident.key, "err", err)
				} else {
					m.log.Warn("save resume keyframe index failed", "key", s.ident.key, "err", err)
				}
			}
			cancel()
		}
	}()
}

// dropGeneration discards a generation that never became current.
func (m *Manager) dropGeneration(g *generation) {
	m.mu.Lock()
	delete(m.tokens, g.sess.id+"/"+g.token)
	m.mu.Unlock()
	g.markIndexReady()
	g.retire()
}

// forgetUnreadable marks a provably unreadable release bad, so neither the
// resolver, a remembered resume nor another candidate pass of this title
// picks it again.
func (m *Manager) forgetUnreadable(s *session, g *generation, reason string) {
	if g == nil || g.res == nil || g.releaseID() == "" || s.isKind() {
		return
	}
	m.markBad(g.releaseID(), reason)
	m.rememberOpenFailure(s, g.res.Release, g.sourceFailure())
}

func (m *Manager) markBad(releaseID, reason string) {
	if m.deps.Resolver == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if err := m.deps.Resolver.MarkBad(ctx, releaseID, reason); err != nil {
		m.log.Warn("mark release bad failed", "release", releaseID, "err", err)
	}
}

// Failover swaps the session to the next candidate release, keeping the
// timestamp at. The current release is marked bad. When an HLS session
// reads from this playback session it is restarted on the new input at the
// same position. Returns the updated session.
func (m *Manager) Failover(ctx context.Context, sessionID string, at time.Duration) (*core.PlaySession, error) {
	s := m.lookup(sessionID)
	if s == nil {
		return nil, fmt.Errorf("playback: session %s: %w", sessionID, core.ErrNotFound)
	}
	if _, err := m.failover(ctx, s, s.current(), at, "failover requested"); err != nil && !errors.Is(err, errRequireRemux) {
		return nil, err
	}
	return s.snapshot(m.now()), nil
}

// FailoverInput handles an exhausted encoder input. The input URL identifies
// the exact source generation, so an old encoder's delayed exit cannot discard
// a newer source. Exhausted recovery closes the session and its resources.
func (m *Manager) FailoverInput(ctx context.Context, sessionID, inputURL string, at time.Duration) error {
	s := m.lookup(sessionID)
	if s == nil {
		return nil // the client already stopped
	}
	g := s.current()
	if g == nil || m.urlFor(g) != inputURL {
		return nil // another reader already recovered this input
	}
	_, err := m.failoverWith(ctx, s, g, at, failoverOpts{reason: "encoder input failed", stopOnFailure: true})
	return err
}

// failoverOpts controls candidate selection and recovery behavior. Permanent
// source failures are marked bad independently of these options.
type failoverOpts struct {
	encoder        bool        // encoder shortage: only accept a video-copy candidate
	stillNeeded    func() bool // speculative recovery rechecks before publishing a swap
	quietFailure   bool
	preserveBuffer bool
	reason         string
	down           bool
	sourceBps      float64 // measured sustainable source throughput for a speed-triggered downshift
	stopOnFailure  bool
	strictDirect   bool // a different release needs a new direct-play decision
	// stallRecovery: the trigger is a stalled media host, not bad data.
	// When every alternate fails fast, the session may continue its
	// original release once instead of handing off over the same edge.
	stallRecovery bool
	// followPlayer: at was mapped from a failed source read, which may be
	// a container tail/index read or predate a seek; resume at the
	// player's latest segment request when the HLS session reports it.
	followPlayer bool
}

func (m *Manager) failover(ctx context.Context, s *session, observed *generation, at time.Duration, reason string) (*generation, error) {
	return m.failoverWith(ctx, s, observed, at, failoverOpts{reason: reason})
}

// maxStallContinues bounds how often a session may continue its release past
// a failover where every alternate failed fast. Bounded, so a genuinely dead
// release still ends or restarts within a few grace windows (issue #169).
const maxStallContinues = 1

// takeStallContinue consumes the session's grace to keep the current release
// through a media-host stall. Bad data and provider pauses (sources that
// refuse a range retry) cannot improve by waiting the same host out.
func (s *session) takeStallContinue(cur *generation) bool {
	if permanentSourceFailure(cur.sourceFailure()) {
		return false
	}
	if retry, ok := cur.src.(core.DirectPlayRetrier); ok && !retry.RetryDirectPlay() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stallContinues >= maxStallContinues {
		return false
	}
	s.stallContinues++
	return true
}

// failoverWith replaces generation observed. Concurrent callers that
// observed the same failing generation share one swap: the first does the
// work and the others return the generation it installed.
func (m *Manager) failoverWith(ctx context.Context, s *session, observed *generation, at time.Duration, fo failoverOpts) (result *generation, retErr error) {
	if err := lock(ctx, s.failSem); err != nil {
		return nil, err
	}
	s.failingOver.Store(true)
	defer func() {
		s.failingOver.Store(false)
		unlock(s.failSem)
	}()
	// lock's select is random when the semaphore frees as ctx is cancelled
	// (a stopped Live TV session): a cancelled caller opens no provider.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started := time.Now()

	s.mu.Lock()
	cur, closed, failovers := s.cur, s.closed, s.failovers
	// A release that is provably unreadable (corrupt or missing data) can
	// never deliver this session, wherever its selection came from.
	unreadable := !fo.encoder && cur == observed && cur != nil && permanentSourceFailure(cur.sourceFailure())
	replaceable := s.replaceableLocked(unreadable)
	direct := fo.strictDirect || (s.decision == core.DecisionDirectPlay && !spliceable(s.container, s.name))
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("playback: session %s ended: %w", s.id, core.ErrNotFound)
	}
	if cur != observed {
		return cur, nil // someone else already swapped
	}
	if fo.stillNeeded != nil && !fo.stillNeeded() {
		return cur, nil
	}
	if fo.stopOnFailure {
		defer func() {
			if retErr != nil && !errors.Is(retErr, errRequireRemux) {
				m.Stop(s.id)
			}
		}()
	}
	if !replaceable {
		if fo.stopOnFailure && m.trySkipDamagedSegment(ctx, s, cur, at) {
			return cur, nil
		}
		if unreadable {
			// The viewer's own pick (or a pinned source) stays put, but it
			// cannot play: end with a reason the viewer can act on, and keep
			// every later start off this release.
			m.forgetUnreadable(s, cur, fo.reason)
			m.recordTimeline(s, TimelineEvent{
				Kind:    "error",
				Level:   "error",
				Failing: true,
				Message: "This version can't be played: the file is incomplete on your provider",
				Detail:  OwnerError(cur.sourceFailure()),
			})
			return nil, fmt.Errorf("release %s: %w", cur.releaseID(), core.ErrVersionUnreadable)
		}
		return nil, fmt.Errorf("%w: selected release cannot be replaced", core.ErrNotAvailable)
	}
	if failovers >= m.opts.MaxFailovers {
		if fo.stopOnFailure && m.trySkipDamagedSegment(ctx, s, cur, at) {
			return cur, nil
		}
		return nil, fmt.Errorf("%w: session %s gave up after %d failovers", core.ErrNotAvailable, s.id, failovers)
	}
	log := m.log.With("session", s.id, "key", s.ident.key, "at", at)
	log.Warn("failing over", "release", cur.res.Release.Name, "reason", fo.reason, "down", fo.down)
	failMsg := "Failing over from " + releaseLabel(cur.res.Release)
	if fo.down {
		failMsg = "Adapting down from " + releaseLabel(cur.res.Release)
	}
	m.recordTimeline(s, TimelineEvent{
		Kind:    "failover",
		Level:   "warn",
		Failing: true,
		Message: failMsg,
		Detail:  fo.reason,
	})
	// A provider outage is not evidence that the release itself is invalid.
	if unreadable {
		m.forgetUnreadable(s, cur, fo.reason)
	}
	if !fo.encoder && core.IsExplicitVersionSlotSelector(s.prefer) {
		m.noteStaleRelease(s.ident.key, cur.releaseID(), s.title, "", fo.reason)
	}
	if cur.res != nil {
		s.setAlternateMeta(cur.res.VersionSelector, cur.res.QualityFallback)
	}

	s.mu.Lock()
	tried := slices.Clone(s.tried)
	if !slices.Contains(tried, cur.releaseID()) {
		tried = append(tried, cur.releaseID())
	}
	if !fo.encoder {
		s.tried = slices.Clone(tried)
	}
	alternates := s.alternates
	s.mu.Unlock()

	oldInfo := cur.infoSnapshot()
	if direct {
		// Identical-size reposts can continue the client's byte stream;
		// try them before opening candidates in rank order.
		alternates = orderExactSizeFirst(alternates, cur.res.Release)
	} else if fo.down {
		alternates = orderLowerBitrateFirst(alternates, cur.res.Release)
	}
	fallback := func(size int64) core.MediaInfo {
		info := oldInfo
		info.Size = size
		info.Estimated = true
		info.Keyframes = nil
		return info
	}
	curBps := replacementBps(cur, 0)
	// Compare file consumption, independently of the codecs the muxer can
	// copy. Allow a margin for variable-rate media and short fetch stalls.
	maxSourceBps := fo.sourceBps * 0.8
	maxReplacementBps := maxSourceBps
	if fo.down {
		// The last mile can tighten the swap budget. It supplies no evidence
		// that the source can feed a video encode.
		if budget := float64(m.clientDeliveryBudget(s)) * (1000.0 / 8); budget > 0 && (maxSourceBps == 0 || budget < maxSourceBps) {
			maxReplacementBps = budget
		}
	}
	admitDown := func(next *generation) error {
		if !fo.down {
			return nil
		}
		bps := replacementBps(next, oldInfo.Duration)
		if bps <= 0 || bps >= curBps || (maxReplacementBps > 0 && bps > maxReplacementBps) ||
			(cur.src.Size() > 0 && next.src.Size() >= cur.src.Size()) {
			return fmt.Errorf("down-failover candidate exceeds bitrate budget (candidate %.0f B/s, current %.0f B/s, budget %.0f B/s): %w", bps, curBps, maxReplacementBps, core.ErrNotAvailable)
		}
		return nil
	}
	var (
		g           *generation
		fallbackGen *generation // healthy but not spliceable into direct play
		remuxSwitch bool
		lastErr     error
	)
	var prepared *core.Resolution
	if !fo.encoder {
		prepared = cur.takeDamageResolution(tried)
	}
	copyCandidate := func(next *generation) bool {
		hls, ok := m.deps.HLSLookup(s.id)
		if !ok || hls == nil {
			return false
		}
		p, ok := hls.(interface{ SwapWouldCopyVideo(core.MediaInfo) bool })
		return ok && p.SwapWouldCopyVideo(next.infoSnapshot())
	}
	// Without a throughput estimate, avoid adding video encoding to an
	// already slow path. A measured budget permits a fitting, convertible
	// input when no copy-compatible release is available.
	acceptDown := func(next *generation) error {
		if err := admitDown(next); err != nil {
			return err
		}
		if fo.down && !direct && maxSourceBps <= 0 && !copyCandidate(next) {
			return fmt.Errorf("replacement needs video encoding without a measured source budget: %w", core.ErrNotAvailable)
		}
		return nil
	}
	if fo.down {
		g = m.joinSpeedAlternate(ctx, cur, tried)
		if g != nil {
			err := acceptDown(g)
			if err == nil {
				err = m.preflightSwap(ctx, s, g)
			}
			if err != nil {
				log.Debug("prepared alternate rejected", "release", g.res.Release.Name, "err", err)
				tried = append(tried, g.releaseID())
				m.dropGeneration(g)
				g = nil
			}
		}
	}
	var preferCopy func(*generation) bool
	if !direct && !fo.encoder {
		if hls, ok := m.deps.HLSLookup(s.id); ok && hls != nil {
			if p, ok := hls.(interface{ SwapWouldCopyVideo(core.MediaInfo) bool }); ok {
				preferCopy = func(next *generation) bool { return p.SwapWouldCopyVideo(next.infoSnapshot()) }
			}
		}
	}
	if g == nil && !direct && prepared == nil && !s.isKind() {
		// Any healthy alternate continues an encoder session, so open
		// several at once instead of stalling through each broken one.
		var failed []string
		width := failoverRaceWidth
		var accept func(*generation) error
		if fo.down {
			width = 2
			accept = acceptDown
		}
		if fo.encoder {
			accept = func(next *generation) error {
				if !copyCandidate(next) {
					return fmt.Errorf("replacement needs video encoding: %w", core.ErrNotAvailable)
				}
				return nil
			}
		}
		g, failed, lastErr = m.raceAlternates(ctx, s, tried, alternates, fallback, width, accept, preferCopy)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tried = append(tried, failed...)
	}
	const maxFailoverMorePasses = 8
	const maxHLSSwapAttempts = 3
	var newInfo core.MediaInfo
	for hlsSwapAttempt := 0; ; hlsSwapAttempt++ {
		var copySearchCtx context.Context
		var copySearchCancel context.CancelFunc
		morePasses := 0
		for attempt := 0; g == nil && m.attemptAllowed(attempt); attempt++ {
			attemptCtx := ctx
			if copySearchCtx != nil {
				attemptCtx = copySearchCtx
			}
			if ctx.Err() != nil {
				if copySearchCancel != nil {
					copySearchCancel()
				}
				// The copy-preference retry skips the post-failure check
				// below; never start another candidate's search or open.
				if fallbackGen != nil {
					m.dropGeneration(fallbackGen)
				}
				return nil, ctx.Err()
			}
			var (
				res *core.Resolution
				err error
			)
			if s.isKind() {
				g, res, err = m.openKindCandidate(attemptCtx, s, tried, fallback)
				if err != nil && res == nil {
					lastErr = err
					break
				}
			} else {
				res = prepared
				prepared = nil
				if res == nil {
					res, err = m.nextResolutionTracking(attemptCtx, s, &tried, nil, alternates)
				}
				if err != nil {
					if errors.Is(err, core.ErrMoreCandidates) && attemptCtx.Err() == nil && morePasses < maxFailoverMorePasses {
						morePasses++
						alternates = nil
						attempt--
						continue
					}
					lastErr = err
					break
				}
				g, err = m.openGeneration(attemptCtx, s, res, fallback)
			}
			if err == nil {
				if perr := m.preflightSwap(attemptCtx, s, g); perr != nil {
					m.dropGeneration(g)
					g, err = nil, perr
				}
			}
			if err == nil && fo.encoder && !copyCandidate(g) {
				m.dropGeneration(g)
				g, err = nil, fmt.Errorf("replacement needs video encoding: %w", core.ErrNotAvailable)
			}
			if err == nil {
				err = acceptDown(g)
				if err != nil {
					m.dropGeneration(g)
					g = nil
				}
			}
			// Byte-range clients retain the original container headers and
			// offsets. Equal lengths do not prove equal bytes or Cues. A different
			// release is kept as a remux-switch fallback rather than being served
			// under the original representation's validators.
			if err == nil && direct && (g.releaseID() != cur.releaseID() || g.src.Size() != cur.src.Size() || fo.strictDirect) {
				if fallbackGen == nil {
					fallbackGen = g
				} else {
					m.dropGeneration(g)
				}
				g = nil
				err = fmt.Errorf("incompatible direct-play replacement: %w", core.ErrNotAvailable)
				if s.isKind() && fallbackGen != nil && !fo.down {
					lastErr = err
					break
				}
			}
			if err == nil && preferCopy != nil && !preferCopy(g) {
				if fallbackGen == nil {
					fallbackGen = g
					copySearchCtx, copySearchCancel = context.WithTimeout(ctx, 500*time.Millisecond)
				} else {
					m.dropGeneration(g)
				}
				g = nil
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				continue
			}
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				if copySearchCancel != nil {
					copySearchCancel()
				}
				if fallbackGen != nil {
					m.dropGeneration(fallbackGen)
				}
				return nil, ctx.Err()
			}
			lastErr = err
			log.Warn("failover candidate failed", "release", res.Release.Name, "err", err)
			tried = append(tried, res.Release.ID)
			if len(res.Alternates) > 0 {
				alternates = res.Alternates
			}
			if direct {
				alternates = orderExactSizeFirst(alternates, cur.res.Release)
			} else if fo.down {
				alternates = orderLowerBitrateFirst(alternates, cur.res.Release)
			}
		}
		if copySearchCancel != nil {
			copySearchCancel()
		}
		if g == nil && fo.stallRecovery && direct && !fo.encoder && !fo.down {
			if fo.stillNeeded != nil && !fo.stillNeeded() {
				// Progress during the candidate search already cleared the
				// stall: leave recovery as it is without spending the grace,
				// and close the opened fallback this call owns — like the
				// cancellation paths, nothing downstream sees it.
				if fallbackGen != nil {
					m.dropGeneration(fallbackGen)
				}
				return cur, nil
			}
			if s.takeStallContinue(cur) {
				// A CDN edge that stalls mid-file usually recovers in tens of
				// seconds, and the alternates that just failed dial the same
				// provider network. One continuation keeps a cached, previously
				// healthy release under repair instead of handing the client a
				// remux restart across the same failing hosts. The grace is one
				// per playback session, not per outage.
				if fallbackGen != nil {
					m.dropGeneration(fallbackGen)
				}
				log.Warn("media host stalled; continuing the original release", "release", cur.res.Release.Name, "lastErr", lastErr)
				return cur, nil
			}
		}
		if g == nil && fallbackGen != nil {
			// No identical representation: do not splice. Swap anyway and restart
			// the client as a zero-encode remux at the mapped time.
			g, fallbackGen = fallbackGen, nil
			if direct {
				remuxSwitch = true
				log.Warn("direct-play replacement changes representation; switching to remux", "release", g.res.Release.Name)
			}
		}
		if fallbackGen != nil {
			m.dropGeneration(fallbackGen)
			fallbackGen = nil
		}
		if g == nil {
			if fo.stillNeeded != nil && !fo.stillNeeded() {
				return cur, nil
			}
			if maxSourceBps > 0 && !direct && ctx.Err() == nil {
				if err := m.reduceOutputBitrate(ctx, s, cur, int(maxReplacementBps*8/1000)); err == nil {
					return cur, nil
				} else if !errors.Is(err, core.ErrNotAvailable) {
					lastErr = err
				}
			}
			if fo.stopOnFailure && m.trySkipDamagedSegment(ctx, s, cur, at) {
				return cur, nil
			}
			if lastErr == nil {
				lastErr = core.ErrNotAvailable
			}
			if fo.quietFailure {
				log.Debug("failover found no working release", "err", lastErr)
			} else {
				log.Error("failover found no working release", "err", lastErr)
				m.emitEvent("error", s, lastErr)
			}
			if fo.down {
				m.noteFailedDownFailover(s, tried)
			}
			return nil, fmt.Errorf("playback: failover: %w", lastErr)
		}

		if fo.stillNeeded != nil && !fo.stillNeeded() {
			m.dropGeneration(g)
			return cur, nil
		}
		// An HLS session restarts ffmpeg on fresh headers; anything else that
		// swaps to a different-size file must be remuxed. Mark that in the same
		// critical section as the swap, so no request sees the new release
		// without the remux requirement.
		hls, hasHLS := m.deps.HLSLookup(s.id)
		hasHLS = hasHLS && hls != nil
		if hasHLS {
			remuxSwitch = false
		}

		// Take the replacement pin before it is handed to the encoder. Stop can
		// race the handoff; until publication this function owns the extra pin.
		m.pinLive(g.releaseID())
		newInfo = g.infoSnapshot()
		if !hasHLS {
			break
		}
		var seekAt time.Time
		if source, ok := hls.(core.RecoveryBufferSource); ok {
			seekAt = source.RecoveryBuffer().SeekAt
		}
		if position, ok := hls.(core.PlaybackPosition); ok && fo.followPlayer {
			// Read only now: the player may have sought while candidates
			// were being opened.
			requested := position.RequestedPosition()
			log.Info("failover follows player", "requested", requested)
			at = requested
		}
		target := m.alignForFailover(ctx, s, cur, g, at)
		swap := hls.SwapInput
		if buffered, ok := hls.(core.BufferedInputSwapper); ok && fo.preserveBuffer && target == at {
			if source, ok := hls.(core.RecoveryBufferSource); ok {
				boundary := source.RecoveryBuffer().ReadyUntil
				// Check the handoff itself, not only a zero playhead where PAL
				// speed corrections and different edits can happen to agree.
				if boundary > at && boundary < newInfo.Duration && m.alignForFailover(ctx, s, cur, g, boundary) == boundary {
					swap = buffered.SwapInputBuffered
				}
			}
		}
		swapCtx, cancelSwap := context.WithCancel(ctx)
		stopCancel := context.AfterFunc(s.stopCtx, cancelSwap)
		err := swap(swapCtx, m.urlFor(g), newInfo, target)
		if err == nil {
			if ready, ok := hls.(core.ReplacementOutputWaiter); ok {
				readyCtx, cancelReady := context.WithTimeout(swapCtx, 30*time.Second)
				err = ready.WaitForReplacementOutput(readyCtx, m.urlFor(g))
				if err == nil {
					elapsed := time.Since(started)
					m.swapLatency.observe(elapsed)
					log.Info("replacement output contiguous", "release", g.releaseID(), "elapsed", elapsed, "sourceReserveTarget", m.swapLatency.target())
					// One old source stays available briefly after the first
					// contiguous replacement output, without a second encoder.
					timer := time.NewTimer(time.Second)
					select {
					case <-timer.C:
					case <-readyCtx.Done():
						err = readyCtx.Err()
					}
					timer.Stop()
					if err == nil {
						err = ready.WaitForReplacementOutput(readyCtx, m.urlFor(g))
					}
				}
				cancelReady()
			}
		}
		stopCancel()
		cancelSwap()
		if errors.Is(err, core.ErrReplacementSuperseded) {
			// A seek abandons the old boundary. Do not roll it back.
			if state, ok := hls.(core.InputSwapState); ok {
				switch state.CurrentSwapInput() {
				case m.urlFor(g):
					err = nil
				case m.urlFor(cur):
					m.dropGeneration(g)
					m.unpinLive(g.releaseID())
					return cur, nil
				}
			}
		}
		if err == nil {
			if fo.down {
				if adaptive, ok := hls.(core.AdaptiveBitrateCapper); ok && maxReplacementBps > 0 {
					if _, capErr := adaptive.CapAdaptiveBitrate(m.urlFor(g), int(maxReplacementBps*8/1000)); capErr != nil {
						log.Debug("adaptive bitrate cap could not be applied after source replacement", "err", capErr)
					}
				}
			}
			break
		}
		changed := false
		if state, ok := hls.(core.InputSwapState); ok {
			changed = state.CurrentSwapInput() == m.urlFor(g)
		}
		// Never roll back to an input that already failed encoder input
		// (fo.stopOnFailure): rolling back revives the broken source and
		// ping-pongs forever with the failing replacement (#694).
		rolledBack := changed && !fo.stopOnFailure && m.rollbackHLS(s, hls, cur, g, at, seekAt)
		failedID := g.releaseID()
		m.dropGeneration(g)
		m.unpinLive(failedID)
		tried = append(tried, failedID)
		if fo.down {
			m.noteFailedDownFailover(s, tried)
		} else if fo.encoder {
			m.noteTriedReleases(s, []string{failedID})
		} else {
			m.noteTriedReleases(s, tried)
		}
		if rolledBack {
			s.resetSpeed(m.now(), m.opts.SpeedGrace)
			return cur, nil
		}
		if fo.stopOnFailure {
			lastErr = err
			if ctx.Err() == nil && hlsSwapAttempt+1 < maxHLSSwapAttempts && !errors.Is(err, core.ErrEncoderExhausted) {
				log.Warn("hls replacement failed; trying next release", "release", failedID, "err", err, "attempt", hlsSwapAttempt+1)
				g = nil
				remuxSwitch = false
				continue
			}
			if m.trySkipDamagedSegment(ctx, s, cur, at) {
				return cur, nil
			}
			log.Error("hls input swap failed", "err", err)
			return cur, fmt.Errorf("playback: swap hls input: %w", err)
		}
		if changed {
			m.Stop(s.id) // never leave an encoder pointed at a revoked input
		}
		log.Error("hls input swap failed", "err", err)
		return cur, fmt.Errorf("playback: swap hls input: %w", err)
	}

	// Pin before publishing g: a Stop right after the swap unpins s.cur,
	// and must find this pin rather than leave one taken later behind.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		m.dropGeneration(g)
		m.unpinLive(g.releaseID())
		return nil, fmt.Errorf("playback: session %s ended: %w", s.id, core.ErrNotFound)
	}
	s.cur = g
	if remuxSwitch {
		s.requiresRemux = true
	}
	s.failovers++
	if fo.down {
		s.downFailovers++
	}
	// Racers that failed after the winner may already have been recorded.
	if fo.encoder {
		tried = []string{cur.releaseID()}
	}
	for _, id := range tried {
		if !slices.Contains(s.tried, id) {
			s.tried = append(s.tried, id)
		}
	}
	if len(g.res.Alternates) > 0 {
		s.alternates = g.res.Alternates
	} else {
		s.alternates = withoutTried(alternates, append(slices.Clone(s.tried), g.releaseID()))
	}
	s.mu.Unlock()

	if rec, ok := m.deps.Cache.(core.CacheProvenanceRecorder); ok && g.src != nil {
		rec.RecordProvenance(core.CacheProvenance{ReleaseID: g.releaseID(), RatingKey: g.res.Title, File: g.src.Name(), Size: g.src.Size(), Source: releaseLabel(g.res.Release), Indexer: g.res.Release.Indexer, Origin: sourceOrigin(g.src, g.res.Release.Protocol), Cause: core.CacheCauseFailover})
	}

	m.mu.Lock()
	delete(m.tokens, s.id+"/"+cur.token)
	m.mu.Unlock()
	cur.retire()
	m.unpinLive(cur.releaseID())
	m.saveResume(ctx, s, g)
	s.resetSpeed(m.now(), m.opts.SpeedGrace)

	if fo.down {
		m.recordTimeline(s, TimelineEvent{
			Kind:    "quality",
			Level:   "warn",
			Failing: true,
			Message: "Switched to lower-bitrate release " + releaseLabel(g.res.Release),
			Detail:  timelineOpenedDetail(newInfo, g.res.Release, g.src.Size()),
		})
		m.emitSourceFailover(s, cur, g, true, fo.reason)
		m.emitEvent("downfailover", s, nil)
	} else {
		detail := timelineOpenedDetail(newInfo, g.res.Release, g.src.Size())
		if remuxSwitch {
			if detail != "" {
				detail += " · switching to remux"
			} else {
				detail = "switching to remux"
			}
		}
		m.recordTimeline(s, TimelineEvent{
			Kind:    "failover",
			Level:   "info",
			Message: "Switched to " + releaseLabel(g.res.Release),
			Detail:  detail,
		})
		m.emitSourceFailover(s, cur, g, false, fo.reason)
		m.emitEvent("failover", s, nil)
	}
	log.Info("failed over", "release", g.res.Release.Name, "size", g.src.Size(), "duration", newInfo.Duration, "remuxSwitch", remuxSwitch,
		"candidateBps", int64(replacementBps(g, oldInfo.Duration)), "measuredSourceBps", int64(fo.sourceBps), "sourceBudgetBps", int64(maxSourceBps))
	if remuxSwitch {
		return g, errRequireRemux
	}
	return g, nil
}

// noteTriedReleases records release IDs that failed to open or swap so later
// failovers never loop back to the same failing candidates (#694).
func (m *Manager) noteTriedReleases(s *session, tried []string) {
	if s == nil || len(tried) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, id := range tried {
		if id != "" && !slices.Contains(s.tried, id) {
			s.tried = append(s.tried, id)
		}
	}
}

// trySkipDamagedSegment asks an HLS encoder session to advance past a bounded
// corrupt mid-stream segment range on the current source when no alternate
// release recovered the segment.
func (m *Manager) trySkipDamagedSegment(ctx context.Context, s *session, cur *generation, at time.Duration) bool {
	if at <= 0 || cur == nil || cur.sourceFailure() != nil || cur.articleMissing.Load() {
		return false
	}
	hls, ok := m.deps.HLSLookup(s.id)
	if !ok || hls == nil {
		return false
	}
	skipper, ok := hls.(core.DamagedSegmentSkipper)
	if !ok {
		return false
	}
	skipCtx, cancelSkip := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(s.stopCtx, cancelSkip)
	err := skipper.SkipDamagedSegment(skipCtx, m.urlFor(cur), cur.infoSnapshot(), at)
	stopCancel()
	cancelSkip()
	if err != nil {
		m.log.Warn("damaged segment skip failed", "session", s.id, "at", at, "err", err)
		return false
	}
	return true
}

// rollbackHLS only returns to a source that is still open, compatible and
// responds at the current playback window. It never revives a failed source
// or a timeline abandoned by a seek.
func (m *Manager) rollbackHLS(s *session, hls core.HLSSession, old, replacement *generation, at time.Duration, seekAt time.Time) bool {
	if s.isClosed() || s.current() != old || old.sourceFailure() != nil || old.articleMissing.Load() || !old.acquire() {
		return false
	}
	defer old.release()
	ctx, cancel := context.WithTimeout(s.stopCtx, 3*time.Second)
	defer cancel()
	if m.preflightSwap(ctx, s, old) != nil {
		return false
	}
	if buffer, ok := hls.(core.RecoveryBufferSource); ok {
		b := buffer.RecoveryBuffer()
		if b.SeekAt != seekAt || m.alignForFailover(ctx, s, replacement, old, b.Position) != b.Position || m.alignForFailover(ctx, s, replacement, old, b.ReadyUntil) != b.ReadyUntil {
			return false
		}
		at = b.Position
	}
	tm := old.timeMapSnapshot()
	off := tm.KeyframeOffsetAt(at)
	if off < 0 || off >= old.src.Size() {
		return false
	}
	p := make([]byte, min(int64(32<<10), old.src.Size()-off))
	var n int
	var err error
	if reader, ok := old.src.(core.PrefetchReaderAt); ok {
		n, err = reader.ReadAtPrefetchContext(ctx, p, off)
	} else if reader, ok := old.src.(core.ContextReaderAt); ok {
		n, err = reader.ReadAtContext(ctx, p, off)
	} else {
		return false
	}
	if err != nil || n != len(p) || ctx.Err() != nil || s.isClosed() || s.current() != old {
		return false
	}
	if buffered, ok := hls.(core.BufferedInputSwapper); ok {
		err = buffered.SwapInputBuffered(ctx, m.urlFor(old), old.infoSnapshot(), at)
	} else {
		err = hls.SwapInput(ctx, m.urlFor(old), old.infoSnapshot(), at)
	}
	return err == nil
}

// alignForFailover maps a failover timestamp between releases: through
// the optional dialogue TimelineAligner (300ms budget) when wired, else
// duration-based AlignTime (PAL rule).
func (m *Manager) alignForFailover(ctx context.Context, s *session, from, to *generation, at time.Duration) time.Duration {
	fromDur := from.infoSnapshot().Duration
	toDur := to.infoSnapshot().Duration
	if m.deps.Aligner != nil && from.res != nil && to.res != nil {
		req := core.AlignRequest{
			Key:          s.ident.key,
			From:         from.res.Release,
			To:           to.res.Release,
			FromDuration: fromDur,
			ToDuration:   toDur,
		}
		type result struct {
			mapped time.Duration
			ok     bool
		}
		ch := make(chan result, 1)
		actx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		go func() {
			mapped, ok := m.deps.Aligner.AlignTime(actx, req, at)
			ch <- result{mapped, ok}
		}()
		select {
		case r := <-ch:
			cancel()
			if r.ok {
				if r.mapped < 0 {
					return 0
				}
				if toDur > 0 && r.mapped > toDur {
					return toDur
				}
				return r.mapped
			}
		case <-actx.Done():
			cancel()
			m.log.Debug("timeline aligner timed out; using duration alignment", "session", s.id)
		}
	}
	return AlignTime(at, fromDur, toDur)
}

// orderExactSizeFirst stably moves candidates whose known size matches
// the failing release (identical reposts — same size, then same name,
// then same group) ahead of rank order for direct-play failover.
func orderExactSizeFirst(alts []core.Release, cur core.Release) []core.Release {
	if cur.Size <= 0 || len(alts) < 2 {
		return alts
	}
	out := slices.Clone(alts)
	rank := func(r core.Release) int {
		switch {
		case r.Size == cur.Size && r.Name == cur.Name:
			return 0
		case r.Size == cur.Size && r.Info.Group != "" && r.Info.Group == cur.Info.Group:
			return 1
		case r.Size == cur.Size:
			return 2
		}
		return 3
	}
	sortStableByRank(out, rank)
	return out
}

// orderLowerBitrateFirst stably moves candidates of known smaller size
// (lower bitrate at equal duration) ahead for down-failover, keeping the
// resolver's ranking within each group.
func orderLowerBitrateFirst(alts []core.Release, cur core.Release) []core.Release {
	if cur.Size <= 0 || len(alts) < 2 {
		return alts
	}
	out := slices.Clone(alts)
	rank := func(r core.Release) int {
		if r.Size > 0 && r.Size < cur.Size {
			return 0
		}
		return 1
	}
	sortStableByRank(out, rank)
	return out
}

func sortStableByRank(rs []core.Release, rank func(core.Release) int) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rank(rs[j]) < rank(rs[j-1]); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func withoutTried(rs []core.Release, tried []string) []core.Release {
	var out []core.Release
	for _, r := range rs {
		if !slices.Contains(tried, r.ID) {
			out = append(out, r)
		}
	}
	return out
}

func containerFromName(name string) string {
	switch ct := contentTypeFor("", name); ct {
	case "video/x-matroska", "audio/x-matroska":
		return "mkv"
	case "video/mp4", "audio/mp4":
		return "mp4"
	case "video/mp2t":
		return "mpegts"
	case "video/x-msvideo":
		return "avi"
	case "video/webm", "audio/webm":
		return "webm"
	case "audio/flac":
		return "flac"
	case "audio/mpeg":
		return "mp3"
	case "audio/aac":
		return "aac"
	case "audio/ogg":
		return "ogg"
	case "audio/wav":
		return "wav"
	case "audio/aiff":
		return "aiff"
	case "audio/x-ms-wma":
		return "wma"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Resume records

// resumeResolution rebuilds a Resolution from the stored resume record when
// it was resolved for the same profile selection key (configured
// definition, pre-clamp) and, when the profile compares original-language
// audio, the same catalogue language. Legacy records store a plain name,
// never match, and are replaced by the fresh resolution's record. A
// language mismatch drops the memory for this new selection; it does not
// change a stream that is already open.
func (m *Manager) resumeResolution(ctx context.Context, userID int64, key core.RatingKey, profile core.QualityProfile, caps core.ClientCaps, originalLanguage string) *core.Resolution {
	if m.resume == nil || m.deps.Availability.Down() {
		return nil
	}
	if res := m.resume.rememberedResolution(userID, key, profile, profileKey(profile), originalLanguage, caps); res != nil && !m.releaseBlocked(ctx, res.Release.ID) && m.resumeSourceAllowed(res.Release) {
		return res
	}
	rec, err := m.resume.get(ctx, userID, key)
	if err != nil {
		m.log.Warn("resume lookup failed", "err", err)
		return nil
	}
	if rec == nil || m.releaseBlocked(ctx, rec.releaseID) || rec.profile != profileKey(profile) || !rememberedOriginalAudioCurrent(profile, rec.originalLanguage, originalLanguage, rec.release) || !rememberedCodecCurrent(rec.release, caps) || !m.resumeSourceAllowed(rec.release) || !sourceFitsBitrate(rec.size, rec.duration, 0, caps.MaxBitrateKbps) || !sourceFitsBitrate(rec.size, rec.duration, 0, caps.BroadcastBitrateKbps) {
		return nil
	}
	if caps.BroadcastBitrateKbps > 0 && (rec.size <= 0 || rec.duration <= 0) {
		return nil
	}
	if caps.BroadcastBitrateKbps > 0 {
		file := rec.release
		file.Size = rec.size
		if !releasepolicy.ChannelSourceAllowed(file, rec.duration, caps.BroadcastBitrateKbps) {
			return nil
		}
	}
	nzb, err := m.resume.readResumeNZB(ctx, rec)
	if err != nil {
		m.log.Warn("stored nzb unreadable; resolving afresh", "release", rec.releaseID, "err", err)
		return nil
	}
	m.log.Debug("reusing stored release", "key", key, "release", rec.release.Name)
	return &core.Resolution{Title: key, Release: rec.release, NZB: nzb, ResolvedAt: rec.updatedAt, VersionSelector: rec.versionSelector}
}

// saveResume remembers the release now serving a session.
func (m *Manager) saveResume(ctx context.Context, s *session, g *generation) {
	info := g.infoSnapshot()
	if g.res != nil {
		m.noteSelectionProbe(s.ident.key, g.res, info)
	}
	if s.ident.key.IsPluginKey() {
		m.noteItemProbeMemory(s.ident.key, info)
	}
	steppedDownSlot := core.IsExplicitVersionSlotSelector(s.prefer) && g.res != nil && !g.res.MatchesVersion(s.prefer)
	if m.resume == nil || g.res == nil || len(g.res.NZB) == 0 || steppedDownSlot {
		if g.res != nil || s.ident.key.IsPluginKey() {
			var rel core.Release
			var nzb []byte
			if g.res != nil {
				rel = g.res.Release
				if len(g.res.NZB) > 0 {
					nzb = append([]byte(nil), g.res.NZB...)
				}
			}
			key := s.ident.key
			m.beginResumeSave()
			go func() {
				defer m.endResumeSave()
				pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if rel.ID != "" || rel.InfoHash != "" || rel.ContentID != "" {
					m.persistReleaseProbe(pctx, rel, nzb, info)
				}
				if key.IsPluginKey() {
					m.persistItemProbe(pctx, key, info)
				}
			}()
		}
		return
	}
	selector := ""
	if s.prefer != "" && !isAutoVersionSelector(s.prefer) && g.res.MatchesVersion(s.prefer) {
		if g.res.VersionSelector != "" && !isAutoVersionSelector(g.res.VersionSelector) {
			selector = g.res.VersionSelector
		} else {
			selector = s.prefer
		}
	}
	rec := resumeRecord{
		userID:           s.ident.userID,
		key:              s.ident.key,
		release:          g.res.Release,
		profile:          s.profileKey,
		originalLanguage: s.originalLanguage,
		versionSelector:  selector,
		size:             g.src.Size(),
		duration:         info.Duration,
		updatedAt:        m.now(),
	}
	nzb := append([]byte(nil), g.res.NZB...)
	points := append([]core.KeyframePoint(nil), info.Keyframes...)
	var liveProbe *probeCache
	if facts, ok := persistedProbeFacts(info); ok {
		liveProbe = &probeCache{
			releaseID:    rec.release.ID,
			info:         facts,
			points:       indexPointsOf(points),
			needsAugment: true,
			nzbHash:      nzbHash(nzb),
		}
	}
	// The next prepare reads this copy. The durable row, including its NZB
	// rename, follows after the response so a slow disk cannot stall it.
	m.resume.noteLive(rec, nzb, liveProbe)
	m.beginResumeSave()
	go func() {
		defer m.endResumeSave()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.resume.putWithProbe(ctx, rec, nzb, points, info); err != nil {
			m.log.Warn("save resume record failed", "key", rec.key, "err", err)
		}
		m.persistReleaseProbe(ctx, rec.release, nzb, info)
		if rec.key.IsPluginKey() {
			m.persistItemProbe(ctx, rec.key, info)
		}
	}()
}

// ResumePoint returns the release that last served (or was prefetched for)
// a user's title, with its NZB; nil when there is none.
func (m *Manager) ResumePoint(ctx context.Context, userID int64, key core.RatingKey) (*ResumePoint, error) {
	return m.resumePoint(ctx, userID, key, true)
}

// resumePoint is ResumePoint. Without payload, a stored point whose payload
// this process already read, verified and matched to the title comes back
// with no NZB and no keyframes (its hash is kept). Reading a stored payload takes a guarded
// ownership transaction, and list and Home views showing in-progress titles'
// tracks did that for every title on every request.
func (m *Manager) resumePoint(ctx context.Context, userID int64, key core.RatingKey, payload bool) (*ResumePoint, error) {
	if m.resume == nil {
		return nil, nil
	}
	var title *core.Title
	if key.Kind() == core.KindEpisode {
		title = &core.Title{Key: key, Type: core.TypeEpisode, Season: key.Season(), Episode: key.Episode()}
	}
	if live := m.resume.livePoint(userID, key); live != nil {
		if m.releaseBlocked(ctx, live.Release.ID) {
			return nil, nil
		}
		if title != nil && (!releaseMatchesTitle(live.Release, title) || !payloadMatchesTitle(live.NZB, title)) {
			return nil, nil
		}
		if payload {
			if pts, err := m.resume.loadIndex(ctx, userID, key, live.Release.ID); err == nil {
				live.Keyframes = pts
			}
		}
		return live, nil
	}
	rec, err := m.resume.get(ctx, userID, key)
	if err != nil || rec == nil {
		return nil, err
	}
	if m.releaseBlocked(ctx, rec.releaseID) {
		return nil, nil
	}
	if title != nil && !releaseMatchesTitle(rec.release, title) {
		return nil, nil
	}
	if !payload && rec.nzbHash != "" && m.resume.payloadStillChecked(rec.nzbHash, key) {
		return rec.point(nil), nil // the track view reads no keyframes either
	}
	nzb, err := m.resume.readResumeNZB(ctx, rec)
	if err != nil {
		return nil, err
	}
	if title != nil && !payloadMatchesTitle(nzb, title) {
		return nil, nil
	}
	if rec.nzbHash != "" {
		m.resume.notePayloadChecked(rec.nzbHash, key)
	}
	point := rec.point(nzb)
	if payload {
		point.Keyframes, err = m.resume.loadIndex(ctx, userID, key, rec.releaseID)
		if err != nil {
			return nil, err
		}
	}
	return point, nil
}

// Remember stores a resolution for a title the user has not played yet
// (a prefetched next episode) so the eventual Prepare opens the same,
// already warm release. An existing record for the same profile is kept.
func (m *Manager) Remember(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, res *core.Resolution) error {
	if m.resume == nil || user == nil || res == nil || len(res.NZB) == 0 {
		return nil
	}
	prof := m.selectProfile(caps, user, key)
	profile := profileKey(prof)
	// Profiles that do not compare original-language audio keep the
	// previous resume path: no catalogue read, and the language column
	// stays blank.
	current := ""
	if releasepolicy.UsesOriginalAudio(prof.ReleaseRules) {
		lang, err := m.catalogueLanguage(ctx, key)
		if err != nil {
			return err
		}
		current = lang
	}
	rec, err := m.resume.get(ctx, user.ID, key)
	if err != nil {
		return err
	}
	if rec != nil && rec.profile == profile && rememberedOriginalAudioCurrent(prof, rec.originalLanguage, current, rec.release) && rememberedCodecCurrent(rec.release, caps) && m.resumeSourceAllowed(rec.release) && sourceFitsBitrate(rec.size, rec.duration, 0, caps.MaxBitrateKbps) {
		return nil
	}
	return m.resume.save(ctx, resumeRecord{
		userID:           user.ID,
		key:              key,
		release:          res.Release,
		profile:          profile,
		originalLanguage: current,
		updatedAt:        m.now(),
	}, res.NZB)
}

// ResumeFor is ResumePoint restricted to a record resolved for the quality
// profile this client and user would get, i.e. the release a Prepare from
// that client would open.
func (m *Manager) ResumeFor(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) (*ResumePoint, error) {
	return m.resumeFor(ctx, user, key, caps, true)
}

// resumeFor is ResumeFor; payload is as for resumePoint.
func (m *Manager) resumeFor(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, payload bool) (*ResumePoint, error) {
	if user == nil {
		return nil, nil
	}
	rp, err := m.resumePoint(ctx, user.ID, key, payload)
	if err != nil || rp == nil {
		return nil, err
	}
	prof := m.selectProfile(caps, user, key)
	current := ""
	if releasepolicy.UsesOriginalAudio(prof.ReleaseRules) {
		lang, err := m.catalogueLanguage(ctx, key)
		if err != nil {
			return nil, nil
		}
		current = lang
	}
	if rp.profileKey != profileKey(prof) || !rememberedOriginalAudioCurrent(prof, rp.originalLanguage, current, rp.Release) || !rememberedCodecCurrent(rp.Release, caps) || !m.resumeSourceAllowed(rp.Release) || !sourceFitsBitrate(rp.Size, rp.Duration, 0, caps.MaxBitrateKbps) {
		return nil, nil
	}
	probe := m.persistedProbeInfoHash(ctx, user.ID, key, rp.Release.ID, rp.payloadHash())
	var info core.MediaInfo
	if probe != nil {
		info = probe.info
	}
	if exceedsRuleHeight(caps.QualityRuleMaxHeight, rp.Release, info) ||
		(probe != nil && !rememberedInfoCodecCurrent(info, caps)) {
		return nil, nil
	}
	rp.Profile = prof.Name // name from the profile object, not the stored column
	return rp, nil
}

func titleLanguage(t *core.Title) string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.Language)
}

func (m *Manager) catalogueLanguage(ctx context.Context, key core.RatingKey) (string, error) {
	if m.deps.Catalog == nil {
		return "", fmt.Errorf("playback: catalogue unavailable: %w", core.ErrNotFound)
	}
	t, err := m.deps.Catalog.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("playback: title %s: %w", key, core.ErrNotFound)
	}
	return titleLanguage(t), nil
}

// rememberedOriginalAudioCurrent reports whether a remembered release may
// open a new selection. Profiles that do not compare original-language
// audio ignore catalogue language, including a blank column left by
// migration. When they do, a blank language is the pre-migration row and
// is not reused. A known language has to be the one this selection sees,
// and the release has to still satisfy Require and Avoid. Prefer-only rows
// are dropped when that language changed so the new selection can rank
// again. An already open stream is not rewritten. The stored profile
// column stays Name|<16 hex>.
func rememberedOriginalAudioCurrent(profile core.QualityProfile, stored, current string, rel core.Release) bool {
	if !releasepolicy.UsesOriginalAudio(profile.ReleaseRules) {
		return true
	}
	current = strings.TrimSpace(current)
	stored = strings.TrimSpace(stored)
	if stored == "" || stored != current {
		return false
	}
	pol, err := releasepolicy.Compile(profile.ReleaseRules)
	if err != nil {
		return false
	}
	return !pol.Evaluate(rel, releasepolicy.Context{OriginalLanguage: current}).Rejected
}

func rememberedCodecCurrent(rel core.Release, caps core.ClientCaps) bool {
	if caps.DeferSourceCodecValidation || len(caps.VideoCodecs) == 0 {
		return true
	}
	return rel.Info.VideoCodec == "" || caps.SupportsVideo(rel.Info.VideoCodec)
}

func rememberedInfoCodecCurrent(info core.MediaInfo, caps core.ClientCaps) bool {
	if caps.DeferSourceCodecValidation || len(caps.VideoCodecs) == 0 {
		return true
	}
	v := info.Video()
	return v == nil || v.Codec == "" || caps.SupportsVideo(v.Codec)
}

func permanentSourceFailure(err error) bool {
	return errors.Is(err, core.ErrOriginalLost) || errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, core.ErrReleaseRejected)
}
func recoverableSourceFailure(err error) bool {
	if permanentSourceFailure(err) || errors.Is(err, core.ErrSourceUnavailable) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrNoProgress) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

func audioExcluded(excluded []core.Release, rel core.Release) bool {
	for _, ex := range excluded {
		if core.SameReleaseContent(ex, rel) {
			return true
		}
	}
	return false
}

func (m *Manager) effectiveResolveUser(ctx context.Context, u core.User) core.User {
	if m.deps.ResolveViewerLanguages != nil {
		return *m.deps.ResolveViewerLanguages(ctx, &u)
	}
	return u
}

// probedAudioAcceptable reports whether a probed release carries
// at least one audio stream in the viewer's wanted language (or an untagged
// audio stream). When every audio stream is explicitly tagged in a non-wanted
// language (e.g. a Spanish/Portuguese ".DUAL" release on an English server),
// openInitial tries the next candidate while keeping the first healthy
// generation as a fallback if no alternate carries the wanted language.
func (m *Manager) probedAudioAcceptable(s *session, profile core.QualityProfile, info core.MediaInfo) bool {
	if s.title == nil || info.Estimated {
		return true
	}
	orig := strings.TrimSpace(s.title.Language)
	if orig == "" && s.title.ShowKey != 0 && m.deps.Catalog != nil {
		if cached, ok := m.deps.Catalog.(interface {
			CachedGet(context.Context, core.RatingKey) (*core.Title, error)
		}); ok {
			if show, err := cached.CachedGet(m.ctx, s.title.ShowKey); err == nil && show != nil {
				orig = strings.TrimSpace(show.Language)
			}
		}
	}
	// The profile's Language (Radarr-style) decides first: Any accepts
	// every probed track, a specific language is the wanted one.
	profileLang := strings.ToLower(strings.TrimSpace(profile.Language))
	if profileLang == core.ProfileLanguageAny {
		return true
	}
	user := m.effectiveResolveUser(m.ctx, s.user)
	userAudio := user.AudioLanguages
	preferOriginal := releasepolicy.PrefersOriginalAudio(profile, &user)
	if preferOriginal && orig != "" {
		langs := releasepolicy.ProbedAudioLanguages(info.Streams)
		if len(langs) > 0 {
			return slices.ContainsFunc(langs, func(lang string) bool {
				return releaseparse.LanguageMatch(lang, orig) != releaseparse.LanguageNoMatch
			})
		}
		return true
	}
	specificProfileLang := profileLang != "" && profileLang != core.ProfileLanguageOriginal && releaseparse.KnownLanguage(profileLang)
	var wanted []string
	switch {
	case specificProfileLang:
		wanted = append(wanted, profileLang)
		if profile.EffectiveAudioLanguageMode() == core.AudioLanguageModePreferred && len(userAudio) > 0 {
			wanted = append(wanted, userAudio...)
		}
	case len(profile.PreferredLanguages) > 0:
		wanted = append(wanted, profile.PreferredLanguages...)
	case len(userAudio) > 0:
		wanted = append(wanted, userAudio...)
	case preferOriginal:
		// Preferring original language must not fall back to Catalog.Language.
	default:
		if serverLang := strings.TrimSpace(m.config().Catalog.Language); serverLang != "" {
			base, _, _ := strings.Cut(serverLang, "-")
			wanted = append(wanted, base)
		}
	}
	if len(wanted) == 0 {
		return true
	}
	audioCount := 0
	for _, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		audioCount++
		lang := strings.ToLower(strings.TrimSpace(st.Language))
		if lang == "" || lang == "und" || lang == "unk" || lang == "mul" || lang == "zxx" {
			return true
		}
		for _, w := range wanted {
			if releaseparse.LanguageMatch(lang, w) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return audioCount == 0
}

// A size/runtime estimate is sufficient for admission; never publish that
// estimate as a container duration or seek boundary.
func replacementBps(g *generation, duration time.Duration) float64 {
	info := g.infoSnapshot()
	duration = cmp.Or(info.Duration, duration)
	if duration > 0 && g.src.Size() > 0 {
		return float64(g.src.Size()) / duration.Seconds()
	}
	if info.Estimated {
		return 0
	}
	return mediaBps(info, g.src.Size())
}

func (m *Manager) rememberOpenFailure(s *session, r core.Release, err error) {
	if !permanentSourceFailure(err) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.openFailures == nil {
		m.openFailures = make(map[core.RatingKey]map[string]bool)
	}
	if len(m.openFailures) >= 256 && m.openFailures[s.ident.key] == nil {
		clear(m.openFailures)
	}
	if m.openFailures[s.ident.key] == nil {
		m.openFailures[s.ident.key] = map[string]bool{}
	}
	if len(m.openFailures[s.ident.key]) < 256 {
		m.openFailures[s.ident.key][r.ID] = true
	}
}
