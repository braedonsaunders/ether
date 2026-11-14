package vela

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type aiSearchFake struct {
	frontendtest.Experience
	available bool
	refine    func(context.Context) (frontend.SearchPage, error)
}

func (f *aiSearchFake) AISearchAvailable() bool { return f.available }
func (f *aiSearchFake) AISearch(ctx context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	if q.Limit != 50 || !q.NoTotal {
		return frontend.SearchPage{}, errors.New("unbounded query")
	}
	return f.refine(ctx)
}
func (f *aiSearchFake) DescriptiveSearchAvailable() bool { return f.available }
func (f *aiSearchFake) DescriptiveSearch(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.SearchPage, error) {
	return frontend.SearchPage{Continuation: frontend.ContinuationUnknown}, nil
}

func checkAISearch(t *testing.T) {
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	release := make(chan struct{})
	source := &aiSearchFake{available: true}
	source.refine = func(ctx context.Context) (frontend.SearchPage, error) {
		select {
		case <-ctx.Done():
			return frontend.SearchPage{}, ctx.Err()
		case <-release:
		}
		return frontend.SearchPage{Page: frontend.Page{Items: []frontend.Title{{Key: 1, Title: "Refined"}, {Key: 2, Title: "Now hidden"}}}, Continuation: frontend.ContinuationUnknown}, nil
	}
	source.ItemFunc = func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		if r.Key == 2 {
			return frontend.ItemDetail{}, frontend.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: r.Key, Title: "Refined"}}, nil
	}
	i := &instance{ctx: ctx, host: frontend.Host{Identity: identityFake{}, Experience: source}}
	request := func(raw, id, device string, h http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/vela/v0/search/ai", nil)
		r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, json.RawMessage(raw)))
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", device)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	wait := func(id string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			i.aiMu.Lock()
			state := i.aiJobs[id].status
			i.aiMu.Unlock()
			if state == "complete" || state == "failed" {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("job did not finish")
	}
	for _, available := range []bool{false, true} {
		source.available = available
		features := i.capabilities()["features"].(object)
		for _, name := range []string{"aiSearch", "descriptiveSearch"} {
			if features[name].(object)["available"] != available {
				t.Fatalf("%s availability", name)
			}
		}
	}
	w := request(`{"query":"cozy cute girls night in","generation":7}`, "", "device", i.startAISearch)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	job := decode(w)
	id := job["id"].(string)
	if job["status"] != "queued" || job["generation"] != float64(7) {
		t.Fatal(job)
	}
	// A live refinement cannot delay ordinary or cached descriptive search.
	fast := request(`{"query":"title","mode":"fast","generation":8}`, "", "device", i.search)
	if fast.Code != 200 {
		t.Fatal(fast.Body.String())
	}
	if got := request(`{}`, id, "other-device", i.pollAISearch); got.Code != 404 {
		t.Fatal("cross-device job", got.Code)
	}
	close(release)
	wait(id)
	w = request(`{}`, id, "device", i.pollAISearch)
	result := decode(w)
	if w.Code != 200 || result["status"] != "complete" {
		t.Fatal(w.Code, result)
	}
	results := result["results"].(map[string]any)
	if results["generation"] != float64(7) || len(results["items"].([]any)) != 1 || results["items"].([]any)[0].(map[string]any)["name"] != "Refined" {
		t.Fatal(results)
	}
	source.refine = func(context.Context) (frontend.SearchPage, error) {
		return frontend.SearchPage{}, errors.New("secret provider failure")
	}
	w = request(`{"query":"failure","generation":9}`, "", "device", i.startAISearch)
	id = decode(w)["id"].(string)
	wait(id)
	failed := decode(request(`{}`, id, "device", i.pollAISearch))
	if failed["status"] != "failed" || failed["results"] != nil || failed["error"].(map[string]any)["code"] != "ai_search_failed" {
		t.Fatal(failed)
	}
	if got := request(`{"query":"title","mode":"fast"}`, "", "device", i.search); got.Code != 200 {
		t.Fatal("model failure affected fast lane")
	}
	source.refine = func(ctx context.Context) (frontend.SearchPage, error) {
		<-ctx.Done()
		return frontend.SearchPage{}, ctx.Err()
	}
	w = request(`{"query":"cancel","generation":10}`, "", "device", i.startAISearch)
	id = decode(w)["id"].(string)
	if got := request(`{"generation":11}`, id, "device", i.cancelAISearch); got.Code != 409 {
		t.Fatal("wrong generation accepted")
	}
	cancelled := decode(request(`{"generation":10}`, id, "device", i.cancelAISearch))
	if cancelled["status"] != "cancelled" || cancelled["results"] != nil {
		t.Fatal(cancelled)
	}
	source.available = false
	if got := request(`{"query":"disabled"}`, "", "device", i.startAISearch); got.Code != 403 {
		t.Fatal("disabled AI", got.Code)
	}
	if got := request(`{"query":"disabled"}`, "", "", i.startAISearch); got.Code != 401 {
		t.Fatal("auth precedence", got.Code)
	}
}
