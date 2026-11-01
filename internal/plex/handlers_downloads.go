package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Offline downloads (PMS Download Queue API). Each queue belongs to one
// (user, client); items move deciding -> waiting -> processing ->
// available as the title is resolved with caps.Download set and its
// streams are copied to a temporary faststart MP4 by Deps.Downloads.
// The queue lives in memory only: a restart drops it together with the
// temp files (the remuxer cleans its directory on startup), matching
// Ether's zero-permanent-storage design. PMS persists queues, so a
// client that re-asks after an Ether restart simply creates a new one.

const (
	downloadDeciding   = "deciding"
	downloadWaiting    = "waiting"
	downloadProcessing = "processing"
	downloadAvailable  = "available"
	downloadError      = "error"
	downloadExpired    = "expired"
	downloadDone       = "done" // queue-level only
)

// downloadsEnabled reports whether the server can serve downloads at
// all: owner setting on, remuxer and playback wired.
func (s *Server) downloadsEnabled() bool {
	return s.cfg().Downloads.Enabled && s.d.Downloads != nil && s.d.Playback != nil
}

// downloadsAllowed reports whether u may download (and so sees
// allowSync="1"): the owner always, friends only with the owner's
// AllowFriends setting unless the access policy decides per user.
func (s *Server) downloadsAllowed(u *core.User) bool {
	return experience.DownloadsAllowed(u, s.downloadsEnabled(), s.cfg().Downloads.AllowFriends, s.d.AccessPolicy)
}

// DownloadsAllowed exposes the same admission decision for informational
// frontend capability reporting; the download handler still enforces it.
func (s *Server) DownloadsAllowed(u *core.User) bool { return s.downloadsAllowed(u) }

func (s *Server) requireDownloads(w http.ResponseWriter, r *http.Request) bool {
	if !s.downloadsEnabled() {
		writeHTTPError(w, http.StatusNotFound)
		return false
	}
	if !s.downloadsAllowed(FromContext(r.Context()).User) {
		writeHTTPError(w, http.StatusForbidden)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Store

type downloadItem struct {
	id      int64
	queueID int64
	key     core.RatingKey
	title   string // display title, for the served filename

	caps core.ClientCaps // as requested at add time (Download set at run)
	user *core.User

	status   string
	errText  string
	progress float64 // 0..1
	started  bool
	cancel   context.CancelFunc
	// run numbers each start of the item, so a restarted job gets its own
	// playback session instead of sharing (and later stopping) the
	// previous run's.
	run int64

	path       string
	size       int64
	readyAt    time.Time
	videoCodec string
	audioCodec string
	duration   time.Duration
	bandwidth  int // requested maxVideoBitrate, kbps
}

type downloadQueue struct {
	id       int64
	userID   int64
	clientID string
	items    []*downloadItem
}

type downloadStore struct {
	mu        sync.Mutex
	queues    map[int64]*downloadQueue
	byClient  map[string]int64 // "userID\x00clientID" -> queue id
	nextQueue int64
	nextItem  int64
	nextRun   int64
}

func newDownloadStore() *downloadStore {
	return &downloadStore{queues: map[int64]*downloadQueue{}, byClient: map[string]int64{}}
}

func (st *downloadStore) queueFor(userID int64, clientID string, create bool) *downloadQueue {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := strconv.FormatInt(userID, 10) + "\x00" + clientID
	if id, ok := st.byClient[k]; ok {
		return st.queues[id]
	}
	if !create {
		return nil
	}
	st.nextQueue++
	q := &downloadQueue{id: st.nextQueue, userID: userID, clientID: clientID}
	st.queues[q.id] = q
	st.byClient[k] = q.id
	return q
}

// queueByID returns the queue when it belongs to this user and client.
func (st *downloadStore) queueByID(id, userID int64, clientID string) *downloadQueue {
	st.mu.Lock()
	defer st.mu.Unlock()
	q := st.queues[id]
	if q == nil || q.userID != userID || q.clientID != clientID {
		return nil
	}
	return q
}

func (q *downloadQueue) item(id int64) *downloadItem {
	for _, it := range q.items {
		if it.id == id {
			return it
		}
	}
	return nil
}

func queueStatus(q *downloadQueue) string {
	seen := map[string]bool{}
	for _, it := range q.items {
		seen[it.status] = true
	}
	for _, st := range []string{downloadDeciding, downloadProcessing, downloadWaiting, downloadError} {
		if seen[st] {
			return st
		}
	}
	return downloadDone
}

// ---------------------------------------------------------------------------
// Wire models (components/schemas DownloadQueue, DownloadQueueItem,
// AddedQueueItem and DecisionResult of the PMS OpenAPI spec).

type downloadQueueModel struct {
	ID        int64  `plex:"id"`
	ItemCount int    `plex:"itemCount"`
	Status    string `plex:"status"`
}

type addedQueueItemModel struct {
	ID  int64  `plex:"id"`
	Key string `plex:"key"`
}

type decisionResultModel struct {
	AvailableBandwidth     int    `plex:"availableBandwidth,omitempty"`
	DirectPlayDecisionCode int    `plex:"directPlayDecisionCode"`
	DirectPlayDecisionText string `plex:"directPlayDecisionText"`
	GeneralDecisionCode    int    `plex:"generalDecisionCode"`
	GeneralDecisionText    string `plex:"generalDecisionText"`
	MDEDecisionCode        int    `plex:"mdeDecisionCode"`
	MDEDecisionText        string `plex:"mdeDecisionText"`
	TranscodeDecisionCode  int    `plex:"transcodeDecisionCode"`
	TranscodeDecisionText  string `plex:"transcodeDecisionText"`
}

type downloadQueueItemModel struct {
	ID       int64                `plex:"id"`
	QueueID  int64                `plex:"queueId"`
	Key      string               `plex:"key"`
	Status   string               `plex:"status"`
	Error    string               `plex:"error,omitempty"`
	Decision *decisionResultModel `plex:"DecisionResult,object"`
	Session  *model.TranscodeSess `plex:"TranscodeSession,object"`
}

type downloadContainer struct {
	Size       int    `plex:"size"`
	Identifier string `plex:"identifier,omitempty"`

	Queue []downloadQueueModel     `plex:"DownloadQueue"`
	Items []downloadQueueItemModel `plex:"DownloadQueueItem"`
	Added []addedQueueItemModel    `plex:"AddedQueueItems"`
}

func keyPath(k core.RatingKey) string { return "/library/metadata/" + k.String() }

// itemModel renders one queue item. The download "transcode" is a stream
// copy; the DecisionResult says so honestly (conversion, not direct play).
func itemModel(it *downloadItem) downloadQueueItemModel {
	m := downloadQueueItemModel{
		ID: it.id, QueueID: it.queueID, Key: keyPath(it.key), Status: it.status, Error: it.errText,
	}
	dr := &decisionResultModel{AvailableBandwidth: it.bandwidth}
	text := "Direct play not available; Remux (copy) to MP4 OK."
	switch it.status {
	case downloadError:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeDirectPlayError, it.errText
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeGeneralError, it.errText
		dr.MDEDecisionCode, dr.MDEDecisionText = codeGeneralError, it.errText
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeTranscodeError, it.errText
	case downloadDeciding:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeGeneralError, "Deciding."
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeGeneralError, "Deciding."
		dr.MDEDecisionCode, dr.MDEDecisionText = codeGeneralError, "Deciding."
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeGeneralError, "Deciding."
	default:
		dr.DirectPlayDecisionCode, dr.DirectPlayDecisionText = codeDirectPlayError, text
		dr.GeneralDecisionCode, dr.GeneralDecisionText = codeConversionOK, text
		dr.MDEDecisionCode, dr.MDEDecisionText = codeConversionOK, text
		dr.TranscodeDecisionCode, dr.TranscodeDecisionText = codeConversionOK, text
	}
	m.Decision = dr
	if it.status != downloadDeciding {
		m.Session = &model.TranscodeSess{
			Key:              fmt.Sprintf("/downloadQueue/%d/item/%d", it.queueID, it.id),
			Complete:         it.status == downloadAvailable,
			Progress:         it.progress * 100,
			Duration:         ms(it.duration),
			Context:          "static",
			SourceVideoCodec: it.videoCodec,
			SourceAudioCodec: it.audioCodec,
			VideoDecision:    "copy",
			AudioDecision:    "copy",
			Protocol:         "http",
			Container:        "mp4",
			VideoCodec:       it.videoCodec,
			AudioCodec:       it.audioCodec,
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Handlers

// downloadQueueParam resolves {queue} for the requesting user/client.
func (s *Server) downloadQueueParam(w http.ResponseWriter, r *http.Request) *downloadQueue {
	pr := FromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "queue"), 10, 64)
	if err != nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil
	}
	q := s.downloads.queueByID(id, pr.User.ID, pr.ClientID)
	if q == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil
	}
	return q
}

func (s *Server) writeQueue(w http.ResponseWriter, r *http.Request, q *downloadQueue) {
	s.downloads.mu.Lock()
	m := downloadQueueModel{ID: q.id, ItemCount: len(q.items), Status: queueStatus(q)}
	s.downloads.mu.Unlock()
	s.write(w, r, &downloadContainer{Size: 1, Queue: []downloadQueueModel{m}})
}

// POST /downloadQueue — get or create this client+user's queue.
func (s *Server) handleDownloadQueueCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	pr := FromContext(r.Context())
	q := s.downloads.queueFor(pr.User.ID, pr.ClientID, true)
	s.writeQueue(w, r, q)
}

// GET /downloadQueue/{queue}
func (s *Server) handleDownloadQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	if q := s.downloadQueueParam(w, r); q != nil {
		s.writeQueue(w, r, q)
	}
}

// parseDownloadKey accepts "/library/metadata/123[/children]" or a bare
// rating key.
func parseDownloadKey(s string) (core.RatingKey, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "metadata/"); i >= 0 {
		s = s[i+len("metadata/"):]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return core.ParseRatingKey(s)
}

// syncVideoQualities maps the legacy videoQuality index clients send to
// (max bitrate kbps, max height); the table is PMS's (see python-plexapi
// sync.VIDEO_QUALITIES).
var syncVideoQualities = []struct{ kbps, height int }{
	{64, 128}, {96, 128}, {208, 160}, {320, 240}, {720, 320}, {1500, 480},
	{2000, 720}, {3000, 720}, {4000, 720}, {8000, 1080}, {10000, 1080},
	{12000, 1080}, {20000, 1080},
}

// downloadCaps builds the caps an add request asked for.
func (s *Server) downloadCaps(r *http.Request, pr *Request) core.ClientCaps {
	caps := s.declaredCaps(pr)
	q := r.URL.Query()
	if v := q.Get("videoResolution"); v != "" {
		if _, h, ok := strings.Cut(v, "x"); ok {
			if n, err := strconv.Atoi(h); err == nil && n > 0 {
				caps.MaxHeight = n
			}
		}
	}
	for _, name := range []string{"maxVideoBitrate", "videoBitrate"} {
		if v := q.Get(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.MaxBitrateKbps = n
				break
			}
		}
	}
	if caps.MaxBitrateKbps == 0 && q.Get("videoResolution") == "" {
		if v := q.Get("videoQuality"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(syncVideoQualities) {
				caps.MaxBitrateKbps = syncVideoQualities[n].kbps
				caps.MaxHeight = syncVideoQualities[n].height
			}
		}
	}
	if mh := s.cfg().Downloads.MaxHeight; mh > 0 && (caps.MaxHeight == 0 || caps.MaxHeight > mh) {
		caps.MaxHeight = mh
	}
	return caps
}

// expandDownloadKey turns a show/season key into its episodes; playable
// keys pass through. Returns titles the user may see.
func (s *Server) expandDownloadKey(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	t, err := s.d.Catalog.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if !s.visibleTitle(ctx, u, t) {
		return nil, core.ErrNotFound
	}
	switch t.Type {
	case core.TypeMovie, core.TypeEpisode:
		return []core.Title{*t}, nil
	case core.TypeSeason:
		kids, err := s.d.Catalog.Children(ctx, k)
		if err != nil {
			return nil, err
		}
		return s.visibleTitles(u, kids), nil
	case core.TypeShow:
		eps, err := s.d.Catalog.Grandchildren(ctx, k)
		if err != nil {
			return nil, err
		}
		return s.visibleTitles(u, eps), nil
	}
	return nil, core.ErrNotFound
}

// POST /downloadQueue/{queue}/add?keys=...
func (s *Server) handleDownloadQueueAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	pr := FromContext(r.Context())
	ctx := r.Context()
	raw := r.URL.Query()["keys"]
	if len(raw) == 0 {
		raw = r.URL.Query()["key"]
	}
	var keys []core.RatingKey
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			k, err := parseDownloadKey(part)
			if err != nil {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	caps := s.downloadCaps(r, pr)
	added := []addedQueueItemModel{}
	for _, k := range keys {
		titles, err := s.expandDownloadKey(ctx, pr.User, k)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		for _, t := range titles {
			it := s.addDownloadItem(q, pr.User, t, caps)
			added = append(added, addedQueueItemModel{ID: it.id, Key: keyPath(t.Key)})
		}
	}
	s.scheduleDownloads()
	s.write(w, r, &downloadContainer{Size: len(added), Added: added})
}

// addDownloadItem appends an item, or returns the existing one when the
// same title is already queued with the same parameters (PMS dedupes).
func (s *Server) addDownloadItem(q *downloadQueue, u *core.User, t core.Title, caps core.ClientCaps) *downloadItem {
	s.downloads.mu.Lock()
	defer s.downloads.mu.Unlock()
	for _, it := range q.items {
		if it.key == t.Key && it.caps.MaxHeight == caps.MaxHeight &&
			it.caps.MaxBitrateKbps == caps.MaxBitrateKbps &&
			it.status != downloadError && it.status != downloadExpired {
			return it
		}
	}
	s.downloads.nextItem++
	it := &downloadItem{
		id: s.downloads.nextItem, queueID: q.id, key: t.Key, title: t.Title,
		caps: caps, user: u, status: downloadDeciding, bandwidth: caps.MaxBitrateKbps,
	}
	q.items = append(q.items, it)
	return it
}

// parseItemIDs parses "{item}" path params and ?ids= lists; PMS allows
// comma-separated ids in one segment.
func parseItemIDs(r *http.Request) []int64 {
	raw := chi.URLParam(r, "item")
	if raw == "" {
		raw = r.URL.Query().Get("ids")
	}
	var out []int64
	for _, p := range strings.Split(raw, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) writeDownloadItems(w http.ResponseWriter, r *http.Request, q *downloadQueue, only map[int64]bool) {
	s.downloads.mu.Lock()
	items := []downloadQueueItemModel{}
	for _, it := range q.items {
		if only != nil && !only[it.id] {
			continue
		}
		items = append(items, itemModel(it))
	}
	s.downloads.mu.Unlock()
	s.write(w, r, &downloadContainer{Size: len(items), Items: items})
}

// GET /downloadQueue/{queue}/items
func (s *Server) handleDownloadQueueItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	if q := s.downloadQueueParam(w, r); q != nil {
		s.writeDownloadItems(w, r, q, nil)
	}
}

// GET /downloadQueue/{queue}/items/{item}
func (s *Server) handleDownloadQueueItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	only := map[int64]bool{}
	for _, id := range parseItemIDs(r) {
		only[id] = true
	}
	s.writeDownloadItems(w, r, q, only)
}

// DELETE /downloadQueue/{queue}/items/{item} — cancel and clean up.
func (s *Server) handleDownloadQueueItemDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	ids := parseItemIDs(r)
	if len(ids) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	s.downloads.mu.Lock()
	keep := q.items[:0]
	var removed []*downloadItem
	for _, it := range q.items {
		drop := false
		for _, id := range ids {
			if it.id == id {
				drop = true
			}
		}
		if drop {
			removed = append(removed, it)
		} else {
			keep = append(keep, it)
		}
	}
	q.items = keep
	s.downloads.mu.Unlock()
	for _, it := range removed {
		s.cancelDownloadItem(it)
	}
	s.scheduleDownloads()
	w.WriteHeader(http.StatusOK)
}

// POST /downloadQueue/{queue}/items/{item}/restart — reprocess with the
// original decision parameters.
func (s *Server) handleDownloadQueueItemRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return
	}
	s.downloads.mu.Lock()
	for _, id := range parseItemIDs(r) {
		it := q.item(id)
		if it == nil {
			continue
		}
		if it.cancel != nil {
			it.cancel()
		}
		if it.path != "" {
			s.d.Downloads.ReleaseDownload(it.path)
		}
		it.path, it.size, it.readyAt = "", 0, time.Time{}
		it.errText, it.progress, it.started = "", 0, false
		it.status = downloadDeciding
	}
	s.downloads.mu.Unlock()
	s.scheduleDownloads()
	w.WriteHeader(http.StatusOK)
}

// cancelDownloadItem stops an item's remux (if running) and releases its
// finished file (if any).
func (s *Server) cancelDownloadItem(it *downloadItem) {
	s.downloads.mu.Lock()
	cancel, p := it.cancel, it.path
	it.path = ""
	s.downloads.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if p != "" && s.d.Downloads != nil {
		s.d.Downloads.ReleaseDownload(p)
	}
}

// downloadItemParam resolves queue+item for the single-item endpoints.
func (s *Server) downloadItemParam(w http.ResponseWriter, r *http.Request) (*downloadQueue, *downloadItem) {
	q := s.downloadQueueParam(w, r)
	if q == nil {
		return nil, nil
	}
	ids := parseItemIDs(r)
	if len(ids) != 1 {
		writeHTTPError(w, http.StatusBadRequest)
		return nil, nil
	}
	s.downloads.mu.Lock()
	it := q.item(ids[0])
	s.downloads.mu.Unlock()
	if it == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, nil
	}
	return q, it
}

// GET /downloadQueue/{queue}/item/{item}/decision — the playback-style
// decision for the item's chosen media.
func (s *Server) handleDownloadItemDecision(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	_, it := s.downloadItemParam(w, r)
	if it == nil {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	t, err := s.d.Catalog.Get(ctx, it.key)
	if err == nil && !s.visibleTitle(ctx, pr.User, t) {
		err = core.ErrNotFound // restrictions can change after queueing
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.downloads.mu.Lock()
	m := itemModel(it)
	s.downloads.mu.Unlock()
	mc := s.itemContainer(t.Type)
	md := s.metadata(ctx, pr, t, renderOpts{detail: true, media: true})
	decisionItem(&md)
	mc.Metadata = []model.Metadata{md}
	mc.Size = 1
	dr := m.Decision
	mc.Extra = []model.Attr{
		model.A("directPlayDecisionCode", dr.DirectPlayDecisionCode),
		model.A("directPlayDecisionText", dr.DirectPlayDecisionText),
		model.A("generalDecisionCode", dr.GeneralDecisionCode),
		model.A("generalDecisionText", dr.GeneralDecisionText),
		model.A("transcodeDecisionCode", dr.TranscodeDecisionCode),
		model.A("transcodeDecisionText", dr.TranscodeDecisionText),
		model.A("mdeDecisionCode", dr.MDEDecisionCode),
		model.A("mdeDecisionText", dr.MDEDecisionText),
	}
	s.write(w, r, mc)
}

// GET /downloadQueue/{queue}/item/{item}/media — the remuxed MP4 with
// Range support; 503 + Retry-After while it is still being produced.
func (s *Server) handleDownloadItemMedia(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w, r) {
		return
	}
	s.expireDownloads(s.now())
	_, it := s.downloadItemParam(w, r)
	if it == nil {
		return
	}
	s.downloads.mu.Lock()
	status, p, readyAt, title, key := it.status, it.path, it.readyAt, it.title, it.key
	s.downloads.mu.Unlock()
	// Restrictions can change after an item was queued: serve the cached
	// file only while the viewer may still see the title.
	if t, err := s.d.Catalog.Get(r.Context(), key); err != nil || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	switch status {
	case downloadDeciding, downloadWaiting, downloadProcessing:
		w.Header().Set("Retry-After", "-1")
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	case downloadExpired:
		writeHTTPError(w, http.StatusGone)
		return
	case downloadError:
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeHTTPError(w, http.StatusGone)
		return
	}
	defer f.Close()
	name := downloadFilename(title)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	cw := s.newCountWriter(w, r)
	http.ServeContent(cw, r, name, readyAt, f)
	cw.finish()
}

// downloadFilename is the client-visible file name for a download.
func downloadFilename(title string) string {
	if title == "" {
		title = "download"
	}
	var b strings.Builder
	for _, c := range title {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == ' ', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	return strings.TrimSpace(b.String()) + ".mp4"
}

func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename*=UTF-8''%s; filename=\"%s\"",
		pathEscape(name), strings.ReplaceAll(name, "\"", "_"))
}

func pathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Processing

// scheduleDownloads starts queued items up to the per-user cap. The
// remuxer enforces the server-wide cap.
func (s *Server) scheduleDownloads() {
	perUser := s.cfg().Downloads.MaxConcurrentPerUser
	if perUser <= 0 {
		perUser = 1
	}
	type job struct {
		it  *downloadItem
		ctx context.Context
		run int64
	}
	var jobs []job
	s.downloads.mu.Lock()
	active := map[int64]int{}
	for _, q := range s.downloads.queues {
		for _, it := range q.items {
			if it.started && (it.status == downloadDeciding || it.status == downloadWaiting || it.status == downloadProcessing) {
				active[it.user.ID]++
			}
		}
	}
	for _, q := range s.downloads.queues {
		for _, it := range q.items {
			if it.started || (it.status != downloadDeciding && it.status != downloadWaiting) {
				continue
			}
			if active[it.user.ID] >= perUser {
				it.status = downloadWaiting
				continue
			}
			ctx, cancel := context.WithCancel(s.ctx)
			s.downloads.nextRun++
			it.cancel = cancel
			it.started = true
			it.status = downloadDeciding
			it.run = s.downloads.nextRun
			active[it.user.ID]++
			jobs = append(jobs, job{it, ctx, it.run})
		}
	}
	s.downloads.mu.Unlock()
	for _, j := range jobs {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.processDownloadItem(j.ctx, j.it, j.run)
		}()
	}
}

func (s *Server) processDownloadItem(ctx context.Context, it *downloadItem, run int64) {
	defer s.scheduleDownloads()
	fail := func(msg string) {
		s.downloads.mu.Lock()
		if ctx.Err() == nil {
			it.status = downloadError
			it.errText = msg
		}
		s.downloads.mu.Unlock()
	}

	caps := it.caps
	caps.Download = true
	// A dedicated client identity keeps the download's playback session
	// from colliding with a live stream of the same title on this
	// device; it still counts against stream limits (it holds Usenet
	// connections for the whole file). The run number keeps each job on
	// its own session: Playback reuses a session per (user, client,
	// title), so a restarted job, or a second queued copy of the title at
	// another quality, would otherwise share it and be cut off by the
	// other job's deferred Stop.
	caps.ClientID = it.caps.ClientID + "/download/" + strconv.FormatInt(run, 10)
	ps, err := s.d.Playback.Prepare(ctx, it.user, it.key, caps)
	if err != nil {
		fail(playbackMessage(err))
		return
	}
	defer s.d.Playback.Stop(ps.ID)

	s.downloads.mu.Lock()
	if ctx.Err() != nil {
		// Deleted or restarted while preparing: the item belongs to the
		// next run now.
		s.downloads.mu.Unlock()
		return
	}
	it.status = downloadWaiting
	it.duration = ps.Info.Duration
	if v := ps.Info.Video(); v != nil {
		it.videoCodec = v.Codec
	}
	for _, st := range ps.Info.Streams {
		if st.Type == "audio" {
			it.audioCodec = st.Codec
			break
		}
	}
	s.downloads.mu.Unlock()

	p, size, err := s.d.Downloads.RemuxDownload(ctx, core.DownloadRemuxRequest{
		InputURL:    s.d.Playback.InputURL(ps.ID),
		Info:        ps.Info,
		AudioStream: -1,
	}, func(f float64) {
		s.downloads.mu.Lock()
		if ctx.Err() != nil {
			s.downloads.mu.Unlock()
			return
		}
		it.progress = f
		if it.status == downloadWaiting {
			it.status = downloadProcessing
		}
		s.downloads.mu.Unlock()
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// Deleted, restarted or shutting down; state already reset.
		case errors.Is(err, core.ErrDownloadNeedsTranscode):
			fail("This title cannot be downloaded: its codecs cannot be copied to MP4.")
		case errors.Is(err, core.ErrDownloadBudget):
			fail("Not enough temporary disk space on the server for this download.")
		default:
			s.log.Warn("download remux failed", "key", it.key, "err", err)
			fail(msgPlayFailed)
		}
		return
	}
	s.downloads.mu.Lock()
	if ctx.Err() != nil {
		s.downloads.mu.Unlock()
		s.d.Downloads.ReleaseDownload(p)
		return
	}
	it.path, it.size, it.readyAt = p, size, s.now()
	it.progress = 1
	it.status = downloadAvailable
	s.downloads.mu.Unlock()
}

// expireDownloads frees finished downloads older than the configured
// retention; their items stay listed as expired until deleted.
func (s *Server) expireDownloads(now time.Time) {
	if s.downloads == nil {
		return
	}
	expiry := s.cfg().Downloads.Expiry.D()
	if expiry <= 0 {
		expiry = 24 * time.Hour
	}
	var paths []string
	s.downloads.mu.Lock()
	for _, q := range s.downloads.queues {
		for _, it := range q.items {
			if it.status == downloadAvailable && it.path != "" && now.After(it.readyAt.Add(expiry)) {
				paths = append(paths, it.path)
				it.path, it.size = "", 0
				it.status = downloadExpired
			}
		}
	}
	s.downloads.mu.Unlock()
	if s.d.Downloads != nil {
		for _, p := range paths {
			s.d.Downloads.ReleaseDownload(p)
		}
	}
}

// releaseAllDownloads cancels in-flight remuxes and deletes every temp
// file (server shutdown).
func (s *Server) releaseAllDownloads() {
	if s.downloads == nil {
		return
	}
	var items []*downloadItem
	s.downloads.mu.Lock()
	for _, q := range s.downloads.queues {
		items = append(items, q.items...)
	}
	s.downloads.mu.Unlock()
	for _, it := range items {
		s.cancelDownloadItem(it)
	}
}
