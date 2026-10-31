package diagnostics

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func newTestRecorder(t *testing.T) (*Recorder, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)}
	return New(Deps{Now: func() time.Time { return clk.t }}), clk
}

func TestPollOnlyDoesNotDiagnosePlayback(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "Living Room", Product: "Plex for Samsung", Model: "QN90", AppVersion: "5.9"})
	for i := 0; i < 5; i++ {
		clk.t = clk.t.Add(time.Second)
		kind := ClassifyRequest("/player/proxy/poll", 404, "text/plain", 40)
		r.Observe(Event{Time: clk.t, Kind: kind, DeviceKey: "tv", Route: "/player/proxy/poll", Status: 404, DeviceName: "Living Room"})
	}
	id := r.Devices()[0].ID
	v, err := r.Start("owner", StartIn{DeviceID: id})
	if err != nil {
		t.Fatal(err)
	}
	var polls, rejected, bytes int
	for _, o := range v.Observations {
		switch o.Kind {
		case KindPoll:
			polls += o.Count
		case KindSourceRejected:
			rejected++
		case KindMediaBytes, KindPlaybackReported:
			bytes++
		}
	}
	if polls != 5 || rejected != 0 || bytes != 0 {
		t.Fatalf("obs %+v", v.Observations)
	}
	if !hasHypothesis(v, "companion_poll_only") {
		t.Fatalf("hypotheses %+v", v.Hypotheses)
	}
	for _, h := range v.Hypotheses {
		if h.Code == "playback_failed" || h.Code == "source_rejected" {
			t.Fatal(h)
		}
	}
}

func TestUnknownDeviceDoesNotAutoSelect(t *testing.T) {
	r, clk := newTestRecorder(t)
	v, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "new-phone", DeviceName: "Phone", Route: "/decision", Title: "Secret Movie"})
	got, err := r.Get("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceConfirmed || got.Device != nil {
		t.Fatal("auto selected")
	}
	if len(got.Discovery) != 1 || got.Discovery[0].Name != "Phone" {
		t.Fatalf("discovery %+v", got.Discovery)
	}
	for _, o := range got.Observations {
		if o.Kind == KindPlaybackRequested || strings.Contains(o.Route, "decision") {
			t.Fatalf("playback retained before confirm: %+v", o)
		}
	}
	_, err = r.SelectDevice("owner", v.ID, "missing")
	if !errors.Is(err, ErrStaleDevice) {
		t.Fatal(err)
	}
	still, _ := r.Get("owner", v.ID)
	if still.DeviceConfirmed {
		t.Fatal("stale id widened the capture")
	}
	chosen, err := r.SelectDevice("owner", v.ID, got.Discovery[0].ID)
	if err != nil || !chosen.DeviceConfirmed {
		t.Fatal(err, chosen.DeviceConfirmed)
	}
	if len(chosen.Observations) != 1 || chosen.Observations[0].Kind != KindPlaybackRequested {
		t.Fatalf("after confirm %+v", chosen.Observations)
	}
}

func TestStaleDeviceDoesNotOpenCapture(t *testing.T) {
	r, _ := newTestRecorder(t)
	if _, err := r.Start("owner", StartIn{DeviceID: "deadbeef"}); !errors.Is(err, ErrStaleDevice) {
		t.Fatal(err)
	}
	if len(r.captures) != 0 {
		t.Fatal("stale start created a capture")
	}
}

func TestOtherOwnerCannotSeeCapture(t *testing.T) {
	r, _ := newTestRecorder(t)
	v, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("other-session", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := r.Get("owner", "no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestEncoderWithoutDeviceCorrelatesOrOmits(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", SessionKey: "sess-1", DeviceName: "TV"})
	clk.t = clk.t.Add(time.Second)
	r.Observe(Event{Time: clk.t, Kind: KindEncoderFailed, SessionKey: "sess-1", ErrorExcerpt: "ffmpeg failed"})
	clk.t = clk.t.Add(time.Second)
	r.Observe(Event{Time: clk.t, Kind: KindEncoderFailed, SessionKey: strings.Repeat("z", maxHashKey+1), ErrorExcerpt: "orphan"})
	id := r.Devices()[0].ID
	v, err := r.Start("owner", StartIn{DeviceID: id})
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, o := range v.Observations {
		if o.Kind == KindEncoderFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("encoder observations %d %+v", failed, v.Observations)
	}
	var omitted bool
	for _, g := range v.Coverage {
		if g.Collector == "encoder" && g.Status == "omitted" {
			omitted = true
		}
	}
	if !omitted {
		t.Fatalf("coverage %+v", v.Coverage)
	}
}

// The Android TV remux 500: ffmpeg has started, the encoder event has a
// session and no device, and the device that session remembered has already
// left the table. That lookup must not dereference a nil device.
func TestEncoderAfterDeviceExpiryDoesNotPanic(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", SessionKey: "sess-1", DeviceName: "TV"})
	clk.t = clk.t.Add(deviceRecent + time.Second)
	r.Observe(Event{Time: clk.t, Kind: KindEncoderStarted, SessionKey: "sess-1", FFmpegExcerpt: "ffmpeg started"})
	if _, ok := r.sessions["sess-1"]; ok {
		t.Fatal("session still points at the expired device")
	}
	if got := r.Devices(); len(got) != 0 {
		t.Fatalf("expired device still listed: %+v", got)
	}
}

func TestSelectedDeviceExcludesTheOther(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", DeviceName: "TV", Title: "Only TV"})
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "pad", DeviceName: "Pad", Title: "Only Pad"})
	var tv string
	for _, d := range r.Devices() {
		if d.Name == "TV" {
			tv = d.ID
		}
	}
	v, err := r.Start("owner", StartIn{DeviceID: tv})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Observations) != 1 {
		t.Fatalf("%+v", v.Observations)
	}
}

func TestRingDropsOldest(t *testing.T) {
	r, clk := newTestRecorder(t)
	for i := 0; i < maxEvents+10; i++ {
		r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", Route: "/decision/" + itoa(i)})
	}
	if r.n != maxEvents {
		t.Fatal(r.n)
	}
	var sawFirst bool
	r.eachLocked(func(e Event) {
		if e.Route == "/decision/0" {
			sawFirst = true
		}
	})
	if sawFirst {
		t.Fatal("oldest retained")
	}
}

func TestMediaErrorDoesNotAdvanceDelivery(t *testing.T) {
	r, clk := newTestRecorder(t)
	kind := ClassifyRequest("/library/parts/9/file.mkv", 404, "application/json", 100)
	r.Observe(Event{Time: clk.t, Kind: kind, DeviceKey: "tv", DeviceName: "TV", Route: "/library/parts/9/file.mkv", Status: 404, Bytes: 100})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range v.Observations {
		if o.Kind == KindMediaBytes {
			t.Fatal("error body counted as media")
		}
	}
	if v.Observations[0].Kind != KindMediaRequest {
		t.Fatal(v.Observations[0].Kind)
	}
}

func TestArchiveRedactsEncodedSecretAndHidesOtherViewer(t *testing.T) {
	const secret = "jazz"
	r, clk := newTestRecorder(t)
	r.deps.Secrets = func() []string { return []string{secret} }
	r.Observe(Event{
		Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", DeviceName: "TV",
		Title: "Only TV", Username: "Ada", Peer: "203.0.113.9",
		ErrorExcerpt: `{"token":"%6a%61%7A%7a"}`,
	})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID, Note: "see https://example.test/help"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Preview == nil || v.Preview.Identity.UsernamesIncluded || len(v.Preview.Identity.Usernames) != 0 || len(v.Preview.Identity.Addresses) != 0 {
		t.Fatalf("preview %+v", v.Preview)
	}
	finished, err := r.Finish("owner", v.ID, FinishIn{})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Preview.Identity.Explanation == "" || !strings.Contains(finished.Preview.Identity.Explanation, "Usernames are not in the ZIP") {
		t.Fatal(finished.Preview.Identity.Explanation)
	}
	blob, err := r.Archive("other", v.ID)
	if err == nil || len(blob) != 0 {
		t.Fatal("other session read the archive")
	}
	blob, err = r.Archive("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if strings.HasSuffix(f.Name, ".json") || strings.HasSuffix(f.Name, ".jsonl") {
			for _, line := range bytes.Split(body, []byte("\n")) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				if !json.Valid(line) {
					t.Fatalf("%s invalid json %s", f.Name, line)
				}
			}
		}
		all.Write(body)
	}
	text := all.String()
	if strings.Contains(text, secret) || strings.Contains(strings.ToLower(text), "%6a") || strings.Contains(text, "Ada") || strings.Contains(text, "203.0.113.9") {
		t.Fatalf("leaked\n%s", text)
	}
	if !strings.Contains(text, "untrusted data") || !strings.Contains(text, "https://example.test/help") {
		t.Fatalf("note missing\n%s", text)
	}
}

func TestCoalesceSpanIsBoundedByFirstTimestamp(t *testing.T) {
	r, clk := newTestRecorder(t)
	base := clk.t
	r.Observe(Event{Time: base, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	for i := 0; i <= 40; i++ {
		r.Observe(Event{Time: base.Add(time.Duration(i) * time.Second), Kind: KindPoll, DeviceKey: "tv", Route: "/player/proxy/poll", Status: 404})
	}
	var maxSpan time.Duration
	r.eachLocked(func(e Event) {
		if e.Kind != KindPoll {
			return
		}
		until := e.Until
		if until.IsZero() {
			until = e.Time
		}
		if span := until.Sub(e.Time); span > maxSpan {
			maxSpan = span
		}
		if span := until.Sub(e.Time); span > coalesceWindow {
			t.Fatalf("span %s from %s", span, e.Time)
		}
	})
	if maxSpan == 0 {
		t.Fatal("polls did not coalesce at all")
	}
	clk.t = base.Add(100 * time.Second)
	from := clk.t.Add(-preRoll)
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	var inside, crossed int
	r.eachLocked(func(e Event) {
		if e.Kind != KindPoll || e.DeviceKey != "tv" {
			return
		}
		until := e.Until
		if until.IsZero() {
			until = e.Time
		}
		if until.Before(from) || e.Time.After(clk.t) {
			return
		}
		n := e.Repeat + 1
		if e.Time.Before(from) || until.After(clk.t) {
			crossed += n
			return
		}
		inside += n
	})
	if inside == 0 || crossed == 0 {
		t.Fatalf("inside %d crossed %d", inside, crossed)
	}
	if got := sampleCount(v, KindPoll); got != inside {
		t.Fatalf("observations %d, in-window samples %d, crossed %d, obs %+v", got, inside, crossed, v.Observations)
	}
	if !hasGapCount(v, "omitted", crossed) {
		t.Fatalf("coverage %+v", v.Coverage)
	}
	for _, o := range v.Observations {
		if o.At.Before(from) {
			t.Fatal(o.At)
		}
	}

	live, clk2 := newTestRecorder(t)
	live.Observe(Event{Time: clk2.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	lv, err := live.Start("owner", StartIn{DeviceID: live.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		clk2.t = clk2.t.Add(time.Second)
		live.Observe(Event{Time: clk2.t, Kind: KindPoll, DeviceKey: "tv", Route: "/player/proxy/poll", Status: 404})
	}
	got, err := live.Get("owner", lv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sampleCount(got, KindPoll) != 10 {
		t.Fatalf("%+v", got.Observations)
	}
	for _, o := range got.Observations {
		if o.Kind == KindPoll && o.Count >= 10 {
			t.Fatal("one poll span absorbed the whole run")
		}
	}
	cap := live.captures[lv.ID]
	cap.eachEv(func(e Event) {
		until := e.Until
		if until.IsZero() {
			until = e.Time
		}
		if until.Sub(e.Time) > coalesceWindow {
			t.Fatal(until.Sub(e.Time))
		}
	})

	// Samples after the deadline must not increment the in-window count.
	edge, clk3 := newTestRecorder(t)
	clk3.t = base
	edge.Observe(Event{Time: clk3.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	ev, err := edge.Start("owner", StartIn{DeviceID: edge.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	deadline := ev.Deadline
	for i := 0; i < 5; i++ {
		at := deadline.Add(time.Duration(i-1) * time.Second)
		edge.Observe(Event{Time: at, Kind: KindPoll, DeviceKey: "tv", Route: "/player/proxy/poll", Status: 404})
	}
	clk3.t = deadline.Add(time.Minute)
	closed, err := edge.Get("owner", ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sampleCount(closed, KindPoll) != 1 {
		t.Fatalf("%+v", closed.Observations)
	}
	if !hasGapCount(closed, "omitted", 2) {
		t.Fatalf("coverage %+v", closed.Coverage)
	}
	if closed.FinishedAt == nil || !closed.FinishedAt.Equal(deadline) {
		t.Fatalf("finished %+v", closed.FinishedAt)
	}
}

func TestFinishedEvidenceSurvivesRingEviction(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", DeviceName: "TV", Route: "/decision/tv", Title: "Kept"})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxEvents+10; i++ {
		r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "other", DeviceName: "Other", Route: "/decision/o" + itoa(i)})
	}
	live, err := r.Get("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Observations) != 1 || live.Observations[0].Route != "/decision/tv" {
		t.Fatalf("%+v", live.Observations)
	}
	for _, g := range live.Coverage {
		if g.Status == "truncated" {
			t.Fatalf("other devices truncated this capture: %+v", live.Coverage)
		}
	}
	finished, err := r.Finish("owner", v.ID, FinishIn{})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.Archive("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Observe(Event{Time: clk.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "Renamed"})
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", Route: "/decision/later"})
	for i := 0; i < maxEvents; i++ {
		r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "other", Route: "/decision/z" + itoa(i)})
	}
	again, err := r.Get("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Observations) != 1 || again.Observations[0].Route != finished.Observations[0].Route {
		t.Fatalf("preview changed %+v", again.Observations)
	}
	if again.Device == nil || again.Device.Name != "TV" {
		t.Fatalf("device %+v", again.Device)
	}
	blob2, err := r.Archive("owner", v.ID)
	if err != nil || string(blob2) != string(blob) {
		t.Fatal("archive changed after finish", err)
	}
}

func TestCaptureBufferReportsExactOverflow(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", DeviceName: "TV", Route: "/decision/0"})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxCaptureEvents; i++ {
		clk.t = clk.t.Add(time.Millisecond)
		r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", Route: "/decision/" + itoa(i)})
	}
	got, err := r.Get("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != maxCaptureEvents {
		t.Fatalf("len %d", len(got.Observations))
	}
	for _, o := range got.Observations {
		if o.Route == "/decision/0" {
			t.Fatal("dropped event still shown")
		}
	}
	if !hasGapText(got, "truncated", "Dropped 1 retained events (1 samples)") {
		t.Fatalf("%+v", got.Coverage)
	}
}

func TestFinishStopsAtDeadline(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	started := v.StartedAt
	clk.t = started.Add(time.Minute)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", Route: "/decision/in"})
	clk.t = started.Add(maxDuration + time.Minute)
	r.Observe(Event{Time: clk.t, Kind: KindPlaybackRequested, DeviceKey: "tv", Route: "/decision/late"})
	clk.t = started.Add(10 * time.Minute)
	fin, err := r.Finish("owner", v.ID, FinishIn{})
	if err != nil {
		t.Fatal(err)
	}
	if fin.FinishedAt == nil || !fin.FinishedAt.Equal(started.Add(maxDuration)) {
		t.Fatalf("finished %+v want %s", fin.FinishedAt, started.Add(maxDuration))
	}
	if len(fin.Observations) != 1 || fin.Observations[0].Route != "/decision/in" {
		t.Fatalf("%+v", fin.Observations)
	}
}

func TestAbandonedCaptureExpiresInOnePass(t *testing.T) {
	r, clk := newTestRecorder(t)
	v, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	clk.t = v.StartedAt.Add(maxDuration + retainFor)
	r.Expire()
	if _, ok := r.captures[v.ID]; ok {
		t.Fatal("capture still held a slot")
	}
	if _, err := r.Get("owner", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := r.Start("owner", StartIn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start("owner", StartIn{}); err != nil {
		t.Fatal(err)
	}

	late, clk2 := newTestRecorder(t)
	w, err := late.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	clk2.t = w.StartedAt.Add(maxDuration + retainFor)
	if _, err := late.Finish("owner", w.ID, FinishIn{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := late.Archive("owner", w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := late.SelectDevice("owner", w.ID, "dead"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(late.captures) != 0 {
		t.Fatal(len(late.captures))
	}
	if _, err := late.Start("owner", StartIn{}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceBecomesStaleWithAge(t *testing.T) {
	r, clk := newTestRecorder(t)
	r.Observe(Event{Time: clk.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	id := r.Devices()[0].ID
	clk.t = clk.t.Add(deviceRecent - time.Second)
	if len(r.Devices()) != 1 {
		t.Fatalf("%+v", r.Devices())
	}
	clk.t = clk.t.Add(time.Second)
	if len(r.Devices()) != 0 {
		t.Fatalf("still recent %+v", r.Devices())
	}
	if _, err := r.Start("owner", StartIn{DeviceID: id}); !errors.Is(err, ErrStaleDevice) {
		t.Fatal(err)
	}
	if len(r.captures) != 0 {
		t.Fatal("stale id opened a capture")
	}
	r.Observe(Event{Time: clk.t, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	if len(r.Devices()) != 1 || r.Devices()[0].ID != id {
		t.Fatalf("%+v", r.Devices())
	}
	if _, err := r.Start("owner", StartIn{DeviceID: id}); err != nil {
		t.Fatal(err)
	}
}

func sampleCount(v CaptureView, kind Kind) int {
	n := 0
	for _, o := range v.Observations {
		if o.Kind == kind {
			n += o.Count
		}
	}
	return n
}

func hasGapCount(v CaptureView, status string, n int) bool {
	needle := itoa(n)
	for _, g := range v.Coverage {
		if g.Status == status && strings.Contains(g.Detail, needle) {
			return true
		}
	}
	return false
}

func hasGapText(v CaptureView, status, text string) bool {
	for _, g := range v.Coverage {
		if g.Status == status && strings.Contains(g.Detail, text) {
			return true
		}
	}
	return false
}

func hasHypothesis(v CaptureView, code string) bool {
	for _, h := range v.Hypotheses {
		if h.Code == code && strings.Contains(strings.ToLower(h.Text), "no playback request was observed") {
			return true
		}
	}
	return false
}

func TestStartEvictsOldestFinishedWhenSlotsAreFull(t *testing.T) {
	r, clk := newTestRecorder(t)
	v1, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Finish("owner", v1.ID, FinishIn{}); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	v2, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatal(err)
	}
	// One finished + one recording = 2 slots; starting a third evicts v1 (finished).
	clk.t = clk.t.Add(time.Second)
	v3, err := r.Start("owner", StartIn{})
	if err != nil {
		t.Fatalf("third capture should evict oldest finished: %v", err)
	}
	if _, err := r.Get("owner", v1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest finished should be evicted, got %v", err)
	}
	if _, err := r.Get("owner", v2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("owner", v3.ID); err != nil {
		t.Fatal(err)
	}
	// Two active recordings cannot be evicted; a fourth start must return ErrTooMany.
	if _, err := r.Start("owner", StartIn{}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("expected ErrTooMany when two recordings are active, got %v", err)
	}
}
