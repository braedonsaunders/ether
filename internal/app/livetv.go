package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/playback"
	"time"
)

func (a *App) buildLiveTV() error {
	if err := a.buildOnAir(); err != nil {
		return err
	}
	var tc core.Transcoder
	if a.transcoder != nil {
		tc = a.transcoder
	}
	var avail core.AvailabilitySource
	if a.resolver != nil {
		avail = a.resolver
	}
	ffmpeg := ""
	if a.transcoder != nil {
		ffmpeg = a.transcoder.FFmpegPath()
	}
	svc, err := livetv.New(livetv.Options{BreakInput: a.channelBreakInput, BreakAnalyzer: &markers.BreakAnalyzer{DB: a.db, FFmpeg: ffmpeg, OnEvidence: a.opts.BreakEvidenceObserver}, GuideAI: a.llm, Reels: a.onairReelSnapshot, Graphics: a.onair, InputUser: a.liveTVInputUser, DefaultStyle: func() string { return a.cfg.Read().LiveTV.DefaultStyleID }, StyleScreens: a.onair.StyleScreens, Screens: func() config.LiveTVScreens { return a.cfg.Read().LiveTV.Screens }, PersonalChanged: func(viewer int64) {

		if srv := a.currentPlexServer(); srv != nil {
			srv.NotifyLiveLineupFor(viewer)
		}
	}, Picker: a.personalize, Watchlist: a.watchlist, State: a.state, Users: a.users, Logger: a.log.With("component", "livetv"), PlexGuideToken: func() string { return a.cfg.Read().Plex.Token }, Now: parityClock(), DB: a.db, Sealer: a.cfg, Catalog: a.catalog, Collections: a.collections, Playback: a.playback, Transcoder: tc, Availability: avail, Allowed: a.safety.Allowed, CanPlay: a.safety.CanPlay, LogoIndexURL: func() string { return a.cfg.Read().LiveTV.LogoIndexURL }, HealthSweepMinutes: func() int { return a.cfg.Read().LiveTV.HealthSweepMinutes }, StreamLimit: func(u *core.User) int { return playback.UserStreamLimit(a.cfg.Read(), a.safety.UserPolicy(u)) }, Warm: func(ctx context.Context, key core.RatingKey, at time.Duration) error {
		if !a.registrar.Status().PlexPass {
			return core.ErrUnauthorized
		}
		u, err := a.users.Owner(ctx)
		if err != nil {
			return err
		}
		a.playback.Prewarm(ctx, u, key, core.ClientCaps{Product: "Ether Live TV", ClientID: "ether-channel-prewarm", MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}, Containers: []string{"mp4", "mkv"}}, core.PrewarmOptions{Reason: core.PrewarmDetail, StartOffset: at, DuringPlayback: true, PrefetchBytes: 32 << 20})
		return nil
	}})
	if err != nil {
		return err
	}
	a.livetv = svc
	a.cfg.Subscribe(func(config.Config) {
		if err := svc.RefreshGuide(context.Background()); err != nil {
			a.log.Warn("live guide configuration refresh failed", "error", err)
		}
	})
	a.onair.Start()
	a.closers = append(a.closers, func() { svc.Close() })
	return nil
}

// liveTVInputUser supplies source identity only after admin auth has admitted
// a tokenless LAN setup request. It never grants channel or HTTP access.
func (a *App) liveTVInputUser(ctx context.Context) (*core.User, error) {
	u, err := a.users.Owner(ctx)
	if err == nil {
		return u, nil
	}
	c := a.cfg.Read()
	access, admitted := admin.RequestAccess(ctx)
	if errors.Is(err, core.ErrNotFound) && admitted && access.SetupPlayback && !access.Owner && !access.SetupComplete && !c.SetupComplete && !c.Plex.Claimed && c.Plex.OwnerPlexID == 0 {
		return &core.User{ID: core.SetupOwnerID, Username: "setup-owner", IsOwner: true}, nil
	}
	return nil, err
}
