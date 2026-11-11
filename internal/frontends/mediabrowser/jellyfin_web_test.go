package mediabrowser

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/webclient"
	"github.com/ulikunitz/xz"
)

// minimalDeb builds a .deb whose data.tar.xz carries the given web files plus
// a copyright notice.
func minimalDeb(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		write("./usr/share/jellyfin/web/"+name, []byte(body))
	}
	write("./usr/share/doc/jellyfin-web/copyright", []byte("GPL-2.0"))
	tw.Close()

	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		t.Fatal(err)
	}
	xw.Write(tarBuf.Bytes())
	xw.Close()

	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&ar, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(body))
		ar.Write(body)
		if len(body)%2 == 1 {
			ar.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("data.tar.xz", xzBuf.Bytes())
	return ar.Bytes()
}

// installedWebClient returns an installer already populated from an httptest
// server serving a synthetic deb.
func installedWebClient(t *testing.T) *webclient.Installer {
	t.Helper()
	deb := minimalDeb(t, map[string]string{
		"index.html":     "<html>jellyfin-web</html>",
		"main.abc123.js": "console.log('x')",
		"config.json":    "{}",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(deb)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(deb)
	in := webclient.New(t.TempDir(), srv.Client())
	in.URL = srv.URL
	in.SHA256 = hex.EncodeToString(sum[:])
	in.Size = int64(len(deb))
	if _, err := in.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	return in
}

func jellyfinRouter(t *testing.T, client *webclient.Installer) http.Handler {
	t.Helper()
	r := &route.Router{}
	info := PublicInfo{ID: "123", ServerName: "Ether", Version: "12.4.0", ProductName: "Jellyfin Server"}
	if err := publicRoutes(r, func() PublicInfo { return info }, nil, nil, client); err != nil {
		t.Fatal(err)
	}
	return StripPrefix("/jellyfin", r)
}

func TestJellyfinWebServesInstalledClient(t *testing.T) {
	h := jellyfinRouter(t, installedWebClient(t))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "jellyfin-web") {
		t.Fatalf("index: %d %q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control %q", cc)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("index content-type %q", ct)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web/main.abc123.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("bundle: %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=604800" {
		t.Fatalf("bundle cache-control %q", cc)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web/config.json", nil))
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("config cache-control %q", cc)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web/missing.js", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
}

func TestJellyfinWebRedirectsAndRoot(t *testing.T) {
	h := jellyfinRouter(t, installedWebClient(t))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/jellyfin/web/" {
		t.Fatalf("/web: %d %q", w.Code, w.Header().Get("Location"))
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/jellyfin/web/" {
		t.Fatalf("/: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestJellyfinWebPreparingPageWhileDownloading(t *testing.T) {
	// An installer pointing at a dir that will never be populated: Begin keeps
	// reporting downloading because the download is in flight (blocked server).
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	in := webclient.New(t.TempDir(), srv.Client())
	in.URL, in.SHA256, in.Size = srv.URL, strings.Repeat("0", 64), 1

	h := jellyfinRouter(t, in)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/jellyfin/web/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Preparing the Jellyfin web client") {
		t.Fatalf("preparing: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Refresh") != "3" {
		t.Fatalf("refresh header %q", w.Header().Get("Refresh"))
	}
}
