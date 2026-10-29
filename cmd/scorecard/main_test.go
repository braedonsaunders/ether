package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
