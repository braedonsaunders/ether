package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/plexguide"
)

func (a *App) buildLiveTV() error {
	// Resolve the additive switch once from existing owner state, before workers start.
	if a.cfg.Read().LiveTV.Enabled == nil {
		// Existing setup is an opt-in, even when every saved channel is disabled.
		// Probe only for existence and never persist OFF after a database error.
		var enabled bool
		if err := a.db.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM livetv_sources) OR EXISTS(SELECT 1 FROM livetv_channels)`).Scan(&enabled); err != nil {
			return err
		}
		if err := a.cfg.Update(func(c *config.Config) error { c.LiveTV.Enabled = &enabled; return nil }); err != nil {
			return err
		}
	}
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
	svc, err := livetv.New(livetv.Options{Enabled: func() bool { v := a.cfg.Read().LiveTV.Enabled; return v != nil && *v }, SessionsChanged: a.notifySessionsChanged, SourcesChanged: a.publishDashboard, VODChanged: func() {
		if a.resolver != nil {
			a.resolver.SourcesChanged(context.Background())
		}
	}, BreakInput: a.channelBreakInput, BreakAnalyzer: &markers.BreakAnalyzer{DB: a.db, FFmpeg: ffmpeg, OnEvidence: a.opts.BreakEvidenceObserver, Hardware: func() core.HWAccel {
		if a.transcoder == nil {
			return core.HWAccel{}
		}
		return a.transcoder.HWAccel()
	}, ScanPolicy: func() markers.BreakScanPolicy {
		c := a.cfg.Read().LiveTV
		return markers.BreakScanPolicy{DailyBytes: c.BreakScanDailyBytes, ReleaseBytes: c.BreakScanReleaseBytes, LargeReleaseBytes: c.BreakScanLargeReleaseBytes, AllowLarge: c.BreakScanAllowLarge}
	}}, GuideAI: a.llm, Reels: a.onairReelSnapshot, Graphics: a.onair, InputUser: a.liveTVInputUser, StyleScreens: a.onair.StyleScreens, AudioTrack: a.liveTVAudioTrack, PersonalChanged: func(viewer int64) {
		a.frontends.notifyLiveLineup(viewer)
	}, Picker: a.personalize, Watchlist: a.watchlist, State: a.state, Users: a.users, Logger: a.log.With("component", "livetv"), GuideProvider: &guide.Client{DB: a.db, HTTPClient: egress.Client(egress.CapDirect, 20*time.Second), Token: func() string { return a.cfg.Read().Plex.Token }}, Now: parityClock(), Showcase: parityShowcase(), DB: a.db, Sealer: a.cfg, Catalog: a.catalog, Collections: a.collections, Playback: a.playback, Transcoder: tc, Availability: avail, CheckAvailability: func(ctx context.Context, key core.RatingKey) (bool, bool) {
		if a.resolver == nil {
			return false, false
		}
		title, err := a.catalog.Get(ctx, key)
		if err != nil {
			return false, false
		}
		if a.availability != nil {
			ctx = a.availability.ChannelCheckContext(ctx)
		}
		return a.resolver.CheckChannelAvailability(ctx, title)
	}, Allowed: a.safety.Allowed, CanPlay: a.safety.CanPlay, LogoIndexURL: func() string { return a.cfg.Read().LiveTV.LogoIndexURL }, HealthSweepMinutes: func() int { return a.cfg.Read().LiveTV.HealthSweepMinutes }, Stability: func() string { return a.cfg.Read().LiveTV.Stability }, StreamLimit: func(u *core.User) int { return playback.UserStreamLimit(a.cfg.Read(), a.safety.UserPolicy(u)) }, Warm: func(ctx context.Context, key core.RatingKey, at time.Duration) error {
		if !a.registrar.Status().PlexPass {
			return core.ErrUnauthorized
		}
		u, err := a.users.Owner(ctx)
		if err != nil {
			return err
		}
		a.playback.Prewarm(ctx, u, key, core.ClientCaps{Product: "Ether Live TV", ClientID: "ether-channel-prewarm", MaxHeight: 1080, MaxBitrateKbps: 6000, BroadcastBitrateKbps: 6000, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}, Containers: []string{"mp4", "mkv"}}, core.PrewarmOptions{Reason: core.PrewarmDetail, StartOffset: at, DuringPlayback: true, PrefetchBytes: 32 << 20})
		return nil
	}})
	if err != nil {
		return err
	}
	a.livetv = svc
	svc.SetOnChange(func() {
		a.frontends.notifyLiveLineup(0)
	})
	var enabledMu sync.Mutex
	wasEnabled := svc.Enabled()
	a.cfg.Subscribe(func(config.Config) {
		enabledMu.Lock()
		defer enabledMu.Unlock()
		enabled := svc.Enabled()
		if enabled {
			a.onair.SetEnabled(true)
		}
		if enabled && !wasEnabled {
			if err := svc.EnableShowcaseDefaults(context.Background()); err != nil {
				a.log.Warn("live showcase enable failed", "error", err)
			}
		}
		wasEnabled = enabled
		svc.ReconcileEnabled()
		a.onair.SetEnabled(svc.Enabled())
		if err := svc.RefreshGuide(context.Background()); err != nil {
			a.log.Warn("live guide configuration refresh failed", "error", err)
		}
	})
	a.onair.SetEnabled(svc.Enabled())
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

// liveTVAudioTrack chooses a channel programme's audio with the user's stream
// preferences, the same rules normal playback applies.
func (a *App) liveTVAudioTrack(ctx context.Context, user *core.User, key core.RatingKey, info core.MediaInfo, original string) int {
	if a.trackprefs == nil || user == nil {
		return -1
	}
	sel, err := a.trackprefs.Select(ctx, user.ID, key, info, original)
	if err != nil {
		a.log.Warn("livetv: stream preferences for channel audio", "err", err, "key", key)
		return -1
	}
	return sel.Audio
}
