CREATE TABLE search_history (
    id INTEGER PRIMARY KEY,
    media_id INTEGER REFERENCES media(id) ON DELETE SET NULL,
    instance TEXT NOT NULL,
    kind TEXT NOT NULL,
    file_id INTEGER NOT NULL,
    media_title TEXT NOT NULL,
    language TEXT NOT NULL,
    outcome TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    release_name TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    manual INTEGER NOT NULL DEFAULT 0,
    finished_at_ns INTEGER NOT NULL
);
CREATE INDEX search_history_finished ON search_history(finished_at_ns);

ALTER TABLE installations ADD COLUMN candidate_signature TEXT NOT NULL DEFAULT '';
ALTER TABLE installations ADD COLUMN artifact_checksum TEXT NOT NULL DEFAULT '';

ALTER TABLE search_states ADD COLUMN replace_installed INTEGER NOT NULL DEFAULT 0 CHECK (replace_installed IN (0, 1));
