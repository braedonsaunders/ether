package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// Provider errors. Callers classify with errors.Is.
var (
	errDuplicateTorrent = errors.New("debrid: torrent already exists")
	// ErrAuth is a rejected API key or an account that cannot use the API
	// (expired premium). A configuration problem: never a release failure.
	// It is core.ErrDebridAuth so a playback error can name the cause.
	ErrAuth = core.ErrDebridAuth
	// ErrRateLimited is HTTP 429 or the provider's equivalent.
	ErrRateLimited = errors.New("debrid: rate limited")
	// ErrGone is a torrent, file or link the provider no longer has; the
	// payload's stable identifiers can usually re-add it.
	ErrGone = errors.New("debrid: torrent or link no longer available")
	// ErrBlocked is a file the provider holds but refuses to serve, such
	// as Real-Debrid's infringing_file answer to a cached torrent. Re-adding
	// the torrent cannot help, so on this provider it counts as not cached;
	// another provider may still serve it.
	ErrBlocked = fmt.Errorf("debrid: the provider blocks this file: %w", core.ErrNotCached)
	// ErrAddRefused is a provider refusing to add a torrent at all, as
	// Real-Debrid does with infringing_file both for filtered names and,
	// after a burst of adds, for any torrent for a while. The release is
	// skipped on that provider for now and repeated refusals pause adds.
	ErrAddRefused = fmt.Errorf("debrid: the provider refused to add the torrent: %w", core.ErrNotCached)
	// ErrNoProvider means no debrid provider is enabled.
	ErrNoProvider = errors.New("debrid: no debrid provider is enabled")
	// ErrUnsupported is an operation a provider does not offer.
	ErrUnsupported = errors.New("debrid: not supported by this provider")
	// errNoVideo is a torrent without a playable file (or without the
	// wanted episode): a property of the release, not of a provider.
	errNoVideo = fmt.Errorf("debrid: no playable video file: %w", core.ErrReleaseRejected)
	// errFileMissing is a payload's file absent from its torrent.
	errFileMissing = fmt.Errorf("debrid: file is no longer in the torrent: %w", ErrGone)
)

// rateLimitError carries the provider's Retry-After hint.
type rateLimitError struct{ retryAfter time.Duration }

func (e *rateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *rateLimitError) Unwrap() error { return ErrRateLimited }

// File is one file of a torrent as a provider lists it.
type File struct {
	ID   string // provider file id ("" when the provider has none)
	Path string
	Size int64
	// Link is the provider-side link for the file (Real-Debrid hoster
	// link, AllDebrid file link, Premiumize, Debrid-Link and Offcloud
	// direct link).
	Link     string
	selected bool // Real-Debrid: already selected in a reused torrent
}

// AcquireRequest asks a provider to make a torrent available.
type AcquireRequest struct {
	InfoHash string
	Magnet   string
	// TorrentID reuses a torrent already on the account ("" adds anew).
	TorrentID string
	// Choose picks the wanted file; providers that select files (Real-Debrid)
	// call it before selection.
	Choose         func([]File) (File, error)
	CachedOnly     bool
	RemoveUncached bool
	// MayRemove, when set, is asked before a torrent this call added is
	// removed again (RemoveUncached); false keeps it on the account because
	// a cluster node is streaming from it.
	MayRemove func(ctx context.Context, torrentID string) bool
}

// mayRemove reports whether a torrent added by this request may be removed.
func (r AcquireRequest) mayRemove(ctx context.Context, torrentID string) bool {
	return r.MayRemove == nil || r.MayRemove(ctx, torrentID)
}

// Acquired is a torrent made available by a provider.
type Acquired struct {
	TorrentID string
	File      File
	// Added reports whether this call added the torrent to the account.
	Added bool
}

// Handle identifies a file for link generation.
type Handle struct {
	TorrentID, FileID, Path, Link, Magnet, InfoHash string
	Size                                            int64
}

// DirectLink is a time-limited HTTPS URL for a file.
type DirectLink struct {
	URL     string
	Size    int64
	Expires time.Time
}

// Account is what the admin test reports.
type Account struct {
	User    string
	Premium bool
	Expires time.Time
}

// Provider is one debrid service. Implementations must never wait for an
// actual torrent download: Acquire returns core.ErrNotCached for a torrent
// the service cannot serve instantly (removing a torrent it just added when
// req.CachedOnly and req.RemoveUncached are set).
type Provider interface {
	Kind() string
	// CheckCached reports which hashes are cached; ErrUnsupported when the
	// service has no bulk availability check.
	CheckCached(ctx context.Context, hashes []string) (map[string]bool, error)
	// Find returns the id of a torrent with this hash already on the
	// account ("" when none), so re-adds are avoided; ErrUnsupported when
	// the service keeps no torrent list.
	Find(ctx context.Context, infoHash string) (string, error)
	Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error)
	// Link returns a direct URL for a file; ErrGone when the torrent or
	// file must be re-acquired first.
	Link(ctx context.Context, h Handle) (DirectLink, error)
	Remove(ctx context.Context, torrentID string) error
	Account(ctx context.Context) (Account, error)
}

// ProviderOptions are shared by provider constructors (tests point BaseURL
// at an httptest server and shorten the polls).
type ProviderOptions struct {
	HTTPClient   *http.Client
	BaseURL      string
	PollInterval time.Duration // 0 = 500ms
	ReadyPolls   int           // 0 = 6: how long a cached torrent may take to report ready
	Now          func() time.Time
}

func (o ProviderOptions) withDefaults(base string) ProviderOptions {
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapDebrid, 20*time.Second)
	}
	if o.BaseURL == "" {
		o.BaseURL = base
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.ReadyPolls <= 0 {
		o.ReadyPolls = 6
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// NewProvider builds the provider for a config entry.
func NewProvider(cfg config.DebridProvider, opts ProviderOptions) (Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("debrid provider %q has no API key: %w", displayName(cfg), ErrAuth)
	}
	switch cfg.Kind {
	case config.DebridRealDebrid:
		return newRealDebrid(cfg.APIKey, opts), nil
	case config.DebridTorBox:
		return newTorBox(cfg.APIKey, opts), nil
	case config.DebridAllDebrid:
		return newAllDebrid(cfg.APIKey, opts), nil
	case config.DebridPremiumize:
		return newPremiumize(cfg.APIKey, opts), nil
	case config.DebridDebridLink:
		return newDebridLink(cfg.APIKey, opts), nil
	case config.DebridOffcloud:
		return newOffcloud(cfg.APIKey, opts), nil
	}
	return nil, fmt.Errorf("debrid: unknown provider kind %q", cfg.Kind)
}

// displayName names an account in logs and errors: the owner's name for it,
// else its service ("Real-Debrid"), never the internal id.
func displayName(cfg config.DebridProvider) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if label, ok := kindLabels[cfg.Kind]; ok {
		return label
	}
	if cfg.Kind != "" {
		return cfg.Kind
	}
	return cfg.ID
}

var kindLabels = map[string]string{
	config.DebridRealDebrid: "Real-Debrid",
	config.DebridTorBox:     "TorBox",
	config.DebridAllDebrid:  "AllDebrid",
	config.DebridPremiumize: "Premiumize",
	config.DebridDebridLink: "Debrid-Link",
	config.DebridOffcloud:   "Offcloud",
}

// ---------------------------------------------------------------------------
// HTTP plumbing shared by providers.

// apiRequest is one provider API call.
type apiRequest struct {
	method string
	path   string
	query  url.Values
	form   url.Values // urlencoded body
	body   io.Reader  // raw body (with contentType)
	ctype  string
}

// call performs an API request and returns the status and body. Errors
// never include the request URL: several providers take the key as a
// query parameter.
func call(ctx context.Context, hc *http.Client, base string, r apiRequest, auth func(*http.Request)) (int, []byte, error) {
	if err := core.SpendBackground(ctx, "debrid", base, 1); err != nil {
		return 0, nil, err
	}
	u := base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	ctype := r.ctype
	switch {
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
		ctype = "application/x-www-form-urlencoded"
	case r.body != nil:
		body = r.body
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return 0, nil, errors.New("debrid: invalid request")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ether")
	if auth != nil {
		auth(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, sanitize(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, sanitize(err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, data, &rateLimitError{retryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	return resp.StatusCode, data, nil
}

// sanitize strips the URL from transport errors.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("debrid: request timed out: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("debrid: request failed: %w", ue.Err)
	}
	return err
}

// retryAfter parses a Retry-After header, capped at maxBackoff: a huge
// value must neither overflow the duration nor pause a provider or CDN
// host for longer than this server's own longest back-off.
func retryAfter(v string) time.Duration {
	d, _ := retryafter.Parse(v, time.Now(), maxBackoff)
	return d
}

// statusError maps an unexpected HTTP status to a classified error.
func statusError(kind string, status int, msg string) error {
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrAuth)
	case status == http.StatusNotFound || status == http.StatusGone:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrGone)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %s: %w", kind, msg, ErrRateLimited)
	}
	return fmt.Errorf("%s: HTTP %d: %s", kind, status, msg)
}

func decodeJSON(kind string, data []byte, out any) error {
	if err := json.Unmarshal(bytes.TrimSpace(data), out); err != nil {
		return fmt.Errorf("%s: unexpected response", kind)
	}
	return nil
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// listCache remembers a provider's torrent list (hash -> id) briefly so
// reuse lookups cost at most one list call per minute.
type listCache struct {
	mu     sync.Mutex
	at     time.Time
	byHash map[string]string
	flight *listFlight // used only by TorBox and Debrid-Link
}

const listTTL = time.Minute

func (c *listCache) find(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHash == nil || now.Sub(c.at) > listTTL {
		m, err := load(ctx)
		if err != nil {
			return "", err
		}
		c.byHash, c.at = m, now
	}
	return c.byHash[strings.ToLower(hash)], nil
}

// listFlight publishes one shared refresh without holding mu during I/O.
// Other providers keep their existing synchronous find behavior.
type listFlight struct {
	done chan struct{}
	err  error
}

// shared starts a bounded refresh or joins the one already running. Detached
// refreshes retain admission policy but outlive a speculative caller's cancel,
// so foreground duplicate recovery can use the same account listing.
func (c *listCache) shared(ctx context.Context, now time.Time, load func(context.Context) (map[string]string, error)) *listFlight {
	if c.flight != nil {
		return c.flight
	}
	f := &listFlight{done: make(chan struct{})}
	c.flight = f
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerOpTimeout)
	go func() {
		defer cancel()
		m, err := load(ctx)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			c.byHash, c.at = m, now
		}
		f.err = err
		c.flight = nil
		close(f.done)
	}()
	return f
}

// background returns only fresh local state; a cold snapshot is fetched once
// alongside the caller. Holding mu never entails waiting on network I/O.
func (c *listCache) background(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		return c.byHash[strings.ToLower(hash)]
	}
	c.shared(ctx, now, load)
	return ""
}

// reuse waits only for an already-running refresh, and returns a positive
// fresh hint. A negative snapshot never substitutes for duplicate recovery.
func (c *listCache) reuse(ctx context.Context, now time.Time, hash string) (string, error) {
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		return c.byHash[strings.ToLower(hash)], nil
	}
	return "", nil
}

func (c *listCache) findShared(ctx context.Context, now time.Time, hash string, load func(context.Context) (map[string]string, error)) (string, error) {
	c.mu.Lock()
	if c.byHash != nil && now.Sub(c.at) <= listTTL {
		id := c.byHash[strings.ToLower(hash)]
		c.mu.Unlock()
		return id, nil
	}
	f := c.shared(ctx, now, load)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.done:
	}
	if f.err != nil {
		return "", f.err
	}
	return c.reuse(ctx, now, hash)
}

// remember records an id the provider just returned.
func (c *listCache) remember(hash, id string) {
	c.mu.Lock()
	if c.byHash != nil {
		c.byHash[strings.ToLower(hash)] = id
	}
	c.mu.Unlock()
}

// forget drops a removed torrent.
func (c *listCache) forget(id string) {
	c.mu.Lock()
	for h, v := range c.byHash {
		if v == id {
			delete(c.byHash, h)
		}
	}
	c.mu.Unlock()
}

// recentLinks holds the direct URL a provider listed while acquiring a
// torrent, so the Link call that immediately follows needs no second
// listing. Providers whose listed URL is already the direct link (and that
// document no expiry for it) must not trust a stored URL later: a removed
// torrent would hand out a dead link on every refresh. An entry is used
// once, within recentLinkTTL.
type recentLinks struct {
	mu sync.Mutex
	m  map[string]recentLink
}

type recentLink struct {
	url string
	at  time.Time
}

const recentLinkTTL = time.Minute

func (r *recentLinks) put(torrentID, fileID, u string, now time.Time) {
	if u == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]recentLink{}
	}
	for k, v := range r.m {
		if now.Sub(v.at) > recentLinkTTL {
			delete(r.m, k)
		}
	}
	r.m[torrentID+"\x00"+fileID] = recentLink{url: u, at: now}
}

func (r *recentLinks) take(torrentID, fileID string, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := torrentID + "\x00" + fileID
	v, ok := r.m[k]
	if !ok {
		return ""
	}
	delete(r.m, k)
	if now.Sub(v.at) > recentLinkTTL {
		return ""
	}
	return v.url
}

func (r *recentLinks) forget(torrentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.m {
		if strings.HasPrefix(k, torrentID+"\x00") {
			delete(r.m, k)
		}
	}
}

// ---------------------------------------------------------------------------
// File selection.

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true,
	".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

func isVideo(p string) bool {
	return videoExts[strings.ToLower(path.Ext(p))]
}

// isExtra reports samples, trailers and featurettes.
func isExtra(p string) bool {
	lower := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	for _, tok := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '/' || r == '.' || r == '-' || r == '_' || r == ' ' || r == '[' || r == ']' || r == '(' || r == ')'
	}) {
		switch tok {
		case "sample", "samples", "trailer", "trailers", "featurette", "featurettes", "extras":
			return true
		}
	}
	return false
}

// ChooseFile picks the main video file of a torrent: for a target episode
// the largest file whose name matches it, else the largest video.
func ChooseFile(files []File, target *core.ReleaseTarget) (File, error) {
	var videos []File
	identityMismatch := false
	for _, f := range files {
		if target != nil && !fileIdentityMatches(f, target) {
			identityMismatch = identityMismatch || (isVideo(f.Path) && !isExtra(f.Path) && f.Size > 0)
			continue
		}
		if isVideo(f.Path) && !isExtra(f.Path) && f.Size > 0 {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		if identityMismatch {
			// The torrent may be correct for a different series. A mismatch
			// must never permanently blacklist it for that title as well.
			return File{}, fmt.Errorf("debrid: different series or episode title: %w", core.ErrNotAvailable)
		}
		return File{}, errNoVideo
	}
	sort.SliceStable(videos, func(i, j int) bool { return videos[i].Size > videos[j].Size })
	if target == nil || target.Episode <= 0 {
		return videos[0], nil
	}
	// Pass 1: explicit season + episode match on base name or full path.
	for _, f := range videos {
		normPath := strings.ReplaceAll(f.Path, "\\", "/")
		baseInfo := releaseparse.Parse(path.Base(normPath))
		fullInfo := releaseparse.Parse(strings.ReplaceAll(normPath, "/", " "))
		if len(baseInfo.Seasons) == 0 && len(fullInfo.Seasons) > 0 {
			baseInfo.Seasons = fullInfo.Seasons
		}
		if releaseparse.EpisodeMatches(baseInfo, target.Season, target.Episode) ||
			releaseparse.EpisodeMatches(fullInfo, target.Season, target.Episode) {
			return f, nil
		}
	}
	// Pass 2: classic pack numbering ("Season 2/05 - Title.mkv", "205 - Title.avi",
	// or miniseries "Part 2 - Title.mkv") when no standard SxxExx matched.
	if len(videos) > 1 {
		for _, f := range videos {
			if classicPackEpisodeMatches(f.Path, target.Season, target.Episode) {
				return f, nil
			}
		}
	}
	// A single-episode torrent whose file name carries no episode number.
	// A file that names a different episode (a partial pack) never stands
	// in for the wanted one.
	if len(videos) == 1 {
		info := releaseparse.Parse(strings.NewReplacer("/", " ", "\\", " ").Replace(videos[0].Path))
		if len(info.Episodes) == 0 && !namesOtherClassicEpisode(videos[0].Path, target.Season, target.Episode) {
			return videos[0], nil
		}
	}
	return File{}, fmt.Errorf("debrid: no file for S%02dE%02d in torrent: %w", target.Season, target.Episode, errNoVideo)
}

var (
	rePartEpisode    = regexp.MustCompile(`(?i)\b(?:Part|Pt)\.?\s*(\d{1,2})\b`)
	reLeadingEpisode = regexp.MustCompile(`^\s*(\d{2,3})(?:\s*[-._]\s*|\s+)`)
)

func classicPackEpisodeMatches(filePath string, wantSeason, wantEp int) bool {
	normPath := strings.ReplaceAll(filePath, "\\", "/")
	dirInfo := releaseparse.Parse(strings.ReplaceAll(path.Dir(normPath), "/", " "))
	if len(dirInfo.Seasons) > 0 {
		seasonOK := false
		for _, s := range dirInfo.Seasons {
			if s == wantSeason {
				seasonOK = true
			}
		}
		if !seasonOK {
			return false
		}
	}
	base := path.Base(normPath)
	if wantSeason == 1 {
		if m := rePartEpisode.FindStringSubmatch(base); m != nil {
			if ep, err := strconv.Atoi(m[1]); err == nil && ep == wantEp {
				return true
			}
		}
	}
	if m := reLeadingEpisode.FindStringSubmatch(base); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return false
		}
		// Avoid mistaking 4-digit years or resolutions ("1080p") for episode numbers.
		if n == wantEp && (len(dirInfo.Seasons) > 0 || wantSeason == 1) {
			return true
		}
		if wantSeason > 0 && wantEp > 0 && wantEp < 100 && n == wantSeason*100+wantEp {
			return true
		}
	}
	return false
}

func namesOtherClassicEpisode(filePath string, wantSeason, wantEp int) bool {
	base := path.Base(strings.ReplaceAll(filePath, "\\", "/"))
	if wantSeason == 1 {
		if m := rePartEpisode.FindStringSubmatch(base); m != nil {
			if ep, err := strconv.Atoi(m[1]); err == nil && ep > 0 && ep != wantEp {
				return true
			}
		}
	}
	return false
}

// sameFile chooses the file a payload already names (link refresh after
// re-adding a torrent, or on another provider): same base name and size,
// else the only file of that size.
func sameFile(p Payload) func([]File) (File, error) {
	return func(files []File) (File, error) {
		name := strings.ToLower(p.Name())
		var bySize []File
		for _, f := range files {
			if f.Size != p.Size {
				continue
			}
			if strings.ToLower(path.Base(strings.ReplaceAll(f.Path, "\\", "/"))) == name {
				return f, nil
			}
			bySize = append(bySize, f)
		}
		if len(bySize) == 1 {
			return bySize[0], nil
		}
		return File{}, fmt.Errorf("%s: %w", p.Name(), errFileMissing)
	}
}

func fileIdentityMatches(f File, target *core.ReleaseTarget) bool {
	if target == nil {
		return true
	}
	idMatch, idConflict := releaseparse.CompareTitleIDs(target.ReportedIDs, target.ExpectedIDs)
	if idConflict {
		return false
	}
	full := strings.NewReplacer("/", " ", "\\", " ").Replace(f.Path)
	info := releaseparse.Parse(full)
	year := releaseparse.EpisodeSeriesYear(full, info.Year)
	return releaseparse.SeriesYearMatches(year, target.ShowYear) && (year != 0 || idMatch || releaseparse.EpisodeTitleMatches(path.Base(strings.ReplaceAll(f.Path, "\\", "/")), target.EpisodeTitle))
}
