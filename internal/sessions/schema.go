// Package sessions owns protocol-independent playback sessions.
package sessions

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.RegisterAfter("sessions", "plex")
	// Keep the historical Plex migration unchanged, including play queues.

	store.Register("plex", 2, `
CREATE TABLE plex_sessions (
	id          TEXT    PRIMARY KEY,
	node_id     TEXT    NOT NULL,
	user_id     INTEGER NOT NULL,
	session_key INTEGER NOT NULL,
	data        TEXT    NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX plex_sessions_node ON plex_sessions(node_id);
CREATE TABLE plex_counters (
	name  TEXT    PRIMARY KEY,
	value INTEGER NOT NULL
);
INSERT INTO plex_counters(name, value) VALUES ('session_key', 0);
INSERT INTO plex_counters(name, value) VALUES ('playqueue_item', 0);
CREATE TABLE plex_playqueues (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	rev        INTEGER NOT NULL,
	data       TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX plex_playqueues_user ON plex_playqueues(user_id);
CREATE INDEX plex_playqueues_updated ON plex_playqueues(updated_at);
`)
	// Historical names, protocol-neutral content. Old nodes use the original
	// columns during a rolling upgrade; standalone brokers can create them too.
	store.Register("sessions", 1, `
ALTER TABLE plex_sessions ADD COLUMN protocol TEXT;
ALTER TABLE plex_sessions ADD COLUMN surface TEXT;
ALTER TABLE plex_sessions ADD COLUMN device_id TEXT;
ALTER TABLE plex_sessions ADD COLUMN device_data TEXT;
`)
	store.Register("sessions", 2, `ALTER TABLE plex_sessions ADD COLUMN report_sequence TEXT;`)
	store.Register("sessions", 3, `CREATE TABLE sessions_devices (
		viewer_id BIGINT NOT NULL,
		protocol TEXT NOT NULL,
		device_id TEXT NOT NULL,
		capabilities TEXT NOT NULL,
		updated_at BIGINT NOT NULL,
		PRIMARY KEY (viewer_id, protocol, device_id)
	);`)
	store.Register("sessions", 4, `
ALTER TABLE plex_sessions ADD COLUMN active INTEGER NOT NULL DEFAULT 1;
UPDATE plex_sessions SET active=0
WHERE protocol IS NOT NULL AND protocol<>'plex' AND surface IS NOT NULL AND device_id IS NOT NULL
AND EXISTS (SELECT 1 FROM plex_sessions newer WHERE newer.user_id=plex_sessions.user_id
 AND newer.surface=plex_sessions.surface AND newer.device_id=plex_sessions.device_id
 AND newer.protocol IS NOT NULL AND newer.protocol<>'plex'
 AND (newer.session_key>plex_sessions.session_key OR (newer.session_key=plex_sessions.session_key AND newer.id>plex_sessions.id)));
CREATE UNIQUE INDEX sessions_active_device ON plex_sessions(user_id,surface,device_id)
 WHERE active=1 AND protocol IS NOT NULL AND protocol<>'plex' AND surface IS NOT NULL AND device_id IS NOT NULL;
`)
	store.Register("sessions", 5, `CREATE TABLE sessions_lazy_streams (
		hash TEXT PRIMARY KEY,
		protocol TEXT NOT NULL,
		viewer_id BIGINT NOT NULL,
		grant_reference TEXT NOT NULL,
		request TEXT NOT NULL,
		expires BIGINT NOT NULL
	); CREATE INDEX sessions_lazy_streams_expiry ON sessions_lazy_streams(expires);`)

	store.Register("sessions", 6, `CREATE TABLE sessions_version_handles (
		handle TEXT PRIMARY KEY,
		viewer_id BIGINT NOT NULL,
		surface TEXT NOT NULL,
		device_id TEXT NOT NULL,
		item_id BIGINT NOT NULL,
		release_id TEXT NOT NULL,
		UNIQUE(viewer_id,surface,device_id,item_id,release_id)
	);`)
	store.Register("sessions", 7, `CREATE TABLE sessions_detail_sources (
		handle TEXT PRIMARY KEY,
		viewer_id BIGINT NOT NULL,
		surface TEXT NOT NULL,
		device_id TEXT NOT NULL,
		item_id BIGINT NOT NULL,
		release_id TEXT NOT NULL,
		revision TEXT NOT NULL,
		expires BIGINT NOT NULL,
		UNIQUE(viewer_id,surface,device_id,item_id,revision)
	); CREATE INDEX sessions_detail_sources_expiry ON sessions_detail_sources(expires);`)

}
