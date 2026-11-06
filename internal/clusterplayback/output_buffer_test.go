package clusterplayback

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playbackbuffer"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"github.com/braedonsaunders/ether/internal/store"
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
	r := &Runtime{o: Options{OutputBuffer: b, MaxSegmentBytes: int64(len(data) * 2)}, outputScratch: make(chan struct{}, 1), executions: map[string]*execution{}}
	route := Route{Fence: placement.Fence{Session: "retained", Generation: 1}}
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
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.Header().Set("ETag", `"original"`)
		w.WriteHeader(200)
		n, err := w.Write(data)
		return remotetranscode.RepresentationCompletion{StatusCode: 200, ContentLength: int64(len(data)), ReadBytes: int64(len(data)), WrittenBytes: int64(n), BodyPresent: true, EOF: true, Closed: true, WriteError: err != nil || n != len(data)}
	}
}
func TestOutputBufferContiguousAndImmutable(t *testing.T) {
	s, data := outputFixture(t)
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
	for _, test := range []string{"head", "range", "conditional", "206", "error", "short", "unknown", "oversize", "cancel", "type", "content-range", "headers", "scratch", "no-store", "no-cache"} {
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
			case "scratch":
				s.r.outputScratch <- struct{}{}
				defer func() { <-s.r.outputScratch }()
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
				if test == "no-store" || test == "no-cache" {
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
			if calls != 1 || got.Code != want.Code || got.Body.String() != want.Body.String() || !reflect.DeepEqual(got.Header(), want.Header()) {
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
func TestOutputBufferScratchReservedDuringUnknownStream(t *testing.T) {
	s, _ := outputFixture(t)
	q := httptest.NewRequest("GET", "/segment", nil)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s.serveSegment(httptest.NewRecorder(), q, 0, func(w http.ResponseWriter, _ *http.Request) remotetranscode.RepresentationCompletion {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write([]byte("chunk"))
			close(entered)
			<-release
			return remotetranscode.RepresentationCompletion{}
		})
	}()
	<-entered
	if len(s.r.outputScratch) != 1 {
		t.Error("unknown length stream uncharged")
	}
	s.serveSegment(httptest.NewRecorder(), q, 1, outputResponse([]byte("second")))
	close(release)
	<-done
	if len(s.r.outputScratch) != 0 {
		t.Fatal("scratch leaked")
	}
	snap, _ := s.r.o.OutputBuffer.Snapshot("retained")
	if len(snap.Entries) != 0 {
		t.Fatal("scratch miss retained")
	}
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
	db := store.OpenTest(t)
	ledger, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: time.Minute, LockTimeout: time.Second}, outputRetirementVerifier{})
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
	if got.Code != 503 {
		t.Fatal("retired authority served cache", got.Code)
	}
	if err = s.r.ReleaseOutput(ctx, "retained", rec.Fence); err != nil {
		t.Fatal(err)
	}
}

func TestOutputBufferLifetimeCapacity(t *testing.T) {
	s, data := outputFixture(t)
	b, err := playbackbuffer.New(playbackbuffer.Limits{Global: playbackbuffer.Budget{Bytes: int64(len(data)) * 5, Segments: 1, Inits: 1}, PerSession: playbackbuffer.Budget{Bytes: int64(len(data)) * 5, Segments: 1, Inits: 1}, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.OpenSession("retained", playbackbuffer.State{Generation: 1, PublishedFloor: -1}); err != nil {
		t.Fatal(err)
	}
	s.r.o.OutputBuffer = b
	q := httptest.NewRequest("GET", "/segment", nil)
	s.serveSegment(httptest.NewRecorder(), q, 0, outputResponse(data))
	got := httptest.NewRecorder()
	s.serveSegment(got, q, 1, outputResponse(data))
	if got.Code != 200 || got.Body.String() != string(data) {
		t.Fatal("capacity changed ordinary serving")
	}
	snap, err := s.r.SnapshotOutput("retained")
	if err != nil || snap.Retaining || len(snap.Buffer.Entries) != 1 || snap.Buffer.State.PublishedFloor != 0 {
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
	controller := nodewire.Identity{Cluster: "output", NodeID: "controller", Boot: "cboot"}
	worker := nodewire.Identity{Cluster: "output", NodeID: "worker", Boot: "wboot"}
	guards := fixtureGuards(t, controller, worker)
	id := remotetranscode.Identity{Controller: "controller", Worker: "worker", Session: "retained", Generation: 1}
	calls := 0
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
		calls++
		w.Header().Set("Content-Type", "video/mp2t")
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
	for n := 0; n < 3; n++ {
		w := httptest.NewRecorder()
		s.ServeSegment(w, httptest.NewRequest("GET", "/segment", nil), n)
		if w.Code != 200 || w.Body.String() != string(data) {
			t.Fatal("ordinary response changed", n, w.Code)
		}
	}
	snap, err := s.r.o.OutputBuffer.Snapshot("retained")
	if err != nil || len(snap.Entries) != 1 || snap.State.PublishedFloor != 0 || calls != 3 {
		t.Fatal("only complete full TS may publish, exactly one backend read each", snap, calls, err)
	}
}
