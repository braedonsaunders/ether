package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMixedWorkloadVerifiesBytesDurationAndCleanup(t *testing.T) {
	media := []byte("verified media bytes")
	sum := sha256.Sum256(media)
	hash := hex.EncodeToString(sum[:])
	var cleanup atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"secret-token"}`))
		case "/browse":
			if r.Header.Get("Authorization") != "secret-token" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"Items":[]}`))
		case "/play":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"path":"/media"}`))
		case "/media":
			_, _ = w.Write(media)
		case "/subtitle":
			_, _ = w.Write([]byte("subtitle"))
		case "/logout":
			cleanup.Add(1)
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	steps := []workloadStep{{Kind: "auth", Method: "POST", Path: "/auth", ExpectedStatus: 200, MaxBytes: 1024, Capture: map[string]string{"access_token": "/token"}}, {Kind: "browse", Method: "GET", Path: "/browse", Headers: map[string]string{"Authorization": "${access_token}"}, ExpectedStatus: 200, MaxBytes: 1024}, {Kind: "play", Method: "POST", Path: "/play", ExpectedStatus: 200, MaxBytes: 1024, Capture: map[string]string{"stream": "/path"}}, {Kind: "seek", Method: "GET", URLVariable: "stream", Headers: map[string]string{"Range": "bytes=0-4"}, ExpectedStatus: 200, MaxBytes: 1024}, {Kind: "subtitle", Method: "GET", Path: "/subtitle", ExpectedStatus: 200, MaxBytes: 1024}, {Kind: "media", Method: "GET", URLVariable: "stream", ExpectedStatus: 200, MaxBytes: 1024, ExpectedSHA256: hash, ExpectedDurationMS: 1000, DurationToleranceMS: 10}, {Kind: "cleanup", Method: "POST", Path: "/logout", ExpectedStatus: 204, MaxBytes: 1024}}
	cfg := workloadConfig{TimeoutMS: 1000, Flows: map[string]workloadFlow{"mixed": {RequiresMedia: true, Steps: steps}}}
	target := workloadTarget{BaseURL: server.URL}
	verify := func(_ context.Context, path string) (int64, error) {
		data, e := os.ReadFile(path)
		if e != nil || string(data) != string(media) {
			return 0, errors.New("wrong temp media")
		}
		return 1000, nil
	}
	event := runWorkloadFlow(context.Background(), server.Client(), cfg, target, workloadArrival{Client: "test-device", Flow: "mixed"}, verify)
	if !event.Completed || event.Error != "" || event.VerifiedDurationMS != 1000 || event.CleanupFailures != 0 || cleanup.Load() != 1 {
		t.Fatalf("mixed event %+v cleanup=%d", event, cleanup.Load())
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), "secret-token") {
		t.Fatal("token leaked in result")
	}
	bad := func(context.Context, string) (int64, error) { return 2000, nil }
	event = runWorkloadFlow(context.Background(), server.Client(), cfg, target, workloadArrival{Client: "test-device", Flow: "mixed"}, bad)
	if event.Completed || event.Error != "media_duration_mismatch" || cleanup.Load() != 2 {
		t.Fatalf("mismatch/cleanup: %+v", event)
	}
}
func TestWorkloadCancellationAndCleanupFailureAreCounted(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logout" {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, "abcdef")
	}))
	defer s.Close()
	cfg := workloadConfig{TimeoutMS: 1000, Flows: map[string]workloadFlow{"cancel": {Steps: []workloadStep{{Kind: "cancel", Method: "GET", Path: "/stream", ExpectedStatus: 200, MaxBytes: 100, CancelAfter: 3}, {Kind: "cleanup", Method: "POST", Path: "/logout", ExpectedStatus: 204, MaxBytes: 100}}}}}
	event := runWorkloadFlow(context.Background(), s.Client(), cfg, workloadTarget{BaseURL: s.URL}, workloadArrival{Client: "c", Flow: "cancel"}, func(context.Context, string) (int64, error) { return 0, nil })
	if !event.Cancelled || event.CleanupFailures != 1 || event.Completed {
		t.Fatalf("cleanup failure hidden: %+v", event)
	}
}
func TestWorkloadRejectsNonStagingAndUnmatchedMedia(t *testing.T) {
	hash := strings.Repeat("a", 64)
	cfg := workloadConfig{ReservationID: "slot", Concurrency: 1, Repetitions: 1, TimeoutMS: 1000, FFmpeg: "/usr/bin/ffmpeg", FFmpegSHA256: hash, ManifestSHA256: hash, CacheState: "warm", Reference: workloadTarget{BaseURL: "http://127.0.0.1:8096", Variables: map[string]string{"hash": hash}}, Ether: workloadTarget{BaseURL: "http://127.0.0.1:18096", Variables: map[string]string{"hash": hash}}, Flows: map[string]workloadFlow{"play": {RequiresMedia: true, Steps: []workloadStep{{Kind: "media", Method: "GET", Path: "/video", ExpectedStatus: 200, MaxBytes: 100, ExpectedSHA256: "${hash}", ExpectedDurationMS: 1000}, {Kind: "cleanup", Method: "POST", Path: "/logout", ExpectedStatus: 204, MaxBytes: 100}}}}, Trace: []workloadArrival{{Client: "c", Flow: "play"}}}
	if e := validateWorkload(cfg); e != nil {
		t.Fatal(e)
	}
	cfg.Ether.Variables["hash"] = strings.Repeat("b", 64)
	if validateWorkload(cfg) == nil {
		t.Fatal("mismatched delivered hashes accepted")
	}
	cfg.Ether.Variables["hash"] = hash
	cfg.Ether.BaseURL = "http://10.0.0.43:32400"
	if validateWorkload(cfg) == nil {
		t.Fatal("non-loopback target accepted")
	}
	if _, e := workloadURL("http://127.0.0.1:18096", workloadStep{URLVariable: "url"}, map[string]string{"url": "http://10.0.0.43:32400/path"}); e == nil {
		t.Fatal("cross-origin stream accepted")
	}
}

func TestDecodeDurationUsesActualDecodedOutput(t *testing.T) {
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=size=32x32:rate=10", "-t", "1", "-c:v", "mpeg4", "-threads", "1", "-an", path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("generate media: %v %s", e, out)
	}
	duration, e := decodeDuration(context.Background(), ffmpeg, path)
	if e != nil || duration < 900 || duration > 1100 {
		t.Fatalf("decoded duration %d ms: %v", duration, e)
	}
}

func TestMixedWorkloadCountsAllOutcomesAndInterruptions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad-logout" {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/logout" {
			w.WriteHeader(204)
			return
		}
		_, _ = w.Write([]byte("abc"))
	}))
	defer s.Close()
	cg := t.TempDir()
	for name, value := range map[string]string{"cpu.stat": "usage_usec 0\n", "memory.current": "1", "pids.current": "0", "io.stat": "", "cgroup.procs": ""} {
		if e := os.WriteFile(filepath.Join(cg, name), []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
	}
	sum := sha256.Sum256([]byte("abc"))
	hash := hex.EncodeToString(sum[:])
	media := workloadStep{Kind: "media", Method: "GET", Path: "/media", ExpectedStatus: 200, MaxBytes: 100, ExpectedSHA256: hash, ExpectedDurationMS: 1000}
	goodCleanup := workloadStep{Kind: "cleanup", Method: "POST", Path: "/logout", ExpectedStatus: 204, MaxBytes: 100}
	badCleanup := workloadStep{Kind: "cleanup", Method: "POST", Path: "/bad-logout", ExpectedStatus: 204, MaxBytes: 100}
	cancelStep := workloadStep{Kind: "cancel", Method: "GET", Path: "/media", ExpectedStatus: 200, MaxBytes: 100, CancelAfter: 1}
	cfg := workloadConfig{Concurrency: 2, TimeoutMS: 1000, Flows: map[string]workloadFlow{"good": {RequiresMedia: true, Steps: []workloadStep{media, goodCleanup}}, "bad": {Steps: []workloadStep{cancelStep, badCleanup}}}, Trace: []workloadArrival{{Client: "a", Flow: "good"}, {Client: "b", Flow: "bad"}}}
	v := func(context.Context, string) (int64, error) { return 1000, nil }
	run := executeWorkload(context.Background(), cfg, "reference", workloadTarget{BaseURL: s.URL, Cgroup: cg}, v)
	if !run.Complete || run.Offered != 2 || run.Started != 2 || run.Completed != 1 || run.Failed != 1 || run.CleanupFailures != 1 || run.CPUSecondsPerVerifiedStreamMinute != nil {
		t.Fatalf("incorrect workload accounting: %+v", run)
	}
	cfg.Trace = []workloadArrival{{Client: "a", Flow: "good"}, {Client: "b", Flow: "good", AtMS: 60000}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(25*time.Millisecond, cancel)
	run = executeWorkload(ctx, cfg, "reference", workloadTarget{BaseURL: s.URL, Cgroup: cg}, v)
	if run.Complete || run.Dropped != 1 || run.Offered != 2 || run.Started != 1 || run.CPUSecondsPerVerifiedStreamMinute != nil {
		t.Fatalf("interruption hidden: %+v", run)
	}
}
