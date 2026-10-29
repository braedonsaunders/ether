package capture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readEntry(t *testing.T, dir string, n int) Entry {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%08d.json", n))
	for i := 0; i < 100; i++ {
		b, e := os.ReadFile(path)
		if e == nil {
			var v Entry
			if e = json.Unmarshal(b, &v); e != nil {
				t.Fatal(e)
			}
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("missing capture %d", n)
	return Entry{}
}
func TestProxyRedactsAndForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "secret-token" {
			t.Error("request auth not forwarded")
		}
		if r.URL.Path == "/binary" {
			if r.Header.Get("Range") != "bytes=0-65535" {
				t.Error("range was not forwarded")
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(bytes.Repeat([]byte("A"), 70000))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Name":"lab owner","AccessToken":"secret-token","ServerId":"server-private","DateCreated":"2026-01-02T00:00:00Z","Path":"/private/media","Address":"192.168.2.10","Id":"0123456789abcdef0123456789abcdef"}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	rec, err := New("emby", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/Users/AuthenticateByName?api_key=secret-token", strings.NewReader(`{"Username":"lab owner","Pw":"passphrase"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", "secret-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	forwarded, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(forwarded, []byte("secret-token")) {
		t.Fatal("response changed in transit")
	}
	entry := readEntry(t, dir, 1)
	if !strings.Contains(entry.Request.Path, "api_key=${access_token}") {
		t.Fatal("query token template was not preserved")
	}
	encoded, _ := json.Marshal(entry)
	decoded := append(encoded, entry.Request.Body...)
	decoded = append(decoded, entry.Response.Body...)
	for _, s := range []string{"secret-token", "lab owner", "passphrase", "server-private", "2026-01-02", "/private/media", "192.168.2.10", "0123456789abcdef0123456789abcdef"} {
		if bytes.Contains(decoded, []byte(s)) {
			t.Errorf("capture retained secret type %q", s)
		}
	}
	req, _ = http.NewRequest("GET", srv.URL+"/binary", nil)
	req.Header.Set("X-Emby-Token", "secret-token")
	req.Header.Set("Range", "bytes=0-65535")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != 70000 {
		t.Fatal("binary forwarding truncated")
	}
	entry = readEntry(t, dir, 2)
	if entry.Response.Kind != "binary-range" || len(entry.Response.Body) != 0 || entry.Response.BodyLength != 70000 || !entry.Response.Truncated || entry.Response.SHA256 == "" {
		t.Fatalf("invalid binary capture: %+v", entry.Response)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "00000002.json"))
	if bytes.Contains(stored, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("A"), 30)))) {
		t.Fatal("media bytes were persisted")
	}
	out := filepath.Join(t.TempDir(), "apple.json")
	if err = ProfileFromCapture(dir, "emby", "apple", out); err != nil {
		t.Fatal(err)
	}
	profile, _ := os.ReadFile(out)
	for _, s := range []string{"secret-token", "lab owner", "passphrase"} {
		if bytes.Contains(profile, []byte(s)) {
			t.Fatal("secret leaked to profile")
		}
	}
	if !bytes.Contains(profile, []byte(`"source": "captured"`)) {
		t.Fatal("capture provenance missing")
	}
	if !bytes.Contains(profile, []byte("${username}")) || !bytes.Contains(profile, []byte("${password}")) {
		t.Fatal("credential templates missing")
	}
	loaded, err := mediabrowser.Load("emby", filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Corpus("apple")) != 2 {
		t.Fatal("captured profile is not loadable")
	}
	if loaded.Corpus("apple")[1].RangeKind != "captured" {
		t.Fatal("captured range lost")
	}

}
func TestWebSocketUpgradePassesThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(buf, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buf.Flush()
		b := make([]byte, 4)
		if _, err = io.ReadFull(conn, b); err == nil {
			conn.Write(b)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	rec, err := New("silo", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	conn.Write([]byte("ping"))
	reply := make([]byte, 4)
	if _, err = io.ReadFull(reader, reply); err != nil || string(reply) != "ping" {
		t.Fatalf("upgrade echo: %q %v", reply, err)
	}
	entry := readEntry(t, dir, 1)
	if entry.Response.Status != 101 {
		t.Fatalf("missing upgrade capture")
	}
	if entry.Request.Headers.Get("Sec-WebSocket-Key") != "${websocket_key}" {
		t.Fatal("WebSocket nonce was not templated")
	}
	if entry.Response.Kind != "websocket" {
		t.Fatal("upgrade kind missing")
	}
	out := filepath.Join(t.TempDir(), "apple.json")
	if err = ProfileFromCapture(dir, "silo", "apple", out); err != nil {
		t.Fatal(err)
	}
	profile, err := mediabrowser.Load("silo", filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	response, err := profile.Fetch(context.Background(), upstream.Client(), upstream.URL, "", profile.Corpus("apple")[0], "apple", map[string]string{}, true)
	if err != nil || response.Status != 101 || response.Kind != "websocket" {
		t.Fatalf("upgrade replay failed: %v", err)
	}

}
func TestProfileRejectsCaptureGapsAndIncompleteBodies(t *testing.T) {
	dir := t.TempDir()
	e := Entry{Sequence: 2, Protocol: "silo", Client: "apple", Request: Request{Method: "GET", Path: "/x"}}
	b, _ := json.Marshal(e)
	if err := os.WriteFile(filepath.Join(dir, "00000002.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProfileFromCapture(dir, "silo", "apple", filepath.Join(t.TempDir(), "x.json")); err == nil {
		t.Fatal("sequence gap accepted")
	}
	e.Sequence = 1
	e.RequestTruncated = true
	b, _ = json.Marshal(e)
	os.Remove(filepath.Join(dir, "00000002.json"))
	os.WriteFile(filepath.Join(dir, "00000001.json"), b, 0600)
	if err := ProfileFromCapture(dir, "silo", "apple", filepath.Join(t.TempDir(), "x.json")); err == nil {
		t.Fatal("truncated request accepted")
	}
}
func TestXMLCaptureRedactsPrivateValues(t *testing.T) {
	r := &Recorder{ids: map[string]string{}}
	b := r.sanitize([]byte(`<Server serverId="private-id" dateCreated="2026-01-01" path="/Users/test file"><username>private user</username><title>Film</title></Server>`), nil, "/System/Info/Public")
	for _, s := range []string{"private-id", "2026-01-01", "/Users/test file", "private user"} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatal("XML secret remained")
		}
	}
	if !bytes.Contains(b, []byte("Film")) {
		t.Fatal("ordinary XML data lost")
	}
}
func TestCaptureWriteFailureIsReported(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	base := t.TempDir()
	dir := filepath.Join(base, "captures")
	rec, err := New("silo", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	select {
	case <-rec.Errors():
	case <-time.After(time.Second):
		t.Fatal("capture write failure not reported")
	}
}
func TestFormCredentialsAreTemplated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("emby", "tv", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("username=alice&password=p%26ss"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if bytes.Contains(entry.Request.Body, []byte("alice")) || bytes.Contains(entry.Request.Body, []byte("p%26ss")) {
		t.Fatal("form credentials leaked")
	}
	out := filepath.Join(t.TempDir(), "tv.json")
	if err = ProfileFromCapture(dir, "emby", "tv", out); err != nil {
		t.Fatal(err)
	}
	p, err := mediabrowser.Load("emby", filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Corpus("tv")
	if len(r) != 1 || !bytes.Contains(r[0].Body, []byte("${password}")) {
		t.Fatal("form credential template missing")
	}
}
func TestHLSCaptureRedactsSignedQuery(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-DEFINE:NAME=\"silo_query\",VALUE=\"st=private-signature\"\nsegment.ts?{$silo_query}\n")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("silo", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/playlist.m3u8?st=private-signature")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if bytes.Contains(entry.Response.Body, []byte("private-signature")) {
		t.Fatal("signed query leaked")
	}
	if !bytes.Contains(entry.Response.Body, []byte("#EXTM3U")) {
		t.Fatal("playlist was not captured")
	}
	if !strings.Contains(entry.Request.Path, "st=${stream_token}") {
		t.Fatal("signed query was not templated")
	}
}
func TestMediaPathCannotPersistBytesWhenMislabeledText(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "private media bytes")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("silo", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL + "/segment.ts")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	entry := readEntry(t, dir, 1)
	if len(entry.Response.Body) != 0 || entry.Response.Kind != "binary-range" {
		t.Fatal("media bytes persisted as text")
	}
}
func TestBinaryUploadRecordsOnlyBoundedMetadata(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); w.WriteHeader(204) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("silo", "tv", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/upload", "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte{'x'}, 70000)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if len(entry.Request.Body) != 0 || entry.Request.BodyKind != "binary" || entry.Request.BodyLength != 70000 || entry.Request.SHA256 == "" || !entry.RequestTruncated {
		t.Fatal("binary upload was not safely captured")
	}
}
func TestTextCaptureRedactsDatesAndHostPaths(t *testing.T) {
	r := &Recorder{ids: map[string]string{}}
	b := r.sanitize([]byte("#EXTM3U\n#EXT-X-PROGRAM-DATE-TIME:2026-03-04T05:06:07Z\n/Users/private/movie.ts?st=hidden\n"), nil, "/playlist.m3u8")
	for _, s := range []string{"2026-03-04", "/Users/private", "hidden"} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatalf("text leaked sensitive value")
		}
	}
}
func TestProxyStreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("last"))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("silo", "tv", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/movie.mp4", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	first := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, first)
	close(release)
	if err != nil || string(first) != "first" {
		resp.Body.Close()
		t.Fatalf("first chunk did not stream: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if entry.Response.BodyLength != 9 || len(entry.Response.Body) != 0 {
		t.Fatal("binary stream capture incorrect")
	}
}
func TestCaptureRedactsEncodedEmailInPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	dir := t.TempDir()
	rec, err := New("silo", "apple", dir, u)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/lookup/alice%40example.com")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry := readEntry(t, dir, 1)
	if strings.Contains(strings.ToLower(entry.Request.Path), "alice") || strings.Contains(strings.ToLower(entry.Request.Path), "example.com") {
		t.Fatal("encoded email leaked into path")
	}
}
