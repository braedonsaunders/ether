package livetv

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// guideStream emits an XMLTV document without holding it. Most of the bytes
// are programmes for a channel the playlist does not map, plus a few
// programmes on the mapped channel inside and outside the import window.
type guideStream struct {
	now     time.Time
	bulk    int64
	written int64
	buf     []byte
	pos     int
	phase   int
	n       int
}

func (g *guideStream) Read(p []byte) (int, error) {
	if g.pos >= len(g.buf) {
		if err := g.refill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, g.buf[g.pos:])
	g.pos += n
	g.written += int64(n)
	return n, nil
}

func (g *guideStream) refill() error {
	g.pos = 0
	g.buf = g.buf[:0]
	switch g.phase {
	case 0:
		g.buf = append(g.buf, `<tv><channel id="keep"><display-name>Keep</display-name></channel><channel id="drop"><display-name>Drop</display-name></channel>`...)
		g.buf = append(g.buf, g.prog("keep", g.now, "Inside")...)
		g.buf = append(g.buf, g.prog("keep", g.now.Add(-72*time.Hour), "Old")...)
		g.buf = append(g.buf, g.prog("keep", g.now.Add(20*24*time.Hour), "Future")...)
		g.phase = 1
	case 1:
		if g.written >= g.bulk {
			g.buf = append(g.buf, "</tv>"...)
			g.phase = 2
			return nil
		}
		// ~32KB per skipped programme. The parser must not retain these.
		g.n++
		g.buf = append(g.buf, `<programme channel="drop" start="20200101000000 +0000" stop="20200101010000 +0000"><title>x</title><desc>`...)
		g.buf = append(g.buf, descPad...)
		g.buf = append(g.buf, `</desc></programme>`...)
	default:
		return io.EOF
	}
	return nil
}

var descPad = bytes.Repeat([]byte("A"), 32<<10)

func (g *guideStream) prog(channel string, start time.Time, title string) string {
	stop := start.Add(time.Hour)
	return fmt.Sprintf(`<programme channel="%s" start="%s" stop="%s"><title>%s</title></programme>`,
		channel, start.UTC().Format("20060102150405 -0700"), stop.UTC().Format("20060102150405 -0700"), title)
}

func TestStreamedGuideFiltersChannelsAndWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	gen := &guideStream{now: now, bulk: 300 << 20}
	ids := map[string]struct{}{"keep": {}}
	g, _, err := parseXMLTV(gen, guideFilter{From: now.Add(-guideLookback), Until: now.Add(guideLookahead), IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if gen.written < 300<<20 {
		t.Fatalf("generator wrote %d bytes, want at least 300 MiB", gen.written)
	}
	if len(g.Channels) != 1 || g.Channels["keep"].Name != "Keep" {
		t.Fatalf("channels: %+v", g.Channels)
	}
	if _, ok := g.Channels["drop"]; ok {
		t.Fatal("unmapped channel kept")
	}
	if len(g.Programmes) != 1 || g.Programmes[0].Item.Title != "Inside" {
		t.Fatalf("programmes: %+v", g.Programmes)
	}
}

func TestGzipGuideUploadAndBomb(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	if ownerJSON(t, s, "POST", "/sources?pending=1", `{"id":"up","name":"Upload"}`, nil) != 200 {
		t.Fatal("pending source")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	gen := &guideStream{now: now, bulk: 1 << 20}
	if _, err := io.Copy(zw, gen); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sources/up/playlist", strings.NewReader("#EXTM3U\n#EXTINF:-1 tvg-id=\"keep\",Keep\nhttps://owner.invalid/keep.m3u8\n"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("playlist upload: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/sources/up/guide", bytes.NewReader(buf.Bytes()))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("guide upload: %d %s", rec.Code, rec.Body.String())
	}
	src := sourceByID(t, s, "up")
	if src.ProgrammeCount != 1 || src.GuideChannelCount != 1 || src.ChannelCount != 1 {
		t.Fatalf("trimmed import: %+v", src)
	}
	var raw string
	if err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_sources WHERE id='up'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "Drop") || strings.Contains(raw, "Old") || strings.Contains(raw, "Future") {
		t.Fatal("stored guide kept filtered entries")
	}
	if len(raw) > 64<<10 {
		t.Fatalf("stored source is %d bytes", len(raw))
	}

	pr, pw := io.Pipe()
	go func() {
		zw, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		zeros := make([]byte, 1<<20)
		for i := 0; i < 32; i++ {
			if _, err := zw.Write(zeros); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.CloseWithError(zw.Close())
	}()
	defer pr.Close()
	rd, err := maybeGunzip(pr, maxGuideBytes, "guide")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, rd)
	if err == nil || !strings.Contains(err.Error(), "100:1") {
		t.Fatalf("gzip bomb accepted: %v", err)
	}
}

func TestGuideURLStreamsAndFilters(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	gen := &guideStream{now: now, bulk: 300 << 20}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list.m3u":
			io.WriteString(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"keep\",Keep\nhttps://owner.invalid/keep.m3u8\n")
		case "/guide.xml":
			io.Copy(w, gen)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := testService(t, Options{Now: func() time.Time { return now }, HTTPClient: srv.Client()})
	src, err := s.ImportSource(context.Background(), Source{ID: "url", Name: "URL", PlaylistURL: srv.URL + "/list.m3u", GuideURL: srv.URL + "/guide.xml"})
	if err != nil {
		t.Fatal(err)
	}
	if gen.written < 300<<20 {
		t.Fatalf("guide URL wrote %d bytes", gen.written)
	}
	if src.ProgrammeCount != 1 || src.GuideChannelCount != 1 || src.ChannelCount != 1 {
		t.Fatalf("url import: %+v", src)
	}
	var raw string
	if err := s.o.DB.QueryRow(`SELECT data FROM livetv_sources WHERE id='url'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "<tv") || strings.Contains(raw, "Drop") || len(raw) > 64<<10 {
		t.Fatalf("fetched guide persisted (%d bytes)", len(raw))
	}
}

func TestPlaylistLimitNames64MB(t *testing.T) {
	body := io.MultiReader(strings.NewReader("#EXTM3U\n"), &padReader{left: maxPlaylistBytes})
	_, err := ParseM3U(body, "s")
	if err == nil || !strings.Contains(err.Error(), "64 MB") {
		t.Fatalf("limit error: %v", err)
	}
}

type padReader struct {
	left int64
	nl   bool
}

func (r *padReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.left {
		n = int(r.left)
	}
	for i := 0; i < n; i++ {
		if r.nl {
			p[i] = '\n'
		} else {
			p[i] = '#'
		}
		r.nl = !r.nl
	}
	r.left -= int64(n)
	return n, nil
}
