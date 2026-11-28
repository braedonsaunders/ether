package clusterplayback

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playbackbuffer"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
)

func outputFixture(t *testing.T) (*bufferedSession, []byte) {
	t.Helper()
	data, err := os.ReadFile("../playbackbuffer/testdata/cpu.ts")
	if err != nil {
		t.Fatal(err)
	}
	b, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: 1 << 20, Segments: 16, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: 1 << 20, Segments: 16, Inits: 1}, Sessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{o: Options{SurfacePolicy: testSessionPolicy, Delivery: testDeliveryPolicy(), OutputBuffer: b, MaxSegmentBytes: int64(len(data) * 2)}, outputScratch: make(chan struct{}, outputScratchSlots(int64(len(data)*2))), executions: map[string]*execution{}}
	db := openFixtureDB(t)
	ledger, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: time.Minute, LockTimeout: fixtureSQLBudget()}, outputRetirementVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := placement.Target{NodeID: "encoder", Boot: "boot"}
	if err := ledger.SetPool(ctx, "cpu", config.RoleTranscode, 1); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetTarget(ctx, target, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"cpu"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := ledger.Reserve(ctx, placement.Request{Session: "retained", Operation: "operation", Controller: placement.Controller{ID: "controller", Boot: "boot"}, Target: target, Role: config.RoleTranscode, Claims: []placement.Claim{{Scope: "cpu", Units: 1}}, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r.o.DB, r.o.Placement = db, ledger
	route := Route{Fence: rec.Fence}
	if err := r.putRoute(ctx, route); err != nil {
		t.Fatal(err)
	}
	s := r.newBufferedSession(route, core.HLSRequest{Decision: core.Decision{Container: "mpegts"}}, &remotetranscode.Session{})
	if s == nil {
		t.Fatal("no wrapper")
	}
	r.executions[route.Fence.Session] = &execution{route: route, output: s}
	return s, data
}
func outputResponse(data []byte) func(http.ResponseWriter, *http.Request) remotetranscode.RepresentationCompletion {
	return func(w http.ResponseWriter, q *http.Request) remotetranscode.RepresentationCompletion {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", `"original"`)
		w.WriteHeader(200)
		n, err := w.Write(data)
		return remotetranscode.RepresentationCompletion{StatusCode: 200, ContentLength: int64(len(data)), ReadBytes: int64(len(data)), WrittenBytes: int64(n), BodyPresent: true, EOF: true, Closed: true, WriteError: err != nil || n != len(data)}
	}
}
func TestOutputBufferContiguousAndImmutable(t *testing.T) {
	s, _ := outputFixture(t)
	// Real synthetic lab output has 96 unbounded H.264 PES packets.
	compressed, err := os.ReadFile("../playbackbuffer/testdata/lab-unbounded.ts.gz")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	buffer, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: 16 << 20, Segments: 16, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: 16 << 20, Segments: 16, Inits: 1}, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = buffer.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	s.r.o.OutputBuffer, s.r.o.MaxSegmentBytes = buffer, 16<<20
	q := httptest.NewRequest("GET", "/segment", nil)
	s.serveSegment(httptest.NewRecorder(), q, 1, outputResponse(data))
	if _, err := s.r.o.OutputBuffer.Acquire(s.key(1)); err == nil {
		t.Fatal("future segment published")
	}
	s.serveSegment(httptest.NewRecorder(), q, 1, outputResponse([]byte("different")))
	s.serveSegment(httptest.NewRecorder(), q, 0, outputResponse(data))
	snap, err := s.r.SnapshotOutput("retained")
	if err != nil || snap.Buffer.State.PublishedFloor != 1 || !snap.Retaining {
		t.Fatal(snap, err)
	}
	for _, m := range snap.Buffer.Entries {
		if !m.Facts.Verified {
			t.Fatal("fixture lost structural facts")
		}
	}
	lease, err := s.r.o.OutputBuffer.Acquire(s.key(1))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	got, err := io.ReadAll(io.NewSectionReader(lease, 0, int64(len(data))))
	if err != nil || string(got) != string(data) {
		t.Fatal("immutable bytes changed", err)
	}
}
func TestOutputBufferPassThroughAndIncomplete(t *testing.T) {
	for _, test := range []string{"head", "range", "conditional", "206", "error", "short", "unknown", "oversize", "cancel", "type", "content-range", "headers", "no-store"} {
		t.Run(test, func(t *testing.T) {
			s, data := outputFixture(t)
			q := httptest.NewRequest("GET", "/segment", nil)
			status := 200
			body := data
			cl := strconv.Itoa(len(data))
			ct := "video/mp2t"
			switch test {
			case "head":
				q.Method = "HEAD"
			case "range":
				q.Header.Set("Range", "bytes=0-5")
			case "conditional":
				q.Header.Set("If-None-Match", `"original"`)
			case "206":
				status = 206
			case "error":
				status = 403
			case "short":
				cl = strconv.Itoa(len(data) + 1)
			case "unknown":
				cl = ""
			case "oversize":
				body = []byte(strings.Repeat("x", int(s.r.o.MaxSegmentBytes)+1))
				cl = strconv.Itoa(len(body))
			case "cancel":
				ctx, cancel := context.WithCancel(q.Context())
				cancel()
				q = q.WithContext(ctx)
			case "type":
				ct = "text/plain"
			}
			calls := 0
			serve := func(w http.ResponseWriter, _ *http.Request) remotetranscode.RepresentationCompletion {
				calls++
				w.Header().Set("Content-Type", ct)
				if cl != "" {
					w.Header().Set("Content-Length", cl)
				}
				if test == "content-range" {
					w.Header().Set("Content-Range", "bytes 0-1/2")
				}
				if test == "no-store" {
					w.Header().Set("Cache-Control", test)
				}
				if test == "headers" {
					w.Header().Set("ETag", strings.Repeat("x", 17000))
				}
				w.WriteHeader(status)
				_, _ = w.Write(body)
				return remotetranscode.RepresentationCompletion{}
			}
			want := httptest.NewRecorder()
			serve(want, q)
			calls = 0
			got := httptest.NewRecorder()
			s.serveSegment(got, q, 0, serve)
			if test == "cancel" {
				if got.Code != 409 {
					t.Fatal("cancelled generation published", got.Code)
				}
			} else if test == "unknown" || test == "oversize" || test == "type" || test == "content-range" || test == "headers" || test == "no-store" {
				if got.Code != 503 {
					t.Fatal("unsupported successful representation acknowledged", test, got.Code)
				}
			} else if calls != 1 || got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
				t.Fatal("upstream response changed or retried")
			}
			snap, err := s.r.o.OutputBuffer.Snapshot("retained")
			if err != nil || len(snap.Entries) != 0 {
				t.Fatal("invalid full representation retained", snap, err)
			}
		})
	}
}

type outputFailWriter struct{ *httptest.ResponseRecorder }

func (w outputFailWriter) Write(p []byte) (int, error) { return len(p) / 2, io.ErrClosedPipe }
func TestOutputCaptureDownstreamFailure(t *testing.T) {
	w := &segmentCapture{ResponseWriter: outputFailWriter{httptest.NewRecorder()}, limit: 10, body: make([]byte, 0, 10)}
	outputResponse([]byte("hello"))(w, httptest.NewRequest("GET", "/", nil))
	if w.complete(context.Background()) || len(w.body) > 0 {
		t.Fatal("failed delivery retained")
	}
}

// Every completed successful response must already have immutable published
// bytes, even when the next request overlaps the former upstream EOF/close.
func TestOutputBufferPublicationBeforeSuccessInvariant(t *testing.T) {
	s, data := outputFixture(t)
	s.r.o.MemberRole = func(context.Context, nodewire.Identity, config.NodeRole) error { return nil }
	writer := func(n int) *publicationWriter {
		return &publicationWriter{ResponseRecorder: httptest.NewRecorder(), check: func() bool {
			snap, err := s.r.o.OutputBuffer.Snapshot("retained")
			return err == nil && snap.State.PublishedFloor >= int64(n)
		}, length: len(data), t: t}
	}
	q := func() *http.Request { return httptest.NewRequest("GET", "/segment", nil) }
	first, second := writer(0), writer(1)
	bodyWritten, release, done0, done1 := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done0)
		s.serveSegment(first, q(), 0, func(w http.ResponseWriter, r *http.Request) remotetranscode.RepresentationCompletion {
			facts := outputResponse(data)(w, r)
			close(bodyWritten)
			<-release
			return facts
		})
	}()
	<-bodyWritten
	// Upstream EOF/publication has not completed: not even this already-written
	// full source body may be acknowledged by a complete downstream body.
	if first.Body.Len() == len(data) {
		t.Error("client completed before upstream completion/publication")
	}
	go func() { defer close(done1); s.serveSegment(second, q(), 1, outputResponse(data)) }()
	// Cancellation while waiting must not enter another upstream operation.
	c, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := httptest.NewRecorder()
	s.serveSegment(cancelled, q().WithContext(c), 2, func(http.ResponseWriter, *http.Request) remotetranscode.RepresentationCompletion {
		t.Error("cancelled waiter reached worker")
		return remotetranscode.RepresentationCompletion{}
	})
	if cancelled.Code != 409 {
		t.Error("cancelled waiter not fenced", cancelled.Code)
	}
	close(release)
	<-done0
	<-done1
	for i, w := range []*publicationWriter{first, second} {
		if w.Body.String() != string(data) || w.Code != 200 {
			t.Fatal("overlap lost representation", i, w.Code, w.Body.Len())
		}
	}
	for _, failure := range []string{"read-error", "cancel-after-body"} {
		ctx, stop := context.WithCancel(context.Background())
		failed := writer(2)
		s.serveSegment(failed, q().WithContext(ctx), 2, func(w http.ResponseWriter, r *http.Request) remotetranscode.RepresentationCompletion {
			f := outputResponse(data)(w, r)
			if failure == "read-error" {
				f.ReadError = true
			} else {
				stop()
			}
			return f
		})
		stop()
		snap, err := s.r.o.OutputBuffer.Snapshot("retained")
		if failed.Body.Len() >= len(data) || err != nil || snap.State.PublishedFloor != 1 {
			t.Fatal("failed/cancelled body acknowledged or published", failure, snap, err)
		}
	}
	for n := 2; n <= 3; n++ {
		w := writer(n)
		s.serveSegment(w, q(), n, outputResponse(data))
		if w.Body.String() != string(data) {
			t.Fatal("retry lost bytes", n)
		}
	}
	// A staged future representation cannot fall through to an unobserved worker
	// read. Publishing its missing predecessor makes the exact staged key usable.
	future := writer(5)
	s.serveSegment(future, q(), 5, outputResponse(data))
	if future.Body.Len() >= len(data) {
		t.Fatal("future uncommitted body acknowledged")
	}
	pending := httptest.NewRecorder()
	s.serveSegment(pending, q(), 5, func(http.ResponseWriter, *http.Request) remotetranscode.RepresentationCompletion {
		t.Error("staged key reread worker")
		return remotetranscode.RepresentationCompletion{}
	})
	if pending.Code != 503 {
		t.Fatal("staged future not retryable", pending.Code)
	}
	fourth := writer(4)
	s.serveSegment(fourth, q(), 4, outputResponse(data))
	fifth := writer(5)
	s.serveSegment(fifth, q(), 5, outputResponse(data))
	if fourth.Body.String() != string(data) || fifth.Body.String() != string(data) {
		t.Fatal("contiguous staged prefix lost")
	}
	if len(s.r.outputScratch) != 0 {
		t.Fatal("scratch leaked")
	}
}

type publicationWriter struct {
	*httptest.ResponseRecorder
	t      *testing.T
	length int
	check  func() bool
}

func (w *publicationWriter) Write(p []byte) (int, error) {
	if w.Body.Len()+len(p) >= w.length && !w.check() {
		w.t.Error("successful full representation preceded contiguous publication")
	}
	return w.ResponseRecorder.Write(p)
}
func TestOutputBufferExactOptionalMethodsAndNil(t *testing.T) {
	original := reflect.TypeOf(&remotetranscode.Session{})
	wrapped := reflect.TypeOf(&bufferedSession{})
	for i := 0; i < original.NumMethod(); i++ {
		m := original.Method(i)
		got, ok := wrapped.MethodByName(m.Name)
		if !ok || got.Type.NumIn() != m.Type.NumIn() || got.Type.NumOut() != m.Type.NumOut() {
			t.Fatal("lost method", m.Name)
		}
		for j := 1; j < m.Type.NumIn(); j++ {
			if m.Type.In(j) != got.Type.In(j) {
				t.Fatal(m.Name)
			}
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			if m.Type.Out(j) != got.Type.Out(j) {
				t.Fatal(m.Name)
			}
		}
	}
	r := &Runtime{}
	if r.newBufferedSession(Route{}, core.HLSRequest{}, &remotetranscode.Session{}) != nil {
		t.Fatal("nil option wrapped")
	}
}

type outputRetirementVerifier struct{}

func (outputRetirementVerifier) VerifyRetired(context.Context, placement.Fence, string) error {
	return nil
}
func TestOutputBufferCachedFailureAndAuthority(t *testing.T) {
	s, data := outputFixture(t)
	db := openFixtureDB(t)
	ledger, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: time.Minute, LockTimeout: fixtureSQLBudget()}, outputRetirementVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := placement.Target{NodeID: "encoder", Boot: "boot"}
	if err = ledger.SetPool(ctx, "cpu", config.RoleTranscode, 1); err != nil {
		t.Fatal(err)
	}
	if err = ledger.SetTarget(ctx, target, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"cpu"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := ledger.Reserve(ctx, placement.Request{Session: "retained", Operation: "operation", Controller: placement.Controller{ID: "controller", Boot: "boot"}, Target: target, Role: config.RoleTranscode, Claims: []placement.Claim{{Scope: "cpu", Units: 1}}, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	s.route.Fence = rec.Fence
	s.r.executions["retained"].route = s.route
	s.r.o.Placement = ledger
	s.r.o.DB = db
	if err := s.r.putRoute(ctx, s.route); err != nil {
		t.Fatal(err)
	}
	denied := false
	s.r.o.MemberRole = func(context.Context, nodewire.Identity, config.NodeRole) error {
		if denied {
			return errors.New("revoked")
		}
		return nil
	}
	q := httptest.NewRequest("GET", "/segment", nil)
	first := httptest.NewRecorder()
	s.serveSegment(first, q, 0, outputResponse(data))
	failed := func(http.ResponseWriter, *http.Request) remotetranscode.RepresentationCompletion {
		t.Fatal("cached bytes fetched dead worker")
		return remotetranscode.RepresentationCompletion{}
	}
	got := httptest.NewRecorder()
	s.serveSegment(got, q, 0, failed)
	if got.Code != 200 || got.Body.String() != string(data) || !reflect.DeepEqual(got.Header(), first.Header()) {
		t.Fatalf("cache changed representation: status=%d headers=%v want=%v bytes=%d", got.Code, got.Header(), first.Header(), got.Body.Len())
	}
	denied = true
	got = httptest.NewRecorder()
	s.serveSegment(got, q, 0, failed)
	if got.Code != 503 {
		t.Fatal("revoked member served cache", got.Code)
	}
	denied = false
	s.r.executions["retained"].stopping = true
	got = httptest.NewRecorder()
	s.serveSegment(got, q, 0, failed)
	if got.Code != 410 {
		t.Fatal("stopping execution served cache", got.Code)
	}
	s.r.executions["retained"].stopping = false
	lease, err := s.r.o.OutputBuffer.Acquire(s.key(0))
	if err != nil {
		t.Fatal(err)
	}
	if err = ledger.Retire(ctx, rec.Fence, "test"); err != nil {
		t.Fatal(err)
	}
	s.r.executions["retained"].retired = true
	if err = s.r.ReleaseOutput(ctx, "retained", rec.Fence); err == nil {
		t.Fatal("released reader pinned bytes")
	}
	_ = lease.Close()
	got = httptest.NewRecorder()
	s.serveSegment(got, q, 0, failed)
	if got.Code != 409 {
		t.Fatal("retired authority served cache", got.Code)
	}
	if err = s.r.ReleaseOutput(ctx, "retained", rec.Fence); err != nil {
		t.Fatal(err)
	}
}

// A full recovery buffer must never stop delivery (#789). The session's own
// budget rolls: segments behind the restart watermark are released, the
// floor advances and recovery stays available. When retention is impossible
// anyway (here the global pool), the segment is delivered and the session
// gives up recovery instead of withholding its last byte forever.
func TestOutputBufferLifetimeCapacity(t *testing.T) {
	s, data := outputFixture(t)
	q := httptest.NewRequest("GET", "/segment", nil)
	full := func(n int) {
		t.Helper()
		got := httptest.NewRecorder()
		s.serveSegment(got, q, n, outputResponse(data))
		if !bytes.Equal(got.Body.Bytes(), data) {
			t.Fatalf("segment %d: %d of %d bytes delivered", n, got.Body.Len(), len(data))
		}
	}
	rolling, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: int64(len(data)) * 64, Segments: 64, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: int64(len(data)) * 8, Segments: 64, Inits: 1}, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = rolling.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	s.r.o.OutputBuffer = rolling
	for n := range 10 {
		full(n)
	}
	snap, err := s.r.SnapshotOutput("retained")
	if err != nil || !snap.Retaining || snap.Buffer.State.PublishedFloor != 9 || s.isUnbuffered() {
		t.Fatal("rolling retention lost the watermark", snap, err)
	}
	if rolling.Has(s.key(0)) || !rolling.Has(s.key(9)) {
		t.Fatal("released the wrong end of the published range")
	}
	full(0) // released behind the watermark: served straight through

	s, data = outputFixture(t)
	b, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: int64(len(data)) * 5, Segments: 1, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: int64(len(data)) * 5, Segments: 1, Inits: 1}, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	s.r.o.OutputBuffer = b
	full(0)
	full(1)
	snap, err = s.r.SnapshotOutput("retained")
	if err != nil || snap.Retaining || !s.isUnbuffered() || len(snap.Buffer.Entries) != 1 || snap.Buffer.State.PublishedFloor != 0 {
		t.Fatal(snap, err)
	}
	lease, err := b.Acquire(s.key(0))
	if err != nil {
		t.Fatal("old bytes evicted", err)
	}
	_ = lease.Close()
}

func TestOutputRuntimeAvailabilityRequiresCommittedBytes(t *testing.T) {
	s, data := outputFixture(t)
	x := s.r.executions["retained"]
	x.adapter = &remotetranscode.Adapter{}
	if _, ok := s.r.Session("retained"); ok {
		t.Fatal("empty cache claims available")
	}
	s.serveSegment(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), 0, outputResponse(data))
	if got, ok := s.r.Session("retained"); !ok || got != s {
		t.Fatal("committed output unavailable after encoder loss")
	}
	x.stopping = true
	if _, ok := s.r.Session("retained"); ok {
		t.Fatal("stopped cache claims available")
	}
}

func TestOutputBufferRemoteMTLSCompletion(t *testing.T) {
	s, data := outputFixture(t)
	// Default offline coverage derives legal reset packets from the existing TS
	// fixture. The scoped lab gate also supplies sealed restarted-muxer bytes;
	// those artifacts stay outside git and no encoder is invoked by this test.
	if path := os.Getenv("ETHER_TEST_INITIAL_TS"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != "7a7f2bf9a2077f35d55c44edaf995316a586a9e770371d758e45de4f039d0b1e" {
			t.Fatal("initial-marker fixture changed")
		}
	} else {
		original := data
		data = nil
		seen := map[uint16]bool{}
		for off := 0; off < len(original); off += 188 {
			packet := original[off : off+188]
			pid := uint16(packet[1]&31)<<8 | uint16(packet[2])
			if !seen[pid] {
				data = append(data, initialResetPacket(pid, packet[3]&15)...)
				seen[pid] = true
			}
			data = append(data, packet...)
		}
	}
	buffer, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: 16 << 20, Segments: 16, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: 16 << 20, Segments: 16, Inits: 1}, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = buffer.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	s.r.o.OutputBuffer, s.r.o.MaxSegmentBytes = buffer, int64(len(data)*2)
	s.r.o.EncoderSegmentWait = time.Second
	s.r.deliveryScratch = make(chan struct{}, 4)
	s.r.o.MemberRole = func(context.Context, nodewire.Identity, config.NodeRole) error { return nil }
	var healthy atomic.Bool
	bad := append([]byte(nil), data...)
	middle := (len(bad) / 188 / 2) * 188
	bad = append(append(append([]byte(nil), bad[:middle]...), initialResetPacket(17, 0)...), bad[middle:]...)

	controller := nodewire.Identity{Cluster: "output", NodeID: "controller", Boot: "cboot"}
	worker := nodewire.Identity{Cluster: "output", NodeID: "worker", Boot: "wboot"}
	guards := fixtureGuards(t, controller, worker)
	id := remotetranscode.Identity{Controller: "controller", Worker: "worker", Session: "retained", Generation: 1}
	var calls atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if _, err := guards[1].Request(q); err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		var message struct {
			Op      string
			Segment int
		}
		if json.NewDecoder(q.Body).Decode(&message) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if message.Op == "start" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Identity": id, "State": "running"})
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		if healthy.Load() {
			payload := data
			if message.Segment == 2 {
				payload = bad
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
			return
		}

		switch message.Segment {
		case 0:
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data)
		case 1:
			w.Header().Set("Content-Length", strconv.Itoa(len(data)+1))
			_, _ = w.Write(data)
		case 2:
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			_, _ = w.Write(data)
		default:
			http.Error(w, "denied", 403)
		}
	}))
	server.TLS = guards[1].ServerTLS()
	server.StartTLS()
	defer server.Close()
	tc, err := guards[0].ClientTLS(worker, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := remotetranscode.NewAdapter(remotetranscode.AdapterOptions{Endpoint: server.URL, Client: &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}, RecheckServer: func(ctx context.Context, cs tls.ConnectionState) error {
		return guards[0].RecheckServer(ctx, cs, worker, "127.0.0.1")
	}, Assign: func(context.Context, core.HLSRequest) (remotetranscode.Assignment, error) {
		return remotetranscode.Assignment{Identity: id, Grant: "test", Expires: time.Now().Add(time.Minute)}, nil
	}, Decide: func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision { return core.Decision{} }, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	hs, _, err := adapter.StartHLSOwned(context.Background(), core.HLSRequest{SessionID: "retained", Decision: core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCopy: true, AudioCopy: true, SubtitleStream: -1, Enhancement: core.VideoEnhancement{State: "off"}}})
	if err != nil {
		t.Fatal(err)
	}
	s.Session = hs.(*remotetranscode.Session)
	recovery := &recoverySession{s}

	for n := 0; n < 3; n++ {
		w := httptest.NewRecorder()
		if n == 0 {
			recovery.ServeSegment(w, httptest.NewRequest("GET", "/segment", nil), n)
		} else {
			s.ServeSegment(w, httptest.NewRequest("GET", "/segment", nil), n)
		}
		if n == 2 {
			if w.Code != 503 {
				t.Fatal("unknown framing acknowledged", w.Code)
			}
		} else if w.Code != 200 || w.Body.String() != string(data) {
			t.Fatal("ordinary response changed", n, w.Code)
		}
	}
	snap, err := s.r.o.OutputBuffer.Snapshot("retained")
	if err != nil || len(snap.Entries) != 1 || snap.State.PublishedFloor != 0 || calls.Load() != 3 {
		t.Fatal("only complete full TS may publish, exactly one backend read each", snap, calls.Load(), err)
	}
	healthy.Store(true)
	published := &publicationWriter{ResponseRecorder: httptest.NewRecorder(), length: len(data), t: t, check: func() bool { v, e := buffer.Snapshot("retained"); return e == nil && v.State.PublishedFloor >= 1 }}
	recovery.ServeSegment(published, httptest.NewRequest("GET", "/segment", nil), 1)
	if published.Code != 200 || published.Body.String() != string(data) {
		t.Fatal("legal initial reset did not complete after publication", published.Code, published.Body.Len())
	}
	malformed := httptest.NewRecorder()
	recovery.ServeSegment(malformed, httptest.NewRequest("GET", "/segment", nil), 2)
	snap, err = buffer.Snapshot("retained")
	if malformed.Code != 503 || err != nil || snap.State.PublishedFloor != 1 {
		t.Fatal("midsegment reset published", malformed.Code, snap, err)
	}
	if len(s.r.outputScratch) != 0 || len(s.r.deliveryScratch) != 0 {
		t.Fatal("capture/delivery scratch leaked")
	}
}

// Retired lifetimes release bounded recovery capacity, but a retained reader
// still pins its bytes. Exercise the actual maintenance path over many sessions.
func TestRetiredOutputCapacityReclaimed(t *testing.T) {
	s, data := outputFixture(t)
	r := s.r
	r.o.Roles = config.NodeRoles{config.RoleDelivery}
	ctx := context.Background()
	for n := 0; n < 16; n++ {
		if n > 0 {
			route := s.route
			rec, err := r.o.Placement.Reserve(ctx, placement.Request{Session: fmt.Sprintf("next-%d", n), Operation: fmt.Sprintf("op-%d", n), Controller: route.Fence.Controller, Target: route.Fence.Target, Role: config.RoleTranscode, Claims: []placement.Claim{{Scope: "cpu", Units: 1}}, Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			route.Fence = rec.Fence
			if err := r.putRoute(ctx, route); err != nil {
				t.Fatal(err)
			}
			s = r.newBufferedSession(route, core.HLSRequest{Decision: core.Decision{Container: "mpegts"}}, &remotetranscode.Session{})
			if s == nil {
				t.Fatalf("session %d exhausted retired buffer slots", n)
			}
			r.executions[rec.Session] = &execution{route: route, output: s}
		}
		s.serveSegment(httptest.NewRecorder(), httptest.NewRequest("GET", "/segment", nil), 0, outputResponse(data))
		lease, err := r.o.OutputBuffer.Acquire(s.key(0))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.o.Placement.Retire(ctx, s.route.Fence, "test"); err != nil {
			t.Fatal(err)
		}
		r.executions[s.route.Fence.Session].retired = true
		_ = r.Tick(ctx)
		if _, err := r.o.OutputBuffer.Snapshot(s.route.Fence.Session); err != nil {
			t.Fatal("reader lost pinned output", err)
		}
		_ = lease.Close()
		_ = r.Tick(ctx)
		if _, err := r.o.OutputBuffer.Snapshot(s.route.Fence.Session); err == nil {
			t.Fatal("retired output not disposed")
		}
	}
}

// An adaptation-only packet changes no payload or timestamp. Its reset marker
// is legal only when it is the first observed transport packet on this PID.
func initialResetPacket(pid uint16, cc byte) []byte {
	p := bytes.Repeat([]byte{0xff}, 188)
	p[0], p[1], p[2], p[3], p[4], p[5] = 0x47, byte(pid>>8)&31, byte(pid), 0x20|cc, 183, 0x80
	return p
}

func TestConcurrentBufferedSessionsAndScratchReuse(t *testing.T) {
	if got := outputScratchSlots(16 << 20); got != 4 {
		t.Fatalf("outputScratchSlots(16 MiB) = %d, want 4", got)
	}
	if got := outputScratchSlots(64 << 20); got != 2 {
		t.Fatalf("outputScratchSlots(64 MiB) = %d, want 2", got)
	}
	s1, data := outputFixture(t)
	ctx := context.Background()
	r := s1.r
	r.o.EncoderSegmentWait = 150 * time.Millisecond
	r.o.MaxSegmentBytes = 16 << 20
	r.outputScratch = make(chan struct{}, outputScratchSlots(r.o.MaxSegmentBytes))
	b, err := playbackbuffer.New(playbackbuffer.Limits{
		Global:     playbackbuffer.Budget{Bytes: 64 << 20, Segments: 64, Inits: 1},
		PerSession: playbackbuffer.Budget{Bytes: 32 << 20, Segments: 64, Inits: 1},
		Sessions:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	r.o.OutputBuffer = b

	if err := r.o.Placement.SetPool(ctx, "cpu", config.RoleTranscode, 4); err != nil {
		t.Fatal(err)
	}
	rec2, err := r.o.Placement.Reserve(ctx, placement.Request{
		Session:    "retained-2",
		Operation:  "operation-2",
		Controller: s1.route.Fence.Controller,
		Target:     s1.route.Fence.Target,
		Role:       config.RoleTranscode,
		Claims:     []placement.Claim{{Scope: "cpu", Units: 1}},
		Lease:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	route2 := Route{Fence: rec2.Fence}
	if err := r.putRoute(ctx, route2); err != nil {
		t.Fatal(err)
	}
	s2 := r.newBufferedSession(route2, core.HLSRequest{Decision: core.Decision{Container: "mpegts"}}, &remotetranscode.Session{})
	if s2 == nil {
		t.Fatal("no second buffered session")
	}
	r.executions[route2.Fence.Session] = &execution{route: route2, output: s2}

	s1InFlight := make(chan struct{})
	releaseS1 := make(chan struct{})
	s1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s1.serveSegment(w, httptest.NewRequest("GET", "/segment", nil), 0, func(out http.ResponseWriter, req *http.Request) remotetranscode.RepresentationCompletion {
			close(s1InFlight)
			<-releaseS1
			return outputResponse(data)(out, req)
		})
		s1Done <- w
	}()

	<-s1InFlight
	w2 := httptest.NewRecorder()
	s2.serveSegment(w2, httptest.NewRequest("GET", "/segment", nil), 0, outputResponse(data))
	close(releaseS1)
	w1 := <-s1Done

	if w2.Code != http.StatusOK || !bytes.Equal(w2.Body.Bytes(), data) {
		t.Fatalf("second concurrent buffered session blocked by first: status=%d len=%d", w2.Code, w2.Body.Len())
	}
	if w1.Code != http.StatusOK || !bytes.Equal(w1.Body.Bytes(), data) {
		t.Fatalf("first buffered session failed: status=%d len=%d", w1.Code, w1.Body.Len())
	}

	// Capture buffers must grow to segment size and reuse pooled scratch rather
	// than allocating MaxSegmentBytes (16 MiB) per segment request.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for seq := 1; seq <= 20; seq++ {
		w := httptest.NewRecorder()
		s2.serveSegment(w, httptest.NewRequest("GET", "/segment", nil), seq, outputResponse(data))
		if w.Code != http.StatusOK {
			t.Fatalf("seq %d: status=%d", seq, w.Code)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("20 small segment captures allocated %d MiB (want < 32 MiB with grow-on-demand pooling)", allocated>>20)
	}
}
