package livetv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Search remains bounded over a large lineup; overlapping guides never duplicate
// identities and hidden-guide membership cannot bypass viewer access policy.
func TestLiveSearchAcrossGuidesScale(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 10000; n++ {
		id := fmt.Sprintf("c%05d", n)
		c := Channel{ID: id, Name: "Station " + id, Number: fmt.Sprint(n), Kind: "iptv", Enabled: true, SourceID: "provider", GuideID: id, Group: "News", StreamURL: "https://example.invalid/live"}
		if n == 9999 {
			c.Group = "Hidden"
		}
		b, _ := json.Marshal(c)
		if _, err = stmt.ExecContext(ctx, id, b); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	p := Programme{Start: now.Add(-time.Minute), End: now.Add(time.Hour), Item: Item{Title: "Morning Report", EpisodeTitle: "Satellite Launch", Summary: "A telescope explores the stars", Rating: "G"}}
	b, _ := json.Marshal(p)
	if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)`, "provider", "c00042", p.Start.UnixMilli(), p.End.UnixMilli(), b); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveOwnerGuides(ctx, []OwnerGuide{{ID: "news", Name: "News", Groups: []GuideGroup{{"provider", "News"}}}, {ID: "overlap", Name: "Overlap", Channels: []string{"c00042"}}, {ID: "hidden", Name: "Hidden", Hidden: true, Groups: []GuideGroup{{"provider", "Hidden"}}}}); err != nil {
		t.Fatal(err)
	}
	host := liveTVHost{s: s}
	owner := &core.User{ID: 1, IsOwner: true}
	first, e := host.SearchLiveTV(ctx, owner, "42", 20)
	if e != nil || len(first.Channels) == 0 {
		t.Fatalf("first search fallback: %+v %v", first, e)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		hits, e := host.SearchLiveTV(ctx, owner, "c00042", 20)
		if e != nil {
			t.Fatal(e)
		}
		if !hits.Partial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("index not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	virtual := testChannel()
	virtual.ID = "virtual-search"
	virtual.Name = "Virtual movies"
	virtual.SourceID = "provider"
	virtual.Group = "News"
	virtual.Number = "10001"
	virtual.Items[0].Title = "VirtualSearchUnique"
	if _, err = s.SaveChannel(ctx, virtual); err != nil {
		t.Fatal(err)
	}
	if err = s.refreshVirtualGuide(ctx); err != nil {
		t.Fatal(err)
	}
	builds := s.virtualGuide.builds.Load()
	vh, e := host.SearchLiveTV(ctx, owner, "VirtualSearchUnique", 5)
	if e != nil || len(vh.Programmes) == 0 || vh.Programmes[0].ChannelID != virtual.ID {
		t.Fatalf("snapshot programme search: %+v %v", vh, e)
	}
	if s.virtualGuide.builds.Load() != builds {
		t.Fatal("search computed an extra schedule")
	}

	for _, query := range []string{"c00042", "42"} {
		hits, e := host.SearchLiveTV(ctx, owner, query, 20)
		if e != nil || len(hits.Channels) == 0 || hits.Channels[0].ID != "c00042" {
			t.Fatalf("channel %q: %+v %v", query, hits, e)
		}
	}
	for _, query := range []string{"Morning Report", "Satellite", "telescope"} {
		hits, e := host.SearchLiveTV(ctx, owner, query, 20)
		if e != nil || len(hits.Programmes) != 1 || len(hits.ProgrammeChannels) != 1 || hits.Programmes[0].ChannelID != "c00042" {
			t.Fatalf("programme %q: %+v %v", query, hits, e)
		}
	}
	hits, err := host.SearchLiveTV(ctx, owner, "c09999", 20)
	if err != nil || len(hits.Channels) != 0 {
		t.Fatalf("hidden: %+v %v", hits, err)
	}
	if err = s.SavePolicy(ctx, Policy{UserID: 2, Restricted: true, Channels: []string{"c00043"}}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"c00042", "Satellite"} {
		hits, e := host.SearchLiveTV(ctx, &core.User{ID: 2}, query, 20)
		if e != nil || len(hits.Channels)+len(hits.Programmes) != 0 {
			t.Fatalf("policy: %+v %v", hits, e)
		}
	}
	private := p
	private.ID = "personal-2-c00043"
	private.ChannelID = "c00043"
	private.Item.Title = "PrivateRundownNeedle"
	tx, err = s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeSearchProgramme(ctx, tx, 2, private.ChannelID, private); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []*core.User{owner, {ID: 2}} {
		hits, e := host.SearchLiveTV(ctx, viewer, "PrivateRundownNeedle", 10)
		want := 0
		if viewer.ID == 2 {
			want = 1
		}
		if e != nil || len(hits.Programmes) != want {
			t.Fatalf("personal programme isolation viewer=%d: %+v %v", viewer.ID, hits, e)
		}
	}
	hits, err = host.SearchLiveTV(ctx, owner, "Station", 7)
	if err != nil || len(hits.Channels) != 7 {
		t.Fatalf("bound: %+v %v", hits, err)
	}
	if store.DialectOf(s.o.DB) == store.Postgres {
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		// Ask whether the production expression can use its GIN index even
		// when this small fixture would make a sequential scan cheaper.
		if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
			t.Fatal(err)
		}
		var plan string
		if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT id FROM livetv_channels WHERE to_tsvector('simple',coalesce(data::jsonb->>'name','') || ' ' || coalesce(data::jsonb->>'number','') || ' ' || coalesce(data::jsonb->>'guideId','')) @@ to_tsquery('simple','c00042:*') ORDER BY id LIMIT 400`).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, `"Index Name": "livetv_search_channels"`) {
			t.Fatalf("search lost its GIN index: %s", plan)
		}
		return
	}

	rows, err := s.o.DB.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM livetv_search_channels WHERE livetv_search_channels MATCH 'station*' ORDER BY rank LIMIT 400`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "VIRTUAL TABLE INDEX")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("search lost its FTS index")
	}
}

// A logical backup crosses engines without exporting SQLite docids or claiming
// an absent FTS cache is ready. Restoring leaves cache work until search demand.
func TestLiveSearchRestoreDemand(t *testing.T) {
	ctx := t.Context()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := testService(t, Options{DB: db})
	c := Channel{ID: "restore-search", Name: "RestoreSearchNeedle", Number: "1", Kind: "iptv", Enabled: true, StreamURL: "https://example.invalid/live"}
	if _, err := source.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := source.buildSearchIndex(ctx, true); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := store.ExportLogical(ctx, db, &dump); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"livetv_search_state", "livetv_search_channel_keys", "livetv_search_programme_keys"} {
		if bytes.Contains(dump.Bytes(), []byte(`"table":"`+table+`"`)) {
			t.Fatalf("dump contains cache %s", table)
		}
	}
	target := testService(t, Options{})
	if err := store.ImportLogical(ctx, target.o.DB, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatal(err)
	}
	var active, ready int
	if err := target.o.DB.QueryRowContext(ctx, `SELECT active,ready FROM livetv_search_state WHERE id=1`).Scan(&active, &ready); err != nil {
		t.Fatal(err)
	}
	if active != 0 || ready != 0 {
		t.Fatalf("restore enabled search cache: %d/%d", active, ready)
	}
	if store.DialectOf(target.o.DB) != store.Postgres {
		var count int
		if err := target.o.DB.QueryRowContext(ctx, `SELECT count(*) FROM livetv_search_channel_keys`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("restore eagerly indexed %d channels", count)
		}
	}
	host := liveTVHost{s: target}
	for deadline := time.Now().Add(5 * time.Second); ; {
		hits, err := host.SearchLiveTV(ctx, &core.User{ID: 1, IsOwner: true}, "RestoreSearchNeedle", 5)
		if err != nil {
			t.Fatal(err)
		}
		if !hits.Partial {
			if len(hits.Channels) != 1 || hits.Channels[0].ID != c.ID {
				t.Fatalf("restored search: %+v", hits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restored search index not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
