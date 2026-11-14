package livetv

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestTapStartupBodyIdleAndCancellation(t *testing.T) {
	old := relayIOTimeout
	relayIOTimeout = 40 * time.Millisecond
	defer func() { relayIOTimeout = old }()
	for _, prefix := range []string{"", "#EXTM3U\n"} {
		t.Run(prefix, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(prefix))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer up.Close()
			s := testService(t, Options{HTTPClient: up.Client()})
			u, _ := url.Parse(up.URL)
			g := &inputGrant{origin: relayOrigin(u), headers: http.Header{}, ctx: s.serviceContext()}
			tap, _, _ := s.inputRelay.taps.acquire("test", "", 0, g)
			done := make(chan struct{})
			go func() { s.openTap(tap, g, up.URL, http.Header{}, context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("body sniff or manifest read hung")
			}
			tap.close(nil)
		})
	}
}

func TestTapReadDeadlineWakesWithoutProviderBytes(t *testing.T) {
	tap := &upstreamTap{}
	tap.cond = sync.NewCond(&tap.mu)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := tap.read(ctx, 0, make([]byte, 188)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled reader remained asleep")
	}
}

func TestTapSlowTransportStreamUsesIdleNotTotalTimeout(t *testing.T) {
	old := relayIOTimeout
	relayIOTimeout = 200 * time.Millisecond
	defer func() { relayIOTimeout = old }()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		packet := make([]byte, 188)
		packet[0] = 0x47
		for range 8 {
			if _, err := w.Write(packet); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(40 * time.Millisecond):
			}
		}
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: &http.Client{Timeout: 10 * time.Millisecond}})
	u, _ := url.Parse(up.URL)
	g := &inputGrant{origin: relayOrigin(u), headers: http.Header{}, ctx: s.serviceContext()}
	tap, _, _ := s.inputRelay.taps.acquire("slow", "", 0, g)
	s.openTap(tap, g, up.URL, http.Header{}, context.Background())
	defer tap.close(nil)
	var off int64
	for off < 8*188 {
		n, next, err := tap.read(context.Background(), off, make([]byte, 188))
		off = next
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if off != 8*188 {
		t.Fatalf("lost slow TS bytes: %d", off)
	}
}

func TestTapOpeningDeadlineIncludesFirstBodyByte(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client()})
	g := &inputGrant{origin: up.URL, headers: http.Header{}, ctx: s.serviceContext()}
	tap, _, _ := s.inputRelay.taps.acquire("deadline", "", 0, g)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.openTap(tap, g, up.URL, http.Header{}, ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup deadline ignored after response headers")
	}
	tap.close(nil)
}
