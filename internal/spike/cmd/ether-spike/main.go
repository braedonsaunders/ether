// Command ether-spike serves local video files through Ether's Plex front
// end, for testing official Plex apps (and pmsdiff) before the Usenet
// engine is wired in.
//
//	ether-spike -addr :32400 -file ~/Movies/Big.Buck.Bunny.2008.mp4 -accept-any-token
//
// It reads config.json from the data directory (ETHER_DATA_DIR or the
// platform default) for the machine identifier, friendly name and owner
// token. It does not sign in to plex.tv or publish connections.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/spike"
)

type fileList []string

func (f *fileList) String() string     { return strings.Join(*f, ",") }
func (f *fileList) Set(v string) error { *f = append(*f, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ether-spike:", err)
		os.Exit(1)
	}
}

func run() error {
	var files fileList
	dataDir := flag.String("data", config.DefaultDataDir(), "data directory holding config.json")
	addr := flag.String("addr", "", "listen address (default from config, :32400)")
	anyToken := flag.Bool("accept-any-token", false, "treat every non-empty X-Plex-Token as the owner (LAN testing only)")
	token := flag.String("token", "", "extra token accepted as the owner")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Var(&files, "file", "local video file (repeatable); default config spike.localFiles")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	store, err := config.Open(*dataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := spike.Options{Files: files, AcceptAnyToken: *anyToken, Logger: log}
	if *token != "" {
		opts.Tokens = []string{*token}
	}
	lib, err := spike.New(ctx, store.Get, opts)
	if err != nil {
		return err
	}
	defer lib.Close()
	if *anyToken {
		log.Warn("accepting any token as the owner: do not expose this server")
	}
	srv, closeBroker, err := lib.OpenPlex()
	if err != nil {
		return err
	}
	defer closeBroker()
	defer srv.Close()

	listen := *addr
	if listen == "" {
		c := store.Get()
		port := c.Server.Port
		if port == 0 {
			port = 32400
		}
		listen = fmt.Sprintf("%s:%d", c.Server.BindAddress, port)
	}
	hs := &http.Server{Addr: listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("ether spike listening", "addr", listen)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}
