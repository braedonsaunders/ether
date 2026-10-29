package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/capture"
)

func proxyCommand(args []string) error {
	f := flag.NewFlagSet("proxy", flag.ContinueOnError)
	protocol := f.String("protocol", "", "protocol")
	listen := f.String("listen", "", "listen address")
	upstream := f.String("upstream", "", "reference URL")
	out := f.String("out", "", "capture directory")
	client := f.String("client", "", "client name")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *upstream == "" || *out == "" {
		return fmt.Errorf("proxy requires -listen, -upstream and -out")
	}
	u, err := url.Parse(*upstream)
	if err != nil {
		return err
	}
	rec, err := capture.New(*protocol, *client, *out, u)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: rec, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "Capturing %s client %s on %s\n", *protocol, *client, *listen)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err = <-rec.Errors():
		_ = server.Close()
		<-done
		return err
	case err = <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		err = <-done
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
func profileFromCapture(args []string) error {
	f := flag.NewFlagSet("profile-from-capture", flag.ContinueOnError)
	protocol := f.String("protocol", "", "protocol")
	client := f.String("client", "", "client name")
	dir := f.String("capture", "", "capture directory")
	out := f.String("out", "", "output profile JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("profile-from-capture requires -capture")
	}
	if *out == "" {
		*out = filepath.Join("internal", "conform", "profiles", *protocol, *client+".json")
	}
	return capture.ProfileFromCapture(*dir, *protocol, *client, *out)
}
