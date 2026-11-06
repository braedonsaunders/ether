package stats

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("stats_device_registry", 1, `
CREATE TABLE stats_device_registry_counter (id INTEGER PRIMARY KEY CHECK(id=1),next_id INTEGER NOT NULL);
INSERT INTO stats_device_registry_counter(id,next_id) VALUES(1,2);
CREATE TABLE stats_device_registry (id INTEGER PRIMARY KEY,client_identifier TEXT NOT NULL UNIQUE,name TEXT NOT NULL,platform TEXT NOT NULL,created_at INTEGER NOT NULL);
INSERT INTO stats_device_registry(id,client_identifier,name,platform,created_at) VALUES(1,'','','',0);
`)
}

type deviceRequest struct {
	device   Device
	allocate bool
	done     chan deviceResult
}
type deviceResult struct {
	device Device
	err    error
}
type deviceRegistry struct {
	log       *slog.Logger
	seeded    bool
	db        *sql.DB
	requests  chan deviceRequest
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func newDeviceRegistry(db *sql.DB, log *slog.Logger) *deviceRegistry {
	return &deviceRegistry{db: db, log: log, requests: make(chan deviceRequest, 64), stop: make(chan struct{}), done: make(chan struct{})}
}
func (r *deviceRegistry) load() ([]Device, error) {
	if err := r.seedHistory(); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(`SELECT id,client_identifier,name,platform,created_at FROM stats_device_registry ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		var d Device
		if err = rows.Scan(&d.ID, &d.ClientIdentifier, &d.Name, &d.Platform, &d.CreatedAt); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}
func (r *deviceRegistry) start() { r.once.Do(func() { go r.run() }) }
func (r *deviceRegistry) allocate(device Device) (Device, error) {
	r.start()
	done := make(chan deviceResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case r.requests <- deviceRequest{device: device, allocate: true, done: done}:
	case <-r.stop:
		return Device{}, errors.New("device registry closed")
	case <-ctx.Done():
		return Device{}, ctx.Err()
	}
	select {
	case result := <-done:
		return result.device, result.err
	case <-r.stop:
		return Device{}, errors.New("device registry closed")
	case <-ctx.Done():
		return Device{}, ctx.Err()
	}
}
func (r *deviceRegistry) update(device Device) {
	r.start()
	// Caller owns Dashboard.devMu, so apply backpressure only to the bounded queue.
	select {
	case r.requests <- deviceRequest{device: device}:
	case <-r.stop:
	}
}
func (r *deviceRegistry) close(ctx context.Context) error {
	r.start()
	r.closeOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *deviceRegistry) run() {
	defer close(r.done)
	for {
		var batch []deviceRequest
		stopping := false
		select {
		case first := <-r.requests:
			batch = append(batch, first)
		case <-r.stop:
			stopping = true
		}
		if !stopping {
			timer := time.NewTimer(2 * time.Millisecond)
			select {
			case <-timer.C:
			case <-r.stop:
				stopping = true
				if !timer.Stop() {
					<-timer.C
				}
			}
		}
		for len(batch) < 64 {
			select {
			case request := <-r.requests:
				batch = append(batch, request)
			default:
				goto apply
			}
		}
	apply:
		if len(batch) > 0 {
			r.apply(batch)
		}
		if stopping && len(r.requests) == 0 {
			return
		}
	}
}
func (r *deviceRegistry) apply(batch []deviceRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	seedErr := r.seedHistory()
	tx, err := r.db.BeginTx(ctx, nil)
	if seedErr != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		tx = nil
		err = seedErr
	}
	results := make([]Device, len(batch))
	if err == nil {
		for i, request := range batch {
			d := request.device
			if request.allocate {
				// Reserve under the database row lock. A repeated client can burn a number,
				// but a number acknowledged to a caller is never returned to the allocator.
				var id int64
				err = tx.QueryRowContext(ctx, `UPDATE stats_device_registry_counter SET next_id=next_id+1 WHERE id=1 RETURNING next_id-1`).Scan(&id)
				if err == nil {
					err = tx.QueryRowContext(ctx, `INSERT INTO stats_device_registry(id,client_identifier,name,platform,created_at) VALUES(?,?,?,?,?) ON CONFLICT(client_identifier) DO UPDATE SET name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE stats_device_registry.name END,platform=CASE WHEN excluded.platform<>'' THEN excluded.platform ELSE stats_device_registry.platform END RETURNING id,client_identifier,name,platform,created_at`, id, d.ClientIdentifier, d.Name, d.Platform, d.CreatedAt).Scan(&d.ID, &d.ClientIdentifier, &d.Name, &d.Platform, &d.CreatedAt)
				}
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE stats_device_registry SET name=?,platform=?,created_at=CASE WHEN created_at=0 THEN ? ELSE created_at END WHERE id=?`, d.Name, d.Platform, d.CreatedAt, d.ID)
			}
			results[i] = d
			if err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	for i, request := range batch {
		if request.done != nil {
			request.done <- deviceResult{device: results[i], err: err}
		}
	}
	if err != nil {
		r.log.Warn("persist device registry", "err", err)
	}
}

// Older history has no client identifier, so its IDs cannot be reconstructed.
// Reserve the full old range instead of ever attaching a new client to it.
func (r *deviceRegistry) seedHistory() error {
	if r.seeded {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var floor int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(device_id),0) FROM state_history`).Scan(&floor)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "no such table") && !strings.Contains(msg, "no such column") && !strings.Contains(msg, "does not exist") {
			return err
		}
	}
	if floor >= int64(^uint64(0)>>1)-1 {
		return errors.New("device ID space exhausted")
	}
	floor++
	_, err = r.db.ExecContext(ctx, `UPDATE stats_device_registry_counter SET next_id=CASE WHEN next_id<? THEN ? ELSE next_id END WHERE id=1`, floor, floor)
	if err == nil {
		r.seeded = true
	}
	return err
}
