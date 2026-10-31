package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// previewParallel bounds catalog and PMS lookups inside one preview.
// Page fetches stay sequential; each page's items share this many workers.
const previewParallel = 8

// PreviewProgress is the counts a running preview exposes while it scans.
type PreviewProgress struct {
	Phase     string `json:"phase"`
	Pages     int    `json:"pages"`
	Scanned   int    `json:"scanned"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
}

// PreviewStatus is the owner-facing preview job. Plan is set only when Status is done.
type PreviewStatus struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Progress PreviewProgress `json:"progress"`
	Error    string          `json:"error,omitempty"`
	Plan     *Plan           `json:"plan,omitempty"`
}

type progressBox struct {
	mu sync.Mutex
	p  PreviewProgress
}

type progressKey struct{}
type memoKey struct{}

type mappedRow struct {
	item Item
	warn string
	keep bool
}

func withPreviewState(ctx context.Context) context.Context {
	if ctx.Value(progressKey{}) == nil {
		ctx = context.WithValue(ctx, progressKey{}, &progressBox{})
	}
	if ctx.Value(memoKey{}) == nil {
		ctx = context.WithValue(ctx, memoKey{}, &previewMemo{})
	}
	return ctx
}

func addProgress(ctx context.Context, fn func(*PreviewProgress)) {
	box, _ := ctx.Value(progressKey{}).(*progressBox)
	if box == nil {
		return
	}
	box.mu.Lock()
	fn(&box.p)
	box.mu.Unlock()
}

func (s *Service) logInfo(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

func (s *Service) logError(msg string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Error(msg, args...)
	}
}

func (s *Service) logProgress(ctx context.Context, msg string) {
	if s == nil || s.logger == nil {
		return
	}
	box, _ := ctx.Value(progressKey{}).(*progressBox)
	if box == nil {
		s.logger.Info(msg)
		return
	}
	box.mu.Lock()
	pr := box.p
	box.mu.Unlock()
	s.logger.Info(msg, "phase", pr.Phase, "pages", pr.Pages, "scanned", pr.Scanned, "matched", pr.Matched, "unmatched", pr.Unmatched)
}

func validatePreviewRequest(q Request) error {
	u, err := url.Parse(q.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("PMS URL must be an http(s) origin without credentials")
	}
	if len(q.Viewers) == 0 {
		return errors.New("choose at least one local viewer")
	}
	return nil
}

func (s *Service) eachMedia(ctx context.Context, items []media, fn func(int, media)) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, previewParallel)
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		select {
		case <-ctx.Done():
			wg.Done()
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		go func(i int, item media) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			fn(i, item)
		}(i, item)
	}
	wg.Wait()
	return ctx.Err()
}

func (s *Service) catalogGet(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	load := func() (*core.Title, error) { return s.catalog.Get(ctx, key) }
	if m := memoFrom(ctx); m != nil {
		return m.catalogGet(key, load)
	}
	return load()
}

func (s *Service) externalTitle(ctx context.Context, source, id string) (*core.Title, error) {
	load := func() (*core.Title, error) { return s.catalog.ByExternalID(ctx, source, id) }
	if m := memoFrom(ctx); m != nil {
		return m.external(source+"\n"+id, load)
	}
	return load()
}

type previewMemo struct {
	mu         sync.Mutex
	http       map[string]*httpCall
	catalog    map[core.RatingKey]*titleCall
	byExternal map[string]*titleCall
}

type httpCall struct {
	done chan struct{}
	c    container
	err  error
}

type titleCall struct {
	done  chan struct{}
	title *core.Title
	err   error
}

func memoFrom(ctx context.Context) *previewMemo {
	m, _ := ctx.Value(memoKey{}).(*previewMemo)
	return m
}

func (m *previewMemo) httpGet(key string, fn func() (container, error)) (container, error) {
	m.mu.Lock()
	if m.http == nil {
		m.http = map[string]*httpCall{}
	}
	if call := m.http[key]; call != nil {
		m.mu.Unlock()
		<-call.done
		return call.c, call.err
	}
	call := &httpCall{done: make(chan struct{})}
	m.http[key] = call
	m.mu.Unlock()
	call.c, call.err = fn()
	close(call.done)
	return call.c, call.err
}

func (m *previewMemo) catalogGet(key core.RatingKey, fn func() (*core.Title, error)) (*core.Title, error) {
	m.mu.Lock()
	if m.catalog == nil {
		m.catalog = map[core.RatingKey]*titleCall{}
	}
	if call := m.catalog[key]; call != nil {
		m.mu.Unlock()
		<-call.done
		return call.title, call.err
	}
	call := &titleCall{done: make(chan struct{})}
	m.catalog[key] = call
	m.mu.Unlock()
	call.title, call.err = fn()
	close(call.done)
	return call.title, call.err
}

func (m *previewMemo) external(key string, fn func() (*core.Title, error)) (*core.Title, error) {
	m.mu.Lock()
	if m.byExternal == nil {
		m.byExternal = map[string]*titleCall{}
	}
	if call := m.byExternal[key]; call != nil {
		m.mu.Unlock()
		<-call.done
		return call.title, call.err
	}
	call := &titleCall{done: make(chan struct{})}
	m.byExternal[key] = call
	m.mu.Unlock()
	call.title, call.err = fn()
	close(call.done)
	return call.title, call.err
}

type previewJob struct {
	id      string
	status  string
	err     string
	plan    *Plan
	cancel  context.CancelFunc
	box     *progressBox
	mu      sync.Mutex
	updated time.Time
}

func newPreviewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartPreview validates the request, then scans PMS on a background context.
// The POST that starts it returns as soon as the job exists.
func (s *Service) StartPreview(q Request) (PreviewStatus, error) {
	if err := validatePreviewRequest(q); err != nil {
		return PreviewStatus{}, err
	}
	id, err := newPreviewID()
	if err != nil {
		return PreviewStatus{}, err
	}
	box := &progressBox{}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, progressKey{}, box)
	ctx = context.WithValue(ctx, memoKey{}, &previewMemo{})
	job := &previewJob{id: id, status: "running", cancel: cancel, updated: time.Now(), box: box}
	s.jobsMu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*previewJob{}
	}
	for _, old := range s.jobs {
		old.mu.Lock()
		running := old.status == "running"
		stop := old.cancel
		if running {
			old.status = "cancelled"
			old.updated = time.Now()
		}
		old.mu.Unlock()
		if running && stop != nil {
			stop()
		}
	}
	s.jobs[id] = job
	s.pruneJobsLocked(time.Now())
	s.jobsMu.Unlock()
	s.logInfo("plex preview started", "job", id, "viewers", len(q.Viewers))
	go s.runPreview(job, ctx, q)
	return job.snapshot(), nil
}

func (s *Service) runPreview(job *previewJob, ctx context.Context, q Request) {
	p, err := s.Preview(ctx, q)
	job.mu.Lock()
	job.updated = time.Now()
	switch {
	case job.status == "cancelled" || errors.Is(err, context.Canceled):
		job.status = "cancelled"
		job.mu.Unlock()
		s.logInfo("plex preview cancelled", "job", job.id)
	case err != nil:
		job.status = "error"
		job.err = err.Error()
		job.mu.Unlock()
		s.logError("plex preview failed", "job", job.id, "err", err)
	default:
		job.status = "done"
		job.plan = &p
		job.mu.Unlock()
		s.logInfo("plex preview finished", "job", job.id, "items", len(p.Items), "history", len(p.History), "warnings", len(p.Warnings))
	}
}

func (s *Service) PreviewStatus(id string) (PreviewStatus, bool) {
	s.jobsMu.Lock()
	job := s.jobs[id]
	s.jobsMu.Unlock()
	if job == nil {
		return PreviewStatus{}, false
	}
	return job.snapshot(), true
}

func (s *Service) CancelPreview(id string) (PreviewStatus, bool) {
	s.jobsMu.Lock()
	job := s.jobs[id]
	s.jobsMu.Unlock()
	if job == nil {
		return PreviewStatus{}, false
	}
	job.mu.Lock()
	if job.status == "running" {
		job.status = "cancelled"
		job.updated = time.Now()
		if job.cancel != nil {
			job.cancel()
		}
	}
	job.mu.Unlock()
	s.logInfo("plex preview cancel requested", "job", id)
	return job.snapshot(), true
}

func (j *previewJob) snapshot() PreviewStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := PreviewStatus{ID: j.id, Status: j.status, Error: j.err, Plan: j.plan}
	if j.box != nil {
		j.box.mu.Lock()
		st.Progress = j.box.p
		j.box.mu.Unlock()
	}
	return st
}

func (s *Service) pruneJobsLocked(now time.Time) {
	if len(s.jobs) < 32 {
		return
	}
	for id, job := range s.jobs {
		job.mu.Lock()
		status, updated := job.status, job.updated
		job.mu.Unlock()
		if status != "running" && now.Sub(updated) > 30*time.Minute {
			delete(s.jobs, id)
		}
	}
}
