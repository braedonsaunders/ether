package resolver

// dictionarrySchemaSQL embeds the Dictionarry-Hub/schema 1.1.0 relational
// schema (ops/0.schema.sql, ops/1.languages.sql, ops/2.qualities.sql, and
// ops/3.quality-group-member-position.sql) used to evaluate Dictionarry v2
// SQL operation sets in an isolated in-memory SQLite database.
const dictionarrySchemaSQL = `
CREATE TABLE tags (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(50) UNIQUE NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE languages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(30) UNIQUE NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE regular_expressions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(100) UNIQUE NOT NULL,
    pattern TEXT NOT NULL,
    regex101_id VARCHAR(50),
    description TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE qualities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(100) UNIQUE NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE quality_api_mappings (
    quality_name VARCHAR(100) NOT NULL,
    arr_type VARCHAR(20) NOT NULL,
    api_name VARCHAR(100) NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (quality_name, arr_type),
    FOREIGN KEY (quality_name) REFERENCES qualities(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE custom_formats (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    include_in_rename INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE quality_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    upgrades_allowed INTEGER NOT NULL DEFAULT 1,
    minimum_custom_format_score INTEGER NOT NULL DEFAULT 0,
    upgrade_until_score INTEGER NOT NULL DEFAULT 0,
    upgrade_score_increment INTEGER NOT NULL DEFAULT 1 CHECK (upgrade_score_increment > 0),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE quality_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    quality_profile_name VARCHAR(100) NOT NULL,
    name VARCHAR(100) NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(quality_profile_name, name),
    FOREIGN KEY (quality_profile_name) REFERENCES quality_profiles(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE custom_format_conditions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    custom_format_name VARCHAR(100) NOT NULL,
    name VARCHAR(100) NOT NULL,
    type VARCHAR(50) NOT NULL,
    arr_type VARCHAR(20) NOT NULL DEFAULT 'all',
    negate INTEGER NOT NULL DEFAULT 0,
    required INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(custom_format_name, name),
    FOREIGN KEY (custom_format_name) REFERENCES custom_formats(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE regular_expression_tags (
    regular_expression_name VARCHAR(100) NOT NULL,
    tag_name VARCHAR(50) NOT NULL,
    PRIMARY KEY (regular_expression_name, tag_name),
    FOREIGN KEY (regular_expression_name) REFERENCES regular_expressions(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (tag_name) REFERENCES tags(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE custom_format_tags (
    custom_format_name VARCHAR(100) NOT NULL,
    tag_name VARCHAR(50) NOT NULL,
    PRIMARY KEY (custom_format_name, tag_name),
    FOREIGN KEY (custom_format_name) REFERENCES custom_formats(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (tag_name) REFERENCES tags(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE quality_profile_tags (
    quality_profile_name VARCHAR(100) NOT NULL,
    tag_name VARCHAR(50) NOT NULL,
    PRIMARY KEY (quality_profile_name, tag_name),
    FOREIGN KEY (quality_profile_name) REFERENCES quality_profiles(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (tag_name) REFERENCES tags(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE quality_profile_languages (
    quality_profile_name VARCHAR(100) NOT NULL,
    language_name VARCHAR(30) NOT NULL,
    type VARCHAR(20) NOT NULL DEFAULT 'simple',
    PRIMARY KEY (quality_profile_name, language_name),
    FOREIGN KEY (quality_profile_name) REFERENCES quality_profiles(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (language_name) REFERENCES languages(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE quality_group_members (
    quality_profile_name VARCHAR(100) NOT NULL,
    quality_group_name VARCHAR(100) NOT NULL,
    quality_name VARCHAR(100) NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (quality_profile_name, quality_group_name, quality_name),
    FOREIGN KEY (quality_profile_name, quality_group_name) REFERENCES quality_groups(quality_profile_name, name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (quality_name) REFERENCES qualities(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE quality_profile_qualities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    quality_profile_name VARCHAR(100) NOT NULL,
    quality_name VARCHAR(100),
    quality_group_name VARCHAR(100),
    position INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    upgrade_until INTEGER NOT NULL DEFAULT 0,
    CHECK ((quality_name IS NOT NULL AND quality_group_name IS NULL) OR (quality_name IS NULL AND quality_group_name IS NOT NULL)),
    FOREIGN KEY (quality_profile_name) REFERENCES quality_profiles(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (quality_name) REFERENCES qualities(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (quality_profile_name, quality_group_name) REFERENCES quality_groups(quality_profile_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE quality_profile_custom_formats (
    quality_profile_name VARCHAR(100) NOT NULL,
    custom_format_name VARCHAR(100) NOT NULL,
    arr_type VARCHAR(20) NOT NULL,
    score INTEGER NOT NULL,
    PRIMARY KEY (quality_profile_name, custom_format_name, arr_type),
    FOREIGN KEY (quality_profile_name) REFERENCES quality_profiles(name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (custom_format_name) REFERENCES custom_formats(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_patterns (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    regular_expression_name VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (regular_expression_name) REFERENCES regular_expressions(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_languages (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    language_name VARCHAR(30) NOT NULL,
    except_language INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (language_name) REFERENCES languages(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_indexer_flags (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    flag VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_sources (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    source VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_resolutions (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    resolution VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_quality_modifiers (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    quality_modifier VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_sizes (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    min_bytes INTEGER,
    max_bytes INTEGER,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_release_types (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    release_type VARCHAR(100) NOT NULL,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE condition_years (
    custom_format_name VARCHAR(100) NOT NULL,
    condition_name VARCHAR(100) NOT NULL,
    min_year INTEGER,
    max_year INTEGER,
    PRIMARY KEY (custom_format_name, condition_name),
    FOREIGN KEY (custom_format_name, condition_name) REFERENCES custom_format_conditions(custom_format_name, name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE custom_format_tests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    custom_format_name VARCHAR(100) NOT NULL,
    title TEXT NOT NULL,
    type VARCHAR(20) NOT NULL,
    should_match INTEGER NOT NULL,
    description TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(custom_format_name, title, type),
    FOREIGN KEY (custom_format_name) REFERENCES custom_formats(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE radarr_quality_definitions (
    name VARCHAR(100) NOT NULL,
    quality_name VARCHAR(100) NOT NULL,
    min_size INTEGER NOT NULL DEFAULT 0,
    max_size INTEGER NOT NULL,
    preferred_size INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (name, quality_name),
    FOREIGN KEY (quality_name) REFERENCES qualities(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE sonarr_quality_definitions (
    name VARCHAR(100) NOT NULL,
    quality_name VARCHAR(100) NOT NULL,
    min_size INTEGER NOT NULL DEFAULT 0,
    max_size INTEGER NOT NULL,
    preferred_size INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (name, quality_name),
    FOREIGN KEY (quality_name) REFERENCES qualities(name) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE radarr_naming (
    name VARCHAR(100) NOT NULL PRIMARY KEY,
    rename INTEGER NOT NULL DEFAULT 1,
    movie_format TEXT NOT NULL,
    movie_folder_format TEXT NOT NULL,
    replace_illegal_characters INTEGER NOT NULL DEFAULT 0,
    colon_replacement_format VARCHAR(20) NOT NULL DEFAULT 'smart'
        CHECK (colon_replacement_format IN ('delete', 'dash', 'spaceDash', 'spaceDashSpace', 'smart')),
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sonarr_naming (
    name VARCHAR(100) NOT NULL PRIMARY KEY,
    rename INTEGER NOT NULL DEFAULT 1,
    standard_episode_format TEXT NOT NULL,
    daily_episode_format TEXT NOT NULL,
    anime_episode_format TEXT NOT NULL,
    series_folder_format TEXT NOT NULL,
    season_folder_format TEXT NOT NULL,
    replace_illegal_characters INTEGER NOT NULL DEFAULT 0,
    colon_replacement_format INTEGER NOT NULL DEFAULT 4,
    custom_colon_replacement_format TEXT,
    multi_episode_style INTEGER NOT NULL DEFAULT 5,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE radarr_media_settings (
    name VARCHAR(100) NOT NULL PRIMARY KEY,
    propers_repacks VARCHAR(50) NOT NULL DEFAULT 'doNotPrefer'
        CHECK (propers_repacks IN ('doNotPrefer', 'preferAndUpgrade', 'doNotUpgradeAutomatically')),
    enable_media_info INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sonarr_media_settings (
    name VARCHAR(100) NOT NULL PRIMARY KEY,
    propers_repacks VARCHAR(50) NOT NULL DEFAULT 'doNotPrefer'
        CHECK (propers_repacks IN ('doNotPrefer', 'preferAndUpgrade', 'doNotUpgradeAutomatically')),
    enable_media_info INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE delay_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name VARCHAR(100) UNIQUE NOT NULL,
    preferred_protocol VARCHAR(20) NOT NULL CHECK (
        preferred_protocol IN ('prefer_usenet', 'prefer_torrent', 'only_usenet', 'only_torrent')
    ),
    usenet_delay INTEGER,
    torrent_delay INTEGER,
    bypass_if_highest_quality INTEGER NOT NULL DEFAULT 0,
    bypass_if_above_custom_format_score INTEGER NOT NULL DEFAULT 0,
    minimum_custom_format_score INTEGER,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE test_entities (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL CHECK (type IN ('movie', 'series')),
    tmdb_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    year INTEGER,
    poster_path TEXT,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(type, tmdb_id)
);

CREATE TABLE test_releases (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL CHECK (entity_type IN ('movie', 'series')),
    entity_tmdb_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    size_bytes INTEGER,
    languages TEXT NOT NULL DEFAULT '[]',
    indexers TEXT NOT NULL DEFAULT '[]',
    flags TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (entity_type, entity_tmdb_id) REFERENCES test_entities(type, tmdb_id) ON DELETE CASCADE
);

CREATE INDEX idx_test_releases_entity ON test_releases(entity_type, entity_tmdb_id);

CREATE UNIQUE INDEX idx_one_upgrade_until_per_profile
ON quality_profile_qualities(quality_profile_name)
WHERE upgrade_until = 1;

INSERT INTO languages (name) VALUES
('Unknown'), ('English'), ('French'), ('Spanish'), ('German'), ('Italian'),
('Danish'), ('Dutch'), ('Japanese'), ('Icelandic'), ('Chinese'), ('Russian'),
('Polish'), ('Vietnamese'), ('Swedish'), ('Norwegian'), ('Finnish'), ('Turkish'),
('Portuguese'), ('Flemish'), ('Greek'), ('Korean'), ('Hungarian'), ('Hebrew'),
('Lithuanian'), ('Czech'), ('Hindi'), ('Romanian'), ('Thai'), ('Bulgarian'),
('Portuguese (Brazil)'), ('Arabic'), ('Ukrainian'), ('Persian'), ('Bengali'),
('Slovak'), ('Latvian'), ('Spanish (Latino)'), ('Catalan'), ('Croatian'),
('Serbian'), ('Bosnian'), ('Estonian'), ('Tamil'), ('Indonesian'), ('Telugu'),
('Macedonian'), ('Slovenian'), ('Malayalam'), ('Kannada'), ('Albanian'),
('Afrikaans'), ('Marathi'), ('Tagalog'), ('Urdu'), ('Romansh'), ('Mongolian'),
('Georgian'), ('Azerbaijani'), ('Uzbek'), ('Malay'), ('Any'), ('Original');

INSERT INTO qualities (name) VALUES
('Unknown'), ('WORKPRINT'), ('CAM'), ('TELESYNC'), ('TELECINE'), ('DVDSCR'),
('REGIONAL'), ('SDTV'), ('DVD'), ('DVD-R'), ('HDTV-480p'), ('HDTV-720p'),
('HDTV-1080p'), ('HDTV-2160p'), ('WEBDL-480p'), ('WEBDL-720p'), ('WEBDL-1080p'),
('WEBDL-2160p'), ('WEBRip-480p'), ('WEBRip-720p'), ('WEBRip-1080p'),
('WEBRip-2160p'), ('Bluray-480p'), ('Bluray-576p'), ('Bluray-720p'),
('Bluray-1080p'), ('Bluray-2160p'), ('Remux-1080p'), ('Remux-2160p'),
('BR-DISK'), ('Raw-HD');

INSERT INTO quality_api_mappings (quality_name, arr_type, api_name)
SELECT name, 'radarr', name FROM qualities WHERE name IN (
    'Unknown', 'WORKPRINT', 'CAM', 'TELESYNC', 'TELECINE', 'DVDSCR', 'REGIONAL',
    'SDTV', 'DVD', 'DVD-R', 'HDTV-720p', 'HDTV-1080p', 'HDTV-2160p',
    'WEBDL-480p', 'WEBDL-720p', 'WEBDL-1080p', 'WEBDL-2160p',
    'WEBRip-480p', 'WEBRip-720p', 'WEBRip-1080p', 'WEBRip-2160p',
    'Bluray-480p', 'Bluray-576p', 'Bluray-720p', 'Bluray-1080p', 'Bluray-2160p',
    'Remux-1080p', 'Remux-2160p', 'BR-DISK', 'Raw-HD'
);

INSERT INTO quality_api_mappings (quality_name, arr_type, api_name)
SELECT name, 'sonarr', name FROM qualities WHERE name IN (
    'Unknown', 'SDTV', 'DVD', 'HDTV-720p', 'HDTV-1080p', 'HDTV-2160p',
    'WEBDL-480p', 'WEBDL-720p', 'WEBDL-1080p', 'WEBDL-2160p',
    'WEBRip-480p', 'WEBRip-720p', 'WEBRip-1080p', 'WEBRip-2160p',
    'Bluray-480p', 'Bluray-576p', 'Bluray-720p', 'Bluray-1080p', 'Bluray-2160p',
    'Raw-HD'
);

INSERT INTO quality_api_mappings (quality_name, arr_type, api_name)
SELECT name, 'sonarr', 'Bluray-1080p Remux' FROM qualities WHERE name = 'Remux-1080p';

INSERT INTO quality_api_mappings (quality_name, arr_type, api_name)
SELECT name, 'sonarr', 'Bluray-2160p Remux' FROM qualities WHERE name = 'Remux-2160p';
`
