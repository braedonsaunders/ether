package debrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
)

// Real-Debrid REST API v1 (https://api.real-debrid.com). Bearer token auth.
// Real-Debrid removed its instant-availability endpoint in late 2024, so a
// torrent counts as cached when it reaches "downloaded" within a short poll
// window after its file is selected.
//
// Endpoints used:
//
//	POST   /torrents/addMagnet          form magnet=      -> {id, uri}
//	GET    /torrents/info/{id}                            -> {status, files[{id,path,bytes,selected}], links[]}
//	POST   /torrents/selectFiles/{id}   form files=1,2    -> 204
//	DELETE /torrents/delete/{id}                          -> 204
//	POST   /unrestrict/link             form link=        -> {download, filesize, filename}
//	GET    /user                                          -> {username, type, expiration}
const (
	realDebridBase    = "https://api.real-debrid.com/rest/1.0"
	realDebridLinkTTL = 12 * time.Hour
)

type realDebrid struct {
	pace *accountPacer
	key  string
	opts ProviderOptions
	list listCache
}

func newRealDebrid(key string, opts ProviderOptions) *realDebrid {
	return &realDebrid{pace: opts.paces.forAccount("realdebrid", key, opts.Now), key: key, opts: opts.withDefaults(realDebridBase)}
}

func (p *realDebrid) accountPace() *accountPacer { return p.pace }

func (p *realDebrid) Kind() string { return "realdebrid" }

type rdError struct {
	Error     string `json:"error"`
	ErrorCode int    `json:"error_code"`
}

type rdFile struct {
	ID       int    `json:"id"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Selected int    `json:"selected"`
}

type rdInfo struct {
	ID     string   `json:"id"`
	Hash   string   `json:"hash"`
	Status string   `json:"status"`
	Files  []rdFile `json:"files"`
	Links  []string `json:"links"`
}

func (p *realDebrid) do(ctx context.Context, r apiRequest, out any) error {
	_, err := pacedCall(ctx, p.pace, r, r.path == "/torrents/addMagnet", func(ctx context.Context, r apiRequest) (struct{}, error) {
		return struct{}{}, p.request(ctx, r, out)
	})
	return err
}

func (p *realDebrid) request(ctx context.Context, r apiRequest, out any) error {
	status, body, err := call(ctx, p.opts.HTTPClient, p.opts.BaseURL, r, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+p.key)
	})
	if err != nil {
		if status != 0 {
			logProviderHTTPError(ctx, p.key, status, body)
		}
		return err
	}
	if status >= 200 && status < 300 {
		if out != nil && len(body) > 0 {
			if err := decodeJSON("real-debrid", body, out); err != nil {
				return fmt.Errorf("%w: %w", ErrTransport, err)
			}
		}
		return nil
	}
	var e rdError
	_ = decodeJSON("real-debrid", body, &e)
	code := ""
	if e.ErrorCode != 0 {
		code = strconv.Itoa(e.ErrorCode)
	}
	logProviderResponse(ctx, p.key, status, code, e.Error)
	err = p.classify(status, e)
	if status == http.StatusUnavailableForLegalReasons || errors.Is(err, ErrBlocked) {
		if strings.HasPrefix(r.path, "/unrestrict/") || strings.HasPrefix(r.path, "/torrents/selectFiles/") {
			return ErrBlockedFile
		}
		return ErrBlockedHash
	}
	return err
}

// classify maps Real-Debrid error codes (documented on the API page).
func (p *realDebrid) classify(status int, e rdError) error {
	msg := cleanDiagnostic(e.Error, p.key)
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch e.ErrorCode {
	// Codes from the Real-Debrid API documentation.
	case 8, 9, 10, 11, 12, 13, 14, 15, 20, 22, 23, 36:
		// Bad token, permission denied, two-factor, invalid login, account
		// locked or not activated, premium only, IP not allowed, traffic
		// exhausted, fair-usage limit: the account, not the release.
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrAuth)
	case 21: // too many active downloads: no new torrent until one finishes
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrDownloadLimit)
	case 5, 18, 34: // slow down, hoster limit, too many requests
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrRateLimited)
	case 7, 24: // resource not found, file unavailable
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrGone)
	case 28, 35: // file not allowed or infringing
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrBlocked)
	case 26, 29: // upload or torrent too big
		return fmt.Errorf("real-debrid: %s: %w", msg, ErrTooLarge)
	case 30: // torrent file invalid
		return fmt.Errorf("real-debrid: %s: %w", msg, core.ErrReleaseRejected)
	case 33:
		return errDuplicateTorrent
	case 1, 2, 3, 4, 16, 31, 32, 37:
		return fmt.Errorf("real-debrid: API refused the request (code %d): %w", e.ErrorCode, ErrTransport)
	case -1, 6, 17, 19, 25, 27:
		return fmt.Errorf("real-debrid: API temporarily unavailable (code %d): %w", e.ErrorCode, ErrTransport)
	}
	return statusError("real-debrid", status, msg)
}

func (p *realDebrid) info(ctx context.Context, id string) (*rdInfo, error) {
	var info rdInfo
	if err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/info/" + url.PathEscape(id)}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (p *realDebrid) CheckCached(context.Context, []string) (map[string]bool, error) {
	return nil, ErrUnsupported
}

type rdListItem struct {
	ID     string `json:"id"`
	Hash   string `json:"hash"`
	Status string `json:"status"`
}

func (p *realDebrid) listTorrents(ctx context.Context) ([]rdListItem, error) {
	var items []rdListItem
	err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents", query: url.Values{"limit": {"100"}}}, &items)
	return items, err
}

// Find looks the hash up in the account's most recent torrents.
func (p *realDebrid) Find(ctx context.Context, hash string) (string, error) {
	if lowPriority(ctx) {
		return p.list.known(p.opts.Now(), hash), nil
	}
	return p.list.find(ctx, p.opts.Now(), hash, func(ctx context.Context) (map[string]string, error) {
		items, err := p.listTorrents(ctx)
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		for _, it := range items {
			if it.Status == "downloaded" && it.ID != "" && m[strings.ToLower(it.Hash)] == "" {
				m[strings.ToLower(it.Hash)] = it.ID
			}
		}
		for _, it := range items {
			switch it.Status {
			case "magnet_conversion", "waiting_files_selection", "queued", "downloading", "compressing", "uploading":
				if hash := strings.ToLower(it.Hash); it.ID != "" && m[hash] == "" {
					m[hash] = it.ID
				}
			}
		}
		return m, nil
	})
}

var errRDAddUnconfirmed = fmt.Errorf("real-debrid: torrent add could not be confirmed: %w", core.ErrSourceUnavailable)

// reconcileAdd bypasses the list snapshot and considers pending torrents as
// well as downloaded ones; both can be the result of a committed POST.
func (p *realDebrid) reconcileAdd(ctx context.Context, hash string) (string, error) {
	items, err := p.listTorrents(ctx)
	if err != nil {
		return "", err
	}
	for _, it := range items {
		if it.ID != "" && strings.EqualFold(it.Hash, hash) {
			p.pace.mu.Lock()
			p.pace.api.ListHits++
			p.pace.mu.Unlock()
			p.pace.writeAnswer(ctx, answerKey{hash: hash}, learnedAnswer{verdict: answerPending, torrentID: it.ID}, p.opts.Now())
			return it.ID, nil
		}
	}
	return "", errRDAddUnconfirmed
}

func rdFiles(info *rdInfo) []File {
	out := make([]File, 0, len(info.Files))
	for _, f := range info.Files {
		out = append(out, File{ID: strconv.Itoa(f.ID), Path: f.Path, Size: f.Bytes, selected: f.Selected == 1})
	}
	return out
}

// rdMediaClass returns the pack media class of p ("video", "audio", "book",
// or "comic"), or "" for sidecars, artwork, metadata, and generic archives.
func rdMediaClass(p string) string {
	if isVideo(p) {
		return "video"
	}
	switch fam := nzb.FileFamily(p); fam {
	case "audio", "book", "comic":
		return fam
	default:
		return ""
	}
}

// rdSelection lists the files to select: the chosen one and every other
// sibling file of the same media class in the torrent. Real-Debrid serves a
// torrent instantly only for a file set it already holds, and Torrentio, DMM
// and other debrid apps select every file of a pack, so a cached season pack
// (#262), album, ebook pack, or comic pack is held as the whole pack and
// asking for one file of it starts a new download.
func rdSelection(files []File, chosen File) string {
	ids := []string{chosen.ID}
	class := rdMediaClass(chosen.Path)
	if class == "" {
		return chosen.ID
	}
	for _, f := range files {
		if f.ID == chosen.ID || f.Size <= 0 {
			continue
		}
		if class == "video" {
			if isVideo(f.Path) && !isExtra(f.Path) {
				ids = append(ids, f.ID)
			}
		} else if rdMediaClass(f.Path) == class {
			ids = append(ids, f.ID)
		}
	}
	return strings.Join(ids, ",")
}

// linkFor maps a selected file to its hoster link: links[] holds one entry
// per selected file, in file order.
func rdLinkFor(info *rdInfo, fileID string) string {
	i := 0
	for _, f := range info.Files {
		if f.Selected != 1 {
			continue
		}
		if strconv.Itoa(f.ID) == fileID {
			if i < len(info.Links) {
				return info.Links[i]
			}
			return ""
		}
		i++
	}
	return ""
}

func (p *realDebrid) Acquire(ctx context.Context, req AcquireRequest) (*Acquired, error) {
	return p.pace.acquireRemembered(ctx, req, p.opts.Now, p.acquire)
}

func (p *realDebrid) acquire(ctx context.Context, req AcquireRequest) (_ *Acquired, err error) {
	if req.Private && !req.privateChecked {
		var perr error
		req, perr = prepareAcquire(ctx, p, req, false)
		if perr != nil {
			return nil, perr
		}
	}
	id, added := req.TorrentID, false
	key := answerKey{hash: req.InfoHash}
	if id == "" {
		if previous := p.pace.answer(ctx, key, p.opts.Now()); previous.needsReconciliation() {
			p.pace.noteRemembered()
			if previous.verdict == answerReconcileWait && p.opts.Now().Before(previous.expires.Add(-answerHoldTTL+listTTL)) {
				return nil, errRDAddUnconfirmed
			}
			p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerReconcileWait}, p.opts.Now())
			id, err = p.reconcileAdd(ctx, req.InfoHash)
			if err != nil {
				return nil, err
			}
		} else {
			var out struct {
				ID string `json:"id"`
			}
			form := url.Values{"magnet": {req.Magnet}}
			err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/torrents/addMagnet", form: form}, &out)
			switch {
			case err == nil && out.ID != "":
				id, added = out.ID, true
			case err == nil, errors.Is(err, ErrTransport), errors.Is(err, errDuplicateTorrent):
				p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerReconcile}, p.opts.Now())
				if ctx.Err() != nil {
					return nil, errRDAddUnconfirmed
				}
				id, err = p.reconcileAdd(ctx, req.InfoHash)
				if err != nil {
					p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerReconcileWait}, p.opts.Now())
					return nil, err
				}
			default:
				return nil, err
			}
		}
	}
	defer func() {
		if err == nil {
			p.list.remember(req.InfoHash, id)
		}
	}()
	removed := false
	cleanup := func() bool {
		if added && req.RemoveUncached && ctx.Err() == nil && req.mayRemove(ctx, id) {
			removed = p.Remove(ctx, id) == nil
		}
		return removed
	}
	defer func() {
		if err != nil && added && !removed && pendingTorrent(err) == "" && !errors.Is(err, ErrBlockedHash) && !errors.Is(err, ErrGone) {
			p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerPending, torrentID: id}, p.opts.Now())
		}
	}()
	info, err := p.info(ctx, id)
	for polls := 0; err == nil && info.Status == "magnet_conversion" && polls < p.opts.ReadyPolls*2; polls++ {
		if err = sleep(ctx, p.opts.PollInterval); err == nil {
			info, err = p.info(ctx, id)
		}
	}
	if err != nil {
		if added && !errors.Is(err, context.Canceled) {
			cleanup()
		}
		return nil, err
	}
	if info.Hash != "" && req.InfoHash != "" && !strings.EqualFold(info.Hash, req.InfoHash) {
		return nil, fmt.Errorf("real-debrid: account torrent hash changed: %w", ErrGone)
	}
	if req.Private && info.Status != "downloaded" {
		return nil, core.ErrPrivateUncached
	}
	var chosen File
	switch info.Status {
	case "waiting_files_selection":
		if chosen, err = req.Choose(rdFiles(info)); err != nil {
			cleanup()
			return nil, err
		}
		selection := rdSelection(rdFiles(info), chosen)
		selectFiles := func(ids string) error {
			return p.do(ctx, apiRequest{method: http.MethodPost, path: "/torrents/selectFiles/" + url.PathEscape(id), form: url.Values{"files": {ids}}}, nil)
		}
		err = selectFiles(selection)
		if errors.Is(err, ErrBlockedFile) && selection != chosen.ID {
			// A denied pack selection cannot identify which sibling was
			// denied. Try only the requested file on the same account item.
			err = selectFiles(chosen.ID)
		}
		if err != nil {
			if !errors.Is(err, ErrBlockedFile) {
				cleanup()
			}
			return nil, err
		}
		for polls := 0; polls < p.opts.ReadyPolls; polls++ {
			if info, err = p.info(ctx, id); err != nil {
				cleanup()
				return nil, err
			}
			if info.Status != "magnet_conversion" && info.Status != "waiting_files_selection" && info.Status != "queued" {
				break
			}
			if err = sleep(ctx, p.opts.PollInterval); err != nil {
				return nil, err
			}
		}
	}
	switch info.Status {
	case "downloaded":
	case "magnet_error", "error", "virus", "dead":
		logProviderResponse(ctx, p.key, http.StatusOK, info.Status, "torrent ended in "+info.Status)
		cleanup()
		if !added {
			// A previously remembered torrent died; mark ErrGone so the stale
			// pending answer is cleared and a subsequent attempt can re-add.
			return nil, fmt.Errorf("real-debrid: torrent %s: %w: %w", info.Status, ErrGone, core.ErrNotCached)
		}
		return nil, fmt.Errorf("real-debrid: torrent %s: %w", info.Status, core.ErrNotCached)
	default: // queued, downloading, compressing, uploading: not cached
		err := fmt.Errorf("real-debrid: torrent is %s: %w", info.Status, core.ErrNotCached)
		if req.CachedOnly && cleanup() {
			return nil, err
		}
		return nil, pending(id, err)
	}
	if info.Hash != "" && req.InfoHash != "" && !strings.EqualFold(info.Hash, req.InfoHash) {
		return nil, fmt.Errorf("real-debrid: account torrent hash changed after selection: %w", ErrGone)
	}
	files := rdFiles(info)
	selectedCount := 0
	for _, f := range files {
		if f.selected {
			selectedCount++
		}
	}
	if selectedCount == 0 || selectedCount != len(info.Links) {
		if !added {
			p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerPending, torrentID: id}, p.opts.Now())
		}
		return nil, fmt.Errorf("real-debrid: invalid selected-file links: %w", ErrTransport)
	}
	current := answerFiles(files)
	if len(req.rememberedFiles) != 0 && !slices.Equal(req.rememberedFiles, current) {
		p.pace.writeAnswer(ctx, key, learnedAnswer{verdict: answerCached, torrentID: id, files: current}, p.opts.Now())
	}
	if chosen.ID == "" {
		if chosen, err = req.Choose(files); err != nil {
			return nil, err
		}
	}
	if req.selectedFiles != nil {
		req.selectedFiles(files)
	}
	// Check the file identity again after selection. A changed ID or size
	// must never send a link belonging to a different file.
	if !slices.ContainsFunc(files, func(f File) bool {
		return f.selected && f.ID == chosen.ID && f.Path == chosen.Path && f.Size == chosen.Size
	}) {
		return nil, errSelectionMismatch
	}
	chosen.Link = rdLinkFor(info, chosen.ID)
	if chosen.Link == "" {
		return nil, fmt.Errorf("real-debrid: missing selected-file link: %w", ErrTransport)
	}
	return &Acquired{TorrentID: id, File: chosen, Added: added}, nil
}

func (p *realDebrid) Link(ctx context.Context, h Handle) (DirectLink, error) {
	return p.pace.linkRemembered(ctx, h, p.opts.Now, p.link)
}

func (p *realDebrid) link(ctx context.Context, h Handle) (DirectLink, error) {
	link := h.Link
	if link == "" {
		if h.TorrentID == "" {
			return DirectLink{}, ErrGone
		}
		info, err := p.info(ctx, h.TorrentID)
		if err != nil {
			return DirectLink{}, err
		}
		if info.Status != "downloaded" {
			return DirectLink{}, fmt.Errorf("real-debrid: torrent is %s: %w", info.Status, ErrGone)
		}
		if link = rdLinkFor(info, h.FileID); link == "" {
			return DirectLink{}, ErrGone
		}
	}
	var out struct {
		Download string `json:"download"`
		Filesize int64  `json:"filesize"`
	}
	if err := p.do(ctx, apiRequest{method: http.MethodPost, path: "/unrestrict/link", form: url.Values{"link": {link}}}, &out); err != nil {
		return DirectLink{}, err
	}
	if out.Download == "" {
		return DirectLink{}, fmt.Errorf("real-debrid: unrestrict returned no link: %w", ErrGone)
	}
	return DirectLink{URL: out.Download, Size: out.Filesize, Expires: p.opts.Now().Add(realDebridLinkTTL)}, nil
}

func (p *realDebrid) Remove(ctx context.Context, torrentID string) error {
	if torrentID == "" {
		return nil
	}
	p.list.forget(torrentID)
	return p.do(ctx, apiRequest{method: http.MethodDelete, path: "/torrents/delete/" + url.PathEscape(torrentID)}, nil)
}

func (p *realDebrid) Account(ctx context.Context) (Account, error) {
	ctx = core.WithIndexerPurpose(ctx, core.IndexerPurposeTest)
	var out struct {
		Username   string `json:"username"`
		Type       string `json:"type"`
		Expiration string `json:"expiration"`
	}
	if err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/user"}, &out); err != nil {
		return Account{}, err
	}
	a := Account{User: out.Username, Premium: out.Type == "premium"}
	a.Expires, _ = time.Parse(time.RFC3339, out.Expiration)
	return a, nil
}
