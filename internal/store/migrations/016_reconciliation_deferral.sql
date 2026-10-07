-- Arr history entities whose current media could not be read. Each row keeps
-- the history identity needed to re-check it after the cursor has advanced,
-- Arr's current (unreadable) file, and when that file was first deferred.
CREATE TABLE reconciliation_deferrals (
    instance TEXT NOT NULL,
    kind TEXT NOT NULL,
    entity_id INTEGER NOT NULL,
    history_id INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at_ns INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    first_deferred_at_ns INTEGER NOT NULL,
    PRIMARY KEY(instance, kind, entity_id)
);
