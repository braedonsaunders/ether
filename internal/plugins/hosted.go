package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

// HotRuntime is implemented by hosted runtimes whose plugin exports the
// hot-path ABI (SPEC §5.2): ranking steps and rule conditions. The ranking
// adapters reach it through Manager.Hot. Budgets are the manifest's
// (lowered, never raised) and every call reports a classified outcome.
type HotRuntime interface {
	Runtime
	// RankBatch returns one key per candidate (higher ranks first). A
	// candidate that exceeded its budget gets the neutral key 0 and counts
	// in HotOutcome.Slow; a batch that ran out of time returns neutral keys
	// for the rest.
	RankBatch(ctx context.Context, cands []CandidateFacts, config json.RawMessage) (keys []int64, out HotOutcome, err error)
	// CondMatch evaluates a rule condition; false when the budget runs out.
	CondMatch(ctx context.Context, cand CandidateFacts, operator string, value json.RawMessage) (match bool, out HotOutcome, err error)
	HasRankKey() bool
	HasCondMatch() bool
	// Budgets reports the effective per-candidate and per-batch limits.
	Budgets() HotBudgets
}

// HotOutcome classifies one hot-path call for health and replay.
type HotOutcome struct {
	// Status is ok, error, timeout, forbidden, aborted or governance.
	Status   string        `json:"status"`
	Duration time.Duration `json:"durationNs"`
	Units    int64         `json:"units"`
	// Slow counts candidates that went neutral on budget.
	Slow     int  `json:"slow"`
	MemoHits int  `json:"memoHits"`
	Trap     bool `json:"trap,omitempty"`
}

// Timeout reports whether the call ran out of time or budget.
func (o HotOutcome) Timeout() bool {
	return o.Status == string(wasm.StatusTimeout) || o.Status == string(wasm.StatusGovernance)
}

// HotBudgets are the effective hot-path limits for one plugin.
type HotBudgets struct {
	PerCandidate time.Duration `json:"perCandidateNs"`
	PerBatch     time.Duration `json:"perBatchNs"`
}

// rankingBudget is the manifest's per-step budget (SPEC §4.3: budget).
type rankingBudget struct {
	PerCandidateMicros int64 `json:"perCandidateMicros"`
	PerBatchMicros     int64 `json:"perBatchMicros"`
	PerBatchMillis     int64 `json:"perBatchMillis"`
}

// HotLimits lowers the runtime's hot-path defaults to the tightest budget a
// manifest declares across its ranking steps. The engine clamps anything
// above its defaults, so a manifest can lower budgets but never raise them.
// Studio replay uses it to run a plugin under its installed budgets.
func HotLimits(m Manifest) wasm.Limits {
	lim := RuntimeLimitsFor(m)
	if m.Ether == nil {
		return lim
	}
	for _, step := range m.Ether.Contributes.RankingSteps {
		if len(step.Budget) == 0 {
			continue
		}
		var b rankingBudget
		if json.Unmarshal(step.Budget, &b) != nil {
			continue
		}
		if b.PerCandidateMicros > 0 {
			d := time.Duration(b.PerCandidateMicros) * time.Microsecond
			if lim.RankPerCandidate == 0 || d < lim.RankPerCandidate {
				lim.RankPerCandidate = d
			}
		}
		batch := time.Duration(b.PerBatchMicros) * time.Microsecond
		if b.PerBatchMillis > 0 {
			batch = time.Duration(b.PerBatchMillis) * time.Millisecond
		}
		if batch > 0 && (lim.RankBatch == 0 || batch < lim.RankBatch) {
			lim.RankBatch = batch
		}
	}
	return lim
}

func hotOutcome(o wasm.Outcome) HotOutcome {
	return HotOutcome{Status: string(o.Status), Duration: o.Duration, Units: o.Units, Slow: o.SlowCandidates, MemoHits: o.MemoHits, Trap: o.Trap}
}

// HostedRuntimeFactory opens hosted plugins (runtime.kind "hosted") on the
// wazero engine: JavaScript and TypeScript entries run on QuickJS, raw
// modules through the documented WASM ABI.
func HostedRuntimeFactory(eng *wasm.Engine) RuntimeFactory {
	return func(inst Installed) (Runtime, error) {
		if inst.Manifest.Ether == nil {
			return nil, fmt.Errorf("plugin %s: hosted plugins need an ether block", inst.ID)
		}
		decl := inst.Manifest.Ether.Runtime
		host := hostBridge{h: inst.Host}
		lim := HotLimits(inst.Manifest)
		if inst.RuntimeLimits != nil {
			l := inst.RuntimeLimits
			lim.MemoryBytes = l.MemoryBytes
			lim.Deadline, _ = time.ParseDuration(l.Deadline)
			lim.UnitBudget = l.UnitBudget
		}
		lim.Busy = inst.Busy
		switch {
		case decl.Module != "":
			module, ok := inst.Files(decl.Module)
			if !ok {
				return nil, fmt.Errorf("plugin %s: module %q is missing", inst.ID, decl.Module)
			}
			p, err := eng.LoadWASM(inst.ID, inst.Manifest.Version, module, host, lim)
			if err != nil {
				return nil, err
			}
			return &hostedRuntime{p: p}, nil
		case decl.Entry != "":
			src, ok := inst.Files(decl.Entry)
			if !ok {
				return nil, fmt.Errorf("plugin %s: entry %q is missing", inst.ID, decl.Entry)
			}
			js := string(src)
			if ext := strings.ToLower(path.Ext(decl.Entry)); ext == ".ts" || ext == ".tsx" || ext == ".mts" {
				out, _, diags := wasm.Compile(map[string]string{decl.Entry: js}, decl.Entry)
				if err := diagnosticsError(diags); err != nil {
					return nil, fmt.Errorf("plugin %s: %w", inst.ID, err)
				}
				js = string(out)
			}
			p, err := eng.LoadJS(inst.ID, inst.Manifest.Version, js, host, lim)
			if err != nil {
				return nil, err
			}
			return &hostedRuntime{p: p}, nil
		default:
			return nil, fmt.Errorf("plugin %s: hosted runtime needs an entry or a module", inst.ID)
		}
	}
}

func diagnosticsError(diags []wasm.Diagnostic) error {
	var msgs []string
	for _, d := range diags {
		if d.Severity == "error" {
			msgs = append(msgs, d.Text)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New("compile: " + strings.Join(msgs, "; "))
}

type hostedRuntime struct {
	p *wasm.Plugin
}

// EffectiveBudgets mirrors the engine's clamping: the budgets a manifest
// gets as SPEC 5.2 states them, in reference-machine time. The engine
// scales them by its measured speed (wasm.HotScale); Budgets reports that.
func EffectiveBudgets(lim wasm.Limits) HotBudgets {
	per, batch := lim.RankPerCandidate, lim.RankBatch
	if per <= 0 || per > wasm.DefaultRankPerCandidate {
		per = wasm.DefaultRankPerCandidate
	}
	if batch <= 0 || batch > wasm.DefaultRankBatch {
		batch = wasm.DefaultRankBatch
	}
	return HotBudgets{PerCandidate: per, PerBatch: batch}
}

// Budgets reports the limits in force on this machine: the manifest's,
// scaled by the engine's measured speed.
func (r *hostedRuntime) Budgets() HotBudgets {
	per, batch := r.p.Budgets()
	return HotBudgets{PerCandidate: per, PerBatch: batch}
}

func (r *hostedRuntime) Call(ctx context.Context, req Request) (Response, error) {
	q := make(map[string][]string, len(req.Query))
	for k, v := range req.Query {
		q[k] = []string{v}
	}
	headers := req.Headers
	if req.Locale != "" {
		headers = cloneHeaders(headers)
		headers["X-Ether-Locale"] = req.Locale
	}
	resp, err := r.p.Call(ctx, wasm.Request{Op: req.Op, ID: req.ID, Method: req.Method, Path: req.Path, Query: q, Headers: headers, Body: req.Body})
	if err != nil {
		return Response{}, err
	}
	return Response{Status: resp.Status, Headers: resp.Headers, Body: resp.Body}, nil
}

func (r *hostedRuntime) Close() error       { return r.p.Close() }
func (r *hostedRuntime) HasRankKey() bool   { return r.p.HasRankKey() }
func (r *hostedRuntime) HasCondMatch() bool { return r.p.HasCondMatch() }

func (r *hostedRuntime) RankBatch(ctx context.Context, cands []CandidateFacts, config json.RawMessage) ([]int64, HotOutcome, error) {
	in := make([]wasm.Candidate, len(cands))
	for i, c := range cands {
		facts, err := json.Marshal(c)
		if err != nil {
			return nil, HotOutcome{Status: string(wasm.StatusError)}, err
		}
		in[i] = wasm.Candidate{ReleaseID: c.ReleaseID, Facts: facts}
	}
	keys, out, err := r.p.RankBatch(ctx, in, config)
	return keys, hotOutcome(out), err
}

func (r *hostedRuntime) CondMatch(ctx context.Context, cand CandidateFacts, operator string, value json.RawMessage) (bool, HotOutcome, error) {
	facts, err := json.Marshal(cand)
	if err != nil {
		return false, HotOutcome{Status: string(wasm.StatusError)}, err
	}
	ok, out, err := r.p.CondMatch(ctx, wasm.Candidate{ReleaseID: cand.ReleaseID, Facts: facts}, operator, value)
	return ok, hotOutcome(out), err
}

func cloneHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h)+1)
	for k, v := range h {
		out[k] = v
	}
	return out
}

// Hot returns the hosted runtime of an enabled plugin when it exports the
// hot-path ABI, loading it if needed. It may block on a load, so it is for
// background work (replay, Studio); the play path uses HotReady.
func (m *Manager) Hot(ctx context.Context, pluginID string) (HotRuntime, bool) {
	r, err := m.Get(ctx, pluginID)
	if err != nil || !r.Enabled {
		return nil, false
	}
	rt, err := m.runtimeFor(ctx, r)
	if err != nil {
		return nil, false
	}
	hot, ok := rt.(HotRuntime)
	return hot, ok
}

// HotReady is the play-path accessor. It never blocks: it returns the
// plugin's hosted runtime only when it is already loaded, and otherwise
// starts loading it in the background and reports false, so the caller
// treats the plugin's steps and conditions as neutral for this resolve.
// Plugins in shadow mode are never ready on the play path. Runtimes are dropped on update, disable, remove and failure (closeRuntime)
// and warmed again when descriptors sync.
func (m *Manager) HotReady(pluginID string) (HotRuntime, bool) {
	m.mu.Lock()
	rt, ok := m.runtimes[pluginID]
	shadow := m.shadow[pluginID]
	m.mu.Unlock()
	if shadow {
		// A plugin in shadow mode never changes real picks; replay and the
		// shadow report show what it would have done.
		return nil, false
	}
	if !ok {
		m.warmAsync(pluginID)
		return nil, false
	}
	hot, ok := rt.(HotRuntime)
	return hot, ok
}

// prewarmHot loads, in the background, the runtime of an enabled plugin
// that contributes ranking steps or rule conditions, so the first resolve
// after start, install, update or enable finds it ready.
func (m *Manager) prewarmHot(r Record) {
	if !r.Enabled || r.Manifest.Ether == nil {
		return
	}
	c := r.Manifest.Ether.Contributes
	if len(c.RankingSteps) == 0 && len(c.RuleConditions) == 0 {
		return
	}
	m.warmAsync(r.ID)
}

// warmAsync loads one plugin's runtime once in the background.
func (m *Manager) warmAsync(pluginID string) {
	m.mu.Lock()
	if m.warming[pluginID] {
		m.mu.Unlock()
		return
	}
	m.warming[pluginID] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.warming, pluginID)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := m.Get(ctx, pluginID)
		if err != nil || !r.Enabled {
			return
		}
		if _, err := m.runtimeFor(ctx, r); err != nil {
			m.log.Warn("plugins: warm hot runtime", "plugin", pluginID, "err", err)
		}
	}()
}

// hostBridge maps the hosted runtime's dotted host calls (positional JSON
// argument arrays) onto the Host interface. Grant checks, journalling and
// shadow mode all live behind Host.
type hostBridge struct{ h Host }

func (b hostBridge) CallHost(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	if b.h == nil {
		return nil, wasm.Forbidden("no host services are configured for this plugin")
	}
	var args []json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, &wasm.Fault{Code: "bad_arguments", Message: "host arguments must be an array"}
		}
	}
	str := func(i int) string {
		var s string
		if i < len(args) {
			_ = json.Unmarshal(args[i], &s)
		}
		return s
	}
	num := func(i int) int64 {
		var n float64
		if i < len(args) {
			_ = json.Unmarshal(args[i], &n)
		}
		return int64(n)
	}
	result := func(v any, err error) (json.RawMessage, error) {
		if err != nil {
			return nil, mapHostError(err)
		}
		if v == nil {
			return json.RawMessage("null"), nil
		}
		if r, ok := v.(json.RawMessage); ok {
			return r, nil
		}
		out, err := json.Marshal(v)
		return out, err
	}
	switch name {
	case "http.fetchToCache", "cache.read", "archive.list", "archive.open":
		out, err := CallDataHost(ctx, b.h, name, raw)
		if err != nil {
			return nil, mapHostError(err)
		}
		return out, nil
	case "kv.get":
		v, ok, err := b.h.KVGet(ctx, str(0))
		if err != nil || !ok {
			return result(nil, err)
		}
		return result(string(v), nil)
	case "kv.put":
		var value string
		if len(args) > 1 {
			if err := json.Unmarshal(args[1], &value); err != nil {
				value = string(args[1]) // non-string values are stored as JSON text
			}
		}
		return result(nil, b.h.KVPut(ctx, str(0), []byte(value)))
	case "kv.delete":
		return result(nil, b.h.KVDelete(ctx, str(0)))
	case "kv.list":
		keys, err := b.h.KVList(ctx, str(0))
		return result(keys, err)
	case "settings.get":
		v, err := b.h.Settings(ctx)
		return result(v, err)
	case "secret.get":
		v, err := b.h.SecretGet(ctx, str(0))
		return result(v, err)
	case "http.fetch":
		var req struct {
			Method  string            `json:"method"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
			// BodyBase64 carries a binary request body; it wins over Body.
			BodyBase64 string `json:"bodyBase64"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args[0], &req); err != nil {
				return nil, &wasm.Fault{Code: "bad_arguments", Message: "http.fetch takes {method, url, headers, body}"}
			}
		}
		if req.Method == "" {
			req.Method = "GET"
		}
		body := []byte(req.Body)
		if req.BodyBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(req.BodyBase64)
			if err != nil {
				return nil, &wasm.Fault{Code: "bad_arguments", Message: "bodyBase64 is not valid base64"}
			}
			body = decoded
		}
		resp, err := b.h.HTTPFetch(ctx, FetchRequest{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: body})
		if err != nil {
			return nil, mapHostError(err)
		}
		return result(FetchResult(resp), nil)
	case "catalog.search":
		v, err := b.h.CatalogSearch(ctx, str(0), int(num(1)), int(num(2)))
		return result(v, err)
	case "catalog.get":
		v, err := b.h.CatalogGet(ctx, str(0))
		return result(v, err)
	case "addon.get":
		ah, ok := b.h.(interface {
			AddonGet(ctx context.Context, route string) (json.RawMessage, error)
		})
		if !ok {
			return nil, &wasm.Fault{Code: "unavailable", Message: "this plugin is not bound to an add-on"}
		}
		v, err := ah.AddonGet(ctx, str(0))
		return result(v, err)
	case "viewers.granted":
		v, err := b.h.ViewerGranted(ctx)
		return result(v, err)
	case "viewers.history":
		v, err := b.h.ViewerHistory(ctx, num(0), int(num(1)), int(num(2)))
		return result(v, err)
	case "viewers.setWatchState":
		var watched bool
		if len(args) > 2 {
			_ = json.Unmarshal(args[2], &watched)
		}
		return result(nil, b.h.SetWatchState(ctx, num(0), str(1), watched))
	case "collections.upsert":
		var col json.RawMessage
		if len(args) > 0 {
			col = args[0]
		}
		return result(nil, b.h.CollectionsUpsert(ctx, col))
	case "kinds.upsert":
		var items []core.KindItem
		if len(args) > 1 {
			if err := json.Unmarshal(args[1], &items); err != nil {
				return nil, &wasm.Fault{Code: "bad_arguments", Message: "kinds.upsert takes (kind, items[])"}
			}
		}
		accepted, err := b.h.KindUpsert(ctx, str(0), items)
		if err != nil {
			return nil, mapHostError(err)
		}
		return result(map[string]any{"accepted": accepted}, nil)
	case "kinds.delete":
		var ids []string
		if len(args) > 1 {
			if err := json.Unmarshal(args[1], &ids); err != nil {
				return nil, &wasm.Fault{Code: "bad_arguments", Message: "kinds.delete takes (kind, ids[])"}
			}
		}
		return result(nil, b.h.KindDelete(ctx, str(0), ids))
	case "kinds.items":
		page, err := b.h.KindItems(ctx, str(0), str(1), int(num(2)))
		return result(page, err)
	case "frontend.streamURL":
		u, err := b.h.StreamURL(ctx, str(0))
		if err != nil {
			return nil, mapHostError(err)
		}
		return result(map[string]any{"url": u}, nil)
	case "sources.resolve":
		var q core.SourceQuery
		if len(args) > 0 {
			if err := json.Unmarshal(args[0], &q); err != nil {
				return nil, &wasm.Fault{Code: "bad_arguments", Message: "sources.resolve takes a query object"}
			}
		}
		cands, err := b.h.ResolveSources(ctx, q)
		if err != nil {
			return nil, mapHostError(err)
		}
		if cands == nil {
			cands = []core.SourceCandidate{}
		}
		return result(map[string]any{"candidates": cands}, nil)
	case "cache.put":
		var body []byte
		if len(args) > 2 {
			var s string
			if err := json.Unmarshal(args[2], &s); err == nil {
				body = []byte(s)
			} else {
				var wrapped struct {
					Body       string `json:"body"`
					BodyBase64 string `json:"bodyBase64"`
				}
				if err := json.Unmarshal(args[2], &wrapped); err != nil {
					return nil, &wasm.Fault{Code: "bad_arguments", Message: "cache.put takes (libraryKey, name, data)"}
				}
				body = []byte(wrapped.Body)
				if wrapped.BodyBase64 != "" {
					decoded, err := base64.StdEncoding.DecodeString(wrapped.BodyBase64)
					if err != nil {
						return nil, &wasm.Fault{Code: "bad_arguments", Message: "bodyBase64 is not valid base64"}
					}
					body = decoded
				}
			}
		}
		ref, err := b.h.CachePut(ctx, str(0), str(1), bytes.NewReader(body))
		if err != nil {
			return nil, mapHostError(err)
		}
		return result(map[string]any{"blob": ref}, nil)
	case "cache.open":
		rc, err := b.h.CacheOpen(ctx, str(0))
		if err != nil {
			return nil, mapHostError(err)
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, wasm.MaxHostResultBytes+1))
		if err != nil {
			return nil, mapHostError(err)
		}
		if len(data) > wasm.MaxHostResultBytes {
			return nil, &wasm.Fault{Code: "host_error", Message: "cached blob exceeds host result size cap"}
		}
		out := map[string]any{}
		if utf8.Valid(data) {
			out["body"] = string(data)
		} else {
			out["body"] = ""
			out["bodyBase64"] = base64.StdEncoding.EncodeToString(data)
		}
		return result(out, nil)
	case "folders.list":
		v, err := b.h.FoldersList(ctx, str(0), str(1))
		return result(v, err)
	case "folders.hashStart":
		v, err := b.h.FoldersHashStart(ctx, str(0), str(1))
		return result(v, err)
	case "folders.hashPoll":
		v, err := b.h.FoldersHashPoll(ctx, str(0))
		return result(v, err)
	case "folders.hashCancel":
		v, err := b.h.FoldersHashCancel(ctx, str(0))
		return result(v, err)
	case "folders.hash":
		v, err := b.h.FoldersHash(ctx, str(0), str(1))
		return result(v, err)
	case "folders.stat":
		v, err := b.h.FoldersStat(ctx, str(0), str(1))
		return result(v, err)
	case "notify.owner":
		return result(nil, b.h.NotifyOwner(ctx, str(0)))
	case "log":
		level, msg := str(0), str(1)
		if len(args) == 1 {
			level, msg = "info", str(0)
		}
		var attrs map[string]any
		if len(args) > 2 {
			_ = json.Unmarshal(args[2], &attrs)
		}
		return result(nil, b.h.Log(ctx, level, msg, attrs))
	default:
		return nil, wasm.Forbidden("unknown host service " + name)
	}
}

func mapHostError(err error) error {
	var input *CollectionInputError
	var valid *ValidationError
	switch {
	case errors.As(err, &input):
		return &wasm.Fault{Code: string(wasm.StatusInvalidInput), Message: input.Message}
	case errors.As(err, &valid):
		return &wasm.Fault{Code: string(wasm.StatusInvalidInput), Message: valid.Error()}
	case errors.Is(err, ErrPermissionDenied):
		return wasm.Forbidden(err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return &wasm.Fault{Code: string(wasm.StatusTimeout), Message: "host call timed out"}
	default:
		return &wasm.Fault{Code: "host_error", Message: err.Error()}
	}
}

// FetchResult is the guest-facing http.fetch answer. body is the response
// as text (JSON, HTML, playlists); bodyBase64 carries the exact bytes only
// for responses that are not valid UTF-8 (archives, images), so a large
// text response is not sent twice into guest memory.
func FetchResult(resp FetchResponse) map[string]any {
	out := map[string]any{"status": resp.Status, "headers": resp.Headers}
	if utf8.Valid(resp.Body) {
		out["body"] = string(resp.Body)
	} else {
		out["body"] = ""
		out["bodyBase64"] = base64.StdEncoding.EncodeToString(resp.Body)
	}
	return out
}

// RuntimeLimitsFor returns the granted hosted resources, also used by Studio.
func RuntimeLimitsFor(m Manifest) wasm.Limits {
	return runtimeLimitsWithCeilings(m, defaultRuntimeCeilings())
}

// GrantedRuntimeLimits reports the default-policy grant for standalone tools.
func GrantedRuntimeLimits(m Manifest) *RuntimeLimits { return (&Manager{}).grantedRuntimeLimits(m) }
