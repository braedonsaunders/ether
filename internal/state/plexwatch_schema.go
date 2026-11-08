package state

import "github.com/braedonsaunders/ether/internal/store"

// Component state_plexwatch adds account-sync clocks and intents to state_watch.
// Its migrations depend on the real state component, including the playlist
// markers and last-source migrations. Account tables belong to plexwatch.
func init() {
	store.RegisterAfter("state_plexwatch", "state")
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
	// Repair clocks left by pre-clock writers once, rather than keeping a
	// legacy-reader fallback in every remote import.
	store.Register("state_plexwatch", 5, `
 UPDATE state_watch SET
 rating_at=CASE WHEN updated_at>watch_at AND updated_at>rating_at THEN updated_at ELSE rating_at END,
 watch_at=CASE WHEN last_viewed_at>watch_at THEN last_viewed_at ELSE watch_at END;
 `)
	// never_sent records whether a journal row has never been handed to a
	// network POST. Rows that were never attempted stay normal queued work;
	// previously attempted or claimed rows lack durable never-sent proof.
	store.Register("state_plexwatch", 4, `
ALTER TABLE state_sync_journal ADD COLUMN never_sent INTEGER NOT NULL DEFAULT 0;
UPDATE state_sync_journal SET never_sent=1 WHERE delivered_at=0 AND attempts=0 AND claimed_by='' AND claimed_at=0 AND last_error='';
`)
	// Resume positions are a third aspect ("offset"). A row carries the
	// resume point in milliseconds (0 clears it) and the duration Ether
	// knew for the title, which Plex's timeline call requires.
	store.Register("state_plexwatch", 6, `
ALTER TABLE state_sync_journal ADD COLUMN offset_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_sync_journal ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
`)
}
