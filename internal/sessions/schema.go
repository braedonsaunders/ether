// Package sessions owns protocol-independent playback sessions.
package sessions

import "github.com/braedonsaunders/ether/internal/store"

func init() {
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
}
