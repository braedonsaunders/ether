package plex

import (
	"context"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func TestEnhancedPlayerHLSTargetSurvivesDecision(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	pr := &Request{User: &ownerUser, ClientID: "enhanced-player", Product: "Plex for Apple TV", Platform: "tvOS", ProfileExtra: "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3)"}
	caps := pr.ClientCaps(0)
	if len(caps.HLSContainers) != 1 || caps.HLSContainers[0] != "mkv" {
		t.Fatalf("explicit target was discarded: %+v", caps)
	}
	ps, err := f.playback.Prepare(context.Background(), &ownerUser, fightClub.Key, caps)
	if err != nil {
		t.Fatal(err)
	}
	ps.Resolution = &core.Resolution{Release: core.Release{ID: "on-demand"}}
	p := playRequest{key: fightClub.Key, caps: caps, mde: true, sessionID: "enhanced", protocol: "hls", opts: core.DecideOptions{DirectPlay: true, DirectStream: true}}
	title, session, d, err := f.srv.prepare(context.Background(), pr, &p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != core.DecisionRemux || d.Container != transcode.ContainerMatroska || !d.VideoCopy || !d.AudioCopy {
		t.Fatalf("explicit Matroska remux target lost: %+v", d)
	}
	m := f.srv.decisionMedia(title, session, d, p, &ownerUser)
	if m.Container != "mkv" || m.Part[0].Container != "mkv" || m.Protocol != "hls" {
		t.Fatalf("decision metadata disagrees with output: %+v", m)
	}
	// Direct-play support for MKV does not opt native AVPlayer into custom
	// segmented Matroska. Only the explicit HLS target can do that.
	pr.ProfileExtra = "add-direct-play-profile(type=videoProfile&container=mkv&videoCodec=hevc&audioCodec=eac3)"
	if got := pr.ClientCaps(0).HLSContainers; len(got) != 0 {
		t.Fatalf("direct-play format leaked into HLS: %v", got)
	}
}

func TestMatroskaSidecarCarriesTimestampMap(t *testing.T) {
	st := model.Stream{StreamType: 3, Codec: "srt", Selected: model.Ptr(true), ID: 42, Key: "/library/streams/42", Format: "srt"}
	src := core.StreamInfo{Type: "subtitle", Codec: "subrip"}
	mkv := core.Decision{Kind: core.DecisionTranscode, Container: transcode.ContainerMatroska}
	got, keep := applyStreamDecision(st, src, mkv, true)
	if !keep || got.Location != "sidecar" || got.Key != "/library/streams/42.vtt" || got.Format != "vtt" {
		t.Fatalf("matroska sidecar: %+v", got)
	}
	ts := core.Decision{Kind: core.DecisionTranscode, Container: transcode.ContainerMPEGTS}
	got, _ = applyStreamDecision(st, src, ts, true)
	if got.Key != "/library/streams/42" || got.Format != "srt" {
		t.Fatalf("mpegts sidecar changed: %+v", got)
	}
	body := stampMatroskaVTT([]byte("WEBVTT\n\n1\n00:07:00.000 --> 00:07:02.000\nHola\n"))
	if !strings.Contains(string(body), "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:900000\n") || !strings.Contains(string(body), "00:07:00.000") {
		t.Fatalf("mapped vtt: %q", body)
	}
}
