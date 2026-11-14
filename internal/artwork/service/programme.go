package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

// ProgrammeImages describes only artwork actually supplied with an airing.
// References contain no provider URLs; no image or title fetch occurs here.
func (a Source) ProgrammeImages(viewer *core.Viewer, p livetv.Programme) []frontend.Image {
	if viewer == nil || p.ID == "" || p.ChannelID == "" {
		return nil
	}
	req := frontend.ArtworkRequest{Viewer: viewer, Channel: p.ChannelID, Programme: p.ID}
	var out []frontend.Image
	for _, source := range []struct{ kind, path string }{{frontend.ImageStill, p.Item.Thumb}, {frontend.ImageBackdrop, p.Item.Art}} {
		if source.path == "" {
			continue
		}
		ref := artworkReference(req, source.kind, p.ChannelID+"\x00"+p.ID+"\x00"+source.path)
		out = append(out, frontend.Image{Kind: source.kind, Path: ref, Tag: ref, Subject: p.ID})
	}
	return out
}

func (a Source) programmeEntries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	source, ok := a.LiveTV.(interface {
		GuideForChannels(context.Context, *core.User, time.Time, time.Time, []string) ([]livetv.Programme, error)
	})
	if !ok {
		return nil, false, core.ErrNotAvailable
	}
	// Programme identity is channelID-startMS, shared with Plex and XMLTV.
	startText, ok := strings.CutPrefix(req.Programme, req.Channel+"-")
	startMS, err := strconv.ParseInt(startText, 10, 64)
	if !ok || err != nil || startMS <= 0 {
		return nil, false, core.ErrNotFound
	}
	start := time.UnixMilli(startMS)
	programmes, err := source.GuideForChannels(ctx, req.Viewer, start, start.Add(time.Millisecond), []string{req.Channel})
	if err != nil {
		return nil, false, err
	}
	for _, p := range programmes {
		if p.ID != req.Programme || p.ChannelID != req.Channel {
			continue
		}
		images := a.ProgrammeImages(req.Viewer, p)
		out := make([]frontendArtworkEntry, 0, len(images))
		for _, image := range images {
			path := p.Item.Thumb
			if image.Kind == frontend.ImageBackdrop {
				path = p.Item.Art
			}
			out = append(out, frontendArtworkEntry{image: image, programmePath: path})
		}
		return out, true, nil
	}
	return nil, false, core.ErrNotFound
}
