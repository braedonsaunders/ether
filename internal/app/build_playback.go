package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
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
	a.transcoder, err = transcode.New(cfg.Transcode, log, transcode.WithRuntimeDir(filepath.Join(dataDir, "tools")), transcode.WithRunStartHandler(func(input string, from, to time.Duration) {
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
		} else if a.plexServer != nil {
			recoverErr = a.plexServer.RecoverTranscode(rctx, id, input, at)
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
	var tc core.Transcoder = directOnly{}
	if a.transcoder != nil {
		tc = a.transcoder
	}

	a.trackprefs = trackprefs.New(a.db, trackprefs.Options{ShowName: func(ctx context.Context, id int64) string {
		if title, err := a.catalog.CachedTitle(ctx, core.ShowKey(id)); err == nil {
			return title.Title
		}
		return ""
	}})
	a.markers = markers.New(markers.Options{DB: a.db, Logger: log.With("component", "markers"), Config: func() (bool, string) { c := a.cfg.Get().Markers; return c.TheIntroDB, c.APIKey }, Aligner: a.subtitles.TimelineAligner(), Dialogue: a.markerDialogue})
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
		Engine:     parityEngine(a.media, dataDir),
		Transcoder: tc,
		Catalog:    a.catalog,
		Cache:      a.cache,
		State:      a.state,
		SelectProfile: func(caps core.ClientCaps, u *core.User) core.QualityProfile {
			return resolver.SelectProfile(a.cfg.Get().Quality, caps, u)
		},
		FetchNZB: parityIndexer(a.sources, dataDir).FetchNZB,
		Aligner: func() core.TimelineAligner {
			if a.subtitles == nil {
				return nil
			}
			return a.subtitles.TimelineAligner()
		}(),
		// Transcode sessions are keyed by the client's session id; the Plex
		// front end (built below) maps playback sessions to them.
		HLSLookup: func(id string) (core.HLSSession, bool) {
			if a.frontends != nil && a.frontends.broker != nil {
				if hs, ok := a.frontends.broker.TranscodeFor(context.Background(), id); ok {
					return hs, true
				}
			}
			if a.plexServer != nil {
				if hs, ok := a.plexServer.TranscodeFor(id); ok {
					return hs, true
				}
			}
			if a.transcoder != nil {
				return a.transcoder.Session(id)
			}
			return nil, false
		},
		DB:            a.db,
		Logger:        log,
		Cluster:       a.clusterNode(),
		DescribeGroup: a.describeLiveGroup,
		Availability:  a.dbAvail,
	}, cfg, playback.Options{Now: parityClock(), DataDir: dataDir, VerifyNativeProbe: os.Getenv("ETHER_VERIFY_NATIVE_PROBE") == "1", TracePlayback: os.Getenv("ETHER_TRACE_PLAYBACK") == "1"})
	if err != nil {
		return fmt.Errorf("playback: %w", err)
	}
	a.closers = append(a.closers, func() { a.playback.Close() })
	a.cache.Protect(a.protectedReleases)

	return nil
}
