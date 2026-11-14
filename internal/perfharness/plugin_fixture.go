package perfharness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// pluginFixtureLibraryID is the seeded plugin library's id. It is far above
// the builtin ids 1 and 2 and below the MediaBrowser personal-view bit, so it
// is a normal library everywhere.
const pluginFixtureLibraryID = 100

// pluginFixturePluginID and pluginFixtureKindKey name the seeded plugin kind.
// No plugin record is installed: the library is browsable through the console
// and the plugin item store, which is what the browse bounds measure. A
// frontend only publishes it once a real plugin installs the same (plugin,
// kind), which the synthetic fixture deliberately does not do.
const (
	pluginFixturePluginID = "community.example.perf"
	pluginFixtureKindKey  = "clip"
)

// pluginFixtureDecl is the installed declaration for the seeded kind: one
// flat, playable level, browsable by title and year with a genre facet, so
// the console's plugin-library browser exercises the indexed paths.
func pluginFixtureDecl() plugins.MediaKindMeta {
	decl := plugins.MediaKindMeta{
		Key:     pluginFixtureKindKey,
		Label:   "Clips",
		Library: plugins.KindLibraryDecl{Name: "Field Clips"},
		Levels:  []plugins.KindLevel{{Key: "clip", Label: "Clip", Playable: true}},
		Fields: []plugins.KindField{
			{Key: "genre", Type: "string", Label: "Genre", Indexed: true},
		},
		Browse: plugins.KindBrowseDecl{Sorts: []string{"title", "year"}, Facets: []string{"genre"}, Search: true},
		Sync:   plugins.KindSyncDecl{Mode: plugins.KindSyncPull},
	}
	decl.Present.MediaBrowser = &struct {
		CollectionType string            `json:"collectionType"`
		ItemTypes      map[string]string `json:"itemTypes,omitempty"`
	}{CollectionType: "movies"}
	decl.Present.Stremio = &struct {
		Type string `json:"type"`
	}{Type: "movie"}
	return decl
}

// seedPluginLibrary writes one plugin library with n flat, playable items and
// its maintained tally. It mirrors the rows pluginitems.UpsertKind and the
// pull sync would write, but in one bulk transaction, so the fixture stays
// fast at a million items.
func seedPluginLibrary(ctx context.Context, db *sql.DB, n int) error {
	if n <= 0 {
		return nil
	}
	declJSON, err := json.Marshal(pluginFixtureDecl())
	if err != nil {
		return err
	}
	presentJSON, err := json.Marshal(pluginFixtureDecl().Present)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := int64(1700000000)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO libraries(id, key, name, source, media_type, plugin_id, kind_key, present, sort, hidden, created_at, updated_at)
VALUES(?, ?, 'Field Clips', 'plugin', '', ?, ?, ?, ?, 0, ?, ?)`,
		pluginFixtureLibraryID, "pk-"+pluginFixturePluginID+"-"+pluginFixtureKindKey,
		pluginFixturePluginID, pluginFixtureKindKey, string(presentJSON),
		pluginFixtureLibraryID, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO libraries_seq(id, next) VALUES(1, ?)
ON CONFLICT(id) DO UPDATE SET next = MAX(next, ?)`,
		pluginFixtureLibraryID+1, pluginFixtureLibraryID+1); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_kinds(plugin_id, kind_key, library_id, decl, decl_hash, schema_version)
VALUES(?, ?, ?, ?, ?, 1)`,
		pluginFixturePluginID, pluginFixtureKindKey, pluginFixtureLibraryID, string(declJSON), "fixture"); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO plugin_items(
	item_ref, seq, plugin_id, kind_key, library_id, external_id, parent_ref, level, idx,
	title, sort_title, year, released, rating, playable, child_count, leaf_count,
	hints, fields, etag, added_at, updated_at, seen_at, missed_walks
) VALUES (?, ?, ?, ?, ?, ?, '', 'clip', 0, ?, ?, ?, ?, ?, 1, 0, 0, '{}', '{}', '', ?, ?, ?, 0)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	titles := []string{"Field Clip", "Harbour Clip", "Signal Clip", "Winter Clip", "Glass Clip", "Echo Clip", "Stone Clip", "Iron Clip", "Paper Clip", "Ocean Clip"}
	for i := 0; i < n; i++ {
		seq := int64(i + 1)
		extID := fmt.Sprintf("clip-%07d", i)
		ref := string(core.NewItemRef(pluginFixturePluginID, pluginFixtureKindKey, extID))
		title := fmt.Sprintf("%s %07d", titles[i%len(titles)], i)
		sortTitle := fmt.Sprintf("%s %07d", titles[i%len(titles)], i)
		year := 1970 + (i % 55)
		released := int64(0)
		rating := float64((i % 100)) / 10
		if _, err := stmt.ExecContext(ctx, ref, seq, pluginFixturePluginID, pluginFixtureKindKey, pluginFixtureLibraryID, extID,
			title, sortTitle, year, released, rating, now, now, now); err != nil {
			return fmt.Errorf("seed plugin item %d: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_items_seq SET next = ? WHERE id = 1`, n+1); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_item_tally(library_id, level, n) VALUES(?, 'clip', ?)
ON CONFLICT(library_id, level) DO UPDATE SET n = excluded.n`,
		pluginFixtureLibraryID, n); err != nil {
		return err
	}
	return tx.Commit()
}
