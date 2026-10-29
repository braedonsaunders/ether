//go:build scorecard && !plexparity

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/app"
)

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("data", "", "disposable data directory")
	itemsFile := fs.String("items", "", "reference item listing JSON")
	jf := fs.String("jellyfin", "127.0.0.1:18096", "Jellyfin listen address")
	emby := fs.String("emby", "127.0.0.1:18097", "Emby listen address")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *dir == "" || *itemsFile == "" {
		return fmt.Errorf("data and items are required")
	}
	if os.Getenv("SCORECARD_USER") == "" || os.Getenv("SCORECARD_PASSWORD") == "" {
		return fmt.Errorf("SCORECARD_USER and SCORECARD_PASSWORD are required")
	}
	data, e := os.ReadFile(*itemsFile)
	if e != nil {
		return e
	}
	var listing struct{ Items []app.ScorecardItem }
	if e = json.Unmarshal(data, &listing); e != nil {
		return e
	}
	if len(listing.Items) == 0 {
		return fmt.Errorf("empty reference catalogue")
	}
	if e = os.MkdirAll(*dir, 0700); e != nil {
		return e
	}
	imageServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/3/images/") && strings.HasSuffix(r.URL.Path, "/fixture.jpg") {
			img := image.NewRGBA(image.Rect(0, 0, 320, 180))
			for y := 0; y < 180; y++ {
				for x := 0; x < 320; x++ {
					img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
				}
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_ = jpeg.Encode(w, img, nil)
			return
		}
		if (strings.HasPrefix(r.URL.Path, "/3/movie/") || strings.HasPrefix(r.URL.Path, "/3/tv/")) && (strings.HasSuffix(r.URL.Path, "/recommendations") || strings.HasSuffix(r.URL.Path, "/similar")) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":1,"total_pages":1,"total_results":1,"results":[{"id":987654321,"title":"Future hidden title","name":"Future hidden title","release_date":"2099-01-01","first_air_date":"2099-01-01"}]}`))
			return
		}
		http.NotFound(w, r)
	})}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer l.Close()
	go imageServer.Serve(l)
	defer imageServer.Close()
	os.Setenv("ETHER_PARITY_TMDB_URL", "http://"+l.Addr().String()+"/3")
	os.Setenv("ETHER_PARITY_PLEXTV_URL", "http://"+l.Addr().String())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	handlers, closeApp, e := app.StartScorecard(ctx, *dir, os.Getenv("SCORECARD_USER"), os.Getenv("SCORECARD_PASSWORD"), listing.Items)
	if e != nil {
		return e
	}
	defer closeApp()
	servers := []*http.Server{{Addr: *jf, Handler: handlers["jellyfin"], ReadHeaderTimeout: 10 * time.Second}, {Addr: *emby, Handler: handlers["emby"], ReadHeaderTimeout: 10 * time.Second}}
	errc := make(chan error, 2)
	for _, s := range servers {
		ln, e := net.Listen("tcp", s.Addr)
		if e != nil {
			for _, v := range servers {
				v.Close()
			}
			return e
		}
		go func(s *http.Server, ln net.Listener) { errc <- s.Serve(ln) }(s, ln)
	}
	fmt.Fprintln(os.Stderr, "scorecard fixture ready:", filepath.Base(*dir))
	select {
	case <-ctx.Done():
	case e = <-errc:
		if e != http.ErrServerClosed {
			return e
		}
	}
	shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return nil
}
