package main

import (
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "secret" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer s.Close()
	target := target{URL: s.URL, Headers: map[string]string{"X-Test": "secret"}}
	rows, err := run(config{Samples: 12, Concurrency: []int{1, 10}, Scenarios: []scenario{{Name: "home", Reference: target, Ether: target}}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", rows, err)
	}
	for _, r := range rows {
		if r.Reference.Errors != 0 || r.Ether.Errors != 0 || r.Reference.P95 < r.Reference.P50 {
			t.Fatalf("bad measurement: %+v", r)
		}
	}
}
func TestFailuresAreInconclusive(t *testing.T) {
	if winner(measurement{Errors: 1}, measurement{}) != "inconclusive" {
		t.Fatal("failed requests must not win")
	}
	if percentile([]float64{1, 2, 3, 4}, 95) != 4 {
		t.Fatal("wrong percentile")
	}
}

func TestMeasuresFirstByteSeparatelyFromBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte("payload"))
	}))
	defer s.Close()
	m := measure(s.Client(), target{URL: s.URL}, 3, 1)
	if m.Errors != 0 || m.TTFBP50 <= 0 || m.P50-m.TTFBP50 < 15 {
		t.Fatalf("expected distinct first-byte and body timing: %+v", m)
	}
}

func TestSchemaValidationChecksEachResponseAndBlocksWinner(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.3","paths":{"/Items/{id}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","required":["Id"],"properties":{"Id":{"type":"string"}}}}}}}}}}}`)
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	m := measureWithSchema(srv.Client(), target{URL: srv.URL, SpecPath: "/Items/123"}, 4, 2, doc)
	if m.Errors != 0 || m.SchemaErrors != 4 {
		t.Fatalf("missing required field was not counted: %+v", m)
	}
	if winner(measurement{}, m) != "inconclusive" {
		t.Fatal("invalid response must not win latency")
	}
}

func TestSchemaUndocumentedOperationIsUncovered(t *testing.T) {
	doc, err := openapi.Parse([]byte(`{"openapi":"3.0.3","paths":{"/known":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer s.Close()
	m := measureWithSchema(s.Client(), target{URL: s.URL, SpecPath: "/undocumented"}, 3, 1, doc)
	if m.SchemaUncovered != 3 || m.SchemaErrors != 0 {
		t.Fatalf("undocumented operation misclassified: %+v", m)
	}
	if winner(measurement{}, m) != "inconclusive" {
		t.Fatal("uncovered schema must not win")
	}
}

func TestOpenLoopAccountsForQueueAndTimeouts(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer s.Close()
	m := measureScheduled(s.Client(), target{URL: s.URL}, 6, 1, nil, 1000)
	if m.Errors != 0 || m.QueueP95 < 30 || m.AllScheduledP95 <= m.P95 || m.HTTPSuccesses != 6 {
		t.Fatalf("offered load/queue lost: %+v", m)
	}
	client := &http.Client{Timeout: 5 * time.Millisecond}
	m = measureScheduled(client, target{URL: s.URL}, 2, 1, nil, 1000)
	if m.Timeouts != 2 || m.Errors != 2 || m.AllScheduledP99 < 4 || m.ValidSuccesses != 0 {
		t.Fatalf("timeouts omitted: %+v", m)
	}
}
func TestRepetitionsAlternateServerOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			_, _ = w.Write([]byte("ok"))
		}))
	}
	a, b := makeServer("reference"), makeServer("ether")
	defer a.Close()
	defer b.Close()
	rows, err := run(config{Samples: 1, Concurrency: []int{1}, Repetitions: 2, Scenarios: []scenario{{Name: "views", Reference: target{URL: a.URL}, Ether: target{URL: b.URL}}}})
	if err != nil || len(rows) != 2 || strings.Join(order, ",") != "reference,ether,ether,reference" || rows[1].Order != "ether_first" {
		t.Fatalf("order %v rows %+v err %v", order, rows, err)
	}
}
