package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Touch sources: every place outside the catalogue that holds a title
// reference because a viewer or the owner acted (Tier A), a play log or
// play state that renders titles back to viewers (Tier B), or an opaque
// store that may hold one. The tier migration keeps every title any of
// them names. Over-keeping is harmless; removing a touched title is not,
// so a store is listed whenever it could hold a key.
//
// tierUntouchedTables lists every other table. TestTierSourcesCoverSchema
// fails when a migration adds a table that is in neither list.

// touchForm says how a source column holds its title references.
type touchForm uint8

const (
	// touchKey is an integer rating key.
	touchKey touchForm = iota
	// touchKeyText is a decimal rating key in a TEXT column.
	touchKeyText
	// touchIDOrKey is a rating key, or a bare TMDB id whose kind the
	// column does not say (kept as both its movie and its show).
	touchIDOrKey
	// touchScan is JSON or free text, scanned for rating-key-shaped
	// integers and for TMDB ids written next to "tmdb".
	touchScan
)

type touchSource struct {
	table, column string
	form          touchForm
	// tierA sources are re-read for each removal batch, inside its
	// transaction (touchKey and touchKeyText only: they are range-read).
	tierA bool
	where string
}

var tierTouchSources = []touchSource{
	// Tier A: viewer or owner intent.
	{table: "state_watch", column: "rating_key", tierA: true},
	{table: "state_history", column: "rating_key", tierA: true},
	{table: "state_history_events", column: "rating_key"},
	{table: "state_lifetime_events", column: "rating_key"},
	{table: "state_history_rank", column: "rating_key"},
	{table: "state_continue_hidden", column: "item_key", tierA: true},
	{table: "personalize_discovery_hidden", column: "item_key", tierA: true},
	{table: "state_last_source", column: "rating_key", tierA: true},
	{table: "watchlist_items", column: "item_key", tierA: true},
	{table: "watchlist_episodes", column: "item_key", tierA: true},
	{table: "watchlist_episodes", column: "episode_key", tierA: true},
	{table: "watchlist_episodes_alerts", column: "episode_key"},
	{table: "favorites_items", column: "subject_id", form: touchKeyText, tierA: true},
	{table: "notinterested_titles", column: "title_key", form: touchKeyText, tierA: true},
	{table: "state_playlist_items", column: "rating_key", tierA: true},
	{table: "state_playlists", column: "smart_uri", form: touchScan},
	{table: "collections_members", column: "key", tierA: true},
	{table: "collections_specs", column: "spec_json", form: touchScan},
	{table: "episodeorder_preferences", column: "show_key", tierA: true},
	{table: "trackprefs_memory", column: "show_id", form: touchIDOrKey},
	{table: "trackprefs_scoped", column: "scope_id", form: touchIDOrKey},
	{table: "viewer_episode_alerts", column: "episode_key", form: touchKeyText, tierA: true},
	{table: "viewer_episode_seen", column: "episode_key", form: touchKeyText, tierA: true},
	{table: "viewer_push_claims", column: "episode_key", form: touchKeyText},
	{table: "catalog_owner_tags", column: "key", tierA: true},
	{table: "catalog_owner_includes", column: "key", tierA: true},
	{table: "overlays_artwork", column: "item_key", tierA: true},
	{table: "resolver_pins", column: "rating_key", tierA: true},
	{table: "resolver_bad", column: "rating_key", tierA: true, where: "manual=1"},
	{table: "resolver_rejected", column: "rating_key", tierA: true},
	{table: "livetv_channels", column: "data", form: touchScan},
	{table: "livetv_personal", column: "rundown", form: touchScan},
	{table: "livetv_personal_programmes", column: "data", form: touchScan},
	{table: "migrate_watchlist", column: "key", tierA: true},
	{table: "migrate_objects", column: "local_id"},

	// Tier B: play logs and play state.
	{table: "playback_resume", column: "rating_key"},
	{table: "playback_index", column: "rating_key"},
	{table: "playback_probe", column: "rating_key"},
	{table: "personalize_plays", column: "item_key"},
	{table: "personalize_plays", column: "taste_key"},
	{table: "personalize_item_impressions", column: "item_key"},
	{table: "personalize_serve_items", column: "item_key"},
	{table: "personalize_serve_items", column: "taste_key"},
	{table: "stats_plays", column: "rating_key"},
	{table: "prerolls_plays", column: "rating_key"},
	{table: "resolver_history", column: "rating_key"},
	{table: "state_sync_journal", column: "rating_key"},
	{table: "state_sync_lock", column: "rating_key"},
	{table: "plexwatch_remote", column: "rating_key"},
	{table: "plexwatch_expand", column: "show_key"},
	{table: "plexwatch_expand", column: "after_key"},
	{table: "plexwatch_pms_item", column: "rating_key"},
	{table: "plexwatch_pms_sent", column: "rating_key"},
	{table: "simkl_outbox", column: "rating_key"},
	{table: "trakt_import_queue", column: "event_json", form: touchScan},
	{table: "plex_downloads", column: "snapshot", form: touchScan},
	{table: "plex_playqueues", column: "data", form: touchScan},
	{table: "plex_sessions", column: "data", form: touchScan},
	{table: "plex_decision_arms", column: "title_key"},
	{table: "plex_version_flows", column: "title_key"},
	{table: "playback_live_sessions", column: "data", form: touchScan},
	{table: "sessions_version_ids", column: "item_id"},
	{table: "sessions_subtitle_intent", column: "rating_key"},
	{table: "sessions_detail_sources", column: "item_id"},
	{table: "sessions_delivery_authority", column: "item_id"},
	{table: "sessions_source_offers", column: "item_id"},
	{table: "variants_ids", column: "rating_key"},
	{table: "markers_refresh_queue", column: "rating_key"},

	// Opaque stores: scanned.
	{table: "plugins_kv", column: "value", form: touchScan},
	{table: "plugins_journal", column: "detail", form: touchScan},
	{table: "plugins_journal", column: "inverse", form: touchScan},
	{table: "plugins_outbox", column: "payload", form: touchScan},
	{table: "plugin_studio_projects", column: "project_json", form: touchScan},
	{table: "plugin_collection_links", column: "local_id", form: touchScan},
	{table: "frontend_preferences", column: "value", form: touchScan},
	{table: "agent_messages", column: "parts", form: touchScan},
	{table: "notify_queue", column: "event", form: touchScan},
	{table: "stats_snapshots", column: "payload", form: touchScan},
	{table: "playbackcontinuity_sessions", column: "payload", form: touchScan},
	{table: "experience_receipts", column: "subject", form: touchScan},
	{table: "clusterplayback_admissions", column: "admission", form: touchScan},
	{table: "clusterplayback_neutral_bindings", column: "binding", form: touchScan},
	{table: "clusterplayback_operation_gates", column: "gate", form: touchScan},
	{table: "clusterplayback_plex_bindings", column: "binding", form: touchScan},
	{table: "clusterplayback_preparation_receipts", column: "receipt", form: touchScan},
	{table: "clusterplayback_recovery_decisions", column: "decision", form: touchScan},
	{table: "clusterplayback_recovery_events", column: "event", form: touchScan},
	{table: "clusterplayback_routes", column: "route", form: touchScan},
	{table: "clusterplayback_source_moves", column: "route", form: touchScan},
	{table: "clusterplayback_source_moves", column: "operation", form: touchScan},
	{table: "clusterplayback_source_recovery", column: "request", form: touchScan},
	{table: "placement_sessions", column: "operation", form: touchScan},
}

// tierUntouchedTables hold no reference a viewer or the owner made: the
// catalogue's own tables (the job removes from them), rebuildable caches
// (Tier C), and tables that never name a title. A prefix ending in "_"
// covers every table that starts with it.
//
// The frontends' id mappings are here on purpose. frontend_compact_ids
// (Silo, Xtream, Emby facets), the Silo offer ids and the session version
// handles map a client's id to a rating key; the job never deletes them,
// so an old client id still resolves, and opening a title the job removed
// goes through catalog.Get, which stores it again (promotion). Keeping
// the titles behind them would keep every title an Xtream client ever
// listed, which is the whole mirror.
var tierUntouchedTables = []string{
	// The catalogue (the job's own tables, and its caches).
	"catalog_",
	// Tier C: caches and derived state, rebuilt on demand.
	"markers_cache", "markers_plugin_cache", "markers_breaks_", "ratings_cache", "ratings_settings",
	"plexmeta_titles", "plexguid_", "resolver_availability", "resolver_hardsub", "resolver_reputation",
	"resolver_subtitle_checks", "resolver_verified", "collections_air_dates", "collections_external",
	"collections_names", "collections_defaults", "collections_meta", "release_observations", "availability_",
	"cachefill_shared", "plex_ultrablur", "plex_shelf_generations", "onair_", "xtream_vod", "xtream_vod_refresh", "experience_row_",
	"mediabrowser_personal_home", "frontend_image_grants", "segcache_", "debrid_", "newznab_", "cardigann_",
	"livetv_programmes", "livetv_guide_", "livetv_numbers", "livetv_sources", "livetv_policies",
	"livetv_channel_epoch", "livetv_plex_guide_gate", "livetv_source_history", "artwork_image_",
	"subtitles_auto_attempts", "subtitles_auto_provider_pause",
	// Libraries hold owner filter definitions and plugin-owned items; a
	// plugin match naming a removed title is promoted again on open.
	"libraries", "libraries_", "plugin_", "plugins_setup",
	// Id mappings (see above).
	"frontend_compact_ids", "frontend_compact_id_counters", "silo_source_offer_ids", "frontend_client_families", "frontend_clients_seen",
	// Accounts, identity, sessions and devices.
	"auth_", "identity_", "invites_", "mcp_oauth_", "capability_", "credentials_format", "safety_policies",
	"sessions_devices", "sessions_lazy_", "sessions_sample_receipts", "sessions_delivery_tokens",
	"sessions_xtream_connections", "viewer_alert_preferences", "viewer_episode_scan", "viewer_policy_",
	"viewer_push_subscriptions", "favorites_mirror", "notinterested_revisions", "state_playlist_markers",
	"state_source_sequence", "simkl_devices", "simkl_post_pacing", "simkl_tokens", "trakt_tokens",
	"plexwatch_account", "plexwatch_fence", "plexwatch_pin", "plexwatch_pms", "plexwatch_pms_held", "plexwatch_pms_unmatched",
	"plexwatch_pms_viewer", "plex_counters", "personalize_overrides", "personalize_ranker",
	"personalize_row_impressions", "personalize_serves", "personalize_home_layout", "trackprefs_rules",
	"episodeorder_groups", "watchlist_meta", "watchlist_settings", "migrate_plans",
	// Cluster, placement, playback plumbing (titles only through the
	// scanned sources above).
	"cluster_", "clusterconfig_", "clusterplayback_coordinators", "clusterplayback_neutral_stops",
	"placement_claims", "placement_displaced", "placement_displaced_claims", "placement_history_", "placement_policy", "placement_pools",
	"placement_removed_targets", "placement_targets", "placement_ledger", "playback_payload_",
	"livetv_refresh_leases", "livetv_source_refresh", "livetv_showcase",
	// Settings, logs and plugins' own plumbing.
	"backup_", "notify_channels", "notify_once", "notify_push_keys", "notify_settings", "overlays_settings",
	"prerolls", "prerolls_settings", "stats_costs", "stats_device_registry", "stats_device_registry_counter", "stats_indexer_days",
	"stats_debrid_days", "stats_stream_days",
	"stats_newsletter_", "stats_provider_days", "plugins_artifacts", "plugins_calls", "plugins_catalogue_",
	"plugins_event_log", "plugins_grants", "plugins_health", "plugins_installed", "plugins_logs",
	"plugins_proposals", "plugins_settings", "agent_approvals", "agent_audit", "agent_secret_requests",
	"agent_threads", "schema_migrations", "sqlite_",
}

// tierTouched is the set of titles the touch sources name, collapsed to
// what the catalogue stores: a season or episode key keeps its show (and
// so the show's seasons and episodes), a franchise key keeps the rows of
// that collection.
type tierTouched struct {
	keys        map[core.RatingKey]struct{}
	collections map[int64]struct{}
}

func newTierTouched() *tierTouched {
	return &tierTouched{keys: map[core.RatingKey]struct{}{}, collections: map[int64]struct{}{}}
}

func (t *tierTouched) add(k core.RatingKey) {
	switch k.Kind() {
	case core.KindMovie, core.KindShow:
		t.keys[k] = struct{}{}
	case core.KindSeason, core.KindEpisode:
		t.keys[core.ShowKey(k.TMDB())] = struct{}{}
	case core.KindCollection:
		t.collections[k.TMDB()] = struct{}{}
	}
}

func (t *tierTouched) has(k core.RatingKey) bool {
	_, ok := t.keys[k]
	return ok
}

// ratingKeyShaped is a movie, show, season, episode or collection key.
func ratingKeyShaped(n int64) bool {
	k := core.RatingKey(n)
	switch k.Kind() {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode, core.KindCollection:
		return k.TMDB() > 0
	}
	return false
}

func (t *tierTouched) addValue(form touchForm, v any) {
	switch form {
	case touchKey:
		if n, ok := intValue(v); ok && ratingKeyShaped(n) {
			t.add(core.RatingKey(n))
		}
	case touchKeyText:
		if n, ok := intValue(v); ok && ratingKeyShaped(n) {
			t.add(core.RatingKey(n))
		}
	case touchIDOrKey:
		n, ok := intValue(v)
		switch {
		case !ok || n <= 0:
		case ratingKeyShaped(n):
			t.add(core.RatingKey(n))
		case n < 1_000_000_000:
			t.add(core.MovieKey(n))
			t.add(core.ShowKey(n))
		}
	case touchScan:
		t.scan(v)
	}
}

var (
	scanKeyRE  = regexp.MustCompile(`(?:^|[^0-9])([1-5][0-9]{15})(?:[^0-9]|$)`)
	scanTMDBRE = regexp.MustCompile(`(?i)tmdb[^0-9a-z]{0,4}([0-9]{1,9})(?:[^0-9]|$)`)
)

func (t *tierTouched) scan(v any) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	case int64:
		if ratingKeyShaped(x) {
			t.add(core.RatingKey(x))
		}
		return
	default:
		return
	}
	if s == "" {
		return
	}
	for _, m := range scanKeyRE.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && ratingKeyShaped(n) {
			t.add(core.RatingKey(n))
		}
	}
	for _, m := range scanTMDBRE.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			t.add(core.MovieKey(n))
			t.add(core.ShowKey(n))
		}
	}
}

func intValue(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case float64:
		return int64(x), x == float64(int64(x))
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case []byte:
		n, err := strconv.ParseInt(strings.TrimSpace(string(x)), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// existingTables is the set of tables in the database: a source whose
// package is not part of this binary (or not migrated yet) is skipped.
func existingTables(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, dialect store.Dialect) (map[string]bool, error) {
	query := `SELECT name FROM sqlite_master WHERE type='table'`
	if dialect == store.Postgres {
		query = `SELECT tablename FROM pg_tables WHERE schemaname=current_schema()`
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// collectTouched reads every touch source in full. Each source is one
// streaming read of one column; the job runs it once, off any request.
func (c *Catalog) collectTouched(ctx context.Context) (*tierTouched, map[string]int, error) {
	tables, err := existingTables(ctx, c.db, store.DialectOf(c.db))
	if err != nil {
		return nil, nil, err
	}
	touched := newTierTouched()
	perSource := map[string]int{}
	for _, src := range tierTouchSources {
		if !tables[src.table] {
			continue
		}
		before := len(touched.keys) + len(touched.collections)
		if err := readTouchSource(ctx, c.db, src, touched); err != nil {
			return nil, nil, fmt.Errorf("catalog: tier migration: read %s.%s: %w", src.table, src.column, err)
		}
		perSource[src.table+"."+src.column] = len(touched.keys) + len(touched.collections) - before
	}
	return touched, perSource, nil
}

func readTouchSource(ctx context.Context, db *sql.DB, src touchSource, into *tierTouched) error {
	query := `SELECT DISTINCT ` + src.column + ` FROM ` + src.table
	if src.form == touchScan {
		query = `SELECT ` + src.column + ` FROM ` + src.table
	}
	if src.where != "" {
		query += ` WHERE ` + src.where
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return err
		}
		into.addValue(src.form, v)
	}
	return rows.Err()
}

// keyRange is an inclusive range of rating keys.
type keyRange struct{ lo, hi core.RatingKey }

// recheckTierA re-reads the Tier A sources for the ranges a removal batch
// covers, inside the batch's transaction, and adds what it finds to
// touched. A title touched after the keep set was built is kept.
func recheckTierA(ctx context.Context, tx *sql.Tx, tables map[string]bool, ranges []keyRange, touched *tierTouched) error {
	for _, src := range tierTouchSources {
		if !src.tierA || !tables[src.table] {
			continue
		}
		for _, r := range ranges {
			var lo, hi any = int64(r.lo), int64(r.hi)
			if src.form == touchKeyText {
				// Rating keys in these ranges all have 16 digits, so
				// their decimal strings order as the numbers do.
				lo, hi = strconv.FormatInt(int64(r.lo), 10), strconv.FormatInt(int64(r.hi), 10)
			}
			query := `SELECT ` + src.column + ` FROM ` + src.table + ` WHERE ` + src.column + ` BETWEEN ? AND ?`
			if src.where != "" {
				query += ` AND ` + src.where
			}
			rows, err := tx.QueryContext(ctx, query, lo, hi)
			if err != nil {
				return fmt.Errorf("catalog: tier migration: recheck %s.%s: %w", src.table, src.column, err)
			}
			for rows.Next() {
				var v any
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return err
				}
				touched.addValue(src.form, v)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// TierTouchedKeys returns the movie and show keys the touch sources name
// (seasons and episodes collapsed to their show), sorted.
func (c *Catalog) TierTouchedKeys(ctx context.Context) ([]core.RatingKey, error) {
	touched, _, err := c.collectTouched(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(touched.keys))
	for k := range touched.keys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

// TierUnclassifiedTables lists the tables in db that are neither a touch
// source nor declared untouched: a new table must be classified before the
// tier migration can be trusted with it.
func TierUnclassifiedTables(ctx context.Context, db *sql.DB) ([]string, error) {
	tables, err := existingTables(ctx, db, store.DialectOf(db))
	if err != nil {
		return nil, err
	}
	source := map[string]bool{}
	for _, s := range tierTouchSources {
		source[s.table] = true
	}
	var out []string
	for name := range tables {
		if source[name] || tierDeclaredUntouched(name) || strings.HasPrefix(name, "catalog_titles_fts") || strings.HasPrefix(name, "catalog_alt_titles_fts") || strings.HasPrefix(name, "catalog_person_names_fts") {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

func tierDeclaredUntouched(name string) bool {
	for _, u := range tierUntouchedTables {
		if name == u || (strings.HasSuffix(u, "_") && strings.HasPrefix(name, u)) {
			return true
		}
	}
	return false
}

// TierSource is one touch source, for tests that seed every source.
type TierSource struct {
	Table, Column string
	// Text: a decimal key in a TEXT column. Scan: JSON or text scanned
	// for keys. BareID: a key or a bare TMDB id.
	Text, Scan, BareID bool
	Where              string
}

// TierSources lists the touch sources.
func TierSources() []TierSource {
	out := make([]TierSource, len(tierTouchSources))
	for i, s := range tierTouchSources {
		out[i] = TierSource{Table: s.table, Column: s.column, Text: s.form == touchKeyText, Scan: s.form == touchScan, BareID: s.form == touchIDOrKey, Where: s.where}
	}
	return out
}
