# Conventional Review Repairs Design

**Status:** Approved; implementation planned

## Purpose

Repair five correctness and operability gaps found by a conventional repository-wide review: reconciliation can invalidate an active search lease, reconciliation cannot apply Arr deletion history, Sonarr silently treats a multi-episode file as its first episode, shutdown can wait forever after cancellation, and a failed first-install rollback can hide failure to remove the newly published sidecar.

The repair keeps the service focused and headless. It does not add a UI, a new provider, full-library snapshot polling, or combined-episode subtitle support.

## Selected approach

Use a targeted contract repair. Catalog reconciliation will return typed history changes instead of a list containing only hydrated media. The repository will apply those changes with the same event and lease invariants used by webhooks. Multi-episode Sonarr files will remain indexed but will fail closed before any provider or LAPSE work. Shutdown and installation rollback will gain explicit second-stage failure bounds without changing ordinary successful behavior.

Two alternatives were rejected:

- Full catalog snapshots would discover deletions by diffing every current Arr file against SQLite, but they would add large-library pagination and substantially more Arr traffic.
- Minimal local patches could preserve leases and ignore hydration failures, but they would not represent deletion tombstones reliably and could silently advance past inconsistent state.

## Typed reconciliation contract

Replace `Catalog.ListMediaChangedSince` with `Catalog.ListChangesSince`. The returned value is an ordered collection of typed reconciliation changes. Each change contains:

- the stable Arr history record ID;
- the normalized operation: import, rename, or delete;
- the media reference containing instance, kind, and positive Arr file ID;
- the Arr event timestamp;
- hydrated authoritative media for import and rename only.

Sonarr recognizes `downloadFolderImported`, `episodeFileRenamed`, and `episodeFileDeleted`. Radarr recognizes `downloadFolderImported`, `movieFileRenamed`, and `movieFileDeleted`. Comparison is case-insensitive, while unsupported history types such as grabs and download failures are ignored.

Adapters sort relevant records deterministically by event timestamp and history ID. For each media reference, they retain only the newest relevant event in the requested window before hydration. Therefore an import followed by deletion in one page produces a deletion without attempting to hydrate a file that no longer exists. A deletion followed by a later import or rename hydrates the current file once. The resulting final-state changes retain deterministic order by timestamp, history ID, kind, and file ID.

A relevant history record without a positive history ID, positive file ID, valid event timestamp, or recognized media identity is malformed. Reconciliation fails and does not advance the cursor. An import or rename hydration failure, including HTTP 404, also fails the page. Only an explicit deletion event becomes a delete mutation; transport failures are never inferred to mean deletion.

## Transactional persistence and lease behavior

`Reconciler.Run` captures its page-end cursor before requesting history, converts the catalog changes into storage mutations, and submits the complete page to one repository transaction. The transaction applies every mutation, records bounded audit events, and advances the instance cursor. Any mutation or cursor failure rolls back the whole page.

Reconciliation event IDs derive from the configured instance plus the Arr history record ID. Replaying the same page is idempotent. The repository must not use file modification time as the reconciliation event identity.

Import and rename changes upsert authoritative media and invalidate stale candidate and installation provenance only when the existing fingerprint rules detect changed content. For every configured language, scheduling uses missing priority and the same conflict behavior as live events:

- retain the current lease owner and expiry;
- reset missing-result and technical-failure counters for the pending run;
- retain the greater of the current and requested priority;
- set `rerun_requested=1` when a lease exists, otherwise leave the row immediately due with no rerun flag.

When the active owner completes, the existing completion compare-and-swap observes the flag and releases exactly one immediate rerun. Repeated relevant changes during the same lease remain coalesced into that one rerun.

Deletion uses the established delete-event behavior: locate indexed media by reference, complete its search rows with the `deleted` outcome, release their leases, link the audit event when the media exists, and retain media/install history for explanation. A deletion for an unknown file is still an idempotently audited state change.

## Multi-episode Sonarr files

Add persisted media support status rather than pretending that a multi-episode file represents the first returned episode. Media records gain an `unsupported_reason` field whose normal value is empty. Sonarr sorts returned episode metadata by season number, episode number, absolute number, and ID.

- No episodes is a catalog consistency error and prevents the event or reconciliation page from committing.
- Exactly one episode produces the current searchable media identity.
- Two or more episodes produce indexed media using the earliest sorted episode as display metadata and `unsupported_reason=unsupported_multi_episode`.

The repository stores this media record and creates or updates language search rows so `explain` retains a durable reason, but those rows are completed rather than due. Existing active leases are not silently converted into new provider work; their next repository transition must retain the unsupported terminal state. The worker also loads and checks the persisted reason before invoking the workflow as a defense in depth. No newly dispatched provider search, provider download, archive extraction, LAPSE analysis, LAPSE synchronization, or subtitle installation may begin after the unsupported metadata commits.

An external operation already in flight when Sonarr changes the same file to unsupported may finish before its lease observes the new state. `RecordInstallation` therefore checks the current persisted support status inside its installation transaction. If the media has become unsupported, provenance commit fails and the installer restores or removes the published sidecar through the normal rollback path. This final guard prevents stale work from leaving a managed subtitle installed for unsupported media.

Full combined-episode support is a separate feature. It would need range-aware provider queries, hard gates proving complete range coverage, combined-versus-per-episode archive semantics, embedded/sidecar association rules, and dedicated LAPSE validation. Exact-hash-only behavior is not introduced as a partial exception.

## Bounded shutdown

Daemon shutdown has two explicit phases:

1. Stop leasing new searches and wait up to `ShutdownTimeout` for active search and maintenance work to finish naturally.
2. Cancel the shared work context, log `worker.drain_timed_out` once, and wait for one additional `ShutdownTimeout` grace period.

If work still has not returned after the second deadline, log one final warning with bounded active-work counts and return from the worker. The worker never clears leases speculatively; uncompleted leases recover through normal expiry. Completion channels remain buffered for all started work so a late goroutine cannot block merely because the dispatcher returned.

The deterministic batch-drain helper follows the same two-deadline rule. Cancellation-related errors after shutdown are not reported as ordinary workflow failures.

## Installation rollback failure

Rollback remains best-effort but never silent. After a newly published first-install sidecar fails its database/provenance commit, restoration attempts to remove the destination and sync its parent directory. For a managed replacement, restoration attempts to rename the rollback copy over the destination and sync the parent.

All restoration failures are accumulated with `errors.Join` and returned alongside the initiating error. Cleanup continues after an individual restoration failure where doing so cannot destroy the last known-good copy. The returned technical error is handled by the existing worker failure schedule and structured logging; it never creates a deterministic candidate rejection or blacklist.

If the filesystem prevents removal after a first-install database failure, the remaining untracked sidecar is deliberately protected by the next inventory scan. The combined error explicitly states that rollback was incomplete, while `explain` exposes the protected existing sidecar. The service does not guess ownership or overwrite it automatically.

## Observability

Reuse the existing structured logging vocabulary and redaction rules. Add only bounded reason/outcome values needed for:

- `unsupported_multi_episode` in indexed/search state and `explain`;
- forced return after the second shutdown deadline;
- incomplete installation rollback.

Absolute media/subtitle paths, Arr response bodies, provider URLs, subtitle content, and raw LAPSE output remain forbidden from normal logs. Multi-episode rejection is expected unsupported input, not a provider or LAPSE error.

## Testing and acceptance

Every production behavior follows a focused red-green-refactor cycle. Tests assert observable state and calls rather than source text or mock existence.

Required coverage:

- reconciliation during an active lease preserves owner/expiry and schedules exactly one rerun;
- Sonarr and Radarr history fixtures cover import, rename, delete, duplicate records, import followed by delete, delete followed by import, ignored events, malformed relevant events, and hydration failure without cursor advancement;
- a mixed reconciliation page and its cursor commit atomically, including deletion of known and unknown references;
- multi-episode Sonarr media is indexed with `unsupported_multi_episode`, completes configured search states, appears in `explain`, and causes zero provider and LAPSE calls;
- zero-episode Sonarr media remains a retryable catalog consistency failure;
- cancellation-insensitive work cannot hold daemon or batch shutdown beyond the two configured deadlines;
- first-install removal failure and replacement restoration failure are included in the returned installation error, while candidate rejection remains untouched;
- existing import, rename, deletion, inventory, queue priority, rerun, lease recovery, installation, and structured logging behavior remains green.

Run the affected packages with the race detector after each task. The final gate is:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
git diff --check
```

## Documentation

Update `README.md`, `docs/architecture.md`, `docs/operations.md`, and `docs/implementation-status.md` with the final behavior. Record each completed implementation boundary, its commit, its tests, and the next task in the implementation-status ledger so another agent can resume without reconstructing decisions from chat.

## Non-goals

- Full-library snapshot reconciliation.
- Combined-episode subtitle matching or subtitle-file merging.
- Exact-hash exceptions for multi-episode files.
- New subtitle providers, scoring changes, or LAPSE policy changes.
- Configurable reconciliation event mappings or shutdown phase durations.
- Automatic takeover or deletion of an untracked sidecar left by an incomplete rollback.
