package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	// Ordered list from the semver spec §11 example, plus Ether cases.
	ordered := []string{
		"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1",
		"1.0.0", "v1.0.1", "1.2", "1.2.3+build5", "2.0.0",
	}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			got, err := CompareVersions(ordered[i], ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			// 1.2.3+build5 vs 1.2: build ignored, 1.2.3 > 1.2.0 already ordered.
			if got != want {
				t.Errorf("Compare(%q,%q)=%d want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if _, err := ParseVersion(""); err == nil {
		t.Error("empty version should fail")
	}
	if _, err := ParseVersion("1.x.0"); err == nil {
		t.Error("non-numeric should fail")
	}
	if !Newer("1.0.0", "dev") || Newer("garbage", "dev") || !Newer("2.0.0", "1.9.9") || Newer("1.0.0", "1.0.0") {
		t.Error("Newer semantics wrong")
	}
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type feed struct {
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	payload []byte
	server  *httptest.Server
	hits    atomic.Int64
}

func newFeed(t *testing.T, mutate func(m *Manifest), detached bool) *feed {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	f := &feed{pub: pub, priv: priv, payload: []byte("#!/bin/sh\necho ether-new\n")}
	mux := http.NewServeMux()
	m := &Manifest{
		Version: "1.2.0", Channel: "stable", PublishedAt: time.Now(),
		NotesURL: "https://example.invalid/notes",
		Artifacts: []Artifact{
			{OS: runtime.GOOS, Arch: runtime.GOARCH, Kind: KindBinary,
				URL: "REPLACED", SHA256: shaHex(f.payload), Size: int64(len(f.payload)), Filename: "ether"},
			{OS: "plan9", Arch: "arm", Kind: KindInstaller, URL: "https://example.invalid/x",
				SHA256: shaHex([]byte("x")), Size: 1},
		},
	}
	m.RenewFreshness(time.Now())
	f.server = httptest.NewServer(mux)
	m.Artifacts[0].URL = f.server.URL + "/ether"
	if mutate != nil {
		mutate(m)
	}
	var manifestBytes []byte
	var sig string
	if detached {
		manifestBytes, _ = json.Marshal(m)
		sig, err = SignDetached(priv, manifestBytes)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if err := m.Sign(priv); err != nil {
			t.Fatal(err)
		}
		manifestBytes, _ = json.Marshal(m)
	}
	mux.HandleFunc("/stable.json", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write(manifestBytes)
	})
	mux.HandleFunc("/stable.json.sig", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sig)
	})
	mux.HandleFunc("/ether", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.payload)
	})
	t.Cleanup(f.server.Close)
	return f
}

func (f *feed) client(t *testing.T, current string) *Client {
	t.Helper()
	c, err := NewClient(Options{
		FeedURL: f.server.URL, Channel: "stable",
		CurrentVersion: current, PublicKey: f.pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckFindsUpdateEmbeddedSig(t *testing.T) {
	f := newFeed(t, nil, false)
	upd, err := f.client(t, "1.1.0").Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if upd == nil || upd.Version() != "1.2.0" || upd.Artifact.Kind != KindBinary {
		t.Fatalf("upd = %+v", upd)
	}
}

func TestCheckDetachedSig(t *testing.T) {
	f := newFeed(t, nil, true)
	upd, err := f.client(t, "1.1.0").Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if upd == nil {
		t.Fatal("expected update")
	}
}

func TestCheckNoUpdateWhenCurrent(t *testing.T) {
	f := newFeed(t, nil, false)
	upd, err := f.client(t, "1.2.0").Check(context.Background())
	if err != nil || upd != nil {
		t.Fatalf("upd=%v err=%v", upd, err)
	}
}

func TestCheckRejectsTamperedManifest(t *testing.T) {
	// Sign, then tamper the artifact checksum after signing.
	f := newFeed(t, func(m *Manifest) {
		// mutate runs before signing in the fixture, so also flip a bit by
		// re-signing with a different key to simulate tampering.
		_, other, _ := GenerateKeyPair()
		_ = m.Sign(other)
	}, false)
	// Fixture re-signed with the right key after mutate; instead directly
	// verify the wrong-key signature fails via a fresh client key mismatch:
	otherPub, _, _ := GenerateKeyPair()
	c, err := NewClient(Options{FeedURL: f.server.URL, CurrentVersion: "1.0.0", PublicKey: otherPub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestManifestVerifyDirect(t *testing.T) {
	pub, priv, _ := GenerateKeyPair()
	m := &Manifest{Version: "1.0.0", Channel: "stable", Artifacts: []Artifact{
		{OS: "linux", Arch: "amd64", Kind: KindBinary, URL: "https://x/y", SHA256: shaHex([]byte("y")), Size: 1},
	}}
	if err := m.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(pub); err != nil {
		t.Fatal(err)
	}
	m.Version = "9.9.9" // tamper after signing
	if err := m.Verify(pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest verified: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err) // still well-formed, just not authentic
	}
}

func TestCheckMinVersion(t *testing.T) {
	f := newFeed(t, func(m *Manifest) { m.MinVersion = "1.1.5" }, false)
	if _, err := f.client(t, "1.0.0").Check(context.Background()); !errors.Is(err, ErrTooOldToUpdate) {
		t.Fatalf("err = %v, want ErrTooOldToUpdate", err)
	}
}

func TestCheckNoArtifact(t *testing.T) {
	f := newFeed(t, nil, false)
	c, err := NewClient(Options{FeedURL: f.server.URL, CurrentVersion: "1.0.0", PublicKey: f.pub, GOOS: "aix", GOARCH: "ppc64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background()); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("err = %v, want ErrNoArtifact", err)
	}
}

func TestNewClientRequiresKey(t *testing.T) {
	if _, err := NewClient(Options{}); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("err = %v, want ErrNoPublicKey (no compiled-in key in tests)", err)
	}
}

func TestValidateRejectsBadManifests(t *testing.T) {
	bad := &Manifest{Version: "x", Channel: "nightly"}
	if err := bad.Validate(); err == nil {
		t.Error("expected validation failure")
	}
	bad.Version, bad.Channel = "1.0.0", "stable"
	bad.Artifacts = []Artifact{{OS: "linux", Arch: "amd64", Kind: "nope", URL: "u", SHA256: "XYZ"}}
	if err := bad.Validate(); err == nil {
		t.Error("expected artifact validation failure")
	}
}

func TestDownloadAndApplyBinary(t *testing.T) {
	f := newFeed(t, nil, false)
	c := f.client(t, "1.1.0")
	upd, err := c.Check(context.Background())
	if err != nil || upd == nil {
		t.Fatalf("check: %v %v", upd, err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "ether")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sawProgress bool
	file, err := c.Apply(context.Background(), upd, ApplyOptions{
		Target: target, DownloadDir: filepath.Join(dir, "dl"),
		Progress: func(d, total int64) { sawProgress = d > 0 && total == int64(len(f.payload)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if file == "" || !sawProgress {
		t.Errorf("file=%q progress=%v", file, sawProgress)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(f.payload) {
		t.Fatalf("target = %q, want new payload", got)
	}
	old, err := os.ReadFile(target + ".old")
	if err != nil || string(old) != "OLD" {
		t.Fatalf(".old = %q, %v", old, err)
	}
	// Rollback restores.
	if err := Rollback(target); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatalf("after rollback = %q", got)
	}
	if err := CleanupOld(target); err != nil { // no .old left; must be nil
		t.Fatal(err)
	}
}

func TestApplyBinaryRefusesAppBundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "Ether.app")
	if err := os.Mkdir(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "new")
	if err := os.WriteFile(payload, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyBinary(payload, bundle); err == nil {
		t.Fatal("replacing an app bundle must fail")
	}
	if st, err := os.Stat(bundle); err != nil || !st.IsDir() {
		t.Fatal("bundle was removed")
	}
	if err := CleanupOld(bundle); err == nil {
		t.Fatal("cleanup must refuse an app bundle")
	}
}

func TestDownloadRejectsWrongChecksum(t *testing.T) {
	f := newFeed(t, func(m *Manifest) {
		m.Artifacts[0].SHA256 = shaHex([]byte("not the payload"))
	}, false)
	c := f.client(t, "1.1.0")
	upd, err := c.Check(context.Background())
	if err != nil || upd == nil {
		t.Fatalf("check: %v %v", upd, err)
	}
	_, err = c.Download(context.Background(), upd, t.TempDir(), nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
}

// A file name that is not a plain name ("..", "a/.") must never place the
// download (or its .part file) outside dir.
func TestDownloadStaysInsideDir(t *testing.T) {
	payload := []byte("payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	pub, _, _ := GenerateKeyPair()
	c, err := NewClient(Options{CurrentVersion: "1.0.0", PublicKey: pub, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	for _, art := range []*Artifact{
		{Filename: "..", URL: srv.URL + "/x"},
		{Filename: "sub/..", URL: srv.URL + "/x"},
		{Filename: "a/.", URL: srv.URL + "/x"},
		{URL: srv.URL + "/dl/.."},
	} {
		art.SHA256, art.Size = shaHex(payload), int64(len(payload))
		root := t.TempDir()
		dir := filepath.Join(root, "updates")
		got, err := c.DownloadArtifact(context.Background(), art, dir, nil)
		if err != nil {
			t.Fatalf("%+v: %v", art, err)
		}
		if filepath.Dir(got) != dir {
			t.Fatalf("%+v: downloaded to %s, outside %s", art, got, dir)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 1 {
			t.Fatalf("%+v: files written next to the download dir: %v", art, entries)
		}
	}
}

func TestDownloadCancelled(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	pub, _, _ := GenerateKeyPair()
	c, err := NewClient(Options{CurrentVersion: "1.0.0", PublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	art := &Artifact{OS: "linux", Arch: "amd64", Kind: KindBinary, URL: slow.URL + "/ether",
		SHA256: shaHex([]byte("x")), Size: 1, Filename: "ether"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.DownloadArtifact(ctx, art, t.TempDir(), nil); err == nil {
		t.Fatal("cancelled download should fail")
	}
}

func TestApplyInstallerLaunches(t *testing.T) {
	var launched string
	f2 := newFeed(t, func(m *Manifest) {
		for i := range m.Artifacts {
			m.Artifacts[i].Kind = KindInstaller
			m.Artifacts[i].Filename = "EtherSetup.exe"
		}
	}, false)
	c2 := f2.client(t, "1.0.0")
	upd, err := c2.Check(context.Background())
	if err != nil || upd == nil {
		t.Fatalf("check %v %v", upd, err)
	}
	_, err = c2.Apply(context.Background(), upd, ApplyOptions{
		DownloadDir: t.TempDir(),
		LaunchInstaller: func(_ context.Context, path string) error {
			launched = path
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(launched, "EtherSetup.exe") {
		t.Fatalf("launched = %q", launched)
	}
}

// The installer must keep running after the caller's context ends: the tray
// cancels its context as soon as Apply returns.
func TestStartDetachedOutlivesContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	marker := filepath.Join(t.TempDir(), "installer-ran")
	ctx, cancel := context.WithCancel(context.Background())
	if err := startDetached(ctx, "/bin/sh", "-c", `sleep 0.3; echo ok > "$1"`, "sh", marker); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("installer was killed when the caller's context ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := startDetached(ctx, "/bin/sh", "-c", "true"); err == nil {
		t.Fatal("started after the context ended")
	}
}

func TestInstallerCommand(t *testing.T) {
	for _, tc := range []struct {
		goos, path, name string
		args             []string
	}{
		{"darwin", "/tmp/Ether.dmg", "open", []string{"/tmp/Ether.dmg"}},
		{"windows", `C:\t\Ether.MSI`, "msiexec", []string{"/i", `C:\t\Ether.MSI`}},
		{"windows", `C:\t\EtherSetup.exe`, `C:\t\EtherSetup.exe`, nil},
		{"linux", "/tmp/ether.deb", "xdg-open", []string{"/tmp/ether.deb"}},
	} {
		name, args := installerCommand(tc.goos, tc.path)
		if name != tc.name || fmt.Sprint(args) != fmt.Sprint(tc.args) {
			t.Errorf("%s %s: %s %v", tc.goos, tc.path, name, args)
		}
	}
}

func TestPollNotifiesOnce(t *testing.T) {
	f := newFeed(t, nil, false)
	c := f.client(t, "1.0.0")
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int64
	done := make(chan struct{})
	go func() {
		c.Poll(ctx, 10*time.Millisecond, 0, func(context.Context, *Update) { count.Add(1) })
		close(done)
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	if count.Load() != 1 {
		t.Fatalf("notified %d times, want 1", count.Load())
	}
}

func TestManifestURLResolution(t *testing.T) {
	cases := map[string]string{
		"https://x/releases":         "https://x/releases/stable.json",
		"https://x/releases/":        "https://x/releases/stable.json",
		"https://x/stable.json":      "https://x/stable.json",
		"https://x/{channel}.json":   "https://x/stable.json",
		"https://x/feed?c={channel}": "https://x/feed?c=stable",
	}
	_ = cases
	for in, want := range cases {
		if got := manifestURL(in, "stable"); got != want {
			t.Errorf("manifestURL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestKeyRoundTrip(t *testing.T) {
	pub, priv, _ := GenerateKeyPair()
	gotPub, err := PublicKeyFromB64(EncodeKey(pub))
	if err != nil || !gotPub.Equal(pub) {
		t.Fatalf("pub roundtrip: %v", err)
	}
	gotPriv, err := PrivateKeyFromB64(EncodeKey(priv))
	if err != nil || !gotPriv.Equal(priv) {
		t.Fatalf("priv roundtrip: %v", err)
	}
	seed, err := PrivateKeyFromB64(EncodeKey(priv.Seed()))
	if err != nil || !seed.Equal(priv) {
		t.Fatalf("seed roundtrip: %v", err)
	}
}
