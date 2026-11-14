package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fastschema/qjs"
)

// faultPrefix marks a classified fault inside a JS exception message, the
// same trick appkit-sandbox uses to carry faults across the boundary.
const faultPrefix = "__ETHER_FAULT__"

func encodeFault(f *Fault) string {
	b, _ := json.Marshal(map[string]string{"code": f.Code, "message": f.Message})
	return faultPrefix + string(b)
}

func decodeFault(msg string) *Fault {
	at := strings.Index(msg, faultPrefix)
	if at < 0 {
		return nil
	}
	var f Fault
	// The payload may be followed by a JS stack trace; decode one value.
	dec := json.NewDecoder(strings.NewReader(msg[at+len(faultPrefix):]))
	var raw struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if dec.Decode(&raw) != nil || raw.Code == "" {
		return nil
	}
	f.Code, f.Message = raw.Code, raw.Message
	return &f
}

var qjsOnce sync.Once

// ensureQJS creates the first QuickJS runtime in the process with
// CloseOnContextDone enabled. fastschema/qjs caches its wazero runtime
// config package-globally from the first New call, and context-driven
// module close is the only reliable way to interrupt a tight JS loop
// (its MaxExecutionTime does not interrupt one), so the first runtime
// must be ours.
func ensureQJS() {
	qjsOnce.Do(func() {
		defer func() { _ = recover() }()
		if rt, err := qjs.New(qjs.Option{Context: context.Background(), CloseOnContextDone: true,
			Stdout: io.Discard, Stderr: io.Discard}); err == nil {
			defer func() { _ = recover() }()
			rt.Close()
		}
	})
}

// servicePrelude builds the ether global for request-ABI instances. Host
// calls are synchronous functions backed by __ether_host, charged by the
// Go side; a Fault comes back as a thrown Error whose message carries the
// fault prefix, and ether.abort throws an aborted fault.
const servicePrelude = `
"use strict";
globalThis.ether = (() => {
  const call = (name, args) => JSON.parse(__ether_host(name, JSON.stringify(args)));
  const ether = {
    log: (...a) => { try { __ether_host("log", JSON.stringify(a.map(String))); } catch (e) {} },
    abort: (msg) => { throw new Error("__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(msg == null ? "aborted" : msg) })); },
    kv: { get: (k) => call("kv.get", [k]), put: (k, v) => call("kv.put", [k, v]),
          delete: (k) => call("kv.delete", [k]), list: (p) => call("kv.list", [p == null ? "" : p]) },
    settings: { get: () => call("settings.get", []) },
    secret: { get: (n) => call("secret.get", [n]) },
    http: { fetch: (req) => call("http.fetch", [req]), fetchToCache: (req) => call("http.fetchToCache", [req]) },
    catalog: { search: (q) => call("catalog.search", [q]), get: (id) => call("catalog.get", [id]) },
    addon: { get: (route) => call("addon.get", [route]) },
    viewers: { granted: () => call("viewers.granted", []), history: (id, page) => call("viewers.history", [id, page == null ? 0 : page]),
               setWatchState: (v) => call("viewers.setWatchState", [v]) },
    collections: { upsert: (c) => call("collections.upsert", [c]) },
    kinds: { upsert: (kind, items) => call("kinds.upsert", [kind, items == null ? [] : items]),
             delete: (kind, ids) => call("kinds.delete", [kind, ids == null ? [] : ids]),
             items: (kind, cursor, limit) => call("kinds.items", [kind, cursor == null ? "" : cursor, limit == null ? 0 : limit]) },
    sources: { resolve: (q) => call("sources.resolve", [q == null ? {} : q]) },
    cache: { put: (lib, name, data) => call("cache.put", [lib, name == null ? "" : name, data == null ? "" : data]),
             open: (blobRef) => call("cache.open", [blobRef]), read: (blobRef, range) => call("cache.read", [blobRef, range]) },
    archive: { list: (blobRef) => call("archive.list", [blobRef]), open: (blobRef, member, range) => call("archive.open", [blobRef, member, range]) },
    frontend: { streamURL: (itemRef) => call("frontend.streamURL", [itemRef]) },
    folders: { list: (root, path) => call("folders.list", [root, path == null ? "" : path]),
               hashStart: (root, path) => call("folders.hashStart", [root, path]),
               hashPoll: (id) => call("folders.hashPoll", [id]),
               hashCancel: (id) => call("folders.hashCancel", [id]),
               hash: (root, path) => call("folders.hash", [root, path]),
               stat: (root, path) => call("folders.stat", [root, path]) },
    notify: { owner: (text) => call("notify.owner", [text]) },
  };
  return ether;
})();
globalThis.console = { log: (...a) => ether.log(...a), error: (...a) => ether.log(...a), warn: (...a) => ether.log(...a) };
`

// hotPrelude locks a hot-path instance down: no host services except log
// and abort, Date fixed at the epoch, Math.random a seeded generator that
// __ether_rank_batch resets per batch. The monotonic clock is captured in
// a closure for budget checks, then hidden from plugin code.
//
// Budgets are measured with performance.now (WASI's monotonic clock), not
// Date.now: the wall clock counts whole milliseconds, can step, and on
// Windows advances only once per ~15.6 ms timer tick, so a 2 ms budget read
// from it flagged fast candidates that straddled a tick as slow. wazero
// backs the monotonic clock with QueryPerformanceCounter on Windows.
const hotPrelude = `
"use strict";
globalThis.ether = (() => {
  // Capture the monotonic clock for budget checks before it is replaced.
  // It lives only in this closure; plugin code sees the frozen Date and
  // performance below, and a host-function clock would cost a boundary
  // crossing per check.
  const now = performance.now.bind(performance);
  // The calling thread's CPU time in ms, or -1 without a CPU clock. Hidden
  // from plugin code like the monotonic clock.
  const cpu = typeof __ether_cpu_ms === "function" ? __ether_cpu_ms : () => -1;
  delete globalThis.__ether_cpu_ms;
  let state = 0x2545F491;
  const nextRandom = () => { state = (state * 1103515245 + 12345) & 0x7fffffff; return state / 0x80000000; };
  class FixedDate extends Date { constructor(...a) { super(...(a.length ? a : [0])); } static now() { return 0; } }
  globalThis.Date = FixedDate;
  globalThis.performance = Object.freeze({ now: () => 0, timeOrigin: 0 });
  Math.random = nextRandom;
  // A batch arrives in two calls. __ether_load_batch parses the facts and
  // the config or condition value: host work, run with the JSON functions
  // captured here so no plugin code runs in it, under its own deadline.
  // __ether_rank_batch and __ether_cond_batch then run the plugin over it.
  //
  // Budgets bound the plugin's own time (SPEC 5.2): each candidate gets
  // perMs, and the batch stops starting candidates once batchMs is spent;
  // the rest come back null (neutral, slow, not memoized). Wall time on a
  // busy host includes the time the thread sat descheduled, which is not
  // the plugin's. So the cheap monotonic clock is read first, and when it
  // says a budget is spent the thread's CPU clock decides: the batch goes on
  // while the plugin's CPU time is within budget, and a candidate over
  // budget by the wall clock runs once more and is slow only if that run's
  // CPU time is over budget too. Without a CPU clock the wall clock rules.
  const parse = JSON.parse, stringify = JSON.stringify;
  let batch = [], batchArg = null;
  globalThis.__ether_load_batch = (candsJson, argJson) => {
    batch = parse(candsJson);
    batchArg = parse(argJson);
    return batch.length;
  };
  const each = (perMs, batchMs, call, accept) => {
    const cands = batch;
    batch = [];
    const out = [];
    for (let i = 0; i < cands.length; i++) out.push(null);
    const wall0 = now(), cpu0 = cpu();
    for (let i = 0; i < cands.length; i++) {
      if (now() - wall0 > batchMs && (cpu0 < 0 || cpu() - cpu0 > batchMs)) break;
      let t0 = now(), v = call(cands[i]);
      if (now() - t0 > perMs) {
        // Over by the wall clock: run it again and judge the plugin's own time.
        const c0 = cpu();
        t0 = now();
        v = call(cands[i]);
        if (c0 < 0 ? now() - t0 > perMs : cpu() - c0 > perMs) continue;
      }
      out[i] = accept(v);
    }
    return stringify(out);
  };
  const score = (v) => (typeof v === "bigint" ? Number(v) : v);
  globalThis.__ether_rank_batch = (perMs, batchMs) => {
    state = 0x2545F491;
    const cfg = batchArg;
    return each(perMs, batchMs, (c) => rank_key(c, cfg), score);
  };
  globalThis.__ether_cond_batch = (op, perMs, batchMs) => {
    state = 0x2545F491;
    const value = batchArg;
    return each(perMs, batchMs, (c) => cond_match(c, op, value), (v) => !!v);
  };
  return {
    log: (...a) => { try { __ether_host("log", JSON.stringify(a.map(String))); } catch (e) {} },
    abort: (msg) => { throw new Error("__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(msg == null ? "aborted" : msg) })); },
  };
})();
globalThis.console = { log: (...a) => ether.log(...a), error: (...a) => ether.log(...a), warn: (...a) => ether.log(...a) };
`

// exportProbe reports entry points and normalizes esbuild's IIFE exports
// (global name __plugin) onto globalThis so both authoring styles work.
const exportProbe = `
(() => {
  const pick = (n) => (typeof globalThis[n] === "function" ? globalThis[n]
    : (globalThis.__plugin && typeof globalThis.__plugin[n] === "function" ? globalThis.__plugin[n] : undefined));
  for (const n of ["handle", "rank_key", "cond_match"]) { const f = pick(n); if (f) globalThis[n] = f; }
  return JSON.stringify({ handle: typeof globalThis.handle === "function",
    rank_key: typeof globalThis.rank_key === "function",
    cond_match: typeof globalThis.cond_match === "function" });
})()
`

// swapCtx is a context whose target can be swapped atomically per call.
// qjs calls wasm functions with its stored Context value; giving it a
// swapCtx lets one pooled instance take per-call deadlines (which close
// the module on expiry) without racing wazero's cancellation watcher.
type swapCtx struct {
	cur atomic.Pointer[context.Context]
}

func newSwapCtx() *swapCtx {
	s := &swapCtx{}
	bg := context.Background()
	s.cur.Store(&bg)
	return s
}

func (s *swapCtx) set(ctx context.Context) { s.cur.Store(&ctx) }
func (s *swapCtx) reset()                  { s.set(context.Background()) }
func (s *swapCtx) Deadline() (time.Time, bool) {
	return (*s.cur.Load()).Deadline()
}
func (s *swapCtx) Done() <-chan struct{} { return (*s.cur.Load()).Done() }
func (s *swapCtx) Err() error            { return (*s.cur.Load()).Err() }
func (s *swapCtx) Value(key any) any     { return (*s.cur.Load()).Value(key) }

// jsLogWriter keeps WASI output out of the process streams.
type jsLogWriter struct {
	logger   *slog.Logger
	pluginID string
}

func (w jsLogWriter) Write(p []byte) (int, error) {
	if w.logger != nil {
		w.logger.Debug("plugin output", "plugin", w.pluginID, "output", string(p))
	}
	return len(p), nil
}

type jsInstance struct {
	rt           *qjs.Runtime
	qc           *qjs.Context
	swap         *swapCtx
	instanceCaps caps
	hot          bool
	cs           *callState // set for the duration of one call
}

func newJSInstance(ctx context.Context, source string, lim *Limits, hot bool, logger *slog.Logger, pluginID string) (in *jsInstance, err error) {
	defer func() {
		if r := recover(); r != nil {
			in, err = nil, &Error{Status: StatusError, Message: fmt.Sprintf("create JS instance: %v", r), Trap: true}
		}
	}()
	swap := newSwapCtx()
	initCtx, cancel := context.WithTimeout(ctx, lim.Deadline)
	defer cancel()
	swap.set(initCtx)
	defer swap.reset()
	rt, err2 := qjs.New(qjs.Option{Context: swap, CloseOnContextDone: true,
		MemoryLimit: int(lim.MemoryBytes), MaxStackSize: 1 << 20,
		Stdout: jsLogWriter{logger, pluginID}, Stderr: jsLogWriter{logger, pluginID}})
	if err2 != nil {
		return nil, &Error{Status: StatusError, Message: "create JS runtime: " + err2.Error(), Trap: true}
	}
	in = &jsInstance{rt: rt, qc: rt.Context(), swap: swap, hot: hot}
	qc := in.qc
	qc.SetFunc("__ether_host", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if in.cs == nil || len(args) != 2 {
			return nil, errors.New(encodeFault(Forbidden("host call outside a plugin call")))
		}
		out, fault := in.cs.dispatch(args[0].String(), json.RawMessage(args[1].String()))
		if fault != nil {
			return nil, errors.New(encodeFault(fault))
		}
		return qc.NewString(string(out)), nil
	})
	prelude := servicePrelude
	if hot {
		prelude = hotPrelude
		qc.SetFunc("__ether_cpu_ms", func(*qjs.This) (*qjs.Value, error) {
			d, ok := threadCPU()
			if !ok {
				return qc.NewFloat64(-1), nil
			}
			return qc.NewFloat64(budgetMs(d)), nil
		})
	}
	if _, err := qc.Eval("ether-prelude.js", qjs.Code(prelude)); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "prelude: " + err.Error(), Trap: true}
	}
	if _, err := qc.Eval("plugin.js", qjs.Code(source)); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "load plugin: " + firstLine(err.Error()), Trap: false}
	}
	v, err := qc.Eval("ether-probe.js", qjs.Code(exportProbe))
	if err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "probe exports: " + err.Error(), Trap: true}
	}
	var found struct {
		Handle    bool `json:"handle"`
		RankKey   bool `json:"rank_key"`
		CondMatch bool `json:"cond_match"`
	}
	if err := json.Unmarshal([]byte(v.String()), &found); err != nil {
		in.closeInstance()
		return nil, &Error{Status: StatusError, Message: "probe exports: " + err.Error(), Trap: true}
	}
	in.instanceCaps = caps{Handle: found.Handle, RankKey: found.RankKey, CondMatch: found.CondMatch}
	return in, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (in *jsInstance) caps() caps { return in.instanceCaps }

func (in *jsInstance) closeInstance() {
	defer func() { _ = recover() }()
	in.rt.Close()
}

// jsCall runs fn with the call context installed, converting qjs panics
// (it panics when a deadline closes the module mid-call) into *Error.
func (in *jsInstance) jsCall(ctx context.Context, cs *callState, fn func() (*qjs.Value, error)) (val *qjs.Value, callErr *Error) {
	in.cs = cs
	in.swap.set(ctx)
	defer func() {
		in.cs = nil
		in.swap.reset()
		if r := recover(); r != nil {
			val = nil
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				callErr = &Error{Status: StatusTimeout, Message: "call exceeded its deadline", Trap: true}
				return
			}
			callErr = &Error{Status: StatusError, Message: fmt.Sprintf("%v", r), Trap: true}
		}
	}()
	v, err := fn()
	if err != nil {
		return nil, classifyJSError(ctx, err)
	}
	if v != nil && v.IsPromise() {
		v, err = v.Await()
		if err != nil {
			return nil, classifyJSError(ctx, err)
		}
	}
	return v, nil
}

func classifyJSError(ctx context.Context, err error) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Status: StatusTimeout, Message: "call exceeded its deadline", Trap: true}
	}
	msg := err.Error()
	if f := decodeFault(msg); f != nil {
		st := statusForFaultCode(f.Code)
		return &Error{Status: st, Message: f.Message, Trap: st == StatusAborted}
	}
	if strings.Contains(msg, "module closed") || strings.Contains(msg, "interrupted") {
		return &Error{Status: StatusTimeout, Message: "call exceeded its deadline", Trap: true}
	}
	return &Error{Status: StatusError, Message: firstLine(msg)}
}

func (in *jsInstance) globalFn(name string) *qjs.Value {
	return in.qc.Global().GetPropertyStr(name)
}

func (in *jsInstance) handle(ctx context.Context, cs *callState, reqJSON []byte) ([]byte, *Error) {
	v, err := in.jsCall(ctx, cs, func() (*qjs.Value, error) {
		g := in.qc.Global()
		return in.qc.Invoke(in.globalFn("__ether_dispatch_handle"), g, in.qc.NewString(string(reqJSON)))
	})
	if err != nil {
		// Fall back to calling handle directly with a parsed object; the
		// dispatch wrapper is installed by installDispatch at load. If it is
		// missing this error is the real one.
		return nil, err
	}
	return []byte(v.String()), nil
}

func (in *jsInstance) rankOne(ctx context.Context, cs *callState, cand, cfg []byte) (int64, *Error) {
	unbounded := Limits{RankPerCandidate: 1 << 60, RankBatch: 1 << 60}
	scores, err := in.rankBatch(ctx, cs, [][]byte{cand}, cfg, &unbounded)
	if err != nil {
		return 0, err
	}
	if scores[0] == nil {
		return 0, &Error{Status: StatusTimeout, Message: "candidate exceeded its budget", Trap: false}
	}
	return *scores[0], nil
}

func budgetMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// loadBatch hands a batch's facts and its config (or condition value) to
// the instance: one parse of one JSON array, under its own deadline, so
// the host's share of a batch is never charged to the plugin.
func (in *jsInstance) loadBatch(ctx context.Context, cs *callState, cands [][]byte, arg []byte, lim *Limits) *Error {
	candsJSON, ferr := factsArray(cands)
	if ferr != nil {
		return ferr
	}
	loadCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	_, err := in.jsCall(loadCtx, cs, func() (*qjs.Value, error) {
		return in.qc.Invoke(in.globalFn("__ether_load_batch"), in.qc.Global(),
			in.qc.NewString(string(candsJSON)), in.qc.NewString(string(arg)))
	})
	return err
}

func (in *jsInstance) rankBatch(ctx context.Context, cs *callState, cands [][]byte, cfg []byte, lim *Limits) ([]*int64, *Error) {
	if err := in.loadBatch(ctx, cs, cands, cfg, lim); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	runtime.LockOSThread() // the budget reads this thread's CPU clock
	defer runtime.UnlockOSThread()
	v, err := in.jsCall(runCtx, cs, func() (*qjs.Value, error) {
		return in.qc.Invoke(in.globalFn("__ether_rank_batch"), in.qc.Global(),
			in.qc.NewFloat64(budgetMs(lim.RankPerCandidate)), in.qc.NewFloat64(budgetMs(lim.RankBatch)))
	})
	if err != nil {
		return nil, err
	}
	var out []*int64
	if uerr := json.Unmarshal([]byte(v.String()), &out); uerr != nil {
		return nil, &Error{Status: StatusError, Message: "rank_key returned a non-numeric result: " + uerr.Error()}
	}
	if len(out) != len(cands) {
		return nil, &Error{Status: StatusError, Message: "rank batch returned the wrong number of scores"}
	}
	return out, nil
}

// factsArray joins candidate facts into one JSON array, parsed once per
// batch instead of once per candidate. Each fact must be one JSON value, so
// a malformed fact cannot shift the others' positions.
func factsArray(cands [][]byte) ([]byte, *Error) {
	n := 2
	for _, c := range cands {
		if !json.Valid(c) {
			return nil, &Error{Status: StatusError, Message: "candidate facts are not valid JSON"}
		}
		n += len(c) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, '[')
	for i, c := range cands {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, c...)
	}
	return append(b, ']'), nil
}

func (in *jsInstance) condBatch(ctx context.Context, cs *callState, cands [][]byte, op string, val []byte, lim *Limits) ([]*bool, *Error) {
	if err := in.loadBatch(ctx, cs, cands, val, lim); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, lim.hotKill())
	defer cancel()
	runtime.LockOSThread() // the budget reads this thread's CPU clock
	defer runtime.UnlockOSThread()
	v, err := in.jsCall(runCtx, cs, func() (*qjs.Value, error) {
		return in.qc.Invoke(in.globalFn("__ether_cond_batch"), in.qc.Global(), in.qc.NewString(op),
			in.qc.NewFloat64(budgetMs(lim.RankPerCandidate)), in.qc.NewFloat64(budgetMs(lim.RankBatch)))
	})
	if err != nil {
		return nil, err
	}
	var out []*bool
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil || len(out) != len(cands) {
		return nil, &Error{Status: StatusError, Message: "invalid condition batch result"}
	}
	return out, nil
}

func (in *jsInstance) condOne(ctx context.Context, cs *callState, cand []byte, op string, val []byte) (bool, *Error) {
	v, err := in.jsCall(ctx, cs, func() (*qjs.Value, error) {
		g := in.qc.Global()
		c := in.qc.ParseJSON(string(cand))
		value := in.qc.ParseJSON(string(val))
		return in.qc.Invoke(in.globalFn("cond_match"), g, c, in.qc.NewString(op), value)
	})
	if err != nil {
		return false, err
	}
	return v.Bool(), nil
}

// LoadJS loads a JavaScript plugin (TypeScript authors go through Compile
// first). A probe instance is built eagerly so syntax errors and a missing
// handle surface at load.
func (e *Engine) LoadJS(id, version, source string, host Host, lim Limits) (*Plugin, error) {
	lim.normalize()
	e.scaleHot(&lim)
	p := &Plugin{id: id, version: version, eng: e, host: host, lim: lim, memo: newMemo(4096), source: source}
	makeSvc := func(ctx context.Context) (instance, error) {
		in, err := newJSInstance(ctx, source, &p.lim, false, e.logger, id)
		if err != nil {
			return nil, err
		}
		if err := in.installDispatch(); err != nil {
			in.closeInstance()
			return nil, err
		}
		return in, nil
	}
	makeHot := func(ctx context.Context) (instance, error) {
		return newJSInstance(ctx, source, &p.lim, true, e.logger, id)
	}
	p.svc = newPool(lim.PoolSize, makeSvc)
	p.hot = newWarmPool(lim.PoolSize, makeHot)
	probe, err := p.svc.get(context.Background())
	if err != nil {
		p.Close()
		return nil, err
	}
	p.caps = probe.caps()
	p.svc.put(probe, true)
	if !p.caps.Handle && !p.caps.RankKey && !p.caps.CondMatch {
		p.Close()
		return nil, &Error{Status: StatusError, Message: "plugin defines none of handle, rank_key, cond_match"}
	}
	p.warmHot()
	return p, nil
}

// installDispatch wraps handle so Go passes one JSON string and gets one
// JSON string back; the plugin sees plain objects either side.
func (in *jsInstance) installDispatch() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &Error{Status: StatusError, Message: fmt.Sprintf("install dispatch: %v", r), Trap: true}
		}
	}()
	_, err2 := in.qc.Eval("ether-dispatch.js", qjs.Code(`
globalThis.__ether_dispatch_handle = (reqJson) => {
  const res = handle(JSON.parse(reqJson));
  if (res && typeof res.then === "function") return res.then((r) => JSON.stringify(r == null ? {} : r));
  return JSON.stringify(res == null ? {} : res);
};`))
	if err2 != nil {
		return &Error{Status: StatusError, Message: "install dispatch: " + err2.Error(), Trap: true}
	}
	return nil
}
