package catalog

import (
	"database/sql"

	"github.com/braedonsaunders/ether/internal/store"
)

// knownReleaseSQL is the predicate for a stored release instant.
// released_ts is Unix seconds. Zero means unknown. Any other value, including
// a negative instant before 1970, is known. release_known is generated from
// that sentinel so queries and partial indexes share one spelling.
//
// Unaliased. Qualify it (t.release_known=1) when the table has an alias.
// "Already aired" is knownReleaseSQL plus released_ts<=?. released_ts>0 is
// not that test. catalog_titles_episode_recent, the newest-first walk, and
// the no-episode series fallback must use this same predicate.
const knownReleaseSQL = `release_known=1`

// sqliteIndexedBy pins a SQLite query to index. PostgreSQL has no INDEXED BY
// clause; the same predicate selects the index there.
func sqliteIndexedBy(db *sql.DB, index string) string {
	if db == nil || store.DialectOf(db) == store.Postgres {
		return ""
	}
	return "INDEXED BY " + index + " "
}
