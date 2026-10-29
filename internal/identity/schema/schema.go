package schema

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.Register("identity_links", 1, `
CREATE TABLE identity_links (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 provider TEXT NOT NULL,
 subject TEXT NOT NULL,
 display TEXT NOT NULL DEFAULT '',
 verified_at INTEGER NOT NULL DEFAULT 0,
 password_hash TEXT NOT NULL DEFAULT '',
 UNIQUE(provider, subject)
);
CREATE INDEX identity_links_viewer ON identity_links(viewer_id);
`)
}
