# Missed Arr Delete Reconciliation

**Status:** Approved for implementation on 2026-09-13.

## Purpose

Recover whole-series Sonarr deletions and whole-movie Radarr deletions whose
webhooks were emitted while subsyncd was unavailable. Today those webhook
events retire durable catalog work immediately, but history reconciliation
cannot recover a deletion when Arr emits no retained per-file history record.
The stale media then remains active and repeatedly fails filesystem inventory.

## Selected approach

Every normal and explicit reconciliation obtains one complete current catalog
identity snapshot in addition to the bounded history window:

- Sonarr returns the positive IDs of all current series.
- Radarr returns the positive IDs of all current movies.

The repository compares that complete set with active positive identities it
already tracks for the same instance. A missing Sonarr series ID produces the
existing series-addressed delete mutation, retiring all of its episodes and
searches. A missing Radarr movie entity ID produces the existing
entity-addressed delete mutation. Existing transactional delete behavior,
per-media audits, lease completion, and retained provenance rules remain the
authority for applying those mutations.

This is deliberately narrower than treating every absent file or episode as a
deletion. Individual file replacement and episode/file removal continue to use
webhooks and history reconciliation. Legacy Sonarr rows with `series_id = 0`
cannot be matched and remain unchanged until ordinary hydration supplies their
stable series identity.

## Snapshot validity and concurrency

Absence is authoritative only after the Arr adapter has completely enumerated
and validated the relevant identity collection. Invalid, duplicate-conflicting,
incomplete, non-progressing, canceled, timed-out, or failed enumeration aborts
the reconciliation. It applies no inferred deletions and does not advance the
history cursor.

The reconciler captures the existing reconciliation state and per-instance
event revision before remote history and catalog reads. History mutations,
snapshot-derived delete mutations, and cursor advancement commit together
behind the existing revision/cursor compare-and-swap. A concurrent webhook or
discovery commit makes the snapshot stale, rolls back the entire batch, and
causes normal reconciliation retry. Stable synthetic event IDs identify the
instance, Arr kind, missing stable identity, and snapshot boundary so replay is
idempotent without colliding with webhook or history audit IDs.

An empty catalog is valid only when the adapter positively completed the full
enumeration. This supports intentionally empty Sonarr or Radarr installations
without weakening failure safety.

## Interfaces and data flow

The catalog reconciliation contract gains a method returning a domain-owned
complete set of current top-level identities. Sonarr and Radarr implement it
through arrapi v2.0.5 enumeration; arrapi concrete types remain internal to the
catalog package.

The reconciliation store exposes active tracked top-level identities for one
instance and kind. Reads include only non-deleted rows and positive identities;
Sonarr results are distinct series IDs and Radarr results are movie entity IDs.
The reconciler computes set difference in memory, converts missing identities
to existing delete mutation shapes, and sends one atomic commit.

Full-library discovery remains additive and keeps its completion marker. It
does not independently tombstone omissions; authoritative removal belongs to
the recurring reconciliation transaction described here.

## Failure and operational behavior

Snapshot failures use the existing per-instance reconciliation backoff and are
reported as bounded Arr/reconciliation errors without URLs, credentials,
response bodies, titles, or paths. A successful run restores the normal
interval. Startup and readiness remain offline.

After deployment, the first successful reconciliation retires stale rows left
by missed whole-delete webhooks. No provider request, media read, or subtitle
file mutation is required for retirement.

## Testing and acceptance

Implementation follows focused red-green-refactor cycles using sanitized fakes
only. Required regressions cover:

- a missed Sonarr `SeriesDelete` retiring every stored episode in that series;
- a missed Radarr `MovieDelete` retiring the stored movie;
- preservation of current identities and instance/kind isolation;
- legacy zero Sonarr series identities remaining active;
- empty but complete snapshots;
- incomplete, malformed, failed, and canceled snapshots deleting nothing and
  retaining the cursor;
- a concurrent event revision invalidating the combined snapshot transaction;
- idempotent overlap/replay and existing lease/rerun completion behavior;
- explicit scan and recurring reconciliation using the same recovery path.

Affected package tests run with `-race`, followed by the complete race suite,
`go vet ./...`, tagged end-to-end tests, formatting, and `git diff --check`.
