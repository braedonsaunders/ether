package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/playback"
)

func (a *App) buildLiveTV() error {
	var tc core.Transcoder
	if a.transcoder != nil {
		tc = a.transcoder
	}
	var avail core.AvailabilitySource
	if a.resolver != nil {
		avail = a.resolver
	}
	svc, err := livetv.New(livetv.Options{Now: parityClock(), DB: a.db, Sealer: a.cfg, Catalog: a.catalog, Collections: a.collections, Playback: a.playback, Transcoder: tc, Availability: avail, Allowed: a.safety.Allowed, CanPlay: a.safety.CanPlay, StreamLimit: func(u *core.User) int { return playback.UserStreamLimit(a.cfg.Read(), a.safety.UserPolicy(u)) }, Warm: func(ctx context.Context, key core.RatingKey) error {
		if !a.registrar.Status().PlexPass {
			return core.ErrUnauthorized
		}
		u, err := a.users.Owner(ctx)
		if err != nil {
			return err
		}
		a.playback.PreResolve(ctx, u, key, core.ClientCaps{Product: "Ether Live TV", ClientID: "ether-channel-prewarm", MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}, Containers: []string{"mp4", "mkv"}})
		return nil
	}})
	if err != nil {
		return err
	}
	a.livetv = svc
	a.closers = append(a.closers, func() { svc.Close() })
	return nil
}
