CREATE TABLE reconciliation_replays (
    event_id TEXT PRIMARY KEY,
    instance TEXT NOT NULL
);

CREATE INDEX reconciliation_replays_instance_idx ON reconciliation_replays(instance);
