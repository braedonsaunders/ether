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
type ProfileCondition struct {
	Condition  string `json:"Condition"`
	Property   string `json:"Property"`
	Value      string `json:"Value"`
	IsRequired *bool  `json:"IsRequired"`
}
type TranscodingProfile struct {
	Container                 string             `json:"Container"`
	Type                      string             `json:"Type"`
	VideoCodec                string             `json:"VideoCodec"`
	AudioCodec                string             `json:"AudioCodec"`
	Protocol                  string             `json:"Protocol"`
	Context                   string             `json:"Context"`
	EstimateContentLength     bool               `json:"EstimateContentLength"`
	EnableMpegtsM2TsMode      bool               `json:"EnableMpegtsM2TsMode"`
	CopyTimestamps            bool               `json:"CopyTimestamps"`
	EnableSubtitlesInManifest bool               `json:"EnableSubtitlesInManifest"`
	TranscodeSeekInfo         string             `json:"TranscodeSeekInfo"`
	MaxAudioChannels          string             `json:"MaxAudioChannels"`
	MinSegments               int                `json:"MinSegments"`
	SegmentLength             int                `json:"SegmentLength"`
	BreakOnNonKeyFrames       *bool              `json:"BreakOnNonKeyFrames"`
	EnableAudioVbrEncoding    *bool              `json:"EnableAudioVbrEncoding"`
	Conditions                []ProfileCondition `json:"Conditions"`
}
type SubtitleProfile struct {
	Format    string `json:"Format"`
	Method    string `json:"Method"`
	DidlMode  string `json:"DidlMode"`
	Language  string `json:"Language"`
	Container string `json:"Container"`
}
type ContainerProfile struct {
	Type         string             `json:"Type"`
	Container    string             `json:"Container"`
	SubContainer string             `json:"SubContainer"`
	Conditions   []ProfileCondition `json:"Conditions"`
}
type CodecProfile struct {
	Type            string             `json:"Type"`
	Codec           string             `json:"Codec"`
	Container       string             `json:"Container"`
	SubContainer    string             `json:"SubContainer"`
	Conditions      []ProfileCondition `json:"Conditions"`
	ApplyConditions []ProfileCondition `json:"ApplyConditions"`
}
type DeviceProfile struct {
	DirectPlayProfiles                        []DirectPlayProfile  `json:"DirectPlayProfiles"`
	TranscodingProfiles                       []TranscodingProfile `json:"TranscodingProfiles"`
	SubtitleProfiles                          []SubtitleProfile    `json:"SubtitleProfiles"`
	ContainerProfiles                         []ContainerProfile   `json:"ContainerProfiles"`
	CodecProfiles                             []CodecProfile       `json:"CodecProfiles"`
	MaxStreamingBitrate                       int                  `json:"MaxStreamingBitrate"`
	AlwaysUseMostCompatibleTranscodingProfile bool                 `json:"AlwaysUseMostCompatibleTranscodingProfile"`
	MaxStaticBitrate                          int                  `json:"MaxStaticBitrate"`
	MusicStreamingTranscodingBitrate          int                  `json:"MusicStreamingTranscodingBitrate"`
	MaxStaticMusicBitrate                     int                  `json:"MaxStaticMusicBitrate"`
}

// DeviceInfo is the neutral subset of a Silo or other client's declaration.
type DeviceInfo struct {
	Protocol                                                 core.Protocol
	Product, Platform, Version, Device, DeviceName, ClientID string
	VideoCodecs, AudioCodecs, Containers                     []string
	SubtitleCodecs                                           []string
	DVProfiles                                               []int
	MaxHeight, MaxBitrateKbps                                int
	HDR10, DolbyVision                                       bool
	Remote, Download                                         bool
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

func conditions(input []ProfileCondition) []core.ClientProfileCondition {
	if input == nil {
		return nil
	}
	out := make([]core.ClientProfileCondition, 0, len(input))
	for _, c := range input {
		required := true
		if c.IsRequired != nil {
			required = *c.IsRequired
		}
		out = append(out, core.ClientProfileCondition{Operator: c.Condition, Property: c.Property, Value: c.Value, Required: required})
	}
	return out
}

func preserveProfiles(p DeviceProfile) *core.ClientCapabilityProfiles {
	if p.DirectPlayProfiles == nil && p.TranscodingProfiles == nil && p.SubtitleProfiles == nil && p.ContainerProfiles == nil && p.CodecProfiles == nil && p.MaxStaticBitrate == 0 && p.MusicStreamingTranscodingBitrate == 0 && p.MaxStaticMusicBitrate == 0 {
		return nil
	}
	out := &core.ClientCapabilityProfiles{MaxStaticBitrate: p.MaxStaticBitrate, MusicStreamingTranscodingBitrate: p.MusicStreamingTranscodingBitrate, MaxStaticMusicBitrate: p.MaxStaticMusicBitrate}
	if p.DirectPlayProfiles != nil {
		out.DirectPlay = make([]core.ClientDirectPlayProfile, 0, len(p.DirectPlayProfiles))
	}
	for _, v := range p.DirectPlayProfiles {
		out.DirectPlay = append(out.DirectPlay, core.ClientDirectPlayProfile{MediaType: strings.ToLower(v.Type), Containers: add(nil, v.Container), VideoCodecs: add(nil, v.VideoCodec), AudioCodecs: add(nil, v.AudioCodec)})
	}
	if p.TranscodingProfiles != nil {
		out.Transcoding = make([]core.ClientTranscodeProfile, 0, len(p.TranscodingProfiles))
	}
	for _, v := range p.TranscodingProfiles {
		out.Transcoding = append(out.Transcoding, core.ClientTranscodeProfile{MediaType: strings.ToLower(v.Type), Delivery: strings.ToLower(v.Protocol), Context: strings.ToLower(v.Context), Containers: add(nil, v.Container), VideoCodecs: add(nil, v.VideoCodec), AudioCodecs: add(nil, v.AudioCodec), EstimateContentLength: v.EstimateContentLength, MPEGTSM2TS: v.EnableMpegtsM2TsMode, CopyTimestamps: v.CopyTimestamps, SubtitlesInManifest: v.EnableSubtitlesInManifest, SeekInfo: v.TranscodeSeekInfo, MaxAudioChannels: v.MaxAudioChannels, MinSegments: v.MinSegments, SegmentLength: v.SegmentLength, BreakOnNonKeyFrames: v.BreakOnNonKeyFrames, AudioVBR: v.EnableAudioVbrEncoding, Conditions: conditions(v.Conditions)})
	}
	if p.SubtitleProfiles != nil {
		out.Subtitles = make([]core.ClientSubtitleProfile, 0, len(p.SubtitleProfiles))
	}
	for _, v := range p.SubtitleProfiles {
		out.Subtitles = append(out.Subtitles, core.ClientSubtitleProfile{Format: strings.ToLower(v.Format), Method: strings.ToLower(v.Method), DIDLMode: v.DidlMode, Language: v.Language, Container: v.Container})
	}
	if p.ContainerProfiles != nil {
		out.Containers = make([]core.ClientContainerProfile, 0, len(p.ContainerProfiles))
	}
	for _, v := range p.ContainerProfiles {
		out.Containers = append(out.Containers, core.ClientContainerProfile{MediaType: strings.ToLower(v.Type), Containers: add(nil, v.Container), SubContainers: add(nil, v.SubContainer), Conditions: conditions(v.Conditions)})
	}
	if p.CodecProfiles != nil {
		out.Codecs = make([]core.ClientCodecProfile, 0, len(p.CodecProfiles))
	}
	for _, v := range p.CodecProfiles {
		out.Codecs = append(out.Codecs, core.ClientCodecProfile{MediaType: strings.ToLower(v.Type), Codecs: add(nil, v.Codec), Containers: add(nil, v.Container), SubContainers: add(nil, v.SubContainer), Conditions: conditions(v.Conditions), ApplyConditions: conditions(v.ApplyConditions)})
	}
	return out
}

// Build merges declarations, then applies learning followed by owner rules.
func (b Builder) Build(ctx context.Context, device DeviceInfo, profile DeviceProfile, protocolInfo []string, userAgent string) (core.ClientCaps, error) {
	c := core.ClientCaps{Protocol: device.Protocol, Product: device.Product, Platform: device.Platform, Version: device.Version, Device: device.Device, DeviceName: device.DeviceName, ClientID: device.ClientID, MaxHeight: device.MaxHeight, MaxBitrateKbps: device.MaxBitrateKbps, HDR10: device.HDR10, DolbyVision: device.DolbyVision}
	c.Profiles = preserveProfiles(profile)
	c.Remote, c.Download = device.Remote, device.Download
	c.SubtitleCodecs = append([]string(nil), device.SubtitleCodecs...)
	if device.SubtitleCodecs != nil && c.SubtitleCodecs == nil {
		c.SubtitleCodecs = []string{}
	}
	c.DVProfiles = append([]int(nil), device.DVProfiles...)
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
