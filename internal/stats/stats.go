// Package stats records owner analytics from observed playback and provider data.
// It never reconstructs device or bandwidth information from watch counts.
package stats

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/braedonsaunders/ether/internal/catalog" // summary joins the catalogue
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
	// Viewer experience (Activity › Experience). measured marks plays
	// recorded with this accounting; older rows keep 0 and show no start
	// time or buffering rather than invented numbers.
	store.Register("stats", 8, `ALTER TABLE stats_plays ADD COLUMN measured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN played INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN pictured INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN ttff_ms INTEGER NOT NULL DEFAULT -1;
ALTER TABLE stats_plays ADD COLUMN start_failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN buffer_events INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN buffer_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN down_failovers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN source_kbps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN uhd INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN hdr INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN remote INTEGER NOT NULL DEFAULT 0;
ALTER TABLE stats_plays ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN release_name TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN reason_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN reason_detail TEXT NOT NULL DEFAULT '';`)
	store.Register("stats", 9, `ALTER TABLE stats_plays ADD COLUMN quality_fallback TEXT NOT NULL DEFAULT '';`)
	store.Register("stats", 10, `ALTER TABLE stats_plays ADD COLUMN client_ip TEXT;
ALTER TABLE stats_plays ADD COLUMN audio_copy INTEGER;`)
	store.Register("stats", 15, `ALTER TABLE stats_plays ADD COLUMN channel_id TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN channel_name TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN channel_number TEXT NOT NULL DEFAULT '';`)
	store.Register("stats", 16, `ALTER TABLE stats_plays ADD COLUMN live_source_name TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN live_login_label TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN live_login_number INTEGER NOT NULL DEFAULT 0;`)
	store.Register("stats", 17, `ALTER TABLE stats_plays ADD COLUMN chapter_count INTEGER;`)
	store.RegisterAfter("stats_seen_devices", "stats")
	store.Register("stats_seen_devices", 1, `ALTER TABLE stats_plays ADD COLUMN product TEXT NOT NULL DEFAULT '';
ALTER TABLE stats_plays ADD COLUMN device_match TEXT NOT NULL DEFAULT '';
CREATE INDEX stats_plays_device_latest ON stats_plays(platform,device,updated DESC,id DESC);`)
	// Backfill device_match for older plays where it can be derived:
	// 1. Copy device_match from the latest play of the same (platform, device)
	//    that recorded it (recovers pre-v1 plays and headerless plays).
	// 2. Post-v1 plays (product <> '') that still have empty device_match came
	//    from protocols without a separate Device field (Jellyfin/Emby, Silo,
	//    Vela) or headerless Plex, where device IS the runtime match field.
	// 3. Propagate those derived values back to pre-v1 plays of the same
	//    (platform, device). Pre-v1 plays with no post-v1 play stay empty.
	store.Register("stats_seen_devices", 2, `UPDATE stats_plays SET device_match=COALESCE((SELECT p2.device_match FROM stats_plays p2 WHERE p2.platform=stats_plays.platform AND p2.device=stats_plays.device AND p2.device_match<>'' ORDER BY p2.updated DESC,p2.id DESC LIMIT 1),'') WHERE device_match='' AND device<>'';
UPDATE stats_plays SET device_match=device WHERE device_match='' AND product<>'' AND device<>'';
UPDATE stats_plays SET device_match=COALESCE((SELECT p2.device_match FROM stats_plays p2 WHERE p2.platform=stats_plays.platform AND p2.device=stats_plays.device AND p2.device_match<>'' ORDER BY p2.updated DESC,p2.id DESC LIMIT 1),'') WHERE device_match='' AND product='' AND device<>'';`)
	// The hardware model the client reported (X-Plex-Model). Seen devices
	// shows it so two clients with the same device name (every Apple TV
	// says "Apple TV") can be told apart. Older plays stay empty.
	store.Register("stats_seen_devices", 3, `ALTER TABLE stats_plays ADD COLUMN model TEXT NOT NULL DEFAULT '';`)
}

type Options struct {
	// DeliveredVOD observes the first proven media delivery from apps that
	// do not send a timeline. The callback must return without blocking.
	DeliveredVOD func(id string, viewerID int64, key core.RatingKey, app core.Protocol, started time.Time)

	Newsletter    func(context.Context, string) error
	StopSession   func(string) error
	SessionExists func(string) bool
	Session       func(string) (*core.PlaySession, bool)
	LiveSessions  func() []*core.PlaySession
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
	// Debrid and Stream report this node's cumulative debrid account and
	// streaming-engine counters; the sampler keeps their daily growth.
	Debrid func() []DebridSample
	Stream func() (StreamSample, bool)
}
type Service struct {
	db       *sql.DB
	opts     Options
	mu       sync.Mutex
	previous map[string]usenet.ProviderStats
	// prevDebrid and prevStream are the last debrid samples (under mu).
	prevDebrid map[string]DebridSample
	prevStream StreamSample
	// vmu serializes play records: viewings maps playback sessions onto
	// history rows, and a row's writes must land in observation order.
	vmu      sync.Mutex
	viewings *viewings
	// plays holds play records through a database outage (clustered).
	plays *store.Outbox[string, playRecord]
	dash  *Dashboard

	timelineMu       sync.Mutex
	timelineOnce     sync.Once
	timelineStopOnce sync.Once
	timelineCh       chan timelineCmd
	timelineStop     chan struct{}
	timelineDone     chan struct{}
	logSink          *SessionLogSink
}

func (s *Service) liveSession(id string) (*core.PlaySession, bool) {
	if s.opts.Session != nil {
		return s.opts.Session(id)
	}
	if s.opts.Playback != nil {
		return s.opts.Playback.Session(id)
	}
	return nil, false
}

// playRecord is one observation of a play, captured when it was made.
type playRecord struct {
	p            core.PlaySession
	at           time.Time
	ended        bool
	problem      string
	release      string // release name, kept after Resolution is dropped
	watchedMS    int64  // active duration for live sessions; 0 for VOD (tracked by ObserveState)
	deliveredVOD bool
}

// mergePlay keeps the newest observation of a row but never forgets that
// it failed. The newest record decides whether the row is open: a viewing
// that a reopened session continues is live again.
func mergePlay(old, nw playRecord) playRecord {
	if nw.problem == "" {
		nw.problem = old.problem
	}
	if nw.release == "" {
		nw.release = old.release
	}
	nw.deliveredVOD = nw.deliveredVOD || old.deliveredVOD
	if old.watchedMS > nw.watchedMS {
		nw.watchedMS = old.watchedMS
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
	svc := &Service{db: db, opts: opts, previous: map[string]usenet.ProviderStats{}, viewings: newViewings(), dash: NewDashboard(DashboardOptions{Now: opts.Now})}
	svc.dash.registry = newDeviceRegistry(db, opts.Logger)
	if devices, err := svc.dash.registry.load(); err != nil {
		opts.Logger.Warn("load device registry", "err", err)
	} else {
		for i := range devices {
			device := devices[i]
			if device.CreatedAt == 0 {
				device.CreatedAt = opts.Now().Unix()
				svc.dash.registry.update(device)
			}
			svc.dash.byClient[device.ClientIdentifier] = &device
			svc.dash.byID[device.ID] = &device
		}
	}
	if opts.Availability != nil {
		svc.plays = store.NewOutbox(store.OutboxOptions[string, playRecord]{
			Name: "playback stats", Availability: opts.Availability, Logger: opts.Logger, Merge: mergePlay,
			Apply: func(ctx context.Context, _ string, r playRecord) error { return svc.record(ctx, r) },
		})
	}
	if _, err := db.Exec(`UPDATE stats_plays SET ended=updated WHERE ended=0 AND node_id=?`, opts.NodeID); err != nil {
		opts.Logger.Warn("close prior playback stats", "err", err)
	}
	// The first start with experience accounting dates "measured from".
	if _, err := db.Exec(`INSERT INTO stats_snapshots(kind,payload,at) VALUES(?,'{}',?) ON CONFLICT(kind) DO NOTHING`, measuredSinceKind, opts.Now().Unix()); err != nil {
		opts.Logger.Warn("record experience start", "err", err)
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
	ended := ev.Kind == "stop"
	if !ended && ev.Kind == "error" && (s.opts.Session != nil || s.opts.Playback != nil) {
		if _, live := s.liveSession(ev.Session.ID); !live {
			ended = true
		}
	}
	if err := s.Record(ctx, ev.Session, ended, ev.Error); err != nil {
		s.opts.Logger.Warn("record playback stats", "err", err)
	}
}

// Record observes a playback session and upserts the history row of the
// viewing it belongs to (viewing.go): sessions of one viewer, client and
// title within the play-session window share a row, and a row is written
// only once media was delivered or the attempt failed. On a clustered node
// a record the database cannot take is held and written when it is back.
func (s *Service) Record(ctx context.Context, p *core.PlaySession, ended bool, problem string) error {
	if p == nil || p.ID == "" {
		return nil
	}
	// Internal virtual-channel programme feeders belong to the channel's live
	// viewing session, never as separate VOD plays.
	if strings.HasPrefix(p.ClientID, "channel-input/") || strings.HasPrefix(p.Client.ClientID, "channel-input/") {
		return nil
	}
	problem = clampTimelineText(problem)
	release := ""
	if p.Resolution != nil {
		release = p.Resolution.Release.Name
	}
	s.vmu.Lock()
	defer s.vmu.Unlock()
	r, rowID, ok := s.viewings.observe(*p, s.opts.Now().UTC(), ended, problem)
	if rowID != "" && rowID != p.ID {
		s.aliasTimelineSession(p.ID, rowID)
	}
	if !ok {
		return nil
	}
	r.release = release
	if s.plays != nil {
		_, err := s.plays.Write(ctx, r.p.ID, r)
		return err
	}
	return s.record(ctx, r)
}

// Close flushes device metadata, timeline writes, and play records held through a database outage.
// The caller controls the shutdown budget with ctx.
func (s *Service) Close(ctx context.Context) error {
	var err error
	if s.dash.registry != nil {
		err = s.dash.registry.close(ctx)
	}
	if s.plays != nil {
		err = errors.Join(err, s.plays.Close(ctx))
	}
	err = errors.Join(err, s.closeTimeline(ctx))
	return err
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
	p, now, ended, problem := &r.p, r.at, r.ended, clampTimelineText(r.problem)
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
	deviceMatch := p.Client.Device
	if deviceMatch == "" {
		deviceMatch = p.Client.DeviceName
	}
	clientID := p.Client.ClientID
	if clientID == "" {
		clientID = p.ClientID
	}
	if clientID != "" {
		s.dash.SeeDevice(clientID, device, p.Client.Platform)
	}
	q := measure(p, r.release)
	if problem == "" {
		problem = clampTimelineText(q.startFailure)
	}
	var channelID, channelName, channelNumber, liveSourceName, liveLoginLabel string
	var liveLoginNumber int
	if p.LiveChannel != nil {
		channelID = cmp.Or(p.LiveChannel.ID, p.LiveChannel.Name)
		channelName = cmp.Or(p.LiveChannel.Name, p.TitleName)
		channelNumber = p.LiveChannel.Number
		if provider := p.LiveChannel.Provider; provider != nil {
			liveSourceName, liveLoginLabel, liveLoginNumber = provider.SourceName, provider.LoginLabel, provider.LoginNumber
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stats_plays(id,user_id,rating_key,username,title,platform,device,decision,started,updated,ended,bytes,watched_ms,duration_ms,failovers,error,node_id,`+
		`measured,played,pictured,ttff_ms,start_failed,buffer_events,buffer_ms,down_failovers,source_kbps,uhd,hdr,remote,origin,release_name,reason_kind,reason_detail,quality_fallback,client_ip,audio_copy,product,device_match,channel_id,channel_name,channel_number,live_source_name,live_login_label,live_login_number,chapter_count,model) `+
		`VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,title=excluded.title,platform=excluded.platform,device=excluded.device,decision=excluded.decision,updated=excluded.updated,`+
		`ended=excluded.ended,product=CASE WHEN excluded.product='' THEN stats_plays.product ELSE excluded.product END,device_match=CASE WHEN excluded.device_match='' THEN stats_plays.device_match ELSE excluded.device_match END,`+
		`model=CASE WHEN excluded.model='' THEN stats_plays.model ELSE excluded.model END,`+
		`bytes=CASE WHEN excluded.bytes>stats_plays.bytes THEN excluded.bytes ELSE stats_plays.bytes END,`+
		`watched_ms=CASE WHEN excluded.watched_ms>stats_plays.watched_ms THEN excluded.watched_ms ELSE stats_plays.watched_ms END,`+
		`duration_ms=CASE WHEN excluded.duration_ms>stats_plays.duration_ms THEN excluded.duration_ms ELSE stats_plays.duration_ms END,`+
		`failovers=excluded.failovers,error=CASE WHEN excluded.error='' THEN stats_plays.error ELSE excluded.error END,node_id=excluded.node_id,`+
		// The viewing (viewing.go) holds the row's experience totals.
		`played=excluded.played,pictured=excluded.pictured,ttff_ms=excluded.ttff_ms,start_failed=excluded.start_failed,`+
		`buffer_events=excluded.buffer_events,buffer_ms=excluded.buffer_ms,down_failovers=excluded.down_failovers,`+
		// Media facts are replaced only by a record that knows them.
		`source_kbps=CASE WHEN excluded.source_kbps>0 THEN excluded.source_kbps ELSE stats_plays.source_kbps END,`+
		`uhd=CASE WHEN excluded.source_kbps>0 THEN excluded.uhd ELSE stats_plays.uhd END,`+
		`hdr=CASE WHEN excluded.source_kbps>0 THEN excluded.hdr ELSE stats_plays.hdr END,`+
		`remote=excluded.remote,client_ip=COALESCE(excluded.client_ip,stats_plays.client_ip),audio_copy=COALESCE(excluded.audio_copy,stats_plays.audio_copy),`+
		`origin=CASE WHEN excluded.origin='' THEN stats_plays.origin ELSE excluded.origin END,`+
		`release_name=CASE WHEN excluded.release_name='' THEN stats_plays.release_name ELSE excluded.release_name END,`+
		`reason_kind=CASE WHEN excluded.decision='' THEN stats_plays.reason_kind ELSE excluded.reason_kind END,`+
		`reason_detail=CASE WHEN excluded.decision='' THEN stats_plays.reason_detail ELSE excluded.reason_detail END,`+
		`quality_fallback=CASE WHEN excluded.quality_fallback='' THEN stats_plays.quality_fallback ELSE excluded.quality_fallback END,`+
		`channel_id=CASE WHEN excluded.channel_id='' THEN stats_plays.channel_id ELSE excluded.channel_id END,`+
		`channel_name=CASE WHEN excluded.channel_name='' THEN stats_plays.channel_name ELSE excluded.channel_name END,`+
		`channel_number=CASE WHEN excluded.channel_number='' THEN stats_plays.channel_number ELSE excluded.channel_number END,`+
		`live_source_name=CASE WHEN excluded.live_source_name='' THEN stats_plays.live_source_name ELSE excluded.live_source_name END,`+
		`live_login_label=CASE WHEN excluded.live_source_name='' THEN stats_plays.live_login_label ELSE excluded.live_login_label END,`+
		`live_login_number=CASE WHEN excluded.live_source_name='' THEN stats_plays.live_login_number ELSE excluded.live_login_number END,`+
		`chapter_count=excluded.chapter_count`,
		p.ID, p.UserID, int64(p.Key), p.Username, p.TitleName, p.Client.Platform, device, string(p.Decision), start.Unix(), now.Unix(), end, p.BytesServed, r.watchedMS, p.Info.Duration.Milliseconds(), p.Failovers, problem, s.opts.NodeID,
		q.played, q.pictured, q.ttffMS, q.startFailed, q.bufferEvents, q.bufferMS, p.DownFailovers, q.sourceKbps, q.uhd, q.hdr, q.remote, p.Origin, q.release, q.reasonKind, q.reasonDetail, p.QualityFallback, nullIP(p.Client.ClientIP), audioCopy(p), p.Client.Product, deviceMatch, channelID, channelName, channelNumber, liveSourceName, liveLoginLabel, liveLoginNumber, p.ProbedChapterCount(), p.Client.Model)
	if err == nil && r.deliveredVOD && s.opts.DeliveredVOD != nil {
		s.opts.DeliveredVOD(p.ID, p.UserID, p.Key, p.Client.Protocol, start)
	}
	if err == nil && clientID != "" {
		_, err = s.db.ExecContext(ctx, `UPDATE stats_device_registry SET last_seen=?,latest_play_id=? WHERE client_identifier=? AND last_seen<=?`, now.Unix(), p.ID, clientID, now.Unix())
	}
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
	var newlyCompletedID string
	if ev.Completed {
		completed = 1
		var id string
		var prevCompleted, count int
		if err := s.db.QueryRowContext(ctx,
			`SELECT id,completed,(SELECT COUNT(*) FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0 AND channel_id='') FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0 AND channel_id='' LIMIT 1`,
			ev.UserID, int64(ev.Key), ev.UserID, int64(ev.Key),
		).Scan(&id, &prevCompleted, &count); err == nil && count == 1 && prevCompleted == 0 {
			newlyCompletedID = id
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE stats_plays SET watched_ms=watched_ms+CASE WHEN CAST(? AS TEXT)='progress' AND timeline_state='playing' AND timeline>0 AND CAST(? AS BIGINT)>timeline AND CAST(? AS BIGINT)-timeline<=60000 THEN CAST(? AS BIGINT)-timeline ELSE 0 END,`+
		`timeline=?,timeline_state=?,duration_ms=CASE WHEN CAST(? AS BIGINT)>duration_ms THEN CAST(? AS BIGINT) ELSE duration_ms END,completed=CASE WHEN CAST(? AS BIGINT)>completed THEN CAST(? AS BIGINT) ELSE completed END WHERE id=(SELECT id FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0 AND channel_id='') AND (SELECT COUNT(*) FROM stats_plays WHERE user_id=? AND rating_key=? AND ended=0 AND channel_id='')=1`, string(ev.Kind), t, t, t, t, ev.State, ev.Duration.Milliseconds(), ev.Duration.Milliseconds(), completed, completed, ev.UserID, int64(ev.Key), ev.UserID, int64(ev.Key))
	if err != nil {
		s.opts.Logger.Warn("record timeline stats", "err", err)
	} else if newlyCompletedID != "" {
		s.ObserveTimeline(playback.TimelineEvent{
			SessionID: newlyCompletedID,
			At:        now,
			Kind:      "completed",
			Level:     "info",
			Message:   "Playback completed",
		})
	}
	if ev.Kind == state.EventCompleted && s.dash != nil && s.opts.MetadataType != nil {
		if typ := s.opts.MetadataType(ev.Key); typ > 0 {
			name := ""
			if s.opts.AccountName != nil {
				name = s.opts.AccountName(ev.UserID)
			}
			s.dash.RecordPlay(ev.UserID, name, ev.Origin.ClientID, typ, int64(ev.Duration/time.Second))
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
	s.vmu.Lock()
	s.viewings.prune(s.opts.Now().UTC())
	s.vmu.Unlock()
	if s.opts.Availability.Down() {
		// Plays are held by Record; cumulative provider counters and
		// snapshots simply wait for the next sample after the outage.
		if s.opts.Playback != nil {
			for _, p := range s.opts.Playback.Sessions() {
				_ = s.Record(ctx, p, false, "")
			}
		}
		if s.opts.LiveSessions != nil {
			for _, p := range s.opts.LiveSessions() {
				if p.ViewerState == "playing" && core.SessionActiveStream(p) {
					_ = s.Record(ctx, p, false, "")
				}
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
	if s.opts.LiveSessions != nil {
		for _, p := range s.opts.LiveSessions() {
			if p.ViewerState != "playing" || !core.SessionActiveStream(p) {
				continue
			}
			if err := s.Record(ctx, p, false, ""); err != nil {
				s.opts.Logger.Warn("sample live play stats", "err", err)
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
	if s.opts.Debrid != nil {
		if err := s.SnapshotDebrid(ctx, s.opts.Debrid()); err != nil {
			s.opts.Logger.Warn("sample debrid stats", "err", err)
		}
	}
	if s.opts.Stream != nil {
		if v, ok := s.opts.Stream(); ok {
			if err := s.SnapshotStream(ctx, v); err != nil {
				s.opts.Logger.Warn("sample stream stats", "err", err)
			}
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

func nullIP(ip string) any {
	if ip == "" {
		return nil
	}
	return ip
}

func audioCopy(p *core.PlaySession) any {
	if p.Decision != core.DecisionRemux {
		return nil
	}
	if p.DecisionAudioCopy {
		return 1
	}
	return 0
}

type Play struct {
	IP              *string `json:"ip"`
	ID              string  `json:"id"`
	UserID          int64   `json:"userId"`
	Key             int64   `json:"ratingKey"`
	Username        string  `json:"username"`
	Title           string  `json:"title"`
	ChannelID       string  `json:"channelId,omitempty"`
	ChannelName     string  `json:"channelName,omitempty"`
	ChannelNumber   string  `json:"channelNumber,omitempty"`
	LiveSourceName  string  `json:"liveSourceName,omitempty"`
	LiveLoginLabel  string  `json:"liveLoginLabel,omitempty"`
	LiveLoginNumber int     `json:"liveLoginNumber,omitempty"`
	Platform        string  `json:"platform"`
	Device          string  `json:"device"`
	Decision        string  `json:"decision"`
	Started         int64   `json:"started"`
	Updated         int64   `json:"updated"`
	Ended           int64   `json:"ended"`
	Bytes           int64   `json:"bytes"`
	WatchedMS       int64   `json:"watchedMs"`
	DurationMS      int64   `json:"durationMs"`
	Completed       bool    `json:"completed"`
	Failovers       int     `json:"failovers"`
	Error           string  `json:"error"`
}

func (s *Service) History(ctx context.Context, since, until time.Time, user int64, limit int) ([]Play, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,rating_key,username,title,channel_id,channel_name,channel_number,live_source_name,live_login_label,live_login_number,platform,device,decision,started,updated,ended,bytes,watched_ms,duration_ms,completed,failovers,error,client_ip FROM stats_plays WHERE started>=? AND started<? AND (?=0 OR user_id=?) ORDER BY started DESC LIMIT ?`, since.Unix(), until.Unix(), user, user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Play{}
	for rows.Next() {
		var p Play
		if err := rows.Scan(&p.ID, &p.UserID, &p.Key, &p.Username, &p.Title, &p.ChannelID, &p.ChannelName, &p.ChannelNumber, &p.LiveSourceName, &p.LiveLoginLabel, &p.LiveLoginNumber, &p.Platform, &p.Device, &p.Decision, &p.Started, &p.Updated, &p.Ended, &p.Bytes, &p.WatchedMS, &p.DurationMS, &p.Completed, &p.Failovers, &p.Error, &p.IP); err != nil {
			return nil, err
		}
		p.Error = clampTimelineText(p.Error)
		out = append(out, p)
	}
	return out, rows.Err()
}

type Bucket struct {
	Label string `json:"label"`
	// Key identifies the bucket's subject when it has one: a title's or
	// show's rating key, or a viewer's user id (for artwork and avatars).
	Key string `json:"key,omitempty"`
	// ChannelID identifies a Live TV channel bucket so the console shows the
	// channel logo rather than looking up a catalogue poster.
	ChannelID string `json:"channelId,omitempty"`
	Plays     int64  `json:"plays"`
	WatchedMS int64  `json:"watchedMs"`
	Bytes     int64  `json:"bytes"`
}
type Summary struct {
	Heatmap        []HeatCell      `json:"heatmap"`
	DailyKinds     []DailyKind     `json:"dailyKinds"`
	DailyDelivery  []DailyDelivery `json:"dailyDelivery"`
	PeakStreams    int             `json:"peakStreams"`
	AvgBitrateKbps float64         `json:"avgBitrateKbps"`
	Decisions      []Bucket        `json:"decisions"`
	Origins        []Bucket        `json:"origins"`
	Qualities      []Bucket        `json:"qualities"`
	Places         []Bucket        `json:"places"`
	Shows          []Bucket        `json:"shows"`
	Genres         []Bucket        `json:"genres"`
	Kinds          []Bucket        `json:"kinds"`
	Plays          int64           `json:"plays"`
	WatchedMS      int64           `json:"watchedMs"`
	Bytes          int64           `json:"bytes"`
	Completed      int64           `json:"completed"`
	Daily          []Bucket        `json:"daily"`
	Hourly         []Bucket        `json:"hourly"`
	Platforms      []Bucket        `json:"platforms"`
	Titles         []Bucket        `json:"titles"`
	Users          []Bucket        `json:"users"`
	Since          time.Time       `json:"since"`
	Until          time.Time       `json:"until"`
}

// friendlyPlatform normalises raw client platform slugs case-insensitively to
// owner-facing names before Delivery aggregates play counts and transfer bytes.
// mapped reports whether raw matched a known canonical platform table entry.
func friendlyPlatform(raw string) (label string, mapped bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", true
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	switch b.String() {
	case "", "unknown", "generic":
		return "", true
	case "web", "plexweb", "browser", "html5":
		return "Web", true
	case "tvos", "appletv", "appletvos":
		return "Apple TV", true
	case "ios", "iphone", "ipod":
		return "iOS", true
	case "ipados", "ipad":
		return "iPadOS", true
	case "visionos", "applevision":
		return "visionOS", true
	case "macos", "macosx", "osx", "darwin", "mac":
		return "macOS", true
	case "androidtv":
		return "Android TV", true
	case "googletv":
		return "Google TV", true
	case "firetv", "fireos", "amazonfiretv":
		return "Fire TV", true
	case "android":
		return "Android", true
	case "linux":
		return "Linux", true
	case "windows", "win32", "win64", "uwp":
		return "Windows", true
	case "tizen", "samsung", "samsungtizen":
		return "Tizen", true
	case "webos", "lgwebos", "netcast":
		return "webOS", true
	case "roku":
		return "Roku", true
	case "chromecast":
		return "Chromecast", true
	case "xbox":
		return "Xbox", true
	case "playstation", "ps4", "ps5":
		return "PlayStation", true
	case "vidaa", "hisense":
		return "VIDAA", true
	case "vizio", "smartcast":
		return "Vizio", true
	case "kodi":
		return "Kodi", true
	case "chrome":
		return "Chrome", true
	case "safari":
		return "Safari", true
	case "firefox":
		return "Firefox", true
	case "edge":
		return "Edge", true
	}
	if s == strings.ToLower(s) && s[0] >= 'a' && s[0] <= 'z' {
		return strings.ToUpper(s[:1]) + s[1:], false
	}
	return s, false
}

func mergePlatformBuckets(raw []Bucket) []Bucket {
	if len(raw) == 0 {
		return []Bucket{}
	}
	byKey := make(map[string]int, len(raw))
	out := make([]Bucket, 0, len(raw))
	for _, b := range raw {
		trimmed := strings.TrimSpace(b.Label)
		label, mapped := friendlyPlatform(trimmed)
		key := strings.ToLower(label)
		if idx, ok := byKey[key]; ok {
			out[idx].Plays += b.Plays
			out[idx].WatchedMS += b.WatchedMS
			out[idx].Bytes += b.Bytes
			if !mapped && trimmed != strings.ToLower(trimmed) {
				out[idx].Label = trimmed
			}
			continue
		}
		byKey[key] = len(out)
		out = append(out, Bucket{
			Label:     label,
			Plays:     b.Plays,
			WatchedMS: b.WatchedMS,
			Bytes:     b.Bytes,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Plays != out[j].Plays {
			return out[i].Plays > out[j].Plays
		}
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// A play delivered media. A failed attempt stays in History, marked
// with its error, but is not a play, and neither is a zero-byte row an
// earlier version recorded for a probe or a duplicate open.
const summaryPlayFilter = `(bytes>0 OR watched_ms>0)`

func (s *Service) daysWindow(days int) (since, until time.Time) {
	if days <= 0 {
		days = 30
	}
	now := s.opts.Now().UTC()
	return now.AddDate(0, 0, -days), now.Add(time.Second)
}

func tzShift(tzMinutes ...int) int {
	if len(tzMinutes) > 0 && tzMinutes[0] >= -1440 && tzMinutes[0] <= 1440 {
		return tzMinutes[0] * 60
	}
	return 0
}

// Activity returns daily observed playback totals over the same rolling window,
// timezone bucketing, and play filter as Summary (GET /stats/summary?days=<days>),
// oldest first, including empty days so overview charts keep a continuous axis.
func (s *Service) Activity(ctx context.Context, days int, tzMinutes ...int) ([]core.ActivityDay, error) {
	if days <= 0 {
		days = 30
	}
	if days > 3660 {
		days = 3660
	}
	since, until := s.daysWindow(days)
	shift := tzShift(tzMinutes...)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT (started+%d)/86400,COUNT(*),CAST(COALESCE(SUM(watched_ms),0) AS BIGINT),COUNT(DISTINCT user_id) FROM stats_plays WHERE started>=? AND started<? AND `+summaryPlayFilter+` GROUP BY 1 ORDER BY 1`,
		shift,
	), since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type agg struct {
		plays     int
		watchedMS int64
		viewers   int
	}
	byDay := map[int64]agg{}
	for rows.Next() {
		var dayIdx int64
		var a agg
		if err := rows.Scan(&dayIdx, &a.plays, &a.watchedMS, &a.viewers); err != nil {
			return nil, err
		}
		byDay[dayIdx] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Cover every local calendar day overlapping [since, until), matching
	// Summary's rolling window so summing daily entries equals Summary totals.
	firstDay := (since.Unix() + int64(shift)) / 86400
	lastDay := (until.Unix() - 1 + int64(shift)) / 86400
	out := make([]core.ActivityDay, 0, lastDay-firstDay+1)
	for d := firstDay; d <= lastDay; d++ {
		ad := core.ActivityDay{Date: time.Unix(d*86400, 0).UTC().Format("2006-01-02")}
		if a, ok := byDay[d]; ok {
			ad.Plays = a.plays
			ad.WatchedMS = a.watchedMS
			ad.Hours = math.Round(float64(a.watchedMS)/360000) / 10
			ad.UniqueViewers = a.viewers
		}
		out = append(out, ad)
	}
	return out, nil
}

func (s *Service) Summary(ctx context.Context, since, until time.Time, user int64, tzMinutes ...int) (Summary, error) {
	out := Summary{Since: since, Until: until}
	where := ` FROM stats_plays WHERE started>=? AND started<? AND (?=0 OR user_id=?) AND ` + summaryPlayFilter
	args := []any{since.Unix(), until.Unix(), user, user}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),CAST(COALESCE(SUM(watched_ms),0) AS BIGINT),CAST(COALESCE(SUM(bytes),0) AS BIGINT),CAST(COALESCE(SUM(completed),0) AS BIGINT)`+where, args...).Scan(&out.Plays, &out.WatchedMS, &out.Bytes, &out.Completed)
	if err != nil {
		return out, err
	}
	var bitrateBytes, bitrateTime int64
	err = s.db.QueryRowContext(ctx, `SELECT CAST(COALESCE(SUM(CASE WHEN watched_ms>0 THEN bytes ELSE 0 END),0) AS BIGINT),CAST(COALESCE(SUM(CASE WHEN watched_ms>0 THEN watched_ms ELSE 0 END),0) AS BIGINT)`+where, args...).Scan(&bitrateBytes, &bitrateTime)
	if err != nil {
		return out, err
	}
	if bitrateTime > 0 {
		out.AvgBitrateKbps = float64(bitrateBytes) * 8 / float64(bitrateTime)
	}
	shift := tzShift(tzMinutes...)
	// Day and hour buckets group on integer arithmetic (portable across
	// SQLite and PostgreSQL) and are labelled here, in UTC.
	day := func(n int64) string { return time.Unix(n*86400, 0).UTC().Format("2006-01-02") }
	hour := func(n int64) string { return fmt.Sprintf("%02d", n) }
	groups := []struct {
		expr  string
		dst   *[]Bucket
		order string
		label func(int64) string // set when expr is numeric
	}{{fmt.Sprintf(`(started+%d)/86400`, shift), &out.Daily, "1", day}, {fmt.Sprintf(`((started+%d)/3600)%%24`, shift), &out.Hourly, "1", hour}, {`CASE WHEN decision='directplay' THEN 'direct-play' WHEN decision='copy' AND (audio_copy=1 OR audio_copy IS NULL) THEN 'direct-stream' WHEN decision='copy' AND audio_copy=0 THEN 'audio-transcode' WHEN decision='transcode' THEN 'transcode' ELSE 'unknown' END`, &out.Decisions, "2 DESC", nil}, {`CASE WHEN origin='' THEN 'Other' ELSE origin END`, &out.Origins, "2 DESC", nil}, {`CASE WHEN uhd<>0 AND hdr<>0 THEN '4K HDR' WHEN uhd<>0 THEN '4K' WHEN hdr<>0 THEN 'HDR' ELSE 'HD/SD' END`, &out.Qualities, "2 DESC", nil}, {`CASE WHEN remote<>0 THEN 'away' ELSE 'home' END`, &out.Places, "2 DESC", nil}, {`CASE WHEN channel_id<>'' THEN 'live' WHEN rating_key/1000000000000000=1 THEN 'movie' WHEN rating_key/1000000000000000=4 THEN 'episode' ELSE 'unknown' END`, &out.Kinds, "2 DESC", nil}, {`platform`, &out.Platforms, "2 DESC", nil}, {`username || ' (' || user_id || ')'`, &out.Users, "2 DESC", nil}}
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
			if (g.dst == &out.Decisions || g.dst == &out.Kinds) && b.Label == "unknown" {
				continue
			}
			*g.dst = append(*g.dst, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	out.Platforms = mergePlatformBuckets(out.Platforms)
	if err := s.keyBuckets(ctx, &out, where, args); err != nil {
		return out, err
	}
	if err := s.dailyKinds(ctx, &out, where, args, shift, day); err != nil {
		return out, err
	}
	if err := s.catalogSummary(ctx, &out, args); err != nil {
		return out, err
	}
	if err := s.activitySummary(ctx, &out, where, args, shift); err != nil {
		return out, err
	}
	return out, nil
}

type Cost struct {
	Kind     string  `json:"kind"`
	ID       string  `json:"id"`
	Monthly  float64 `json:"monthly"`
	Currency string  `json:"currency"`
}

// costKinds are the sources a plan price can belong to: Usenet providers,
// indexers, debrid accounts and IPTV sources.
var costKinds = map[string]bool{"provider": true, "indexer": true, "debrid": true, "iptv": true}

func (s *Service) SetCost(ctx context.Context, c Cost) error {
	c.Currency = strings.ToUpper(c.Currency)
	validCurrency := len(c.Currency) == 3
	for _, r := range c.Currency {
		if r < 'A' || r > 'Z' {
			validCurrency = false
		}
	}
	if !costKinds[c.Kind] || c.ID == "" || c.Monthly < 0 || c.Monthly > 1000000 || math.IsNaN(c.Monthly) || math.IsInf(c.Monthly, 0) || !validCurrency {
		return fmt.Errorf("kind, ID, nonnegative price and three-letter currency required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stats_costs(kind,id,monthly,currency) VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET monthly=excluded.monthly,currency=excluded.currency`, c.Kind, c.ID, c.Monthly, c.Currency)
	return err
}

// DailyKind is one day's watch time split into movies, TV episodes and Live TV.
type DailyKind struct {
	Label     string `json:"label"`
	MovieMS   int64  `json:"movieMs"`
	EpisodeMS int64  `json:"episodeMs"`
	LiveMS    int64  `json:"liveMs"`
}

// keyBuckets populates Top titles (grouping Live TV rows by channel with their
// channel id, and VOD rows by title with their rating key) and splits viewer
// buckets into plain name and user id.
func (s *Service) keyBuckets(ctx context.Context, out *Summary, where string, args []any) error {
	out.Titles = []Bucket{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT CASE WHEN channel_id<>'' THEN COALESCE(NULLIF(channel_name,''),title) ELSE title END,`+
			`channel_id,CAST(MAX(CASE WHEN channel_id='' THEN rating_key ELSE 0 END) AS BIGINT),`+
			`COUNT(*),CAST(COALESCE(SUM(watched_ms),0) AS BIGINT),CAST(COALESCE(SUM(bytes),0) AS BIGINT)`+
			where+` GROUP BY channel_id,CASE WHEN channel_id<>'' THEN COALESCE(NULLIF(channel_name,''),title) ELSE title END ORDER BY 4 DESC,5 DESC,1 ASC LIMIT 366`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b Bucket
		var key int64
		if err = rows.Scan(&b.Label, &b.ChannelID, &key, &b.Plays, &b.WatchedMS, &b.Bytes); err != nil {
			rows.Close()
			return err
		}
		if b.ChannelID == "" && key > 0 {
			b.Key = strconv.FormatInt(key, 10)
		}
		out.Titles = append(out.Titles, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Viewer buckets are labelled "name (id)" so that two viewers sharing a
	// name stay apart; split that into the name and the id.
	for i := range out.Users {
		l := out.Users[i].Label
		if open := strings.LastIndex(l, " ("); open >= 0 && strings.HasSuffix(l, ")") {
			out.Users[i].Key = l[open+2 : len(l)-1]
			out.Users[i].Label = l[:open]
		}
	}
	return nil
}

// dailyKinds splits each day's watch time into movies, episodes and Live TV, on
// the same local-day buckets as Daily.
func (s *Service) dailyKinds(ctx context.Context, out *Summary, where string, args []any, shift int, day func(int64) string) error {
	out.DailyKinds = []DailyKind{}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT (started+%d)/86400,`+
		`CAST(COALESCE(SUM(CASE WHEN channel_id='' AND rating_key/1000000000000000=1 THEN watched_ms ELSE 0 END),0) AS BIGINT),`+
		`CAST(COALESCE(SUM(CASE WHEN channel_id='' AND rating_key/1000000000000000=4 THEN watched_ms ELSE 0 END),0) AS BIGINT),`+
		`CAST(COALESCE(SUM(CASE WHEN channel_id<>'' THEN watched_ms ELSE 0 END),0) AS BIGINT)`, shift)+
		where+` GROUP BY 1 ORDER BY 1 LIMIT 366`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		var d DailyKind
		if err := rows.Scan(&n, &d.MovieMS, &d.EpisodeMS, &d.LiveMS); err != nil {
			return err
		}
		d.Label = day(n)
		out.DailyKinds = append(out.DailyKinds, d)
	}
	return rows.Err()
}
