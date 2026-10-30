package transcode

import "math"

// h264LevelLimits are H.264 Annex A frame, rate and decoded-picture-buffer
// bounds. Bitrate limits below use the conservative High-profile VCL factor.
type h264LevelLimits struct {
	level, maxFrameMB, maxMBPerSecond, maxDPB, maxKbps, maxCPBKbits int
}

func h264Levels() []h264LevelLimits {
	return []h264LevelLimits{
		{30, 1620, 40500, 8100, 12500, 12500},
		{31, 3600, 108000, 18000, 17500, 17500},
		{32, 5120, 216000, 20480, 25000, 25000},
		{40, 8192, 245760, 32768, 25000, 31250},
		{41, 8192, 245760, 32768, 62500, 78125},
		{42, 8704, 522240, 34816, 62500, 78125},
		{50, 22080, 589824, 110400, 168750, 168750},
		{51, 36864, 983040, 184320, 300000, 300000},
		{52, 36864, 2073600, 184320, 300000, 300000},
	}
}

// H264HighLevel returns the smallest supported High-profile level for a
// known encode geometry, rate, peak bitrate and decoded-picture budget. Zero
// means the facts are unknown or exceed this encoder contract.
func H264HighLevel(width, height int, fps float64, peakKbps, dpbFrames int) int {
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 || fps <= 0 || math.IsNaN(fps) || math.IsInf(fps, 0) || peakKbps <= 0 || dpbFrames <= 0 {
		return 0
	}
	w, h := (width+15)/16, (height+15)/16
	frame := w * h
	for _, l := range h264Levels() {
		if frame <= l.maxFrameMB && w*w <= 8*l.maxFrameMB && h*h <= 8*l.maxFrameMB && float64(frame)*fps <= float64(l.maxMBPerSecond) && frame*dpbFrames <= l.maxDPB && peakKbps <= l.maxKbps {
			return l.level
		}
	}
	return 0
}

// H264HighLevelLimits returns peak bitrate and decoded-picture limits for a
// level selected by H264HighLevel.
func H264HighLevelLimits(level int) (int, int, bool) {
	for _, l := range h264Levels() {
		if l.level == level {
			return l.maxKbps, l.maxCPBKbits, true
		}
	}
	return 0, 0, false
}

// OutputGeometry uses the same geometry as the encoder's scale plan.
func OutputGeometry(width, height, maxHeight int) (int, int) {
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	return outputSize(width, height, maxHeight)
}
