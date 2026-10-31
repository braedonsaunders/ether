package debrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Debrid-Link API v2 (https://debrid-link.com/api_doc/v2; the page renders
// the JSON at /api/v2/api_doc/infos and /api/v2/api_doc/errors). The owner's
// private API key (https://debrid-link.com/webapp/apikey) is sent as a
// Bearer token. Successful answers are {"success":true,"value":...}; errors
// are {"success":false,"error":"badToken"} with an HTTP 4xx/5xx status.
//
// V2 has no bulk availability check (v1's /seedbox/cached is gone), but
// /seedbox/add documents that "the hash is only added if it is already
// cached on our servers": a cached-only add sends the bare info hash, so
// the cached guard is server-side and atomic, as with TorBox's
// add_only_if_cached. The error code returned for an uncached hash is not
// documented; notAddTorrent and badArguments on a hash add are read as
// "not cached".
//
// Endpoints used:
//
//	POST   /seedbox/add             form url=hash|magnet  -> value{id,hashString,status,isZip,files[{id,name,size,downloadUrl,downloadPercent}]}
//	GET    /seedbox/list?ids=id                           -> value[{...same torrent object, all files}]
//	GET    /seedbox/list?perPage=100                      -> value[...] (reuse by hashString)
//	DELETE /seedbox/{id}/remove                           -> value[ids]
//	GET    /account/infos                                 -> value{username, accountType, premiumLeft}
//
// files[].downloadUrl is already a direct HTTPS link; the API documents no
// expiry for it, so Link re-reads the torrent rather than trusting a stored
// URL. A file's "name" is documented only by a bare-name example; whether
// list mode includes sub-folders is not stated.
const (
	debridLinkBase    = "https://debrid-link.com/api/v2"
	debridLinkLinkTTL = 6 * time.Hour
	dlStatusFinished  = 100
)

var reInfoHash = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type debridLink struct {
	key     string
	opts    ProviderOptions
	list    listCache
	recent  recentLinks
	knownMu sync.Mutex
	known   map[string]string // validated owned ids; no network lock on Play
}

func newDebridLink(key string, opts ProviderOptions) *debridLink {
	return &debridLink{key: key, opts: opts.withDefaults(debridLinkBase), known: make(map[string]string)}
}

func (p *debridLink) Kind() string { return "debridlink" }

type dlEnvelope struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Desc    string          `json:"error_description"`
	Value   json.RawMessage `json:"value"`
}

type dlFile struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	DownloadURL     string `json:"downloadUrl"`
	DownloadPercent int    `json:"downloadPercent"`
}

type dlTorrent struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	HashString      string   `json:"hashString"`
	Status          int      `json:"status"`
	DownloadPercent int      `json:"downloadPercent"`
	IsZip           bool     `json:"isZip"`
	SrvMaint        bool     `json:"srvMaint"`
	Files           []dlFile `json:"files"`
}

func (p *debridLink) do(ctx context.Context, r apiRequest) (json.RawMessage, error) {
	status, body, err := call(ctx, p.opts.HTTPClient, p.opts.BaseURL, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+p.key)
	})
	if err != nil {
		return nil, err
	}
	var env dlEnvelope
	derr := decodeJSON("debrid-link", body, &env)
	if status >= 200 && status < 300 && derr == nil && env.Success {
		return env.Value, nil
	}
	if derr != nil && status >= 200 && status < 300 {
		return nil, derr
	}
	return nil, p.classify(status, env)
}

// dlError keeps Debrid-Link's error code next to its classification.
type dlError struct {
	code string
	err  error
}

func (e *dlError) Error() string { return e.err.Error() }
func (e *dlError) Unwrap() error { return e.err }

func dlCode(err error) string {
	var e *dlError
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

func (p *debridLink) classify(status int, env dlEnvelope) error {
	return &dlError{code: env.Error, err: dlClassify(status, env)}
}

// dlClassify maps the error codes listed by /api/v2/api_doc/errors.
func dlClassify(status int, env dlEnvelope) error {
	msg := env.Desc
	if msg == "" {
		msg = env.Error
	}
	if lower := strings.ToLower(env.Error + " " + msg); strings.Contains(lower, "duplicate") || strings.Contains(lower, "already exists") || strings.Contains(lower, "already added") {
		// The torrent is already on the account; Acquire recovers it by
		// hash (findFresh) instead of adding a second entry.
		return errDuplicateTorrent
	}
	switch env.Error {
	case "badToken", "unverifiedEmail", "access_denied", "invalid_client", "unauthorized_client", "invalid_grant", "notFreeHost",
		"maxData", "maxDataHost", "maxLink", "maxLinkHost":
		// Invalid or expired key, a free account that must verify its email,
		// an app without permission, a feature that needs premium, or the
		// account's daily traffic exhausted: the account, not the release.
		return fmt.Errorf("debrid-link: %s: %w", msg, ErrAuth)
	case "floodDetected", "maxTorrent", "maxTransfer", "freeServerOverload":
		// Too many requests (~1h), the daily new-torrent or simultaneous
		// transfer limit, or overloaded free servers: back off and retry.
		return fmt.Errorf("debrid-link: %s: %w", msg, ErrRateLimited)
	case "badId", "fileNotFound":
		return fmt.Errorf("debrid-link: %s: %w", msg, ErrGone)
	case "infringingFile": // unavailable for legal reasons
		return fmt.Errorf("debrid-link: %s: %w", msg, ErrBlocked)
	case "notAddTorrent", "torrentTooBig":
		// The torrent could not be added, or is too big for this service:
		// another provider may still have it.
		return fmt.Errorf("debrid-link: %s: %w", msg, core.ErrNotCached)
	case "badTorrentFile":
		return fmt.Errorf("debrid-link: %s: %w", msg, core.ErrReleaseRejected)
	}
	// internalError, server_error, notDebrid, maintenanceHost,
	// fileNotAvailable, unknowR, badArguments: transient or a caller bug.
	if status >= 200 && status < 300 {
		status = http.StatusBadGateway
	}
	return statusError("debrid-link", status, msg)
}

func (p *debridLink) CheckCached(context.Context, []string) (map[string]bool, error) {
	return nil, ErrUnsupported
}

// Find looks the hash up in the account's most recent torrents.
func (p *debridLink) Find(ctx context.Context, hash string) (string, error) {
	if id := p.findKnown(hash); id != "" {
		return id, nil
	}
	return p.list.findShared(ctx, p.opts.Now(), hash, p.loadList)
}

func (p *debridLink) findBackground(ctx context.Context, hash string) string {
	return p.list.background(ctx, p.opts.Now(), hash, p.loadList)
}

func (p *debridLink) loadList(ctx context.Context) (map[string]string, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/seedbox/list", query: url.Values{"perPage": {"100"}}})
	if err != nil {
		return nil, err
	}
	var items []dlTorrent
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, errors.New("debrid-link: unexpected torrent list")
	}
	m := map[string]string{}
	for _, it := range items {
		if it.ID != "" && dlComplete(&it) {
			m[strings.ToLower(it.HashString)] = it.ID
		}
	}
	return m, nil
}

func dlComplete(t *dlTorrent) bool {
	return t.DownloadPercent >= 100 || t.Status == dlStatusFinished
}

// torrent lists one torrent with all its files (?ids= never zips them).
func (p *debridLink) torrent(ctx context.Context, id string) (*dlTorrent, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/seedbox/list", query: url.Values{"ids": {id}}})
	if err != nil {
		return nil, err
	}
	var items []dlTorrent
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, errors.New("debrid-link: unexpected torrent list")
	}
	for i := range items {
		if items[i].ID == id {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("debrid-link: torrent %s not listed: %w", id, ErrGone)
}

func dlFiles(t *dlTorrent) []File {
	out := make([]File, 0, len(t.Files))
	for _, f := range t.Files {
		out = append(out, File{ID: f.ID, Path: f.Name, Size: f.Size, Link: f.DownloadURL})
	}
	return out
}

func dlFileByID(t *dlTorrent, id string) *dlFile {
	for i := range t.Files {
		if t.Files[i].ID == id {
			return &t.Files[i]
		}
	}
	return nil
}

func (p *debridLink) Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error) {
	id, added := req.TorrentID, false
	var t *dlTorrent
	if id == "" {
		src, hashOnly := req.Magnet, false
		if req.CachedOnly && reInfoHash.MatchString(req.InfoHash) {
			src, hashOnly = strings.ToLower(req.InfoHash), true
		}
		data, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/seedbox/add", form: url.Values{"url": {src}}})
		if errors.Is(err, errDuplicateTorrent) {
			// The account already holds this hash (added by another app
			// or node). Only this collision needs a fresh account-wide
			// hash lookup; never delete the existing item.
			id, err = p.findFresh(ctx, req.InfoHash)
			if err != nil {
				return nil, err
			}
			if id == "" {
				return nil, fmt.Errorf("debrid-link: duplicate torrent not listed: %w", core.ErrSourceUnavailable)
			}
		} else if err != nil {
			switch {
			case errors.Is(err, ErrBlocked):
				return nil, fmt.Errorf("debrid-link: add refused: %w", ErrAddRefused)
			case hashOnly && dlCode(err) == "badArguments":
				return nil, fmt.Errorf("debrid-link: hash not cached: %w", core.ErrNotCached)
			}
			return nil, err
		} else {
			var out dlTorrent
			if err := json.Unmarshal(data, &out); err != nil || out.ID == "" {
				return nil, errors.New("debrid-link: add returned no torrent id")
			}
			id, added = out.ID, true
			if !out.IsZip && len(out.Files) > 0 {
				t = &out
			}
		}
	}
	cleanup := func() {
		if added && req.RemoveUncached {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if req.mayRemove(rctx, id) {
				_ = p.Remove(rctx, id)
			}
			cancel()
		}
	}
	var chosen File
	ready := false
	for polls := 0; polls < p.opts.ReadyPolls; polls++ {
		if t == nil || polls > 0 {
			var err error
			if t, err = p.torrent(ctx, id); err != nil && (!added || !errors.Is(err, ErrGone)) {
				return nil, err
			}
		}
		// A torrent just added may not be listed yet, and its metadata may
		// be incomplete at first.
		if t != nil && len(t.Files) > 0 && !t.IsZip {
			var err error
			if chosen, err = req.Choose(dlFiles(t)); err != nil {
				cleanup()
				return nil, err
			}
			if f := dlFileByID(t, chosen.ID); f != nil && f.DownloadPercent >= 100 && f.DownloadURL != "" {
				ready = true
				break
			}
		}
		if err := sleep(ctx, p.opts.PollInterval); err != nil {
			return nil, err
		}
	}
	if !ready {
		if req.CachedOnly {
			cleanup()
		}
		return nil, fmt.Errorf("debrid-link: torrent is not ready: %w", core.ErrNotCached)
	}
	if t.SrvMaint {
		return nil, fmt.Errorf("debrid-link: the torrent's server is under maintenance: %w", core.ErrSourceUnavailable)
	}
	p.rememberKnown(req.InfoHash, id)
	p.list.remember(req.InfoHash, id)
	p.recent.put(id, chosen.ID, chosen.Link, p.opts.Now())
	return &Acquired{TorrentID: id, File: chosen, Added: added}, nil
}

func (p *debridLink) Link(ctx context.Context, h Handle) (DirectLink, error) {
	if h.TorrentID == "" || h.FileID == "" {
		return DirectLink{}, ErrGone
	}
	now := p.opts.Now()
	if u := p.recent.take(h.TorrentID, h.FileID, now); u != "" {
		return DirectLink{URL: u, Size: h.Size, Expires: now.Add(debridLinkLinkTTL)}, nil
	}
	t, err := p.torrent(ctx, h.TorrentID)
	if err != nil {
		return DirectLink{}, err
	}
	f := dlFileByID(t, h.FileID)
	if f == nil {
		return DirectLink{}, errFileMissing
	}
	if f.DownloadPercent < 100 || f.DownloadURL == "" {
		return DirectLink{}, fmt.Errorf("debrid-link: file is not ready: %w", ErrGone)
	}
	if t.SrvMaint {
		return DirectLink{}, fmt.Errorf("debrid-link: the torrent's server is under maintenance: %w", core.ErrSourceUnavailable)
	}
	size := f.Size
	if size <= 0 {
		size = h.Size
	}
	return DirectLink{URL: f.DownloadURL, Size: size, Expires: now.Add(debridLinkLinkTTL)}, nil
}

func (p *debridLink) Remove(ctx context.Context, torrentID string) error {
	if torrentID == "" {
		return nil
	}
	p.list.forget(torrentID)
	p.forgetKnown(torrentID)
	p.recent.forget(torrentID)
	_, err := p.do(ctx, apiRequest{method: http.MethodDelete, path: "/seedbox/" + url.PathEscape(torrentID) + "/remove"})
	return err
}

func (p *debridLink) Account(ctx context.Context) (Account, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/account/infos"})
	if err != nil {
		return Account{}, err
	}
	var out struct {
		Username    string `json:"username"`
		AccountType int    `json:"accountType"` // 0 free, 1 premium, 2 lifetime
		PremiumLeft int64  `json:"premiumLeft"` // seconds; 0 for free users
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Account{}, errors.New("debrid-link: unexpected account response")
	}
	a := Account{User: out.Username, Premium: out.AccountType == 2 || (out.AccountType == 1 && out.PremiumLeft > 0)}
	if out.AccountType == 1 && out.PremiumLeft > 0 {
		a.Expires = p.opts.Now().Add(time.Duration(out.PremiumLeft) * time.Second)
	}
	return a, nil
}
