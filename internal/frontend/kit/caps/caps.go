// Package caps builds neutral client capabilities from protocol profiles.
package caps

import (
	"context"
	"mime"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// DirectPlayProfile is the common Jellyfin/Emby DeviceProfile direct-play shape.
type DirectPlayProfile struct {
	Container  string `json:"Container"`
	VideoCodec string `json:"VideoCodec"`
	AudioCodec string `json:"AudioCodec"`
	Type       string `json:"Type"`
}
type DeviceProfile struct {
	DirectPlayProfiles  []DirectPlayProfile `json:"DirectPlayProfiles"`
	MaxStreamingBitrate int                 `json:"MaxStreamingBitrate"`
}

// DeviceInfo is the neutral subset of a Silo or other client's declaration.
type DeviceInfo struct {
	Product, Platform, Version, Device, DeviceName, ClientID string
	VideoCodecs, AudioCodecs, Containers                     []string
	MaxHeight, MaxBitrateKbps                                int
	HDR10, DolbyVision                                       bool
}

// Layer applies learned capabilities or owner rules after declared capabilities.
type Layer func(context.Context, core.ClientCaps) (core.ClientCaps, error)
type Builder struct{ Learn, Rules Layer }

func add(dst []string, value string) []string {
	for _, s := range strings.Split(value, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		exists := false
		for _, old := range dst {
			if old == s {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, s)
		}
	}
	return dst
}

// Build merges declarations, then applies learning followed by owner rules.
func (b Builder) Build(ctx context.Context, device DeviceInfo, profile DeviceProfile, protocolInfo []string, userAgent string) (core.ClientCaps, error) {
	c := core.ClientCaps{Product: device.Product, Platform: device.Platform, Version: device.Version, Device: device.Device, DeviceName: device.DeviceName, ClientID: device.ClientID, MaxHeight: device.MaxHeight, MaxBitrateKbps: device.MaxBitrateKbps, HDR10: device.HDR10, DolbyVision: device.DolbyVision}
	if c.Product == "" {
		c.Product = userAgent
	}
	for _, s := range device.VideoCodecs {
		c.VideoCodecs = add(c.VideoCodecs, s)
	}
	for _, s := range device.AudioCodecs {
		c.AudioCodecs = add(c.AudioCodecs, s)
	}
	for _, s := range device.Containers {
		c.Containers = add(c.Containers, s)
	}
	for _, p := range profile.DirectPlayProfiles {
		c.Containers = add(c.Containers, p.Container)
		c.VideoCodecs = add(c.VideoCodecs, p.VideoCodec)
		c.AudioCodecs = add(c.AudioCodecs, p.AudioCodec)
	}
	if profile.MaxStreamingBitrate > 0 {
		kbps := profile.MaxStreamingBitrate / 1000
		if kbps == 0 {
			kbps = 1
		}
		if c.MaxBitrateKbps == 0 || kbps < c.MaxBitrateKbps {
			c.MaxBitrateKbps = kbps
		}
	}
	for _, p := range protocolInfo {
		fields := strings.SplitN(p, ":", 4)
		if len(fields) != 4 {
			continue
		}
		content, _, err := mime.ParseMediaType(fields[2])
		if err != nil {
			continue
		}
		switch content {
		case "video/mp4":
			c.Containers = add(c.Containers, "mp4")
		case "video/x-matroska":
			c.Containers = add(c.Containers, "mkv")
		case "video/mpeg":
			c.Containers = add(c.Containers, "mpeg")
		case "video/mp2t":
			c.Containers = add(c.Containers, "mpegts")
		case "audio/mpeg":
			c.AudioCodecs = add(c.AudioCodecs, "mp3")
		case "audio/flac":
			c.AudioCodecs = add(c.AudioCodecs, "flac")
		}
	}
	var err error
	if b.Learn != nil {
		c, err = b.Learn(ctx, c)
		if err != nil {
			return core.ClientCaps{}, err
		}
	}
	if b.Rules != nil {
		c, err = b.Rules(ctx, c)
		if err != nil {
			return core.ClientCaps{}, err
		}
	}
	return c, nil
}
