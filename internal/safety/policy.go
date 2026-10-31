// Package safety persists owner policy and enforces it at Plex request boundaries.
package safety

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // Owner time policies work on hosts without a zoneinfo database.

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("safety", 1, `CREATE TABLE safety_policies(user_id INTEGER PRIMARY KEY, policy TEXT NOT NULL);`)
}

type Policy struct {
	StreamLimit      int      `json:"streamLimit"`
	MaxContentRating string   `json:"maxContentRating"`
	BlockedGenres    []string `json:"blockedGenres"`
	BlockedKeywords  []string `json:"blockedKeywords"`
	RemoteDisabled   bool     `json:"remoteDisabled"`
	MaxBitrateKbps   int      `json:"maxBitrateKbps"`
	// AllowedHours is empty for any time, otherwise hours 0..23 in Timezone.
	AllowedHours []int  `json:"allowedHours"`
	Timezone     string `json:"timezone"`
}

type Service struct {
	catalogAllowed func(*core.Title) bool // installed once before request serving
	db             *sql.DB
	mu             sync.RWMutex
	policies       map[int64]Policy

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
	gen     atomic.Uint64

	playbackMu    sync.Mutex
	playbackEpoch map[int64]playbackConfirm
}

// Generation changes when a viewer's safety policy is saved.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

// playbackConfirm is an unrestricted viewer's fenced snapshot at one epoch.
// A later play re-reads the epoch and skips the write when it has not moved.
type playbackConfirm struct {
	epoch  int64
	user   core.User
	policy Policy
}

func New(ctx context.Context, db *sql.DB) (*Service, error) {
	s := &Service{db: db, policies: map[int64]Policy{}}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reload replaces the cached policies with the stored ones.
func (s *Service) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,policy FROM safety_policies`)
	if err != nil {
		return err
	}
	defer rows.Close()
	policies := map[int64]Policy{}
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var p Policy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		policies[id] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.policies = policies
	s.mu.Unlock()
	return nil
}

// SetBus connects the cluster bus (nil disconnects): policy changes made on
// this node reach the others' caches and vice versa.
func (s *Service) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "safety", nil, func(ctx context.Context, _ []string, _ bool) {
		_ = s.reload(ctx) // on error keep the last policies; the next change retries
	})
	s.invalMu.Lock()
	old := s.inval
	s.inval = iv
	s.invalMu.Unlock()
	old.Close()
}

// Close disconnects the cluster bus.
func (s *Service) Close() { s.SetBus(nil) }
func (s *Service) Get(id int64) Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.policies[id]
	p.AllowedHours = append([]int(nil), p.AllowedHours...)
	p.BlockedGenres = append([]string(nil), p.BlockedGenres...)
	p.BlockedKeywords = append([]string(nil), p.BlockedKeywords...)
	return p
}

// Validate reports why p cannot be stored, if it cannot.
func (p Policy) Validate() error {
	if p.MaxBitrateKbps < 0 || p.StreamLimit < 0 {
		return fmt.Errorf("invalid viewer or bitrate")
	}
	if p.MaxContentRating != "" && catalog.RatingRank(p.MaxContentRating) == 0 {
		return fmt.Errorf("unrecognized rating limit")
	}
	for _, h := range p.AllowedHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("hours must be 0..23")
		}
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return fmt.Errorf("invalid timezone")
		}
	}
	return nil
}

func (s *Service) Put(ctx context.Context, id int64, p Policy) error {
	// Cache an independent snapshot, just as Get returns one to callers.
	p.AllowedHours = append([]int(nil), p.AllowedHours...)
	p.BlockedGenres = append([]string(nil), p.BlockedGenres...)
	p.BlockedKeywords = append([]string(nil), p.BlockedKeywords...)
	if id == 0 {
		return fmt.Errorf("invalid viewer or bitrate")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO safety_policies(user_id,policy) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET policy=excluded.policy`, id, string(raw)); err != nil {
		return err
	}
	s.mu.Lock()
	s.policies[id] = p
	s.gen.Add(1)
	s.mu.Unlock()
	s.invalMu.Lock()
	iv := s.inval
	s.invalMu.Unlock()
	iv.Notify(strconv.FormatInt(id, 10))
	return nil
}

// SetCatalogPolicy installs the global catalogue policy during app construction.
func (s *Service) SetCatalogPolicy(fn func(*core.Title) bool) { s.catalogAllowed = fn }

func (s *Service) Allowed(u *core.User, t *core.Title) bool {
	if s.catalogAllowed != nil && !s.catalogAllowed(t) {
		return false
	}
	if t == nil || t.Adult {
		return false
	}
	if u == nil {
		return true
	}
	return AllowedWithPolicy(u, t, s.ForUser(u))
}

// AllowedWithPolicy evaluates caller-owned policy facts. The caller must
// separately enforce the catalog visibility gate and durable policy fence.
func AllowedWithPolicy(u *core.User, t *core.Title, p Policy) bool {
	if t == nil || t.Adult {
		return false
	}
	if u == nil {
		return true
	}
	if catalog.RatingExceeds(t.ContentRating, u.MaxContentRating) || catalog.RatingExceeds(t.ContentRating, p.MaxContentRating) {
		return false
	}
	blocked := func(values, bans []string) bool {
		for _, v := range values {
			for _, b := range bans {
				if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(b)) {
					return true
				}
			}
		}
		return false
	}
	return !blocked(t.Genres, p.BlockedGenres) && !blocked(t.Keywords, p.BlockedKeywords)
}
func (s *Service) CanPlay(u *core.User, remote bool, now time.Time) bool {
	return CanPlayWithPolicy(u, s.ForUser(u), remote, now)
}

// CanPlayWithPolicy evaluates the same playback rules against an owned policy
// snapshot. It does not establish freshness or an authorization interval.
func CanPlayWithPolicy(u *core.User, p Policy, remote bool, now time.Time) bool {
	if u == nil {
		return false
	}
	if remote && p.RemoteDisabled {
		return false
	}
	if len(p.AllowedHours) == 0 {
		return true
	}
	loc := time.UTC
	if p.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(p.Timezone)
		if err != nil {
			return false
		}
	}
	for _, h := range p.AllowedHours {
		if now.In(loc).Hour() == h {
			return true
		}
	}
	return false
}

// PolicyInTx reads the durable policy row. A missing row is unrestricted.
func (s *Service) PolicyInTx(ctx context.Context, tx *sql.Tx, viewerID int64) (Policy, error) {
	if s == nil || tx == nil || viewerID <= 0 {
		return Policy{}, fmt.Errorf("safety: invalid viewer")
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT policy FROM safety_policies WHERE user_id=?`, viewerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	var policy Policy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}
func IsRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
func KidsPreset(age string) Policy {
	rating := "PG"
	switch age {
	case "preschool":
		rating = "TV-Y"
	case "teens":
		rating = "PG-13"
	}
	return Policy{MaxContentRating: rating, BlockedKeywords: []string{"pornography", "erotic movie"}}
}
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid viewer", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Get(id))
	})
	m.HandleFunc("PUT /{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var p Policy
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		d.DisallowUnknownFields()
		if id <= 0 {
			http.Error(w, "invalid viewer", 400)
			return
		}
		if err := d.Decode(&p); err != nil {
			http.Error(w, "invalid policy", 400)
			return
		}
		if err := s.Put(r.Context(), id, p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})
	m.HandleFunc("GET /presets/kids", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KidsPreset(r.URL.Query().Get("age")))
	})
	return m
}

// ForUser is u's policy.
func (s *Service) ForUser(u *core.User) Policy {
	if u == nil {
		return Policy{}
	}
	return s.Get(u.ID)
}
func (s *Service) UserPolicy(u *core.User) *core.User {
	return UserPolicyWithPolicy(u, s.ForUser(u))
}

// UserPolicyWithPolicy applies restrictions from a caller-owned policy copy.
func UserPolicyWithPolicy(u *core.User, p Policy) *core.User {
	if u == nil {
		return nil
	}
	out := *u
	if p.MaxContentRating != "" && (out.MaxContentRating == "" || catalog.RatingRank(p.MaxContentRating) < catalog.RatingRank(out.MaxContentRating)) {
		out.MaxContentRating = p.MaxContentRating
	}
	if p.StreamLimit > 0 && (out.StreamLimit <= 0 || p.StreamLimit < out.StreamLimit) {
		out.StreamLimit = p.StreamLimit
	}
	return &out
}
func (s *Service) BitrateLimit(u *core.User) int { return s.ForUser(u).MaxBitrateKbps }

func (s *Service) Restricted(u *core.User) bool {
	if u == nil {
		return true
	}
	p := s.ForUser(u)
	return u.MaxContentRating != "" || len(u.AllowedSections) > 0 || p.MaxContentRating != "" || len(p.BlockedGenres) > 0 || len(p.BlockedKeywords) > 0
}
