package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestInfuseDirectResumeNeverNeedsDecision(t *testing.T) {
	t.Setenv("ETHER_PARITY_EXPECT_EARLY_RESUME", "1")
	var events []string
	offset := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Product") != "Infuse" || r.Header.Get("X-Plex-Platform") != "tvOS" {
			t.Errorf("missing Infuse client headers")
		}
		events = append(events, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/decision") {
			t.Error("Infuse direct playback requested a decision")
		}
		if r.URL.Path == "/library/sections/1/all" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"456","title":"Fixture Movie 004","type":"movie"}]}}`)
			return
		}
		if r.URL.Path == "/:/unscrobble" {
			offset = 0
		}
		if r.URL.Path == "/:/timeline" {
			offset, _ = strconv.Atoi(r.URL.Query().Get("time"))
			if offset < 60000 {
				offset = 0
			}
		}
		if r.URL.Path == "/library/metadata/456" {
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"456","viewOffset":%d}]}}`, offset)
			return
		}
		if r.URL.Path == "/hubs/continueWatching" {
			if offset > 0 {
				fmt.Fprint(w, `{"MediaContainer":{"Hub":[{"Metadata":[{"ratingKey":"456"}]}]}}`)
			} else {
				fmt.Fprint(w, `{"MediaContainer":{"Hub":[]}}`)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/library/parts/") {
			if r.Header.Get("Range") != "bytes=0-4095" {
				t.Error("missing direct range")
			}
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, strings.Repeat("x", 4096))
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{}}`)
	}))
	defer s.Close()
	captures, err := infuseDirectResume(context.Background(), s.Client(), s.URL, http.Header{}, map[string]string{"movieKey": "123", "movieSection": "1"})
	if err != nil || len(captures) != 25 || len(events) != 25 {
		t.Fatalf("captures=%d requests=%d err=%v", len(captures), len(events), err)
	}
	for i := 1; i < len(events); i += 6 {
		if events[i+1] != "GET /library/parts/456/1792324800/file.mp4" || events[i+2] != "POST /:/timeline" || events[i+3] != "POST /:/timeline" {
			t.Fatalf("wrong direct/timeline sequence: %v", events[i:i+6])
		}
	}
}

func TestInfuseDirectResumeRejectsMissingContinueItem(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/sections/1/all":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"456","title":"Fixture Movie 004","type":"movie"}]}}`)
		case strings.HasPrefix(r.URL.Path, "/library/parts/"):
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, strings.Repeat("x", 4096))
		case r.URL.Path == "/library/metadata/456":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"456","viewOffset":61000}]}}`)
		case r.URL.Path == "/hubs/continueWatching":
			fmt.Fprint(w, `{"MediaContainer":{"Hub":[]}}`)
		default:
			fmt.Fprint(w, `{"MediaContainer":{}}`)
		}
	}))
	defer s.Close()
	if _, err := infuseDirectResume(context.Background(), s.Client(), s.URL, http.Header{}, map[string]string{"movieKey": "123", "movieSection": "1"}); err == nil {
		t.Fatal("missing Continue Watching item was accepted")
	}
}
