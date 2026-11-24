package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Offcloud API (https://offcloud.com/api, redirected to a login page as of
// 2026; the official reference is github.com/Offcloud/offcloud-api). That
// reference documents only the key query parameter (?key=), POST /api/cache
// with "hashes", POST /api/cloud with "url" returning
// {requestId, fileName, status: created|downloaded|error, originalLink},
// POST /api/cloud/status with "requestId", and GET /api/cloud/explore/{id};
// it gives no response shapes for the cache, status or explore calls.
//
// The shapes and the remaining endpoints below follow the two maintained
// open-source clients, which agree with each other: StremThru's
// store/offcloud (Go) and TheBeastLT/node-offcloud (the Torrentio author's
// client). Both send the key as a Bearer token with JSON bodies; Ether sends
// it both ways. Errors are {"error":"..."} objects ("NOAUTH" for a bad key);
// a cloud add the plan does not cover answers {"not_available": "premium" |
// "links" | "proxy" | "video" | "cloud"}.
//
// Endpoints used:
//
//	POST /cache/info           {"urls":[magnet...]}      -> [{cached}] in request order
//	POST /cloud                {"url":magnet}            -> {requestId, fileName, status}
//	POST /cloud/status         {"requestId":id}          -> {status:{status}} (a bare string is accepted too)
//	GET  /cloud/explore/{id}?format=detailed             -> {files:[{id,name,size,path,url}]}
//	GET  /cloud/history                                  -> [{requestId, status, originalLink}] (reuse by hash)
//	POST /cloud/remove         {"requestIds":[id]}
//	GET  /account/info                                   -> {user_id, is_premium, expiration_date "2026-12-31"}
//
// Unverified against the live service: the batch limit of /cache/info (Ether
// sends 50 per call), the /cloud/remove body (node-offcloud only), and the
// error strings other than NOAUTH, which are matched by wording.
const (
	offcloudBase    = "https://offcloud.com/api"
	offcloudLinkTTL = 6 * time.Hour
	offcloudBatch   = 50
)

type offcloud struct {
	key    string
	opts   ProviderOptions
	list   listCache
	recent recentLinks
}

func newOffcloud(key string, opts ProviderOptions) *offcloud {
	return &offcloud{key: key, opts: opts.withDefaults(offcloudBase)}
}

func (p *offcloud) Kind() string { return "offcloud" }

type ocError struct {
	Error        string `json:"error"`
	NotAvailable string `json:"not_available"`
}

type ocFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	URL  string `json:"url"`
}

// do sends r with a JSON body (when body != nil) and returns the raw
// answer, or a classified error when the answer is an error object.
func (p *offcloud) do(ctx context.Context, r apiRequest, body any) (json.RawMessage, error) {
	if r.query == nil {
		r.query = url.Values{}
	}
	r.query.Set("key", p.key)
	if body != nil {
		b, _ := json.Marshal(body)
		r.body, r.ctype = bytes.NewReader(b), "application/json"
	}
	status, data, err := call(ctx, p.opts.HTTPClient, p.opts.BaseURL, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+p.key)
	})
	if err != nil {
		if status != 0 {
			logProviderHTTPError(ctx, p.key, status, data)
		}
		return nil, err
	}
	t := bytes.TrimSpace(data)
	var e ocError
	if len(t) > 0 && t[0] == '{' {
		_ = json.Unmarshal(t, &e)
	}
	if status >= 200 && status < 300 && e.Error == "" && e.NotAvailable == "" {
		if len(t) == 0 {
			return nil, nil
		}
		if t[0] != '{' && t[0] != '[' {
			return nil, errors.New("offcloud: unexpected response")
		}
		return t, nil
	}
	msg := e.Error
	if msg == "" {
		msg = e.NotAvailable
	}
	logProviderResponse(ctx, p.key, status, "", msg)
	return nil, ocClassify(status, e)
}

// ocClassify maps Offcloud errors. Only NOAUTH and the not_available values
// are documented; the rest is matched by wording.
func ocClassify(status int, e ocError) error {
	if e.NotAvailable != "" {
		// The account must buy the named addon (premium, links, proxy,
		// video, cloud): a plan problem, not a release failure.
		return fmt.Errorf("offcloud: not available on this plan (%s): %w", e.NotAvailable, ErrAuth)
	}
	msg := e.Error
	lower := strings.ToLower(msg)
	switch {
	case strings.EqualFold(msg, "NOAUTH") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "log in") || strings.Contains(lower, "logged in") || strings.Contains(lower, "api key") ||
		strings.Contains(lower, "apikey") || strings.Contains(lower, "premium"):
		return fmt.Errorf("offcloud: %s: %w", msg, ErrAuth)
	case strings.Contains(lower, "too many") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "slow down"):
		return fmt.Errorf("offcloud: %s: %w", msg, ErrRateLimited)
	case strings.Contains(lower, "dmca") || strings.Contains(lower, "copyright") || strings.Contains(lower, "infring") || strings.Contains(lower, "blocked"):
		return fmt.Errorf("offcloud: %s: %w", msg, ErrBlocked)
	case strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "no such") ||
		strings.Contains(lower, "invalid request id") || strings.Contains(lower, "invalid requestid"):
		return fmt.Errorf("offcloud: %s: %w", msg, ErrGone)
	}
	if status >= 200 && status < 300 {
		status = http.StatusBadGateway
	}
	return statusError("offcloud", status, msg)
}

func magnetFor(hash string) string { return "magnet:?xt=urn:btih:" + strings.ToLower(hash) }

func (p *offcloud) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	out := make(map[string]bool, len(hashes))
	for start := 0; start < len(hashes); start += offcloudBatch {
		batch := hashes[start:min(start+offcloudBatch, len(hashes))]
		urls := make([]string, len(batch))
		for i, h := range batch {
			urls[i] = magnetFor(h)
		}
		data, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/cache/info"}, map[string]any{"urls": urls})
		if err != nil {
			return nil, err
		}
		var items []struct {
			Cached bool `json:"cached"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, errors.New("offcloud: unexpected cache response")
		}
		for i, h := range batch {
			out[strings.ToLower(h)] = i < len(items) && items[i].Cached
		}
	}
	return out, nil
}

var reMagnetHash = regexp.MustCompile(`(?i)urn:btih:([0-9a-f]{40})`)

// Find looks the hash up in the account's cloud history.
func (p *offcloud) Find(ctx context.Context, hash string) (string, error) {
	return p.list.find(ctx, p.opts.Now(), hash, func(ctx context.Context) (map[string]string, error) {
		data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/cloud/history"}, nil)
		if err != nil {
			return nil, err
		}
		var items []struct {
			RequestID    string `json:"requestId"`
			Status       string `json:"status"`
			OriginalLink string `json:"originalLink"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, errors.New("offcloud: unexpected cloud history")
		}
		m := map[string]string{}
		for _, it := range items {
			if it.Status != "downloaded" || it.RequestID == "" {
				continue
			}
			if mh := reMagnetHash.FindStringSubmatch(it.OriginalLink); mh != nil {
				m[strings.ToLower(mh[1])] = it.RequestID
			}
		}
		return m, nil
	})
}

// status reports a cloud download's state: created, queued, downloading,
// downloaded, error or canceled.
func (p *offcloud) status(ctx context.Context, id string) (string, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/cloud/status"}, map[string]string{"requestId": id})
	if err != nil {
		return "", err
	}
	var out struct {
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", errors.New("offcloud: unexpected status response")
	}
	var s string
	if json.Unmarshal(out.Status, &s) == nil {
		return s, nil
	}
	var inner struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(out.Status, &inner) == nil && inner.Status != "" {
		return inner.Status, nil
	}
	return "", fmt.Errorf("offcloud: cloud download %s not listed: %w", id, ErrGone)
}

// files lists a cloud download's files with their direct links.
func (p *offcloud) files(ctx context.Context, id string) ([]File, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/cloud/explore/" + url.PathEscape(id), query: url.Values{"format": {"detailed"}}}, nil)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Files []ocFile `json:"files"`
	}
	if len(data) > 0 && data[0] == '[' {
		err = json.Unmarshal(data, &wrap.Files)
	} else {
		err = json.Unmarshal(data, &wrap)
	}
	if err != nil {
		return nil, errors.New("offcloud: unexpected explore response")
	}
	out := make([]File, 0, len(wrap.Files))
	for _, f := range wrap.Files {
		if f.URL == "" {
			continue
		}
		p := f.Path
		if p == "" {
			p = f.Name
		}
		id := f.ID
		if id == "" {
			id = p
		}
		out = append(out, File{ID: id, Path: p, Size: f.Size, Link: f.URL})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("offcloud: no files for cloud download %s: %w", id, ErrGone)
	}
	return out, nil
}

func (p *offcloud) Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error) {
	if req.Private && !req.privateChecked {
		var perr error
		req, perr = prepareAcquire(ctx, p, req, false)
		if perr != nil {
			return nil, perr
		}
	}
	id, added := req.TorrentID, false
	state := ""
	if id == "" {
		// A cloud add of an uncached magnet starts a real download, and
		// Offcloud has no add-only-if-cached flag: ask the cache first.
		if req.CachedOnly {
			cached, err := p.CheckCached(ctx, []string{req.InfoHash})
			if err != nil {
				return nil, err
			}
			if !cached[strings.ToLower(req.InfoHash)] {
				return nil, fmt.Errorf("offcloud: %w", core.ErrNotCached)
			}
		}
		data, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/cloud"}, map[string]string{"url": req.Magnet})
		if err != nil {
			return nil, err
		}
		var out struct {
			RequestID string `json:"requestId"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(data, &out); err != nil || out.RequestID == "" {
			return nil, errors.New("offcloud: cloud add returned no request id")
		}
		id, added, state = out.RequestID, true, out.Status
	}
	cleanup := func() bool {
		if added && req.RemoveUncached {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if req.mayRemove(rctx, id) {
				return p.Remove(rctx, id) == nil
			}
		}
		return false
	}
	for polls := 0; state != "downloaded" && polls < p.opts.ReadyPolls; polls++ {
		if polls > 0 || state == "" {
			var err error
			if state, err = p.status(ctx, id); err != nil {
				return nil, err
			}
		}
		if state == "downloaded" {
			break
		}
		if state == "error" || state == "canceled" {
			logProviderResponse(ctx, p.key, http.StatusOK, state, "cloud download "+state)
			cleanup()
			return nil, fmt.Errorf("offcloud: cloud download %s: %w", state, core.ErrNotCached)
		}
		if err := sleep(ctx, p.opts.PollInterval); err != nil {
			return nil, err
		}
	}
	if state != "downloaded" {
		err := fmt.Errorf("offcloud: cloud download is not ready: %w", core.ErrNotCached)
		if req.CachedOnly && cleanup() {
			return nil, err
		}
		return nil, pending(id, err)
	}
	files, err := p.files(ctx, id)
	if err != nil {
		return nil, err
	}
	chosen, err := req.Choose(files)
	if err != nil {
		cleanup()
		return nil, err
	}
	p.list.remember(req.InfoHash, id)
	p.recent.put(id, chosen.ID, chosen.Link, p.opts.Now())
	return &Acquired{TorrentID: id, File: chosen, Added: added}, nil
}

// Link re-reads the cloud download's file list rather than trusting a
// stored URL, which outlives the download being removed.
func (p *offcloud) Link(ctx context.Context, h Handle) (DirectLink, error) {
	if h.TorrentID == "" {
		return DirectLink{}, ErrGone
	}
	now := p.opts.Now()
	if u := p.recent.take(h.TorrentID, h.FileID, now); u != "" {
		return DirectLink{URL: u, Size: h.Size, Expires: now.Add(offcloudLinkTTL)}, nil
	}
	files, err := p.files(ctx, h.TorrentID)
	if err != nil {
		return DirectLink{}, err
	}
	var f File
	found := false
	for _, c := range files {
		if h.FileID != "" && c.ID == h.FileID {
			f, found = c, true
			break
		}
	}
	if !found {
		if f, err = sameFile(Payload{Path: h.Path, Size: h.Size})(files); err != nil {
			return DirectLink{}, err
		}
	}
	size := f.Size
	if size <= 0 {
		size = h.Size
	}
	return DirectLink{URL: f.Link, Size: size, Expires: now.Add(offcloudLinkTTL)}, nil
}

func (p *offcloud) Remove(ctx context.Context, torrentID string) error {
	if torrentID == "" {
		return nil
	}
	p.list.forget(torrentID)
	p.recent.forget(torrentID)
	_, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/cloud/remove"}, map[string][]string{"requestIds": {torrentID}})
	return err
}

func (p *offcloud) Account(ctx context.Context) (Account, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/account/info"}, nil)
	if err != nil {
		return Account{}, err
	}
	var out struct {
		UserID         string `json:"user_id"`
		IsPremium      bool   `json:"is_premium"`
		ExpirationDate string `json:"expiration_date"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Account{}, errors.New("offcloud: unexpected account response")
	}
	a := Account{User: out.UserID, Premium: out.IsPremium}
	if t, err := time.Parse(time.DateOnly, out.ExpirationDate); err == nil {
		a.Expires = t
		if !t.After(p.opts.Now()) {
			a.Premium = false
		}
	}
	return a, nil
}
