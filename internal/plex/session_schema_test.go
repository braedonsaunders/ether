package plex

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestMixedVersionSessionRowsAndCounter(t *testing.T) {
	db := store.OpenTest(t)
	// An old node writes only the original columns. A broker can read the
	// nullable new columns and treat the missing protocol as Plex.
	if _, err := db.Exec(`INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at) VALUES ('old','a',1,7,'{}',1)`); err != nil {
		t.Fatal(err)
	}
	var protocol string
	var key int
	if err := db.QueryRow(`SELECT COALESCE(protocol,'plex'),session_key FROM plex_sessions WHERE id='old'`).Scan(&protocol, &key); err != nil {
		t.Fatal(err)
	}
	if protocol != "plex" || key != 7 {
		t.Fatalf("old row: %q %d", protocol, key)
	}
	// The old node reads a newer row without knowing any new columns.
	if _, err := db.Exec(`INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at,protocol,surface,device_id,device_data) VALUES ('new','b',2,8,'{}',2,'jellyfin','jellyfin','tv','{}')`); err != nil {
		t.Fatal(err)
	}
	var id, node, data string
	var user, updated int
	if err := db.QueryRow(`SELECT id,node_id,user_id,session_key,data,updated_at FROM plex_sessions WHERE id='new'`).Scan(&id, &node, &user, &key, &data, &updated); err != nil {
		t.Fatal(err)
	}
	if id != "new" || node != "b" || user != 2 || key != 8 || data != "{}" || updated != 2 {
		t.Fatalf("new row: %s %s %d %d %s %d", id, node, user, key, data, updated)
	}
	if _, err := db.Exec(`INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at) VALUES ('new','c',2,8,'{}',3)
ON CONFLICT(id) DO UPDATE SET node_id=excluded.node_id,user_id=excluded.user_id,session_key=excluded.session_key,data=excluded.data,updated_at=excluded.updated_at`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT protocol FROM plex_sessions WHERE id='new'`).Scan(&protocol); err != nil || protocol != "jellyfin" {
		t.Fatalf("old update lost protocol: %q, %v", protocol, err)
	}
	for want := 1; want <= 2; want++ {
		if err := db.QueryRow(`UPDATE plex_counters SET value=value+1 WHERE name='session_key' RETURNING value`).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if key != want {
			t.Fatalf("counter = %d, want %d", key, want)
		}
	}
}
