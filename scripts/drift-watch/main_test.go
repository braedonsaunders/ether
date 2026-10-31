package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLabComposeKeepsTheFFmpegSeed(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/lab/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(raw)
	images, err := serviceImages(compose)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(images["jellyfin"], "jellyfin/jellyfin:") || !strings.Contains(images["emby"], "emby/embyserver:") || !strings.Contains(images["silo"], "ghcr.io/silo-server/silo-server:aio@") {
		t.Fatalf("pins: %#v", images)
	}
	if imageLineCount(compose, images["jellyfin"]) != 2 {
		t.Fatalf("expected the seed and jellyfin service to share %s", images["jellyfin"])
	}
	next, err := replaceServiceImage(compose, "jellyfin", "jellyfin/jellyfin:99.0@sha256:"+strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if imageLineCount(next, images["jellyfin"]) != 1 || imageLineCount(next, "jellyfin/jellyfin:99.0@sha256:"+strings.Repeat("e", 64)) != 1 {
		t.Fatal("jellyfin pin replace touched the ffmpeg seed or missed the service")
	}
	if !strings.Contains(next, images["emby"]) || !strings.Contains(next, images["silo"]) {
		t.Fatal("other pins changed")
	}
}

func TestPlanRewritesOnlyReferenceServers(t *testing.T) {
	const old = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	compose := "" +
		"services:\n" +
		"  silo-ffmpeg-seed:\n" +
		"    image: jellyfin/jellyfin:12.4@" + old + "\n" +
		"  jellyfin:\n" +
		"    image: jellyfin/jellyfin:12.4@" + old + "\n" +
		"  emby:\n" +
		"    image: emby/embyserver:4.10.2.0@" + old + "\n" +
		"  silo:\n" +
		"    image: ghcr.io/silo-server/silo-server:aio@" + old + "\n"
	srv := fakeRegistries(t)
	defer srv.Close()
	r := resolver{client: srv.Client(), githubAPI: srv.URL, dockerAuth: srv.URL, dockerRegistry: srv.URL, ghcr: srv.URL}
	updated, changes, err := r.plan(context.Background(), compose)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(updated, "silo-ffmpeg-seed:\n    image: jellyfin/jellyfin:12.4@"+old) != 1 {
		t.Fatalf("seed changed:\n%s", updated)
	}
	byService := map[string]imageChange{}
	for _, change := range changes {
		byService[change.Service] = change
		if !change.Changed {
			t.Fatalf("expected bump: %+v", change)
		}
	}
	if !strings.Contains(byService["jellyfin"].To, "jellyfin/jellyfin:12.5@sha256:"+strings.Repeat("1", 64)) {
		t.Fatalf("jellyfin: %+v", byService["jellyfin"])
	}
	if !strings.Contains(byService["emby"].To, "emby/embyserver:4.11.1.0@sha256:"+strings.Repeat("2", 64)) {
		t.Fatalf("emby: %+v", byService["emby"])
	}
	if !strings.Contains(byService["silo"].To, "ghcr.io/silo-server/silo-server:aio@sha256:"+strings.Repeat("3", 64)) {
		t.Fatalf("silo: %+v", byService["silo"])
	}
}

func TestStableTagRejectsPrereleases(t *testing.T) {
	if _, err := stableTag("v12.5-rc1"); err == nil {
		t.Fatal("accepted a release candidate")
	}
	got, err := stableTag("v4.10.2.0")
	if err != nil || got != "4.10.2.0" {
		t.Fatalf("tag: %s %v", got, err)
	}
}

func TestBlockingReportsAndPullRequestBody(t *testing.T) {
	dir := t.TempDir()
	raw := `{"differences":[{"client":"swiftfin","endpoint":"/Items","path":"$fixture","kind":"value","blocking":true,"reason":"recorded response changed"},{"endpoint":"/Skip","path":"/x","kind":"value","blocking":false}]}`
	summary := `{"endpoints":[{"endpoint":"/Videos","missing":[{"format":"json","path":"/Id","kind":"missing","clients":["androidtv"]}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "jellyfin-recording.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "emby-compare.json"), []byte(summary), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := loadBlockingDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Endpoint != "/Videos" || rows[0].Client != "androidtv" || rows[1].Endpoint != "/Items" || rows[1].Reason != "recorded response changed" {
		t.Fatalf("rows: %+v", rows)
	}
	doc := reportDoc{
		APIDriftMarkdown: "# API endpoint drift\n\n| Priority | Protocol | Change | Method | Endpoint |\n| --- | --- | --- | --- | --- |\n| used-by-clients | jellyfin | changed | GET | `/Items` |\n| other | jellyfin | added | POST | `/Admin` |\n",
		Images:           []imageChange{{Service: "silo", From: "old", To: "ghcr.io/silo-server/silo-server:aio@sha256:" + strings.Repeat("3", 64), Changed: true}},
		Recorded:         true,
		RecordNote:       recordNote(true),
		Blocking:         rows,
	}
	if !shouldOpen(doc) {
		t.Fatal("expected a pull request")
	}
	body := renderPRBody(doc, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"Drift watch 2026-10-30", "used-by-clients", "`/Items`", "`/Videos`", "androidtv", "bump from `old`", "drift-watch-out/", "DRIFT_WATCH_APPLY=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "recorded response changed") && strings.Contains(body, "sha256:"+strings.Repeat("a", 64)) {
		t.Fatal("body echoed a fixture hash")
	}
	empty := reportDoc{RecordNote: recordNote(false)}
	if shouldOpen(empty) {
		t.Fatal("empty report would open a pull request")
	}
	if !shouldOpen(reportDoc{RecordError: "record failed: jellyfin"}) {
		t.Fatal("record failure should be reviewed")
	}
}

func fakeRegistries(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "ether-drift-watch" {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		switch {
		case r.URL.Path == "/repos/jellyfin/jellyfin/releases/latest":
			w.Write([]byte(`{"tag_name":"v12.5"}`))
		case r.URL.Path == "/repos/MediaBrowser/Emby.Releases/releases/latest":
			w.Write([]byte(`{"tag_name":"4.11.1.0"}`))
		case r.URL.Path == "/token":
			if !strings.Contains(r.URL.Query().Get("scope"), "pull") {
				t.Errorf("scope %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"token":"test-token"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/") && strings.HasSuffix(r.URL.Path, "/manifests/12.5"):
			requireBearer(t, r)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("1", 64))
		case strings.HasPrefix(r.URL.Path, "/v2/") && strings.Contains(r.URL.Path, "embyserver"):
			requireBearer(t, r)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("2", 64))
		case strings.HasPrefix(r.URL.Path, "/v2/") && strings.Contains(r.URL.Path, "silo-server"):
			requireBearer(t, r)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("3", 64))
		default:
			http.NotFound(w, r)
		}
	}))
}

func imageLineCount(compose, ref string) int {
	n := 0
	for _, line := range strings.Split(compose, "\n") {
		if strings.TrimSpace(line) == "image: "+ref {
			n++
		}
	}
	return n
}

func requireBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("authorization %q", r.Header.Get("Authorization"))
	}
	if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
		t.Errorf("accept %q", r.Header.Get("Accept"))
	}
}
