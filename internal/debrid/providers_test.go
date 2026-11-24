package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const testKey = "test-key-123"

var testHash = strings.Repeat("ab", 20)

func fastOpts(base string) ProviderOptions {
	return ProviderOptions{BaseURL: base, PollInterval: time.Millisecond, ReadyPolls: 3}
}

func acquireReq(cachedOnly bool) AcquireRequest {
	return AcquireRequest{InfoHash: testHash, Magnet: "magnet:?xt=urn:btih:" + testHash,
		Choose: func(fs []File) (File, error) { return ChooseFile(fs, nil) }, CachedOnly: cachedOnly, RemoveUncached: true}
}

// --- Real-Debrid ------------------------------------------------------------

type fakeRD struct {
	mu       sync.Mutex
	cached   bool
	selected string
	adds     int
	deleted  []string
	existing bool // a downloaded torrent with the hash is already on the account
}

func (f *fakeRD) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":"bad_token","error_code":8}`)
				return
			}
			h(w, r)
		}
	}
	files := func(sel string) string {
		s1, s2 := 0, 0
		if sel == "1" || sel == "all" {
			s1 = 1
		}
		if sel == "2" {
			s2 = 1
		}
		return fmt.Sprintf(`[{"id":1,"path":"/Movie.2024/Movie.2024.1080p.mkv","bytes":4000000000,"selected":%d},{"id":2,"path":"/Movie.2024/Sample/sample.mkv","bytes":30000000,"selected":%d}]`, s1, s2)
	}
	mux.HandleFunc("POST /torrents/addMagnet", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if !strings.Contains(r.PostForm.Get("magnet"), testHash) {
			t.Errorf("magnet %q", r.PostForm.Get("magnet"))
		}
		f.mu.Lock()
		f.adds++
		f.mu.Unlock()
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"T1","uri":"x"}`)
	}))
	mux.HandleFunc("GET /torrents", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.existing {
			fmt.Fprintf(w, `[{"id":"OLD","hash":"%s","status":"downloaded"}]`, strings.ToUpper(testHash))
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	mux.HandleFunc("GET /torrents/info/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		switch {
		case id == "OLD":
			fmt.Fprintf(w, `{"id":"OLD","hash":"%s","status":"downloaded","files":%s,"links":["https://rd.example/d/OLD1"]}`, testHash, files("1"))
		case f.selected == "":
			fmt.Fprintf(w, `{"id":"T1","hash":"%s","status":"waiting_files_selection","files":%s,"links":[]}`, testHash, files(""))
		case f.cached:
			fmt.Fprintf(w, `{"id":"T1","hash":"%s","status":"downloaded","files":%s,"links":["https://rd.example/d/L1"]}`, testHash, files(f.selected))
		default:
			fmt.Fprintf(w, `{"id":"T1","hash":"%s","status":"downloading","files":%s,"links":[]}`, testHash, files(f.selected))
		}
	}))
	mux.HandleFunc("POST /torrents/selectFiles/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.selected = r.PostForm.Get("files")
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("DELETE /torrents/delete/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /unrestrict/link", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		link := r.PostForm.Get("link")
		if strings.HasSuffix(link, "GONE") {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"unavailable_file","error_code":24}`)
			return
		}
		fmt.Fprintf(w, `{"download":"https://cdn.example/%s/Movie.mkv","filesize":4000000000,"filename":"Movie.mkv"}`, link[strings.LastIndex(link, "/")+1:])
	}))
	mux.HandleFunc("GET /user", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"username":"owner","type":"premium","expiration":"2030-01-01T00:00:00.000Z"}`)
	}))
	return mux
}

func TestRealDebridCachedFlow(t *testing.T) {
	f := &fakeRD{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newRealDebrid(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	if id, err := p.Find(ctx, testHash); err != nil || id != "" {
		t.Fatalf("find on empty account: %q %v", id, err)
	}
	acq, err := p.Acquire(ctx, acquireReq(true))
	if err != nil {
		t.Fatal(err)
	}
	if acq.TorrentID != "T1" || acq.File.ID != "1" || acq.File.Link != "https://rd.example/d/L1" || !acq.Added || f.selected != "1" {
		t.Fatalf("acquired %+v selected %q", acq, f.selected)
	}
	dl, err := p.Link(ctx, Handle{TorrentID: acq.TorrentID, FileID: acq.File.ID, Link: acq.File.Link})
	if err != nil || dl.URL != "https://cdn.example/L1/Movie.mkv" || dl.Size != 4000000000 || dl.Expires.IsZero() {
		t.Fatalf("link %+v %v", dl, err)
	}
	// Without the stored hoster link, the link is looked up from the torrent.
	if dl, err = p.Link(ctx, Handle{TorrentID: "T1", FileID: "1"}); err != nil || !strings.Contains(dl.URL, "L1") {
		t.Fatalf("lookup link %+v %v", dl, err)
	}
	if _, err := p.Link(ctx, Handle{TorrentID: "T1", FileID: "1", Link: "https://rd.example/d/GONE"}); !errors.Is(err, ErrGone) {
		t.Fatalf("gone link: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a cached torrent was deleted: %v", f.deleted)
	}
	a, err := p.Account(ctx)
	if err != nil || !a.Premium || a.User != "owner" {
		t.Fatalf("account %+v %v", a, err)
	}
}

func TestRealDebridUncachedIsRemoved(t *testing.T) {
	f := &fakeRD{cached: false}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newRealDebrid(testKey, fastOpts(srv.URL))
	_, err := p.Acquire(context.Background(), acquireReq(true))
	if !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("want not cached, got %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "T1" {
		t.Fatalf("uncached torrent not removed: %v", f.deleted)
	}
	// Not cached-only: the torrent is left to download.
	f.selected, f.deleted = "", nil
	if _, err = p.Acquire(context.Background(), acquireReq(false)); !errors.Is(err, core.ErrNotCached) || len(f.deleted) != 0 {
		t.Fatalf("download mode: %v deleted %v", err, f.deleted)
	}
}

// Real-Debrid's filename filter answers unrestrict with infringing_file for
// a torrent that reached "downloaded". The release must count as not cached
// on that account, leave no torrent behind and not be promoted or retried.
func TestRealDebridInfringingFileIsNotCached(t *testing.T) {
	f := &fakeRD{cached: true}
	base := f.handler(t)
	var unrestricts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unrestrict/link" {
			f.mu.Lock()
			unrestricts++
			f.mu.Unlock()
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"infringing_file","error_code":35}`)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p := newRealDebrid(testKey, fastOpts(srv.URL))
	cfg := config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{{ID: "rd", Kind: "realdebrid", APIKey: testKey, Enabled: true}}}
	svc := NewService(cfg, Options{NewProvider: func(config.DebridProvider, ProviderOptions) (Provider, error) { return p, nil }})
	rel := core.Release{ID: "r1", Name: "Movie.2024.1080p", Protocol: core.ProtocolTorrent, InfoHash: testHash}

	_, err := svc.Acquire(t.Context(), rel)
	if !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("want not cached, got %v", err)
	}
	if svc.KnownCached(testHash) {
		t.Fatal("a blocked file is still promoted as known cached")
	}
	f.mu.Lock()
	deleted := append([]string(nil), f.deleted...)
	f.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "T1" {
		t.Fatalf("blocked torrent left on the account: deleted %v", deleted)
	}
	st := svc.Stats()[0]
	if st.Blocked != 1 || st.Failures != 0 || st.State != "ok" {
		t.Fatalf("stats %+v", st)
	}

	// The next Play skips the release on this account without API calls.
	if _, err := svc.Acquire(t.Context(), rel); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("second play: %v", err)
	}
	f.mu.Lock()
	adds, calls := f.adds, unrestricts
	f.mu.Unlock()
	if adds != 1 || calls != 1 {
		t.Fatalf("blocked release retried: adds=%d unrestricts=%d", adds, calls)
	}
}

func TestRealDebridReusesExistingTorrentAndRejectsBadKey(t *testing.T) {
	f := &fakeRD{existing: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newRealDebrid(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	id, err := p.Find(ctx, testHash)
	if err != nil || id != "OLD" {
		t.Fatalf("find %q %v", id, err)
	}
	req := acquireReq(true)
	req.TorrentID = id
	acq, err := p.Acquire(ctx, req)
	if err != nil || acq.Added || acq.File.Link != "https://rd.example/d/OLD1" || f.adds != 0 {
		t.Fatalf("reuse %+v %v adds %d", acq, err, f.adds)
	}
	bad := newRealDebrid("wrong", fastOpts(srv.URL))
	if _, err := bad.Account(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("bad key: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "wrong") {
		t.Fatal("error leaks the key")
	}
}

// --- TorBox ----------------------------------------------------------------

type fakeTorBox struct {
	mu      sync.Mutex
	cached  bool
	created int
	deleted []string
	cachedQ string
}

func (f *fakeTorBox) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, data string) {
		fmt.Fprintf(w, `{"success":true,"error":null,"detail":"ok","data":%s}`, data)
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"success":false,"error":"BAD_TOKEN","detail":"invalid token","data":null}`)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /torrents/checkcached", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cachedQ = r.URL.Query().Get("hash")
		cached := f.cached
		f.mu.Unlock()
		if cached {
			ok(w, fmt.Sprintf(`{"%s":{"name":"Show.S01","size":9000000000,"hash":"%s"}}`, testHash, testHash))
			return
		}
		ok(w, `{}`)
	}))
	mux.HandleFunc("POST /torrents/createtorrent", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil || !strings.Contains(r.FormValue("magnet"), testHash) {
			t.Errorf("createtorrent form: %v", err)
		}
		f.mu.Lock()
		if r.FormValue("add_only_if_cached") == "true" && !f.cached {
			f.mu.Unlock()
			fmt.Fprint(w, `{"success":false,"error":"TORRENT_NOT_CACHED","detail":"not cached","data":null}`)
			return
		}
		f.created++
		f.mu.Unlock()
		ok(w, `{"torrent_id":77,"hash":"x","auth_id":"y"}`)
	}))
	mux.HandleFunc("GET /torrents/mylist", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		cached := f.cached
		f.mu.Unlock()
		files := `[{"id":0,"name":"Show.S01/Show.S01E01.mkv","size":1500000000},{"id":1,"name":"Show.S01/Show.S01E02.mkv","size":1400000000},{"id":2,"name":"Show.S01/Show.S01E02.nfo","size":100}]`
		if r.URL.Query().Get("id") == "" {
			ok(w, fmt.Sprintf(`[{"id":77,"hash":"%s","download_present":%v,"files":%s}]`, testHash, cached, files))
			return
		}
		ok(w, fmt.Sprintf(`{"id":77,"hash":"%s","download_present":%v,"download_finished":%v,"files":%s}`, testHash, cached, cached, files))
	}))
	mux.HandleFunc("GET /torrents/requestdl", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("token") != testKey || q.Get("torrent_id") != "77" {
			t.Errorf("requestdl query %v", q)
		}
		ok(w, fmt.Sprintf(`"https://cdn.torbox.example/%s"`, q.Get("file_id")))
	}))
	mux.HandleFunc("POST /torrents/controltorrent", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.deleted = append(f.deleted, fmt.Sprint(body["torrent_id"], ":", body["operation"]))
		f.mu.Unlock()
		ok(w, `null`)
	}))
	mux.HandleFunc("GET /user/me", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, `{"email":"o@example.com","plan":2}`)
	}))
	return mux
}

func TestTorBoxFlow(t *testing.T) {
	f := &fakeTorBox{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newTorBox(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	other := strings.Repeat("cd", 20)
	got, err := p.CheckCached(ctx, []string{testHash, other})
	if err != nil || !got[testHash] || got[other] {
		t.Fatalf("checkcached %v %v", got, err)
	}
	req := acquireReq(true)
	req.Choose = func(fs []File) (File, error) { return ChooseFile(fs, &core.ReleaseTarget{Season: 1, Episode: 2}) }
	acq, err := p.Acquire(ctx, req)
	if err != nil || acq.TorrentID != "77" || acq.File.ID != "1" || acq.File.Size != 1400000000 {
		t.Fatalf("acquire %+v %v", acq, err)
	}
	if f.cachedQ != testHash+","+other {
		t.Fatal("Acquire repeated the bulk cache check on the Play path")
	}
	dl, err := p.Link(ctx, Handle{TorrentID: "77", FileID: "1", Size: 1400000000})
	if err != nil || dl.URL != "https://cdn.torbox.example/1" {
		t.Fatalf("link %+v %v", dl, err)
	}
	if id, err := p.Find(ctx, testHash); err != nil || id != "77" {
		t.Fatalf("find %q %v", id, err)
	}
	if a, err := p.Account(ctx); err != nil || !a.Premium {
		t.Fatalf("account %+v %v", a, err)
	}
}

func TestTorBoxCacheExpiryBeforeCreateCannotStartDownload(t *testing.T) {
	f := &fakeTorBox{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newTorBox(testKey, fastOpts(srv.URL))
	cached, err := p.CheckCached(t.Context(), []string{testHash})
	if err != nil || !cached[testHash] {
		t.Fatalf("initial cache check: %v %v", cached, err)
	}
	f.mu.Lock()
	f.cached = false
	f.mu.Unlock()
	if _, err := p.Acquire(t.Context(), acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("expired cache returned %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != 0 {
		t.Fatal("an uncached torrent was created after its cached result expired")
	}
}

func TestTorBoxUncachedSkipsWithoutAdding(t *testing.T) {
	f := &fakeTorBox{cached: false}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newTorBox(testKey, fastOpts(srv.URL))
	if _, err := p.Acquire(context.Background(), acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("got %v", err)
	}
	if f.created != 0 {
		t.Fatal("an uncached torrent was added in cached-only mode")
	}
	if err := p.Remove(context.Background(), "77"); err != nil || len(f.deleted) != 1 || f.deleted[0] != "77:delete" {
		t.Fatalf("remove %v %v", err, f.deleted)
	}
	if _, err := newTorBox("nope", fastOpts(srv.URL)).Account(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("bad key: %v", err)
	}
}

// --- AllDebrid ---------------------------------------------------------------

type fakeAD struct {
	mu      sync.Mutex
	ready   bool
	deleted []string
}

func (f *fakeAD) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, data string) { fmt.Fprintf(w, `{"status":"success","data":%s}`, data) }
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("agent") != "ether" {
				t.Errorf("missing agent on %s", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer "+testKey {
				fmt.Fprint(w, `{"status":"error","error":{"code":"AUTH_BAD_APIKEY","message":"The auth apikey is invalid"}}`)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /v4/magnet/upload", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if !strings.Contains(r.PostForm.Get("magnets[]"), testHash) {
			t.Errorf("upload form %v", r.PostForm)
		}
		ok(w, fmt.Sprintf(`{"magnets":[{"magnet":"m","hash":"%s","name":"Movie","size":1,"ready":%v,"id":555}]}`, testHash, f.ready))
	}))
	mux.HandleFunc("GET /v4.1/magnet/status", auth(func(w http.ResponseWriter, r *http.Request) {
		code := 1
		if f.ready {
			code = 4
		}
		if r.URL.Query().Get("id") != "" {
			ok(w, fmt.Sprintf(`{"magnets":{"id":555,"hash":"%s","statusCode":%d}}`, testHash, code))
			return
		}
		ok(w, fmt.Sprintf(`{"magnets":[{"id":555,"hash":"%s","statusCode":%d}]}`, testHash, code))
	}))
	mux.HandleFunc("POST /v4/magnet/files", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, `{"magnets":[{"id":"555","files":[{"n":"Movie.2024","e":[{"n":"Movie.2024.mkv","s":3000000000,"l":"https://alldebrid.com/f/abc"},{"n":"Sample","e":[{"n":"sample.mkv","s":2000,"l":"https://alldebrid.com/f/s"}]}]}]}]}`)
	}))
	mux.HandleFunc("POST /v4/link/unlock", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("link") == "https://alldebrid.com/f/dead" {
			fmt.Fprint(w, `{"status":"error","error":{"code":"LINK_DOWN","message":"down"}}`)
			return
		}
		ok(w, `{"link":"https://cdn.alldebrid.example/abc/Movie.2024.mkv","filesize":3000000000}`)
	}))
	mux.HandleFunc("POST /v4/magnet/delete", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PostForm.Get("id"))
		f.mu.Unlock()
		ok(w, `{"message":"deleted"}`)
	}))
	mux.HandleFunc("GET /v4/user", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, `{"user":{"username":"o","isPremium":true,"premiumUntil":1900000000}}`)
	}))
	return mux
}

func TestAllDebridFlow(t *testing.T) {
	f := &fakeAD{ready: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newAllDebrid(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	acq, err := p.Acquire(ctx, acquireReq(true))
	if err != nil || acq.TorrentID != "555" || acq.File.Path != "Movie.2024/Movie.2024.mkv" || acq.File.Link != "https://alldebrid.com/f/abc" {
		t.Fatalf("acquire %+v %v", acq, err)
	}
	dl, err := p.Link(ctx, Handle{TorrentID: "555", Link: acq.File.Link})
	if err != nil || dl.Size != 3000000000 {
		t.Fatalf("link %+v %v", dl, err)
	}
	if dl, err = p.Link(ctx, Handle{TorrentID: "555", Path: "Movie.2024/Movie.2024.mkv", Size: 3000000000}); err != nil || dl.URL == "" {
		t.Fatalf("relink by path %+v %v", dl, err)
	}
	if _, err = p.Link(ctx, Handle{Link: "https://alldebrid.com/f/dead"}); !errors.Is(err, ErrGone) {
		t.Fatalf("dead link: %v", err)
	}
	if id, _ := p.Find(ctx, testHash); id != "555" {
		t.Fatalf("find %q", id)
	}
	if _, err := newAllDebrid("x", fastOpts(srv.URL)).Account(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("auth: %v", err)
	}
}

func TestAllDebridNotReadyIsRemoved(t *testing.T) {
	f := &fakeAD{ready: false}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newAllDebrid(testKey, fastOpts(srv.URL))
	if _, err := p.Acquire(context.Background(), acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("got %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "555" {
		t.Fatalf("deleted %v", f.deleted)
	}
}

// --- Premiumize --------------------------------------------------------------

func premiumizeServer(t *testing.T, cached bool, creates *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("apikey")
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			key = r.PostForm.Get("apikey")
		}
		if key != testKey {
			fmt.Fprint(w, `{"status":"error","message":"Not logged in."}`)
			return
		}
		switch r.URL.Path {
		case "/cache/check":
			items := r.URL.Query()["items[]"]
			resp := make([]bool, len(items))
			for i, h := range items {
				resp[i] = cached && h == testHash
			}
			b, _ := json.Marshal(resp)
			fmt.Fprintf(w, `{"status":"success","response":%s}`, b)
		case "/transfer/directdl":
			fmt.Fprint(w, `{"status":"success","content":[{"path":"Movie/Movie.mkv","size":2500000000,"link":"https://pm.example/dl/Movie.mkv","stream_link":"x"},{"path":"Movie/movie.nfo","size":10,"link":"https://pm.example/dl/nfo"}]}`)
		case "/transfer/create":
			*creates++
			fmt.Fprint(w, `{"status":"success","id":"x"}`)
		case "/account/info":
			fmt.Fprint(w, `{"status":"success","customer_id":123,"premium_until":4102444800}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
}

func TestPremiumizeFlow(t *testing.T) {
	creates := 0
	srv := premiumizeServer(t, true, &creates)
	defer srv.Close()
	p := newPremiumize(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	acq, err := p.Acquire(ctx, acquireReq(true))
	if err != nil || acq.TorrentID != "" || acq.File.Size != 2500000000 {
		t.Fatalf("acquire %+v %v", acq, err)
	}
	dl, err := p.Link(ctx, Handle{Magnet: "magnet:?xt=urn:btih:" + testHash, Path: acq.File.Path, Size: acq.File.Size})
	if err != nil || dl.URL != "https://pm.example/dl/Movie.mkv" {
		t.Fatalf("link %+v %v", dl, err)
	}
	if _, err := p.Find(ctx, testHash); !errors.Is(err, ErrUnsupported) {
		t.Fatal("premiumize has no torrent list")
	}
	if a, err := p.Account(ctx); err != nil || !a.Premium {
		t.Fatalf("account %+v %v", a, err)
	}
	if _, err := newPremiumize("bad", fastOpts(srv.URL)).Account(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("auth %v", err)
	}
}

func TestPremiumizeUncached(t *testing.T) {
	creates := 0
	srv := premiumizeServer(t, false, &creates)
	defer srv.Close()
	p := newPremiumize(testKey, fastOpts(srv.URL))
	if _, err := p.Acquire(context.Background(), acquireReq(true)); !errors.Is(err, core.ErrNotCached) || creates != 0 {
		t.Fatalf("cached-only: %v creates %d", err, creates)
	}
	if _, err := p.Acquire(context.Background(), acquireReq(false)); !errors.Is(err, core.ErrNotCached) || creates != 1 {
		t.Fatalf("download mode: %v creates %d", err, creates)
	}
}

// --- shared ------------------------------------------------------------------

func TestNewProviderAndErrorsNeverLeakKeys(t *testing.T) {
	for _, kind := range config.DebridKinds {
		p, err := NewProvider(config.DebridProvider{Kind: kind, APIKey: "k"}, ProviderOptions{})
		if err != nil || p.Kind() != kind {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if _, err := NewProvider(config.DebridProvider{Kind: "realdebrid"}, ProviderOptions{}); !errors.Is(err, ErrAuth) {
		t.Fatal("missing key accepted")
	}
	if _, err := NewProvider(config.DebridProvider{Kind: "nope", APIKey: "k"}, ProviderOptions{}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// A transport error must not echo the URL (TorBox and Premiumize put
	// the key in the query string).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
	}))
	defer srv.Close()
	p := newPremiumize("secret-key-xyz", fastOpts(srv.URL))
	_, err := p.CheckCached(context.Background(), []string{testHash})
	if err == nil || strings.Contains(err.Error(), "secret-key-xyz") {
		t.Fatalf("error %v", err)
	}

}

// A permanent per-release block must not trip the account-wide add pause.
func TestRealDebridBlockedReleasesDoNotPauseAdds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h1, h2, h3 := hashN(1), hashN(2), hashN(3)
	remote, rd := newRefusingRD(t, h1, h2)
	svc := refusalService(t, rd, nil, true, &now)
	for _, hash := range []string{h1, h2} {
		rel := core.Release{ID: hash, Name: "Movie.2024.1080p", Protocol: core.ProtocolTorrent, InfoHash: hash}
		if _, err := svc.Acquire(t.Context(), rel); !errors.Is(err, core.ErrNotCached) {
			t.Fatalf("blocked release: %v", err)
		}
	}
	good := core.Release{ID: h3, Name: "Movie.2024.1080p", Protocol: core.ProtocolTorrent, InfoHash: h3}
	if _, err := svc.Acquire(t.Context(), good); err != nil {
		t.Fatalf("next release was prevented from playing: %v", err)
	}
	now = now.Add(24 * time.Hour)
	blocked := good
	blocked.ID, blocked.InfoHash = h1, h1
	_, _ = svc.Acquire(t.Context(), blocked)
	_, _ = svc.QueueDownload(t.Context(), blocked)
	if remote.addCount(h1) != 1 || remote.addCount(h2) != 1 || remote.addCount(h3) != 1 {
		t.Fatal("a permanently blocked release was retried")
	}
}

// --- Debrid-Link ---------------------------------------------------------------

// seasonPack is a three-file season pack whose wanted episode (S01E02) is
// neither the first nor the largest file.
var seasonPack = []struct {
	name string
	size int64
}{
	{"Show.S01/Show.S01E01.1080p.mkv", 1500000000},
	{"Show.S01/Show.S01E02.1080p.mkv", 1400000000},
	{"Show.S01/Show.S01E02.nfo", 100},
}

func episodeReq(cachedOnly bool) AcquireRequest {
	req := acquireReq(cachedOnly)
	req.Choose = func(fs []File) (File, error) { return ChooseFile(fs, &core.ReleaseTarget{Season: 1, Episode: 2}) }
	return req
}

type fakeDL struct {
	mu       sync.Mutex
	cached   bool // the hash is cached on Debrid-Link
	stalls   bool // an accepted torrent never reaches 100%
	srvMaint bool // the torrent's server reports maintenance
	torrents map[string]bool
	adds     []string // url= of each add
	deleted  []string
	lists    int
	errCode  string // forced error code for every authenticated call
	errHTTP  int
}

func (f *fakeDL) torrentJSON(id string, zip bool) string {
	pct := 10
	if f.cached && !f.stalls {
		pct = 100
	}
	var files []string
	if zip {
		files = append(files, fmt.Sprintf(`{"id":"%s-z","name":"Show.S01.zip","size":2900000100,"downloadUrl":"https://dl.example/zip","downloadPercent":%d}`, id, pct))
	} else {
		for i, sf := range seasonPack {
			files = append(files, fmt.Sprintf(`{"id":"%s-%d","name":"%s","size":%d,"downloadUrl":"https://seed1.debrid.example/dl/%s-%d","downloadPercent":%d}`,
				id, i+1, sf.name, sf.size, id, i+1, pct))
		}
	}
	status := 4
	if pct == 100 {
		status = 100
	}
	return fmt.Sprintf(`{"id":"%s","name":"Show.S01","hashString":"%s","status":%d,"downloadPercent":%d,"isZip":%v,"srvMaint":%v,"files":[%s]}`,
		id, testHash, status, pct, zip, f.srvMaint, strings.Join(files, ","))
}

func (f *fakeDL) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, v string) { fmt.Fprintf(w, `{"success":true,"value":%s}`, v) }
	fail := func(w http.ResponseWriter, status int, code string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"success":false,"error":"%s","error_description":"%s happened"}`, code, code)
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey {
				fail(w, 401, "badToken")
				return
			}
			f.mu.Lock()
			code, status := f.errCode, f.errHTTP
			f.mu.Unlock()
			if code != "" {
				fail(w, status, code)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /seedbox/add", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		src := r.PostForm.Get("url")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.adds = append(f.adds, src)
		if !strings.Contains(src, testHash) {
			t.Errorf("add url %q", src)
		}
		// A bare hash is only added when it is cached.
		if src == testHash && !f.cached {
			fail(w, 400, "notAddTorrent")
			return
		}
		if f.torrents == nil {
			f.torrents = map[string]bool{}
		}
		f.torrents["T9"] = true
		// Many-file torrents come back zipped; ?ids= lists every file.
		ok(w, f.torrentJSON("T9", true))
	}))
	mux.HandleFunc("GET /seedbox/list", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists++
		ids := r.URL.Query().Get("ids")
		if ids != "" && !f.torrents[ids] {
			ok(w, `[]`)
			return
		}
		if ids == "" && r.URL.Query().Get("perPage") != "100" {
			t.Errorf("list query %v", r.URL.Query())
		}
		if len(f.torrents) == 0 {
			ok(w, `[]`)
			return
		}
		ok(w, "["+f.torrentJSON("T9", false)+"]")
	}))
	mux.HandleFunc("DELETE /seedbox/{ids}/remove", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("ids")
		f.deleted = append(f.deleted, id)
		delete(f.torrents, id)
		ok(w, fmt.Sprintf(`["%s"]`, id))
	}))
	mux.HandleFunc("GET /account/infos", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, `{"username":"owner","accountType":1,"premiumLeft":3628800}`)
	}))
	return mux
}

func TestDebridLinkCachedSeasonPackFlow(t *testing.T) {
	f := &fakeDL{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newDebridLink(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	if _, err := p.CheckCached(ctx, []string{testHash}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("debrid-link v2 has no bulk cache check: %v", err)
	}
	if id, err := p.Find(ctx, testHash); err != nil || id != "" {
		t.Fatalf("find on empty account: %q %v", id, err)
	}
	acq, err := p.Acquire(ctx, episodeReq(true))
	if err != nil {
		t.Fatal(err)
	}
	if acq.TorrentID != "T9" || acq.File.ID != "T9-2" || acq.File.Size != 1400000000 || !acq.Added {
		t.Fatalf("acquired %+v", acq)
	}
	// Cached-only adds send the bare hash, which Debrid-Link adds only when cached.
	if len(f.adds) != 1 || f.adds[0] != testHash {
		t.Fatalf("adds %v", f.adds)
	}
	lists := f.lists
	dl, err := p.Link(ctx, Handle{TorrentID: acq.TorrentID, FileID: acq.File.ID, Size: acq.File.Size})
	if err != nil || dl.URL != "https://seed1.debrid.example/dl/T9-2" || dl.Expires.IsZero() || f.lists != lists {
		t.Fatalf("first link %+v %v (lists %d -> %d)", dl, err, lists, f.lists)
	}
	// A later refresh re-reads the torrent rather than trusting a stored URL.
	if dl, err = p.Link(ctx, Handle{TorrentID: "T9", FileID: "T9-2"}); err != nil || dl.URL != "https://seed1.debrid.example/dl/T9-2" || dl.Size != 1400000000 || f.lists != lists+1 {
		t.Fatalf("refreshed link %+v %v", dl, err)
	}
	if _, err := p.Link(ctx, Handle{TorrentID: "T9", FileID: "T9-7"}); !errors.Is(err, ErrGone) {
		t.Fatalf("missing file: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a cached torrent was deleted: %v", f.deleted)
	}
	// A fresh provider finds the torrent on the account and reuses it.
	p2 := newDebridLink(testKey, fastOpts(srv.URL))
	id, err := p2.Find(ctx, testHash)
	if err != nil || id != "T9" {
		t.Fatalf("find %q %v", id, err)
	}
	req := episodeReq(true)
	req.TorrentID = id
	if acq, err := p2.Acquire(ctx, req); err != nil || acq.Added || acq.File.ID != "T9-2" || len(f.adds) != 1 {
		t.Fatalf("reuse %+v %v adds %v", acq, err, f.adds)
	}
	if err := p.Remove(ctx, "T9"); err != nil || len(f.deleted) != 1 || f.deleted[0] != "T9" {
		t.Fatalf("remove %v %v", err, f.deleted)
	}
	if _, err := p.Link(ctx, Handle{TorrentID: "T9", FileID: "T9-2"}); !errors.Is(err, ErrGone) {
		t.Fatalf("link after remove: %v", err)
	}
	a, err := p.Account(ctx)
	if err != nil || !a.Premium || a.User != "owner" || a.Expires.IsZero() {
		t.Fatalf("account %+v %v", a, err)
	}
}

func TestDebridLinkUncachedIsRemoved(t *testing.T) {
	f := &fakeDL{cached: false}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newDebridLink(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	// Cached-only: the bare hash is refused, so nothing reaches the account.
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("want not cached, got %v", err)
	}
	if len(f.torrents) != 0 || len(f.deleted) != 0 {
		t.Fatalf("uncached hash reached the account: %v %v", f.torrents, f.deleted)
	}
	// Accepted but never complete: removed again.
	f.cached, f.stalls = true, true
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("stalled torrent: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "T9" {
		t.Fatalf("not-ready torrent not removed: %v", f.deleted)
	}
	// Not cached-only: the magnet is added and left to download.
	f.cached, f.stalls, f.deleted = false, false, nil
	if _, err := p.Acquire(ctx, acquireReq(false)); !errors.Is(err, core.ErrNotCached) || len(f.deleted) != 0 {
		t.Fatalf("download mode: %v deleted %v", err, f.deleted)
	}
	if last := f.adds[len(f.adds)-1]; !strings.HasPrefix(last, "magnet:") {
		t.Fatalf("download mode sent %q, want the magnet", last)
	}
}

func TestDebridLinkErrors(t *testing.T) {
	f := &fakeDL{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctx := context.Background()
	bad := newDebridLink("wrong-key-abc", fastOpts(srv.URL))
	_, err := bad.Account(ctx)
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "wrong-key-abc") {
		t.Fatalf("bad key: %v", err)
	}
	p := newDebridLink(testKey, fastOpts(srv.URL))
	for _, c := range []struct {
		code   string
		status int
		want   error
	}{
		{"floodDetected", 400, ErrRateLimited},
		{"maxTorrent", 400, ErrRateLimited},
		{"unverifiedEmail", 403, ErrAuth},
		{"badId", 400, ErrGone},
		{"internalError", 500, nil},
	} {
		f.mu.Lock()
		f.errCode, f.errHTTP = c.code, c.status
		f.mu.Unlock()
		_, err := p.Account(ctx)
		if c.want == nil {
			if err == nil || errors.Is(err, ErrAuth) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrGone) {
				t.Errorf("%s: want a transient error, got %v", c.code, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.code, err, c.want)
		}
	}
	// HTTP 429 is rate limiting whatever the body says.
	f.mu.Lock()
	f.errCode, f.errHTTP = "floodDetected", 429
	f.mu.Unlock()
	if _, err := p.Account(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	// An infringing torrent refused at add time is blocked for that
	// release, like Real-Debrid; it does not pause adds on the account.
	f.mu.Lock()
	f.errCode, f.errHTTP = "infringingFile", 451
	f.mu.Unlock()
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, ErrBlocked) || errors.Is(err, ErrAddRefused) || !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("infringing add: %v", err)
	}
}

// Two infringing releases in a row must not keep the next release on the
// same Debrid-Link account from playing.
func TestDebridLinkBlockedReleasesDoNotPauseAdds(t *testing.T) {
	f := &fakeDL{cached: true, errCode: "infringingFile", errHTTP: 451}
	_, svc, _, _ := dlService(t, f)
	for _, hash := range []string{hashN(1), hashN(2)} {
		rel := dlRelease()
		rel.ID, rel.InfoHash = hash, hash
		if _, err := svc.Acquire(t.Context(), rel); !errors.Is(err, core.ErrNotCached) {
			t.Fatalf("blocked release: %v", err)
		}
	}
	f.mu.Lock()
	f.errCode, f.errHTTP = "", 0
	f.mu.Unlock()
	if _, err := svc.Acquire(t.Context(), dlRelease()); err != nil {
		t.Fatalf("next release was prevented from playing: %v", err)
	}
}

// --- Offcloud ------------------------------------------------------------------

type fakeOC struct {
	mu        sync.Mutex
	cached    bool   // /cache/info answer
	state     string // cloud download state after the add ("" = downloaded)
	limited   bool
	created   int
	explores  int
	removed   []string
	cacheURLs []string
}

func (f *fakeOC) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testKey || r.URL.Query().Get("key") != testKey {
				fmt.Fprint(w, `{"error":"NOAUTH"}`)
				return
			}
			f.mu.Lock()
			limited := f.limited
			f.mu.Unlock()
			if limited {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":"Too many requests"}`)
				return
			}
			if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s: content type %q", r.URL.Path, r.Header.Get("Content-Type"))
			}
			h(w, r)
		}
	}
	state := func() string {
		if f.state == "" {
			return "downloaded"
		}
		return f.state
	}
	mux.HandleFunc("POST /cache/info", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URLs []string `json:"urls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cacheURLs = body.URLs
		items := make([]string, len(body.URLs))
		for i, u := range body.URLs {
			items[i] = fmt.Sprintf(`{"cached":%v}`, f.cached && strings.Contains(u, testHash))
		}
		fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
	}))
	mux.HandleFunc("POST /cloud", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body.URL, testHash) {
			t.Errorf("cloud url %q", body.URL)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.created++
		fmt.Fprintf(w, `{"requestId":"R1","fileName":"Show.S01","status":"%s","originalLink":"%s"}`, state(), body.URL)
	}))
	mux.HandleFunc("POST /cloud/status", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID string `json:"requestId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"status":{"requestId":"%s","status":"%s"}}`, body.RequestID, state())
	}))
	mux.HandleFunc("GET /cloud/explore/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "detailed" {
			t.Errorf("explore query %v", r.URL.Query())
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.explores++
		if r.PathValue("id") != "R1" || len(f.removed) > 0 {
			fmt.Fprint(w, `{"error":"Request not found"}`)
			return
		}
		var files []string
		for i, sf := range seasonPack {
			files = append(files, fmt.Sprintf(`{"id":"f%d","name":"%s","size":%d,"path":"%s","url":"https://oc1.offcloud.example/cloud/download/R1/%d"}`,
				i+1, sf.name[strings.LastIndex(sf.name, "/")+1:], sf.size, sf.name, i+1))
		}
		fmt.Fprint(w, `{"files":[`+strings.Join(files, ",")+`]}`)
	}))
	mux.HandleFunc("GET /cloud/history", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"requestId":"R1","fileName":"Show.S01","status":"downloaded","originalLink":"magnet:?xt=urn:btih:%s&dn=Show","createdOn":"2026-01-01T00:00:00.000Z"},{"requestId":"R0","status":"error","originalLink":"magnet:?xt=urn:btih:%s"}]`,
			strings.ToUpper(testHash), strings.Repeat("cd", 20))
	}))
	mux.HandleFunc("POST /cloud/remove", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestIDs []string `json:"requestIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.removed = append(f.removed, body.RequestIDs...)
		f.mu.Unlock()
		fmt.Fprint(w, `{"success":true}`)
	}))
	mux.HandleFunc("GET /account/info", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"user_id":"u-1","is_premium":true,"expiration_date":"2099-12-31","can_download":true}`)
	}))
	return mux
}

func TestOffcloudCachedSeasonPackFlow(t *testing.T) {
	f := &fakeOC{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newOffcloud(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	other := strings.Repeat("ef", 20)
	got, err := p.CheckCached(ctx, []string{testHash, other})
	if err != nil || !got[testHash] || got[other] {
		t.Fatalf("checkcached %v %v", got, err)
	}
	if len(f.cacheURLs) != 2 || f.cacheURLs[0] != "magnet:?xt=urn:btih:"+testHash {
		t.Fatalf("cache urls %v", f.cacheURLs)
	}
	acq, err := p.Acquire(ctx, episodeReq(true))
	if err != nil {
		t.Fatal(err)
	}
	if acq.TorrentID != "R1" || acq.File.ID != "f2" || acq.File.Path != "Show.S01/Show.S01E02.1080p.mkv" || acq.File.Size != 1400000000 || !acq.Added {
		t.Fatalf("acquired %+v", acq)
	}
	explores := f.explores
	dl, err := p.Link(ctx, Handle{TorrentID: "R1", FileID: acq.File.ID, Size: acq.File.Size})
	if err != nil || dl.URL != "https://oc1.offcloud.example/cloud/download/R1/2" || dl.Expires.IsZero() || f.explores != explores {
		t.Fatalf("first link %+v %v", dl, err)
	}
	// Later refreshes re-explore; a payload without a file id matches by path and size.
	if dl, err = p.Link(ctx, Handle{TorrentID: "R1", Path: "Show.S01/Show.S01E02.1080p.mkv", Size: 1400000000}); err != nil || dl.URL != "https://oc1.offcloud.example/cloud/download/R1/2" {
		t.Fatalf("relink by path %+v %v", dl, err)
	}
	p2 := newOffcloud(testKey, fastOpts(srv.URL))
	if id, err := p2.Find(ctx, testHash); err != nil || id != "R1" {
		t.Fatalf("find %q %v", id, err)
	}
	if id, _ := p2.Find(ctx, strings.Repeat("cd", 20)); id != "" {
		t.Fatalf("failed download reused: %q", id)
	}
	req := episodeReq(true)
	req.TorrentID = "R1"
	if acq, err := p2.Acquire(ctx, req); err != nil || acq.Added || f.created != 1 {
		t.Fatalf("reuse %+v %v created %d", acq, err, f.created)
	}
	if len(f.removed) != 0 {
		t.Fatalf("a cached download was removed: %v", f.removed)
	}
	if err := p.Remove(ctx, "R1"); err != nil || len(f.removed) != 1 || f.removed[0] != "R1" {
		t.Fatalf("remove %v %v", err, f.removed)
	}
	if _, err := p.Link(ctx, Handle{TorrentID: "R1", FileID: "f2"}); !errors.Is(err, ErrGone) {
		t.Fatalf("link after remove: %v", err)
	}
	a, err := p.Account(ctx)
	if err != nil || !a.Premium || a.User != "u-1" || a.Expires.Year() != 2099 {
		t.Fatalf("account %+v %v", a, err)
	}
}

func TestOffcloudUncachedIsRemoved(t *testing.T) {
	f := &fakeOC{cached: false}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := newOffcloud(testKey, fastOpts(srv.URL))
	ctx := context.Background()
	// Cached-only: the cache check fails, so nothing is added.
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, core.ErrNotCached) || f.created != 0 {
		t.Fatalf("cached-only: %v created %d", err, f.created)
	}
	// Reported cached, but the cloud download does not finish in time: removed.
	f.cached, f.state = true, "downloading"
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("stalled download: %v", err)
	}
	if f.created != 1 || len(f.removed) != 1 || f.removed[0] != "R1" {
		t.Fatalf("not-ready download not removed: created %d removed %v", f.created, f.removed)
	}
	// A failed cloud download is removed even when found by the poll.
	f.state, f.removed = "error", nil
	if _, err := p.Acquire(ctx, acquireReq(true)); !errors.Is(err, core.ErrNotCached) || len(f.removed) != 1 {
		t.Fatalf("failed download: %v removed %v", err, f.removed)
	}
	// Not cached-only: added without a cache check and left to download.
	f.cached, f.state, f.removed = false, "downloading", nil
	if _, err := p.Acquire(ctx, acquireReq(false)); !errors.Is(err, core.ErrNotCached) || len(f.removed) != 0 || f.created != 3 {
		t.Fatalf("download mode: %v removed %v created %d", err, f.removed, f.created)
	}
}

// Both providers return the direct URL from their listing; through the
// service the payload carries it without a second listing call.
func TestDebridLinkAndOffcloudThroughService(t *testing.T) {
	dlf := &fakeDL{cached: true}
	dls := httptest.NewServer(dlf.handler(t))
	defer dls.Close()
	ocf := &fakeOC{cached: true}
	ocs := httptest.NewServer(ocf.handler(t))
	defer ocs.Close()
	for _, c := range []struct {
		kind string
		p    Provider
		url  string
	}{
		{config.DebridDebridLink, newDebridLink(testKey, fastOpts(dls.URL)), "https://seed1.debrid.example/dl/T9-2"},
		{config.DebridOffcloud, newOffcloud(testKey, fastOpts(ocs.URL)), "https://oc1.offcloud.example/cloud/download/R1/2"},
	} {
		cfg := config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{{ID: c.kind, Kind: c.kind, APIKey: testKey, Enabled: true}}}
		svc := NewService(cfg, Options{NewProvider: func(config.DebridProvider, ProviderOptions) (Provider, error) { return c.p, nil }})
		rel := core.Release{ID: "r-" + c.kind, Name: "Show.S01.1080p", Protocol: core.ProtocolTorrent, InfoHash: testHash, Target: &core.ReleaseTarget{Season: 1, Episode: 2}}
		raw, err := svc.Acquire(t.Context(), rel)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		pl, err := DecodePayload(raw)
		if err != nil || pl.Kind != c.kind || pl.URL != c.url || pl.Size != 1400000000 || pl.Name() != "Show.S01E02.1080p.mkv" {
			t.Fatalf("%s: payload %+v %v", c.kind, pl, err)
		}
	}
}

func TestOffcloudErrors(t *testing.T) {
	f := &fakeOC{cached: true}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctx := context.Background()
	_, err := newOffcloud("wrong-key-abc", fastOpts(srv.URL)).Account(ctx)
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "wrong-key-abc") {
		t.Fatalf("bad key: %v", err)
	}
	p := newOffcloud(testKey, fastOpts(srv.URL))
	f.limited = true
	_, err = p.CheckCached(ctx, []string{testHash})
	var rl *rateLimitError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.retryAfter != 30*time.Second {
		t.Fatalf("rate limit: %v", err)
	}
	f.limited = false
	for body, want := range map[string]error{
		`{"not_available":"premium"}`:              ErrAuth,
		`{"error":"User not authorized"}`:          ErrAuth,
		`{"error":"Request not found"}`:            ErrGone,
		`{"error":"Too many requests, slow down"}`: ErrRateLimited,
		`{"error":"File removed due to DMCA"}`:     ErrBlocked,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, err := newOffcloud(testKey, fastOpts(srv.URL)).Account(ctx)
		srv.Close()
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", body, err, want)
		}
	}
	// A DMCA refusal at add time is blocked for that release, like
	// Real-Debrid; it does not pause adds on the account.
	dmca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cache/info" {
			fmt.Fprint(w, `[{"cached":true}]`)
			return
		}
		fmt.Fprint(w, `{"error":"File removed due to DMCA"}`)
	}))
	defer dmca.Close()
	if _, err := newOffcloud(testKey, fastOpts(dmca.URL)).Acquire(ctx, acquireReq(true)); !errors.Is(err, ErrBlocked) || errors.Is(err, ErrAddRefused) || !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("dmca add: %v", err)
	}
}

// Two DMCA'd releases in a row must not keep the next release on the same
// Offcloud account from playing.
func TestOffcloudBlockedReleasesDoNotPauseAdds(t *testing.T) {
	f := &fakeOC{cached: true}
	base := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL  string   `json:"url"`
			URLs []string `json:"urls"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		switch {
		case r.URL.Path == "/cache/info":
			fmt.Fprint(w, "["+strings.TrimSuffix(strings.Repeat(`{"cached":true},`, len(body.URLs)), ",")+"]")
			return
		case r.URL.Path == "/cloud" && !strings.Contains(body.URL, testHash):
			fmt.Fprint(w, `{"error":"File removed due to DMCA"}`)
			return
		case r.URL.Path == "/cloud/history": // nothing to reuse: the next release needs an add
			fmt.Fprint(w, `[]`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p := newOffcloud(testKey, fastOpts(srv.URL))
	cfg := config.Debrid{CachedOnly: true, RemoveUncached: true, Providers: []config.DebridProvider{{ID: "oc", Kind: "offcloud", APIKey: testKey, Enabled: true}}}
	svc := NewService(cfg, Options{NewProvider: func(config.DebridProvider, ProviderOptions) (Provider, error) { return p, nil }})
	rel := core.Release{ID: "oc-episode", Name: "Show.S01", Protocol: core.ProtocolTorrent, InfoHash: testHash, Target: &core.ReleaseTarget{Season: 1, Episode: 2}}
	for _, hash := range []string{hashN(1), hashN(2)} {
		blocked := rel
		blocked.ID, blocked.InfoHash = hash, hash
		if _, err := svc.Acquire(t.Context(), blocked); !errors.Is(err, core.ErrNotCached) {
			t.Fatalf("blocked release: %v", err)
		}
	}
	if _, err := svc.Acquire(t.Context(), rel); err != nil {
		t.Fatalf("next release was prevented from playing: %v", err)
	}
}
