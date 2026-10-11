-- When the oldest provider in resume_providers_json answered clean-empty.
-- The list is ignored once that is older than the six-hour search-result
-- cache, so those providers are asked again. 0 (every list saved before this
-- migration) counts as expired.
ALTER TABLE search_states ADD COLUMN resume_saved_at_ns INTEGER NOT NULL DEFAULT 0;

-- Reconciliation timing survives a restart: the last attempt, the failure
-- count that sets its backoff, and whether it waited for unreadable media.
ALTER TABLE instances ADD COLUMN reconcile_attempted_at_ns INTEGER NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN reconcile_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE instances ADD COLUMN reconcile_deferred INTEGER NOT NULL DEFAULT 0;

-- Accepted Arr webhooks waiting to be applied, in arrival order per instance.
CREATE TABLE webhook_inbox (
    id INTEGER PRIMARY KEY,
    instance TEXT NOT NULL,
    body BLOB NOT NULL,
    received_at_ns INTEGER NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX webhook_inbox_due_idx ON webhook_inbox(instance, next_attempt_at_ns, id);

-- A subtitle install between publishing its file and recording it. A row
-- left behind by a crash is undone at the next start.
CREATE TABLE pending_installs (
    id INTEGER PRIMARY KEY,
    media_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    destination TEXT NOT NULL,
    checksum TEXT NOT NULL,
    staged_path TEXT NOT NULL DEFAULT '',
    rollback_path TEXT NOT NULL DEFAULT '',
    created_at_ns INTEGER NOT NULL
);
