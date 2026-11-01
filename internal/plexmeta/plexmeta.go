package plexmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("plexmeta", 1, `CREATE TABLE plexmeta_titles(rating_key INTEGER PRIMARY KEY,guid TEXT NOT NULL,payload TEXT NOT NULL,fetched_at INTEGER NOT NULL);`)
}

const (
	cacheTTL        = 30 * 24 * time.Hour
	bgWorkers       = 2
	bgQueueSize     = 256
	bgLookupTimeout = 8 * time.Second
	pageSize        = 50
	breakerFails    = 5
	breakerCooldown = 2 * time.Minute
)

// Options configure the background metadata cache.
type Options struct {
	HTTP *http.Client
	// ClientIdentifier is the per-install id shared with plexguid.
	ClientIdentifier func() string
	BaseURL          string
	Token            func() string
	// GUID returns a verified plex:// mapping; empty means skip the key.
	GUID func(context.Context, core.RatingKey) string
	// Enabled is the owner toggle and claimed check. Nil means off.
	Enabled    func() bool
	Background bool
	Now        func() time.Time
}

// Service caches Plex metadata-provider payloads. Reads never wait on Plex.
type Service struct {
	db *sql.DB
	o  Options

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closeOne sync.Once

	mu      sync.Mutex
	idle    *sync.Cond
	pending map[core.RatingKey]bool
	jobs    chan core.RatingKey
	brk     breaker
}

func New(db *sql.DB, o Options) *Service {
	if o.HTTP == nil {
		o.HTTP = egress.Client(egress.CapMetadata, 8*time.Second)
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://metadata.provider.plex.tv"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		db:      db,
		o:       o,
		ctx:     ctx,
		cancel:  cancel,
		pending: map[core.RatingKey]bool{},
		brk:     breaker{cooldown: breakerCooldown, threshold: breakerFails, now: o.Now},
	}
	s.idle = sync.NewCond(&s.mu)
	if o.Background {
		s.jobs = make(chan core.RatingKey, bgQueueSize)
		for i := 0; i < bgWorkers; i++ {
			s.wg.Add(1)
			go s.worker()
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			<-ctx.Done()
			s.mu.Lock()
			s.idle.Broadcast()
			s.mu.Unlock()
		}()
	}
	return s
}

func (s *Service) Close() {
	s.closeOne.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

// Wait blocks until queued background fetches finish.
func (s *Service) Wait() {
	if s == nil || s.jobs == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.pending) > 0 && s.ctx.Err() == nil {
		s.idle.Wait()
	}
}

func (s *Service) on() bool {
	return s != nil && s.o.Enabled != nil && s.o.Enabled()
}

func (s *Service) clientIdentifier() string {
	if s.o.ClientIdentifier != nil {
		if id := strings.TrimSpace(s.o.ClientIdentifier()); id != "" {
			return id
		}
	}
	return plexguid.ClientIdentifier("", "")
}

// Schedule queues keys that have a verified plex:// guid. Safe on request paths.
func (s *Service) Schedule(keys ...core.RatingKey) {
	if s == nil || s.jobs == nil || s.ctx.Err() != nil || !s.on() {
		return
	}
	if s.o.Token == nil || s.o.Token() == "" || s.o.GUID == nil {
		return
	}
	if s.brk.Open() {
		return
	}
	for _, k := range keys {
		if k == 0 {
			continue
		}
		s.mu.Lock()
		if s.pending[k] {
			s.mu.Unlock()
			continue
		}
		s.pending[k] = true
		s.mu.Unlock()
		select {
		case s.jobs <- k:
		default:
			s.done(k)
		}
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case k := <-s.jobs:
			func() {
				defer s.done(k)
				if !s.on() || s.brk.Open() {
					return
				}
				rctx, cancel := context.WithTimeout(s.ctx, bgLookupTimeout)
				defer cancel()
				_ = s.refresh(rctx, k)
			}()
		}
	}
}

func (s *Service) done(k core.RatingKey) {
	s.mu.Lock()
	delete(s.pending, k)
	if len(s.pending) == 0 {
		s.idle.Broadcast()
	}
	s.mu.Unlock()
}

// Cached is a primary-key lookup. It never talks to Plex.
func (s *Service) Cached(ctx context.Context, k core.RatingKey) (Cached, bool) {
	if s == nil || s.db == nil || k == 0 {
		return Cached{}, false
	}
	var raw, guid string
	var at int64
	if err := s.db.QueryRowContext(ctx, `SELECT guid,payload,fetched_at FROM plexmeta_titles WHERE rating_key=?`, int64(k)).Scan(&guid, &raw, &at); err != nil {
		return Cached{}, false
	}
	var o Overlay
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &o)
	}
	return Cached{Overlay: o, GUID: guid, FetchedAt: time.Unix(at, 0)}, true
}

// CachedMany batch-loads overlays by rating key only.
func (s *Service) CachedMany(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]Cached {
	if s == nil || s.db == nil || len(keys) == 0 {
		return nil
	}
	uniq := make([]core.RatingKey, 0, len(keys))
	seen := make(map[core.RatingKey]struct{}, len(keys))
	for _, k := range keys {
		if k == 0 {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[core.RatingKey]Cached, len(uniq))
	const chunkSize = 200
	for start := 0; start < len(uniq); start += chunkSize {
		end := start + chunkSize
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		var b strings.Builder
		b.WriteString(`SELECT rating_key,guid,payload,fetched_at FROM plexmeta_titles WHERE rating_key IN (`)
		args := make([]any, 0, len(chunk))
		for i, k := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			args = append(args, int64(k))
		}
		b.WriteByte(')')
		rows, err := s.db.QueryContext(ctx, b.String(), args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var rk int64
			var guid, raw string
			var at int64
			if err := rows.Scan(&rk, &guid, &raw, &at); err != nil {
				continue
			}
			var o Overlay
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &o)
			}
			out[core.RatingKey(rk)] = Cached{Overlay: o, GUID: guid, FetchedAt: time.Unix(at, 0)}
		}
		_ = rows.Close()
	}
	return out
}

// StoreForTest writes a cache row. Tests only.
func (s *Service) StoreForTest(k core.RatingKey, guid string, o Overlay) error {
	now := time.Now()
	if s.o.Now != nil {
		now = s.o.Now()
	}
	return s.store(context.Background(), k, guid, o, now)
}

func (s *Service) store(ctx context.Context, k core.RatingKey, guid string, o Overlay, now time.Time) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plexmeta_titles(rating_key,guid,payload,fetched_at) VALUES(?,?,?,?) ON CONFLICT(rating_key) DO UPDATE SET guid=excluded.guid,payload=excluded.payload,fetched_at=excluded.fetched_at`, int64(k), guid, string(raw), now.Unix())
	return err
}
