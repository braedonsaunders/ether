package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// streamSample uses the real continuous decoder/encoder. The sample is four
// consecutive finished TS segments, copy-remuxed without a second encode.
func streamSample(ctx context.Context, input, output string, width, seconds, budget int) error {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return err
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "onair-stream-sample-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: dir, HWAccel: "videotoolbox"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	defer manager.Close()
	runctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	format := transcode.RawFormat{Width: width, Height: width * 9 / 16, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	decoder, err := manager.StartDecoder(runctx, transcode.DecoderRequest{SessionID: "sample-input", InputURL: input, Format: format, Loop: true})
	if err != nil {
		return err
	}
	defer decoder.Close()
	encoder, err := manager.StartEncoder(runctx, transcode.EncoderRequest{SessionID: "owner-sample", Source: format, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", BitrateKbps: budget, AudioBitrateKbps: 128, AudioChannels: 2, AudioSampleRate: 48000, MaxHeight: format.Height, H264Level: 40}, DVRWindow: time.Minute})
	if err != nil {
		return err
	}
	defer encoder.Close()
	writing := make(chan error, 1)
	go func() {
		start := time.Now()
		for i := 0; ; i++ {
			frame, e := decoder.Next(runctx)
			if e != nil {
				writing <- e
				return
			}
			if i == 0 {
				start = time.Now()
			}
			if e = encoder.Write(runctx, frame); e != nil {
				writing <- e
				return
			}
			delay := time.Until(start.Add(time.Duration(i+1) * time.Second / 30))
			if delay <= 0 {
				start = time.Now().Add(-time.Duration(i) * time.Second / 30)
				delay = time.Second / 30
			}
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-runctx.Done():
					timer.Stop()
					writing <- runctx.Err()
					return
				}
			}
		}
	}()
	capture := filepath.Join(dir, "viewer.ts")
	file, err := os.Create(capture)
	if err != nil {
		return err
	}
	index := encoder.Segments()
	for sequence := int64(0); sequence < int64(seconds/4); sequence++ {
		waitctx, stop := context.WithTimeout(runctx, 30*time.Second)
		waited := make(chan error, 1)
		go func() { _, e := index.Wait(waitctx, sequence-1); waited <- e }()
		select {
		case err = <-waited:
		case e := <-writing:
			stop()
			file.Close()
			return fmt.Errorf("stream writer before segment %d: %w", sequence, e)
		}
		stop()
		fmt.Printf("SEGMENT %s %d/%d\n", filepath.Base(output), sequence+1, seconds/4)
		if err != nil {
			file.Close()
			cancel()
			<-writing
			return err
		}
		recorder := httptest.NewRecorder()
		index.ServeTS(recorder, httptest.NewRequest("GET", "http://viewer/segment.ts", nil), sequence)
		if recorder.Code != 200 {
			file.Close()
			cancel()
			<-writing
			return fmt.Errorf("segment %d: %d", sequence, recorder.Code)
		}
		if _, err = file.Write(recorder.Body.Bytes()); err != nil {
			file.Close()
			cancel()
			<-writing
			return err
		}
	}
	if err = file.Close(); err != nil {
		return err
	}
	cancel()
	_ = decoder.Close()
	_ = encoder.Close()
	<-writing
	bytes, err := mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, ff, "-nostdin", "-v", "error", "-y", "-i", capture, "-map", "0:v:0", "-map", "0:a:0", "-t", fmt.Sprint(seconds), "-c", "copy", "-movflags", "+faststart", output).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remux: %w %s", err, bytes)
	}
	caption := "Actual viewer output through transcode.Manager.StartDecoder → StartEncoder → four consecutive HLS TS segments; packets copy-remuxed."
	if data, e := os.ReadFile(input[:len(input)-4] + ".json"); e == nil {
		var prior struct {
			Caption string `json:"caption"`
		}
		_ = json.Unmarshal(data, &prior)
		caption = prior.Caption + " " + caption
	}
	if err = metadata(ctx, output, caption); err != nil {
		return err
	}
	data, err := os.ReadFile(output[:len(output)-4] + ".json")
	if err != nil {
		return err
	}
	var sidecar map[string]any
	if err = json.Unmarshal(data, &sidecar); err != nil {
		return err
	}
	sidecar["session_bitrate_kbps"] = budget
	sidecar["encoder"] = "h264_videotoolbox"
	sidecar["audio_bitrate_kbps"] = 128
	sidecar["pipeline"] = "StartDecoder → StartEncoder → SegmentIndex.ServeTS → copy remux"
	if filepath.Base(input) == "late-night-overlay.mp4" || filepath.Base(input) == "late-night-overlay-720p.mp4" {
		sidecar["programme_source"] = "https://download.blender.org/peach/trailer/trailer_1080p.ogg"
		sidecar["programme_credit"] = "Big Buck Bunny © Blender Foundation | peach.blender.org | CC BY 3.0"
	}
	data, err = json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(output[:len(output)-4]+".json", append(data, '\n'), 0644)
}
