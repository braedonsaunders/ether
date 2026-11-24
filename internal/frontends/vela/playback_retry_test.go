package vela

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func retryPlayPlan() frontend.PlayPlan {
	return frontend.PlayPlan{
		ID:         "plan-1",
		Media:      frontend.MediaInfo{Container: "mp4", Duration: 2 * time.Hour},
		Decision:   frontend.Decision{Kind: frontend.DecisionDirectPlay, AudioStream: -1, SubtitleStream: -1, Reason: "direct_play"},
		DirectPlay: true,
		ETag:       "etag-1",
	}
}

// A refusal with Retry-After on POST /playback asks the client to try again,
// and the client retries with the same idempotency key. The answer must
// therefore not be kept as that key's stored reply, or every retry gets the
// old refusal back without Plan running again.
func TestVelaPlaybackRetryAfterIsNotReplayed(t *testing.T) {
	movieKey := frontend.MovieKey(603)
	movieGUID := ids.RatingKeyGUID(movieKey)

	cases := []struct {
		name       string
		err        error
		status     int
		retryAfter string
		code       string
	}{
		{"preparing", &frontend.PreparingError{RetryAfter: 5 * time.Second}, 503, "5", `"preparing"`},
		{"prepare slow", frontend.ErrPrepareSlow, 503, "2", `"preparation_pending"`},
		{"session busy", &frontend.SessionBusyError{RetryAfter: 3 * time.Second}, 503, "3", `"playback_capacity"`},
		{"track source unavailable", frontend.ErrTrackSourceUnavailable, 502, "2", `"track_source_unavailable"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &versionsSessionsFake{}
			handler := mountVersionsVela(t, movieKey, &versionsEditionsFake{}, &versionsHandlesFake{item: movieKey}, sess)

			planCalls := 0
			failing := true
			sess.planFunc = func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
				planCalls++
				if failing {
					return frontend.PlayPlan{}, tc.err
				}
				return retryPlayPlan(), nil
			}
			body := `{"idempotencyKey":"play-retry","titleId":"` + movieGUID + `","client":{"device":{"id":"dev-1"}}}`
			post := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, velaReq("POST", "/vela/v0/playback", body))
				return w
			}

			w := post()
			if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retryAfter || !strings.Contains(w.Body.String(), tc.code) || !strings.Contains(w.Body.String(), `"allowed":true`) {
				t.Fatalf("first attempt = %d Retry-After=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}

			// The same key once the source is ready reaches Plan again and plays.
			failing = false
			w = post()
			if w.Code != 200 || planCalls != 2 {
				t.Fatalf("retry with the same key = %d planCalls=%d body=%s", w.Code, planCalls, w.Body.String())
			}
			first := w.Body.String()

			// The successful answer is still the key's stored reply.
			w = post()
			if w.Code != 200 || planCalls != 2 || w.Body.String() != first {
				t.Fatalf("replay of the success = %d planCalls=%d body=%s", w.Code, planCalls, w.Body.String())
			}
		})
	}
}

// The broker refuses a start for lack of a start slot in StartStream, after
// Open. The handler stops the opened session, and the same key then plays.
func TestVelaPlaybackStartStreamBusyIsNotReplayed(t *testing.T) {
	movieKey := frontend.MovieKey(603)
	movieGUID := ids.RatingKeyGUID(movieKey)
	sess := &versionsSessionsFake{}
	handler := mountVersionsVela(t, movieKey, &versionsEditionsFake{}, &versionsHandlesFake{item: movieKey}, sess)

	planCalls := 0
	sess.planFunc = func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		planCalls++
		return retryPlayPlan(), nil
	}
	busy := true
	sess.StartStreamFunc = func(_ context.Context, _ frontend.SessionAccess, id string, _ frontend.StreamRequest) (string, error) {
		if busy {
			return "", &frontend.SessionBusyError{RetryAfter: time.Second}
		}
		return "/sessions/media/" + id + "/file", nil
	}
	var stopped []string
	sess.StopFunc = func(_ context.Context, _ frontend.SessionAccess, id string) error {
		stopped = append(stopped, id)
		return nil
	}
	body := `{"idempotencyKey":"play-busy","titleId":"` + movieGUID + `","client":{"device":{"id":"dev-1"}}}`
	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, velaReq("POST", "/vela/v0/playback", body))
		return w
	}

	w := post()
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"playback_capacity"`) || !strings.Contains(w.Body.String(), `"allowed":true`) {
		t.Fatalf("first attempt = %d Retry-After=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if len(stopped) != 1 || stopped[0] != "sess-1" {
		t.Fatalf("opened session not stopped after the refused start: stopped=%v", stopped)
	}

	busy = false
	w = post()
	if w.Code != 200 || planCalls != 2 || len(stopped) != 1 {
		t.Fatalf("retry with the same key = %d planCalls=%d stopped=%v body=%s", w.Code, planCalls, stopped, w.Body.String())
	}
}

// A 503 without Retry-After is not a retry invitation and stays stored.
func TestVelaPlaybackUnavailableWithoutRetryAfterIsReplayed(t *testing.T) {
	movieKey := frontend.MovieKey(603)
	movieGUID := ids.RatingKeyGUID(movieKey)
	sess := &versionsSessionsFake{}
	handler := mountVersionsVela(t, movieKey, &versionsEditionsFake{}, &versionsHandlesFake{item: movieKey}, sess)

	planCalls := 0
	sess.planFunc = func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		planCalls++
		return frontend.PlayPlan{}, frontend.ErrStreamLimit
	}
	body := `{"idempotencyKey":"play-limit","titleId":"` + movieGUID + `","client":{"device":{"id":"dev-1"}}}`
	for attempt := 1; attempt <= 2; attempt++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, velaReq("POST", "/vela/v0/playback", body))
		if w.Code != 503 || w.Header().Get("Retry-After") != "" || planCalls != 1 {
			t.Fatalf("attempt %d = %d Retry-After=%q planCalls=%d body=%s", attempt, w.Code, w.Header().Get("Retry-After"), planCalls, w.Body.String())
		}
	}
}
