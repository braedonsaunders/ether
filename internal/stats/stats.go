// Package stats records owner analytics from observed playback and provider data.
// It never reconstructs device or bandwidth information from watch counts.
package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func init() {
	store.Register("stats", 1, `
CREATE TABLE stats_plays (id TEXT PRIMARY KEY,user_id INTEGER NOT NULL,rating_key INTEGER NOT NULL,username TEXT NOT NULL,title TEXT NOT NULL,platform TEXT NOT NULL,device TEXT NOT NULL,decision TEXT NOT NULL,started INTEGER NOT NULL,updated INTEGER NOT NULL,ended INTEGER NOT NULL DEFAULT 0,bytes INTEGER NOT NULL DEFAULT 0,watched_ms INTEGER NOT NULL DEFAULT 0,duration_ms INTEGER NOT NULL DEFAULT 0,completed INTEGER NOT NULL DEFAULT 0,failovers INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',timeline INTEGER NOT NULL DEFAULT 0,timeline_state TEXT NOT NULL DEFAULT '');
CREATE INDEX stats_plays_started ON stats_plays(started DESC);
CREATE INDEX stats_plays_user ON stats_plays(user_id,started DESC);
CREATE TABLE stats_provider_days (provider TEXT NOT NULL,day TEXT NOT NULL,bytes INTEGER NOT NULL,articles INTEGER NOT NULL,misses INTEGER NOT NULL,errors INTEGER NOT NULL,PRIMARY KEY(provider,day));
CREATE TABLE stats_snapshots (kind TEXT PRIMARY KEY,payload TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE stats_costs (kind TEXT NOT NULL,id TEXT NOT NULL,monthly REAL NOT NULL,currency TEXT NOT NULL,PRIMARY KEY(kind,id));
`)
	// node_id records which node observed a play so a starting node closes
	// only its own interrupted sessions, never another live node's.
	store.Register("stats", 6, `ALTER TABLE stats_plays ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
CREATE INDEX stats_plays_open ON stats_plays(node_id) WHERE ended=0;`)
}

type Options struct {
	Newsletter    func(context.Context, string) error
	StopSession   func(string) error
	SessionExists func(string) bool
	Playback      core.Playback
	Providers     func() []usenet.ProviderStats
	Indexers      func(context.Context) ([]newznab.Remaining, error)
	Logger        *slog.Logger
	Now           func() time.Time
	// NodeID identifies this node in a cluster. Plays are tagged with it and
	// New closes only this node's unfinished plays. Empty for a standalone
	// server, which therefore closes every unfinished play as before.
	NodeID string
	// Availability, when set (clustered nodes), holds play records the
	// database cannot take through an outage (the latest per play) and
	// skips periodic provider snapshots while the database is down; their
	// counters are cumulative, so nothing is lost. Nil writes synchronously.
	Availability *store.Availability
	// MetadataType maps a rating key to a Plex metadata type number for
	// Dashboard media rows. Zero or nil skips the play.
	MetadataType func(core.RatingKey) int
	// AccountName labels a user on Dashboard media rows.
	AccountName func(int64) string
}
type Service struct {
	db       *sql.DB
	opts     Options
	mu       sync.Mutex
	previous map[string]usenet.ProviderStats
	// plays holds play records through a database outage (clustered).
	plays *store.Outbox[string, playRecord]
	dash  *Dashboard
}

// playRecord is one observation of a play, captured when it was made.
type playRecord struct {
	p       core.PlaySession
	at      time.Time
	ended   bool
	problem string
}

// mergePlay keeps the newest observation but never forgets that the play
// ended or failed.
func mergePlay(old, nw playRecord) playRecord {
	if old.ended && !nw.ended {
		nw.ended, nw.at = true, old.at
	}
	if nw.problem == "" {
		nw.problem = old.problem
	}
	return nw
}

func New(db *sql.DB, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	svc := &Service{db: db, opts: opts, previous: map[string]usenet.ProviderStats{}, dash: NewDashboard(DashboardOptions{Now: opts.Now})}
	if opts.Availability != nil {
		svc.plays = store.NewOutbox(store.OutboxOptions[string, playRecord]{
			Name: "playback stats", Availability: opts.Availability, Logger: opts.Logger, Merge: mergePlay,
			Apply: func(ctx context.Context, _ string, r playRecord) error { return svc.record(ctx, r) },
		})
	}
	if _, err := db.Exec(`UPDATE stats_plays SET ended=updated WHERE ended=0 AND node_id=?`, opts.NodeID); err != nil {
		opts.Logger.Warn("close prior playback stats", "err", err)
	}
	return svc
}

// ObservePlayback is suitable for playback.Manager.Subscribe.
func (s *Service) ObservePlayback(ev playback.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ev.Session == nil {
		return
	}
	if err := s.Record(ctx, ev.Session, ev.Kind == "stop", ev.Error); err != nil {
		s.opts.Logger.Warn("record playback stats", "err", err)
	}
}

// Record upserts a play. On a clustered node a record the database cannot
// take is held and written when it is back.
func (s *Service) Record(ctx context.Context, p *core.PlaySession, ended bool, problem string) error {
	if p == nil || p.ID == "" {
		return nil
	}
	r := playRecord{p: *p, at: s.opts.Now().UTC(), ended: ended, problem: problem}
	r.p.Source, r.p.Resolution = nil, nil
	if s.plays != nil {
		_, err := s.plays.Write(ctx, p.ID, r)
		return err
	}
	return s.record(ctx, r)
}

// Close writes play records still held from a database outage (bounded by
// ctx); a no-op on a standalone server.
func (s *Service) Close(ctx context.Context) error {
	if s.plays == nil {
		return nil
	}
	return s.plays.Close(ctx)
}

// OutboxStatus reports play records held through a database outage; ok is
// false on a standalone server.
func (s *Service) OutboxStatus() (st store.OutboxStatus, ok bool) {
	if s.plays == nil {
		return st, false
	}
	return s.plays.Status(), true
}

func (s *Service) record(ctx context.Context, r playRecord) error {
	p, now, ended, problem := &r.p, r.at, r.ended, r.problem
	start := p.StartedAt
	if start.IsZero() {
		start = now
	}
	end := int64(0)
	if ended {
		end = now.Unix()
	}
	device := p.Client.DeviceName
	if device == "" {
		device = p.Client.Device
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,bytes,duration_ms,failovers,error,node_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,title=excluded.title,platform=excluded.platform,device=excluded.device,decision=excluded.decision,updated=excluded.updated,`+
		`ended=CASE WHEN excluded.ended>stats_plays.ended THEN excluded.ended ELSE stats_plays.ended END,`+
		`bytes=CASE WHEN excluded.bytes>stats_plays.bytes THEN excluded.bytes ELSE stats_plays.bytes END,`+
		`duration_ms=CASE WHEN excluded.duration_ms>stats_plays.duration_ms THEN excluded.duration_ms ELSE stats_plays.duration_ms END,`+
		`failovers=excluded.failovers,error=CASE WHEN excluded.error='' THEN stats_plays.error ELSE excluded.error END,node_id=excluded.node_id`,
		p.ID, p.UserID, int64(p.Key), p.Username, p.TitleName, p.Client.Platform, device, string(p.Decision), start.Unix(), now.Unix(), end, p.BytesServed, p.Info.Duration.Milliseconds(), p.Failovers, problem, s.opts.NodeID)
	return err
}

// ObserveState correlates timeline data only when exactly one active session
// matches the user/title. Plex state events do not identify the client.
func (s *Service) ObserveState(ev state.Event) {
	// Imported history updates watch state, not the currently playing session.
	if ev.Imported {
		return
	}
	if ev.Kind != state.EventProgress && ev.Kind != state.EventCompleted && ev.Kind != state.EventScrobble {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := ev.At
	if now.IsZero() {
		now = s.opts.Now()
	}
	t := now.UnixMilli()
	completed := 0
	if ev.Completed {
		completed = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE stats_plays SET watched_ms=watched_ms+CASE WHEN CAST(? AS TEXT)='progress' AND timeline_state='playing' AND timeline>0 AND CAST(? AS BIGINT)>timeline AND CAST(? AS BIGINT)-timeline<=60000 THEN CAST(? AS BIGINT)-timeline ELSE 0 END,`+
		`timeline=?,timeline_state=?,duration_ms=CASE WHEN CAST(? AS BIGINT)>duration_ms THEN CAST(? AS BIGINT) ELSE duration_ms END,completed=CASE WHEN CAST(? AS BIGINT)>completed THEN CAST(? AS BIGINT) ELSE completed END WHERE id=(SELECT id FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0) AND (SELECT COUNT(*) FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0)=1`, string(ev.Kind), t, t, t, t, ev.State, ev.Duration.Milliseconds(), ev.Duration.Milliseconds(), completed, completed, ev.UserID, int64(ev.Key), ev.UserID, int64(ev.Key))
	if err != nil {
		s.opts.Logger.Warn("record timeline stats", "err", err)
	}
	if ev.Kind == state.EventCompleted && s.dash != nil && s.opts.MetadataType != nil {
		if typ := s.opts.MetadataType(ev.Key); typ > 0 {
			name := ""
			if s.opts.AccountName != nil {
				name = s.opts.AccountName(ev.UserID)
			}
			s.dash.RecordPlay(ev.UserID, name, ev.Origin.ClientID, typ, ev.Duration.Milliseconds())
		}
	}
}

// Dashboard is the in-memory Plex Dashboard store for this node.
func (s *Service) Dashboard() *Dashboard { return s.dash }

// Run samples active bandwidth and provider counters until cancellation.
func (s *Service) Run(ctx context.Context) {
	s.Sample(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sample(ctx)
		}
	}
}
func (s *Service) Sample(ctx context.Context) {
	if s.opts.Availability.Down() {
		// Plays are held by Record; cumulative provider counters and
		// snapshots simply wait for the next sample after the outage.
		if s.opts.Playback != nil {
			for _, p := range s.opts.Playback.Sessions() {
				_ = s.Record(ctx, p, false, "")
			}
		}
		return
	}
	if err := s.MaybeNewsletter(ctx); err != nil {
		s.opts.Logger.Warn("weekly newsletter queue failed")
	}
	if s.opts.Playback != nil {
		for _, p := range s.opts.Playback.Sessions() {
			if err := s.Record(ctx, p, false, ""); err != nil {
				s.opts.Logger.Warn("sample play stats", "err", err)
			}
		}
	}
	if s.opts.Providers != nil {
		if err := s.SnapshotProviders(ctx, s.opts.Providers()); err != nil {
			s.opts.Logger.Warn("sample provider stats", "err", err)
		}
	}
	if s.opts.Indexers != nil {
		if v, err := s.opts.Indexers(ctx); err == nil {
			s.snapshot(ctx, "indexers", v)
		}
	}
}
func (s *Service) snapshot(ctx context.Context, kind string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO stats_snapshots(kind,payload,at) VALUES(?,?,?) ON CONFLICT(kind) DO UPDATE SET payload=excluded.payload,at=excluded.at`, kind, string(b), s.opts.Now().Unix())
	return err
}
func (s *Service) SnapshotProviders(ctx context.Context, ps []usenet.ProviderStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.opts.Now().UTC().Format("2006-01-02")
	delta := func(n, o int64) int64 {
		if n < o {
			return n
		}
		return n - o
	}
	for _, p := range ps {
		id := p.ID
		if id == "" {
			id = p.Host
		}
		old := s.previous[id]
		_, err := s.db.ExecContext(ctx, `INSERT INTO stats_provider_days(provider,day,bytes,articles,misses,errors,backfill_saves,body_misses,body_takedowns,spillovers) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,day) DO UPDATE SET bytes=stats_provider_days.bytes+excluded.bytes,articles=stats_provider_days.articles+excluded.articles,misses=stats_provider_days.misses+excluded.misses,errors=stats_provider_days.errors+excluded.errors,backfill_saves=stats_provider_days.backfill_saves+excluded.backfill_saves,body_misses=stats_provider_days.body_misses+excluded.body_misses,body_takedowns=stats_provider_days.body_takedowns+excluded.body_takedowns,spillovers=stats_provider_days.spillovers+excluded.spillovers`, id, day, delta(p.Bytes, old.Bytes), delta(p.Articles, old.Articles), delta(p.Misses, old.Misses), delta(p.Errors, old.Errors), delta(p.BackfillSaves, old.BackfillSaves), delta(p.BodyMisses, old.BodyMisses), delta(p.BodyTakedowns, old.BodyTakedowns), delta(p.Spillovers, old.Spillovers))
		if err != nil {
			return err
		}
		s.previous[id] = p
	}
	return s.snapshot(ctx, "providers", ps)
}

type Play struct {
	ID         string `json:"id"`
	UserID     int64  `json:"userId"`
	Key        int64  `json:"ratingKey"`
	Username   string `json:"username"`
	Title      string `json:"title"`
	Platform   string `json:"platform"`
	Device     string `json:"device"`
	Decision   string `json:"decision"`
	Started    int64  `json:"started"`
	Updated    int64  `json:"updated"`
	Ended      int64  `json:"ended"`
	Bytes      int64  `json:"bytes"`
	WatchedMS  int64  `json:"watchedMs"`
	DurationMS int64  `json:"durationMs"`
	Completed  bool   `json:"completed"`
	Failovers  int    `json:"failovers"`
	Error      string `json:"error"`
}

func (s *Service) History(ctx context.Context, since, until time.Time, user int64, limit int) ([]Play, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,bytes,watched_ms,duration_ms,completed,failovers,error FROM stats_plays WHERE started>=? AND started<? AND (?=0 OR user_id=?) ORDER BY started DESC LIMIT ?`, since.Unix(), until.Unix(), user, user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Play{}
	for rows.Next() {
		var p Play
		if err := rows.Scan(&p.ID, &p.UserID, &p.Key, &p.Username, &p.Title, &p.Platform, &p.Device, &p.Decision, &p.Started, &p.Updated, &p.Ended, &p.Bytes, &p.WatchedMS, &p.DurationMS, &p.Completed, &p.Failovers, &p.Error); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type Bucket struct {
	Label     string `json:"label"`
	Plays     int64  `json:"plays"`
	WatchedMS int64  `json:"watchedMs"`
	Bytes     int64  `json:"bytes"`
}
type Summary struct {
	Plays     int64     `json:"plays"`
	WatchedMS int64     `json:"watchedMs"`
	Bytes     int64     `json:"bytes"`
	Completed int64     `json:"completed"`
	Daily     []Bucket  `json:"daily"`
	Hourly    []Bucket  `json:"hourly"`
	Platforms []Bucket  `json:"platforms"`
	Titles    []Bucket  `json:"titles"`
	Users     []Bucket  `json:"users"`
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
}

func (s *Service) Summary(ctx context.Context, since, until time.Time, user int64) (Summary, error) {
	out := Summary{Since: since, Until: until}
	where := ` FROM stats_plays WHERE started>=? AND started<? AND (?=0 OR user_id=?)`
	args := []any{since.Unix(), until.Unix(), user, user}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),CAST(COALESCE(SUM(watched_ms),0) AS BIGINT),CAST(COALESCE(SUM(bytes),0) AS BIGINT),CAST(COALESCE(SUM(completed),0) AS BIGINT)`+where, args...).Scan(&out.Plays, &out.WatchedMS, &out.Bytes, &out.Completed)
	if err != nil {
		return out, err
	}
	// Day and hour buckets group on integer arithmetic (portable across
	// SQLite and PostgreSQL) and are labelled here, in UTC.
	day := func(n int64) string { return time.Unix(n*86400, 0).UTC().Format("2006-01-02") }
	hour := func(n int64) string { return fmt.Sprintf("%02d", n) }
	groups := []struct {
		expr  string
		dst   *[]Bucket
		order string
		label func(int64) string // set when expr is numeric
	}{{`started/86400`, &out.Daily, "1", day}, {`(started/3600)%24`, &out.Hourly, "1", hour}, {`platform`, &out.Platforms, "2 DESC", nil}, {`title`, &out.Titles, "2 DESC", nil}, {`username || ' (' || user_id || ')'`, &out.Users, "2 DESC", nil}}
	for _, g := range groups {
		*g.dst = []Bucket{}
		rows, err := s.db.QueryContext(ctx, `SELECT `+g.expr+`,COUNT(*),CAST(COALESCE(SUM(watched_ms),0) AS BIGINT),CAST(COALESCE(SUM(bytes),0) AS BIGINT)`+where+` GROUP BY 1 ORDER BY `+g.order+` LIMIT 366`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var b Bucket
			var n int64
			var dst any = &b.Label
			if g.label != nil {
				dst = &n
			}
			if err := rows.Scan(dst, &b.Plays, &b.WatchedMS, &b.Bytes); err != nil {
				rows.Close()
				return out, err
			}
			if g.label != nil {
				b.Label = g.label(n)
			}
			*g.dst = append(*g.dst, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

type Cost struct {
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	Monthly  float64 `json:"monthly"`
	Currency string  `json:"currency"`
}

func (s *Service) SetCost(ctx context.Context, c Cost) error {
	c.Currency = strings.ToUpper(c.Currency)
	validCurrency := len(c.Currency) == 3
	for _, r := range c.Currency {
		if r < 'A' || r > 'Z' {
			validCurrency = false
		}
	}
	if (c.Kind != "provider" && c.Kind != "indexer") || c.ID == "" || c.Monthly < 0 || c.Monthly > 1000000 || math.IsNaN(c.Monthly) || math.IsInf(c.Monthly, 0) || !validCurrency {
		return fmt.Errorf("kind, ID, nonnegative price and three-letter currency required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stats_costs(kind,id,monthly,currency) VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET monthly=excluded.monthly,currency=excluded.currency`, c.Kind, c.ID, c.Monthly, c.Currency)
	return err
}
