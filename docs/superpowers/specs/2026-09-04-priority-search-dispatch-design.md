# Priority Search Dispatch Design

**Status:** Approved for implementation

## Purpose

Make daemon-mode search scheduling responsive to Arr webhooks without giving up the existing SQLite durability, deduplication, retry, and lease guarantees. A newly imported file should enter durable storage before the webhook returns, start as soon as workflow capacity is available, and take precedence over routine missing-subtitle retries and subtitle upgrades.

The design keeps SQLite as the only source of truth. In-process signaling is an optimization that reduces latency; losing a signal must never lose work.

## Alternatives considered

The selected approach combines a prioritized SQLite queue with a non-blocking in-process wake signal and a continuously refilled worker pool. It uses the existing storage and lease model while eliminating batch barriers.

Shortening the current polling interval would be a smaller change, but a webhook arriving during a long `RunOnce` batch would still wait for every leased job in that batch. It also creates unnecessary empty database polls.

Launching work directly from the webhook handler would reduce latency but would couple HTTP request lifetime to provider and LAPSE work, weaken restart recovery, and bypass durable ordering. An external broker would restore durability but is disproportionate for a single-process, UI-free service.

## Configuration

Add a top-level worker section:

```yaml
worker:
  max_concurrent: 1
```

`worker.max_concurrent` controls concurrent subtitle workflows, including LAPSE processes. It defaults to `1`, must be an integer from `1` through `8`, and is independent of provider HTTP concurrency. Existing configurations therefore become more conservative without requiring edits.

The existing approximately 30-second jittered poll remains an internal recovery interval. It is not exposed in this change because webhook wakeups remove the normal latency and operators have no useful reason to tune it.

## Persistent queue model

Migration `008_search_priorities.sql` adds these non-null columns to `search_states`:

```text
priority            INTEGER NOT NULL DEFAULT 200
rerun_requested     INTEGER NOT NULL DEFAULT 0
```

The migration replaces `search_due_idx` with an index beginning with `state`, `priority DESC`, and `next_attempt_at_ns`. Existing rows receive missing-retry priority so upgrading an existing database does not silently demote unresolved work.

Use named Go priority constants rather than untyped numbers throughout the application:

```text
SearchPriorityImport  = 300
SearchPriorityMissing = 200
SearchPriorityUpgrade = 100
```

Due searches are leased in this deterministic order:

1. Higher priority.
2. Earlier `next_attempt_at`.
3. Lower media ID.
4. Canonical language tag.

Priority changes follow the search lifecycle:

- Sonarr or Radarr import and rename webhooks schedule import priority, due immediately.
- Reconciliation-discovered missing languages schedule missing priority.
- `no_result` and `rejected` backoff remains missing priority.
- Successful installations and satisfied non-exact subtitles schedule their next reassessment at upgrade priority.
- Exact-hash satisfaction with no next upgrade completes the row.
- Provider throttling and transient workflow failure retain the job's current priority while changing only its next-attempt time and counters as appropriate.
- Delete events complete all related searches exactly as they do now.

The `(media_id, language)` uniqueness constraint remains. Multiple events coalesce rather than create an unbounded list of equivalent jobs.

## Events that arrive during an active search

A new event for a different media/language key inserts or updates its own row and is immediately eligible for any free slot.

A non-duplicate event for the same key must not steal or erase an active lease. If its row is leased, the event updates authoritative media metadata, raises the stored priority to import priority, sets `rerun_requested=1`, and preserves `lease_owner` and `lease_until_ns`. The active workflow retains its lease. Existing media-fingerprint guards prevent it from installing an artifact for content that has been replaced.

The event resets missing-result and workflow-failure counters for the pending rerun, just as an ordinary import does today. The active lease already carries its original counter values, so this reset cannot change how the in-flight attempt is classified.

When that active lease completes, `CompleteSearch` checks `rerun_requested` before applying the ordinary outcome schedule. If set, it clears the lease, clears the flag, leaves the row pending at the retained highest priority, and makes it due immediately. This guarantees one follow-up run against current metadata without running the same key concurrently.

Identical replayed webhooks remain no-ops because the existing stable event ID is inserted transactionally with `ON CONFLICT DO NOTHING`.

## Wake signal

The application constructs one buffered channel with capacity one. Each webhook handler receives an `OnApplied func()` callback. After at least one media mutation commits successfully, the handler invokes that callback, including when a later item in the same multi-file webhook fails. The callback performs a non-blocking send, so webhook latency never depends on worker state and bursts collapse into one wakeup.

The callback is not invoked for test events, invalid payloads, duplicate event IDs, or failed transactions. A successful database mutation followed by process termination before signaling is safe because startup dispatch and the periodic poll discover the pending row.

Reconciliation also signals after each successful run; a harmless wake with no due rows is preferable to expanding the reconciler interface solely to report whether rows changed. Job completion itself causes an immediate refill and does not require a channel round trip.

## Continuous dispatcher

Replace the daemon's batch-and-wait search loop with a capacity-aware dispatcher. `RunOnce` remains available as a deterministic maintenance/testing entry point, but daemon `Run` no longer waits for an entire leased batch before checking for new work.

The daemon dispatcher owns the active-workflow count. On startup, webhook wake, recovery poll, reconciliation enqueue, or workflow completion it:

1. Computes `available = max_concurrent - active`.
2. Leases at most `available` due searches from SQLite.
3. Starts one goroutine per returned lease.
4. Increments the active count only for successfully claimed leases.
5. Receives each result through a completion channel, reports any error, decrements the active count, and immediately refills the slot.

The dispatcher never leases work merely to leave it waiting behind an in-memory semaphore. Consequently a lower-priority batch cannot reserve all leases while higher-priority webhook work waits in SQLite.

Lease renewal remains one minute against a five-minute lease by default. Multiple processes still cannot claim the same unexpired lease. After a crash, another process recovers the row when the lease expires.

Reconciliation and notification delivery retain their existing schedules, retry rules, and durable notification queue. Their execution must not consume search workflow slots or block search-slot refill. This change does not add notification priorities.

## Shutdown and failures

On shutdown, the dispatcher stops leasing new searches and gives active workflows the existing bounded drain window. If the window expires, it cancels the remaining workflow contexts; leases then recover through normal expiry. It does not clear leases speculatively.

Database leasing, completion, or renewal errors are reported through the existing `OnError` hook. A renewal failure cancels only the affected workflow. The recovery poll continues after non-fatal dispatch errors.

An unavailable provider follows existing persisted cooldown and backoff behavior. Queue priority changes ordering among due jobs only; it never bypasses provider rate limits, cooldowns, minimum scores, or upgrade policy.

## Observability

Structured logs add these fields at dispatch boundaries without exposing paths or credentials:

```text
media_id language priority queue_trigger active max_concurrent
```

`explain` displays the stored search priority and whether a rerun is pending. No web UI or queue-management endpoint is added.

## Testing and acceptance

Tests must demonstrate:

- migration of an existing database assigns missing priority and preserves rows;
- lease ordering is import, missing, then upgrade, with deterministic ordering inside a priority;
- a new high-priority row cannot preempt a running workflow but occupies a free slot immediately;
- with `max_concurrent: 1`, a webhook job remains durable until the active workflow completes, then starts without a poll delay;
- a webhook arriving while one of two slots is free starts before the older workflow completes;
- the dispatcher never exceeds configured concurrency;
- a same-key event during an active lease requests exactly one immediate rerun and does not permit concurrent execution;
- duplicate webhook delivery neither adds work nor signals unnecessarily;
- lost wake signals are recovered by startup or periodic polling;
- lease renewal, crash recovery, retry/backoff, notification delivery, and bounded shutdown retain their existing behavior;
- configuration defaults to one and rejects zero, negative, or values above eight;
- race-enabled tests show no dispatcher, wake-channel, or shutdown data races.

Run the complete race-enabled Go suite, `go vet ./...`, tagged end-to-end tests, and `git diff --check`. Update the example configuration, README feature/configuration material, operations guide, architecture document, and implementation-status ledger.

## Non-goals

- External brokers or distributed queue coordination beyond the existing SQLite lease safety.
- Manual reordering, cancellation, or inspection APIs.
- Provider-request priority or bypassing provider throttles.
- Notification priority changes.
- Configurable polling, batch, lease, renewal, or shutdown durations.
- Preemptively killing a lower-priority workflow when a webhook arrives.
- Changing subtitle scoring or LAPSE candidate selection.
