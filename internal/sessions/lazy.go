package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	db       *sql.DB
	broker   *Broker
	protocol frontend.Protocol
	grants   LazyGrants
	ttl      time.Duration
}

// NewLazyStreams creates a surface-bound durable, reusable capability issuer.
// Capabilities live at most one hour and are revalidated on every request.
func NewLazyStreams(db *sql.DB, b *Broker, protocol frontend.Protocol, grants LazyGrants, ttl time.Duration) (frontend.LazyStreams, error) {
	if db == nil || b == nil || protocol == "" || grants == nil {
		return nil, errors.New("sessions: missing lazy stream dependency")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if ttl > time.Hour {
		return nil, errors.New("sessions: lazy stream lifetime exceeds one hour")
	}
	return &lazyStreams{db: db, broker: b, protocol: protocol, grants: grants, ttl: ttl}, nil
}

func lazyHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *lazyStreams) Issue(ctx context.Context, credential string, req frontend.PlanRequest) (string, error) {
	if req.Protocol != s.protocol || req.Device.ID == "" || req.Viewer == nil || req.Intent.Plex != nil || req.Item == 0 || req.Channel != "" {
		return "", core.ErrUnauthorized
	}
	ref, viewer, err := s.grants.GrantReference(ctx, string(s.protocol), credential)
	if err != nil || viewer == nil || viewer.ID != req.Viewer.ID || ref == "" {
		return "", core.ErrUnauthorized
	}
	// Validate policy and item without resolving a release or consuming a stream.
	req.Viewer = viewer
	estimate := req
	estimate.Version = "" // Version lookup may resolve releases; defer it until GET.
	estimate.Intent.Estimate = true
	if _, err = s.broker.Plan(ctx, estimate); err != nil {
		return "", err
	}
	req.Viewer = nil
	req.Intent.Estimate = false
	data, err := json.Marshal(req)
	if err != nil || len(data) > 65536 {
		return "", core.ErrNotAvailable
	}
	token := rand.Text()
	now := s.broker.deps.Now()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions_lazy_streams WHERE expires<=?`, now.UnixMilli())
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,?,?,?,?,?)`, lazyHash(token), string(s.protocol), viewer.ID, ref, string(data), now.Add(s.ttl).UnixMilli())
	if err != nil {
		return "", err
	}
	return "/sessions/lazy/" + token + "/file", nil
}

func (s *lazyStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	token, ok := strings.CutPrefix(r.URL.Path, "/sessions/lazy/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, ok = strings.CutSuffix(token, "/file")
	if !ok || token == "" || strings.Contains(token, "/") {
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
	// Recheck the current safety and title policy even when reusing a session.
	title, err := s.broker.deps.Lookup(ctx, viewer, req.Device, req.Item)
	if err != nil || title == nil || title.Key != req.Item || !s.broker.permitted(viewer, req.Caps) || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
		http.NotFound(w, r)
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
	if id != "" {
		if _, _, err := s.broker.access(ctx, access, id); err != nil {
			id = ""
		}
	}
	opened := false
	if id == "" {
		plan, err := s.broker.Plan(ctx, req)
		if err != nil {
			http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
			return
		}
		session, err := s.broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: req.Device, Protocol: s.protocol, PlanID: plan.ID})
		if err != nil {
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
	path, err := s.broker.StartStream(ctx, access, id, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		if opened {
			_ = s.broker.Stop(ctx, access, id)
			s.broker.mu.Lock()
			delete(s.broker.lazySessions, hash)
			s.broker.mu.Unlock()
		}
		http.Error(w, "playback unavailable", http.StatusServiceUnavailable)
		return
	}
	// The scoped path remains internal; every client request must pass this
	// handler so revoking the issuing grant revokes the capability as well.
	clone := r.Clone(ctx)
	u := *r.URL
	u.Path = path
	u.RawPath = ""
	clone.URL = &u
	unlock()
	unlock = func() {}
	s.broker.ServeMediaFor(s.protocol, w, clone)
}
