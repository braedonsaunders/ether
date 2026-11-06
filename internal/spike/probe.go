package spike

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// ffprobeJSON is the subset of `ffprobe -print_format json -show_format
// -show_streams` output the spike uses.
type ffprobeJSON struct {
	Streams []struct {
		Index         int    `json:"index"`
		CodecName     string `json:"codec_name"`
		CodecType     string `json:"codec_type"`
		Profile       string `json:"profile"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		PixFmt        string `json:"pix_fmt"`
		BitsPerRaw    string `json:"bits_per_raw_sample"`
		ColorTransfer string `json:"color_transfer"`
		AvgFrameRate  string `json:"avg_frame_rate"`
		RFrameRate    string `json:"r_frame_rate"`
		SampleRate    string `json:"sample_rate"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
		BitRate       string `json:"bit_rate"`
		Disposition   struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
		Tags         map[string]string `json:"tags"`
		SideDataList []struct {
			SideDataType string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}

// ParseProbe converts ffprobe JSON into core.MediaInfo. ext is the file
// extension, used to tell MP4 from MOV (ffprobe reports both as one
// format_name).
func ParseProbe(data []byte, ext string) (core.MediaInfo, error) {
	var p ffprobeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return core.MediaInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	info := core.MediaInfo{Container: containerFromFormat(p.Format.FormatName, ext)}
	if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
		info.Duration = time.Duration(d * float64(time.Second))
	}
	info.Size, _ = strconv.ParseInt(p.Format.Size, 10, 64)
	if br, err := strconv.ParseInt(p.Format.BitRate, 10, 64); err == nil {
		info.BitrateKbps = int(br / 1000)
	}
	for _, s := range p.Streams {
		st := core.StreamInfo{
			Index:   s.Index,
			Type:    s.CodecType,
			Codec:   normalizeProbeCodec(s.CodecName, s.Profile),
			Profile: s.Profile,
			Default: s.Disposition.Default == 1,
			Forced:  s.Disposition.Forced == 1,
		}
		if lang := s.Tags["language"]; lang != "" && lang != "und" {
			st.Language = lang
		}
		st.Title = s.Tags["title"]
		if br, err := strconv.ParseInt(s.BitRate, 10, 64); err == nil {
			st.BitrateKbps = int(br / 1000)
		} else if bps := s.Tags["BPS"]; bps != "" {
			if br, err := strconv.ParseInt(bps, 10, 64); err == nil {
				st.BitrateKbps = int(br / 1000)
			}
		}
		switch s.CodecType {
		case "video":
			if s.Tags["mimetype"] != "" || strings.HasPrefix(s.CodecName, "mjpeg") || s.CodecName == "png" {
				continue // cover art attachment
			}
			st.Width, st.Height = s.Width, s.Height
			st.FrameRate = parseRate(s.AvgFrameRate)
			if st.FrameRate == 0 {
				st.FrameRate = parseRate(s.RFrameRate)
			}
			st.BitDepth = 8
			if n, err := strconv.Atoi(s.BitsPerRaw); err == nil && n > 0 {
				st.BitDepth = n
			} else if strings.Contains(s.PixFmt, "10") {
				st.BitDepth = 10
			} else if strings.Contains(s.PixFmt, "12") {
				st.BitDepth = 12
			}
			st.ColorTransfer = s.ColorTransfer
			for _, sd := range s.SideDataList {
				if strings.Contains(strings.ToLower(sd.SideDataType), "dovi") {
					st.DolbyVision = true
				}
			}
		case "audio":
			st.Channels = s.Channels
			st.ChannelLayout = s.ChannelLayout
			st.SampleRate, _ = strconv.Atoi(s.SampleRate)
		case "subtitle":
		default:
			continue
		}
		info.Streams = append(info.Streams, st)
	}
	if info.BitrateKbps == 0 && info.Duration > 0 && info.Size > 0 {
		info.BitrateKbps = int(float64(info.Size*8) / info.Duration.Seconds() / 1000)
	}
	return info, nil
}

func parseRate(s string) float64 {
	n, d, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	nf, err1 := strconv.ParseFloat(n, 64)
	df, err2 := strconv.ParseFloat(d, 64)
	if err1 != nil || err2 != nil || df == 0 {
		return 0
	}
	return nf / df
}

func normalizeProbeCodec(name, profile string) string {
	switch name {
	case "dts":
		if strings.Contains(strings.ToLower(profile), "hd") {
			return "dtshd"
		}
		return "dts"
	case "subrip":
		return "srt"
	case "hdmv_pgs_subtitle":
		return "pgs"
	case "dvd_subtitle":
		return "vobsub"
	case "webvtt":
		return "vtt"
	}
	return name
}

func containerFromFormat(format, ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	switch {
	case strings.Contains(format, "matroska"):
		if ext == "webm" {
			return "webm"
		}
		return "mkv"
	case strings.Contains(format, "mp4") || strings.Contains(format, "mov"):
		if ext == "mov" {
			return "mov"
		}
		return "mp4"
	case format == "mpegts":
		return "mpegts"
	case format == "avi":
		return "avi"
	}
	return containerFromExt(ext)
}

func containerFromExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "mkv":
		return "mkv"
	case "mp4", "m4v":
		return "mp4"
	case "mov":
		return "mov"
	case "ts", "m2ts":
		return "mpegts"
	case "avi":
		return "avi"
	case "webm":
		return "webm"
	}
	return "mkv"
}

// estimateInfo is the media info used when ffprobe is unavailable: a guess
// from the extension and size, flagged Estimated.
func estimateInfo(path string, size int64) core.MediaInfo {
	ext := filepath.Ext(path)
	return core.MediaInfo{
		Container: containerFromExt(ext),
		Size:      size,
		Estimated: true,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080, FrameRate: 23.976, BitDepth: 8, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, ChannelLayout: "stereo", SampleRate: 48000, Default: true},
		},
	}
}

// prober runs ffprobe when it is available.
type prober struct {
	path string // empty: not available
}

func newProber(configured string) prober {
	if configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return prober{path: configured}
		}
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return prober{path: p}
	}
	return prober{}
}

func (p prober) probe(ctx context.Context, file string) (core.MediaInfo, error) {
	if p.path == "" {
		return core.MediaInfo{}, fmt.Errorf("ffprobe not available")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := mediaproc.CommandContext(ctx, mediaproc.ProbeTimeout, p.path, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", file).Output()
	if err != nil {
		return core.MediaInfo{}, fmt.Errorf("ffprobe %s: %w", filepath.Base(file), err)
	}
	return ParseProbe(out, filepath.Ext(file))
}
