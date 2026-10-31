package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"
)

// PprofAddrEnv enables Go's profiler on a loopback address, for profiling
// a hot path on real data (cmd/etherperf sets it). Non-loopback addresses
// are refused: profiles expose memory contents.
const PprofAddrEnv = "ETHER_PPROF_ADDR"

func servePprof(ctx context.Context, log *slog.Logger) {
	addr := strings.TrimSpace(os.Getenv(PprofAddrEnv))
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Warn("profiler not started: address must be loopback", "addr", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		log.Info("profiler listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("profiler stopped", "err", err)
		}
	}()
}
