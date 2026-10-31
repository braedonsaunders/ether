package state

import "github.com/braedonsaunders/ether/internal/store"

// Component state_plexwatch holds the account-sync additions to watch state.
// The origin/prism tip fetched in this worktree on 2026-10-30 was
// b4ef791d53a68741c0f1513071328594b62eb9ed. That commit registers state/3
// (state_playlist_markers) and state/4 (state_last_source,
// state_source_sequence). It is the snapshot this integration check read,
// not a claim about a newer coordinator head. Those version numbers stay
// Prism's. state/1 and state/2 stay the watch tables.
//
// Migrations apply by component name, then version. This tree has no
// RegisterAfter. state_plexwatch sorts after state, so Prism's state/3 and
// state/4 run before these statements when both are registered.
// plexwatch_state would sort before state, and the clock ALTER would run
// before state_watch exists. Account tables stay on component plexwatch.
func init() {
	// Durable intents share the watch-state transaction. A subscriber that
	// only listens in memory can miss a commit that crashes before the
	// callback runs. Rows here are the source for account sync.
	store.Register("state_plexwatch", 1, `
CREATE TABLE state_sync_journal (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  aspect TEXT NOT NULL,
  kind TEXT NOT NULL,
  rating REAL NOT NULL DEFAULT 0,
  occurred_at INTEGER NOT NULL,
  bulk INTEGER NOT NULL DEFAULT 0,
  superseded INTEGER NOT NULL DEFAULT 0,
  claimed_by TEXT NOT NULL DEFAULT '',
  claimed_at INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER NOT NULL DEFAULT 0,
  dead INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX state_sync_journal_live ON state_sync_journal(dead, superseded, delivered_at, next_attempt, seq);
CREATE TABLE state_sync_lock (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  aspect TEXT NOT NULL,
  holder TEXT NOT NULL DEFAULT '',
  until_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key, aspect)
);`)
	// Watch marks and ratings move on different clocks. A newer rating must
	// not make an older watched mark look newer, or the reverse.
	store.Register("state_plexwatch", 2, `
ALTER TABLE state_watch ADD COLUMN watch_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_watch ADD COLUMN rating_at INTEGER NOT NULL DEFAULT 0;
UPDATE state_watch SET watch_at=last_viewed_at WHERE watch_at=0 AND last_viewed_at<>0;
UPDATE state_watch SET rating_at=updated_at WHERE rating_at=0 AND user_rating<>0;`)
	// consent_generation is the account fence generation observed when the
	// intent was admitted. A later generation must not deliver it.
	store.Register("state_plexwatch", 3, `
ALTER TABLE state_sync_journal ADD COLUMN consent_generation INTEGER NOT NULL DEFAULT 0;`)
	// Legacy undelivered intents have no durable evidence that a request was
	// never transmitted. Retain their holds separately from compactable work.
	store.Register("state_plexwatch", 4, `
ALTER TABLE state_sync_journal ADD COLUMN never_sent INTEGER NOT NULL DEFAULT 0;
`)

}
