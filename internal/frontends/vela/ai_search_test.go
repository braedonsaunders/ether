package vela

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func (f *aiSearchFake) AISearchVisible(ctx context.Context, v *frontend.Viewer, keys []frontend.RatingKey) ([]frontend.Title, error) {
	var out []frontend.Title
	for _, key := range keys {
		detail, err := f.Item(ctx, frontend.ItemRequest{Viewer: v, Key: key})
		if errors.Is(err, frontend.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, detail.Title)
	}
	return out, nil
}

func (f *aiSearchFake) AISearchAvailable() bool { return f.available }
func (f *aiSearchFake) AISearch(ctx context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	if q.Limit != 50 || !q.NoTotal {
		return frontend.SearchPage{}, errors.New("unbounded query")
	}
	return f.refine(ctx)
}
func (f *aiSearchFake) AISearchProgress(ctx context.Context, v *frontend.Viewer, d frontend.Device, query string, q frontend.ItemQuery, publish func(frontend.SearchPage)) (frontend.SearchPage, error) {
	publish(frontend.SearchPage{Page: frontend.Page{Items: []frontend.Title{{Key: 3, Title: "Provisional"}}}, Descriptive: &frontend.DescriptiveSearchStatus{State: frontend.DescriptivePartial, RetryAfterSeconds: 2}, Continuation: frontend.ContinuationUnknown})
	publish(frontend.SearchPage{Page: frontend.Page{Items: []frontend.Title{{Key: 3, Title: "Provisional"}, {Key: 4, Title: "Second result"}}}, Descriptive: &frontend.DescriptiveSearchStatus{State: frontend.DescriptivePartial, RetryAfterSeconds: 2}, Continuation: frontend.ContinuationUnknown})
	return f.AISearch(ctx, v, d, query, q)
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
	var decorations atomic.Int32
	source.UserDataFunc = func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		decorations.Add(1)
		return nil, nil
	}
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
		deadline := time.Now().Add(3 * time.Second)
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
	// The same job exposes provisional cards and remains nonterminal.
	deadline := time.Now().Add(time.Second)
	for {
		provisional := decode(request(`{}`, id, "device", i.pollAISearch))
		if provisional["status"] == "partial" {
			results := provisional["results"].(map[string]any)
			descriptive := results["descriptive"].(map[string]any)
			if descriptive["state"] != "partial" || descriptive["retry"].(map[string]any)["allowed"] != true {
				t.Fatal(provisional)
			}
			if len(results["items"].([]any)) == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("second partial was not visible before completion", provisional)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait(id)
	preparedDecorations := decorations.Load()
	pollStarted := time.Now()
	w = request(`{}`, id, "device", i.pollAISearch)
	t.Logf("fake completed GET=%s", time.Since(pollStarted))
	result := decode(w)
	if w.Code != 200 || result["status"] != "complete" {
		t.Fatal(w.Code, result)
	}
	results := result["results"].(map[string]any)
	if results["generation"] != float64(7) || len(results["items"].([]any)) != 1 || results["items"].([]any)[0].(map[string]any)["name"] != "Refined" {
		t.Fatal(results)
	}
	pollStarted = time.Now()
	cached := request(`{}`, id, "device", i.pollAISearch)
	t.Logf("fake cached GET=%s", time.Since(pollStarted))
	if cached.Code != 200 || decorations.Load() != preparedDecorations {
		t.Fatalf("cached reads repeated result decoration: count=%d status=%d", decorations.Load(), cached.Code)
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
	var attempts atomic.Int32
	source.refine = func(context.Context) (frontend.SearchPage, error) {
		if attempts.Add(1) == 1 {
			return frontend.SearchPage{Descriptive: &frontend.DescriptiveSearchStatus{State: frontend.DescriptiveBusy, RetryAfterSeconds: 1}}, nil
		}
		return frontend.SearchPage{Descriptive: &frontend.DescriptiveSearchStatus{State: frontend.DescriptiveReady}}, nil
	}
	w = request(`{"query":"contention","generation":9}`, "", "device", i.startAISearch)
	id = decode(w)["id"].(string)
	wait(id)
	if got := decode(request(`{}`, id, "device", i.pollAISearch)); got["status"] != "complete" || attempts.Load() != 2 {
		t.Fatalf("temporary contention became terminal: %v", got)
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
