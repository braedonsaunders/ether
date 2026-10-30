package sessions

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// LazyGrants authenticates an issuing surface credential and rechecks the
// non-secret grant reference without extending its lifetime.
type LazyGrants interface {
	GrantReference(context.Context, string, string) (string, *core.Viewer, error)
	ValidateGrantReference(context.Context, string, string, int64) error
}

type lazySession struct {
	id      string
	expires int64
}

type lazyStreams struct {
	db           *sql.DB
	broker       *Broker
	protocol     frontend.Protocol
	grants       LazyGrants
	ttl          time.Duration
	remote       func(*http.Request) bool
	newHeartbeat func(time.Duration) (<-chan time.Time, func())
}

// NewLazyStreams creates a surface-bound durable, reusable capability issuer.
// Capabilities expire after at most one hour of inactivity and are
// revalidated on every request.
func NewLazyStreams(db *sql.DB, b *Broker, protocol frontend.Protocol, grants LazyGrants, ttl time.Duration, remote func(*http.Request) bool) (frontend.LazyStreams, error) {
	if db == nil || b == nil || protocol == "" || grants == nil || remote == nil {
		return nil, errors.New("sessions: missing lazy stream dependency")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > time.Hour {
		return nil, errors.New("sessions: lazy stream lifetime exceeds one hour")
	}
	if ttl < 3*time.Second {
		return nil, errors.New("sessions: lazy stream lifetime below three seconds")
	}
	return &lazyStreams{db: db, broker: b, protocol: protocol, grants: grants, ttl: ttl, remote: remote}, nil
}

func lazyHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *lazyStreams) Issue(ctx context.Context, credential string, req frontend.PlanRequest) (string, error) {
	return s.issue(ctx, credential, req, false)
}

func (s *lazyStreams) issue(ctx context.Context, credential string, req frontend.PlanRequest, stable bool) (string, error) {
	if req.Protocol != s.protocol || req.Device.ID == "" || req.Viewer == nil || req.Intent.Plex != nil || (req.Item == 0) == (req.Channel == "") || (stable && req.Channel != "") {
		return "", core.ErrUnauthorized
	}
	ref, viewer, err := s.grants.GrantReference(ctx, string(s.protocol), credential)
	if err != nil || viewer == nil || viewer.ID != req.Viewer.ID || ref == "" {
		return "", core.ErrUnauthorized
	}
	// Validate policy and item without resolving a release or consuming a stream.
	req.Viewer = viewer
	if stable {
		// Authenticated paths are requested for every range. Check only local
		// policy facts here; resolving a release belongs to the first GET.
		if s.broker.deps.MediaLookup == nil || !s.broker.permitted(viewer, req.Caps) {
			return "", core.ErrUnauthorized
		}
		title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
		if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
			return "", core.ErrUnauthorized
		}
	} else {
		estimate := req
		estimate.Version = "" // Version lookup may resolve releases; defer it until GET.
		estimate.Intent.Estimate = true
		if _, err = s.broker.Plan(ctx, estimate); err != nil {
			return "", err
		}
	}
	req.Viewer = nil
	req.Intent.Estimate = false
	data, err := json.Marshal(req)
	if err != nil || len(data) > 65536 {
		return "", core.ErrNotAvailable
	}
	token := rand.Text()
	if stable {
		// This token never leaves the process: it keys an authenticated path's
		// reusable session and cannot be used as the issuing credential.
		identity, err := json.Marshal(struct {
			Protocol           frontend.Protocol
			ViewerID           int64
			DeviceID           string
			Item               core.RatingKey
			Version            string
			ExpectedSourceETag string
			DetailSource       string
		}{s.protocol, viewer.ID, req.Device.ID, req.Item, req.Version, req.ExpectedSourceETag, req.DetailSource})
		if err != nil {
			return "", core.ErrNotAvailable
		}
		mac := hmac.New(sha256.New, []byte(credential))
		_, _ = mac.Write([]byte("ether-authenticated-lazy-v1\x00"))
		_, _ = mac.Write(identity)
		token = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	now := s.broker.deps.Now()
	expires := now.Add(s.ttl)
	if expiring, ok := s.grants.(interface {
		GrantReferenceExpiry(context.Context, string, string, int64) (time.Time, error)
	}); ok {
		grantExpiry, err := expiring.GrantReferenceExpiry(ctx, string(s.protocol), ref, viewer.ID)
		if err != nil {
			return "", core.ErrUnauthorized
		}
		if !grantExpiry.IsZero() && grantExpiry.Before(expires) {
			expires = grantExpiry
		}
	}
	if !expires.After(now) {
		return "", core.ErrUnauthorized
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE expires<=?`, now.UnixMilli())
	result, err := s.db.ExecContext(ctx, `INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, lazyHash(token), string(s.protocol), viewer.ID, ref, string(data), expires.UnixMilli())
	if err != nil {
		return "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted == 0 {
		if !stable {
			return "", core.ErrNotAvailable
		}
		var oldRaw, oldRef string
		var oldViewer int64
		if err := s.db.QueryRowContext(ctx, `SELECT request,grant_reference,viewer_id FROM sessions_lazy_streams WHERE hash=? AND protocol=? AND expires>?`, lazyHash(token), string(s.protocol), now.UnixMilli()).Scan(&oldRaw, &oldRef, &oldViewer); err != nil {
			return "", core.ErrNotAvailable
		}
		var previous frontend.PlanRequest
		if oldRef != ref || oldViewer != viewer.ID || json.Unmarshal([]byte(oldRaw), &previous) != nil || !sameLazyDecision(previous, req) {
			return "", core.ErrNotAvailable
		}
		result, err = s.db.ExecContext(ctx, `UPDATE sessions_lazy_streams SET expires=? WHERE hash=? AND protocol=? AND request=? AND expires>?`, expires.UnixMilli(), lazyHash(token), string(s.protocol), oldRaw, now.UnixMilli())
		if err != nil {
			return "", err
		}
		if updated, err := result.RowsAffected(); err != nil || updated != 1 {
			return "", core.ErrNotAvailable
		}
	}
	if stable {
		s.broker.mu.Lock()
		if entry, ok := s.broker.lazySessions[lazyHash(token)]; ok {
			entry.expires = expires.UnixMilli()
			s.broker.lazySessions[lazyHash(token)] = entry
		}
		s.broker.mu.Unlock()
	}
	if req.Channel != "" {
		return "/sessions/lazy/" + token + "/live/index.m3u8", nil
	}
	return "/sessions/lazy/" + token + "/file", nil
}

// sameLazyDecision ignores display-only fields and the current network flag.
// Current locality is re-derived on every request before the broker uses it.
func sameLazyDecision(a, b frontend.PlanRequest) bool {
	normalize := func(r frontend.PlanRequest) frontend.PlanRequest {
		r.Device.Client = ""
		r.Device.DeviceName = ""
		r.Caps.DeviceName = ""
		r.Caps.ClientID = ""
		r.Caps.Remote = false
		return r
	}
	x, errX := json.Marshal(normalize(a))
	y, errY := json.Marshal(normalize(b))
	return errX == nil && errY == nil && string(x) == string(y)
}

// ServeAuthenticated is for a surface such as WebDAV that authenticates every
// GET and has a stable logical path rather than an issued capability URL.
func (s *lazyStreams) ServeAuthenticated(w http.ResponseWriter, r *http.Request, credential string, req frontend.PlanRequest) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	path, err := s.issue(r.Context(), credential, req, true)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Path = path
	u.RawPath = ""
	clone.URL = &u
	s.ServeHTTP(w, clone)
}

func (s *lazyStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/sessions/lazy/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, tail, ok := strings.Cut(rest, "/")
	if !ok || token == "" || len(tail) > 1024 || !validLazyTail(tail) {
		http.NotFound(w, r)
		return
	}
	hash := lazyHash(token)
	ctx := r.Context()
	var viewerID, expires int64
	var reference, raw string
	err := s.db.QueryRowContext(ctx, `SELECT viewer_id,grant_reference,request,expires FROM sessions_lazy_streams WHERE hash=? AND protocol=?`, hash, string(s.protocol)).Scan(&viewerID, &reference, &raw, &expires)
	if err != nil || expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.broker.deps.ActiveViewer(ctx, viewerID)
	if err != nil || viewer == nil {
		http.NotFound(w, r)
		return
	}
	var req frontend.PlanRequest
	if json.Unmarshal([]byte(raw), &req) != nil || req.Protocol != s.protocol || req.Device.ID == "" {
		http.NotFound(w, r)
		return
	}
	req.Viewer = viewer
	req.Caps.Remote = req.Caps.Remote || s.remote(r)
	if !s.broker.permitted(viewer, req.Caps) || (req.Item == 0) == (req.Channel == "") {
		http.NotFound(w, r)
		return
	}
	if req.Channel != "" {
		if !strings.HasPrefix(tail, "live/") || s.broker.deps.Live == nil || s.broker.deps.Live.Allowed(ctx, viewer, req.Channel) != nil {
			http.NotFound(w, r)
			return
		}
	} else {
		// Missing cache facts fail closed rather than causing network lookups on
		// every range request.
		if tail != "file" || s.broker.deps.MediaLookup == nil {
			http.NotFound(w, r)
			return
		}
		title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
		if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
			http.NotFound(w, r)
			return
		}
	}
	if r.Method == http.MethodHead {
		if expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
			http.NotFound(w, r)
			return
		}
		if req.Channel != "" && tail != "live/index.m3u8" {
			s.broker.mu.Lock()
			id := s.broker.lazySessions[hash].id
			s.broker.mu.Unlock()
			access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
			if existing, _, err := s.broker.access(ctx, access, id); id == "" || err != nil || (req.Caps.Remote && !existing.caps.Remote) {
				http.NotFound(w, r)
				return
			}
		} else if req.Item != 0 {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}
	unlock, err := s.broker.gates.Acquire(ctx, "lazy/"+hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { unlock() }()
	if s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil {
		http.NotFound(w, r)
		return
	}
	s.broker.mu.Lock()
	for key, entry := range s.broker.lazySessions {
		if entry.expires <= s.broker.deps.Now().UnixMilli() {
			delete(s.broker.lazySessions, key)
		}
	}
	id := s.broker.lazySessions[hash].id
	s.broker.mu.Unlock()
	access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
	var existingLive *brokerSession
	if id != "" {
		if existing, _, err := s.broker.access(ctx, access, id); err != nil {
			id = ""
		} else if req.Caps.Remote && !existing.caps.Remote {
			if err := s.broker.Stop(ctx, access, id); err != nil {
				http.NotFound(w, r)
				return
			}
			id = ""
		} else if req.Channel != "" && existing.live && existing.public.Channel == req.Channel {
			existingLive = existing
		} else if req.Channel != "" {
			id = ""
		}
	}
	if req.Channel != "" && tail != "live/index.m3u8" && id == "" {
		http.NotFound(w, r)
		return
	}
	// Reserve before opening a source. Stuck dependencies retain a slot, so
	// they cannot create an unbounded number of heartbeat goroutines.
	if !s.broker.reserveLazyHeartbeat() {
		http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
		return
	}
	heartbeatOwned := true
	defer func() {
		if heartbeatOwned {
			s.broker.releaseLazyHeartbeat()
		}
	}()
	opened := false
	reserved := false
	defer func() {
		if reserved {
			s.broker.releaseLazyCleanup()
		}
	}()
	if id == "" {
		if !s.broker.reserveLazyCleanup() {
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		reserved = true
		plan, err := s.broker.Plan(ctx, req)
		if err != nil {
			if errors.Is(err, frontend.ErrSourceGone) {
				s.sourceGone(w, r)
				return
			}
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		session, err := s.broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: req.Device, Protocol: s.protocol, PlanID: plan.ID})
		if err != nil {
			if errors.Is(err, frontend.ErrSourceGone) {
				s.sourceGone(w, r)
				return
			}
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		id = session.ID
		opened = true
		s.broker.mu.Lock()
		if s.broker.lazySessions == nil {
			s.broker.lazySessions = make(map[string]lazySession)
		}
		s.broker.lazySessions[hash] = lazySession{id: id, expires: expires}
		s.broker.mu.Unlock()
	}
	format := frontend.StreamDirect
	if req.Channel != "" {
		format = frontend.StreamHLS
	}
	var path string
	if existingLive != nil {
		// Live sessions are already started; the scoped media handler below
		// revalidates current channel policy again before serving each resource.
		path = "/sessions/media/" + id + "/" + existingLive.grant + "/live/index.m3u8"
	} else {
		path, err = s.broker.StartStream(ctx, access, id, frontend.StreamRequest{Format: format})
	}
	if err != nil {
		if opened {
			if s.discardOpened(ctx, access, id) {
				reserved = false // retry worker owns the reservation
			}
			s.broker.mu.Lock()
			delete(s.broker.lazySessions, hash)
			s.broker.mu.Unlock()
		}
		if errors.Is(err, frontend.ErrSourceGone) {
			s.sourceGone(w, r)
			return
		}
		http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
		return
	}
	if expires <= s.broker.deps.Now().UnixMilli() || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil || s.renewActive(ctx, hash, reference, viewerID, id) != nil {
		if opened {
			if s.discardOpened(ctx, access, id) {
				reserved = false // retry worker owns the reservation
			}
			s.broker.mu.Lock()
			delete(s.broker.lazySessions, hash)
			s.broker.mu.Unlock()
		}
		http.NotFound(w, r)
		return
	}
	if reserved {
		s.broker.releaseLazyCleanup()
		reserved = false
	}
	// The scoped path remains internal; every client request must pass this
	// handler so revoking the issuing grant revokes the capability as well.
	streamCtx, cancelStream := context.WithCancel(ctx)
	unregister, ok := s.broker.registerLazyTransfer(id, cancelStream)
	if !ok {
		cancelStream()
		http.NotFound(w, r)
		return
	}
	defer unregister()
	done := make(chan struct{})
	heartbeatOwned = false
	go func() {
		defer close(done)
		defer s.broker.releaseLazyHeartbeat()
		s.heartbeat(streamCtx, cancelStream, hash, reference, viewerID, id, req)
	}()
	defer func() {
		cancelStream()
		// Real dependencies honor context cancellation. A broken dependency
		// must not hold HTTP teardown forever; its slot remains reserved until
		// its worker actually exits.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}()
	clone := r.Clone(streamCtx)
	u := *r.URL
	u.Path = path
	if req.Channel != "" {
		u.Path = strings.TrimSuffix(path, "index.m3u8") + strings.TrimPrefix(tail, "live/")
	}
	u.RawPath = ""
	clone.URL = &u
	unlock()
	unlock = func() {}
	if req.Channel != "" {
		depth := len(strings.Split(strings.TrimPrefix(tail, "live/"), "/")) - 1
		base := "./"
		if depth > 0 {
			base = strings.Repeat("../", depth)
		}
		s.broker.serveMediaAt(s.protocol, nil, w, clone, base)
	} else {
		s.broker.ServeMediaFor(s.protocol, w, clone)
	}
}

func (s *lazyStreams) sourceGone(w http.ResponseWriter, r *http.Request) {
	serveSourceGone(s.protocol, w, r)
}
func serveSourceGone(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
	if protocol == "jellyfin" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "Error processing request.")
		}
		return
	}
	http.NotFound(w, r)
}

const maxLazyHeartbeats = 1024

type lazyTransfer struct{ marker byte }

func (b *Broker) registerLazyTransfer(id string, cancel context.CancelFunc) (func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.sessions[id] == nil {
		return nil, false
	}
	if b.lazyTransfers == nil {
		b.lazyTransfers = make(map[string]map[*lazyTransfer]context.CancelFunc)
	}
	if b.lazyTransfers[id] == nil {
		b.lazyTransfers[id] = make(map[*lazyTransfer]context.CancelFunc)
	}
	key := &lazyTransfer{}
	b.lazyTransfers[id][key] = cancel
	return func() {
		b.mu.Lock()
		delete(b.lazyTransfers[id], key)
		if len(b.lazyTransfers[id]) == 0 {
			delete(b.lazyTransfers, id)
		}
		b.mu.Unlock()
	}, true
}

func (b *Broker) reserveLazyHeartbeat() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.lazyHeartbeatCount >= maxLazyHeartbeats {
		return false
	}
	b.lazyHeartbeatCount++
	return true
}

func (b *Broker) releaseLazyHeartbeat() {
	b.mu.Lock()
	b.lazyHeartbeatCount--
	b.mu.Unlock()
}

// heartbeat rechecks authority while a response is still being transferred.
// Revocation during a transfer is detected at the next tick, at most one
// minute later. New requests still check authorization immediately.
func (s *lazyStreams) heartbeat(ctx context.Context, cancel context.CancelFunc, hash, reference string, viewerID int64, id string, req frontend.PlanRequest) {
	interval := min(time.Minute, s.ttl/3)
	var ticks <-chan time.Time
	var stop func()
	if s.newHeartbeat != nil {
		ticks, stop = s.newHeartbeat(interval)
	} else {
		ticker := time.NewTicker(interval)
		ticks, stop = ticker.C, ticker.Stop
	}
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			viewer, err := s.broker.deps.ActiveViewer(ctx, viewerID)
			if err != nil || viewer == nil || s.grants.ValidateGrantReference(ctx, string(s.protocol), reference, viewerID) != nil || !s.broker.permitted(viewer, req.Caps) {
				cancel()
				return
			}
			access := frontend.SessionAccess{Viewer: viewer, Device: req.Device, Protocol: s.protocol}
			if _, _, err = s.broker.access(ctx, access, id); err != nil {
				cancel()
				return
			}
			if req.Channel != "" {
				if s.broker.deps.Live == nil || s.broker.deps.Live.Allowed(ctx, viewer, req.Channel) != nil {
					cancel()
					return
				}
			} else {
				if s.broker.deps.MediaLookup == nil {
					cancel()
					return
				}
				title, err := s.broker.deps.MediaLookup(ctx, viewer, req.Device, req.Item)
				if err != nil || title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
					cancel()
					return
				}
			}
			if s.renewActive(ctx, hash, reference, viewerID, id) != nil {
				cancel()
				return
			}
		}
	}
}

// renewActive is called only after a GET has established or found a session
// and passed grant, viewer and current media policy checks. HEAD probes never
// keep an abandoned capability alive. The issuing grant is never extended.
func (s *lazyStreams) renewActive(ctx context.Context, hash, reference string, viewerID int64, sessionID string) error {
	now := s.broker.deps.Now()
	until := now.Add(s.ttl)
	if expiring, ok := s.grants.(interface {
		GrantReferenceExpiry(context.Context, string, string, int64) (time.Time, error)
	}); ok {
		grantExpiry, err := expiring.GrantReferenceExpiry(ctx, string(s.protocol), reference, viewerID)
		if err != nil {
			return err
		}
		if !grantExpiry.IsZero() && grantExpiry.Before(until) {
			until = grantExpiry
		}
	}
	if !until.After(now) {
		return core.ErrUnauthorized
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sessions_lazy_streams SET expires=? WHERE hash=? AND protocol=? AND viewer_id=? AND grant_reference=? AND expires>?`, until.UnixMilli(), hash, string(s.protocol), viewerID, reference, now.UnixMilli())
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return core.ErrUnauthorized
	}
	s.broker.mu.Lock()
	if entry, ok := s.broker.lazySessions[hash]; ok && entry.id == sessionID {
		entry.expires = until.UnixMilli()
		s.broker.lazySessions[hash] = entry
	}
	s.broker.mu.Unlock()
	return nil
}

func validLazyTail(tail string) bool {
	if tail == "file" {
		return true
	}
	if !strings.HasPrefix(tail, "live/") {
		return false
	}
	for _, part := range strings.Split(tail, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return false
		}
	}
	return true
}

// discardOpened rolls back a session that this request just opened, even when
// its viewer is no longer active. It only operates on the matching local
// session and takes the same ownership gate as normal stop operations.
func (s *lazyStreams) discardOpened(ctx context.Context, access frontend.SessionAccess, id string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.broker.lazyCleanupWait)
	defer cancel()
	s.broker.mu.Lock()
	entry := s.broker.sessions[id]
	if entry == nil || access.Viewer == nil || entry.public.ViewerID != access.Viewer.ID || entry.public.Protocol != access.Protocol || entry.public.Device.ID != access.Device.ID {
		s.broker.mu.Unlock()
		return false
	}
	cleanup := lazyCleanupEntry{session: entry, viewerID: access.Viewer.ID, deviceID: access.Device.ID, protocol: access.Protocol, clientID: entry.clientID}
	s.broker.mu.Unlock()
	if s.broker.attemptLazyCleanup(ctx, cleanup) {
		return false
	}
	return s.broker.queueLazyCleanup(id, cleanup)
}
