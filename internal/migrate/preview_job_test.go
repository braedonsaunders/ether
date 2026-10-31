package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type lockBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type slowCatalog struct {
	core.Catalog
	delay time.Duration
	mu    sync.Mutex
	calls int
	open  int
	max   int
}

func (c *slowCatalog) Get(ctx context.Context, k core.RatingKey) (*core.Title, error) {
	c.mu.Lock()
	c.calls++
	c.open++
	if c.open > c.max {
		c.max = c.open
	}
	c.mu.Unlock()
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
	}
	c.mu.Lock()
	c.open--
	c.mu.Unlock()
	return &core.Title{Key: k, Title: "Film", Year: 2020, Type: core.TypeMovie}, nil
}

func testService(t *testing.T, catalog core.Catalog, log *slog.Logger) *Service {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at)VALUES(1,99,'viewer',0,0)`); err != nil {
		t.Fatal(err)
	}
	return New(db, catalog, Options{Logger: log})
}

func postPreview(t *testing.T, h http.Handler, body any) (PreviewStatus, time.Duration) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/preview", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("start status %d body %s", rec.Code, rec.Body.String())
	}
	var st PreviewStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st, elapsed
}

func pollPreview(t *testing.T, h http.Handler, id string, limit time.Duration) PreviewStatus {
	t.Helper()
	deadline := time.Now().Add(limit)
	var st PreviewStatus
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/preview/"+id, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("poll status %d body %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.Status != "running" {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("preview still %s after %s (scanned %d)", st.Status, limit, st.Progress.Scanned)
	return st
}

func moviePage(start, total int) string {
	end := start + 500
	if end > total {
		end = total
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<MediaContainer totalSize="%d">`, total)
	for i := start; i < end; i++ {
		id := i + 1
		fmt.Fprintf(&b, `<Video ratingKey="%d" type="movie" title="Film %d" viewCount="1" lastViewedAt="1700000000"><Guid id="tmdb://%d"/></Video>`, id, id, id)
	}
	b.WriteString(`</MediaContainer>`)
	return b.String()
}

func TestLargeSlowPreviewJob(t *testing.T) {
	const total = 2000
	const pageDelay = 600 * time.Millisecond
	catalog := &slowCatalog{delay: 15 * time.Millisecond}
	var logs lockBuf
	log := slog.New(slog.NewTextHandler(&logs, nil))
	const token = "do-not-log-this-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != token {
			http.Error(w, "token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/identity":
			io.WriteString(w, `<MediaContainer machineIdentifier="large-fixture"/>`)
		case "/accounts":
			io.WriteString(w, `<MediaContainer><Account id="1" name="viewer"/></MediaContainer>`)
		case "/library/sections":
			io.WriteString(w, `<MediaContainer><Directory key="1" type="movie" title="Movies"/></MediaContainer>`)
		case "/library/sections/1/all":
			select {
			case <-r.Context().Done():
				return
			case <-time.After(pageDelay):
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
			io.WriteString(w, moviePage(start, total))
		default:
			io.WriteString(w, `<MediaContainer totalSize="0"></MediaContainer>`)
		}
	}))
	defer srv.Close()
	s := testService(t, catalog, log)
	h := s.Handler()
	started := time.Now()
	st, postElapsed := postPreview(t, h, Request{URL: srv.URL, Token: token, Viewers: []Viewer{{LocalUserID: 1, Name: "viewer"}}})
	if postElapsed > 250*time.Millisecond {
		t.Fatalf("POST /preview blocked for %s; a synchronous scan would still be inside the first PMS page", postElapsed)
	}
	if st.Status != "running" || st.ID == "" || st.Plan != nil {
		t.Fatalf("start status %+v", st)
	}
	done := pollPreview(t, h, st.ID, 12*time.Second)
	elapsed := time.Since(started)
	if done.Status != "done" || done.Plan == nil {
		t.Fatalf("finished %+v log %s", done, logs.String())
	}
	if len(done.Plan.Items) != total || done.Progress.Scanned < total {
		t.Fatalf("items=%d scanned=%d", len(done.Plan.Items), done.Progress.Scanned)
	}
	if done.Plan.ID == "" {
		t.Fatal("finished preview was not saved")
	}
	catalog.mu.Lock()
	calls, max := catalog.calls, catalog.max
	catalog.mu.Unlock()
	if calls != total || max <= 1 || max > previewParallel {
		t.Fatalf("catalog calls=%d max in flight=%d", calls, max)
	}
	// One worker would spend about 32s in catalog lookups alone.
	if elapsed > 10*time.Second {
		t.Fatalf("preview took %s; bounded workers should overlap the slow catalog lookups", elapsed)
	}
	text := logs.String()
	if !strings.Contains(text, "plex preview started") || !strings.Contains(text, "plex preview finished") || !strings.Contains(text, "plex preview progress") {
		t.Fatalf("missing progress logs: %s", text)
	}
	if strings.Contains(text, token) || strings.Contains(string(mustJSON(t, done)), token) {
		t.Fatal("preview log or status leaked the PMS token")
	}
}

func TestPreviewGrandparentFetchedOnce(t *testing.T) {
	const episodes = 1000
	const shows = 4
	var parents sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.URL.Path == "/identity":
			io.WriteString(w, `<MediaContainer machineIdentifier="episode-fixture"/>`)
		case r.URL.Path == "/accounts":
			io.WriteString(w, `<MediaContainer><Account id="1" name="viewer"/></MediaContainer>`)
		case r.URL.Path == "/library/sections":
			io.WriteString(w, `<MediaContainer><Directory key="2" type="show" title="TV"/></MediaContainer>`)
		case r.URL.Path == "/library/sections/2/all":
			var b strings.Builder
			fmt.Fprintf(&b, `<MediaContainer totalSize="%d">`, episodes)
			for i := 0; i < episodes; i++ {
				show := (i % shows) + 1
				fmt.Fprintf(&b, `<Video ratingKey="e%d" type="episode" title="Episode %d" grandparentRatingKey="%d" parentIndex="1" index="%d" viewCount="1" lastViewedAt="1700000000"/>`, i, i, show, i+1)
			}
			b.WriteString(`</MediaContainer>`)
			io.WriteString(w, b.String())
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			id := strings.TrimPrefix(r.URL.Path, "/library/metadata/")
			n, _ := parents.LoadOrStore(id, 0)
			parents.Store(id, n.(int)+1)
			fmt.Fprintf(w, `<MediaContainer><Directory ratingKey="%s" type="show"><Guid id="tmdb://%s"/></Directory></MediaContainer>`, id, id)
		default:
			io.WriteString(w, `<MediaContainer totalSize="0"></MediaContainer>`)
		}
	}))
	defer srv.Close()
	s := testService(t, catalogFake{}, nil)
	h := s.Handler()
	st, _ := postPreview(t, h, Request{URL: srv.URL, Token: "secret", Viewers: []Viewer{{LocalUserID: 1, Name: "viewer"}}})
	done := pollPreview(t, h, st.ID, 8*time.Second)
	if done.Status != "done" || done.Plan == nil || len(done.Plan.Items) != episodes {
		t.Fatalf("status=%s items=%d err=%s", done.Status, len(planItems(done.Plan)), done.Error)
	}
	var fetches int
	parents.Range(func(_, v any) bool {
		fetches++
		if v.(int) != 1 {
			t.Errorf("grandparent fetched %d times", v.(int))
		}
		return true
	})
	if fetches != shows {
		t.Fatalf("grandparent shows fetched=%d want %d", fetches, shows)
	}
}

func TestPreviewJobReportsPMSError(t *testing.T) {
	const token = "do-not-log-this-token"
	var logs lockBuf
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			io.WriteString(w, `<MediaContainer machineIdentifier="err-fixture"/>`)
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := testService(t, catalogFake{}, slog.New(slog.NewTextHandler(&logs, nil)))
	st, _ := postPreview(t, s.Handler(), Request{URL: srv.URL, Token: token, Viewers: []Viewer{{LocalUserID: 1, Name: "viewer"}}})
	done := pollPreview(t, s.Handler(), st.ID, 3*time.Second)
	if done.Status != "error" || !strings.Contains(done.Error, "HTTP 500") {
		t.Fatalf("status %+v", done)
	}
	if !strings.Contains(logs.String(), "plex preview failed") || !strings.Contains(logs.String(), "HTTP 500") {
		t.Fatalf("log %s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("error log leaked the PMS token")
	}
}

func TestPreviewJobCancel(t *testing.T) {
	blocked := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			io.WriteString(w, `<MediaContainer machineIdentifier="cancel-fixture"/>`)
			return
		}
		if r.URL.Path == "/library/sections" {
			once.Do(func() { close(blocked) })
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s := testService(t, catalogFake{}, nil)
	h := s.Handler()
	st, postElapsed := postPreview(t, h, Request{URL: srv.URL, Token: "secret", Viewers: []Viewer{{LocalUserID: 1}}})
	if postElapsed > 250*time.Millisecond {
		t.Fatalf("POST /preview waited %s for a blocked PMS", postElapsed)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("preview never reached PMS")
	}
	req := httptest.NewRequest(http.MethodPost, "/preview/"+st.ID+"/cancel", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cancel %d %s", rec.Code, rec.Body.String())
	}
	done := pollPreview(t, h, st.ID, 3*time.Second)
	if done.Status != "cancelled" {
		t.Fatalf("status %+v", done)
	}
}

func planItems(p *Plan) []Item {
	if p == nil {
		return nil
	}
	return p.Items
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
