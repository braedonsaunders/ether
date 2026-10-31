package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	load := func() (*core.Title, error) {
		t, err := s.catalog.Get(ctx, key)
		if err != nil && !errors.Is(err, context.Canceled) {
			if lookup, ok := s.catalog.(core.StoredTitleLookup); ok {
				if st, serr := lookup.StoredTitle(ctx, key); serr == nil && st != nil && !st.Adult {
					return st, nil
				}
			}
		}
		return t, err
	}
	if m := memoFrom(ctx); m != nil {
		return m.catalogGet(key, load)
	}
	return load()
}

func (s *Service) externalTitle(ctx context.Context, source, id string) (*core.Title, error) {
	return s.externalTitleForType(ctx, source, id, "")
}

func (s *Service) externalTitleForType(ctx context.Context, source, id, typ string) (*core.Title, error) {
	load := func() (*core.Title, error) {
		if typed, ok := s.catalog.(interface {
			ByExternalIDForType(context.Context, string, string, core.MediaType) (*core.Title, error)
		}); ok && typ != "" {
			return typed.ByExternalIDForType(ctx, source, id, core.MediaType(typ))
		}
		return s.catalog.ByExternalID(ctx, source, id)
	}
	if m := memoFrom(ctx); m != nil {
		return m.external(source+"\n"+id+"\n"+typ, load)
	}
	return load()
}

func (s *Service) catalogSearch(ctx context.Context, query string, types []core.MediaType, limit int) ([]core.Title, error) {
	if all, ok := s.catalog.(interface {
		SearchAll(context.Context, string, []core.MediaType, int) ([]core.Title, error)
	}); ok {
		return all.SearchAll(ctx, query, types, limit)
	}
	return s.catalog.Search(ctx, query, types, limit)
}

func splitTitleYear(title string) (string, int) {
	title = strings.TrimSpace(title)
	split := strings.LastIndexAny(title, " \t")
	if split < 1 {
		return title, 0
	}
	base, suffix := strings.TrimSpace(title[:split]), strings.TrimSpace(title[split+1:])
	if strings.HasPrefix(suffix, "(") && strings.HasSuffix(suffix, ")") {
		inner := suffix[1 : len(suffix)-1]
		if len(inner) == 4 {
			if y, err := strconv.Atoi(inner); err == nil && y >= 1900 && y <= 2099 && base != "" {
				return base, y
			}
		}
	}
	return title, 0
}

func splitTitleCountry(title string) (base, iso, name string) {
	title = strings.TrimSpace(title)
	split := strings.LastIndexAny(title, " \t")
	if split < 1 {
		return title, "", ""
	}
	prefix, suffix := strings.TrimSpace(title[:split]), strings.TrimSpace(title[split+1:])
	if prefix == "" || !strings.HasPrefix(suffix, "(") || !strings.HasSuffix(suffix, ")") {
		return title, "", ""
	}
	tag := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(suffix[1:len(suffix)-1]), ".", ""))
	switch tag {
	case "US", "USA":
		return prefix, "US", "United States of America"
	case "UK", "GB":
		return prefix, "GB", "United Kingdom"
	case "AU", "AUS":
		return prefix, "AU", "Australia"
	case "CA", "CAN":
		return prefix, "CA", "Canada"
	case "NZ":
		return prefix, "NZ", "New Zealand"
	case "JP", "JPN":
		return prefix, "JP", "Japan"
	case "KR", "KOR":
		return prefix, "KR", "South Korea"
	}
	return title, "", ""
}

func normalizeAlnum(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space && b.Len() > 0 {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func titleMatchesExact(t *core.Title, want string) bool {
	return strings.EqualFold(strings.TrimSpace(t.Title), want) ||
		(t.OriginalTitle != "" && strings.EqualFold(strings.TrimSpace(t.OriginalTitle), want))
}

func titleMatchesNormalized(t *core.Title, wantNorm string) bool {
	if wantNorm == "" {
		return false
	}
	return normalizeAlnum(t.Title) == wantNorm ||
		(t.OriginalTitle != "" && normalizeAlnum(t.OriginalTitle) == wantNorm)
}

func countryMatches(t *core.Title, iso, fullName string) bool {
	if iso == "" {
		return false
	}
	for _, c := range t.Countries {
		if strings.EqualFold(c, iso) || (fullName != "" && strings.EqualFold(c, fullName)) {
			return true
		}
	}
	return false
}

func (s *Service) searchTitle(ctx context.Context, typ, title string, year int) (*core.Title, error) {
	title = strings.TrimSpace(title)
	if base, y := splitTitleYear(title); y > 0 {
		if year == 0 {
			year = y
		}
		title = base
	}
	load := func() (*core.Title, error) {
		ts, err := s.catalogSearch(ctx, title, []core.MediaType{core.MediaType(typ)}, 20)
		if err != nil {
			return nil, err
		}
		pick := func(matchFn func(*core.Title) bool, yearDiff int) (*core.Title, bool) {
			var match *core.Title
			for i := range ts {
				t := &ts[i]
				if string(t.Type) != typ || !matchFn(t) {
					continue
				}
				diff := t.Year - year
				if diff < 0 {
					diff = -diff
				}
				if diff != yearDiff {
					continue
				}
				if match != nil && match.Key != t.Key {
					return nil, false
				}
				match = t
			}
			return match, match != nil
		}
		if match, ok := pick(func(t *core.Title) bool { return titleMatchesExact(t, title) }, 0); ok {
			return match, nil
		}
		wantNorm := normalizeAlnum(title)
		if match, ok := pick(func(t *core.Title) bool { return titleMatchesNormalized(t, wantNorm) }, 0); ok {
			return match, nil
		}
		if year > 0 {
			if match, ok := pick(func(t *core.Title) bool { return titleMatchesExact(t, title) }, 1); ok {
				return match, nil
			}
			if match, ok := pick(func(t *core.Title) bool { return titleMatchesNormalized(t, wantNorm) }, 1); ok {
				return match, nil
			}
		}
		return nil, core.ErrNotFound
	}
	if m := memoFrom(ctx); m != nil {
		return m.external("search\n"+typ+"\n"+strings.ToLower(title)+"\n"+strconv.Itoa(year), load)
	}
	return load()
}

func (s *Service) searchShow(ctx context.Context, title string, maxAirYear, season int) (*core.Title, error) {
	title = strings.TrimSpace(title)
	if title == "" || s.catalog == nil {
		return nil, core.ErrNotFound
	}
	if base, y := splitTitleYear(title); y > 0 {
		if t, err := s.searchTitle(ctx, "show", base, y); err == nil && t != nil && t.Key != 0 {
			return t, nil
		} else if errors.Is(err, context.Canceled) {
			return nil, err
		}
		title = base
	}
	baseCountry, iso, countryName := splitTitleCountry(title)
	load := func() (*core.Title, error) {
		queries := []string{title}
		if iso != "" && baseCountry != "" && !strings.EqualFold(baseCountry, title) {
			queries = append(queries, baseCountry)
		}
		for _, q := range queries {
			ts, err := s.catalogSearch(ctx, q, []core.MediaType{core.TypeShow}, 20)
			if err != nil {
				return nil, err
			}
			wantNorm := normalizeAlnum(q)
			var exact, normCands []*core.Title
			for i := range ts {
				t := &ts[i]
				if t.Type != core.TypeShow || t.Key == 0 {
					continue
				}
				if titleMatchesExact(t, q) {
					exact = append(exact, t)
				} else if titleMatchesNormalized(t, wantNorm) {
					normCands = append(normCands, t)
				}
			}
			cands := exact
			if len(cands) == 0 {
				cands = normCands
			}
			if len(cands) == 0 {
				continue
			}
			if iso != "" {
				var byCountry []*core.Title
				for _, c := range cands {
					if countryMatches(c, iso, countryName) {
						byCountry = append(byCountry, c)
					}
				}
				if len(byCountry) > 0 {
					cands = byCountry
				}
			}
			if len(cands) > 1 && maxAirYear > 0 {
				var byAir []*core.Title
				for _, c := range cands {
					if c.Year == 0 || c.Year <= maxAirYear+1 {
						byAir = append(byAir, c)
					}
				}
				if len(byAir) > 0 {
					cands = byAir
				}
			}
			if len(cands) > 1 && season > 1 {
				var bySeason []*core.Title
				for _, c := range cands {
					if c.ChildCount == 0 || c.ChildCount >= season {
						bySeason = append(bySeason, c)
					}
				}
				if len(bySeason) > 0 {
					cands = bySeason
				}
			}
			if len(cands) == 1 {
				return cands[0], nil
			}
			// Deduplicate identical keys.
			unique := cands[:0]
			seen := map[core.RatingKey]bool{}
			for _, c := range cands {
				if !seen[c.Key] {
					seen[c.Key] = true
					unique = append(unique, c)
				}
			}
			if len(unique) == 1 {
				return unique[0], nil
			}
			if len(unique) > 1 && unique[0].Popularity >= 10 && unique[0].Popularity >= unique[1].Popularity*3 {
				return unique[0], nil
			}
		}
		return nil, core.ErrNotFound
	}
	if m := memoFrom(ctx); m != nil {
		return m.external("searchShow\n"+strings.ToLower(title)+"\n"+strconv.Itoa(maxAirYear)+"\n"+strconv.Itoa(season), load)
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
