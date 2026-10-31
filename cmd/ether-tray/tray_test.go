package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/updater"
)

func boolp(b bool) *bool { return &b }

func testServer(t *testing.T, withAPI bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<MediaContainer size="0" machineIdentifier="abc123" friendlyName="Living Room" version="1.2.3" platform="Ether"/>`)
	})
	if withAPI {
		mux.HandleFunc("/ether/api/status", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"version":"1.2.4","setupComplete":true,"activeStreams":2,"somethingNew":true}`)
		})
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestPollParsesIdentityAndAPI(t *testing.T) {
	s := testServer(t, true)
	st, err := NewStatusClient(s.URL, s.Client()).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.MachineID != "abc123" || st.FriendlyName != "Living Room" {
		t.Fatalf("st=%+v", st)
	}
	if st.Version != "1.2.4" { // API version wins over identity
		t.Errorf("version=%q", st.Version)
	}
	if st.SetupComplete == nil || !*st.SetupComplete || st.Streams != 2 {
		t.Errorf("setup/streams: %+v", st)
	}
}

func TestPollIdentityOnly(t *testing.T) {
	s := testServer(t, false)
	st, err := NewStatusClient(s.URL, s.Client()).Poll(context.Background())
	if err != nil || !st.Running || st.Version != "1.2.3" || st.SetupComplete != nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestPollNegotiatesIdentityXMLAndStatusJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				fmt.Fprint(w, `{"MediaContainer":{"machineIdentifier":"abc123"}}`)
				return
			}
			fmt.Fprint(w, `<MediaContainer machineIdentifier="abc123" version="1.2.3"/>`)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "JSON required", http.StatusNotAcceptable)
			return
		}
		fmt.Fprint(w, `{"version":"0.2.0"}`)
	}))
	defer srv.Close()
	st, err := NewStatusClient(srv.URL, srv.Client()).Poll(context.Background())
	if err != nil || !st.Running || st.Version != "0.2.0" {
		t.Fatalf("status=%+v err=%v", st, err)
	}
}

func TestPollServerDown(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	st, err := NewStatusClient(url, http.DefaultClient).Poll(context.Background())
	if err == nil || st.Running {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestStatusLine(t *testing.T) {
	if got := statusLine(Status{}); got != "Ether: not running" {
		t.Errorf("down: %q", got)
	}
	got := statusLine(Status{Running: true, FriendlyName: "Den", Version: "1.0.0", SetupComplete: boolp(false)})
	if got != "Ether: running (Den) · v1.0.0 · setup needed" {
		t.Errorf("up: %q", got)
	}
	got = statusLine(Status{Running: true, FriendlyName: "Ether"})
	if got != "Ether: running" {
		t.Errorf("default name: %q", got)
	}
	got = statusLine(Status{Running: true, Version: "1.43.4", VersionIsPMS: true})
	if got != "Ether: running" {
		t.Errorf("PMS compatibility version shown as Ether release: %q", got)
	}
}

func TestAutostartDarwinAndLinux(t *testing.T) {
	for goos, marker := range map[string]string{
		"darwin": "io.github.braedonsaunders.ether-tray",
		"linux":  "[Desktop Entry]",
	} {
		home := t.TempDir()
		a := &Autostart{GOOS: goos, HomeDir: home, ExecPath: "/opt/Ether/ether-tray"}
		if a.Enabled() {
			t.Fatalf("%s: enabled before install", goos)
		}
		if err := a.Enable(); err != nil {
			t.Fatalf("%s enable: %v", goos, err)
		}
		if !a.Enabled() {
			t.Fatalf("%s: not enabled after Enable", goos)
		}
		// File contents must reference the executable.
		var path string
		if goos == "darwin" {
			path = a.launchAgentPath()
		} else {
			path = a.desktopPath()
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), marker) || !strings.Contains(string(b), "/opt/Ether/ether-tray") {
			t.Errorf("%s content:\n%s", goos, b)
		}
		if err := a.Disable(); err != nil || a.Enabled() {
			t.Fatalf("%s disable: %v enabled=%v", goos, err, a.Enabled())
		}
		if err := a.Disable(); err != nil { // idempotent
			t.Fatalf("%s disable again: %v", goos, err)
		}
	}
}

// Exec paths with spaces or specials must be quoted per the XDG spec, or
// the session splits "/home/me/My Apps/ether-tray" into two arguments.
func TestDesktopEntryQuotesExec(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/Ether/ether-tray":          "Exec=/opt/Ether/ether-tray\n",
		"/home/me/My Apps/ether-tray":    `Exec="/home/me/My Apps/ether-tray"` + "\n",
		`/home/me/a"b$c/ether-tray`:      `Exec="/home/me/a\\"b\\$c/ether-tray"` + "\n",
		"/home/me/100%/ether-tray":       "Exec=/home/me/100%%/ether-tray\n",
		`/home/me/back\slash/ether-tray`: `Exec="/home/me/back\\\\slash/ether-tray"` + "\n",
	} {
		got := (&Autostart{ExecPath: path}).DesktopEntry()
		if !strings.Contains(got, want) {
			t.Errorf("%q: entry\n%s\nwant line %q", path, got, want)
		}
	}
}

func TestAutostartWindowsCommands(t *testing.T) {
	var calls [][]string
	a := &Autostart{
		GOOS: "windows", ExecPath: `C:\Program Files\Ether\ether-tray.exe`,
		Run: func(name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
		QueryRunValue: func() (string, bool) { return "", len(calls) > 0 },
	}
	if a.Enabled() {
		t.Fatal("enabled before add")
	}
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	if !a.Enabled() {
		t.Fatal("not enabled after add")
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "reg add") || !strings.Contains(joined, "EtherTray") || !strings.Contains(joined, "ether-tray.exe") {
		t.Errorf("add call: %s", joined)
	}
	if err := a.Disable(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls[1], " "), "reg delete") {
		t.Errorf("delete call: %v", calls[1])
	}
}

func TestOpenCommand(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "open", "windows": "rundll32", "linux": "xdg-open"} {
		name, args := openCommand(goos, "http://x/")
		if name != want || args[len(args)-1] != "http://x/" {
			t.Errorf("%s: %s %v", goos, name, args)
		}
	}
}

type fakeSvc struct{ actions []string }

func (f *fakeSvc) Start() error   { f.actions = append(f.actions, "start"); return nil }
func (f *fakeSvc) Stop() error    { f.actions = append(f.actions, "stop"); return nil }
func (f *fakeSvc) Restart() error { f.actions = append(f.actions, "restart"); return nil }
func (f *fakeSvc) Status() (service.Status, error) {
	return service.StatusRunning, nil
}

type fakeUpdates struct {
	upd     *updater.Update
	applied []updater.ApplyOptions
}

func (f *fakeUpdates) Check(context.Context) (*updater.Update, error) { return f.upd, nil }
func (f *fakeUpdates) Apply(_ context.Context, _ *updater.Update, opts updater.ApplyOptions) (string, error) {
	f.applied = append(f.applied, opts)
	return "/tmp/Ether.dmg", nil
}

func trayUpdate(kind string) *updater.Update {
	return &updater.Update{
		Manifest: &updater.Manifest{Version: "2.0.0"},
		Artifact: &updater.Artifact{Kind: kind},
	}
}

// The tray must never replace a binary or restart the service for an
// update, even with auto-update on: that used to overwrite the tray with
// the server binary and cut off active streams.
func TestCheckUpdatesNeverApplies(t *testing.T) {
	fu := &fakeUpdates{upd: trayUpdate(updater.KindBinary)}
	svc := &fakeSvc{}
	a := &app{updates: fu, installer: fu, cfg: config.Updates{AutoUpdate: true}, svc: svc, dataDir: t.TempDir()}
	got, err := a.checkUpdates(context.Background())
	if err != nil || got != fu.upd {
		t.Fatalf("upd=%v err=%v", got, err)
	}
	if len(fu.applied) != 0 || len(svc.actions) != 0 {
		t.Fatalf("tray applied %v / service actions %v", fu.applied, svc.actions)
	}
	if _, err := (&app{}).checkUpdates(context.Background()); err == nil {
		t.Fatal("dev build without a checker should report unavailable")
	}
}

func TestOpenUpdate(t *testing.T) {
	cases := []struct {
		goos, kind   string
		wantLaunched bool
	}{
		{"darwin", updater.KindInstaller, true},
		{"darwin", updater.KindBinary, false},
		{"windows", updater.KindBinary, false},
		{"windows", updater.KindInstaller, false},
		{"linux", updater.KindInstaller, false},
	}
	for _, c := range cases {
		fu := &fakeUpdates{}
		var opened []string
		a := &app{
			baseURL: "http://127.0.0.1:32400", installer: fu, goos: c.goos, log: slog.Default(),
			openURL: func(u string) error { opened = append(opened, u); return nil },
		}
		launched, err := a.openUpdate(context.Background(), trayUpdate(c.kind))
		if err != nil || launched != c.wantLaunched {
			t.Errorf("%s/%s: launched=%v err=%v", c.goos, c.kind, launched, err)
			continue
		}
		if c.wantLaunched {
			if len(fu.applied) != 1 || len(opened) != 0 {
				t.Errorf("%s/%s: applied=%v opened=%v", c.goos, c.kind, fu.applied, opened)
			}
			continue
		}
		if len(fu.applied) != 0 || len(opened) != 1 || opened[0] != "http://127.0.0.1:32400/ether/maintenance/updates" {
			t.Errorf("%s/%s: applied=%v opened=%v", c.goos, c.kind, fu.applied, opened)
		}
	}
}

func TestUpdateOptionsDefaults(t *testing.T) {
	o := updateOptions(config.Updates{}, "windows", nil)
	if o.Channel != updater.ChannelStable || len(o.PreferKinds) != 0 {
		t.Errorf("unreadable config: %+v", o)
	}
	o = updateOptions(config.Updates{Channel: "nightly"}, "linux", nil)
	if o.Channel != updater.ChannelStable {
		t.Errorf("unknown channel: %q", o.Channel)
	}
	o = updateOptions(config.Updates{Channel: "beta", FeedURL: "https://example.test/feed"}, "darwin", nil)
	if o.Channel != updater.ChannelBeta || o.FeedURL != "https://example.test/feed" || len(o.PreferKinds) == 0 || o.PreferKinds[0] != updater.KindInstaller {
		t.Errorf("darwin beta: %+v", o)
	}
}

func TestServiceActionAndIconsEmbedded(t *testing.T) {
	svc := &fakeSvc{}
	a := &app{svc: svc}
	for _, act := range []string{"start", "stop", "restart"} {
		if err := a.serviceAction(act); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(svc.actions, ",") != "start,stop,restart" {
		t.Errorf("actions=%v", svc.actions)
	}
	if err := a.serviceAction("bogus"); err == nil {
		t.Error("bogus action should fail")
	}
	for name, b := range map[string][]byte{"template": iconTemplate, "png": iconPNG, "off": iconOffPNG, "ico": iconICO} {
		if len(b) == 0 {
			t.Errorf("embedded icon %s empty", name)
		}
	}
	if p := serverExecutable(); p != "" && !filepath.IsAbs(p) {
		t.Errorf("serverExecutable should be absolute or empty, got %q", p)
	}
}
