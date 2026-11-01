package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

// PerfTraceEnv names a file that receives one JSON line per HTTP request
// (surface, path, status, duration and its database work). cmd/etherperf
// sets it; production leaves it unset.
const PerfTraceEnv = "ETHER_PERF_TRACE"

// PerfTraceDepthEnv sets how many callers each traced statement's site
// records (default 1).
const PerfTraceDepthEnv = "ETHER_PERF_TRACE_DEPTH"

// PerfTagHeader is copied into the trace record of the request carrying it.
const PerfTagHeader = "X-Ether-Perf-Tag"

// Request budgets. A request over either database bound, or a non-streaming
// request over the wall-clock bound, is logged with its most expensive
// statements so a hot path that has started to scale with the library is
// visible in the owner's logs, not only on a device that hangs.
const (
	perfSlowRequest   = 2 * time.Second
	perfSlowDBTime    = 500 * time.Millisecond
	perfChattyQueries = 300
	perfWarnInterval  = time.Minute
	// requestProviderWait is the most one request waits on TMDB in all.
	requestProviderWait = 1500 * time.Millisecond
)

// perfObserver gives every request a store.QueryStats.
type perfObserver struct {
	log *slog.Logger

	mu       sync.Mutex
	lastWarn map[string]time.Time
	trace    *os.File
	encoder  *json.Encoder
}

func newPerfObserver(log *slog.Logger) *perfObserver {
	p := &perfObserver{log: log, lastWarn: make(map[string]time.Time)}
	if path := strings.TrimSpace(os.Getenv(PerfTraceEnv)); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Warn("performance trace unavailable", "err", err)
		} else {
			p.trace, p.encoder = f, json.NewEncoder(f)
			store.CaptureCallSites(true)
			if depth, err := strconv.Atoi(os.Getenv(PerfTraceDepthEnv)); err == nil {
				store.SetCallSiteDepth(depth)
			}
		}
	}
	return p
}

func (p *perfObserver) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trace != nil {
		_ = p.trace.Close()
		p.trace, p.encoder = nil, nil
	}
}

// PerfTraceRecord is one line of the ETHER_PERF_TRACE file.
type PerfTraceRecord struct {
	// Tag echoes the X-Ether-Perf-Tag request header, so the harness can
	// join a trace line to the step that sent it.
	Tag        string                `json:"tag,omitempty"`
	Surface    string                `json:"surface"`
	Method     string                `json:"method"`
	Path       string                `json:"path"`
	Status     int                   `json:"status"`
	Bytes      int                   `json:"bytes"`
	Start      time.Time             `json:"start"`
	Duration   time.Duration         `json:"duration"`
	Queries    int64                 `json:"queries"`
	Writes     int64                 `json:"writes"`
	Rows       int64                 `json:"rows"`
	DBTime     time.Duration         `json:"dbTime"`
	LockWait   time.Duration         `json:"lockWait"`
	Statements []store.StatementStat `json:"statements,omitempty"`
}

// wrap measures next as surface. Without a trace file the response writer
// is passed through untouched.
func (p *perfObserver) wrap(surface string, next http.Handler) http.Handler {
	if p == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, stats := store.WithQueryStats(r.Context())
		ctx = catalog.WithReadWaitBudget(ctx, requestProviderWait)
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			ctx = reqmemo.With(ctx)
		}
		start := time.Now()
		var recorder middleware.WrapResponseWriter
		p.mu.Lock()
		tracing := p.encoder != nil
		p.mu.Unlock()
		if tracing {
			recorder = middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			w = recorder
		}
		next.ServeHTTP(w, r.WithContext(ctx))
		elapsed := time.Since(start)
		if tracing {
			p.writeTrace(PerfTraceRecord{
				Tag: r.Header.Get(PerfTagHeader), Surface: surface, Method: r.Method, Path: r.URL.Path, Status: recorder.Status(), Bytes: recorder.BytesWritten(),
				Start: start, Duration: elapsed, Queries: stats.Queries(), Writes: stats.Writes(), Rows: stats.Rows(), DBTime: stats.DBTime(), LockWait: stats.LockWait(),
				Statements: stats.Statements(),
			})
		}
		p.budget(surface, r, w.Header().Get("Content-Type"), elapsed, stats)
	})
}

func (p *perfObserver) writeTrace(rec PerfTraceRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.encoder != nil {
		_ = p.encoder.Encode(rec)
	}
}

// budget logs a request over budget, at most once a minute per route shape.
func (p *perfObserver) budget(surface string, r *http.Request, contentType string, elapsed time.Duration, stats *store.QueryStats) {
	slowDB := stats.DBTime() > perfSlowDBTime || stats.Queries() > perfChattyQueries
	slowWall := elapsed > perfSlowRequest && !streamingResponse(contentType, r)
	if !slowDB && !slowWall {
		return
	}
	shape := surface + " " + r.Method + " " + routeShape(r.URL.Path)
	now := time.Now()
	p.mu.Lock()
	if last, ok := p.lastWarn[shape]; ok && now.Sub(last) < perfWarnInterval {
		p.mu.Unlock()
		return
	}
	if len(p.lastWarn) > 1024 {
		clear(p.lastWarn)
	}
	p.lastWarn[shape] = now
	p.mu.Unlock()
	attrs := []any{"surface", surface, "method", r.Method, "route", routeShape(r.URL.Path), "duration", elapsed.Round(time.Millisecond),
		"queries", stats.Queries(), "writes", stats.Writes(), "rows", stats.Rows(), "db_time", stats.DBTime().Round(time.Millisecond), "lock_wait", stats.LockWait().Round(time.Millisecond)}
	for i, st := range stats.Statements() {
		if i == 3 {
			break
		}
		attrs = append(attrs, "top"+string(rune('1'+i)), summarizeStatement(st))
	}
	p.log.Warn("request over performance budget", attrs...)
}

// streamingResponse reports media, event and socket responses whose
// duration is the client's viewing time, not server work.
func streamingResponse(contentType string, r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	ct := strings.ToLower(contentType)
	for _, prefix := range []string{"video/", "audio/", "image/", "text/event-stream", "application/octet-stream", "application/vnd.apple.mpegurl", "application/x-mpegurl", "application/dash+xml"} {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

// routeShape replaces identifiers and opaque tokens in a path with
// placeholders, so a log line never carries a credential and repeated
// requests for different items share one shape.
func routeShape(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		if opaqueSegment(part) {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

func opaqueSegment(s string) bool {
	digits, letters := 0, 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		}
	}
	if digits > 0 && letters == 0 {
		return true
	}
	return len(s) >= 16 && digits > 0
}

func summarizeStatement(st store.StatementStat) string {
	sql := strings.Join(strings.Fields(st.SQL), " ")
	if len(sql) > 160 {
		sql = sql[:160] + "…"
	}
	return sql + " ×" + strconv.FormatInt(st.Count, 10) + " rows=" + strconv.FormatInt(st.Rows, 10) + " " + st.Time.Round(time.Millisecond).String()
}
