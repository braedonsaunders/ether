package silo

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestLastPlayedFactsOnlyExposeKnownSourceFacts(t *testing.T) {
	hdr := false
	out := map[string]any{}
	addLastPlayedFacts(out, &frontend.PlaybackSourceHistory{VideoCodec: "h264", Height: 1080, HDR: &hdr})
	if out["last_codec_video"] != "h264" || out["last_resolution"] != "1080p" || out["last_hdr"] != false {
		t.Fatalf("unexpected known source facts: %#v", out)
	}
	if _, ok := out["last_file_id"]; ok {
		t.Fatal("source identity must not be confused with an opaque version handle")
	}
	unknown := map[string]any{}
	addLastPlayedFacts(unknown, &frontend.PlaybackSourceHistory{})
	if len(unknown) != 0 {
		t.Fatalf("unknown source facts exposed: %#v", unknown)
	}
}
