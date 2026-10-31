package diagnostics

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPlaybackDecisionExportsReleaseNamesWithoutIdentity(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	fourK := "Passengers.2016.2160p.NF.WEB-DL.DDP5.1.H.265-HHWEB"
	hd := "Passengers.2016.1080p.BluRay.x264-GRP"
	r := New(Deps{Now: func() time.Time { return now }})
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: "web", DeviceName: "Brave"})
	r.Observe(Event{
		Time: now, Kind: KindPlaybackDecision, DeviceKey: "web", SessionKey: "sess",
		Username: "roadhazard", Peer: "203.0.113.8", Title: "Passengers",
		Playback: &PlaybackDecision{
			Profile: "1080p", ProfileMaxHeight: 1080, ClientMaxHeight: 2160, EffectiveMaxHeight: 1080,
			Chooser: "auto", ChosenRelease: hd, RequestedPartID: 9003938094011373,
			Candidates: []PlaybackCandidate{
				{Name: hd, Height: 1080, Score: 126, Healthy: true, HealthChecked: true},
				{Name: fourK, Height: 2160, Score: 75, Rejected: true, Reason: "above profile max height"},
			},
			Variants: []PlaybackVariant{
				{PartID: 603, Auto: true, Height: 1080, Release: hd},
				{PartID: 9003938094011373, Height: 2160, Release: fourK, Requested: true},
			},
		},
	})
	if len(r.Devices()) == 0 {
		t.Fatal("device missing")
	}
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Finish("owner", v.ID, FinishIn{SetFlags: true}); err != nil {
		t.Fatal(err)
	}
	blob, err := r.Archive("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		text += string(b)
	}
	for _, want := range []string{
		`"kind":"playback.decision"`,
		`"profile":"1080p"`,
		`"profileMaxHeight":1080`,
		`"clientMaxHeight":2160`,
		`"effectiveMaxHeight":1080`,
		`"allowHdr":false`,
		`"chooser":"auto"`,
		fourK,
		hd,
		`"reason":"above profile max height"`,
		"9003938094011373",
		`"score":126`,
		`"score":75`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("capture missing %q", want)
		}
	}
	for _, ban := range []string{"roadhazard", "203.0.113.8"} {
		if strings.Contains(text, ban) {
			t.Fatalf("capture contained %q", ban)
		}
	}
}
