package diagnostics

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Seeded values a reviewer can search for. They are fixtures, not live credentials.
const (
	fxClient      = "chosen-client"
	fxOther       = "other-viewer-client"
	fxPrefix      = "chosen-client-extra"
	fxSess1       = "sess-one"
	fxSess2       = "sess-two"
	fxUser        = "alice"
	fxOtherUser   = "bob"
	fxPeer        = "192.0.2.1"
	fxOtherPeer   = "198.51.100.8"
	fxTitle       = "Movie.Name"
	fxOtherTitle  = "Other.Show"
	fxPath        = "/tmp/library/movie.mkv"
	fxOtherPath   = "/tmp/other/show.mkv"
	fxJSONSecret  = "cred-Q7n4<wP>&x"
	fxPlainSecret = "plain secret/alpha"
)

func TestConfiguredSecretSurvivesNeitherJSONNorAlias(t *testing.T) {
	uses, _ := prepareSecrets([]string{fxJSONSecret, fxPlainSecret})
	raw, err := json.Marshal(map[string]string{"note": "x " + fxJSONSecret + " " + url.QueryEscape(fxPlainSecret)})
	if err != nil {
		t.Fatal(err)
	}
	if !leaks(raw, uses) {
		t.Fatalf("json encoding hid a configured secret: %s", raw)
	}
	line, err := json.Marshal(map[string]string{"ffmpeg": fxPlainSecret})
	if err != nil {
		t.Fatal(err)
	}
	jsonl := append(append([]byte("{}\n"), line...), '\n')
	if !leaks(jsonl, uses) {
		t.Fatal("jsonl encoding hid a configured secret")
	}
	for _, scope := range []string{"device", "session", "attempt"} {
		alias := Alias(scope, fxJSONSecret)
		if alias == fxJSONSecret || strings.Contains(alias, "Q7n4") || strings.Contains(alias, "<") {
			t.Fatalf("alias %s kept secret material %q", scope, alias)
		}
	}
}

func TestWritePrivacyReviewFixtures(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		flags [3]bool
		want  []string
		ban   []string
	}{
		{name: "default", flags: [3]bool{false, false, false}, ban: []string{fxUser, fxPeer, fxTitle, "Movie [redacted] Cut"}},
		{name: "titles", flags: [3]bool{true, false, false}, want: []string{fxTitle, "Movie [redacted] Cut"}, ban: []string{fxUser, fxPeer}},
		{name: "usernames", flags: [3]bool{false, true, false}, want: []string{fxUser}, ban: []string{fxPeer, fxTitle, "Movie [redacted] Cut"}},
		{name: "addresses", flags: [3]bool{false, false, true}, want: []string{fxPeer}, ban: []string{fxUser, fxTitle, "Movie [redacted] Cut"}},
		{name: "all", flags: [3]bool{true, true, true}, want: []string{fxUser, fxPeer, fxTitle, "Movie [redacted] Cut"}},
	}
	dir := t.TempDir()
	if env := os.Getenv("ETHER_SUPPORT_REVIEW_DIR"); env != "" {
		dir = env
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seed := "Privacy review seeds. These strings must not appear inside any ZIP or finish preview,\n" +
		"except the opt-in values named in each case's finish-preview.json.\n" +
		"This file is not part of an archive.\n\n" +
		"json secret: " + fxJSONSecret + "\n" +
		"plain secret: " + fxPlainSecret + "\n" +
		"percent-encoded plain secret: " + url.QueryEscape(fxPlainSecret) + "\n" +
		"other viewer: " + fxOther + " user " + fxOtherUser + " peer " + fxOtherPeer + " title " + fxOtherTitle + "\n" +
		"paths: " + fxPath + " " + fxOtherPath + "\n" +
		"raw correlation ids: " + fxClient + " " + fxSess1 + " " + fxSess2 + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SEED.txt"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, blob := finishFixture(t, now, tc.flags)
			caseDir := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(caseDir, 0o755); err != nil {
				t.Fatal(err)
			}
			zipPath := filepath.Join(caseDir, "ether-support.zip")
			previewPath := filepath.Join(caseDir, "finish-preview.json")
			if err := os.WriteFile(zipPath, blob, 0o644); err != nil {
				t.Fatal(err)
			}
			pretty, err := json.MarshalIndent(view, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			pretty = append(pretty, '\n')
			if err := os.WriteFile(previewPath, pretty, 0o644); err != nil {
				t.Fatal(err)
			}
			files := readZip(t, blob)
			assertManifest(t, files, view.ID)
			names := zipEntryNames(t, blob)
			if view.Preview == nil || len(view.Preview.Files) == 0 || !sameStrings(names, view.Preview.Files) {
				t.Fatalf("preview files %v zip %v", view.Preview.Files, names)
			}
			text := zipText(files) + "\n" + string(pretty)
			for _, s := range tc.want {
				if !strings.Contains(text, s) {
					t.Fatalf("missing opted-in value %q", s)
				}
				if !previewLists(view, s) {
					t.Fatalf("finish preview did not list %q", s)
				}
			}
			for _, s := range tc.ban {
				if strings.Contains(text, s) {
					t.Fatalf("archive or preview contained %q", s)
				}
			}
			for _, s := range alwaysBanned() {
				if strings.Contains(text, s) {
					t.Fatalf("archive or preview contained %q", s)
				}
			}
			if !strings.Contains(text, "Free-text log messages") {
				t.Fatal("missing free-text omission")
			}
			a1, a2 := Alias("session", fxSess1), Alias("session", fxSess2)
			if a1 == a2 || !strings.Contains(string(files["events.jsonl"]), a1) || !strings.Contains(string(files["events.jsonl"]), a2) {
				t.Fatal("session aliases were not distinct in events")
			}
			if !strings.Contains(string(files["logs/capture.log"]), a1) || !strings.Contains(string(files["logs/capture.log"]), a2) {
				t.Fatal("session aliases were not shared with logs")
			}
			if !strings.Contains(string(files["logs/capture.log"]), Alias("attempt", fxJSONSecret)) {
				t.Fatal("attempt alias missing from logs")
			}
			if _, ok := files["settings.json"]; !ok {
				t.Fatal("settings.json missing")
			}
			if !strings.Contains(string(files["settings.json"]), "House [redacted]") {
				t.Fatal("settings secret was not replaced")
			}
		})
	}
}

func alwaysBanned() []string {
	uses, _ := prepareSecrets([]string{fxJSONSecret, fxPlainSecret})
	var out []string
	out = append(out, fxOther, fxPrefix, fxOtherUser, fxOtherPeer, fxOtherTitle, fxPath, fxOtherPath, fxClient, fxSess1, fxSess2)
	for _, u := range uses {
		out = append(out, u.forms...)
	}
	out = append(out, url.QueryEscape(fxJSONSecret), url.QueryEscape(fxPlainSecret))
	var html bytes.Buffer
	json.HTMLEscape(&html, []byte(fxJSONSecret))
	out = append(out, html.String())
	html.Reset()
	json.HTMLEscape(&html, []byte(fxPlainSecret))
	out = append(out, html.String())
	return out
}

func previewLists(view CaptureView, s string) bool {
	if view.Preview == nil {
		return false
	}
	id := view.Preview.Identity
	for _, v := range append(append(append([]string{}, id.Usernames...), id.Addresses...), id.Titles...) {
		if v == s {
			return true
		}
	}
	return false
}

func finishFixture(t *testing.T, now time.Time, flags [3]bool) (CaptureView, []byte) {
	t.Helper()
	records := fixtureLogs(now)
	r := New(Deps{
		Now: func() time.Time { return now },
		Secrets: func() []string {
			return []string{fxJSONSecret, fxPlainSecret}
		},
		Settings: func() (Settings, error) {
			return Settings{FriendlyName: "House " + fxJSONSecret, LogLevel: "info"}, nil
		},
		Sessions: func(deviceKey string) ([]SessionFact, int, error) {
			if deviceKey != fxClient {
				return nil, 0, nil
			}
			return []SessionFact{{
				DeviceKey: fxClient, State: "playing", Title: fxTitle, Username: fxUser, Peer: fxPeer,
			}}, 1, nil
		},
		Hardware:     func() (HardwareFact, error) { return HardwareFact{Name: "VideoToolbox", Available: true}, nil },
		Storage:      func() (StorageFact, error) { return StorageFact{DiskFreeKnown: true, DiskFreeBytes: 1024}, nil },
		Connectivity: func() (ConnectivityFact, error) { return ConnectivityFact{SecureConnections: "preferred"}, nil },
		LogExtract: func(LogQuery) ([]LogRecord, bool, error) {
			return records, false, nil
		},
	})
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: fxClient, DeviceName: "Living Room", Product: "Plex for Samsung"})
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: fxOther, DeviceName: "Other Room"})
	r.Observe(Event{
		Time: now, Kind: KindPlaybackRequested, DeviceKey: fxClient, SessionKey: fxSess1, AttemptKey: fxJSONSecret,
		Username: fxUser, Peer: fxPeer, Title: fxTitle,
		Route: "/library/" + fxJSONSecret + "/file.mkv", FFmpegExcerpt: "ffmpeg " + fxPlainSecret,
	})
	r.Observe(Event{
		Time: now, Kind: KindPlaybackRequested, DeviceKey: fxClient, SessionKey: fxSess2,
		Username: fxUser, Peer: fxPeer, Title: "Movie " + fxJSONSecret + " Cut",
	})
	r.Observe(Event{
		Time: now, Kind: KindPlaybackRequested, DeviceKey: fxOther, SessionKey: "sess-bob",
		Username: fxOtherUser, Peer: fxOtherPeer, Title: fxOtherTitle, Route: fxOtherPath,
	})
	var deviceID string
	for _, d := range r.Devices() {
		if d.Name == "Living Room" {
			deviceID = d.ID
		}
	}
	if deviceID == "" {
		t.Fatal("chosen device was not listed")
	}
	v, err := r.Start("owner", StartIn{
		DeviceID: deviceID,
		Category: "playback",
		Note:     "see " + fxJSONSecret + " and " + url.QueryEscape(fxPlainSecret),
	})
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
	return finished, blob
}

func fixtureLogs(now time.Time) []LogRecord {
	return []LogRecord{
		{Time: now, Level: "INFO", Message: "user=" + fxUser + " peer=" + fxPeer + " release=" + fxTitle + " path=" + fxPath + " " + fxJSONSecret + " " + fxPlainSecret, Attrs: map[string]string{
			"client": fxClient, "session": fxSess1, "attempt": fxJSONSecret, "user": fxUser, "peer": fxPeer,
			"release": fxTitle, "path": fxPath, "token": url.QueryEscape(fxPlainSecret),
		}},
		{Time: now, Level: "INFO", Message: "second attempt " + fxPlainSecret, Attrs: map[string]string{
			"client": fxClient, "session": fxSess2, "user": fxUser, "release": fxTitle, "path": fxPath,
		}},
		{Time: now, Level: "INFO", Message: "other viewer " + fxOtherUser, Attrs: map[string]string{
			"client": fxOther, "user": fxOtherUser, "peer": fxOtherPeer, "release": fxOtherTitle, "path": fxOtherPath,
		}},
		{Time: now, Level: "INFO", Message: "prefix trap " + fxUser, Attrs: map[string]string{
			"client": fxPrefix, "user": fxOtherUser,
		}},
		{Time: now.Add(-2 * time.Hour), Level: "INFO", Message: fxJSONSecret, Attrs: map[string]string{
			"client": fxClient, "user": fxUser, "path": fxPath,
		}},
		{Time: now, Level: "INFO", Message: "a playback " + fxJSONSecret, Attrs: map[string]string{
			"msg": "a playback " + fxJSONSecret,
		}},
	}
}

func zipEntryNames(t *testing.T, blob []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func readZip(t *testing.T, blob []byte) map[string][]byte {
	t.Helper()
	if len(blob) < 2 || blob[0] != 'P' || blob[1] != 'K' {
		t.Fatal("archive is not a zip")
	}
	if len(blob) > maxZip {
		t.Fatalf("zip is %d bytes", len(blob))
	}
	zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			t.Fatalf("unsafe zip name %s", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxUncompressed+1))
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

func assertManifest(t *testing.T, files map[string][]byte, captureID string) {
	t.Helper()
	raw, ok := files["manifest.json"]
	if !ok {
		t.Fatal("manifest.json missing")
	}
	if !json.Valid(raw) {
		t.Fatal("manifest.json is not json")
	}
	var man manifestDoc
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if man.Schema != Schema || man.CaptureID != captureID {
		t.Fatalf("manifest schema %s id %s", man.Schema, man.CaptureID)
	}
	if _, ok := files["omissions.json"]; !ok {
		t.Fatal("omissions.json missing")
	}
	if !json.Valid(files["omissions.json"]) {
		t.Fatal("omissions.json is not json")
	}
	listed := map[string]fileInfo{}
	for _, f := range man.Files {
		listed[f.Name] = f
	}
	if _, ok := listed["manifest.json"]; ok {
		t.Fatal("manifest lists itself")
	}
	for name, body := range files {
		if name == "manifest.json" {
			continue
		}
		info, ok := listed[name]
		if !ok {
			t.Fatalf("manifest missing %s", name)
		}
		sum := sha256.Sum256(body)
		if info.Bytes != len(body) || info.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("manifest hash mismatch for %s", name)
		}
		if strings.HasSuffix(name, ".json") && !json.Valid(body) {
			t.Fatalf("%s is not json", name)
		}
		if strings.HasSuffix(name, ".jsonl") {
			for _, line := range bytes.Split(body, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if len(line) > 0 && !json.Valid(line) {
					t.Fatalf("invalid jsonl in %s", name)
				}
			}
		}
	}
	for name := range listed {
		if _, ok := files[name]; !ok {
			t.Fatalf("manifest lists missing file %s", name)
		}
	}
}

func zipText(files map[string][]byte) string {
	var b strings.Builder
	for _, body := range files {
		b.Write(body)
		b.WriteByte('\n')
	}
	return b.String()
}
