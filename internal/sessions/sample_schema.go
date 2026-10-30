package sessions

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.Register("sessions_samples", 1, `CREATE TABLE sessions_sample_receipts(session_id TEXT PRIMARY KEY,installation_id TEXT NOT NULL,sequence TEXT NOT NULL,fingerprint TEXT NOT NULL);`)
}
