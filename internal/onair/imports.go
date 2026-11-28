package onair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/extras"
	"github.com/go-chi/chi/v5"
)

// Import jobs share the bounded background queue with look preparation. No
// network request or media probe runs on the console request goroutine.
type ImportRequest struct {
	URL        string   `json:"url"`
	URLs       []string `json:"urls"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Mode       string   `json:"mode,omitempty"`
	DurationMS int64    `json:"durationMs,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}
type ImportJob struct {
	UseSourceTitle bool    `json:"useSourceTitle,omitempty"`
	ID             string  `json:"id"`
	Source         string  `json:"source"`
	Attribution    string  `json:"attribution,omitempty"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	Status         string  `json:"status"`
	Progress       float64 `json:"progress"`
	Error          string  `json:"error,omitempty"`
	Link           bool    `json:"link,omitempty"`
}

func publicImportURL(ctx context.Context, raw string) error {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("a public HTTP media link is required")
	}
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if e != nil || len(ips) == 0 {
		return fmt.Errorf("cannot resolve media host")
	}
	for _, a := range ips {
		ip := a.IP
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("private media hosts are not allowed")
		}
	}
	return nil
}

func (s *Service) validateImportURL(ctx context.Context, raw string) error {
	if s.o.ValidateURL != nil {
		return s.o.ValidateURL(ctx, raw)
	}
	return publicImportURL(ctx, raw)
}

func (s *Service) archiveOrigin() string {
	if s.o.ArchiveOrigin != "" {
		return strings.TrimRight(s.o.ArchiveOrigin, "/")
	}
	return "https://archive.org"
}

func (s *Service) importHTTPClient() (*http.Client, func()) {
	if s.o.HTTPClient != nil {
		return s.o.HTTPClient, func() {}
	}
	c := importClient()
	return c, c.CloseIdleConnections
}

func importClient() *http.Client {
	c := egress.Client(egress.CapPlugins, 10*time.Minute)
	tr := c.Transport.(*http.Transport)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, e := dial(ctx, network, addr)
		if e != nil {
			return nil, e
		}
		// RoundTrip records the real request's outbound proxy decision.
		proxied, _ := ctx.Value(importProxyKey{}).(bool)
		if !proxied {
			if a, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || !a.IP.IsGlobalUnicast() || a.IP.IsPrivate() || a.IP.IsLoopback() {
				conn.Close()
				return nil, fmt.Errorf("private media peer refused")
			}
		}
		return conn, nil
	}
	c.Transport = importTransport{tr}
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return publicImportURL(r.Context(), r.URL.String())
	}
	return c
}

type importProxyKey struct{}
type importTransport struct{ base *http.Transport }

func (t importTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if e := publicImportURL(r.Context(), r.URL.String()); e != nil {
		return nil, e
	}
	proxy, e := t.base.Proxy(r)
	if e != nil {
		return nil, e
	}
	return t.base.RoundTrip(r.WithContext(context.WithValue(r.Context(), importProxyKey{}, proxy != nil)))
}
func (t importTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func importGETValidated(ctx context.Context, c *http.Client, raw string, headers map[string]string, validate func(context.Context, string) error) (*http.Response, error) {
	if validate != nil {
		if e := validate(ctx, raw); e != nil {
			return nil, e
		}
	}
	r, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		return nil, e
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, e := c.Do(r)
	if e != nil {
		return nil, fmt.Errorf("media request failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("media returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// resolveArchive accepts an explicit metadata endpoint for offline fake-server
// tests. Production always uses archive.org over the same guarded HTTP client.
func resolveArchive(ctx context.Context, c *http.Client, endpoint, item string) (string, string, string, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", endpoint+"/metadata/"+url.PathEscape(item), nil)
	if e != nil {
		return "", "", "", e
	}
	resp, e := c.Do(r)
	if e != nil {
		return "", "", "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("archive metadata unavailable")
	}
	var v struct {
		Metadata struct {
			Title   string `json:"title"`
			License string `json:"licenseurl"`
		} `json:"metadata"`
		Files []struct {
			Name   string `json:"name"`
			Format string `json:"format"`
			Size   string `json:"size"`
		} `json:"files"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v); e != nil {
		return "", "", "", e
	}
	sort.SliceStable(v.Files, func(i, j int) bool {
		score := func(name, format string) int {
			if strings.HasSuffix(strings.ToLower(name), ".mp4") {
				if strings.Contains(strings.ToLower(format), "h.264") {
					return 3
				}
				return 2
			}
			return 0
		}
		return score(v.Files[i].Name, v.Files[i].Format) > score(v.Files[j].Name, v.Files[j].Format)
	})
	for _, f := range v.Files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".mp4") {
			return endpoint + "/download/" + url.PathEscape(item) + "/" + url.PathEscape(f.Name), v.Metadata.Title, strings.TrimSpace(v.Metadata.Title + " · " + v.Metadata.License), nil
		}
	}
	return "", "", "", fmt.Errorf("archive item has no MP4 video")
}
func (s *Service) saveImport(ctx context.Context, reel string, j ImportJob) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE onair_imports SET data=? WHERE id=? AND reel_id=?", string(b), j.ID, reel)
	return e
}
func (s *Service) imports(ctx context.Context, reel string) ([]ImportJob, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT data FROM onair_imports WHERE reel_id=? ORDER BY id", reel)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ImportJob{}
	for rows.Next() {
		var b string
		var j ImportJob
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &j); e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Service) importClips(ctx context.Context, r Reel) (Reel, error) {
	jobs, e := s.imports(ctx, r.ID)
	for _, j := range jobs {
		found := false
		for _, c := range r.Clips {
			if c.ID == j.ID {
				found = true
			}
		}
		if found {
			continue
		}
		r.Clips = append(r.Clips, ReelClip{ID: j.ID, Name: j.Name, Source: j.Source, Attribution: j.Attribution, Status: j.Status, Progress: j.Progress, Error: j.Error})
	}
	return r, e
}
func (s *Service) importAssets(ctx context.Context, out []Asset) ([]Asset, error) {
	jobs, e := s.imports(ctx, "")
	for _, j := range jobs {
		out = append(out, Asset{Digest: j.ID, Name: j.Name, Kind: j.Kind, Source: j.Source, Attribution: j.Attribution, Status: j.Status, Progress: j.Progress, Error: j.Error, UsedBy: []string{}})
	}
	return out, e
}

func defaultImportName(u *url.URL) string {
	if u == nil {
		return "Imported clip"
	}
	base := strings.TrimSpace(path.Base(u.Path))
	switch strings.ToLower(base) {
	case "", ".", "/", "watch", "shorts", "live", "playlist", "embed":
		if v := strings.TrimSpace(u.Query().Get("v")); v != "" {
			return u.Hostname() + " · " + v
		}
		if u.Hostname() != "" {
			return u.Hostname()
		}
		return "Imported clip"
	default:
		return base
	}
}

func (s *Service) cancelImport(id string) {
	s.mu.Lock()
	cancel := s.importCancels[id]
	delete(s.importCancels, id)
	delete(s.pending, "import-"+id)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) cancelReelImports(ctx context.Context, reel string) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM onair_imports WHERE reel_id=?", reel)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.cancelImport(id)
	}
}

func (s *Service) Import(ctx context.Context, reel string, in ImportRequest) ([]ImportJob, error) {
	urls := append([]string(nil), in.URLs...)
	if in.URL != "" {
		urls = append(urls, in.URL)
	}
	if len(in.Name) > 255 {
		return nil, fmt.Errorf("name maximum 255 characters")
	}
	if len(urls) == 0 || len(urls) > 50 {
		return nil, fmt.Errorf("import between 1 and 50 links")
	}
	if in.Mode != "" && in.Mode != "import" && in.Mode != "link" {
		return nil, fmt.Errorf("invalid import mode")
	}
	if in.Mode == "link" && reel == "" {
		return nil, fmt.Errorf("link mode requires a reel")
	}
	if reel != "" {
		if _, e := s.Reel(ctx, reel); e != nil {
			return nil, e
		}
	} else if in.Kind != "image" && in.Kind != "audio" && in.Kind != "video" {
		return nil, fmt.Errorf("choose image, audio or video")
	}
	// Parse only here; DNS resolution belongs to the background job.
	for _, raw := range urls {
		u, e := url.Parse(raw)
		if len(raw) > 8192 || e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("invalid HTTP media link")
		}
	}
	if in.Mode == "link" {
		out := make([]ImportJob, 0, len(urls))
		for _, raw := range urls {
			c, err := s.AddLinkClip(ctx, reel, LinkClipInput{
				Name:       in.Name,
				Source:     raw,
				DurationMS: in.DurationMS,
				Tags:       in.Tags,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, ImportJob{
				UseSourceTitle: in.Name == "",
				ID:             c.ID,
				Source:         c.Source,
				Name:           c.Name,
				Kind:           "video",
				Status:         c.Status,
				Progress:       100,
				Link:           true,
			})
		}
		return out, nil
	}
	out := []ImportJob{}
	for _, raw := range urls {
		j := ImportJob{UseSourceTitle: in.Name == "", ID: newID(), Source: raw, Name: in.Name, Kind: in.Kind, Status: "queued"}
		if j.Name == "" {
			u, _ := url.Parse(raw)
			j.Name = defaultImportName(u)
		}
		b, _ := json.Marshal(j)
		if _, e := s.db.ExecContext(ctx, "INSERT INTO onair_imports(id,reel_id,data) VALUES(?,?,?)", j.ID, reel, string(b)); e != nil {
			return nil, e
		}
		job := j
		if !s.enqueue(renderJob{download: true, Hash: "import-" + j.ID, prepare: func(ctx context.Context) error { return s.runImport(ctx, reel, job) }}) {
			j.Status = "error"
			j.Error = "import queue full"
			s.saveImport(ctx, reel, j)
		}
		out = append(out, j)
	}
	return out, nil
}
func (s *Service) runImport(ctx context.Context, reel string, j ImportJob) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	s.mu.Lock()
	if s.importCancels == nil {
		s.importCancels = map[string]context.CancelFunc{}
	}
	s.importCancels[j.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.importCancels, j.ID)
		s.mu.Unlock()
		cancel()
	}()
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			j.Status = "error"
			j.Error = err.Error()
			s.saveImport(context.WithoutCancel(ctx), reel, j)
		}
	}()
	// Check that the job was not cancelled while waiting in the queue.
	var exists int
	if e := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM onair_imports WHERE id=?", j.ID).Scan(&exists); e != nil {
		return e
	}
	if exists == 0 {
		return nil
	}
	j.Status = "downloading"
	s.saveImport(ctx, reel, j)
	c, closeClient := s.importHTTPClient()
	defer closeClient()
	raw := j.Source
	u, _ := url.Parse(raw)
	if e := s.validateImportURL(ctx, raw); e != nil {
		return e
	}
	kind := j.Kind
	if reel != "" {
		kind = "video"
	}
	limit := int64(assetLimit)
	maxDur := 30 * time.Second
	if reel != "" {
		limit = 500 << 20
		maxDur = 3 * time.Minute
	}

	if strings.EqualFold(u.Hostname(), "archive.org") && strings.HasPrefix(u.Path, "/details/") {
		item := strings.Split(strings.TrimPrefix(u.Path, "/details/"), "/")[0]
		var title string
		raw, title, j.Attribution, err = resolveArchive(ctx, c, s.archiveOrigin(), item)
		if err != nil {
			if s.o.Downloader != nil && s.o.Downloader.Status().Available {
				return s.runExtractorImport(ctx, c, reel, j, j.Source, kind, limit, maxDur)
			}
			return err
		}
		if j.UseSourceTitle && strings.TrimSpace(title) != "" {
			j.Name = title
		}
	} else if (kind == "video" || kind == "audio") && extras.IsExtractorSiteURL(raw) {
		return s.runExtractorImport(ctx, c, reel, j, raw, kind, limit, maxDur)
	}

	resp, e := importGETValidated(ctx, c, raw, nil, s.validateImportURL)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	mt, _, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e != nil {
		return fmt.Errorf("invalid media content type")
	}
	if !strings.HasPrefix(mt, kind+"/") && mt != "application/octet-stream" {
		if (kind == "video" || kind == "audio") && (mt == "text/html" || mt == "application/xhtml+xml") && s.o.Downloader != nil {
			_ = resp.Body.Close()
			return s.runExtractorImport(ctx, c, reel, j, raw, kind, limit, maxDur)
		}
		return fmt.Errorf("link is not %s media", kind)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("media exceeds size cap")
	}
	f, e := os.CreateTemp(s.o.Dir, ".import-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	n, e := copyImport(f, resp.Body, limit, resp.ContentLength, func(progress float64) { j.Progress = progress; s.saveImport(ctx, reel, j) })
	if e != nil {
		return e
	}
	if n == 0 {
		return fmt.Errorf("empty media")
	}
	if _, e = f.Seek(0, 0); e != nil {
		return e
	}
	return s.commitImportedMedia(ctx, reel, j, f)
}

func (s *Service) runExtractorImport(ctx context.Context, c *http.Client, reel string, j ImportJob, raw, kind string, limit int64, maxDur time.Duration) error {
	dl, err := s.downloadViaExtractor(ctx, c, raw, kind, reel != "", limit, maxDur, func(pct float64) {
		j.Progress = pct
		_ = s.saveImport(ctx, reel, j)
	})
	if err != nil {
		return err
	}
	defer dl.cleanup()
	if dl.title != "" && j.UseSourceTitle {
		j.Name = dl.title
	}
	if j.Attribution == "" && dl.attribution != "" {
		j.Attribution = dl.attribution
	}
	_ = s.saveImport(ctx, reel, j)
	f, err := os.Open(dl.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.commitImportedMedia(ctx, reel, j, f)
}

type downloadedLink struct {
	path        string
	title       string
	attribution string
	cleanup     func()
}

func (s *Service) downloadViaExtractor(ctx context.Context, c *http.Client, raw, kind string, isReel bool, limit int64, maxDur time.Duration, progress func(float64)) (downloadedLink, error) {
	if s.o.Downloader == nil {
		return downloadedLink{}, fmt.Errorf("yt-dlp is required to import from video sites; click Install yt-dlp to set it up")
	}
	workDir, err := os.MkdirTemp(s.o.Dir, ".import-ytdlp-*")
	if err != nil {
		return downloadedLink{}, err
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }
	res, err := s.o.Downloader.Download(ctx, extras.DownloadRequest{
		URL:         raw,
		Kind:        kind,
		Reel:        isReel,
		MaxBytes:    limit,
		MaxDuration: maxDur,
		OutputDir:   workDir,
		Progress:    progress,
	})
	if err != nil {
		cleanup()
		return downloadedLink{}, err
	}
	var title, attr string
	if strings.TrimSpace(res.Title) != "" {
		title = cleanTitle(res.Title)
		if strings.TrimSpace(res.Attribution) != "" {
			attr = cleanTitle(res.Title + " · " + res.Attribution)
		} else {
			attr = title
		}
	} else if strings.TrimSpace(res.Attribution) != "" {
		attr = cleanTitle(res.Attribution)
	}
	if res.Path != "" {
		return downloadedLink{path: res.Path, title: title, attribution: attr, cleanup: cleanup}, nil
	}
	if res.ResolvedURL != "" {
		resp, err := importGETValidated(ctx, c, res.ResolvedURL, res.Headers, s.validateImportURL)
		if err != nil {
			cleanup()
			return downloadedLink{}, err
		}
		defer resp.Body.Close()
		if resp.ContentLength > limit {
			cleanup()
			return downloadedLink{}, fmt.Errorf("media exceeds size cap")
		}
		f, err := os.CreateTemp(workDir, "resolved-*.mp4")
		if err != nil {
			cleanup()
			return downloadedLink{}, err
		}
		n, err := copyImport(f, resp.Body, limit, resp.ContentLength, func(pct float64) {
			if progress != nil {
				progress(pct)
			}
		})
		closeErr := f.Close()
		if err != nil {
			cleanup()
			return downloadedLink{}, err
		}
		if closeErr != nil {
			cleanup()
			return downloadedLink{}, closeErr
		}
		if n == 0 {
			cleanup()
			return downloadedLink{}, fmt.Errorf("empty media")
		}
		return downloadedLink{path: f.Name(), title: title, attribution: attr, cleanup: cleanup}, nil
	}
	cleanup()
	return downloadedLink{}, fmt.Errorf("could not download media from link")
}

// downloadLinkMedia resolves a reel link clip (YouTube, archive.org/details/...,
// extractor sites, or direct HTTP video) to a temporary local file ready for normalization.
func (s *Service) downloadLinkMedia(ctx context.Context, raw string, progress func(float64)) (downloadedLink, error) {
	if err := s.validateImportURL(ctx, raw); err != nil {
		return downloadedLink{}, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return downloadedLink{}, fmt.Errorf("invalid HTTP media link")
	}
	c, closeClient := s.importHTTPClient()
	defer closeClient()

	const limit = int64(500 << 20)
	const maxDur = 3 * time.Minute

	isArchiveDetails := strings.EqualFold(u.Hostname(), "archive.org") && strings.HasPrefix(u.Path, "/details/")
	if isArchiveDetails {
		// Prefer yt-dlp when available (handles Archive.org .mov/.ogv/.mp4 and
		// offline fake-resolver tests without network calls), falling back to
		// Archive.org's JSON metadata API when yt-dlp is unavailable or fails.
		if s.o.Downloader != nil && s.o.Downloader.Status().Available {
			if dl, err := s.downloadViaExtractor(ctx, c, raw, "video", true, limit, maxDur, progress); err == nil {
				return dl, nil
			} else if ctx.Err() != nil {
				return downloadedLink{}, ctx.Err()
			}
		}
		item := strings.Split(strings.TrimPrefix(u.Path, "/details/"), "/")[0]
		resolvedURL, title, attr, err := resolveArchive(ctx, c, s.archiveOrigin(), item)
		if err != nil {
			if s.o.Downloader != nil && !s.o.Downloader.Status().Available {
				return downloadedLink{}, fmt.Errorf("%w (or install yt-dlp to resolve Archive.org formats)", err)
			}
			return downloadedLink{}, err
		}
		dl, err := s.downloadHTTPFile(ctx, c, resolvedURL, "video", limit, progress)
		if err != nil {
			return downloadedLink{}, err
		}
		dl.title = cleanTitle(title)
		dl.attribution = cleanTitle(attr)
		return dl, nil
	}
	if extras.IsExtractorSiteURL(raw) {
		return s.downloadViaExtractor(ctx, c, raw, "video", true, limit, maxDur, progress)
	}
	dl, err := s.downloadHTTPFile(ctx, c, raw, "video", limit, progress)
	if errors.Is(err, errHTMLPage) && s.o.Downloader != nil {
		return s.downloadViaExtractor(ctx, c, raw, "video", true, limit, maxDur, progress)
	}
	return dl, err
}

var errHTMLPage = errors.New("html media page")

func (s *Service) downloadHTTPFile(ctx context.Context, c *http.Client, raw, kind string, limit int64, progress func(float64)) (downloadedLink, error) {
	resp, err := importGETValidated(ctx, c, raw, nil, s.validateImportURL)
	if err != nil {
		return downloadedLink{}, err
	}
	defer resp.Body.Close()
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return downloadedLink{}, fmt.Errorf("invalid media content type")
	}
	if !strings.HasPrefix(mt, kind+"/") && mt != "application/octet-stream" {
		if mt == "text/html" || mt == "application/xhtml+xml" {
			return downloadedLink{}, errHTMLPage
		}
		return downloadedLink{}, fmt.Errorf("link is not %s media", kind)
	}
	if resp.ContentLength > limit {
		return downloadedLink{}, fmt.Errorf("media exceeds size cap")
	}
	f, err := os.CreateTemp(s.o.Dir, ".link-dl-*")
	if err != nil {
		return downloadedLink{}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	n, err := copyImport(f, resp.Body, limit, resp.ContentLength, func(pct float64) {
		if progress != nil {
			progress(pct)
		}
	})
	closeErr := f.Close()
	if err != nil {
		cleanup()
		return downloadedLink{}, err
	}
	if closeErr != nil {
		cleanup()
		return downloadedLink{}, closeErr
	}
	if n == 0 {
		cleanup()
		return downloadedLink{}, fmt.Errorf("empty media")
	}
	return downloadedLink{path: f.Name(), cleanup: cleanup}, nil
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 255 {
		s = strings.TrimSpace(s[:255])
	}
	return s
}

func (s *Service) commitImportedMedia(ctx context.Context, reel string, j ImportJob, f io.Reader) error {
	var exists int
	if e := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM onair_imports WHERE id=?", j.ID).Scan(&exists); e != nil {
		return e
	}
	if exists == 0 {
		return nil
	}
	var e error
	if reel != "" {
		var c ReelClip
		c, e = s.uploadClip(ctx, reel, j.Name, j.Source, j.Attribution, f, j.ID)
		if e == nil && c.SourceDigest != "" {
			_, _ = s.db.ExecContext(ctx, "UPDATE onair_assets SET source=?,attribution=? WHERE digest=?", j.Source, j.Attribution, c.SourceDigest)
		}
	} else {
		var a Asset
		a, e = s.UploadAsset(ctx, j.Name, j.Kind, j.Attribution, f)
		if e == nil {
			_, e = s.db.ExecContext(ctx, "UPDATE onair_assets SET source=?,attribution=? WHERE digest=?", j.Source, j.Attribution, a.Digest)
		}
	}
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "DELETE FROM onair_imports WHERE id=?", j.ID)
	return e
}

func (s *Service) importHandler(reel bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ImportRequest
		if !body(w, r, &in) {
			return
		}
		id := ""
		if reel {
			id = chi.URLParam(r, "id")
		}
		jobs, e := s.Import(r.Context(), id, in)
		if e != nil {
			result(w, nil, e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(jobs)
	}
}

func copyImport(dst io.Writer, src io.Reader, limit, total int64, progress func(float64)) (int64, error) {
	if total > limit {
		return 0, fmt.Errorf("media exceeds size cap")
	}
	var n int64
	buf := make([]byte, 64<<10)
	last := time.Now()
	for {
		nr, re := src.Read(buf)
		if nr > 0 {
			n += int64(nr)
			if n > limit {
				return n, fmt.Errorf("media exceeds size cap")
			}
			if _, e := dst.Write(buf[:nr]); e != nil {
				return n, e
			}
			if total > 0 && progress != nil && time.Since(last) > time.Second {
				progress(99 * float64(n) / float64(total))
				last = time.Now()
			}
		}
		if re == io.EOF {
			return n, nil
		}
		if re != nil {
			return n, re
		}
	}
}

func (s *Service) resumeImports() {
	rows, e := s.db.QueryContext(s.ctx, "SELECT reel_id,data FROM onair_imports")
	if e != nil {
		return
	}
	type saved struct {
		reel string
		j    ImportJob
	}
	var jobs []saved
	for rows.Next() {
		var v saved
		var b string
		if rows.Scan(&v.reel, &b) == nil && json.Unmarshal([]byte(b), &v.j) == nil && v.j.Status != "error" {
			jobs = append(jobs, v)
		}
	}
	rows.Close()
	for _, v := range jobs {
		v := v
		if !s.enqueue(renderJob{download: true, Hash: "import-" + v.j.ID, prepare: func(ctx context.Context) error { return s.runImport(ctx, v.reel, v.j) }}) {
			v.j.Status = "error"
			v.j.Error = "import queue full"
			s.saveImport(s.ctx, v.reel, v.j)
		}
	}
}
