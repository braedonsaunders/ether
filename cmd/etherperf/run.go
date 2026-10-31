package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// result is one timed request.
type result struct {
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

type runner struct {
	basePort int
	http     *http.Client
	viewers  []viewer
	owner    viewer

	mu       sync.Mutex
	results  []result
	sessions map[string]*session // viewer/client
	seq      atomic.Int64
}

func newRunner(basePort int, viewers []viewer) *runner {
	tr := &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second}
	return &runner{basePort: basePort, viewers: viewers, owner: viewers[0], sessions: map[string]*session{},
		http: &http.Client{Transport: tr, Timeout: 90 * time.Second}}
}

func (r *runner) session(v viewer, c client) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := v.Username + "/" + c.Name
	s := r.sessions[key]
	if s == nil {
		s = &session{viewer: v, vars: map[string]string{
			"username": v.Username, "password": perfPassword, "stremio": queryEscape(v.Stremio),
			"ownerToken": r.owner.PlexToken, "viewerId": strconv.FormatInt(v.ID, 10),
		}}
		r.sessions[key] = s
	}
	return s
}

// runClient drives one client's whole sequence for one viewer.
func (r *runner) runClient(ctx context.Context, phase string, c client, v viewer) {
	s := r.session(v, c)
	if c.SignIn != nil {
		r.do(ctx, phase, c, s, *c.SignIn)
	}
	for _, st := range c.Steps {
		if ctx.Err() != nil {
			return
		}
		r.do(ctx, phase, c, s, st)
	}
}

func (r *runner) do(ctx context.Context, phase string, c client, s *session, st step) {
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
	res := result{Phase: phase, Client: c.Name, Surface: c.Surface, Step: st.Name, Class: st.Class, Viewer: s.viewer.Username, Tag: tag}
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
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+strconv.Itoa(r.basePort+c.Port)+path, reader)
	if err != nil {
		res.Err = err.Error()
		r.record(res)
		return
	}
	req.Header = c.Headers(&session{viewer: s.viewer, vars: vars})
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

func (r *runner) record(res result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
}

// sequential runs every client for each viewer, one request at a time.
func (r *runner) sequential(ctx context.Context, phase string, viewers []viewer, cs []client) {
	for _, v := range viewers {
		for _, c := range cs {
			r.runClient(ctx, phase, c, v)
		}
	}
}

// concurrent runs n workers, each a viewer working through every client in
// its own order, rounds times.
func (r *runner) concurrent(ctx context.Context, phase string, n, rounds int, cs []client) {
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
type backgroundWriter struct {
	db     *sql.DB
	stop   chan struct{}
	done   chan struct{}
	txs    atomic.Int64
	maxTx  atomic.Int64
	errors atomic.Int64
}

func startBackgroundWriter(dbPath string) (*backgroundWriter, error) {
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
	w := &backgroundWriter{db: db, stop: make(chan struct{}), done: make(chan struct{})}
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

func (w *backgroundWriter) close() string {
	close(w.stop)
	<-w.done
	w.db.Close()
	return fmt.Sprintf("%d write transactions of 500 rows (longest %s, %d failed)", w.txs.Load(), time.Duration(w.maxTx.Load()).Round(time.Millisecond), w.errors.Load())
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
