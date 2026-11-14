package plugins

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/textclip"
)

func init() {
	// Host-owned digests are separate from plugin KV quotas and cannot be forged by plugins.
	store.Register("plugins", 6, `CREATE TABLE plugins_hash_cache(plugin TEXT NOT NULL,kkey TEXT NOT NULL,value BLOB NOT NULL,PRIMARY KEY(plugin,kkey));`)

	// v4: settled journal rows (undone, or shadow records that were never
	// applied) are the only ones retention removes; this partial index
	// finds them by age without walking the live, undoable history.
	store.Register("plugins", 4, `CREATE INDEX plugins_journal_settled ON plugins_journal(created) WHERE undone=1 OR shadow=1;`)
	// v3: ranking steps left out because the plugin ran out of time.
	store.Register("plugins", 3, `ALTER TABLE plugins_health ADD COLUMN ranking_skipped INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plugins_health ADD COLUMN ranking_skipped_at INTEGER NOT NULL DEFAULT 0;`)
	// v2: the roles an owner switched off on an installed add-on.
	store.Register("plugins", 2, `ALTER TABLE plugins_installed ADD COLUMN roles_off TEXT NOT NULL DEFAULT '';`)
	store.Register("plugins", 1, `
CREATE TABLE plugins_installed(id TEXT PRIMARY KEY,name TEXT NOT NULL,version TEXT NOT NULL,kind TEXT NOT NULL DEFAULT '',manifest TEXT NOT NULL,source_kind TEXT NOT NULL DEFAULT '',source_url TEXT NOT NULL DEFAULT '',enabled INTEGER NOT NULL DEFAULT 1,verified INTEGER NOT NULL DEFAULT 0,shadow INTEGER NOT NULL DEFAULT 0,auto_update INTEGER NOT NULL DEFAULT 0,update_version TEXT NOT NULL DEFAULT '',update_manifest TEXT NOT NULL DEFAULT '',update_files TEXT NOT NULL DEFAULT '',prev_version TEXT NOT NULL DEFAULT '',installed_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE plugins_artifacts(plugin TEXT NOT NULL,version TEXT NOT NULL,path TEXT NOT NULL,content BLOB NOT NULL,digest TEXT NOT NULL DEFAULT '',created INTEGER NOT NULL,PRIMARY KEY(plugin,version,path));
CREATE TABLE plugins_grants(plugin TEXT PRIMARY KEY,grants TEXT NOT NULL);
CREATE TABLE plugins_settings(plugin TEXT PRIMARY KEY,settings TEXT NOT NULL DEFAULT '{}',secrets TEXT NOT NULL DEFAULT '{}',strings TEXT NOT NULL DEFAULT '{}',schema_doc TEXT NOT NULL DEFAULT '');
CREATE TABLE plugins_kv(plugin TEXT NOT NULL,kkey TEXT NOT NULL,value BLOB NOT NULL,size INTEGER NOT NULL,PRIMARY KEY(plugin,kkey));
CREATE TABLE plugins_health(plugin TEXT PRIMARY KEY,status TEXT NOT NULL DEFAULT 'ok',reason TEXT NOT NULL DEFAULT '',failures TEXT NOT NULL DEFAULT '[]',restarts INTEGER NOT NULL DEFAULT 0,slow INTEGER NOT NULL DEFAULT 0,drops INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',last_ok INTEGER NOT NULL DEFAULT 0,next_retry INTEGER NOT NULL DEFAULT 0,observed TEXT NOT NULL DEFAULT '{}',egress_bytes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE plugins_outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,plugin TEXT NOT NULL,event_id TEXT NOT NULL,name TEXT NOT NULL,payload TEXT NOT NULL,created INTEGER NOT NULL);
CREATE INDEX plugins_outbox_plugin ON plugins_outbox(plugin,id);
CREATE TABLE plugins_event_log(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,payload TEXT NOT NULL,created INTEGER NOT NULL);
CREATE INDEX plugins_event_log_created ON plugins_event_log(created);
CREATE TABLE plugins_journal(id INTEGER PRIMARY KEY AUTOINCREMENT,plugin TEXT NOT NULL,op TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',inverse TEXT NOT NULL DEFAULT '{}',shadow INTEGER NOT NULL DEFAULT 0,undone INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL);
CREATE INDEX plugins_journal_plugin ON plugins_journal(plugin,id);
CREATE TABLE plugins_calls(id INTEGER PRIMARY KEY AUTOINCREMENT,plugin TEXT NOT NULL,route TEXT NOT NULL,method TEXT NOT NULL,status INTEGER NOT NULL,ms INTEGER NOT NULL,error TEXT NOT NULL DEFAULT '',request TEXT NOT NULL DEFAULT '',response TEXT NOT NULL DEFAULT '',created INTEGER NOT NULL);
CREATE INDEX plugins_calls_plugin ON plugins_calls(plugin,id);
CREATE TABLE plugins_logs(id INTEGER PRIMARY KEY AUTOINCREMENT,plugin TEXT NOT NULL,line TEXT NOT NULL,created INTEGER NOT NULL);
CREATE INDEX plugins_logs_plugin ON plugins_logs(plugin,id);
`)
}

// Limits from SPEC §5.1.5, §5.7 and §5.13.
const (
	KVQuotaBytes   = 50 << 20
	OutboxCap      = 1000
	CallsCap       = 200
	LogsCap        = 200
	EventLogMaxAge = 7 * 24 * time.Hour
	// JournalSettledMaxAge is how long an undone or shadow journal entry
	// (one undo can no longer act on) stays in the activity history.
	JournalSettledMaxAge = 90 * 24 * time.Hour
	MaxFetchBytes        = wasm.MaxFetchBodyBytes
)

// Grants is what the owner has granted one plugin on top of its manifest
// permissions. Revoked lists permission keys the owner has taken back
// ("network", "catalog", "viewers.identity", "viewers.watch_history",
// "viewers.write_watch_state", "collections", "folders", "secrets",
// "events").
type Grants struct {
	Viewers []int64  `json:"viewers,omitempty"`
	Folders []string `json:"folders,omitempty"`
	Revoked []string `json:"revoked,omitempty"`
	LAN     bool     `json:"lan,omitempty"`
}

func (g Grants) revoked(key string) bool {
	for _, r := range g.Revoked {
		if r == key {
			return true
		}
	}
	return false
}

func (g Grants) viewerGranted(id int64) bool {
	for _, v := range g.Viewers {
		if v == id {
			return true
		}
	}
	return false
}

// Record is one installed plugin as stored.
type Record struct {
	ID             string
	Name           string
	Version        string
	Kind           string
	Manifest       Manifest
	ManifestRaw    json.RawMessage
	SourceKind     string // url|github|pkg|studio|catalogue
	SourceURL      string
	Enabled        bool
	Verified       bool
	Shadow         bool
	AutoUpdate     bool
	UpdateVersion  string
	updateManifest string
	updateFiles    string
	PrevVersion    string
	InstalledAt    time.Time
	UpdatedAt      time.Time
	// RolesOff are the capabilities the owner switched off (a Stremio
	// add-on's catalogs, streams, subtitles…). Manifest already reflects
	// them: EffectiveEther leaves them out.
	RolesOff []Capability
}

// Health is one plugin's health and impact record.
type Health struct {
	Status      string         `json:"status"` // ok|attention|error|disabled
	Reason      string         `json:"reason,omitempty"`
	Failures    []int64        `json:"-"` // unix seconds, pruned to 10 minutes
	Restarts    int64          `json:"restarts"`
	Slow        int64          `json:"slow"`
	Drops       int64          `json:"drops"`
	LastError   string         `json:"lastError,omitempty"`
	LastOK      time.Time      `json:"lastOk,omitempty"`
	NextRetry   time.Time      `json:"nextRetry,omitempty"`
	Observed    map[string]int `json:"observed,omitempty"` // host -> fetch count
	EgressBytes int64          `json:"egressBytes"`
	// RankingSkipped counts the resolves since the plugin's last complete
	// ranking that left its ranking step out because it ran past its time
	// budget; zero once a ranking finishes in time again.
	RankingSkipped   int64     `json:"rankingSkipped"`
	RankingSkippedAt time.Time `json:"rankingSkippedAt,omitempty"`
	// Sidecar is "waiting" while a plugin that runs as its own program
	// (a remote plugin with no URL endpoint) has no sidecar connected.
	// It is computed live for the console, never stored.
	Sidecar string `json:"sidecar,omitempty"`
}

// JournalEntry is one recorded write and its inverse.
type JournalEntry struct {
	ID      int64           `json:"id"`
	Plugin  string          `json:"plugin"`
	Op      string          `json:"op"`
	Detail  string          `json:"detail,omitempty"`
	Inverse json.RawMessage `json:"inverse,omitempty"`
	Shadow  bool            `json:"shadow"`
	Undone  bool            `json:"undone"`
	Created time.Time       `json:"created"`
}

// CallRecord is one inspector entry (redacted, capped per plugin).
type CallRecord struct {
	ID       int64     `json:"id"`
	Plugin   string    `json:"plugin"`
	Route    string    `json:"route"`
	Method   string    `json:"method"`
	Status   int       `json:"status"`
	MS       int64     `json:"ms"`
	Error    string    `json:"error,omitempty"`
	Request  string    `json:"request,omitempty"`
	Response string    `json:"response,omitempty"`
	Created  time.Time `json:"created"`
}

type storage struct {
	db     *sql.DB
	sealer config.FieldSealer
	now    func() time.Time
}

func (s *storage) record(ctx context.Context, id string) (Record, error) {
	var r Record
	var man, updMan, updFiles string
	var en, ver, sh, au int
	var inst, upd int64
	var rolesOff string
	err := s.db.QueryRowContext(ctx, `SELECT id,name,version,kind,manifest,source_kind,source_url,enabled,verified,shadow,auto_update,update_version,update_manifest,update_files,prev_version,installed_at,updated_at,roles_off FROM plugins_installed WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Version, &r.Kind, &man, &r.SourceKind, &r.SourceURL, &en, &ver, &sh, &au, &r.UpdateVersion, &updMan, &updFiles, &r.PrevVersion, &inst, &upd, &rolesOff)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	if r.SourceURL != "" {
		plain, legacy, err := config.OpenWith(s.sealer, r.SourceURL, "plugins_installed", "source_url", r.ID)
		if err != nil {
			return Record{}, err
		}
		if legacy && s.sealer != nil {
			sealed, err := config.SealWith(s.sealer, plain, "plugins_installed", "source_url", r.ID)
			if err != nil {
				return Record{}, err
			}
			if sealed != r.SourceURL {
				if _, err = s.db.ExecContext(ctx, `UPDATE plugins_installed SET source_url=? WHERE id=? AND source_url=?`, sealed, r.ID, r.SourceURL); err != nil {
					return Record{}, err
				}
			}
		}
		r.SourceURL = plain
	}
	r.ManifestRaw = json.RawMessage(man)
	if err := json.Unmarshal([]byte(man), &r.Manifest); err != nil {
		return Record{}, fmt.Errorf("plugins: stored manifest for %s: %w", id, err)
	}
	// A record's manifest id is its record id: a Stremio add-on keeps its
	// own id in the stored document but is installed per configuration.
	r.Manifest.ID = r.ID
	for _, c := range strings.Split(rolesOff, ",") {
		if c = strings.TrimSpace(c); c != "" {
			r.RolesOff = append(r.RolesOff, Capability(c))
		}
	}
	r.Manifest.rolesOff = r.RolesOff
	r.Enabled, r.Verified, r.Shadow, r.AutoUpdate = en == 1, ver == 1, sh == 1, au == 1
	r.updateManifest, r.updateFiles = updMan, updFiles
	r.InstalledAt, r.UpdatedAt = time.Unix(inst, 0).UTC(), time.Unix(upd, 0).UTC()
	return r, nil
}

func (s *storage) list(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM plugins_installed ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		r, err := s.record(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *storage) upsert(ctx context.Context, r Record) error {
	now := s.now().Unix()
	sealedURL, err := config.SealWith(s.sealer, r.SourceURL, "plugins_installed", "source_url", r.ID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plugins_installed(id,name,version,kind,manifest,source_kind,source_url,enabled,verified,shadow,auto_update,update_version,update_manifest,update_files,prev_version,installed_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,version=excluded.version,kind=excluded.kind,manifest=excluded.manifest,source_kind=excluded.source_kind,source_url=excluded.source_url,enabled=excluded.enabled,verified=excluded.verified,shadow=excluded.shadow,auto_update=excluded.auto_update,update_version=excluded.update_version,update_manifest=excluded.update_manifest,update_files=excluded.update_files,prev_version=excluded.prev_version,updated_at=excluded.updated_at`,
		r.ID, r.Name, r.Version, r.Kind, string(r.ManifestRaw), r.SourceKind, sealedURL,
		bi(r.Enabled), bi(r.Verified), bi(r.Shadow), bi(r.AutoUpdate), r.UpdateVersion, r.updateManifest, r.updateFiles, r.PrevVersion, timeOr(r.InstalledAt, now), now)
	return err
}

func (s *storage) setRolesOff(ctx context.Context, id string, off []Capability) error {
	names := make([]string, 0, len(off))
	for _, c := range off {
		names = append(names, string(c))
	}
	sort.Strings(names)
	res, err := s.db.ExecContext(ctx, `UPDATE plugins_installed SET roles_off=?, updated_at=? WHERE id=?`, strings.Join(names, ","), s.now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func timeOr(t time.Time, unix int64) int64 {
	if t.IsZero() {
		return unix
	}
	return t.Unix()
}

func bi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// putFiles stores a version's artifact files (manifest, code, strings).
func (s *storage) putFiles(ctx context.Context, plugin, version string, files map[string][]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for path, content := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugins_artifacts(plugin,version,path,content,digest,created) VALUES(?,?,?,?,?,?) ON CONFLICT(plugin,version,path) DO UPDATE SET content=excluded.content,digest=excluded.digest`,
			plugin, version, path, content, digestOf(content), s.now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dropOtherVersions deletes a plugin's artifact files except the current
// and previous versions, the only two install, runtime and rollback read.
func (s *storage) dropOtherVersions(ctx context.Context, plugin, current, previous string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM plugins_artifacts WHERE plugin=? AND version<>? AND version<>?`, plugin, current, previous)
	return err
}

func (s *storage) file(ctx context.Context, plugin, version, path string) ([]byte, bool) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM plugins_artifacts WHERE plugin=? AND version=? AND path=?`, plugin, version, path).Scan(&b)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (s *storage) grants(ctx context.Context, plugin string) (Grants, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT grants FROM plugins_grants WHERE plugin=?`, plugin).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Grants{}, nil
	}
	if err != nil {
		return Grants{}, err
	}
	var g Grants
	return g, json.Unmarshal([]byte(raw), &g)
}

func (s *storage) putGrants(ctx context.Context, plugin string, g Grants) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plugins_grants(plugin,grants) VALUES(?,?) ON CONFLICT(plugin) DO UPDATE SET grants=excluded.grants`, plugin, string(raw))
	return err
}

type settingsRow struct {
	Settings map[string]any
	Secrets  map[string]string // name -> sealed value
	Strings  map[string]map[string]string
	Schema   json.RawMessage
}

func (s *storage) settings(ctx context.Context, plugin string) (settingsRow, error) {
	var set, sec, str, schema string
	err := s.db.QueryRowContext(ctx, `SELECT settings,secrets,strings,schema_doc FROM plugins_settings WHERE plugin=?`, plugin).Scan(&set, &sec, &str, &schema)
	if errors.Is(err, sql.ErrNoRows) {
		return settingsRow{Settings: map[string]any{}, Secrets: map[string]string{}, Strings: map[string]map[string]string{}}, nil
	}
	if err != nil {
		return settingsRow{}, err
	}
	out := settingsRow{}
	if err := json.Unmarshal([]byte(set), &out.Settings); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(sec), &out.Secrets); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(str), &out.Strings); err != nil {
		return out, err
	}
	if out.Settings == nil {
		out.Settings = map[string]any{}
	}
	if out.Secrets == nil {
		out.Secrets = map[string]string{}
	}
	if out.Strings == nil {
		out.Strings = map[string]map[string]string{}
	}
	if schema != "" {
		out.Schema = json.RawMessage(schema)
	}
	return out, nil
}

func (s *storage) putSettings(ctx context.Context, plugin string, row settingsRow) error {
	set, err := json.Marshal(row.Settings)
	if err != nil {
		return err
	}
	sec, err := json.Marshal(row.Secrets)
	if err != nil {
		return err
	}
	str, err := json.Marshal(row.Strings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plugins_settings(plugin,settings,secrets,strings,schema_doc) VALUES(?,?,?,?,?) ON CONFLICT(plugin) DO UPDATE SET settings=excluded.settings,secrets=excluded.secrets,strings=excluded.strings,schema_doc=excluded.schema_doc`,
		plugin, string(set), string(sec), string(str), string(row.Schema))
	return err
}

// sealContext binds a secret to its plugin: no other plugin can open it.
func sealContext(plugin, name string) []string {
	return []string{"plugins_settings", "secrets", plugin, name}
}

func (s *storage) sealSecret(plugin, name, plain string) (string, error) {
	return config.SealWith(s.sealer, plain, sealContext(plugin, name)...)
}

func (s *storage) openSecret(plugin, name, stored string) (string, error) {
	plain, _, err := config.OpenWith(s.sealer, stored, sealContext(plugin, name)...)
	return plain, err
}

// --- KV ---

func (s *storage) kvUsed(ctx context.Context, plugin string) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM plugins_kv WHERE plugin=?`, plugin).Scan(&n)
	return n.Int64, err
}

func (s *storage) kvGet(ctx context.Context, plugin, key string) ([]byte, bool, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM plugins_kv WHERE plugin=? AND kkey=?`, plugin, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (s *storage) kvPut(ctx context.Context, plugin, key string, value []byte) error {
	if len(key) == 0 || len(key) > 256 {
		return ErrQuotaExceeded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the plugin grant row across nodes before reading and changing usage.
	result, err := tx.ExecContext(ctx, `UPDATE plugins_grants SET grants=grants WHERE plugin=?`, plugin)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrPermissionDenied
	}
	var old int64
	err = tx.QueryRowContext(ctx, `SELECT size FROM plugins_kv WHERE plugin=? AND kkey=?`, plugin, key).Scan(&old)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var used int64
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0),COUNT(*) FROM plugins_kv WHERE plugin=?`, plugin).Scan(&used, &count); err != nil {
		return err
	}
	// A fixed key allowance also bounds Unicode keys on SQLite and PostgreSQL.
	if !exists {
		count++
	}
	if count > 10000 || used-old+int64(len(value))+count*256 > KVQuotaBytes {
		return ErrQuotaExceeded
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugins_kv(plugin,kkey,value,size) VALUES(?,?,?,?) ON CONFLICT(plugin,kkey) DO UPDATE SET value=excluded.value,size=excluded.size`, plugin, key, value, len(value))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *storage) kvDelete(ctx context.Context, plugin, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM plugins_kv WHERE plugin=? AND kkey=?`, plugin, key)
	return err
}

// kvListLimit caps one prefix listing.
const kvListLimit = 10000

func (s *storage) kvList(ctx context.Context, plugin, prefix string) ([]string, error) {
	// The prefix is matched in the query, before the limit, so a plugin
	// with many keys still sees every key under the prefix it asks for
	// (up to kvListLimit; the per-plugin KV quota bounds the total).
	rows, err := s.db.QueryContext(ctx, `SELECT kkey FROM plugins_kv WHERE plugin=? AND substr(kkey, 1, ?)=? ORDER BY kkey LIMIT ?`,
		plugin, len(prefix), prefix, kvListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// --- Health ---

func (s *storage) health(ctx context.Context, plugin string) (Health, error) {
	var h Health
	var failures, observed string
	var lastOK, nextRetry, skippedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT status,reason,failures,restarts,slow,drops,last_error,last_ok,next_retry,observed,egress_bytes,ranking_skipped,ranking_skipped_at FROM plugins_health WHERE plugin=?`, plugin).
		Scan(&h.Status, &h.Reason, &failures, &h.Restarts, &h.Slow, &h.Drops, &h.LastError, &lastOK, &nextRetry, &observed, &h.EgressBytes, &h.RankingSkipped, &skippedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Health{Status: "ok", Observed: map[string]int{}}, nil
	}
	if err != nil {
		return Health{}, err
	}
	_ = json.Unmarshal([]byte(failures), &h.Failures)
	_ = json.Unmarshal([]byte(observed), &h.Observed)
	if h.Observed == nil {
		h.Observed = map[string]int{}
	}
	if lastOK > 0 {
		h.LastOK = time.Unix(lastOK, 0).UTC()
	}
	if nextRetry > 0 {
		h.NextRetry = time.Unix(nextRetry, 0).UTC()
	}
	if skippedAt > 0 {
		h.RankingSkippedAt = time.Unix(skippedAt, 0).UTC()
	}
	return h, nil
}

func (s *storage) putHealth(ctx context.Context, plugin string, h Health) error {
	failures, _ := json.Marshal(h.Failures)
	observed, _ := json.Marshal(h.Observed)
	_, err := s.db.ExecContext(ctx, `INSERT INTO plugins_health(plugin,status,reason,failures,restarts,slow,drops,last_error,last_ok,next_retry,observed,egress_bytes,ranking_skipped,ranking_skipped_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(plugin) DO UPDATE SET status=excluded.status,reason=excluded.reason,failures=excluded.failures,restarts=excluded.restarts,slow=excluded.slow,drops=excluded.drops,last_error=excluded.last_error,last_ok=excluded.last_ok,next_retry=excluded.next_retry,observed=excluded.observed,egress_bytes=excluded.egress_bytes,ranking_skipped=excluded.ranking_skipped,ranking_skipped_at=excluded.ranking_skipped_at`,
		plugin, h.Status, h.Reason, string(failures), h.Restarts, h.Slow, h.Drops, h.LastError, unixOr0(h.LastOK), unixOr0(h.NextRetry), string(observed), h.EgressBytes, h.RankingSkipped, unixOr0(h.RankingSkippedAt))
	return err
}

func unixOr0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// --- Journal, calls, logs, outbox ---

func (s *storage) journal(ctx context.Context, e JournalEntry) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO plugins_journal(plugin,op,detail,inverse,shadow,created) VALUES(?,?,?,?,?,?) RETURNING id`,
		e.Plugin, e.Op, e.Detail, string(orEmptyJSON(e.Inverse)), bi(e.Shadow), s.now().Unix()).Scan(&id)
	return id, err
}

func orEmptyJSON(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

func (s *storage) journalList(ctx context.Context, plugin string, limit int, liveOnly bool) ([]JournalEntry, error) {
	q := `SELECT id,plugin,op,detail,inverse,shadow,undone,created FROM plugins_journal WHERE plugin=?`
	if liveOnly {
		q += ` AND undone=0 AND shadow=0`
	}
	q += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, plugin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var inv string
		var sh, un int
		var created int64
		if err := rows.Scan(&e.ID, &e.Plugin, &e.Op, &e.Detail, &inv, &sh, &un, &created); err != nil {
			return nil, err
		}
		e.Inverse, e.Shadow, e.Undone = json.RawMessage(inv), sh == 1, un == 1
		e.Created = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *storage) markUndone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plugins_journal SET undone=1 WHERE id=?`, id)
	return err
}

// pruneJournal deletes settled journal entries older than
// JournalSettledMaxAge. Live entries (applied, not shadow, not undone) are
// what "Undo everything" and undo-to-here replay, so they are never pruned.
func (s *storage) pruneJournal(ctx context.Context) (int64, error) {
	return store.PruneBatched(ctx, s.db,
		`DELETE FROM plugins_journal WHERE id IN (SELECT id FROM plugins_journal WHERE (undone=1 OR shadow=1) AND created<? ORDER BY created LIMIT ?)`,
		s.now().Add(-JournalSettledMaxAge).Unix())
}

func capRing(ctx context.Context, db *sql.DB, table, plugin string, cap int) {
	_, _ = db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE plugin=? AND id NOT IN (SELECT id FROM %s WHERE plugin=? ORDER BY id DESC LIMIT ?)`, table, table), plugin, plugin, cap)
}

func (s *storage) addCall(ctx context.Context, c CallRecord) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO plugins_calls(plugin,route,method,status,ms,error,request,response,created) VALUES(?,?,?,?,?,?,?,?,?)`,
		c.Plugin, c.Route, c.Method, c.Status, c.MS, c.Error, truncate(c.Request, 8192), truncate(c.Response, 8192), s.now().Unix())
	capRing(ctx, s.db, "plugins_calls", c.Plugin, CallsCap)
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return textclip.Bytes(v, n)
}

func (s *storage) calls(ctx context.Context, plugin string, limit int) ([]CallRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,plugin,route,method,status,ms,error,request,response,created FROM plugins_calls WHERE plugin=? ORDER BY id DESC LIMIT ?`, plugin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CallRecord
	for rows.Next() {
		var c CallRecord
		var created int64
		if err := rows.Scan(&c.ID, &c.Plugin, &c.Route, &c.Method, &c.Status, &c.MS, &c.Error, &c.Request, &c.Response, &created); err != nil {
			return nil, err
		}
		c.Created = time.Unix(created, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *storage) callStats(ctx context.Context, plugin string) (count, errors int, p50, p95 int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ms,error,status FROM plugins_calls WHERE plugin=? ORDER BY id DESC LIMIT ?`, plugin, CallsCap)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()
	// Latency is how long answers took. A call that failed or ran out of
	// time is counted as a failure, never as a measurement of its deadline.
	var ms []int64
	for rows.Next() {
		var v int64
		var e string
		var status int
		if err := rows.Scan(&v, &e, &status); err != nil {
			return 0, 0, 0, 0, err
		}
		count++
		if e != "" || status == 0 || status >= 500 {
			errors++
			continue
		}
		ms = append(ms, v)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	pct := func(p float64) int64 {
		if len(ms) == 0 {
			return 0
		}
		i := int(p * float64(len(ms)-1))
		return ms[i]
	}
	return count, errors, pct(0.5), pct(0.95), nil
}

// redactKnown removes saved plugin credentials even when a plugin includes one
// in free text or under a harmless-looking field name. Fail closed if the
// settings cannot be read or decrypted.
func (s *storage) redactKnown(ctx context.Context, plugin, value string) string {
	row, err := s.settings(ctx, plugin)
	if err != nil {
		return "[redacted]"
	}
	// URL-mode add-ons commonly embed credentials in the source URL.
	// Protect the URL and its configuration components in diagnostics too.
	record, err := s.record(ctx, plugin)
	if err != nil {
		return "[redacted]"
	}
	if record.SourceURL != "" {
		value = strings.ReplaceAll(value, record.SourceURL, "[redacted]")
		if u, err := url.Parse(record.SourceURL); err == nil {
			for _, segment := range strings.Split(u.Path, "/") {
				if len(segment) >= 4 && segment != "manifest.json" {
					value = strings.ReplaceAll(value, segment, "[redacted]")
				}
			}
			for _, values := range u.Query() {
				for _, secret := range values {
					if len(secret) >= 4 {
						value = strings.ReplaceAll(value, secret, "[redacted]")
					}
				}
			}
		}
	}
	for name, sealed := range row.Secrets {
		secret, err := s.openSecret(plugin, name, sealed)
		if err != nil {
			return "[redacted]"
		}
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "[redacted]")
		encoded, _ := json.Marshal(secret)
		if len(encoded) > 2 {
			value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "[redacted]")
		}
	}
	return logredact.String(value)
}

func (s *storage) addLog(ctx context.Context, plugin, line string) {
	line = s.redactKnown(ctx, plugin, line)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO plugins_logs(plugin,line,created) VALUES(?,?,?)`, plugin, truncate(line, 2000), s.now().Unix())
	capRing(ctx, s.db, "plugins_logs", plugin, LogsCap)
}

func (s *storage) logs(ctx context.Context, plugin string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT line FROM plugins_logs WHERE plugin=? ORDER BY id DESC LIMIT ?`, plugin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// enqueue appends to a plugin outbox, dropping the oldest past the cap and
// reporting how many were dropped.
func (s *storage) enqueue(ctx context.Context, plugin string, ev Event) (int, error) {
	payload, _ := json.Marshal(ev)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO plugins_outbox(plugin,event_id,name,payload,created) VALUES(?,?,?,?,?)`, plugin, ev.ID, ev.Name, string(payload), s.now().Unix()); err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugins_outbox WHERE plugin=?`, plugin).Scan(&n); err != nil {
		return 0, err
	}
	if n <= OutboxCap {
		return 0, nil
	}
	drop := n - OutboxCap
	_, err := s.db.ExecContext(ctx, `DELETE FROM plugins_outbox WHERE id IN (SELECT id FROM plugins_outbox WHERE plugin=? ORDER BY id LIMIT ?)`, plugin, drop)
	return drop, err
}

func (s *storage) outboxBatch(ctx context.Context, plugin string, limit int) ([]Event, []int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload FROM plugins_outbox WHERE plugin=? ORDER BY id LIMIT ?`, plugin, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var evs []Event
	var ids []int64
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, nil, err
		}
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		evs = append(evs, ev)
		ids = append(ids, id)
	}
	return evs, ids, rows.Err()
}

func (s *storage) ackOutbox(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM plugins_outbox WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *storage) logEvent(ctx context.Context, ev Event) {
	payload, _ := json.Marshal(ev)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO plugins_event_log(name,payload,created) VALUES(?,?,?)`, ev.Name, string(payload), s.now().Unix())
	_, _ = s.db.ExecContext(ctx, `DELETE FROM plugins_event_log WHERE created<?`, s.now().Add(-EventLogMaxAge).Unix())
}

// purge removes every row a plugin owns.
func (s *storage) purge(ctx context.Context, plugin string) error {
	for _, q := range []string{
		`DELETE FROM plugins_installed WHERE id=?`,
		`DELETE FROM plugins_artifacts WHERE plugin=?`,
		`DELETE FROM onair_feed_values WHERE plugin=?`,
		`DELETE FROM plugins_grants WHERE plugin=?`,
		`DELETE FROM plugins_settings WHERE plugin=?`,
		`DELETE FROM plugins_setup WHERE plugin=?`,
		`DELETE FROM plugins_kv WHERE plugin=?`,
		`DELETE FROM plugins_hash_cache WHERE plugin=?`,
		`DELETE FROM plugins_health WHERE plugin=?`,
		`DELETE FROM plugins_outbox WHERE plugin=?`,
		`DELETE FROM plugins_proposals WHERE plugin=?`,
		`DELETE FROM plugins_journal WHERE plugin=?`,
		`DELETE FROM plugins_calls WHERE plugin=?`,
		`DELETE FROM plugins_logs WHERE plugin=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, plugin); err != nil {
			return err
		}
	}
	return nil
}

// files lists a version's artifact files (manifest, code, strings).
func (s *storage) files(ctx context.Context, plugin, version string) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path,content FROM plugins_artifacts WHERE plugin=? AND version=?`, plugin, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[string][]byte{}
	for rows.Next() {
		var path string
		var b []byte
		if err := rows.Scan(&path, &b); err != nil {
			return nil, err
		}
		out[path] = b
	}
	return out, rows.Err()
}
