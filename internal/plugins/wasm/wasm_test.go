package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// fakeHost is an in-memory Host recording calls.
type fakeHost struct {
	mu    sync.Mutex
	kv    map[string]json.RawMessage
	calls []string
	fail  map[string]error
}

func newFakeHost() *fakeHost {
	return &fakeHost{kv: map[string]json.RawMessage{}, fail: map[string]error{}}
}

func (h *fakeHost) CallHost(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
	if err := h.fail[name]; err != nil {
		return nil, err
	}
	var argv []json.RawMessage
	_ = json.Unmarshal(args, &argv)
	switch name {
	case "kv.get":
		var k string
		_ = json.Unmarshal(argv[0], &k)
		if v, ok := h.kv[k]; ok {
			return v, nil
		}
		return json.RawMessage("null"), nil
	case "kv.put":
		var k string
		_ = json.Unmarshal(argv[0], &k)
		h.kv[k] = argv[1]
		return json.RawMessage("null"), nil
	case "log":
		return json.RawMessage("null"), nil
	}
	return nil, Forbidden("not granted: " + name)
}

func errStatus(t *testing.T, err error) Status {
	t.Helper()
	var we *Error
	if !errors.As(err, &we) {
		t.Fatalf("error is %T (%v), want *Error", err, err)
	}
	return we.Status
}

const helloJS = `
globalThis.handle = function(req) {
  if (req.path === "/loop") { while (true) {} }
  if (req.path === "/oom") { return { status: 200, body: "x".repeat(64 * 1024 * 1024) }; }
  if (req.path === "/abort") { ether.abort("stop now"); }
  if (req.path === "/spam") { for (let i = 0; i < 100000; i++) ether.kv.get("k"); return { status: 200, body: "spammed" }; }
  if (req.path === "/secret") { return { status: 200, body: String(ether.secret.get("api")) }; }
  const prev = ether.kv.get("greeting");
  ether.kv.put("greeting", "stored");
  ether.log("handled", req.path);
  return { status: 200, headers: { "x-plugin": "hello" }, body: "hello " + req.path + " prev=" + prev };
};
globalThis.rank_key = function(c, cfg) { return (c.resolution >= 2160 ? 100 : 0) + (c.cached ? 10 : 0); };
globalThis.cond_match = function(c, op, v) { return op === "gte" ? c.resolution >= v : false; };
`

func TestJSHandleRoundTripAndHost(t *testing.T) {
	e := testEngine(t)
	host := newFakeHost()
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, host, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.HasHandle() || !p.HasRankKey() || !p.HasCondMatch() {
		t.Fatalf("caps = %+v", p.caps)
	}
	resp, out, err := p.CallDetailed(context.Background(), Request{Method: "GET", Path: "/world"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "hello /world") {
		t.Fatalf("resp = %+v body %s", resp, resp.Body)
	}
	if out.Status != StatusOK || out.Units <= BaseCallUnits {
		t.Fatalf("outcome = %+v, want ok with host units charged", out)
	}
	// Second call sees the value the first stored through the host.
	resp, err = p.Call(context.Background(), Request{Method: "GET", Path: "/again"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "prev=stored") {
		t.Fatalf("body = %s", resp.Body)
	}
	if got := p.Stats(); got.Calls != 2 || got.Errors != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestJSForbiddenAndAbortStatuses(t *testing.T) {
	e := testEngine(t)
	host := newFakeHost()
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, host, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, err = p.Call(context.Background(), Request{Method: "GET", Path: "/secret"})
	if errStatus(t, err) != StatusForbidden {
		t.Fatalf("secret status = %v (%v)", errStatus(t, err), err)
	}
	_, out, err := p.CallDetailed(context.Background(), Request{Method: "GET", Path: "/abort"})
	if errStatus(t, err) != StatusAborted || out.Status != StatusAborted {
		t.Fatalf("abort = %v outcome %+v", err, out)
	}
	if got := p.Stats(); got.Forbidden != 1 || got.Aborted != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestJSGovernanceBudget(t *testing.T) {
	e := testEngine(t)
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, newFakeHost(), Limits{UnitBudget: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, out, err := p.CallDetailed(context.Background(), Request{Method: "GET", Path: "/spam"})
	if errStatus(t, err) != StatusGovernance || out.Status != StatusGovernance {
		t.Fatalf("spam = %v outcome %+v", err, out)
	}
	if p.Stats().Governance != 1 {
		t.Fatalf("stats = %+v", p.Stats())
	}
}

func TestJSTimeoutDiscardsInstance(t *testing.T) {
	e := testEngine(t)
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, newFakeHost(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Limit the looping call, not initialization of its replacement on a busy
	// host. Initialization deadlines have their own test below.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, out, err := p.CallDetailed(ctx, Request{Method: "GET", Path: "/loop"})
	if errStatus(t, err) != StatusTimeout || !out.Trap {
		t.Fatalf("loop = %v outcome %+v", err, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	// The pool must hand out a fresh instance: a normal call still works.
	resp, err := p.Call(context.Background(), Request{Method: "GET", Path: "/ok"})
	if err != nil || !strings.Contains(string(resp.Body), "hello /ok") {
		t.Fatalf("after timeout: %v %s", err, resp.Body)
	}
	if got := p.Stats(); got.Timeouts != 1 || got.Traps != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestJSMemoryCap(t *testing.T) {
	e := testEngine(t)
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, newFakeHost(), Limits{MemoryBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, err = p.Call(context.Background(), Request{Method: "GET", Path: "/oom"})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("oom error = %v, want out of memory", err)
	}
}

func TestJSLoadErrors(t *testing.T) {
	e := testEngine(t)
	if _, err := e.LoadJS("bad.syntax", "1", "globalThis.handle = function( {", nil, Limits{}); err == nil {
		t.Fatal("syntax error at load not reported")
	}
	if _, err := e.LoadJS("bad.empty", "1", "const x = 1;", nil, Limits{}); err == nil {
		t.Fatal("missing entry points not reported")
	}
}

func TestTSCompileLoadAndCall(t *testing.T) {
	files := map[string]string{
		"src/index.ts": `
import { greet } from "./greet";
interface Req { path: string }
export function handle(req: Req): { status: number; body: string } {
  return { status: 200, body: greet(req.path) };
}
export function rank_key(c: { resolution: number }): number { return c.resolution >= 2160 ? 7 : 1; }
`,
		"src/greet.ts": `export const greet = (p: string): string => "ts-hello " + p;`,
	}
	js, smap, diags := Compile(files, "src/index.ts")
	if len(diags) != 0 || len(js) == 0 {
		t.Fatalf("compile diags=%v js=%d bytes", diags, len(js))
	}
	if len(smap) == 0 || !strings.Contains(string(smap), "greet.ts") {
		t.Fatalf("sourcemap missing sources: %d bytes", len(smap))
	}
	e := testEngine(t)
	p, err := e.LoadJS("test.ts", "1.0.0", string(js), newFakeHost(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := p.Call(context.Background(), Request{Method: "GET", Path: "/ts"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "ts-hello /ts") {
		t.Fatalf("body = %s", resp.Body)
	}
	scores, _, err := p.RankBatch(context.Background(),
		[]Candidate{{ReleaseID: "r1", Facts: json.RawMessage(`{"resolution":2160}`)}}, nil)
	if err != nil || scores[0] != 7 {
		t.Fatalf("rank = %v %v", scores, err)
	}
}

func TestTSCompileDiagnostics(t *testing.T) {
	_, _, diags := Compile(map[string]string{"a.ts": "const x: number = ;\n"}, "a.ts")
	if len(diags) == 0 || diags[0].Severity != "error" || diags[0].Line == 0 {
		t.Fatalf("diags = %+v", diags)
	}
	js, _, diags := Compile(map[string]string{"a.ts": `import "./missing";`}, "a.ts")
	if js != nil || len(diags) == 0 {
		t.Fatalf("missing import: js=%v diags=%+v", js != nil, diags)
	}
	_, _, diags = Compile(map[string]string{}, "nope.ts")
	if len(diags) == 0 {
		t.Fatal("missing entry not diagnosed")
	}
}

func loadRaw(t *testing.T, e *Engine, name string, host Host, lim Limits) *Plugin {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.LoadWASM("test.raw", "1.0.0", b, host, lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestRawHandleHostAndHot(t *testing.T) {
	e := testEngine(t)
	host := newFakeHost()
	host.kv["k"] = json.RawMessage(`"value42"`)
	p := loadRaw(t, e, "rawplugin.wasm", host, Limits{})
	if !p.HasHandle() || !p.HasRankKey() || !p.HasCondMatch() {
		t.Fatalf("caps = %+v", p.caps)
	}
	resp, err := p.Call(context.Background(), Request{Method: "GET", Path: "/rawpath"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 201 || !strings.Contains(string(resp.Body), "raw-ok:/rawpath") || resp.Headers["x-raw"] != "1" {
		t.Fatalf("resp = %+v body %s", resp, resp.Body)
	}
	resp, err = p.Call(context.Background(), Request{Method: "GET", Path: "/host"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "host:value42") {
		t.Fatalf("host body = %s", resp.Body)
	}
	cands := []Candidate{
		{ReleaseID: "a", Facts: json.RawMessage(`{"resolution":2160,"cached":true}`)},
		{ReleaseID: "b", Facts: json.RawMessage(`{"resolution":1080,"cached":false}`)},
	}
	scores, out, err := p.RankBatch(context.Background(), cands, nil)
	if err != nil || scores[0] != 110 || scores[1] != 0 || out.SlowCandidates != 0 {
		t.Fatalf("scores=%v out=%+v err=%v", scores, out, err)
	}
	ok, _, err := p.CondMatch(context.Background(), cands[0], "contains", json.RawMessage(`"2160"`))
	if err != nil || !ok {
		t.Fatalf("cond = %v %v", ok, err)
	}
	ok, _, err = p.CondMatch(context.Background(), cands[1], "contains", json.RawMessage(`"2160"`))
	if err != nil || ok {
		t.Fatalf("cond = %v %v", ok, err)
	}
}

func TestRawForbiddenImportRefused(t *testing.T) {
	e := testEngine(t)
	b, err := os.ReadFile("testdata/forbidden.wasm")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.LoadWASM("test.bad", "1", b, nil, Limits{})
	if errStatus(t, err) != StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}

func TestRawTimeoutAbortAndMemoryCap(t *testing.T) {
	e := testEngine(t)
	// Only /loop measures the deadline, so only it runs under a short one:
	// the abort and memory-cap calls are not about time and must not fail
	// because a loaded host paused for a few hundred milliseconds.
	looping := loadRaw(t, e, "rawplugin.wasm", newFakeHost(), Limits{Deadline: 300 * time.Millisecond, MemoryBytes: 32 << 20})
	_, err := looping.Call(context.Background(), Request{Method: "GET", Path: "/loop"})
	if errStatus(t, err) != StatusTimeout {
		t.Fatalf("loop err = %v", err)
	}
	if got := looping.Stats(); got.Timeouts != 1 || got.Traps < 1 {
		t.Fatalf("loop stats = %+v", got)
	}
	p := loadRaw(t, e, "rawplugin.wasm", newFakeHost(), Limits{MemoryBytes: 32 << 20})
	_, err = p.Call(context.Background(), Request{Method: "GET", Path: "/abort"})
	if errStatus(t, err) != StatusAborted {
		t.Fatalf("abort err = %v", err)
	}
	resp, err := p.Call(context.Background(), Request{Method: "GET", Path: "/oom"})
	if err != nil {
		t.Fatal(err)
	}
	var pages int
	if _, err := fmt.Sscanf(strings.Trim(string(resp.Body), `"`), "pages:%d", &pages); err != nil {
		t.Fatalf("oom body = %s (%v)", resp.Body, err)
	}
	if pages > 512 { // 32 MiB cap
		t.Fatalf("guest grew to %d pages past the 32 MiB cap", pages)
	}
	if got := p.Stats(); got.Timeouts != 0 || got.Aborted != 1 || got.Traps < 1 {
		t.Fatalf("stats = %+v", got)
	}
}

const hotJS = `
let counter = 0;
globalThis.rank_key = function(c, cfg) {
  counter++;
  if (c.counting) return counter;
  // Determinism probe: these would vary run to run without the hot prelude.
  return Math.floor(Math.random() * 100000) + Date.now() + (c.resolution >= 2160 ? 5 : 0);
};
globalThis.cond_match = function(c, op, v) { return op === "is" ? c.group === v : false; };
`

// Determinism: 1000 candidates whose scores draw on Math.random and
// Date.now must produce the identical sequence on a fresh plugin, and
// again on the same plugin (RNG resets per batch, Date is frozen).
func TestHotDeterminism1000Runs(t *testing.T) {
	e := testEngine(t)
	run := func(p *Plugin, prefix string) []int64 {
		cands := make([]Candidate, 1000)
		for i := range cands {
			cands[i] = Candidate{ReleaseID: fmt.Sprintf("%s-%d", prefix, i), Facts: json.RawMessage(`{"resolution":2160}`)}
		}
		scores, out, err := p.RankBatch(context.Background(), cands, json.RawMessage(`{"k":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if out.SlowCandidates != 0 {
			t.Fatalf("%d candidates went slow in a trivial batch", out.SlowCandidates)
		}
		return scores
	}
	p, err := e.LoadJS("test.hot", "2.0.0", hotJS, nil, roomyHot())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	want := run(p, "a")
	for i, got := range run(p, "b") { // same plugin, unmemoized ids
		if got != want[i] {
			t.Fatalf("same-plugin rerun differs at %d: %d vs %d", i, got, want[i])
		}
	}
	p2, err := e.LoadJS("test.hot", "2.0.0", hotJS, nil, roomyHot())
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	for i, got := range run(p2, "c") { // fresh plugin
		if got != want[i] {
			t.Fatalf("fresh plugin differs at %d: %d vs %d", i, got, want[i])
		}
	}
}

func TestHotMemoization(t *testing.T) {
	e := testEngine(t)
	// One instance, so the guest's call counter shows every real execution.
	p, err := e.LoadJS("test.memo", "1.0.0", hotJS, nil, Limits{PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cands := []Candidate{
		{ReleaseID: "m1", Facts: json.RawMessage(`{"counting":true}`)},
		{ReleaseID: "m2", Facts: json.RawMessage(`{"counting":true}`)},
	}
	first, out1, err := p.RankBatch(context.Background(), cands, nil)
	if err != nil || first[0] != 1 || first[1] != 2 || out1.MemoHits != 0 {
		t.Fatalf("first=%v out=%+v err=%v", first, out1, err)
	}
	// Same batch again: counter would advance to 3,4 without the memo.
	second, out2, err := p.RankBatch(context.Background(), cands, nil)
	if err != nil || second[0] != 1 || second[1] != 2 || out2.MemoHits != 2 {
		t.Fatalf("second=%v out=%+v err=%v", second, out2, err)
	}
	// A different config is a different memo key.
	third, out3, err := p.RankBatch(context.Background(), cands, json.RawMessage(`{"other":true}`))
	if err != nil || third[0] != 3 || out3.MemoHits != 0 {
		t.Fatalf("third=%v out=%+v err=%v", third, out3, err)
	}
	// CondMatch memoizes too, keyed with operator and value.
	ok1, _, err := p.CondMatch(context.Background(), Candidate{ReleaseID: "c1", Facts: json.RawMessage(`{"group":"GRP"}`)}, "is", json.RawMessage(`"GRP"`))
	ok2, outC, err2 := p.CondMatch(context.Background(), Candidate{ReleaseID: "c1", Facts: json.RawMessage(`{"group":"GRP"}`)}, "is", json.RawMessage(`"GRP"`))
	if err != nil || err2 != nil || !ok1 || !ok2 || outC.MemoHits != 1 {
		t.Fatalf("cond memo: %v %v %+v", ok1, ok2, outC)
	}
}

func TestHotBatchBudgetNeutralRaw(t *testing.T) {
	e := testEngine(t)
	p := loadRaw(t, e, "rawplugin.wasm", nil, Limits{})
	cands := []Candidate{
		{ReleaseID: "slow1", Facts: json.RawMessage(`{"resolution":2160,"cached":true,"slow":1}`)},
		{ReleaseID: "fast1", Facts: json.RawMessage(`{"resolution":2160,"cached":true}`)},
	}
	start := time.Now()
	scores, out, err := p.RankBatch(context.Background(), cands, nil)
	if err != nil {
		// A killed instance mid-batch is reported; scores stay neutral.
		if errStatus(t, err) != StatusTimeout {
			t.Fatalf("err = %v", err)
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("batch overran its budget: %v", time.Since(start))
	}
	if scores[0] != 0 || out.SlowCandidates < 1 {
		t.Fatalf("scores=%v out=%+v, want slow candidate neutral and counted", scores, out)
	}
	if p.Stats().SlowCandidates < 1 {
		t.Fatalf("health counter missing: %+v", p.Stats())
	}
	// Fresh instance from the pool scores the fast candidate fine.
	scores, _, err = p.RankBatch(context.Background(), cands[1:], nil)
	if err != nil || scores[0] != 110 {
		t.Fatalf("after slow: scores=%v err=%v", scores, err)
	}
}

func TestHotJSBatchWithinBudget(t *testing.T) {
	e := testEngine(t)
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cands := make([]Candidate, 500)
	for i := range cands {
		cands[i] = Candidate{ReleaseID: fmt.Sprintf("b-%d", i),
			Facts: json.RawMessage(fmt.Sprintf(`{"resolution":%d,"cached":%t}`, []int{1080, 2160}[i%2], i%3 == 0))}
	}
	// The engine judges the 50ms batch budget by the plugin's own time (see
	// hotPrelude), so the outcome, not the test's wall clock, says whether
	// the batch fit: a loaded host's pauses are not the plugin's.
	scores, out, err := p.RankBatch(context.Background(), cands, nil)
	if err != nil || out.Status != StatusOK || out.Trap {
		t.Fatalf("rank: %+v %v", out, err)
	}
	if out.SlowCandidates != 0 {
		t.Fatalf("slow=%d in a simple batch", out.SlowCandidates)
	}
	if scores[1] != 110 && scores[1] != 100 {
		t.Fatalf("scores[1]=%d", scores[1])
	}
	ok, _, err := p.CondMatch(context.Background(), cands[1], "gte", json.RawMessage(`2160`))
	if err != nil || !ok {
		t.Fatalf("js cond = %v %v", ok, err)
	}
}

func TestConcurrentCallsAndRanking(t *testing.T) {
	e := testEngine(t)
	lim := roomyHot()
	lim.PoolSize = 3
	p, err := e.LoadJS("test.hello", "1.0.0", helloJS, newFakeHost(), lim)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				resp, err := p.Call(context.Background(), Request{Method: "GET", Path: fmt.Sprintf("/g%d", g)})
				if err != nil || !strings.Contains(string(resp.Body), "hello") {
					errs <- fmt.Errorf("call g%d: %v %s", g, err, resp.Body)
					return
				}
				cands := []Candidate{{ReleaseID: fmt.Sprintf("g%d-%d", g, i), Facts: json.RawMessage(`{"resolution":2160,"cached":true}`)}}
				scores, _, err := p.RankBatch(context.Background(), cands, nil)
				if err != nil || scores[0] != 110 {
					errs <- fmt.Errorf("rank g%d: %v %v", g, scores, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestLimitsClamp(t *testing.T) {
	lim := Limits{MemoryBytes: 1 << 40, Deadline: time.Hour, UnitBudget: 1 << 40, RankPerCandidate: time.Second, RankBatch: time.Minute}
	lim.normalize()
	if lim.MemoryBytes != MaxMemoryBytes {
		t.Fatalf("memory = %d", lim.MemoryBytes)
	}
	if lim.Deadline != time.Hour || lim.UnitBudget != 1<<40 {
		t.Fatalf("owner call grants were changed: %+v", lim)
	}
	if lim.RankPerCandidate != 2*time.Millisecond || lim.RankBatch != 50*time.Millisecond {
		t.Fatalf("hot budgets not clamped: %v %v", lim.RankPerCandidate, lim.RankBatch)
	}
	viewer := limitsForRequest(lim, "/ether/v1/kinds/games/browse")
	if viewer.UnitBudget != 10000 || viewer.Deadline != 30*time.Second {
		t.Fatalf("viewer budgets: %+v", viewer)
	}
	job := limitsForRequest(lim, "/ether/job/build")
	if job.UnitBudget != lim.UnitBudget || job.Deadline != lim.Deadline {
		t.Fatalf("job budgets were lowered: %+v", job)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitBackground(ctx, func() bool { return true }); err != context.Canceled {
		t.Fatalf("busy wait cancellation: %v", err)
	}

}

// roomyHot gives correctness tests budgets no loaded machine can miss, so
// they check behaviour, not speed. Budget enforcement has its own tests
// with the real limits.
func roomyHot() Limits {
	return Limits{RankPerCandidate: time.Second, RankBatch: time.Minute, unclamped: true}
}

func TestCondBatchExecutesRepeatedFactsWithoutMemo(t *testing.T) {
	e := testEngine(t)
	p, err := e.LoadJS("test.condition-batch", "1.0.0", `let toggle = false; globalThis.cond_match = function() { toggle = !toggle; return toggle; };`, nil, roomyHot())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := Candidate{ReleaseID: "same", Facts: json.RawMessage(`{"cached":true}`)}
	got, out, err := p.CondBatch(context.Background(), []Candidate{c, c}, "is", json.RawMessage(`true`))
	if err != nil || out.SlowCandidates != 0 || len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("results=%v outcome=%+v err=%v", got, out, err)
	}
}

func TestJSInitializationRespectsDeadline(t *testing.T) {
	e := testEngine(t)
	_, err := e.LoadJS("test.loop", "1", `while (true) {}`, nil, Limits{Deadline: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("unbounded initialization accepted")
	}
}

func TestRawStartFunctionRespectsDeadline(t *testing.T) {
	// A valid module with an endless start function; no external compiler needed.
	module := []byte{0, 97, 115, 109, 1, 0, 0, 0, 1, 4, 1, 0x60, 0, 0, 3, 2, 1, 0, 8, 1, 0, 10, 9, 1, 7, 0, 3, 0x40, 0x0c, 0, 0x0b, 0x0b}
	e := testEngine(t)
	_, err := e.LoadWASM("test.start", "1", module, nil, Limits{Deadline: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("endless start accepted")
	}
}
