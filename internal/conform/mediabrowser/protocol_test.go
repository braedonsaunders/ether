package mediabrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/conform"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadFetchAlignAndRedact(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"jellyfin","client":"tv","headers":{"Accept":"application/json"},"flows":{"home":[{"name":"items","method":"GET","path":"/Items"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "tv.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("jellyfin", dir)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), `Token="secret"`) {
				t.Error("missing auth")
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": id, "ProviderIds": map[string]string{"Tmdb": "42"}}}, "AccessToken": "secret", "ServerId": "server", "Path": "/private/file"})
		}))
	}
	ref := serve("reference")
	defer ref.Close()
	target := serve("target")
	defer target.Close()
	ids := p.Discover(context.Background(), ref.Client(), ref.URL, "secret", "tv", nil)
	aligned := p.Discover(context.Background(), target.Client(), target.URL, "secret", "tv", &ids)
	if aligned.Values["id:reference"] != "target" {
		t.Fatalf("alignment: %#v", aligned)
	}
	corpus := p.Corpus("tv")
	if len(corpus) != 1 {
		t.Fatal(corpus)
	}
	resp, err := p.Fetch(context.Background(), ref.Client(), ref.URL, "secret", corpus[0], "tv", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	redacted := string(p.Redact(resp).Body)
	for _, secret := range []string{"secret", "/private/file", "server"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("not redacted: %s", redacted)
		}
	}
}

func TestShippedProfilesLoad(t *testing.T) {
	for _, name := range []string{"jellyfin", "emby", "silo"} {
		t.Run(name, func(t *testing.T) {
			p, err := Load(name, filepath.Join("..", "profiles", name))
			if err != nil {
				t.Fatal(err)
			}
			for id := range p.Clients() {
				if len(p.Corpus(id)) == 0 {
					t.Errorf("empty corpus for %s", id)
				}
			}
		})
	}
}
func TestAccessTokenHeaderTemplate(t *testing.T) {
	dir := t.TempDir()
	body := `{"protocol":"emby","client":"Emby web","headers":{"X-Emby-Token":"${access_token}"},"flows":{"connect":[{"name":"ping","method":"GET","path":"/ping"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "secret" {
			t.Error("token not substituted")
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	if _, err = p.Fetch(context.Background(), server.Client(), server.URL, "secret", p.Corpus("web")[0], "web", nil, true); err != nil {
		t.Fatal(err)
	}
}
func TestBinaryRangeFixturesDoNotContainMedia(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"emby","client":"test","flows":{"play_direct":[{"name":"stream","method":"GET","path":"/video/stream"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(ch byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte{ch}, 200000)))
		}))
	}
	reference := serve('A')
	defer reference.Close()
	target := serve('B')
	defer target.Close()
	corpus := p.Corpus("web")
	if len(corpus) != 3 {
		t.Fatalf("ranges: %d", len(corpus))
	}
	for _, req := range corpus {
		a, err := p.Fetch(context.Background(), reference.Client(), reference.URL, "", req, "web", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.Fetch(context.Background(), target.Client(), target.URL, "", req, "web", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind != "binary-range" || len(a.Body) != 0 || a.SHA256 == "" {
			t.Fatalf("unsafe fixture: %+v", a)
		}
		if got := p.Compare(a, b); got.Blocking() != 0 {
			t.Fatalf("range compare: %+v", got)
		}
		if a.SHA256 == b.SHA256 {
			t.Fatal("different media have same hash")
		}
		if err := conform.SaveFixture(dir, req.Name, a); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(filepath.Join(dir, req.Name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(saved, []byte("QUFBQUFB")) {
			t.Fatal("fixture contains media data")
		}
	}
}
