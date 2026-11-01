package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

func TestDefectRecheck(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"emby","client":"TV","flows":{"play":[{"name":"stream","method":"GET","path":"/video/stream"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "tv.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := mediabrowser.Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Items":[]}`)
			return
		}
		switch r.Header.Get("Range") {
		case "bytes=0-0":
			w.Header().Set("Content-Range", "bytes 0-0/200000")
			w.WriteHeader(206)
			fmt.Fprint(w, "x")
		case "bytes=-65536":
			w.Header().Set("Content-Range", "bytes 0-65536/200000")
			w.Header().Set("Content-Length", "65537")
			w.WriteHeader(206)
			fmt.Fprint(w, strings.Repeat("x", 65537))
		default:
			http.ServeContent(w, r, "media.mp4", time.Time{}, strings.NewReader(strings.Repeat("x", 200000)))
		}
	}))
	defer srv.Close()
	if err := checkDefects(p, nil, srv.URL, "", "tv", 3*time.Second, 1); err != nil {
		t.Fatal(err)
	}
}
