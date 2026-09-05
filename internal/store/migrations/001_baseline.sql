CREATE TABLE instances (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    base_url TEXT NOT NULL,
    reconciliation_cursor TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unknown',
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE media (
    id INTEGER PRIMARY KEY,
    instance TEXT NOT NULL,
    kind TEXT NOT NULL,
    file_id INTEGER NOT NULL,
    entity_id INTEGER NOT NULL CHECK (entity_id > 0),
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    mod_time_ns INTEGER NOT NULL,
    title TEXT NOT NULL,
    episode_title TEXT NOT NULL DEFAULT '',
    alternate_titles_json BLOB NOT NULL DEFAULT '[]',
    year INTEGER NOT NULL DEFAULT 0,
    season INTEGER NOT NULL DEFAULT 0,
    episode INTEGER NOT NULL DEFAULT 0,
    absolute_episode INTEGER NOT NULL DEFAULT 0,
    imdb_id TEXT NOT NULL DEFAULT '',
    tmdb_id INTEGER NOT NULL DEFAULT 0,
    tvdb_id INTEGER NOT NULL DEFAULT 0,
    original_filename TEXT NOT NULL DEFAULT '',
    release_name TEXT NOT NULL DEFAULT '',
    release_group TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    resolution TEXT NOT NULL DEFAULT '',
    streaming_service TEXT NOT NULL DEFAULT '',
    edition TEXT NOT NULL DEFAULT '',
    quality TEXT NOT NULL DEFAULT '',
    duration_ns INTEGER NOT NULL DEFAULT 0,
    unsupported_reason TEXT NOT NULL DEFAULT '',
    updated_at_ns INTEGER NOT NULL,
    UNIQUE(instance, kind, file_id)
);

CREATE UNIQUE INDEX media_entity_identity_idx ON media(instance, kind, entity_id);

CREATE TABLE tracks (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    stream_index INTEGER NOT NULL DEFAULT 0,
    path TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL,
    codec TEXT NOT NULL DEFAULT '',
    embedded INTEGER NOT NULL,
    forced INTEGER NOT NULL,
    is_default INTEGER NOT NULL,
    sdh INTEGER NOT NULL,
    protected INTEGER NOT NULL,
    checksum TEXT NOT NULL DEFAULT ''
);

CREATE INDEX tracks_media_language_idx ON tracks(media_id, language);

CREATE TABLE search_states (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    language TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    attempt INTEGER NOT NULL DEFAULT 0,
    failure_attempt INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ns INTEGER NOT NULL,
    last_outcome TEXT NOT NULL DEFAULT '',
    lease_owner TEXT,
    lease_until_ns INTEGER,
    priority INTEGER NOT NULL DEFAULT 200,
    rerun_requested INTEGER NOT NULL DEFAULT 0 CHECK (rerun_requested IN (0, 1)),
    UNIQUE(media_id, language)
);

CREATE INDEX search_due_idx ON search_states(state, priority DESC, next_attempt_at_ns, lease_until_ns);

CREATE TABLE provider_states (
    provider_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    quota_limit INTEGER NOT NULL DEFAULT 0,
    quota_remaining INTEGER NOT NULL DEFAULT 0,
    reset_at_ns INTEGER NOT NULL DEFAULT 0,
    disabled INTEGER NOT NULL DEFAULT 0,
    failure_attempt INTEGER NOT NULL DEFAULT 0,
    quota_json BLOB NOT NULL DEFAULT '{}',
    PRIMARY KEY(provider_id, scope)
);

CREATE TABLE provider_cache (
    cache_key TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL,
    results_json BLOB NOT NULL,
    expires_at_ns INTEGER NOT NULL
);

CREATE INDEX provider_cache_expiry_idx ON provider_cache(expires_at_ns);

CREATE TABLE pack_cache (
    id INTEGER PRIMARY KEY,
    provider_id TEXT NOT NULL,
    result_id TEXT NOT NULL,
    series_key TEXT NOT NULL,
    season INTEGER NOT NULL,
    language TEXT NOT NULL,
    content_checksum TEXT NOT NULL,
    manifest_path TEXT NOT NULL,
    candidate_json BLOB NOT NULL DEFAULT '{}',
    byte_size INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    last_access_at_ns INTEGER NOT NULL,
    UNIQUE(provider_id, result_id, language, content_checksum)
);

CREATE INDEX pack_lookup_idx ON pack_cache(series_key, season, language, expires_at_ns);
CREATE INDEX pack_lru_idx ON pack_cache(expires_at_ns, last_access_at_ns);

CREATE TABLE pack_members (
    id INTEGER PRIMARY KEY,
    pack_id INTEGER NOT NULL REFERENCES pack_cache(id) ON DELETE CASCADE,
    safe_name TEXT NOT NULL,
    cache_path TEXT NOT NULL,
    checksum TEXT NOT NULL,
    season INTEGER NOT NULL DEFAULT 0,
    episode_from INTEGER NOT NULL DEFAULT 0,
    episode_to INTEGER NOT NULL DEFAULT 0,
    absolute_from INTEGER NOT NULL DEFAULT 0,
    absolute_to INTEGER NOT NULL DEFAULT 0,
    normalized_title TEXT NOT NULL DEFAULT '',
    forced INTEGER NOT NULL DEFAULT 0,
    UNIQUE(pack_id, safe_name)
);

CREATE TABLE candidates (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    language TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    result_id TEXT NOT NULL,
    metadata_json BLOB NOT NULL,
    score_json BLOB NOT NULL,
    validation_json BLOB NOT NULL DEFAULT '{}',
    created_at_ns INTEGER NOT NULL
);

CREATE TABLE installations (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    language TEXT NOT NULL,
    path TEXT NOT NULL,
    checksum TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    candidate_id TEXT NOT NULL DEFAULT '',
    score_json BLOB NOT NULL DEFAULT '{}',
    sync_result_json BLOB NOT NULL DEFAULT '{}',
    rollback_path TEXT NOT NULL DEFAULT '',
    installed_at_ns INTEGER NOT NULL,
    media_path TEXT NOT NULL DEFAULT '',
    media_file_id INTEGER NOT NULL DEFAULT 0,
    media_size INTEGER NOT NULL DEFAULT 0,
    media_mod_time_ns INTEGER NOT NULL DEFAULT 0,
    UNIQUE(media_id, language)
);

CREATE TABLE notifications (
    id INTEGER PRIMARY KEY,
    notifier TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ns INTEGER NOT NULL,
    result TEXT NOT NULL DEFAULT '',
    lease_owner TEXT,
    lease_until_ns INTEGER,
    dedupe_key TEXT
);

CREATE UNIQUE INDEX notifications_dedupe_idx ON notifications(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX notifications_due_idx ON notifications(next_attempt_at_ns, lease_until_ns);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    event_type TEXT NOT NULL,
    media_id INTEGER REFERENCES media(id) ON DELETE SET NULL,
    outcome TEXT NOT NULL DEFAULT '',
    created_at_ns INTEGER NOT NULL,
    event_id TEXT,
    instance TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT '',
    file_id INTEGER NOT NULL DEFAULT 0,
    entity_id INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX events_event_id_idx ON events(event_id) WHERE event_id IS NOT NULL;
CREATE INDEX events_created_idx ON events(created_at_ns);

CREATE TABLE media_hashes (
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    algorithm TEXT NOT NULL,
    hash_value TEXT NOT NULL,
    byte_size INTEGER NOT NULL,
    fingerprint_path TEXT NOT NULL,
    fingerprint_file_id INTEGER NOT NULL,
    fingerprint_size INTEGER NOT NULL,
    fingerprint_mod_time_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    PRIMARY KEY (media_id, algorithm)
);

CREATE TABLE candidate_rejections (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    language TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    result_id TEXT NOT NULL,
    candidate_signature TEXT NOT NULL,
    artifact_checksum TEXT NOT NULL DEFAULT '',
    reason_code TEXT NOT NULL,
    tool_signature TEXT NOT NULL,
    media_path TEXT NOT NULL,
    media_file_id INTEGER NOT NULL,
    media_size INTEGER NOT NULL,
    media_mod_time_ns INTEGER NOT NULL,
    rejected_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    UNIQUE(media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, tool_signature)
);

CREATE INDEX candidate_rejections_lookup_idx ON candidate_rejections(
    media_id, language, provider_id, result_id, candidate_signature, tool_signature, expires_at_ns
);
