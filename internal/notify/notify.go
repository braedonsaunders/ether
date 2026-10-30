// Package notify provides durable, rule-filtered owner notifications. Channel
// secrets never appear in API responses, delivery logs, or transport errors.
package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/faillog"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/usenet"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

func init() {
	store.Register("notify", 1, `
CREATE TABLE notify_channels(id TEXT PRIMARY KEY,config TEXT NOT NULL);
CREATE TABLE notify_queue(id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,event TEXT NOT NULL,created INTEGER NOT NULL,due INTEGER NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX notify_due ON notify_queue(status,due);
`)
	// Claim columns let several nodes drain one queue: a row is delivered
	// by the node whose conditional update moved it out of 'pending'.
	store.Register("notify", 3, `ALTER TABLE notify_queue ADD COLUMN error TEXT NOT NULL DEFAULT '';
ALTER TABLE notify_queue ADD COLUMN immediate INTEGER NOT NULL DEFAULT 0;
CREATE TABLE notify_settings(id TEXT PRIMARY KEY,config TEXT NOT NULL);
CREATE TABLE notify_push_keys(id TEXT PRIMARY KEY,private_key TEXT NOT NULL,public_key TEXT NOT NULL);`)
	store.Register("notify", 2, `
ALTER TABLE notify_queue ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE notify_queue ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;
`)
}

const Redacted = "[configured]"

var Kinds = []string{"server.started", "server.stopping", "server.crash", "provider.recovered", "indexer.recovered", "cache.recovered", "playback.start", "playback.stop", "playback.error", "playback.failover", "provider.outage", "indexer.outage", "indexer.budget", "cache.full", "cert.renewed", "update.available", "backup.failed", "user.joined", "title.available", "newsletter.weekly", "test"}

type Channel struct {
	SMTPMode      string            `json:"smtpMode,omitempty"`
	Subscription  *PushSubscription `json:"subscription,omitempty"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Enabled       bool              `json:"enabled"`
	Events        []string          `json:"events"`
	Endpoint      string            `json:"endpoint"`
	Token         string            `json:"token"`
	Target        string            `json:"target"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	From          string            `json:"from"`
	To            string            `json:"to"`
	PerMinute     int               `json:"perMinute"`
	DigestMinutes int               `json:"digestMinutes"`
}
type Event struct {
	HTML    string    `json:"html,omitempty"`
	Kind    string    `json:"kind"`
	Title   string    `json:"title"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}
type Options struct {
	// NodeID names this node on the queue rows it claims; a random id is
	// used when empty.
	NodeID     string
	Providers  func() []usenet.ProviderStats
	CacheUsage func() (int64, int64)
	Client     *http.Client
	Now        func() time.Time
	Logger     *slog.Logger
	// Sealer protects stored channel secrets and the push identity
	// (secrets.go); nil stores them as given.
	Sealer config.FieldSealer
}
type Service struct {
	monitorDone             <-chan struct{}
	watchMu                 sync.Mutex
	watchUpdate             func() error
	pushPrivate, pushPublic string

	drainMu        chan struct{}
	healthMu       sync.Mutex
	sampleMu       sync.Mutex // serializes provider/cache sampling independently of indexer callbacks
	providerStates map[string]string
	indexerAlerts  map[string]*indexerAlert // indexer+kind -> notification state
	indexerLog     *faillog.Tracker
	cacheFull      bool
	db             *sql.DB
	opts           Options
}

func New(db *sql.DB, opts Options) *Service {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NodeID == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		opts.NodeID = hex.EncodeToString(b[:])
	}
	return &Service{drainMu: make(chan struct{}, 1), db: db, opts: opts, indexerLog: faillog.New(opts.Logger, "enqueue indexer notification")}
}

// claimTimeout is how long a claimed row may stay in 'sending' before it is
// presumed abandoned by a node that stopped mid-delivery.
const claimTimeout = 10 * time.Minute

func (s *Service) channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT config FROM notify_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		c, _, err := s.decodeChannel(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func redacted(c Channel) Channel {
	for _, p := range []*string{&c.Endpoint, &c.Token, &c.Password} {
		if *p != "" {
			*p = Redacted
		}
	}
	if c.Subscription != nil {
		c.Subscription = &PushSubscription{Endpoint: Redacted, Keys: PushKeys{Auth: Redacted, P256dh: Redacted}}
	}
	return c
}
func (s *Service) Channels(ctx context.Context) ([]Channel, error) {
	cs, err := s.channels(ctx)
	for i := range cs {
		cs[i] = redacted(cs[i])
	}
	return cs, err
}
func (s *Service) Save(ctx context.Context, c Channel) error {
	if c.ID == "" || len(c.ID) > 100 || strings.ContainsAny(c.ID, "/\\") {
		return errors.New("channel ID required (no slashes)")
	}
	cs, err := s.channels(ctx)
	if err != nil {
		return err
	}
	for _, old := range cs {
		if old.ID == c.ID {
			if err := keepChannelSecrets(&c, old); err != nil {
				return err
			}
		}
	}
	if c.PerMinute == 0 {
		c.PerMinute = 5
	}
	if c.PerMinute < 1 || c.PerMinute > 60 || c.DigestMinutes < 0 || c.DigestMinutes > 10080 {
		return errors.New("rate must be 1–60/minute; digest must be 0–10080 minutes")
	}
	if err := validateChannel(c); err != nil {
		return err
	}
	raw, err := s.encodeChannel(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO notify_channels(id,config) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET config=excluded.config`, c.ID, raw)
	if err == nil {
		s.refreshMonitor()
	}
	return err
}
func (s *Service) Emit(ctx context.Context, e Event) error {
	e, selected, err := s.prepare(ctx, e)
	if err != nil || len(selected) == 0 {
		return err
	}
	// Commit every destination together. A failed fan-out can be retried
	// without duplicating notifications already queued for earlier channels.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range selected {
		if err := enqueueWith(ctx, tx, c, e, s.opts.Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prepare validates e, stamps its time and selects the channels it goes to.
func (s *Service) prepare(ctx context.Context, e Event) (Event, []Channel, error) {
	if !known(e.Kind) {
		return e, nil, errors.New("unknown event kind")
	}
	if len(e.Title) > 500 || len(e.Message) > 8000 || len(e.HTML) > 256000 {
		return e, nil, errors.New("event too large")
	}
	if e.At.IsZero() {
		e.At = s.opts.Now()
	}
	cs, err := s.channels(ctx)
	if err != nil {
		return e, nil, err
	}
	var selected []Channel
	for _, c := range cs {
		if c.Enabled && matches(c.Events, e.Kind) && (e.Kind != "newsletter.weekly" || c.Kind == "smtp") {
			selected = append(selected, c)
		}
	}
	if len(selected) == 0 && e.Kind == "newsletter.weekly" {
		return e, nil, errors.New("no enabled SMTP channel selected for weekly newsletters")
	}
	return e, selected, nil
}
func known(k string) bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}
func matches(events []string, k string) bool {
	for _, e := range events {
		if e == k || e == "*" {
			return true
		}
	}
	return false
}
func (s *Service) enqueue(ctx context.Context, c Channel, e Event) error {
	return enqueueWith(ctx, s.db, c, e, s.opts.Now())
}

func enqueueWith(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, c Channel, e Event, now time.Time) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO notify_queue(channel,event,created,due,immediate) VALUES(?,?,?,?,?)`, c.ID, string(raw), now.Unix(), deliveryDue(c, e, now).Unix(), immediateEvent(e))
	return err
}

// ObservePlayback only enqueues; network delivery runs separately in Run.
func (s *Service) ObservePlayback(ev playback.Event) {
	if ev.Kind == "update" {
		return
	}
	kind := ev.Kind
	if kind == "downfailover" {
		kind = "failover"
	}
	e := Event{Kind: "playback." + kind, Title: "Ether playback " + ev.Kind, At: ev.At}
	if ev.Session != nil {
		e.Message = ev.Session.Username + ": " + ev.Session.TitleName
	}
	if ev.Error != "" {
		e.Message = ev.Error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Emit(ctx, e); err != nil {
		s.opts.Logger.Warn("enqueue notification", "err", err)
	}
}

// TitleAvailable lets the discovery/watchlist flow publish availability.
func (s *Service) TitleAvailable(ctx context.Context, title string) error {
	return s.Emit(ctx, Event{Kind: "title.available", Title: "Title available", Message: title})
}

// Run is the single queue consumer. Failed sends are recorded without automatic
// retry, avoiding duplicate delivery when an upstream timeout is ambiguous.
func (s *Service) Run(ctx context.Context) {
	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()
	_ = s.SampleHealth(ctx)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-healthTicker.C:
			_ = s.SampleHealth(ctx)
		case <-ticker.C:
			if err := s.Drain(ctx); err != nil && ctx.Err() == nil {
				s.opts.Logger.Warn("notification queue unavailable")
			}
		}
	}
}
func (s *Service) Drain(ctx context.Context) error {
	select {
	case s.drainMu <- struct{}{}:
		defer func() { <-s.drainMu }()
	case <-ctx.Done():
		return ctx.Err()
	}
	cs, err := s.channels(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		now := s.opts.Now().Unix()
		var recent int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_queue WHERE channel=? AND attempted>?`, c.ID, now-60).Scan(&recent); err != nil {
			return err
		}
		if recent >= c.PerMinute {
			continue
		}
		var immediate int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_queue WHERE channel=? AND status='pending' AND immediate=1 AND due<=?`, c.ID, now).Scan(&immediate); err != nil {
			return err
		}
		limit := 1
		if c.DigestMinutes > 0 && immediate == 0 {
			limit = 100
		}
		query := `SELECT id,event FROM notify_queue WHERE channel=? AND status='pending' AND due<=? ORDER BY id LIMIT ?`
		args := []any{c.ID, now, limit}
		if immediate > 0 {
			query = `SELECT id,event FROM notify_queue WHERE channel=? AND status='pending' AND immediate=1 AND due<=? ORDER BY id LIMIT ?`
		} else if c.DigestMinutes > 0 {
			// A digest window opens with its first event: once that event
			// is due, everything queued behind it goes in the same message.
			// Selecting only due rows would send each event on its own as
			// it individually came due, so nothing was ever batched.
			var due int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_queue WHERE channel=? AND status='pending' AND immediate=0 AND due<=?`, c.ID, now).Scan(&due); err != nil {
				return err
			}
			if due == 0 {
				continue
			}
			query = `SELECT id,event FROM notify_queue WHERE channel=? AND status='pending' AND immediate=0 ORDER BY id LIMIT ?`
			args = []any{c.ID, limit}
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		candidates := []int64{}
		pending := []Event{}
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			var e Event
			if json.Unmarshal([]byte(raw), &e) == nil {
				candidates = append(candidates, id)
				pending = append(pending, e)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Claim each row with a conditional update; a row another node
		// claimed first is skipped, so every event is delivered by one node.
		ids := []int64{}
		events := []Event{}
		for i, id := range candidates {
			res, err := s.db.ExecContext(ctx, `UPDATE notify_queue SET status='sending',attempted=?,claimed_by=?,claimed_at=? WHERE id=? AND status='pending'`,
				now, s.opts.NodeID, now, id)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 1 {
				ids = append(ids, id)
				events = append(events, pending[i])
			}
		}
		if len(ids) == 0 {
			continue
		}
		e := events[0]
		if len(events) > 1 {
			e.HTML = ""
			e.Title = fmt.Sprintf("Ether digest: %d events", len(events))
			parts := []string{}
			for _, v := range events {
				parts = append(parts, v.Title+": "+v.Message)
			}
			e.Message = strings.Join(parts, "\n")
		}
		status, detail := "sent", ""
		if err := s.deliver(ctx, c, e); err != nil {
			status, detail = "failed", err.Error()
		}
		for _, id := range ids {
			if _, err := s.db.ExecContext(ctx, `UPDATE notify_queue SET status=?,error=? WHERE id=? AND claimed_by=?`, status, detail, id, s.opts.NodeID); err != nil {
				return err
			}
		}
	}
	// A claim whose node stopped mid-delivery is recorded as failed rather
	// than resent: the event may already have been delivered.
	if _, err := s.db.ExecContext(ctx, `UPDATE notify_queue SET status='failed',error='Delivery interrupted; receipt unknown' WHERE status='sending' AND claimed_at<?`,
		s.opts.Now().Add(-claimTimeout).Unix()); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM notify_queue WHERE status IN ('sent','failed','cancelled') AND created<?`, s.opts.Now().AddDate(0, 0, -30).Unix())
	return err
}

// Critical lifecycle events bypass digest delays. Crash delivery itself is handled
// by the independent monitor, without relying on this queue or database.
func deliveryDue(c Channel, e Event, now time.Time) time.Time {
	if immediateEvent(e) == 1 {
		return now
	}
	return now.Add(time.Duration(c.DigestMinutes) * time.Minute)
}

func immediateEvent(e Event) int {
	if strings.HasPrefix(e.Kind, "server.") || e.Kind == "test" || e.Kind == "newsletter.weekly" {
		return 1
	}
	return 0
}
