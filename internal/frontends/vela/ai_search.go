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
		fail(w, err)
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
		fail(w, err)
		return
	}
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		fail(w, err)
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
		if old.status == "queued" || old.status == "running" {
			old.status = "cancelled"
			old.cancel()
		}
	}
	// Admission never spawns a goroutine that merely waits for capacity.
	activeForScope := 0
	for _, j := range i.aiJobs {
		if j.scope == scope && (j.status == "running" || j.status == "queued") {
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
		page, e := source.AISearch(ctx, v, d, j.query, frontend.ItemQuery{Limit: 50, NoTotal: true})
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
		page.Page.Items = append([]frontend.Title(nil), page.Page.Items[:min(50, len(page.Page.Items))]...)
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
		out["error"] = object{"class": class, "code": code, "message": strings.ReplaceAll(code, "_", " "), "retry": object{"allowed": false}}
	}
	return out
}

func (i *instance) pollAISearch(w http.ResponseWriter, r *http.Request)   { i.readAISearch(w, r, false) }
func (i *instance) cancelAISearch(w http.ResponseWriter, r *http.Request) { i.readAISearch(w, r, true) }
func (i *instance) readAISearch(w http.ResponseWriter, r *http.Request, cancel bool) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err)
		return
	}
	if !i.aiSearchAvailable() {
		i.unavailable(w, r)
		return
	}
	scope, err := i.aiSearchScope(r, v, token)
	if err != nil {
		fail(w, err)
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
	if cancel && (j.status == "queued" || j.status == "running") {
		j.status = "cancelled"
		j.cancel()
	}
	wire := aiJobWire(j)
	page, query, generation := j.page, j.query, j.generation
	i.aiMu.Unlock()
	if !i.aiSearchAvailable() {
		i.unavailable(w, r)
		return
	}
	if wire["status"] == "complete" {
		r = r.WithContext(i.host.Experience.NoProviderWait(r.Context()))
		// Revalidate the bounded result prefix under today's viewer policy. Job
		// creation is never a grant to keep reading a subsequently hidden title.
		items := make([]frontend.Title, 0, len(page.Page.Items))
		for _, title := range page.Page.Items {
			detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: title.Key})
			if errors.Is(e, frontend.ErrNotFound) {
				continue
			}
			if e != nil {
				fail(w, e)
				return
			}
			items = append(items, detail.Title)
		}
		titles, e := i.titles(r, v, token, items)
		if e != nil {
			fail(w, e)
			return
		}
		continuation := page.Continuation
		if continuation == "" || continuation == frontend.ContinuationMore {
			continuation = frontend.ContinuationUnknown
		}
		wire["results"] = object{"query": query, "generation": generation, "items": titles, "continuation": continuation}
	}
	send(w, 200, wire)
}
