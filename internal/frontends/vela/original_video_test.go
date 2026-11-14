package vela

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestOriginalVideoScanFacts(t *testing.T) {
	no, yes, zero := false, true, 0.0
	for _, tc := range []struct {
		name      string
		stream    core.StreamInfo
		estimated bool
		known     bool
	}{
		{name: "unknown", stream: core.StreamInfo{Interlaced: false}},
		{name: "legacy interlaced hint", stream: core.StreamInfo{Interlaced: true}},
		{name: "progressive and zero", stream: core.StreamInfo{ProbedInterlaced: &no, FieldOrder: "progressive", Rotation: &zero}, known: true},
		{name: "interlaced", stream: core.StreamInfo{ProbedInterlaced: &yes, FieldOrder: "tt"}, known: true},
		{name: "estimated", stream: core.StreamInfo{ProbedInterlaced: &no, FieldOrder: "progressive", Rotation: &zero}, estimated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.stream.Type = "video"
			facts := originalVideo(frontend.MediaInfo{Streams: []core.StreamInfo{tc.stream}, Estimated: tc.estimated})
			if tc.estimated {
				if facts != nil {
					t.Fatal("estimated source facts emitted")
				}
				return
			}
			_, hasScan := facts["isInterlaced"]
			_, hasOrder := facts["fieldOrder"]
			_, hasRotation := facts["rotation"]
			if hasScan != tc.known || hasOrder != tc.known || hasRotation != (tc.stream.Rotation != nil) {
				t.Fatalf("lost or invented evidence: %+v", facts)
			}
			if tc.known && facts["isInterlaced"] != *tc.stream.ProbedInterlaced {
				t.Fatalf("wrong scan: %+v", facts)
			}
		})
	}
}
