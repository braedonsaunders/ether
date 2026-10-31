package app

import (
	"context"
	"net/netip"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	experiencepkg "github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/playback"
)

func (a *App) viewerCapabilities(local *identity.Local, experience frontend.Experience) frontend.ViewerCapabilitiesFunc {
	return func(ctx context.Context, viewer *frontend.Viewer, _ frontend.Device, ip netip.Addr) (frontend.ViewerCapabilities, error) {
		var out frontend.ViewerCapabilities
		if viewer == nil || viewer.ID <= 0 {
			return out, core.ErrUnauthorized
		}
		v, err := a.users.ActiveUser(ctx, viewer.ID)
		if err != nil || v == nil {
			return out, core.ErrUnauthorized
		}
		if a.safety != nil {
			v = a.safety.UserPolicy(v)
		}
		vcopy := *v
		vcopy.Delegated = viewer.Delegated
		v = &vcopy
		owner := v.IsOwner && !v.Delegated
		out.IsOwner = &owner
		if ip.IsValid() && !ip.IsUnspecified() && !ip.IsMulticast() {
			remote := !identity.LAN(ip)
			out.PeerRemote = &remote
		}
		if a.safety != nil {
			can := a.safety.CanPlay(v, true, time.Now())
			out.CanPlayRemote = &can
			if out.PeerRemote != nil {
				play := a.safety.CanPlay(v, *out.PeerRemote, time.Now())
				out.CanPlay = &play
			}
			tags := append([]string{}, a.safety.ForUser(v).BlockedKeywords...)
			out.BlockedTags = &tags
			rating := a.safety.UserPolicy(v).MaxContentRating // the stricter of account and policy
			out.MaxRating = &rating
			cap := a.safety.BitrateLimit(v)
			if remoteCap := a.cfg.Get().Transcode.RemoteMaxBitrateKbps; remoteCap > 0 && (cap == 0 || remoteCap < cap) {
				cap = remoteCap
			}
			out.RemoteBitrateKbps = &cap
		}
		limit := playback.UserStreamLimit(a.cfg.Get(), v)
		out.MaxActiveSessions = &limit
		transcode := a.transcoder != nil
		out.CanTranscode = &transcode
		out.CanTranscodeAudio = &transcode
		out.CanTranscodeVideo = &transcode
		out.CanRemux = &transcode
		cfg := a.cfg.Get()
		download := experiencepkg.DownloadsAllowed(v, cfg.Downloads.Enabled && a.downloadRemuxer != nil && a.playback != nil, cfg.Downloads.AllowFriends, a.safety)
		out.CanDownload = &download

		if a.livetv != nil {
			out.CanManageLiveTV = &owner
			if p, e := a.livetv.Policy(ctx, v.ID); e == nil {
				allowed := !p.Disabled
				out.CanUseLiveTV = &allowed
			}
		}
		if local != nil {
			if has, e := local.HasPassword(ctx, v.ID); e == nil {
				out.HasLocalPassword = &has
			}
		}
		if experience != nil {
			if views, e := experience.Views(ctx, v); e == nil {
				if views == nil {
					views = []frontend.View{}
				}
				out.AllowedLibraries = &views
			}
		}
		return out, nil
	}
}
