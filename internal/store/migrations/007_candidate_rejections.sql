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
