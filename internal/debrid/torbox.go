package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TorBox API v1 (https://api.torbox.app/v1/api). Bearer token auth;
// requestdl additionally takes the key as the token query parameter.
//
// Endpoints used:
//
//	GET  /torrents/checkcached?hash=a,b&format=object&list_files=true -> data{hash: {name,size,files}}
//	POST /torrents/createtorrent  multipart magnet=, add_only_if_cached= -> data{torrent_id}
//	GET  /torrents/mylist?id=&bypass_cache=true -> data{id,hash,download_present,download_finished,files[{id,name,size}]}
//	GET  /torrents/mylist?bypass_cache=true     -> data[...] (reuse by hash)
//	GET  /torrents/requestdl?token=&torrent_id=&file_id= -> data "https://..."
//	POST /torrents/controltorrent {torrent_id, operation:"delete"}
//	GET  /user/me -> data{email, plan, premium_expires_at}
const (
	torBoxBase    = "https://api.torbox.app/v1/api"
	torBoxLinkTTL = 3 * time.Hour
)

type torBox struct {
	key     string
	opts    ProviderOptions
	list    listCache
	knownMu sync.Mutex
	known   map[string]string // validated owned ids; no network lock on Play
}

func newTorBox(key string, opts ProviderOptions) *torBox {
	return &torBox{key: key, opts: opts.withDefaults(torBoxBase), known: make(map[string]string)}
}

func (p *torBox) Kind() string { return "torbox" }

type tbEnvelope struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

type tbTorrent struct {
	ID               json.Number `json:"id"`
	Hash             string      `json:"hash"`
	Name             string      `json:"name"`
	DownloadPresent  bool        `json:"download_present"`
	DownloadFinished bool        `json:"download_finished"`
	DownloadState    string      `json:"download_state"`
	Files            []struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
		Size int64       `json:"size"`
	} `json:"files"`
}

func (p *torBox) do(ctx context.Context, r apiRequest) (json.RawMessage, error) {
	status, body, err := call(ctx, p.opts.HTTPClient, p.opts.BaseURL, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+p.key)
	})
	if err != nil {
		return nil, err
	}
	var env tbEnvelope
	derr := decodeJSON("torbox", body, &env)
	if status >= 200 && status < 300 && derr == nil && env.Success {
		return env.Data, nil
	}
	return nil, p.classify(status, env)
}

func (p *torBox) classify(status int, env tbEnvelope) error {
	msg := strings.TrimSpace(env.Detail)
	if msg == "" {
		msg = env.Error
	}
	code := strings.ToUpper(env.Error)
	switch {
	case code == "DUPLICATE_ITEM":
		return errDuplicateTorrent
	case strings.Contains(code, "AUTH") || strings.Contains(code, "TOKEN") || strings.Contains(code, "PLAN") || code == "NO_AUTH":
		return fmt.Errorf("torbox: %s: %w", msg, ErrAuth)
	case strings.Contains(code, "NOT_CACHED") || strings.Contains(strings.ToLower(msg), "not cached"):
		return fmt.Errorf("torbox: %s: %w", msg, core.ErrNotCached)
	case code == "ITEM_NOT_FOUND" || strings.Contains(code, "NOT_FOUND"):
		return fmt.Errorf("torbox: %s: %w", msg, ErrGone)
	case code == "ACTIVE_LIMIT" || code == "COOLDOWN_LIMIT" || strings.Contains(code, "RATE"):
		return fmt.Errorf("torbox: %s: %w", msg, ErrRateLimited)
	}
	if status >= 200 && status < 300 {
		status = http.StatusBadGateway
	}
	return statusError("torbox", status, msg)
}

func (p *torBox) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	out := make(map[string]bool, len(hashes))
	// TorBox documents approximately 100 hashes per GET (URL-size bound).
	// V1 hashes are fixed 40-byte strings: this stays below a 4.2 KiB URL
	// and avoids serial availability round trips for ordinary search sets.
	for start := 0; start < len(hashes); start += 100 {
		batch := hashes[start:min(start+100, len(hashes))]
		q := url.Values{"hash": {strings.Join(batch, ",")}, "format": {"object"}, "list_files": {"false"}}
		data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/checkcached", query: q})
		if err != nil {
			return nil, err
		}
		var found map[string]json.RawMessage
		if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
			_ = json.Unmarshal(t, &found)
		}
		for _, h := range batch {
			_, ok := found[strings.ToLower(h)]
			if !ok {
				_, ok = found[strings.ToUpper(h)]
			}
			out[strings.ToLower(h)] = ok
		}
	}
	return out, nil
}

func (p *torBox) Find(ctx context.Context, hash string) (string, error) {
	if id := p.findKnown(hash); id != "" {
		return id, nil
	}
	return p.list.findShared(ctx, p.opts.Now(), hash, p.loadList)
}

func (p *torBox) findBackground(ctx context.Context, hash string) string {
	return p.list.background(ctx, p.opts.Now(), hash, p.loadList)
}

func (p *torBox) loadList(ctx context.Context) (map[string]string, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/mylist", query: url.Values{"bypass_cache": {"true"}}})
	if err != nil {
		return nil, err
	}
	var items []tbTorrent
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		if err := json.Unmarshal(t, &items); err != nil {
			return nil, errors.New("torbox: unexpected torrent list")
		}
	}
	m := map[string]string{}
	for _, it := range items {
		if it.DownloadPresent {
			m[strings.ToLower(it.Hash)] = it.ID.String()
		}
	}
	return m, nil
}

func (p *torBox) torrent(ctx context.Context, id string) (*tbTorrent, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/mylist", query: url.Values{"id": {id}, "bypass_cache": {"true"}}})
	if err != nil {
		return nil, err
	}
	var t tbTorrent
	if err := json.Unmarshal(data, &t); err != nil || t.ID.String() == "" {
		return nil, fmt.Errorf("torbox: torrent %s not listed: %w", id, ErrGone)
	}
	return &t, nil
}

func (p *torBox) Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error) {
	id, added := req.TorrentID, false
	if id == "" {
		// createtorrent enforces add_only_if_cached atomically. An extra
		// checkcached here repeats the shortlist's bulk query, adds a network
		// round trip and still cannot protect against cache expiry between
		// that check and creation. Keep the server-side guard authoritative.
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("magnet", req.Magnet)
		if req.CachedOnly {
			_ = mw.WriteField("add_only_if_cached", "true")
		}
		_ = mw.Close()
		data, err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/torrents/createtorrent", body: &body, ctype: mw.FormDataContentType()})
		if errors.Is(err, errDuplicateTorrent) {
			// The account may contain an item added by another app/node.
			// Only this collision needs a fresh account-wide hash lookup;
			// never identify it by a similar title or delete the existing item.
			id, err = p.findFresh(ctx, req.InfoHash)
			if err != nil {
				return nil, err
			}
			if id == "" {
				return nil, fmt.Errorf("torbox: duplicate torrent not listed: %w", core.ErrSourceUnavailable)
			}
		} else {
			if err != nil {
				return nil, err
			}
			var out struct {
				TorrentID json.Number `json:"torrent_id"`
			}
			if err := json.Unmarshal(data, &out); err != nil || out.TorrentID.String() == "" {
				return nil, errors.New("torbox: createtorrent returned no torrent id")
			}
			id, added = out.TorrentID.String(), true
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
	var t *tbTorrent
	var err error
	for polls := 0; polls < p.opts.ReadyPolls; polls++ {
		if t, err = p.torrent(ctx, id); err != nil {
			if !added || !errors.Is(err, ErrGone) {
				return nil, err
			}
		} else if t.DownloadPresent || t.DownloadFinished {
			break
		}
		if err = sleep(ctx, p.opts.PollInterval); err != nil {
			return nil, err
		}
	}
	if t == nil || !(t.DownloadPresent || t.DownloadFinished) {
		if req.CachedOnly {
			cleanup()
		}
		return nil, fmt.Errorf("torbox: torrent is not ready: %w", core.ErrNotCached)
	}
	files := make([]File, 0, len(t.Files))
	for _, f := range t.Files {
		files = append(files, File{ID: f.ID.String(), Path: f.Name, Size: f.Size})
	}
	chosen, err := req.Choose(files)
	if err != nil {
		return nil, err
	}
	p.rememberKnown(req.InfoHash, id)
	return &Acquired{TorrentID: id, File: chosen, Added: added}, nil
}

func (p *torBox) Link(ctx context.Context, h Handle) (DirectLink, error) {
	if h.TorrentID == "" || h.FileID == "" {
		return DirectLink{}, ErrGone
	}
	q := url.Values{"token": {p.key}, "torrent_id": {h.TorrentID}, "file_id": {h.FileID}, "zip_link": {"false"}}
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/requestdl", query: q})
	if err != nil {
		return DirectLink{}, err
	}
	var link string
	if err := json.Unmarshal(data, &link); err != nil || link == "" {
		return DirectLink{}, fmt.Errorf("torbox: requestdl returned no link: %w", ErrGone)
	}
	return DirectLink{URL: link, Size: h.Size, Expires: p.opts.Now().Add(torBoxLinkTTL)}, nil
}

func (p *torBox) Remove(ctx context.Context, torrentID string) error {
	if torrentID == "" {
		return nil
	}
	p.list.forget(torrentID)
	p.knownMu.Lock()
	for hash, id := range p.known {
		if id == torrentID {
			delete(p.known, hash)
		}
	}
	p.knownMu.Unlock()
	n, err := strconv.ParseInt(torrentID, 10, 64)
	var idv any = torrentID
	if err == nil {
		idv = n
	}
	body, _ := json.Marshal(map[string]any{"torrent_id": idv, "operation": "delete"})
	_, err = p.do(ctx, apiRequest{method: http.MethodPost, path: "/torrents/controltorrent", body: bytes.NewReader(body), ctype: "application/json"})
	return err
}

func (p *torBox) Account(ctx context.Context) (Account, error) {
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/user/me"})
	if err != nil {
		return Account{}, err
	}
	var out struct {
		Email   string `json:"email"`
		Plan    int    `json:"plan"`
		Expires string `json:"premium_expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Account{}, errors.New("torbox: unexpected account response")
	}
	a := Account{User: out.Email, Premium: out.Plan > 0}
	a.Expires, _ = time.Parse(time.RFC3339, out.Expires)
	return a, nil
}
