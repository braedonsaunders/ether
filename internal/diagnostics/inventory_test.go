package diagnostics

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPreviewInventoryMatchesSealedArchive(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	r := finishRecorder(t, now, []SessionFact{{
		State: "playing", Title: "Movie.Name", Username: "alice",
	}}, []LogRecord{{
		Time: now, Level: "INFO",
		Attrs: map[string]string{"client": "tv", "user": "alice", "peer": "192.0.2.1", "release": "Movie.Name"},
	}})
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	r.Observe(Event{Time: now, Kind: KindPlaybackRequested, DeviceKey: "tv", SessionKey: "sess-one", Username: "alice", Peer: "192.0.2.1", Title: "Movie.Name"})
	view, blob := finishDevice(t, r, now, [3]bool{true, true, true})
	names := zipEntryNames(t, blob)
	if view.Preview == nil || len(names) == 0 || !sameStrings(names, view.Preview.Files) {
		t.Fatalf("preview %v zip %v", view.Preview.Files, names)
	}
	for _, name := range []string{"summary.json", "events.jsonl", "logs/capture.log", "sessions.json", "omissions.json", "manifest.json"} {
		if !containsName(names, name) {
			t.Fatalf("missing %s in %v", name, names)
		}
	}
	again, err := r.Get("owner", view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(again.Preview.Files, view.Preview.Files) || !sameIdentity(again.Preview.Identity, view.Preview.Identity) {
		t.Fatal("finished preview was not frozen")
	}
	files := unzip(t, blob)
	var summary struct {
		Identity IdentityPreview `json:"identity"`
	}
	if err := json.Unmarshal(files["summary.json"], &summary); err != nil {
		t.Fatal(err)
	}
	if !sameIdentity(summary.Identity, view.Preview.Identity) {
		t.Fatalf("summary identity %+v preview %+v", summary.Identity, view.Preview.Identity)
	}
	if !containsName(view.Preview.Identity.Usernames, "alice") || !containsName(view.Preview.Identity.Titles, "Movie.Name") || !containsName(view.Preview.Identity.Addresses, "192.0.2.1") {
		t.Fatalf("identity %+v", view.Preview.Identity)
	}
}

func TestOmittedLogIsNotAdvertised(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	const secret = "status-secret-value"
	const user = "log-only-alice"
	r := finishRecorder(t, now, nil, []LogRecord{{
		Time: now, Level: "INFO",
		Attrs: map[string]string{"client": "tv", "user": user, "status": secret},
	}})
	r.deps.Secrets = func() []string { return []string{secret} }
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	r.Observe(Event{Time: now, Kind: KindPlaybackRequested, DeviceKey: "tv", SessionKey: "sess-one"})
	view, blob := finishDevice(t, r, now, [3]bool{false, true, false})
	names := zipEntryNames(t, blob)
	if !sameStrings(names, view.Preview.Files) {
		t.Fatalf("preview %v zip %v", view.Preview.Files, names)
	}
	if containsName(names, "logs/capture.log") {
		t.Fatal("omitted log file is in the inventory")
	}
	text := string(bytes.Join(valuesOf(unzip(t, blob)), nil)) + view.Preview.Identity.Explanation
	for _, listed := range view.Preview.Identity.Usernames {
		text += listed
	}
	if strings.Contains(text, user) || strings.Contains(text, secret) {
		t.Fatal("omitted log identity or secret was advertised")
	}
	if !strings.Contains(string(unzip(t, blob)["omissions.json"]), "omitted") {
		t.Fatal("omissions did not record the dropped log")
	}
}

func TestBudgetDropIsNotAdvertised(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	const (
		eventUser  = "EventUser"
		eventTitle = "EventTitle"
		logUser    = "LogOnlyUser"
		sessTitle  = "SessionOnlyTitle"
	)
	logs := make([]LogRecord, 200)
	for i := range logs {
		logs[i] = LogRecord{Time: now, Level: "INFO", Attrs: map[string]string{
			"client": "tv", "user": logUser, "release": "LogOnlyTitle",
		}}
	}
	r := finishRecorder(t, now, []SessionFact{{
		State: "playing", Title: strings.Repeat("S", 20<<10) + sessTitle,
	}}, logs)
	r.testArchiveLimit = 12 << 10
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: "tv", DeviceName: "TV"})
	r.Observe(Event{Time: now, Kind: KindPlaybackRequested, DeviceKey: "tv", SessionKey: "sess-one", Username: eventUser, Title: eventTitle})
	view, blob := finishDevice(t, r, now, [3]bool{true, true, true})
	names := zipEntryNames(t, blob)
	if view.Preview == nil || !sameStrings(names, view.Preview.Files) {
		t.Fatalf("preview %v zip %v", view.Preview.Files, names)
	}
	if containsName(names, "logs/capture.log") || containsName(names, "sessions.json") {
		t.Fatalf("budget-dropped files remained %v", names)
	}
	if !containsName(names, "events.jsonl") || !containsName(names, "summary.json") {
		t.Fatalf("kept files missing %v", names)
	}
	id := view.Preview.Identity
	if !containsName(id.Usernames, eventUser) || !containsName(id.Titles, eventTitle) {
		t.Fatalf("retained identity missing %+v", id)
	}
	blobText := string(bytes.Join(valuesOf(unzip(t, blob)), nil))
	for _, banned := range []string{logUser, "LogOnlyTitle", sessTitle} {
		if containsName(id.Usernames, banned) || containsName(id.Titles, banned) || strings.Contains(blobText, banned) {
			t.Fatalf("dropped value %s was advertised", banned)
		}
	}
	omissions := string(unzip(t, blob)["omissions.json"])
	if !strings.Contains(omissions, "logs/capture.log") || !strings.Contains(omissions, "sessions.json") {
		t.Fatalf("budget gaps missing: %s", omissions)
	}
	var summary struct {
		Identity IdentityPreview `json:"identity"`
	}
	if err := json.Unmarshal(unzip(t, blob)["summary.json"], &summary); err != nil {
		t.Fatal(err)
	}
	if !sameIdentity(summary.Identity, id) {
		t.Fatalf("summary %+v preview %+v", summary.Identity, id)
	}
}

func finishRecorder(t *testing.T, now time.Time, sessions []SessionFact, logs []LogRecord) *Recorder {
	t.Helper()
	return New(Deps{
		Now: func() time.Time { return now },
		Settings: func() (Settings, error) {
			return Settings{FriendlyName: "TV", LogLevel: "info"}, nil
		},
		Sessions: func(string) ([]SessionFact, int, error) {
			return sessions, 0, nil
		},
		Hardware:     func() (HardwareFact, error) { return HardwareFact{Name: "VideoToolbox", Available: true}, nil },
		Storage:      func() (StorageFact, error) { return StorageFact{DiskFreeKnown: true, DiskFreeBytes: 10}, nil },
		Connectivity: func() (ConnectivityFact, error) { return ConnectivityFact{SecureConnections: "preferred"}, nil },
		LogExtract: func(LogQuery) ([]LogRecord, bool, error) {
			return logs, false, nil
		},
	})
}

func finishDevice(t *testing.T, r *Recorder, now time.Time, flags [3]bool) (CaptureView, []byte) {
	t.Helper()
	var id string
	for _, d := range r.Devices() {
		if d.Name == "TV" {
			id = d.ID
		}
	}
	if id == "" {
		t.Fatal("device missing")
	}
	v, err := r.Start("owner", StartIn{DeviceID: id})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := r.Finish("owner", v.ID, FinishIn{
		SetFlags: true, IncludeTitles: flags[0], IncludeUsernames: flags[1], IncludeAddresses: flags[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.Archive("owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) < 2 || blob[0] != 'P' || blob[1] != 'K' {
		t.Fatal("not a zip")
	}
	return finished, blob
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func unzip(t *testing.T, blob []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

func valuesOf(files map[string][]byte) [][]byte {
	out := make([][]byte, 0, len(files))
	for _, b := range files {
		out = append(out, b)
	}
	return out
}
