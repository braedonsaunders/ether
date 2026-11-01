package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (a *App) buildPlayback(ctx context.Context, cfg config.Config) error {
	log := a.log
	dataDir := a.opts.DataDir
	var err error
	// Transcoder. A missing ffmpeg is not fatal: direct play still works.
	// A fixture transcoder is shared by Plex and every other protocol without
	// starting ffmpeg.
	tracePlayback := os.Getenv("ETHER_TRACE_PLAYBACK") == "1"
	a.tracePlayback = tracePlayback
	if a.opts.FixtureTranscoder == nil {
		a.transcoder, err = transcode.New(cfg.Transcode, log, transcode.WithRuntimeDir(filepath.Join(dataDir, "tools")), transcode.WithTracePlayback(tracePlayback), transcode.WithEncoderObserver(func(sessionID string, started bool, excerpt string) {
			if a.support == nil {
				return
			}
			kind := diagnostics.KindEncoderFailed
			if started {
				kind = diagnostics.KindEncoderStarted
			}
			a.support.Observe(diagnostics.Event{Kind: kind, SessionKey: sessionID, FFmpegExcerpt: excerpt})
		}), transcode.WithRunStartHandler(func(input string, from, to time.Duration) {
			if a.playback != nil {
				a.playback.WarmInput(input, from, to)
			}
		}), transcode.WithInputFailureHandler(func(id, input string, at time.Duration, failure error) {
			if ctx.Err() != nil {
				return
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			log.Warn("recovering failed encoder input", "session", id, "at", at, "err", failure)
			var recoverErr error
			if a.frontends != nil && a.frontends.broker != nil && a.frontends.broker.HasBinding(id) {
				recoverErr = a.frontends.broker.Recover(rctx, id, input, at)
			}
			if recoverErr != nil {
				log.Warn("encoder recovery exhausted", "session", id, "err", recoverErr)
			}
		}))
		if err != nil {
			log.Warn("transcoder unavailable; only direct play will work", "err", err)
			a.transcoder = nil
		} else {
			a.closers = append(a.closers, func() { a.transcoder.Close() })
		}
	}
	var tc core.Transcoder = directOnly{}
	if got := a.activeTranscoder(); got != nil {
		tc = got
	}

	a.trackprefs = trackprefs.New(a.db, trackprefs.Options{
		ShowName: func(ctx context.Context, id int64) string {
			if title, err := a.catalog.CachedTitle(ctx, core.ShowKey(id)); err == nil {
				return title.Title
			}
			return ""
		},
		SyncSubtitleLanguages: a.syncSubtitleLanguages,
		ServerLanguage: func() string {
			if a.cfg == nil {
				return ""
			}
			return a.cfg.Get().Catalog.Language
		},
		PlexLayer: func(_ context.Context, user int64) (trackprefs.PlexLayer, bool) {
			if a.users == nil {
				return trackprefs.PlexLayer{}, false
			}
			d, ok := a.users.PlexDefaults(user)
			if !ok || !d.HasProfile {
				return trackprefs.PlexLayer{}, false
			}
			return trackprefs.PlexLayer{
				AudioLanguage:    d.AudioLanguage,
				AutoSelectAudio:  d.AutoSelectAudio,
				SubtitleLanguage: d.SubtitleLanguage,
				SubtitleMode:     d.EtherMode(),
			}, true
		},
	})
	a.markers = markers.New(markers.Options{DB: a.db, Logger: log.With("component", "markers"), Config: func() markers.Lookup {
		c := a.cfg.Get().Markers
		return markers.Lookup{Enabled: c.TheIntroDB, APIKey: c.APIKey, IntroOff: !c.OfferIntro(), CreditsOff: !c.OfferCredits()}
	}, Aligner: a.subtitles.TimelineAligner(), Dialogue: a.markerDialogue})
	a.buildAdoption()
	// Playback and prefetch.
	a.playback, err = playback.New(playback.Deps{
		AugmentMedia: a.enrichSubtitles,
		NativeProbe: func(ctx context.Context, src core.MediaSource) (*core.MediaInfo, error) {
			return transcode.ProbeSource(ctx, src, src.Size())
		},
		NativeTrailer: func(ctx context.Context, src core.MediaSource, info *core.MediaInfo) bool {
			return transcode.ProbeTrailer(ctx, src, src.Size(), info)
		},
		Resolver:   a.resolver,
		Engine:     playbackEngine(a),
		Transcoder: tc,
		Catalog:    a.catalog,
		Cache:      a.cache,
		State:      a.state,
		SelectProfile: func(caps core.ClientCaps, u *core.User) core.QualityProfile {
			return resolver.SelectProfile(a.cfg.Read().Quality, caps, u)
		},
		FetchNZB: playbackFetchNZB(a, dataDir),
		Aligner: func() core.TimelineAligner {
			if a.subtitles == nil {
				return nil
			}
			return a.subtitles.TimelineAligner()
		}(),
		// Transcode sessions are keyed by the client session id. The broker
		// owns published sessions; Live TV and the process transcoder cover
		// sessions that were not published there.
		HLSLookup: func(id string) (core.HLSSession, bool) {
			if a.frontends != nil && a.frontends.broker != nil {
				if hs, ok := a.frontends.broker.TranscodeFor(context.Background(), id); ok {
					return hs, true
				}
			}
			if a.livetv != nil {
				if hs, ok := a.livetv.TranscodeFor(id); ok {
					return hs, true
				}
			}
			if got := a.activeTranscoder(); got != nil {
				return got.Session(id)
			}
			return nil, false
		},
		DB:            a.db,
		Logger:        log,
		Cluster:       a.clusterNode(),
		DescribeGroup: a.describeLiveGroup,
		Availability:  a.dbAvail,
	}, cfg, playback.Options{Now: parityClock(), DataDir: dataDir, PayloadReplayGrace: 5 * time.Second, VerifyNativeProbe: os.Getenv("ETHER_VERIFY_NATIVE_PROBE") == "1", TracePlayback: tracePlayback})
	if err != nil {
		return fmt.Errorf("playback: %w", err)
	}
	a.closers = append(a.closers, func() { a.playback.Close() })
	a.cache.Protect(a.protectedReleases)

	return nil
}

func (a *App) activeTranscoder() core.Transcoder {
	if a.opts.FixtureTranscoder != nil {
		return a.opts.FixtureTranscoder
	}
	if a.transcoder != nil {
		return a.transcoder
	}
	return nil
}

func playbackEngine(a *App) core.Engine {
	if a.opts.FixtureMedia != nil {
		return a.opts.FixtureMedia
	}
	return parityEngine(a.media, a.opts.DataDir)
}

func playbackFetchNZB(a *App, dataDir string) func(context.Context, core.Release) ([]byte, error) {
	if a.opts.FixtureMedia != nil {
		return a.opts.FixtureMedia.FetchNZB
	}
	return parityIndexer(a.sources, dataDir).FetchNZB
}
