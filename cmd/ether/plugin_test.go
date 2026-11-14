package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/kit"
	"github.com/braedonsaunders/ether/internal/store"
)

type stubRuntime struct{}

func (s *stubRuntime) Call(ctx context.Context, req plugins.Request) (plugins.Response, error) {
	if req.Path == "/ether/health" {
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"status":"ok"}`)}, nil
	}
	return plugins.Response{Status: 200, Body: json.RawMessage(`{}`)}, nil
}

func (s *stubRuntime) Close() error { return nil }

func newTestPluginManager(t *testing.T) *plugins.Manager {
	t.Helper()
	m, err := plugins.New(plugins.Deps{
		DB:           store.OpenTest(t),
		CheckPackage: kit.CheckInstall,
		Registry:     extend.New(),
		Tools:        tools.NewCatalogue(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		IsLeader:     func() bool { return true },
	})
	if err != nil {
		t.Fatalf("plugins.New: %v", err)
	}
	m.RegisterRuntime("hosted", func(inst plugins.Installed) (plugins.Runtime, error) {
		return &stubRuntime{}, nil
	})
	m.RegisterRuntime("remote", func(inst plugins.Installed) (plugins.Runtime, error) {
		return &stubRuntime{}, nil
	})
	return m
}

func TestPluginCLINewCheckTestPackInstallRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	plugDir := filepath.Join(tmp, "x")
	outPkg := filepath.Join(tmp, "x.etherpkg")

	var out, errBuf bytes.Buffer

	// 1. Scaffold a catalogue with real sample data using the positional recipe.
	if code := runPlugin([]string{"new", "catalogue", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin new exited %d: stderr=%s", code, errBuf.String())
	}

	// 2. ether plugin check x
	out.Reset()
	errBuf.Reset()
	if code := runPlugin([]string{"check", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin check exited %d: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "PASS:") {
		t.Fatalf("expected PASS in check output, got: %s", out.String())
	}

	// 3. ether plugin check --json x
	out.Reset()
	errBuf.Reset()
	if code := runPlugin([]string{"check", "--json", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin check --json exited %d: stderr=%s", code, errBuf.String())
	}
	var rep kit.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal check JSON: %v (raw=%s)", err, out.String())
	}
	if !rep.OK || rep.Kind != "integration" {
		t.Fatalf("unexpected JSON report: %+v", rep)
	}

	// 4. ether plugin test x
	out.Reset()
	errBuf.Reset()
	t.Setenv("ETHER_BIN", "")
	if code := runPlugin([]string{"test", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin test exited %d: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// 5. ether plugin pack -o x.etherpkg x
	out.Reset()
	errBuf.Reset()
	if code := runPlugin([]string{"pack", "-o", outPkg, plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin pack exited %d: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	pkgBytes, err := os.ReadFile(outPkg)
	if err != nil {
		t.Fatalf("read packed .etherpkg: %v", err)
	}

	// 6. Verify check works directly on the packed .etherpkg file.
	out.Reset()
	errBuf.Reset()
	if code := runPlugin([]string{"check", outPkg}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin check on .etherpkg exited %d: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}

	// 7. Verify the packed .etherpkg is installable by plugins.Manager.
	mgr := newTestPluginManager(t)
	preview, err := mgr.PreviewInstall(context.Background(), plugins.Source{Kind: "pkg", Pkg: pkgBytes})
	if err != nil {
		t.Fatalf("PreviewInstall(.etherpkg): %v", err)
	}
	rec, err := mgr.Install(context.Background(), preview, plugins.InstallOptions{})
	if err != nil {
		t.Fatalf("Install(.etherpkg): %v", err)
	}
	if rec.ID != rep.PluginID {
		t.Fatalf("installed ID = %q, want %q", rec.ID, rep.PluginID)
	}

	// 8. ether plugin publish x
	out.Reset()
	errBuf.Reset()
	if code := runPlugin([]string{"publish", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin publish exited %d: stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "Package digest:") || !strings.Contains(out.String(), "catalogue") {
		t.Fatalf("unexpected publish output: %s", out.String())
	}
}

func TestPluginCLICheckFailsOnInvalidPlugin(t *testing.T) {
	tmp := t.TempDir()
	plugDir := filepath.Join(tmp, "bad")
	var out, errBuf bytes.Buffer
	if code := runPlugin([]string{"new", "--kind", "source", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin new: %v", errBuf.String())
	}
	// Inject forbidden copy ("simply" + "!") into i18n/en.json.
	enPath := filepath.Join(plugDir, "i18n", "en.json")
	if err := os.WriteFile(enPath, []byte(`{"summary": "Simply great source!"}`), 0o644); err != nil {
		t.Fatalf("write en.json: %v", err)
	}

	out.Reset()
	errBuf.Reset()
	code := runPlugin([]string{"check", plugDir}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("expected non-zero exit code for invalid copy, got 0:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL:") {
		t.Fatalf("expected FAIL in output, got:\n%s", out.String())
	}

	pkg, err := kit.LoadDir(plugDir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := kit.Pack(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestPluginManager(t).PreviewInstall(context.Background(), plugins.Source{Kind: "pkg", Pkg: raw}); err == nil {
		t.Fatal("install preview accepted copy rejected by the kit")
	}
}

func TestPluginCLIDevHotReloadAndLogs(t *testing.T) {
	tmp := t.TempDir()
	plugDir := filepath.Join(tmp, "devplug")
	var out, errBuf bytes.Buffer
	if code := runPlugin([]string{"new", "--kind", "extension", plugDir}, &out, &errBuf); code != 0 {
		t.Fatalf("plugin new: %v", errBuf.String())
	}

	mgr := newTestPluginManager(t)
	mux := http.NewServeMux()
	mux.Handle("/ether/api/plugins/", http.StripPrefix("/ether/api/plugins", mgr.Handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out.Reset()
	errBuf.Reset()
	t.Setenv("ETHER_TOKEN", "dev-token")
	code := runPlugin([]string{"dev", "--server", srv.URL, "--once", plugDir}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("plugin dev --once exited %d: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "Reloaded community.example.devplug") {
		t.Fatalf("expected Reloaded message in output, got: %s", out.String())
	}

	// The output events drive the test; the deadline is only a hang guard.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out.Reset()
	errBuf.Reset()
	writer := &reloadTestWriter{buffer: &out, onFirst: func() {
		if err := os.WriteFile(filepath.Join(plugDir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
			t.Error(err)
			cancel()
		}
	}, onSecond: cancel}
	code = runPluginWithContext(ctx, []string{"dev", "--server", srv.URL, "--interval", "40ms", plugDir}, writer, &errBuf)
	if code != 0 || writer.count < 2 {
		t.Fatalf("plugin dev watch: code=%d reloads=%d stderr=%s", code, writer.count, errBuf.String())
	}
}

type reloadTestWriter struct {
	buffer            *bytes.Buffer
	count             int
	onFirst, onSecond func()
}

func (w *reloadTestWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if strings.Contains(string(p), "Reloaded ") {
		w.count++
		if w.count == 1 {
			w.onFirst()
		} else if w.count == 2 {
			w.onSecond()
		}
	}
	return n, err
}
