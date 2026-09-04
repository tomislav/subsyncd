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
