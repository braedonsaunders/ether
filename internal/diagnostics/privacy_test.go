package diagnostics

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCorrelationIsExactAndInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	q := LogQuery{From: now.Add(-time.Minute), Until: now.Add(time.Minute), DeviceKey: "abc"}
	if LogCorrelated(LogRecord{Time: now, Attrs: map[string]string{"client": "abcdef"}}, q) {
		t.Fatal("shorter id matched a longer client")
	}
	q.DeviceKey = "a"
	if LogCorrelated(LogRecord{Time: now, Message: "a playback", Attrs: map[string]string{"msg": "a playback"}}, q) {
		t.Fatal("substring of free text matched")
	}
	q.DeviceKey = "abc"
	if LogCorrelated(LogRecord{Time: now.Add(-2 * time.Hour), Attrs: map[string]string{"client": "abc"}}, q) {
		t.Fatal("event outside the capture window matched")
	}
	if !LogCorrelated(LogRecord{Time: now, Attrs: map[string]string{"client": "abc"}}, q) {
		t.Fatal("exact client in window was dropped")
	}
	line := `time=2026-10-30T15:00:00Z level=INFO msg="playback" client=abc user=alice peer=192.0.2.1 release=Movie.Name path=/tmp/library/movie.mkv`
	rec, ok := ParseLogLine(line)
	if !ok || rec.Message != "playback" || rec.Attrs["user"] != "alice" || rec.Attrs["path"] != "/tmp/library/movie.mkv" {
		t.Fatalf("parse %+v", rec)
	}
	if !LogCorrelated(rec, q) {
		t.Fatal("parsed line was not correlated")
	}
}

func TestLogFlagsDoNotLeakUnlistedIdentity(t *testing.T) {
	const (
		client = "chosen-client"
		sess1  = "sess-one"
		sess2  = "sess-two"
		user   = "alice"
		peer   = "192.0.2.1"
		title  = "Movie.Name"
		path   = "/tmp/library/movie.mkv"
		other  = "bob"
	)
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	records := []LogRecord{
		{Time: now, Level: "INFO", Message: "user=" + user + " peer=" + peer + " release=" + title + " path=" + path, Attrs: map[string]string{
			"client": client, "session": sess1, "user": user, "peer": peer, "release": title, "path": path,
		}},
		{Time: now, Level: "INFO", Message: "second attempt", Attrs: map[string]string{
			"client": client, "session": sess2, "user": user, "release": title,
		}},
		{Time: now, Attrs: map[string]string{"client": client + "-extra", "user": other, "peer": "198.51.100.8"}},
		{Time: now.Add(-2 * time.Hour), Attrs: map[string]string{"client": client, "user": user}},
		{Time: now, Message: "a playback", Attrs: map[string]string{"msg": "a playback"}},
	}
	cases := []struct {
		name  string
		flags [3]bool
		want  []string
		ban   []string
	}{
		{name: "none", flags: [3]bool{false, false, false}, ban: []string{user, peer, title, path, other, client, sess1, sess2}},
		{name: "users", flags: [3]bool{false, true, false}, want: []string{user}, ban: []string{peer, title, path, other}},
		{name: "peers", flags: [3]bool{false, false, true}, want: []string{peer}, ban: []string{user, title, path, other}},
		{name: "titles", flags: [3]bool{true, false, false}, want: []string{title}, ban: []string{user, peer, path, other}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, listed := finishZIP(t, now, client, sess1, sess2, records, tc.flags)
			for _, s := range tc.want {
				if !strings.Contains(text, s) {
					t.Fatalf("zip missing opted-in value %q", s)
				}
				if !strings.Contains(listed, s) {
					t.Fatalf("summary did not list %q", s)
				}
			}
			for _, s := range tc.ban {
				if strings.Contains(text, s) {
					t.Fatalf("zip contained %q", s)
				}
			}
			a1, a2 := Alias("session", sess1), Alias("session", sess2)
			if a1 == a2 || a1 == "device" || a2 == "device" {
				t.Fatalf("aliases %s %s", a1, a2)
			}
			if !strings.Contains(text, a1) || !strings.Contains(text, a2) {
				t.Fatalf("aliases not shared across the archive")
			}
			if !strings.Contains(text, "Free-text log messages") {
				t.Fatal("missing free-text omission")
			}
		})
	}
}

func TestLogProjectionDoesNotCutARecord(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	secret := "super-secret-value-that-must-stay-whole"
	records := []LogRecord{
		{Time: now, Attrs: map[string]string{"client": "dev", "user": secret}},
		{Time: now, Attrs: map[string]string{"client": "dev", "user": "second"}},
	}
	q := LogQuery{From: now.Add(-time.Second), Until: now.Add(time.Second), DeviceKey: "dev", MaxLines: 1}
	body, ident, truncated, _ := projectLogs(records, q, [3]bool{false, true, false}, nil)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if strings.Count(string(body), "\n") != 1 {
		t.Fatalf("lines %q", body)
	}
	if strings.Contains(string(body), secret[:8]) && !strings.Contains(string(body), secret) {
		t.Fatal("secret was cut mid-value")
	}
	if len(ident.users) != 1 || ident.users[0] != secret {
		t.Fatalf("identity %+v", ident.users)
	}
}

func finishZIP(t *testing.T, now time.Time, client, sess1, sess2 string, records []LogRecord, flags [3]bool) (zipText, listed string) {
	t.Helper()
	r := New(Deps{Now: func() time.Time { return now }, LogExtract: func(LogQuery) ([]LogRecord, bool, error) {
		return records, false, nil
	}})
	r.Observe(Event{Time: now, Kind: KindDevice, DeviceKey: client, DeviceName: "TV"})
	r.Observe(Event{Time: now, Kind: KindPlaybackRequested, DeviceKey: client, SessionKey: sess1, Username: "alice", Peer: "192.0.2.1", Title: "Movie.Name"})
	r.Observe(Event{Time: now, Kind: KindPlaybackRequested, DeviceKey: client, SessionKey: sess2, Username: "alice", Peer: "192.0.2.1", Title: "Movie.Name"})
	v, err := r.Start("owner", StartIn{DeviceID: r.Devices()[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := r.Finish("owner", v.ID, FinishIn{SetFlags: true, IncludeTitles: flags[0], IncludeUsernames: flags[1], IncludeAddresses: flags[2]})
	if err != nil {
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
	var all strings.Builder
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		all.Write(b)
	}
	listed = finished.Preview.Identity.Explanation
	for _, s := range finished.Preview.Identity.Usernames {
		listed += "\n" + s
	}
	for _, s := range finished.Preview.Identity.Addresses {
		listed += "\n" + s
	}
	for _, s := range finished.Preview.Identity.Titles {
		listed += "\n" + s
	}
	return all.String(), listed
}
