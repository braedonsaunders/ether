-- source 4588b98939bd5becd5136e1eac37da13b14d50c0
-- order component then version, matching origin/main store.registered (alphabetical; RegisterAfter is absent)
-- migration auth 1
CREATE TABLE auth_users (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	plex_id            INTEGER NOT NULL UNIQUE,
	uuid               TEXT    NOT NULL DEFAULT '',
	username           TEXT    NOT NULL DEFAULT '',
	title              TEXT    NOT NULL DEFAULT '',
	email              TEXT    NOT NULL DEFAULT '',
	thumb              TEXT    NOT NULL DEFAULT '',
	is_owner           INTEGER NOT NULL DEFAULT 0,
	is_home            INTEGER NOT NULL DEFAULT 0,
	restricted         INTEGER NOT NULL DEFAULT 0,
	active             INTEGER NOT NULL DEFAULT 1,
	origin             TEXT    NOT NULL DEFAULT 'friend',
	plex_sections      TEXT    NOT NULL DEFAULT '[]',
	stream_limit       INTEGER NOT NULL DEFAULT 0,
	max_content_rating TEXT    NOT NULL DEFAULT '',
	subtitle_languages TEXT    NOT NULL DEFAULT '[]',
	audio_languages    TEXT    NOT NULL DEFAULT '[]',
	allowed_sections   TEXT    NOT NULL DEFAULT '[]',
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL,
	last_seen_at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE auth_tokens (
	token_hash TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
	source     TEXT    NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX auth_tokens_user ON auth_tokens(user_id);
-- prism-upgrade-migration-end
-- migration auth 2
UPDATE auth_users SET is_owner=0 WHERE is_owner=1 AND id <> (
	SELECT id FROM auth_users WHERE is_owner=1 ORDER BY updated_at DESC, id DESC LIMIT 1);
CREATE UNIQUE INDEX auth_users_one_owner ON auth_users(is_owner) WHERE is_owner=1;
-- prism-upgrade-migration-end
-- migration auth 3
CREATE TABLE auth_delegations (
 token_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 expires_at INTEGER NOT NULL
);
CREATE INDEX auth_delegations_user ON auth_delegations(user_id);
-- prism-upgrade-migration-end
-- migration auth_sessions 1
CREATE TABLE auth_sessions (
 token_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE INDEX auth_sessions_user ON auth_sessions(user_id);
-- prism-upgrade-migration-end
-- migration availability 1
CREATE TABLE availability_feeds (
 source_id TEXT PRIMARY KEY, generation TEXT NOT NULL, anchor TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL DEFAULT 0, synced_at INTEGER NOT NULL DEFAULT 0,
 gap INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '', discovered INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE availability_releases (
 id TEXT NOT NULL, rating_key INTEGER NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 payload TEXT NOT NULL, title TEXT NOT NULL, name TEXT NOT NULL, protocol TEXT NOT NULL,
 first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL, first_verified INTEGER NOT NULL DEFAULT 0,
 verified_at INTEGER NOT NULL DEFAULT 0, policy TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'observed',
 reason TEXT NOT NULL DEFAULT '', checked_id TEXT NOT NULL DEFAULT '', next_check INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 revision INTEGER NOT NULL DEFAULT 0, priority REAL NOT NULL DEFAULT 0, PRIMARY KEY(id,rating_key,source_id)
);
CREATE INDEX availability_queue ON availability_releases(next_check,priority);
CREATE INDEX availability_title ON availability_releases(rating_key,verified_at);
CREATE TABLE availability_budget(day TEXT NOT NULL, resource TEXT NOT NULL, used INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(day,resource));
-- prism-upgrade-migration-end
-- migration availability 2
CREATE TABLE availability_feed_items (
 id TEXT NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 match_title TEXT NOT NULL, payload TEXT NOT NULL, seen_at INTEGER NOT NULL,
 PRIMARY KEY(id,source_id));
 CREATE INDEX availability_feed_match ON availability_feed_items(match_title,seen_at);
-- prism-upgrade-migration-end
-- migration availability 3
ALTER TABLE availability_feeds ADD COLUMN catchup_target TEXT NOT NULL DEFAULT '';
 ALTER TABLE availability_feeds ADD COLUMN catchup_tail TEXT NOT NULL DEFAULT '';
 ALTER TABLE availability_feeds ADD COLUMN catchup_offset INTEGER NOT NULL DEFAULT 0;
 -- Earlier monitors only seeded the newest page and discarded gap boundaries.
 -- Recover their recent history once, within the normal catch-up allowance.
 UPDATE availability_feeds SET catchup_target='__ether_recent_history__',gap=1,
 message='Recent feed history is queued for bounded catch-up';
-- prism-upgrade-migration-end
-- migration backup 1
CREATE TABLE backup_settings(id INTEGER PRIMARY KEY CHECK(id=1),interval_hours INTEGER NOT NULL DEFAULT 24,retain INTEGER NOT NULL DEFAULT 7,last_run INTEGER NOT NULL DEFAULT 0); INSERT INTO backup_settings(id)VALUES(1);
-- prism-upgrade-migration-end
-- migration backup 2
ALTER TABLE backup_settings ADD COLUMN shared_dir TEXT NOT NULL DEFAULT '';
CREATE TABLE backup_files(
	node_id TEXT NOT NULL,
	name TEXT NOT NULL,
	node_name TEXT NOT NULL DEFAULT '',
	dir TEXT NOT NULL DEFAULT '',
	shared INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(node_id, name)
);
CREATE INDEX backup_files_created ON backup_files(created);
-- prism-upgrade-migration-end
-- migration cachefill 1
CREATE TABLE cachefill_shared(id INTEGER PRIMARY KEY CHECK(id=1), doc TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT '{}', paused INTEGER NOT NULL DEFAULT 0, epoch INTEGER NOT NULL DEFAULT 0, owner TEXT NOT NULL DEFAULT '', expires INTEGER NOT NULL DEFAULT 0, next_run INTEGER NOT NULL DEFAULT 0);
-- prism-upgrade-migration-end
-- migration cardigann 1
CREATE TABLE cardigann_definitions (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  yaml TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '',
  sha TEXT NOT NULL DEFAULT '',
  installed_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE cardigann_sessions (
  indexer_id TEXT PRIMARY KEY,
  cookies TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);
CREATE TABLE cardigann_tests (
  indexer_id TEXT PRIMARY KEY,
  ok INTEGER NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  results INTEGER NOT NULL DEFAULT 0,
  tested_at INTEGER NOT NULL
);
-- prism-upgrade-migration-end
-- migration catalog 1
CREATE TABLE catalog_titles (
  key INTEGER PRIMARY KEY,
  type TEXT NOT NULL,
  tmdb_id INTEGER NOT NULL DEFAULT 0,
  imdb TEXT NOT NULL DEFAULT '',
  tvdb INTEGER NOT NULL DEFAULT 0,
  show_key INTEGER NOT NULL DEFAULT 0,
  show_tmdb INTEGER NOT NULL DEFAULT 0,
  show_title TEXT NOT NULL DEFAULT '',
  season_title TEXT NOT NULL DEFAULT '',
  season INTEGER NOT NULL DEFAULT 0,
  episode INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL DEFAULT '',
  original_title TEXT NOT NULL DEFAULT '',
  sort_title TEXT NOT NULL DEFAULT '',
  tagline TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  year INTEGER NOT NULL DEFAULT 0,
  released_ts INTEGER NOT NULL DEFAULT 0,
  runtime_sec INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  content_rating TEXT NOT NULL DEFAULT '',
  content_rating_rank INTEGER NOT NULL DEFAULT 0,
  rating REAL NOT NULL DEFAULT 0,
  vote_count INTEGER NOT NULL DEFAULT 0,
  popularity REAL NOT NULL DEFAULT 0,
  poster TEXT NOT NULL DEFAULT '',
  backdrop TEXT NOT NULL DEFAULT '',
  still TEXT NOT NULL DEFAULT '',
  logo TEXT NOT NULL DEFAULT '',
  collection_id INTEGER NOT NULL DEFAULT 0,
  collection_name TEXT NOT NULL DEFAULT '',
  child_count INTEGER NOT NULL DEFAULT 0,
  leaf_count INTEGER NOT NULL DEFAULT 0,
  browsable INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER NOT NULL DEFAULT 0,
  fetched_at INTEGER NOT NULL DEFAULT 0,
  detail INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX catalog_titles_type_browse_pop ON catalog_titles(type, browsable, popularity DESC);
CREATE INDEX catalog_titles_type_browse_rating ON catalog_titles(type, browsable, rating DESC);
CREATE INDEX catalog_titles_type_browse_released ON catalog_titles(type, browsable, released_ts DESC);
CREATE INDEX catalog_titles_type_browse_added ON catalog_titles(type, browsable, added_at DESC);
CREATE INDEX catalog_titles_sort ON catalog_titles(sort_title);
CREATE INDEX catalog_titles_tmdb ON catalog_titles(tmdb_id, type);
CREATE INDEX catalog_titles_imdb ON catalog_titles(imdb);
CREATE INDEX catalog_titles_tvdb ON catalog_titles(tvdb);
CREATE INDEX catalog_titles_show ON catalog_titles(show_key, season, episode);
CREATE INDEX catalog_titles_collection ON catalog_titles(collection_id);
CREATE INDEX catalog_titles_year ON catalog_titles(year);
CREATE TABLE catalog_genres (key INTEGER NOT NULL, genre TEXT NOT NULL, PRIMARY KEY(key, genre));
CREATE INDEX catalog_genres_genre ON catalog_genres(genre, key);
CREATE TABLE catalog_studios (key INTEGER NOT NULL, studio TEXT NOT NULL, PRIMARY KEY(key, studio));
CREATE INDEX catalog_studios_studio ON catalog_studios(studio, key);
CREATE TABLE catalog_countries (key INTEGER NOT NULL, country TEXT NOT NULL, PRIMARY KEY(key, country));
CREATE INDEX catalog_countries_country ON catalog_countries(country, key);
CREATE TABLE catalog_credits (
  key INTEGER NOT NULL, person_tmdb INTEGER NOT NULL, name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'cast', ord INTEGER NOT NULL DEFAULT 0,
  thumb TEXT NOT NULL DEFAULT '', PRIMARY KEY(key, person_tmdb, kind, role));
CREATE INDEX catalog_credits_person ON catalog_credits(person_tmdb, key);
CREATE TABLE catalog_collections (
  tmdb_id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
  poster TEXT NOT NULL DEFAULT '', backdrop TEXT NOT NULL DEFAULT '', fetched_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE catalog_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
-- prism-upgrade-migration-end
-- migration catalog 2
CREATE TABLE catalog_keywords (key INTEGER NOT NULL, keyword TEXT NOT NULL, PRIMARY KEY(key, keyword));
CREATE INDEX catalog_keywords_keyword ON catalog_keywords(keyword, key);
-- prism-upgrade-migration-end
-- migration catalog 3
ALTER TABLE catalog_titles ADD COLUMN adult INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration catalog 4
CREATE TABLE catalog_crowd (
  key INTEGER PRIMARY KEY,
  fetched_at INTEGER NOT NULL,
  pages INTEGER NOT NULL,
  recs TEXT NOT NULL
);
-- prism-upgrade-migration-end
-- migration catalog 5
ALTER TABLE catalog_titles ADD COLUMN detail_at INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration catalog 6
DELETE FROM catalog_titles WHERE
 (type='episode' AND (season < 0 OR season >= 1000 OR episode < 0 OR episode >= 10000))
 OR (type='season' AND (season < 0 OR season >= 1000));
-- prism-upgrade-migration-end
-- migration catalog 7
CREATE INDEX catalog_titles_type_browse_sort ON catalog_titles(type, browsable, adult, sort_title ASC, key ASC);
CREATE INDEX catalog_titles_browse_warm ON catalog_titles(browsable, detail, adult, popularity DESC, vote_count DESC);
CREATE INDEX catalog_titles_type_browse_year ON catalog_titles(type, browsable, adult, year DESC, popularity DESC);
CREATE INDEX catalog_titles_type_browse_lang ON catalog_titles(type, browsable, adult, language, popularity DESC);
CREATE INDEX catalog_titles_type_browse_runtime ON catalog_titles(type, browsable, adult, runtime_sec, popularity DESC);
CREATE INDEX catalog_credits_person_kind ON catalog_credits(person_tmdb, kind, key);
-- prism-upgrade-migration-end
-- migration catalog 8
CREATE TABLE catalog_trends (
 name TEXT NOT NULL, media_type TEXT NOT NULL, items TEXT NOT NULL DEFAULT '[]',
 fetched_at INTEGER NOT NULL DEFAULT 0, attempted_at INTEGER NOT NULL DEFAULT 0,
 failed INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(name,media_type));
-- prism-upgrade-migration-end
-- migration catalog 9
CREATE TABLE catalog_home_release (key INTEGER PRIMARY KEY, checked INTEGER NOT NULL DEFAULT 0, earliest_ts INTEGER NOT NULL DEFAULT 0, fallback_admitted INTEGER NOT NULL DEFAULT 0, attempted_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX catalog_home_release_pending ON catalog_home_release(checked,earliest_ts,fallback_admitted);
CREATE TABLE catalog_home_release_dates (key INTEGER NOT NULL, country TEXT NOT NULL, kind INTEGER NOT NULL, released_ts INTEGER NOT NULL, PRIMARY KEY(key,country,kind,released_ts));
-- prism-upgrade-migration-end
-- migration cluster 1
CREATE TABLE cluster_nodes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	addrs TEXT NOT NULL DEFAULT '[]',
	caps TEXT NOT NULL DEFAULT '{}',
	active_sessions INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	incarnation TEXT NOT NULL DEFAULT ''
);
CREATE INDEX cluster_nodes_last_seen ON cluster_nodes(last_seen);
CREATE TABLE cluster_leader (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	epoch INTEGER NOT NULL,
	node_id TEXT NOT NULL,
	acquired_at INTEGER NOT NULL
);
-- prism-upgrade-migration-end
-- migration cluster 2
ALTER TABLE cluster_nodes ADD COLUMN draining INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cluster_nodes ADD COLUMN migrations TEXT NOT NULL DEFAULT '';
CREATE TABLE cluster_update_lease (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	node_id TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	acquired_at INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL DEFAULT 0,
	failed_version TEXT NOT NULL DEFAULT ''
);
-- prism-upgrade-migration-end
-- migration clusterconfig 1
CREATE TABLE clusterconfig_shared(
	id INTEGER PRIMARY KEY CHECK(id=1),
	version INTEGER NOT NULL,
	doc TEXT NOT NULL,
	key_fingerprint TEXT NOT NULL,
	updated_at INTEGER NOT NULL);
CREATE TABLE clusterconfig_secrets(
	name TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL);
CREATE TABLE clusterconfig_join_tokens(
	id TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at INTEGER NOT NULL DEFAULT 0,
	used_by TEXT NOT NULL DEFAULT '');
-- prism-upgrade-migration-end
-- migration collections 2
CREATE TABLE collections_specs (
  id INTEGER PRIMARY KEY,
  slug TEXT NOT NULL DEFAULT '',
  builtin INTEGER NOT NULL DEFAULT 0,
  owner_user_id INTEGER NOT NULL DEFAULT 0,
  spec_json TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  last_built_at INTEGER NOT NULL DEFAULT 0,
  item_count INTEGER NOT NULL DEFAULT 0,
  warnings_json TEXT NOT NULL DEFAULT '[]',
  build_hash TEXT NOT NULL DEFAULT '',
  changed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX collections_specs_slug ON collections_specs(slug);
CREATE TABLE collections_members (
  spec_id INTEGER NOT NULL,
  pos INTEGER NOT NULL,
  key INTEGER NOT NULL,
  PRIMARY KEY (spec_id, pos)
);
CREATE TABLE collections_names (
  kind TEXT NOT NULL,
  query TEXT NOT NULL,
  id INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL DEFAULT '',
  at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (kind, query)
);
CREATE TABLE collections_external (
  source TEXT NOT NULL,
  ext TEXT NOT NULL,
  key INTEGER NOT NULL DEFAULT 0,
  at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (source, ext)
);
CREATE TABLE collections_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
-- prism-upgrade-migration-end
-- migration collections 3
ALTER TABLE collections_members ADD COLUMN shared INTEGER NOT NULL DEFAULT 1;
-- prism-upgrade-migration-end
-- migration collections 4
UPDATE collections_specs SET build_hash='';
-- prism-upgrade-migration-end
-- migration collections 5
CREATE TABLE collections_defaults (name TEXT PRIMARY KEY, spec_id INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration collections 6
CREATE TABLE collections_air_dates (show_key INTEGER PRIMARY KEY, episode_ts INTEGER NOT NULL, premiere_ts INTEGER NOT NULL, fetched_at INTEGER NOT NULL, attempted_at INTEGER NOT NULL, failed INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration credentials 1
CREATE TABLE credentials_format (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL,
  upgraded_at INTEGER NOT NULL,
  key_fingerprint TEXT NOT NULL DEFAULT ''
);
-- prism-upgrade-migration-end
-- migration debrid 1
CREATE TABLE debrid_payloads (
  release_key TEXT PRIMARY KEY,
  payload     TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE debrid_torrents (
  provider_id TEXT NOT NULL,
  info_hash   TEXT NOT NULL,
  torrent_id  TEXT NOT NULL DEFAULT '',
  cached      INTEGER NOT NULL DEFAULT 0,
  checked_at  INTEGER NOT NULL,
  PRIMARY KEY (provider_id, info_hash)
);
-- prism-upgrade-migration-end
-- migration debrid 2
ALTER TABLE debrid_torrents ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE debrid_torrents ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE debrid_leases (
  provider_id TEXT NOT NULL,
  torrent_id  TEXT NOT NULL,
  node_id     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  PRIMARY KEY (provider_id, torrent_id, node_id)
);
CREATE INDEX debrid_leases_node ON debrid_leases(node_id);
-- prism-upgrade-migration-end
-- migration episodeorder 1
CREATE TABLE episodeorder_preferences(user_id INTEGER NOT NULL,show_key INTEGER NOT NULL,group_id TEXT NOT NULL,specials TEXT NOT NULL DEFAULT 'last',PRIMARY KEY(user_id,show_key)); CREATE TABLE episodeorder_groups(id TEXT PRIMARY KEY,body TEXT NOT NULL,updated_at INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration invites 1
CREATE TABLE invites_links(id INTEGER PRIMARY KEY, token_hash TEXT UNIQUE NOT NULL,label TEXT NOT NULL,expires INTEGER NOT NULL,max_uses INTEGER NOT NULL,uses INTEGER NOT NULL DEFAULT 0,policy TEXT NOT NULL,sections TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0); CREATE TABLE invites_claims(invite_id INTEGER NOT NULL,plex_id INTEGER NOT NULL,state TEXT NOT NULL,PRIMARY KEY(invite_id,plex_id));
-- prism-upgrade-migration-end
-- migration invites 2
CREATE TABLE invites_pins(handle_hash TEXT PRIMARY KEY, invite_id INTEGER NOT NULL, pin_id TEXT NOT NULL, expires INTEGER NOT NULL, plex_id INTEGER NOT NULL DEFAULT 0, email TEXT NOT NULL DEFAULT '');
CREATE INDEX invites_pins_expires ON invites_pins(expires);
-- prism-upgrade-migration-end
-- migration livetv 1
CREATE TABLE livetv_channels(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_sources(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_programmes(source_id TEXT NOT NULL, channel_id TEXT NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source_id,channel_id,start_ms));
CREATE INDEX livetv_programmes_window ON livetv_programmes(channel_id,start_ms,end_ms);
CREATE TABLE livetv_policies(user_id INTEGER PRIMARY KEY, data TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration livetv 2
CREATE TABLE livetv_guide_channels(source_id TEXT NOT NULL, guide_id TEXT NOT NULL, name TEXT NOT NULL, logo TEXT NOT NULL DEFAULT '', names TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(source_id, guide_id));
CREATE INDEX livetv_programmes_guide_end ON livetv_programmes(source_id, channel_id, end_ms, start_ms);
-- prism-upgrade-migration-end
-- migration markers 1
CREATE TABLE markers_cache(rating_key INTEGER NOT NULL,runtime_ms INTEGER NOT NULL,ranges TEXT NOT NULL,expires_at INTEGER NOT NULL,PRIMARY KEY(rating_key,runtime_ms));
-- prism-upgrade-migration-end
-- migration migrate 1
CREATE TABLE migrate_plans(id TEXT PRIMARY KEY, document TEXT NOT NULL, created INTEGER NOT NULL, imported INTEGER NOT NULL DEFAULT 0); CREATE TABLE migrate_objects(source TEXT NOT NULL, viewer INTEGER NOT NULL, kind TEXT NOT NULL, external TEXT NOT NULL, local_id INTEGER NOT NULL, PRIMARY KEY(source,viewer,kind,external)); CREATE TABLE migrate_watchlist(viewer INTEGER NOT NULL,key INTEGER NOT NULL,PRIMARY KEY(viewer,key));
-- prism-upgrade-migration-end
-- migration newznab 1
CREATE TABLE newznab_budget (
  indexer_id TEXT NOT NULL,
  day TEXT NOT NULL,
  hits INTEGER NOT NULL DEFAULT 0,
  grabs INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (indexer_id, day)
);
CREATE TABLE newznab_cache (
  cache_key TEXT PRIMARY KEY,
  indexer_id TEXT NOT NULL,
  results TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX newznab_cache_expires ON newznab_cache(expires_at);
CREATE TABLE newznab_caps (
  indexer_id TEXT PRIMARY KEY,
  caps TEXT NOT NULL,
  fetched_at INTEGER NOT NULL
);
-- prism-upgrade-migration-end
-- migration newznab 2
CREATE TABLE newznab_rate_limit (
  indexer_id TEXT PRIMARY KEY,
  retry_at INTEGER NOT NULL
);
-- prism-upgrade-migration-end
-- migration newznab 3
ALTER TABLE newznab_caps ADD COLUMN endpoint TEXT NOT NULL DEFAULT '';
-- prism-upgrade-migration-end
-- migration newznab 4
CREATE TABLE newznab_feed_modes (indexer_id TEXT PRIMARY KEY, identity TEXT NOT NULL, unfiltered INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration notify 1
CREATE TABLE notify_channels(id TEXT PRIMARY KEY,config TEXT NOT NULL);
CREATE TABLE notify_queue(id INTEGER PRIMARY KEY AUTOINCREMENT,channel TEXT NOT NULL,event TEXT NOT NULL,created INTEGER NOT NULL,due INTEGER NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX notify_due ON notify_queue(status,due);
-- prism-upgrade-migration-end
-- migration notify 2
ALTER TABLE notify_queue ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE notify_queue ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration notify 3
ALTER TABLE notify_queue ADD COLUMN error TEXT NOT NULL DEFAULT '';
ALTER TABLE notify_queue ADD COLUMN immediate INTEGER NOT NULL DEFAULT 0;
CREATE TABLE notify_settings(id TEXT PRIMARY KEY,config TEXT NOT NULL);
CREATE TABLE notify_push_keys(id TEXT PRIMARY KEY,private_key TEXT NOT NULL,public_key TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration notify_once 1
CREATE TABLE notify_once(key TEXT PRIMARY KEY, at INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration overlays 1
CREATE TABLE overlays_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);CREATE TABLE overlays_artwork(item_key INTEGER PRIMARY KEY,value TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration personalize 1
CREATE TABLE personalize_row_impressions (
  user_id INTEGER NOT NULL,
  day INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  serves INTEGER NOT NULL DEFAULT 0,
  best_position INTEGER NOT NULL DEFAULT 0,
  first_at INTEGER NOT NULL,
  last_at INTEGER NOT NULL,
  device TEXT NOT NULL DEFAULT '',
  daypart TEXT NOT NULL DEFAULT '',
  weekend INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, day, hub_id)
);
CREATE INDEX personalize_row_impr_day ON personalize_row_impressions(day);
CREATE TABLE personalize_item_impressions (
  user_id INTEGER NOT NULL,
  day INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  shows INTEGER NOT NULL DEFAULT 0,
  best_rank INTEGER NOT NULL DEFAULT 0,
  first_at INTEGER NOT NULL,
  last_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, day, item_key, hub_id)
);
CREATE INDEX personalize_item_impr_lookup ON personalize_item_impressions(user_id, item_key, last_at);
CREATE INDEX personalize_item_impr_day ON personalize_item_impressions(day);
CREATE TABLE personalize_plays (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  taste_key INTEGER NOT NULL,
  hub_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  start_fraction REAL NOT NULL DEFAULT 0,
  max_fraction REAL NOT NULL DEFAULT 0,
  completed INTEGER NOT NULL DEFAULT 0,
  abandoned INTEGER NOT NULL DEFAULT 0,
  device TEXT NOT NULL DEFAULT '',
  daypart TEXT NOT NULL DEFAULT '',
  weekend INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX personalize_plays_user ON personalize_plays(user_id, started_at);
CREATE INDEX personalize_plays_started ON personalize_plays(started_at);
CREATE TABLE personalize_overrides (
  user_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  value TEXT NOT NULL,
  weight REAL NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, kind, value)
);
-- prism-upgrade-migration-end
-- migration personalize 2
CREATE TABLE personalize_home_layout (id INTEGER PRIMARY KEY CHECK(id=1), document TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration personalize 3
CREATE TABLE personalize_serves (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL,
  first_at INTEGER NOT NULL,
  last_at INTEGER NOT NULL,
  surface TEXT NOT NULL,
  device TEXT NOT NULL DEFAULT '',
  daypart TEXT NOT NULL DEFAULT '',
  weekend INTEGER NOT NULL DEFAULT 0,
  repeats INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX personalize_serves_user ON personalize_serves(user_id, last_at);
CREATE INDEX personalize_serves_last ON personalize_serves(last_at);
CREATE TABLE personalize_serve_items (
  serve_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  item_key INTEGER NOT NULL,
  taste_key INTEGER NOT NULL,
  hub_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  row_pos INTEGER NOT NULL,
  item_pos INTEGER NOT NULL,
  baseline REAL NOT NULL DEFAULT 0,
  explore REAL NOT NULL DEFAULT 0,
  propensity REAL NOT NULL DEFAULT 0,
  fv INTEGER NOT NULL DEFAULT 0,
  features BLOB,
  label INTEGER NOT NULL DEFAULT -1,
  play_id INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (serve_id, row_pos, item_pos)
);
CREATE INDEX personalize_serve_items_user ON personalize_serve_items(user_id, taste_key);
CREATE INDEX personalize_serve_items_label ON personalize_serve_items(label);
CREATE TABLE personalize_ranker (
  id INTEGER PRIMARY KEY CHECK(id=1),
  trained_at INTEGER NOT NULL,
  model TEXT NOT NULL
);
-- prism-upgrade-migration-end
-- migration personalize_discovery 1
CREATE TABLE personalize_discovery_hidden(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,hidden_at INTEGER NOT NULL,PRIMARY KEY(user_id,item_key));
-- prism-upgrade-migration-end
-- migration playback 1
CREATE TABLE playback_resume (
	user_id      INTEGER NOT NULL,
	rating_key   INTEGER NOT NULL,
	release_id   TEXT    NOT NULL,
	release_json TEXT    NOT NULL,
	nzb_hash     TEXT    NOT NULL,
	profile      TEXT    NOT NULL DEFAULT '',
	size         INTEGER NOT NULL DEFAULT 0,
	duration_ms  INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (user_id, rating_key)
);
CREATE INDEX playback_resume_release ON playback_resume(release_id);
CREATE INDEX playback_resume_hash ON playback_resume(nzb_hash);
-- prism-upgrade-migration-end
-- migration playback 2
CREATE TABLE playback_live_sessions (
	id         TEXT    PRIMARY KEY,
	node_id    TEXT    NOT NULL,
	user_id    INTEGER NOT NULL,
	client_id  TEXT    NOT NULL DEFAULT '',
	grp        TEXT    NOT NULL DEFAULT '',
	kind       TEXT    NOT NULL,
	data       TEXT    NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE INDEX playback_live_sessions_node ON playback_live_sessions(node_id);
CREATE INDEX playback_live_sessions_updated ON playback_live_sessions(updated_at);
-- prism-upgrade-migration-end
-- migration playback 3
ALTER TABLE playback_resume ADD COLUMN original_language TEXT NOT NULL DEFAULT '';
-- prism-upgrade-migration-end
-- migration playback_index 1
CREATE TABLE playback_index (
	user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, release_id TEXT NOT NULL,
	points_json TEXT NOT NULL, PRIMARY KEY (user_id, rating_key, release_id)
);
-- prism-upgrade-migration-end
-- migration playback_probe 1
CREATE TABLE playback_probe (
	user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, release_id TEXT NOT NULL,
	nzb_hash TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0,
	info_json TEXT NOT NULL, updated_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, rating_key, release_id)
);
-- prism-upgrade-migration-end
-- migration plex 1
CREATE TABLE plex_ultrablur (
		key          INTEGER PRIMARY KEY,
		backdrop     TEXT    NOT NULL,
		top_left     TEXT    NOT NULL,
		top_right    TEXT    NOT NULL,
		bottom_right TEXT    NOT NULL,
		bottom_left  TEXT    NOT NULL,
		computed_at  INTEGER NOT NULL
	)
-- prism-upgrade-migration-end
-- migration plex 2
CREATE TABLE plex_sessions (
	id          TEXT    PRIMARY KEY,
	node_id     TEXT    NOT NULL,
	user_id     INTEGER NOT NULL,
	session_key INTEGER NOT NULL,
	data        TEXT    NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX plex_sessions_node ON plex_sessions(node_id);
CREATE TABLE plex_counters (
	name  TEXT    PRIMARY KEY,
	value INTEGER NOT NULL
);
INSERT INTO plex_counters(name, value) VALUES ('session_key', 0);
INSERT INTO plex_counters(name, value) VALUES ('playqueue_item', 0);
CREATE TABLE plex_playqueues (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	rev        INTEGER NOT NULL,
	data       TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX plex_playqueues_user ON plex_playqueues(user_id);
CREATE INDEX plex_playqueues_updated ON plex_playqueues(updated_at);
-- prism-upgrade-migration-end
-- migration plexguid 1
CREATE TABLE plexguid_mappings(rating_key INTEGER PRIMARY KEY,guid TEXT NOT NULL,checked_at INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration plexguid 2
CREATE INDEX plexguid_mappings_guid ON plexguid_mappings(guid);
-- prism-upgrade-migration-end
-- migration ratings 1
CREATE TABLE ratings_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);CREATE TABLE ratings_cache(item_key INTEGER NOT NULL,provider TEXT NOT NULL,value TEXT NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(item_key,provider));
-- prism-upgrade-migration-end
-- migration resolver 1
CREATE TABLE resolver_bad (
  release_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE resolver_availability (
  rating_key INTEGER PRIMARY KEY,
  available INTEGER NOT NULL,
  checked_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE resolver_reputation (
  indexer TEXT PRIMARY KEY,
  successes INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0
);
-- prism-upgrade-migration-end
-- migration resolver 2
CREATE TABLE resolver_verified (
  release_id TEXT PRIMARY KEY,
  checked_at INTEGER NOT NULL
);
-- prism-upgrade-migration-end
-- migration resolver 3
ALTER TABLE resolver_bad ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN indexer TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN rating_key INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resolver_bad ADD COLUMN title_name TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN source TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE resolver_bad ADD COLUMN manual INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resolver_bad ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE resolver_bad SET updated_at = created_at;
CREATE INDEX resolver_bad_updated ON resolver_bad(updated_at);
CREATE INDEX resolver_bad_title ON resolver_bad(rating_key);
CREATE TABLE resolver_pins (
  rating_key INTEGER PRIMARY KEY,
  release_id TEXT NOT NULL,
  release_json TEXT NOT NULL,
  title_name TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  pinned_at INTEGER NOT NULL,
  last_status TEXT NOT NULL DEFAULT '',
  last_reason TEXT NOT NULL DEFAULT '',
  last_checked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE resolver_history (
  id INTEGER PRIMARY KEY,
  resolved_at INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  title_name TEXT NOT NULL DEFAULT '',
  profile TEXT NOT NULL DEFAULT '',
  release_id TEXT NOT NULL DEFAULT '',
  release_name TEXT NOT NULL DEFAULT '',
  indexer TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  failover INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0,
  candidates INTEGER NOT NULL DEFAULT 0,
  trace TEXT NOT NULL DEFAULT ''
);
CREATE INDEX resolver_history_at ON resolver_history(resolved_at);
CREATE INDEX resolver_history_title ON resolver_history(rating_key, resolved_at);
-- prism-upgrade-migration-end
-- migration safety 1
CREATE TABLE safety_policies(user_id INTEGER PRIMARY KEY, policy TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration segcache 1
CREATE TABLE segcache_pool (id INTEGER PRIMARY KEY CHECK(id=1), token TEXT NOT NULL, used INTEGER NOT NULL DEFAULT 0);
CREATE TABLE segcache_segments (release_id TEXT NOT NULL, segment INTEGER NOT NULL, file TEXT NOT NULL, size INTEGER NOT NULL, touched INTEGER NOT NULL, PRIMARY KEY(release_id,segment));
CREATE INDEX segcache_segments_age ON segcache_segments(touched);
CREATE TABLE segcache_releases (release_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, plays INTEGER NOT NULL DEFAULT 0, played INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0, segments INTEGER NOT NULL DEFAULT 0, touched INTEGER NOT NULL DEFAULT 0);
CREATE TABLE segcache_pins (release_id TEXT NOT NULL, reason TEXT NOT NULL, owner TEXT NOT NULL, expires INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(release_id,reason,owner));
CREATE TABLE segcache_files (file TEXT PRIMARY KEY, created INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration segcache_reclaim 1
CREATE TABLE segcache_reclaim(id TEXT PRIMARY KEY, node TEXT NOT NULL, bytes INTEGER NOT NULL, at INTEGER NOT NULL);
-- prism-upgrade-migration-end
-- migration simkl 1
CREATE TABLE simkl_tokens (
 user_id INTEGER PRIMARY KEY,
 client_id TEXT NOT NULL,
 access_token TEXT NOT NULL,
 refresh_token TEXT NOT NULL,
 expires_at INTEGER NOT NULL,
 snapshot TEXT NOT NULL DEFAULT '',
 lease_id TEXT NOT NULL DEFAULT '',
 lease_until INTEGER NOT NULL DEFAULT 0,
 backoff_until INTEGER NOT NULL DEFAULT 0,
 next_post INTEGER NOT NULL DEFAULT 0,
 last_warning TEXT NOT NULL DEFAULT ''
 );
 CREATE TABLE simkl_post_pacing (
 id INTEGER PRIMARY KEY,
 next_post INTEGER NOT NULL DEFAULT 0
 );
 INSERT INTO simkl_post_pacing(id,next_post) VALUES(1,0);
 CREATE TABLE simkl_devices (
 user_id INTEGER PRIMARY KEY,
 generation TEXT NOT NULL,
 client_id TEXT NOT NULL,
 device_code TEXT NOT NULL DEFAULT '',
 expires_at INTEGER NOT NULL DEFAULT 0,
 next_poll INTEGER NOT NULL DEFAULT 0,
 interval_seconds INTEGER NOT NULL DEFAULT 0,
 claim_id TEXT NOT NULL DEFAULT '',
 claim_until INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE simkl_outbox (
 user_id INTEGER NOT NULL,
 rating_key INTEGER NOT NULL,
 watched_at INTEGER NOT NULL,
 PRIMARY KEY(user_id,rating_key,watched_at),
 FOREIGN KEY(user_id) REFERENCES simkl_tokens(user_id) ON DELETE CASCADE
 );
-- prism-upgrade-migration-end
-- migration state 1
CREATE TABLE state_watch (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  view_offset_ns INTEGER NOT NULL DEFAULT 0,
  duration_ns INTEGER NOT NULL DEFAULT 0,
  view_count INTEGER NOT NULL DEFAULT 0,
  counted INTEGER NOT NULL DEFAULT 0,
  last_viewed_at INTEGER NOT NULL DEFAULT 0,
  user_rating REAL NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key)
);
CREATE INDEX state_watch_inprogress ON state_watch(user_id, last_viewed_at DESC) WHERE view_offset_ns > 0;
CREATE TABLE state_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  viewed_at INTEGER NOT NULL,
  completed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX state_history_user ON state_history(user_id, viewed_at DESC);
CREATE TABLE state_playlists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  title TEXT NOT NULL,
  smart INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE state_playlist_items (
  playlist_id INTEGER NOT NULL REFERENCES state_playlists(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  PRIMARY KEY (playlist_id, position)
);
-- prism-upgrade-migration-end
-- migration state 2
ALTER TABLE state_watch ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_history ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX state_history_imported ON state_history(user_id, rating_key, viewed_at) WHERE imported=1;
-- prism-upgrade-migration-end
-- migration stats 1
CREATE TABLE stats_plays (id TEXT PRIMARY KEY,user_id INTEGER NOT NULL,rating_key INTEGER NOT NULL,username TEXT NOT NULL,title TEXT NOT NULL,platform TEXT NOT NULL,device TEXT NOT NULL,decision TEXT NOT NULL,started INTEGER NOT NULL,updated INTEGER NOT NULL,ended INTEGER NOT NULL DEFAULT 0,bytes INTEGER NOT NULL DEFAULT 0,watched_ms INTEGER NOT NULL DEFAULT 0,duration_ms INTEGER NOT NULL DEFAULT 0,completed INTEGER NOT NULL DEFAULT 0,failovers INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',timeline INTEGER NOT NULL DEFAULT 0,timeline_state TEXT NOT NULL DEFAULT '');
CREATE INDEX stats_plays_started ON stats_plays(started DESC);
CREATE INDEX stats_plays_user ON stats_plays(user_id,started DESC);
CREATE TABLE stats_provider_days (provider TEXT NOT NULL,day TEXT NOT NULL,bytes INTEGER NOT NULL,articles INTEGER NOT NULL,misses INTEGER NOT NULL,errors INTEGER NOT NULL,PRIMARY KEY(provider,day));
CREATE TABLE stats_snapshots (kind TEXT PRIMARY KEY,payload TEXT NOT NULL,at INTEGER NOT NULL);
CREATE TABLE stats_costs (kind TEXT NOT NULL,id TEXT NOT NULL,monthly REAL NOT NULL,currency TEXT NOT NULL,PRIMARY KEY(kind,id));
-- prism-upgrade-migration-end
-- migration stats 2
CREATE TABLE stats_indexer_days(indexer TEXT NOT NULL,day TEXT NOT NULL,searches INTEGER NOT NULL DEFAULT 0,successes INTEGER NOT NULL DEFAULT 0,grabs INTEGER NOT NULL DEFAULT 0,grab_success INTEGER NOT NULL DEFAULT 0,budgets INTEGER NOT NULL DEFAULT 0,latency_ms INTEGER NOT NULL DEFAULT 0,age_seconds INTEGER NOT NULL DEFAULT 0,age_samples INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(indexer,day));
-- prism-upgrade-migration-end
-- migration stats 3
ALTER TABLE stats_provider_days ADD COLUMN backfill_saves INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration stats 4
ALTER TABLE stats_provider_days ADD COLUMN body_misses INTEGER NOT NULL DEFAULT 0;ALTER TABLE stats_provider_days ADD COLUMN body_takedowns INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration stats 5
CREATE TABLE stats_newsletter_settings(id INTEGER PRIMARY KEY CHECK(id=1),enabled INTEGER NOT NULL DEFAULT 0,weekday INTEGER NOT NULL DEFAULT 1,hour INTEGER NOT NULL DEFAULT 9,user_id INTEGER NOT NULL DEFAULT 0,next_due INTEGER NOT NULL DEFAULT 0);INSERT INTO stats_newsletter_settings(id) VALUES(1);CREATE TABLE stats_newsletter_ledger(due INTEGER PRIMARY KEY,attempted INTEGER NOT NULL,status TEXT NOT NULL);
-- prism-upgrade-migration-end
-- migration stats 6
ALTER TABLE stats_plays ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
CREATE INDEX stats_plays_open ON stats_plays(node_id) WHERE ended=0;
-- prism-upgrade-migration-end
-- migration trackprefs 1
CREATE TABLE trackprefs_rules(user_id INTEGER PRIMARY KEY, rules TEXT NOT NULL); CREATE TABLE trackprefs_memory(user_id INTEGER NOT NULL, show_id INTEGER NOT NULL, kind TEXT NOT NULL, choice TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(user_id,show_id,kind));
-- prism-upgrade-migration-end
-- migration trakt 1
CREATE TABLE trakt_tokens (
  user_id INTEGER PRIMARY KEY,
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  username TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
-- prism-upgrade-migration-end
-- migration trakt 2
CREATE TABLE trakt_import_queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES trakt_tokens(user_id) ON DELETE CASCADE,
  username TEXT NOT NULL,
  event_json TEXT NOT NULL
);
-- prism-upgrade-migration-end
-- migration trakt 3
ALTER TABLE trakt_import_queue ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE trakt_import_queue ADD COLUMN claimed_at INTEGER NOT NULL DEFAULT 0;
-- prism-upgrade-migration-end
-- migration variants 1
CREATE TABLE variants_ids (
  media_id INTEGER PRIMARY KEY,
  rating_key INTEGER NOT NULL,
  release_id TEXT NOT NULL
);
CREATE UNIQUE INDEX variants_ids_pair ON variants_ids(rating_key, release_id);
-- prism-upgrade-migration-end
-- migration watchlist 1
CREATE TABLE watchlist_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);
CREATE TABLE watchlist_items(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,source TEXT NOT NULL,title TEXT NOT NULL,added INTEGER NOT NULL,checked INTEGER NOT NULL DEFAULT 0,available INTEGER NOT NULL DEFAULT 0,notified INTEGER NOT NULL DEFAULT 0,warmed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,item_key,source));
CREATE INDEX watchlist_viewer ON watchlist_items(user_id,available,added);
-- prism-upgrade-migration-end
-- migration watchlist 2
CREATE TABLE watchlist_meta(k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
-- prism-upgrade-migration-end
-- migration watchlist_episodes 1
CREATE TABLE watchlist_episodes(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,episode_key INTEGER NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(user_id,item_key,episode_key));
CREATE TABLE watchlist_episodes_alerts(episode_key INTEGER PRIMARY KEY,label TEXT NOT NULL,at INTEGER NOT NULL,sent INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0);
-- prism-upgrade-migration-end
