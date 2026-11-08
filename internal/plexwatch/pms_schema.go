package plexwatch

import "github.com/braedonsaunders/ether/internal/store"

// Server-to-server sync with Plex Media Servers the owner runs. These tables
// are separate from the per-viewer account path (plexwatch_account and
// friends).
func init() {
	store.Register("plexwatch_pms", 1, `
CREATE TABLE plexwatch_pms (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  token TEXT NOT NULL,
  machine_id TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '',
  owner_name TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL DEFAULT 0,
  problem TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX plexwatch_pms_machine ON plexwatch_pms(machine_id);
CREATE TABLE plexwatch_pms_viewer (
  server_id INTEGER NOT NULL,
  account_id INTEGER NOT NULL,
  account_name TEXT NOT NULL DEFAULT '',
  owner INTEGER NOT NULL DEFAULT 0,
  user_id INTEGER NOT NULL,
  token TEXT NOT NULL DEFAULT '',
  direction INTEGER NOT NULL DEFAULT 0,
  interval_min INTEGER NOT NULL DEFAULT 10,
  in_cursor INTEGER NOT NULL DEFAULT 0,
  in_skip INTEGER NOT NULL DEFAULT 0,
  out_cursor INTEGER NOT NULL DEFAULT 0,
  out_key INTEGER NOT NULL DEFAULT 0,
  out_baseline INTEGER NOT NULL DEFAULT 0,
  next_run INTEGER NOT NULL DEFAULT 0,
  next_allowed INTEGER NOT NULL DEFAULT 0,
  lease INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  problem TEXT NOT NULL DEFAULT '',
  last_import INTEGER NOT NULL DEFAULT 0,
  last_send INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, account_id)
);
CREATE UNIQUE INDEX plexwatch_pms_viewer_user ON plexwatch_pms_viewer(server_id, user_id);
CREATE INDEX plexwatch_pms_viewer_due ON plexwatch_pms_viewer(next_run);
CREATE TABLE plexwatch_pms_item (
  server_id INTEGER NOT NULL,
  pms_key INTEGER NOT NULL,
  rating_key INTEGER NOT NULL DEFAULT 0,
  checked_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, pms_key)
);
CREATE INDEX plexwatch_pms_item_local ON plexwatch_pms_item(server_id, rating_key);
CREATE TABLE plexwatch_pms_sent (
  server_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  pms_key INTEGER NOT NULL DEFAULT 0,
  remote_watched INTEGER NOT NULL DEFAULT 0,
  local_watched INTEGER NOT NULL DEFAULT 0,
  local_at INTEGER NOT NULL DEFAULT 0,
  offset_ms INTEGER NOT NULL DEFAULT 0,
  sent_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, user_id, rating_key)
);
CREATE TABLE plexwatch_pms_unmatched (
  server_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  dir TEXT NOT NULL,
  ref TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL,
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (server_id, user_id, dir, ref)
);`)
}
