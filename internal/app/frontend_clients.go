package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("frontend_clients", 1, `CREATE TABLE frontend_clients_seen (surface TEXT NOT NULL, device_hash TEXT NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY(surface,device_hash)); CREATE INDEX frontend_clients_seen_time ON frontend_clients_seen(surface,last_seen);`)
}

type clientObservation struct {
	surface frontend.Protocol
	hash    string
	seen    int64
}

// frontendClients records authenticated device identifiers without putting
// database latency on the request path. Cluster nodes share the same table.
type frontendClients struct {
	db         *sql.DB
	log        *slog.Logger
	now        func() time.Time
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	queue      chan clientObservation
	mu         sync.Mutex
	incomplete map[frontend.Protocol]time.Time
	pending    map[frontend.Protocol]int
	closed     bool
}

func newFrontendClients(db *sql.DB, log *slog.Logger, now func() time.Time) *frontendClients {
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &frontendClients{db: db, log: log, now: now, ctx: ctx, cancel: cancel, queue: make(chan clientObservation, 1024), incomplete: make(map[frontend.Protocol]time.Time), pending: make(map[frontend.Protocol]int)}
	c.wg.Add(1)
	go c.run()
	return c
}
func (c *frontendClients) observe(surface frontend.Protocol, deviceID string) {
	deviceID = strings.TrimSpace(deviceID)
	if surface == "" {
		return
	}
	if deviceID == "" || len(deviceID) > 512 {
		c.mu.Lock()
		c.incomplete[surface] = c.now()
		c.mu.Unlock()
		return
	}
	hash := sha256.Sum256([]byte(deviceID))
	encoded := hex.EncodeToString(hash[:])
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.queue <- clientObservation{surface, encoded, now.Unix()}:
		c.pending[surface]++
	default:
		c.incomplete[surface] = now
	}
}
func (c *frontendClients) record(ctx context.Context, o clientObservation) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO frontend_clients_seen(surface,device_hash,last_seen) VALUES(?,?,?) ON CONFLICT(surface,device_hash) DO UPDATE SET last_seen=CASE WHEN excluded.last_seen > frontend_clients_seen.last_seen THEN excluded.last_seen ELSE frontend_clients_seen.last_seen END`, string(o.surface), o.hash, o.seen)
	return err
}
func (c *frontendClients) process(ctx context.Context, o clientObservation) {
	request, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := c.record(request, o)
	cancel()
	if err != nil && c.ctx.Err() != nil {
		retry, done := context.WithTimeout(context.Background(), 3*time.Second)
		err = c.record(retry, o)
		done()
	}
	c.mu.Lock()
	c.pending[o.surface]--
	if err != nil {
		c.incomplete[o.surface] = c.now()
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("record frontend client", "err", err)
	}
}
func (c *frontendClients) run() {
	defer c.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			for {
				select {
				case o := <-c.queue:
					c.process(context.Background(), o)
				default:
					return
				}
			}
		case o := <-c.queue:
			c.process(c.ctx, o)
		case <-ticker.C:
			now := c.now()
			ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
			_, _ = c.db.ExecContext(ctx, `DELETE FROM frontend_clients_seen WHERE last_seen < ?`, now.Add(-30*24*time.Hour).Unix())
			cancel()
		}
	}
}
func (c *frontendClients) count(ctx context.Context, surface frontend.Protocol) (int, bool) {
	c.mu.Lock()
	lastIncomplete, failed := c.incomplete[surface]
	incomplete := (failed && c.now().Sub(lastIncomplete) < 24*time.Hour) || c.pending[surface] > 0
	c.mu.Unlock()
	var count int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_clients_seen WHERE surface=? AND last_seen>=?`, string(surface), c.now().Add(-24*time.Hour).Unix()).Scan(&count)
	return count, err == nil && !incomplete
}
func (c *frontendClients) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}
