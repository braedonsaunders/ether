package admin_test

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestLogsDownloadIsOwnerOnlyOrderedAndScrubbed(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const secret = "pl3xS3cretTok"
	write("ether.log.2", "oldest GET /library?X-Plex-Token="+secret+"\n")
	write("ether.log.1", `older err="Get \"https://u:`+secret+`@news.example\": EOF"`+"\n")
	write("ether.log", "newest Authorization: Bearer "+secret+"\nno newline at end")
	write("crash.log", "panic: boom\n")
	write("ether.db", "not a log "+secret)
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), LogDir: dir, Version: "9.9.9"})

	if rec := serve(h, req(http.MethodGet, "/ether/api/logs/download", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous download: %d", rec.Code)
	}
	r := req(http.MethodGet, "/ether/api/logs/download", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec := serve(h, r)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="ether-logs-`) {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if strings.Contains(body, secret) || strings.Contains(body, ownerToken) || strings.Contains(body, "not a log") {
		t.Fatalf("leaked:\n%s", body)
	}
	if !strings.HasPrefix(body, "# Ether 9.9.9 on ") {
		t.Fatalf("header:\n%s", body)
	}
	order := []string{"===== crash.log =====", "panic: boom", "===== ether.log.2 =====", "oldest", "===== ether.log.1 =====", "older", "===== ether.log =====", "newest", "no newline at end"}
	at := 0
	for _, want := range order {
		i := strings.Index(body[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order:\n%s", want, body)
		}
		at += i
	}
}

func TestLogsDownloadFallsBackToMemory(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	buf := admin.NewLogBuffer(10)
	log := slog.New(buf.Handler())
	log.Info("first")
	log.Warn("second", "url", "https://x.example/?apikey=abc123secret")
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Logs: buf, LogDir: t.TempDir()})
	r := req(http.MethodGet, "/ether/api/logs/download", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	body := serve(h, r).Body.String()
	if strings.Contains(body, "abc123secret") || strings.Index(body, "first") > strings.Index(body, "second") || !strings.Contains(body, "in memory") {
		t.Fatalf("%s", body)
	}
}
