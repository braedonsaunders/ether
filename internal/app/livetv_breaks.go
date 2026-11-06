package app

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// channelBreakInput uses playback's configured source engine and loopback range
// transport. Its dedicated prepared source is retired immediately after analysis.
func (a *App) channelBreakInput(ctx context.Context, key core.RatingKey, admittedOwner int64) (markers.BreakInput, error) {
	ctx, _ = core.WithSpeculative(ctx)
	ctx = debrid.AnalysisContext(ctx)
	u, err := a.channelBreakUser(ctx, admittedOwner)
	if err != nil {
		return markers.BreakInput{}, err
	}
	for _, source := range a.playback.Sessions() {
		if !source.PreparedOnly {
			return markers.BreakInput{}, core.ErrBudgetExhausted
		}
	}
	releaseAccount := func() {}
	providerBase := [3]int64{}
	if a.debrid != nil {
		providerBase[0], providerBase[1], providerBase[2] = a.debrid.AnalysisStats()
	}
	if a.debrid != nil {
		releaseAccount, err = a.debrid.BeginAnalysis(ctx)
		if err != nil {
			return markers.BreakInput{}, err
		}
	}
	caps := core.ClientCaps{Product: "Ether act-break analysis", ClientID: "ether-channel-break-analysis", MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}, Containers: []string{"mp4", "mkv"}}
	ps, retire, err := a.playback.PrepareSurfaceSource(ctx, u, key, caps, "", false)
	if err != nil {
		releaseAccount()
		return markers.BreakInput{}, err
	}
	releaseHold, err := a.playback.HoldBackgroundSource(ctx, ps.ID, retire)
	if err != nil {
		_ = a.playback.RetireSource(ps.ID, retire)
		releaseAccount()
		return markers.BreakInput{}, err
	}
	closeInput := func() { releaseHold(); _ = a.playback.RetireSource(ps.ID, retire); releaseAccount() }
	revision := core.SourceRevision(ps)
	if err = a.playback.PinSourceRevision(ctx, ps.ID, revision); err != nil {
		closeInput()
		return markers.BreakInput{}, err
	}
	closePinned := func() { a.playback.UnpinSourceRevision(ps.ID); closeInput() }
	info := ps.Info
	info.Size = ps.Source.Size()
	transcode.ProbeTrailer(ctx, ps.Source, ps.Source.Size(), &info)
	snapshot := *ps
	snapshot.Info = info
	input := markers.PlaybackBreakInput(&snapshot)
	input.URL = a.playback.InputURL(ps.ID) + "?background=1"
	input.Close = closePinned
	input.BytesRead = a.playback.BackgroundReadMeter(ps.ID)
	input.ReadErrors = a.playback.BackgroundErrorMeter(ps.ID)
	input.Drain = func(ctx context.Context) error {
		if err := a.playback.DrainBackgroundSource(ctx, ps.ID); err != nil {
			return err
		}
		if a.debrid != nil {
			return a.debrid.DrainAnalysis(ctx)
		}
		return nil
	}
	if a.debrid != nil && ps.Resolution != nil && debrid.IsPayload(ps.Resolution.NZB) {
		input.ProviderStats = a.debrid.AnalysisStats
		input.ProviderStatsBase = &providerBase
		if payload, err := debrid.DecodePayload(ps.Resolution.NZB); err == nil {
			input.ShouldYield = func() bool { return !a.debrid.AnalysisAllowed(payload.Provider) }
		}
	}
	return input, nil
}

// Background analysis may reuse only the identity of an admitted save. Always
// resolve the current real owner first; closing setup revokes the stand-in.
func (a *App) channelBreakUser(ctx context.Context, admittedOwner int64) (*core.User, error) {
	u, err := a.users.Owner(ctx)
	if err == nil {
		return u, nil
	}
	c := a.cfg.Read()
	if errors.Is(err, core.ErrNotFound) && admittedOwner == core.SetupOwnerID && !c.SetupComplete && !c.Plex.Claimed && c.Plex.OwnerPlexID == 0 {
		return &core.User{ID: core.SetupOwnerID, Username: "setup-owner", IsOwner: true}, nil
	}
	return nil, err
}
