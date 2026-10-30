package playback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type retirementTestSource struct {
	entered      chan struct{}
	readRelease  chan struct{}
	closing      chan struct{}
	closeRelease chan struct{}
	readErr      error
}

func (s *retirementTestSource) Size() int64       { return 1 }
func (s *retirementTestSource) Name() string      { return "test.mkv" }
func (s *retirementTestSource) ReleaseID() string { return "test" }
func (s *retirementTestSource) ReadAt(p []byte, _ int64) (int, error) {
	close(s.entered)
	<-s.readRelease
	if s.readErr != nil {
		return 0, s.readErr
	}
	p[0] = 1
	return 1, nil
}
func (s *retirementTestSource) Close() error { close(s.closing); <-s.closeRelease; return nil }

func retirementFixture() (*Manager, *generation, *retirementTestSource) {
	src := &retirementTestSource{entered: make(chan struct{}), readRelease: make(chan struct{}), closing: make(chan struct{}), closeRelease: make(chan struct{})}
	session := &session{}
	g := &generation{src: src, sess: session}
	session.cur = g
	m := &Manager{opts: Options{ReadAhead: 1}, now: time.Now}
	m.bufs.New = func() any { b := make([]byte, 1); return &b }
	return m, g, src
}
func waitRetirementTest(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
}
func assertRetirementOpen(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("retired early: %s", what)
	default:
	}
}

func TestReadRetirementAfterProducerRelease(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel-close", true: "read-error"}[failure], func(t *testing.T) {
			m, g, src := retirementFixture()
			if failure {
				src.readErr = errors.New("offline source error")
			}
			tracker := &core.ReadRetirement{}
			ctx := core.WithReadRetirement(context.Background(), tracker)
			returned := make(chan struct{})
			go func() { defer close(returned); _, _, _ = m.copyFrom(ctx, io.Discard, g, 0, 1, func(int) {}, true) }()
			waitRetirementTest(t, src.entered, "read start")
			g.retire()
			close(src.readRelease)
			waitRetirementTest(t, src.closing, "source close")
			waitRetirementTest(t, returned, "copy return")
			done := tracker.Seal()
			assertRetirementOpen(t, done, "source close has not returned")
			close(src.closeRelease)
			waitRetirementTest(t, done, "producer released")
		})
	}
}

func TestReadRetirementStalledCancellationAndReplacement(t *testing.T) {
	m, old, src := retirementFixture()
	tracker := &core.ReadRetirement{}
	ctx, cancel := context.WithCancel(core.WithReadRetirement(context.Background(), tracker))
	defer cancel()
	returned := make(chan struct{})
	go func() { defer close(returned); _, _, _ = m.copyFrom(ctx, io.Discard, old, 0, 1, func(int) {}, true) }()
	waitRetirementTest(t, src.entered, "stalled read")
	old.retire()
	cancel()
	done := tracker.Seal()
	assertRetirementOpen(t, done, "stalled producer")
	// A separate replacement generation and request must not wait for the old
	// producer or be included in its retirement proof.
	m2, replacement, src2 := retirementFixture()
	tracker2 := &core.ReadRetirement{}
	returned2 := make(chan struct{})
	go func() {
		defer close(returned2)
		_, _, _ = m2.copyFrom(core.WithReadRetirement(context.Background(), tracker2), io.Discard, replacement, 0, 1, func(int) {}, true)
	}()
	waitRetirementTest(t, src2.entered, "replacement read")
	replacement.retire()
	close(src2.closeRelease)
	close(src2.readRelease)
	waitRetirementTest(t, returned2, "replacement response")
	waitRetirementTest(t, tracker2.Seal(), "replacement producer")
	assertRetirementOpen(t, done, "old producer remains stalled")
	close(src.closeRelease)
	close(src.readRelease)
	waitRetirementTest(t, returned, "cancelled response")
	waitRetirementTest(t, done, "old producer")
}

func TestReadRetirementSealAndConcurrentDone(t *testing.T) {
	tracker := &core.ReadRetirement{}
	finish, ok := tracker.Begin()
	if !ok {
		t.Fatal("begin rejected")
	}
	done := tracker.Seal()
	if _, ok = tracker.Begin(); ok {
		t.Fatal("begin accepted after seal")
	}
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); finish() }()
	}
	wg.Wait()
	waitRetirementTest(t, done, "concurrent completion")
}

func TestReadRetirementIndependentLoopbackContext(t *testing.T) {
	m, g, src := retirementFixture()
	g.token = "12345678"
	m.tokens = map[string]*generation{"session/" + g.token: g}
	req := httptest.NewRequest(http.MethodGet, "/src/session/12345678?background=1", nil)
	req.SetPathValue("session", "session")
	req.SetPathValue("token", g.token)
	returned := make(chan struct{})
	go func() { defer close(returned); m.serveLoopback(httptest.NewRecorder(), req) }()
	waitRetirementTest(t, src.entered, "loopback read")
	// A segment request's independent empty tracker proves nothing about the
	// untracked FFmpeg loopback producer, which is still blocked here.
	segment := &core.ReadRetirement{}
	waitRetirementTest(t, segment.Seal(), "independent segment tracker")
	select {
	case <-returned:
		t.Fatal("loopback producer unexpectedly retired")
	default:
	}
	g.retire()
	close(src.closeRelease)
	close(src.readRelease)
	waitRetirementTest(t, returned, "loopback completion")
}

func TestReadRetirementCapacityAndUntrackedBytes(t *testing.T) {
	for i := 0; i < maxReadProducers; i++ {
		select {
		case readProducers <- struct{}{}:
		default:
			t.Fatal("producer budget leaked by earlier request")
		}
	}
	defer func() {
		for i := 0; i < maxReadProducers; i++ {
			<-readProducers
		}
	}()
	m, g, src := retirementFixture()
	tracker := &core.ReadRetirement{}
	n, rerr, werr := m.copyFrom(core.WithReadRetirement(context.Background(), tracker), io.Discard, g, 0, 1, func(int) {}, true)
	if n != 0 || !errors.Is(rerr, core.ErrNotAvailable) || werr != nil {
		t.Fatalf("capacity: %d %v %v", n, rerr, werr)
	}
	waitRetirementTest(t, tracker.Seal(), "rejected request has no producer")
	assertRetirementOpen(t, src.entered, "capacity rejection started source")
	// Existing Plex and loopback callers omit the tracker. Even under full
	// tracked admission their byte stream remains unchanged.
	close(src.readRelease)
	close(src.closeRelease)
	var body bytes.Buffer
	n, rerr, werr = m.copyFrom(context.Background(), &body, g, 0, 1, func(int) {}, true)
	if n != 1 || rerr != nil || werr != nil || !bytes.Equal(body.Bytes(), []byte{1}) {
		t.Fatalf("untracked copy changed: %d %v %v %x", n, rerr, werr, body.Bytes())
	}
	g.retire()
	waitRetirementTest(t, src.closing, "untracked generation closed")
}

func TestReadRetirementRejectedRegistrationsReleaseCapacity(t *testing.T) {
	m, g, src := retirementFixture()
	close(src.readRelease)
	close(src.closeRelease)
	for _, mode := range []string{"sealed", "retired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			before := len(readProducers)
			tracker := &core.ReadRetirement{}
			ctx, cancel := context.WithCancel(core.WithReadRetirement(context.Background(), tracker))
			defer cancel()
			switch mode {
			case "sealed":
				tracker.Seal()
			case "retired":
				g.retire()
			case "cancelled":
				cancel()
			}
			n, rerr, werr := m.copyFrom(ctx, io.Discard, g, 0, 1, func(int) {}, true)
			if n != 0 || rerr == nil || werr != nil || len(readProducers) != before {
				t.Fatalf("rejection leaked capacity: %d %v %v slots=%d want=%d", n, rerr, werr, len(readProducers), before)
			}
			waitRetirementTest(t, tracker.Seal(), "rejected registration")
		})
	}
}

func TestReadRetirementHTTPByteIdentity(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "plex-client")
	for _, tc := range []struct{ method, span string }{
		{http.MethodGet, ""}, {http.MethodGet, "bytes=5-99"},
		{http.MethodGet, "bytes=-50"}, {http.MethodGet, "bytes=20000-"},
		{http.MethodHead, "bytes=0-9"},
	} {
		t.Run(tc.method+tc.span, func(t *testing.T) {
			request := func(tracker *core.ReadRetirement) *httptest.ResponseRecorder {
				req := httptest.NewRequest(tc.method, "/media", nil)
				if tc.span != "" {
					req.Header.Set("Range", tc.span)
				}
				if tracker != nil {
					req = req.WithContext(core.WithReadRetirement(req.Context(), tracker))
				}
				out := httptest.NewRecorder()
				h.m.ServeRange(out, req, ps.ID)
				return out
			}
			legacy := request(nil)
			tracker := &core.ReadRetirement{}
			tracked := request(tracker)
			waitRetirementTest(t, tracker.Seal(), "tracked HTTP producer")
			if legacy.Code != tracked.Code || !reflect.DeepEqual(legacy.Header(), tracked.Header()) || !bytes.Equal(legacy.Body.Bytes(), tracked.Body.Bytes()) {
				t.Fatalf("HTTP bytes differ: legacy=%d %v %x tracked=%d %v %x", legacy.Code, legacy.Header(), legacy.Body.Bytes(), tracked.Code, tracked.Header(), tracked.Body.Bytes())
			}
		})
	}
}
