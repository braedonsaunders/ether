package plex

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Native MP4 probing now copies codec tag, time base, pixel format, H.264
// NAL length, and constant frame-rate rationals. Plex metadata and stream
// XML keep using the rounded FrameRate, the fixed 4:2:0 chroma label, and
// an empty refFrames/title unless the container already had a title.
func TestNativeProbeFactsStayOffPlexWire(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, tc := range []struct {
		file, codec, rateName string
		frame                 float64
	}{
		{"h264_aac_movtext_faststart.mp4", "h264", "24p", 23.976},
		{"hevc_hdr10_eac3_ac3_moovtail.mp4", "hevc", "24p", 24},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "transcode", "testdata", "nativeprobe", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			info, err := transcode.ProbeSource(context.Background(), bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			video := info.Video()
			if video == nil || video.FrameRate != tc.frame {
				t.Fatalf("clock = %+v", video)
			}
			title := &core.Title{Key: 42, Type: core.TypeMovie, Title: "Fixture"}
			media := buildMedia(title, *info, 1)
			if media.VideoCodec != tc.codec || media.VideoFrameRate != tc.rateName || media.Container != "mp4" {
				t.Fatalf("media codec=%s rate=%s container=%s", media.VideoCodec, media.VideoFrameRate, media.Container)
			}
			if media.OptimizedForStreaming == nil || *media.OptimizedForStreaming != 1 {
				t.Fatalf("mp4 streaming flag %+v", media.OptimizedForStreaming)
			}
			streams := f.srv.buildStreams(title.Key, *info, selection{}, nil)
			if len(streams) == 0 || streams[0].StreamType != 1 {
				t.Fatalf("streams %+v", streams)
			}
			got := streams[0]
			if got.Codec != tc.codec || got.FrameRate != math.Round(video.FrameRate*1000)/1000 {
				t.Fatalf("stream codec=%s frame=%v", got.Codec, got.FrameRate)
			}
			if got.ChromaSubsampling != "4:2:0" || got.RefFrames != 0 || got.Title != "" {
				t.Fatalf("wire delta chroma=%s refs=%d title=%q", got.ChromaSubsampling, got.RefFrames, got.Title)
			}
			if video.CodecTag == "" || video.TimeBase == "" || video.PixelFormat == "" {
				t.Fatalf("probe facts missing before the wire check: %+v", video)
			}
		})
	}
}
