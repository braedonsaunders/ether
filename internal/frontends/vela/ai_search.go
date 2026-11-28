package vela

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Jobs are bounded, lazy and short lived. Only the shared service caches model
// rules; this adapter retains a bounded result prefix for an authenticated job.
type aiSearchJob struct {
	id, scope, query, status string
	generation               int
	page                     frontend.SearchPage
	titles                   []object
	err                      error
	cancel                   context.CancelFunc
	expires                  time.Time
}

func (i *instance) aiSearchAvailable() bool {
	source, ok := i.host.Experience.(frontend.AISearcher)
	return ok && source.AISearchAvailable()
}

func (i *instance) aiSearchScope(r *http.Request, v *frontend.Viewer, token string) (string, error) {
	family, err := i.grantScope(r.Context(), token, requestDevice(r))
	if err != nil {
		return "", err
	}
	return hashJSON([]any{family, v.ID, requestDevice(r).ID}), nil
}

func (i *instance) startAISearch(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	if !i.aiSearchAvailable() {
		i.unavailable(w, r)
		return
	}
	var b struct {
		Query, PreviousJobID string
		Generation           int
	}
	if body(r, &b) != nil || strings.TrimSpace(b.Query) == "" || len(b.Query) > 4096 || b.Generation < 0 {
		problem(w, 400, "request", "invalid_request")
		return
	}
	scope, err := i.aiSearchScope(r, v, token)
	if err != nil {
		fail(w, err, r)
		return
	}
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		fail(w, err, r)
		return
	}
	now := time.Now()
	i.aiMu.Lock()
	if i.aiJobs == nil {
		i.aiJobs = make(map[string]*aiSearchJob)
	}
	for id, j := range i.aiJobs {
		if now.After(j.expires) {
			j.cancel()
			delete(i.aiJobs, id)
		}
	}
	if b.PreviousJobID != "" {
		old := i.aiJobs[b.PreviousJobID]
		if old == nil || old.scope != scope {
			i.aiMu.Unlock()
			problem(w, 404, "not-found", "not_found")
			return
		}
		if old.status == "queued" || old.status == "running" || old.status == "partial" {
			old.status = "cancelled"
			old.cancel()
		}
	}
	// Admission never spawns a goroutine that merely waits for capacity.
	activeForScope := 0
	for _, j := range i.aiJobs {
		if j.scope == scope && (j.status == "running" || j.status == "queued" || j.status == "partial") {
			activeForScope++
		}
	}
	if i.aiActive >= 4 || activeForScope >= 2 || len(i.aiJobs) >= 128 {
		i.aiMu.Unlock()
		problem(w, 503, "capacity", "ai_search_capacity")
		return
	}
	// Preserve request metadata but detach the client connection. Shutdown and
	// the job deadline still cancel all model/catalog work.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	stop := context.AfterFunc(i.ctx, cancel)
	j := &aiSearchJob{id: base64.RawURLEncoding.EncodeToString(nonce[:]), scope: scope, query: b.Query, generation: b.Generation, status: "queued", cancel: cancel, expires: now.Add(10 * time.Minute)}
	i.aiJobs[j.id] = j
	i.aiActive++
	wire := aiJobWire(j)
	i.aiMu.Unlock()
	source := i.host.Experience.(frontend.AISearcher)
	d := requestDevice(r)
	go func() {
		defer cancel()
		defer stop()
		i.aiMu.Lock()
		if j.status == "cancelled" {
			i.aiActive--
			i.aiMu.Unlock()
			return
		}
		j.status = "running"
		i.aiMu.Unlock()
		var page frontend.SearchPage
		var e error
		// Shared descriptive workers can be occupied by superseded queries.
		// Keep this admitted job alive through temporary contention, bounded
		// by its existing deadline and cancellation; never report busy as a
		// model failure. There are at most four admitted jobs.
		for {
			if progressive, ok := source.(frontend.ProgressiveAISearcher); ok {
				page, e = progressive.AISearchProgress(ctx, v, d, j.query, frontend.ItemQuery{Limit: 50, NoTotal: true}, func(partial frontend.SearchPage) {
					if ctx.Err() != nil {
						return
					}
					partial.Page.Items = append([]frontend.Title(nil), partial.Page.Items[:min(50, len(partial.Page.Items))]...)
					cards, err := i.titles(r.WithContext(i.host.Experience.NoProviderWait(ctx)), v, token, partial.Page.Items)
					if err != nil {
						return
					} // The final projection still reports failures.
					i.aiMu.Lock()
					if j.status == "running" || j.status == "partial" {
						j.page, j.titles, j.status = partial, cards, "partial"
					}
					i.aiMu.Unlock()
				})
			} else {
				page, e = source.AISearch(ctx, v, d, j.query, frontend.ItemQuery{Limit: 50, NoTotal: true})
			}
			if e != nil || page.Descriptive == nil || (page.Descriptive.State != frontend.DescriptiveBusy && page.Descriptive.State != frontend.DescriptivePending && page.Descriptive.State != frontend.DescriptivePartial) {
				break
			}
			delay := time.Duration(max(1, min(30, page.Descriptive.RetryAfterSeconds))) * time.Second
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				e = ctx.Err()
			case <-timer.C:
			}
			if e != nil {
				break
			}
		}

		var titles []object
		if e == nil && (page.Descriptive == nil || page.Descriptive.State == frontend.DescriptiveReady) {
			page.Page.Items = append([]frontend.Title(nil), page.Page.Items[:min(50, len(page.Page.Items))]...)
			// User state, ratings and artwork grants are assembled once by the
			// job. GET only authorizes and filters these immutable cards.
			titles, e = i.titles(r.WithContext(i.host.Experience.NoProviderWait(ctx)), v, token, page.Page.Items)
		}
		if e == nil {
			e = ctx.Err()
		}
		i.aiMu.Lock()
		defer i.aiMu.Unlock()
		i.aiActive--
		if j.status == "cancelled" {
			return
		}
		if e != nil {
			j.status = "failed"
			j.err = e
			return
		}
		if page.Descriptive != nil && page.Descriptive.State != frontend.DescriptiveReady {
			j.page = page
			j.status = "failed"
			return
		}
		j.titles = titles
		j.page = page
		j.status = "complete"
	}()
	send(w, 200, wire)
}

func aiJobWire(j *aiSearchJob) object {
	out := object{"id": j.id, "generation": j.generation, "status": j.status}
	if j.status == "failed" {
		class, code := "transient", "ai_search_failed"
		if errors.Is(j.err, frontend.ErrNotAvailable) {
			class, code = "policy", "feature_unavailable"
		}
		retry := object{"allowed": true, "afterSeconds": 60}
		if errors.Is(j.err, frontend.ErrNotAvailable) {
			retry = object{"allowed": false}
		}
		if j.page.Descriptive != nil {
			retry = descriptiveWire(j.page.Descriptive)["retry"].(object)
			if j.page.Descriptive.State == frontend.DescriptiveBusy {
				class, code = "capacity", "ai_search_busy"
			}
		}
		out["error"] = object{"class": class, "code": code, "message": strings.ReplaceAll(code, "_", " "), "retry": retry}
	}
	return out
}

func (i *instance) pollAISearch(w http.ResponseWriter, r *http.Request)   { i.readAISearch(w, r, false) }
func (i *instance) cancelAISearch(w http.ResponseWriter, r *http.Request) { i.readAISearch(w, r, true) }
func (i *instance) readAISearch(w http.ResponseWriter, r *http.Request, cancel bool) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	if !i.aiSearchAvailable() {
		i.unavailable(w, r)
		return
	}
	scope, err := i.aiSearchScope(r, v, token)
	if err != nil {
		fail(w, err, r)
		return
	}
	var b struct{ Generation int }
	if cancel && (body(r, &b) != nil || b.Generation < 0) {
		problem(w, 400, "request", "invalid_request")
		return
	}
	i.aiMu.Lock()
	j := i.aiJobs[r.PathValue("id")]
	if j == nil || j.scope != scope || time.Now().After(j.expires) {
		i.aiMu.Unlock()
		problem(w, 404, "not-found", "not_found")
		return
	}
	if cancel && b.Generation != j.generation {
		i.aiMu.Unlock()
		problem(w, 409, "conflict", "generation_mismatch")
		return
	}
	if cancel && (j.status == "queued" || j.status == "running" || j.status == "partial") {
		j.status = "cancelled"
		j.cancel()
	}
	wire := aiJobWire(j)
	page, query, generation, cachedTitles := j.page, j.query, j.generation, j.titles
	i.aiMu.Unlock()
	if !i.aiSearchAvailable() {
		i.unavailable(w, r)
		return
	}
	if wire["status"] == "complete" || wire["status"] == "partial" {
		r = r.WithContext(i.host.Experience.NoProviderWait(r.Context()))
		// Recheck today's policy in one local batch. Never rerun scoring,
		// detail enrichment, user-data decoration or artwork signing here.
		keys := make([]frontend.RatingKey, 0, len(page.Page.Items))
		for _, title := range page.Page.Items {
			keys = append(keys, title.Key)
		}
		visible, e := i.host.Experience.(frontend.AISearcher).AISearchVisible(r.Context(), v, keys)
		if e != nil {
			fail(w, e, r)
			return
		}
		allowed := make(map[frontend.RatingKey]bool, len(visible))
		for _, title := range visible {
			allowed[title.Key] = true
		}
		titles := make([]object, 0, len(cachedTitles))
		for idx, title := range page.Page.Items {
			if allowed[title.Key] {
				titles = append(titles, cachedTitles[idx])
			}
		}
		continuation := page.Continuation
		if continuation == "" || continuation == frontend.ContinuationMore {
			continuation = frontend.ContinuationUnknown
		}
		wire["results"] = object{"query": query, "generation": generation, "items": titles, "continuation": continuation, "descriptive": descriptiveWire(page.Descriptive)}
	}
	send(w, 200, wire)
}
