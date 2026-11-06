package diagnostics

import (
	"strings"
	"testing"
)

func TestViewerPlaybackScopeAndPreRollIsolation(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Kind: KindDevice, DeviceKey: "phone", ViewerID: 7})
	// Same media path, status and time must never merge distinct viewers/sessions.
	for _, viewer := range []int64{8, 7, 0} {
		r.Observe(Event{Time: clk.t, Kind: KindMediaRequest, ViewerID: viewer, Route: "/library/parts/42/0/file.mkv", Status: 206})
	}
	c, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Event{
		{Kind: KindClientCapabilities, ViewerID: 7, Client: &ClientFacts{Stage: "admission", MaxHeight: 1080}},
		{Kind: KindPlaybackDecision, ViewerID: 7, Playback: &PlaybackDecision{ClientMaxHeight: 1080}},
		{Kind: KindClientCapabilities, ViewerID: 8},
		{Kind: KindClientCapabilities, ViewerID: 7, DeviceKey: "another-device"},
		{Kind: KindClientCapabilities, ViewerID: 7, DeviceKey: strings.Repeat("x", maxHashKey+1)},
		{Kind: KindRequestStarted, ViewerID: 0, Route: "/library/parts/42/0/file.mkv"},
		{Kind: KindRequestStarted, ViewerID: 7, Route: "/library/metadata/42"},
	} {
		e.Time = clk.t
		r.Observe(e)
	}
	cap := r.captures[c.ID]
	events := cap.evidenceCopy(cap.started.Add(-preRoll), cap.deadline)
	if len(events) != 3 {
		t.Fatalf("retained %d events: %+v", len(events), events)
	}
	for _, e := range events {
		if e.ViewerID != 7 || !e.Uncorrelated || e.DeviceKey != "" || e.Repeat != 0 {
			t.Fatalf("bad attribution: %+v", e)
		}
	}
}

func TestClientFactsBoundCopiedAndRedactedAtExport(t *testing.T) {
	in := &ClientFacts{Stage: "admission", Registry: "later-secret", VideoCodecs: []string{"h264", "later-secret"}, Inputs: []ClientInput{{Name: "X-Plex-Product", Header: true}}}
	e := boundEvent(Event{Kind: KindClientCapabilities, Client: in}, nil)
	in.VideoCodecs[0] = "mutated"
	in.Inputs[0].Name = "mutated"
	if e.Client.VideoCodecs[0] != "h264" || e.Client.Inputs[0].Name != "X-Plex-Product" {
		t.Fatal("caller retained ownership")
	}
	cloned := cloneEvent(e)
	cloned.Client.VideoCodecs[0] = "copy"
	if e.Client.VideoCodecs[0] != "h264" {
		t.Fatal("capture clone shares data")
	}
	uses, _ := prepareSecrets([]string{"later-secret"})
	body, skipped, _ := eventsJSONL([]Event{e}, [3]bool{}, "capture", uses)
	if skipped != 0 || strings.Contains(body, "later-secret") || !strings.Contains(body, "h264") {
		t.Fatalf("bad export %s skipped=%d", body, skipped)
	}
}

func TestPlaybackCapabilityReportDistinguishesAdmissionFromReceipt(t *testing.T) {
	body, _, _ := eventsJSONL([]Event{
		{Kind: KindRequestStarted, Uncorrelated: true, Client: &ClientFacts{Stage: "request", Registry: "generic", RegistryMaxHeight: 1080, MaxHeight: 1080}},
		{Kind: KindClientCapabilities, Uncorrelated: true, Client: &ClientFacts{Stage: "admission", Registry: "generic", RegistryMaxHeight: 1080, MaxHeight: 1080}},
	}, [3]bool{}, "capture", nil)
	rows, findings := analyzeJournal(body)
	if len(findings) != 2 {
		t.Fatalf("findings: %+v", findings)
	}
	text := string(reportMarkdown(rows, findings, nil))
	for _, want := range []string{"generic client profile resolution ceiling", "selected viewer; device unknown", "start does not prove response completion"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q", want)
		}
	}
}
