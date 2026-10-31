package perfharness

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// result is one timed request.
type Result struct {
	Phase   string        `json:"phase"`
	Client  string        `json:"client"`
	Surface string        `json:"surface"`
	Step    string        `json:"step"`
	Class   string        `json:"class"`
	Viewer  string        `json:"viewer"`
	Tag     string        `json:"tag"`
	Status  int           `json:"status"`
	Bytes   int           `json:"bytes"`
	Dur     time.Duration `json:"dur"`
	Err     string        `json:"err,omitempty"`
	Skipped string        `json:"skipped,omitempty"`
}

// requestTimeout caps one replayed request; a path that scales with the
// library fails its budget long before this.
const requestTimeout = 60 * time.Second

type Runner struct {
	basePort int
	// shared sends every protocol to the base port under its shared path
	// prefix (/jellyfin, /emby, /silo) instead of its own port.
	shared  bool
	http    *http.Client
	viewers []Viewer
	owner   Viewer

	mu       sync.Mutex
	results  []Result
	sessions map[string]*session // viewer/client
	seq      atomic.Int64
}

// NewRunner drives a server whose main port is basePort. With shared set,
// Jellyfin, Emby and Silo are reached under their path prefixes on that
// port; otherwise on basePort+1, +2 and +3.
func NewRunner(basePort int, shared bool, viewers []Viewer) *Runner {
	tr := &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second}
	return &Runner{basePort: basePort, shared: shared, viewers: viewers, owner: viewers[0], sessions: map[string]*session{},
		http: &http.Client{Transport: tr, Timeout: requestTimeout}}
}

func (r *Runner) session(v Viewer, c Client) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := v.Username + "/" + c.Name
	s := r.sessions[key]
	if s == nil {
		s = &session{Viewer: v, vars: map[string]string{
			"username": v.Username, "password": Password, "stremio": queryEscape(v.Stremio),
			"ownerToken": r.owner.PlexToken, "viewerId": strconv.FormatInt(v.ID, 10),
		}}
		r.sessions[key] = s
	}
	return s
}

// runClient drives one client's whole sequence for one viewer.
func (r *Runner) runClient(ctx context.Context, phase string, c Client, v Viewer) {
	s := r.session(v, c)
	// Clients sign in once and keep their token, as real apps do; repeated
	// sign-ins would also trip the sign-in rate limiter.
	r.mu.Lock()
	signedIn := s.vars["token"] != ""
	r.mu.Unlock()
	if c.SignIn != nil && !signedIn {
		r.do(ctx, phase, c, s, *c.SignIn)
	}
	for _, st := range c.Steps {
		if ctx.Err() != nil {
			return
		}
		r.do(ctx, phase, c, s, st)
	}
}

func (r *Runner) do(ctx context.Context, phase string, c Client, s *session, st Step) {
	now := time.Now()
	r.mu.Lock()
	s.vars["now"] = strconv.FormatInt(now.Unix(), 10)
	s.vars["now3h"] = strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10)
	vars := make(map[string]string, len(s.vars))
	for k, v := range s.vars {
		vars[k] = v
	}
	r.mu.Unlock()
	tag := phase + "|" + c.Name + "|" + st.Name + "|" + strconv.FormatInt(r.seq.Add(1), 10)
	res := Result{Phase: phase, Client: c.Name, Surface: c.Surface, Step: st.Name, Class: st.Class, Viewer: s.Viewer.Username, Tag: tag}
	path, missing, ok := expand(c.Prefix+st.Path, vars)
	body, missingBody, okBody := expand(st.Body, vars)
	if !ok || !okBody {
		if missing == "" {
			missing = missingBody
		}
		res.Skipped = "missing {" + missing + "}"
		r.record(res)
		return
	}
	method := st.Method
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	port := r.basePort + c.Port
	if r.shared {
		port, path = r.basePort, c.SharedPrefix+path
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+strconv.Itoa(port)+path, reader)
	if err != nil {
		res.Err = err.Error()
		r.record(res)
		return
	}
	req.Header = c.Headers(&session{Viewer: s.Viewer, vars: vars})
	req.Header.Set("X-Ether-Perf-Tag", tag)
	start := time.Now()
	resp, err := r.http.Do(req)
	if err != nil {
		res.Dur, res.Err = time.Since(start), err.Error()
		r.record(res)
		return
	}
	buf, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	res.Dur = time.Since(start)
	res.Status, res.Bytes = resp.StatusCode, len(buf)
	if err != nil {
		res.Err = err.Error()
	} else if resp.StatusCode >= 400 && st.Class != classPlayback {
		res.Err = strings.TrimSpace(string(bytes.ToValidUTF8(buf[:min(len(buf), 160)], nil)))
	}
	if st.Discover != nil && resp.StatusCode < 300 {
		r.mu.Lock()
		st.Discover(s.vars, buf)
		r.mu.Unlock()
	}
	r.record(res)
}

// Results returns every timed request so far.
func (r *Runner) Results() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Result(nil), r.results...)
}

func (r *Runner) record(res Result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
}

// sequential runs every client for each viewer, one request at a time.
func (r *Runner) Sequential(ctx context.Context, phase string, viewers []Viewer, cs []Client) {
	for _, v := range viewers {
		for _, c := range cs {
			r.runClient(ctx, phase, c, v)
		}
	}
}

// concurrent runs n workers, each a viewer working through every client in
// its own order, rounds times.
func (r *Runner) Concurrent(ctx context.Context, phase string, n, rounds int, cs []Client) {
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := r.viewers[i%len(r.viewers)]
			order := rand.New(rand.NewPCG(uint64(i), 7)).Perm(len(cs))
			for range rounds {
				for _, j := range order {
					r.runClient(ctx, phase, cs[j], v)
				}
			}
		}(i)
	}
	wg.Wait()
}

// backgroundWriter imitates a catalogue refresh against the same database:
// write transactions of 500 title rows, back to back with a short pause, so
// request paths meet a busy writer the way they do while the mirror syncs.
type BackgroundWriter struct {
	db     *sql.DB
	stop   chan struct{}
	done   chan struct{}
	txs    atomic.Int64
	maxTx  atomic.Int64
	errors atomic.Int64
}

func StartBackgroundWriter(dbPath string) (*BackgroundWriter, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var maxRowID int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM catalog_titles`).Scan(&maxRowID); err != nil {
		db.Close()
		return nil, err
	}
	w := &BackgroundWriter{db: db, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		rng := rand.New(rand.NewPCG(1, 2))
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			from := rng.Int64N(max(maxRowID-500, 1))
			start := time.Now()
			tx, err := db.Begin()
			if err == nil {
				_, err = tx.Exec(`UPDATE catalog_titles SET popularity=popularity WHERE rowid BETWEEN ? AND ?`, from, from+499)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				w.errors.Add(1)
				continue
			}
			w.txs.Add(1)
			if d := int64(time.Since(start)); d > w.maxTx.Load() {
				w.maxTx.Store(d)
			}
		}
	}()
	return w, nil
}

func (w *BackgroundWriter) Close() string {
	close(w.stop)
	<-w.done
	w.db.Close()
	return fmt.Sprintf("%d write transactions of 500 rows (longest %s, %d failed)", w.txs.Load(), time.Duration(w.maxTx.Load()).Round(time.Millisecond), w.errors.Load())
}
