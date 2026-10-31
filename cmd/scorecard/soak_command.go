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
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/app"
	"github.com/braedonsaunders/ether/internal/hoststat"
)

func soak(args []string) error {
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	profile := fs.String("profile", "shakedown", "shakedown or sustained")
	data := fs.String("data", "", "fresh data directory")
	manifestPath := fs.String("manifest", "", "verified scorecard-files.json")
	out := fs.String("out", "", "report JSON path")
	buildID := fs.String("build", "", "source revision")
	user := fs.String("user", "soak-viewer", "disposable viewer")
	password := fs.String("password", "soak-viewer", "disposable viewer password")
	diagnose := fs.Bool("diagnose", false, "play one of each kind and print the result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" || *manifestPath == "" || *out == "" {
		return fmt.Errorf("data, manifest and out are required")
	}
	timing, err := profileTiming(*profile)
	if err != nil && !*diagnose {
		return err
	}
	if *diagnose && err != nil {
		timing = soakTiming{Sample: time.Second}
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	var listed []catalogFile
	if err := json.Unmarshal(raw, &listed); err != nil {
		return err
	}
	itemsJSON, err := synthesizeCatalogue(listed)
	if err != nil {
		return err
	}
	var items []app.ScorecardItem
	if err := json.Unmarshal(itemsJSON, &items); err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*data, "scorecard-files.json"), raw, 0600); err != nil {
		return err
	}
	stubAddr, stub, err := soakImageStub()
	if err != nil {
		return err
	}
	defer stub.Close()
	os.Setenv("ETHER_PARITY_TMDB_URL", "http://"+stubAddr+"/3")
	os.Setenv("ETHER_PARITY_PLEXTV_URL", "http://"+stubAddr)
	os.Setenv("SCORECARD_MOUNT_PLEX", "1")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handlers, closeApp, err := app.StartScorecard(ctx, *data, *user, *password, items)
	if err != nil {
		return err
	}
	defer closeApp()
	plexURL, plexSrv, err := serveLoopback(handlers["plex"])
	if err != nil {
		return err
	}
	defer plexSrv.Close()
	jellyfinURL, jellyfinSrv, err := serveLoopback(handlers["jellyfin"])
	if err != nil {
		return err
	}
	defer jellyfinSrv.Close()
	embyURL, embySrv, err := serveLoopback(handlers["emby"])
	if err != nil {
		return err
	}
	defer embySrv.Close()
	fmt.Fprintf(os.Stderr, "soak plex %s jellyfin %s emby %s\n", plexURL, jellyfinURL, embyURL)
	manifest, runErr := RunSoak(ctx, SoakConfig{
		Profile: *profile, Build: *buildID, Plex: plexURL, Jellyfin: jellyfinURL, Emby: embyURL,
		User: *user, Password: *password, PlexToken: "fixture-owner-token", Timing: timing, GPU: hoststat.GPU("/dev"),
		Diagnose: *diagnose,
	})
	if *diagnose {
		return runErr
	}
	report := EvaluateSoak(manifest)
	if err := writeSoak(*out, manifest, report); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if report.Status != "pass" {
		return fmt.Errorf("soak %s", report.Status)
	}
	return nil
}

func writeSoak(path string, manifest SoakManifest, report SoakReport) error {
	if err := writeJSON(path, report); err != nil {
		return err
	}
	return writeJSON(path+".manifest.json", manifest)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func serveLoopback(handler http.Handler) (string, *http.Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return "http://" + ln.Addr().String(), srv, nil
}

func soakImageStub() (string, *http.Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		img := image.NewRGBA(image.Rect(0, 0, 16, 16))
		img.Set(0, 0, color.RGBA{20, 40, 60, 255})
		w.Header().Set("Content-Type", "image/jpeg")
		_ = jpeg.Encode(w, img, nil)
	})}
	go srv.Serve(ln)
	return ln.Addr().String(), srv, nil
}
