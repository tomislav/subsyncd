CREATE TABLE candidate_lapse_failures (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    language TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    result_id TEXT NOT NULL,
    candidate_signature TEXT NOT NULL,
    artifact_checksum TEXT NOT NULL,
    tool_signature TEXT NOT NULL,
    media_path TEXT NOT NULL,
    media_file_id INTEGER NOT NULL,
    media_size INTEGER NOT NULL,
    media_mod_time_ns INTEGER NOT NULL,
    failure_signature TEXT NOT NULL,
    occurrence_count INTEGER NOT NULL CHECK (occurrence_count BETWEEN 1 AND 2),
    first_observed_at_ns INTEGER NOT NULL,
    last_observed_at_ns INTEGER NOT NULL,
    UNIQUE(media_id, language, provider_id, result_id, candidate_signature, artifact_checksum, tool_signature, media_path, media_file_id, media_size, media_mod_time_ns, failure_signature)
);

CREATE INDEX candidate_lapse_failures_media_language_idx
ON candidate_lapse_failures(media_id, language);
