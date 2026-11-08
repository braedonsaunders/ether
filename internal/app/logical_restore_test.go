package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

// A logical restore must leave exactly the state the source reached through
// ordinary row-by-row writes, whose triggers maintain derived tables (series
// episode counts and added dates, person search names, not-interested
// revisions). PostgreSQL restores with those triggers suspended, so this
// pins that the verbatim copy, the replay of triggers whose derived table a
// dump lacks, and the single policy-epoch advance add up to the same thing.
// Set ETHER_TEST_POSTGRES to run it on PostgreSQL.
func TestLogicalRestoreMatchesRowByRowWrites(t *testing.T) {
	ctx := context.Background()
	src := store.OpenTest(t)
	for _, q := range []string{
		`INSERT INTO catalog_titles(key,type,title,released_ts) VALUES(10,'show','Series',1600000000)`,
		`INSERT INTO catalog_titles(key,type,title,show_key,season,episode,released_ts) VALUES(11,'episode','Pilot',10,1,1,1600000000)`,
		`INSERT INTO catalog_titles(key,type,title,show_key,season,episode,released_ts) VALUES(12,'episode','Second',10,1,2,1600600000)`,
		`INSERT INTO catalog_titles(key,type,title,show_key,season,episode,released_ts,adult) VALUES(13,'episode','Hidden',10,1,3,1601200000,1)`,
		`INSERT INTO catalog_titles(key,type,title,released_ts) VALUES(20,'movie','Film',1500000000)`,
		`INSERT INTO catalog_credits(key,person_tmdb,name,role) VALUES(10,7,'','Lead')`,
		`INSERT INTO catalog_credits(key,person_tmdb,name,role) VALUES(20,7,'Ada Actor','Hero')`,
		`INSERT INTO catalog_credits(key,person_tmdb,name,role) VALUES(20,8,'Bo Builder','Villain')`,
		`INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(1,'20',1,'h1')`,
		`INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(1,'10',2,'h2')`,
		`DELETE FROM notinterested_titles WHERE handle='h2'`,
	} {
		if _, err := src.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var dump bytes.Buffer
	if err := store.ExportLogical(ctx, src, &dump); err != nil {
		t.Fatal(err)
	}
	want := tableContents(t, src)
	if !strings.Contains(want["catalog_show_episodes"], "10|2") || !strings.Contains(want["catalog_person_names"], "7|Ada Actor|2") {
		t.Fatalf("fixture did not exercise the derived tables: %q %q", want["catalog_show_episodes"], want["catalog_person_names"])
	}

	restore := func(name string, dump []byte) {
		t.Run(name, func(t *testing.T) {
			dst := store.OpenTest(t)
			before := policyEpoch(t, dst)
			beforeChannel := channelEpoch(t, dst)
			if err := store.ImportLogical(ctx, dst, bytes.NewReader(dump)); err != nil {
				t.Fatal(err)
			}
			got := tableContents(t, dst)
			for table, rows := range want {
				if table != "viewer_policy_epoch" && table != "livetv_channel_epoch" && got[table] != rows {
					t.Errorf("%s:\n got %q\nwant %q", table, got[table], rows)
				}
			}
			if after := policyEpoch(t, dst); after <= before || after <= policyEpoch(t, src) {
				t.Errorf("policy epoch %d after restore; target was %d, source %d", after, before, policyEpoch(t, src))
			}
			if after := channelEpoch(t, dst); after <= beforeChannel || after <= channelEpoch(t, src) {
				t.Errorf("channel epoch %d after restore; target was %d, source %d", after, beforeChannel, channelEpoch(t, src))
			}
			if store.DialectOf(dst) != store.Postgres {
				// Full-text indexes are not in dumps; the restore rebuilds them.
				var title, person int
				if err := dst.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM catalog_titles_fts WHERE catalog_titles_fts MATCH 'Second'), (SELECT COUNT(*) FROM catalog_person_names_fts WHERE catalog_person_names_fts MATCH 'Ada')`).Scan(&title, &person); err != nil || title != 1 || person != 1 {
					t.Errorf("search indexes after restore: title %d person %d, %v", title, person, err)
				}
			}
			// Triggers are live again once the restore commits.
			if _, err := dst.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,show_key,season,episode,released_ts) VALUES(14,'episode','Third',10,1,4,1601800000)`); err != nil {
				t.Fatal(err)
			}
			var known int
			if err := dst.QueryRowContext(ctx, `SELECT known FROM catalog_show_episodes WHERE show_key=10`).Scan(&known); err != nil || known != 3 {
				t.Fatalf("episode count after a new write = %d, %v; want 3", known, err)
			}
		})
	}
	restore("current dump", dump.Bytes())
	// A dump from a release before these derived tables existed.
	restore("dump without derived tables", withoutTables(t, dump.Bytes(), "catalog_show_episodes", "catalog_person_names", "catalog_stats_tally", "catalog_stats_tally_delta"))
}

func policyEpoch(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var g int64
	if err := db.QueryRow(`SELECT generation FROM viewer_policy_epoch WHERE id=1`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// foldedTallies are the catalogue's delta-kept count tables and their keys.
var foldedTallies = map[string]string{
	"catalog_browse_tally": "type,genre,rating_rank",
	"catalog_stats_tally":  "kind,browsable,home,year_pending",
}

// tableContents renders every non-empty table as sorted "a|b|c" lines.
func tableContents(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	query := `SELECT name FROM sqlite_master WHERE type='table' AND sql NOT LIKE 'CREATE VIRTUAL%' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE '%\_fts\_%' ESCAPE '\'`
	if store.DialectOf(db) == store.Postgres {
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'`
	}
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "schema_migrations" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	out := map[string]string{}
	for _, table := range tables {
		query := `SELECT * FROM "` + table + `"`
		if dims, ok := foldedTallies[strings.TrimSuffix(table, "_delta")]; ok {
			// A restore rebuilds a tally folded; the source may hold
			// deltas. Both read as the same sums.
			if strings.HasSuffix(table, "_delta") {
				continue
			}
			query = `SELECT ` + dims + `,SUM(n) FROM (SELECT ` + dims + `,n FROM ` + table + ` UNION ALL SELECT ` + dims + `,d FROM ` + table + `_delta) s GROUP BY ` + dims + ` HAVING SUM(n)<>0`
		}
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				parts[i] = fmt.Sprint(v)
			}
			lines = append(lines, strings.Join(parts, "|"))
		}
		rows.Close()
		if len(lines) > 0 {
			sort.Strings(lines)
			out[table] = strings.Join(lines, "\n")
		}
	}
	return out
}

// withoutTables drops whole table sections from a logical dump and fixes
// its trailer, as a dump written before those tables existed would read.
func withoutTables(t *testing.T, dump []byte, drop ...string) []byte {
	t.Helper()
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	var out bytes.Buffer
	var tables, rows int64
	dropping := false
	for _, line := range bytes.Split(bytes.TrimSpace(dump), []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte(`{"table"`)):
			var h struct{ Table string }
			if err := json.Unmarshal(line, &h); err != nil {
				t.Fatal(err)
			}
			if dropping = skip[h.Table]; !dropping {
				tables++
			}
		case bytes.HasPrefix(line, []byte(`{"end"`)):
			fmt.Fprintf(&out, `{"end":true,"tables":%d,"rows":%d}`+"\n", tables, rows)
			continue
		case bytes.HasPrefix(line, []byte(`[`)):
			if !dropping {
				rows++
			}
		}
		if !dropping {
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func channelEpoch(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var g int64
	if err := db.QueryRow(`SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}
