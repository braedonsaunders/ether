package transcode

import (
	"os"
	"path/filepath"
	"testing"
)

// This fixture is real ffprobe 8.1.3 output from an AAC track muxed with
// default, hearing-impaired, visual-impaired and commentary dispositions.
func TestProbePreservesReportedDispositions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ffprobe", "dispositions_actual.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseProbe(raw)
	if err != nil || len(info.Streams) != 1 {
		t.Fatalf("probe %+v %v", info, err)
	}
	s := info.Streams[0]
	if !s.Default || s.DefaultDisposition == nil || !*s.DefaultDisposition || s.Forced || s.ForcedDisposition == nil || *s.ForcedDisposition || s.HearingImpaired == nil || !*s.HearingImpaired || s.VisualImpaired == nil || !*s.VisualImpaired || s.Commentary == nil || !*s.Commentary || s.Original == nil || *s.Original || s.Dub == nil || *s.Dub {
		t.Fatalf("dispositions %+v", s)
	}
	unknown, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"aac"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	u := unknown.Streams[0]
	if u.DefaultDisposition != nil || u.ForcedDisposition != nil || u.HearingImpaired != nil || u.Original != nil || u.VisualImpaired != nil || u.Commentary != nil || u.Dub != nil {
		t.Fatalf("missing dispositions were assumed: %+v", u)
	}
	marked, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"audio","codec_name":"aac","disposition":{"original":1,"dub":1,"forced":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := marked.Streams[0]
	if m.Original == nil || !*m.Original || m.Dub == nil || !*m.Dub || !m.Forced || m.ForcedDisposition == nil || !*m.ForcedDisposition || m.HearingImpaired != nil {
		t.Fatalf("explicit flags %+v", m)
	}
}
