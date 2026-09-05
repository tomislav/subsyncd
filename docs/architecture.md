# Architecture

`subsyncd` is a headless, SQLite-authoritative subtitle acquisition service. Sonarr and Radarr supply media identity and lifecycle events; language-specific provider chains supply candidates; inventory, scoring, LAPSE, installation, and optional Silo notification form the processing pipeline. There is no management HTTP API or browser UI.

## Durable state and event flow

Webhooks and periodic Arr reconciliation converge on the same transactional media-mutation helper. It records an idempotent audit event, applies an import, rename, upgrade, or deletion, invalidates stale evidence when content changes, and updates every configured language search. Reconciliation fetches descending 100-record history pages with strict completeness and progress checks, reduces history by stable Arr entity (`movieId` or `episodeId`), resolves the authoritative current entity, then commits all resulting mutations and the cursor in one transaction. Any page failure leaves the prior cursor intact. Because Arr history accepts only whole-second timestamps, the request overlaps the fractional persisted cursor; stable history event IDs and the same transaction make replay harmless.

The catalog keeps two identities deliberately. `entity_id` is the stable movie or episode and survives file replacement; `file_id` identifies the current physical Arr file and remains part of byte fingerprints, webhook events, workflows, installations, and hashes. An upgrade therefore updates one logical media row while invalidating evidence tied to the prior bytes. `github.com/cplieger/arrapi/v2` v2.0.5 owns bounded history and current-entity requests. A temporary hardened detail client owns only richer release, quality, edition, runtime, and provenance enrichment that arrapi does not expose.

Path mapping is also a scope boundary. A deliberately unmapped or safely mapped-outside-root entity becomes an outside-scope deletion/audit, so a narrow canary does not index or read the remaining Arr library. Traversal, malformed mappings, inaccessible paths, and unsafe filesystem resolution fail the page closed. Every persisted media row has a positive stable Arr entity ID. Startup performs no Arr request, identity backfill, or full-library scan.

SQLite state currently begins at `001_baseline.sql`. Databases containing migration names from the pre-release `001_initial.sql`–`010_media_entity_ids.sql` lineage are unsupported and rejected before migration SQL runs. Operators must preserve the old data directory for rollback and start this build with an empty one. The ordered migration runner remains available for future migrations after the baseline.

Search states in SQLite are the queue. Import, missing, and upgrade priorities affect dispatch order; provider cooldowns and the independent missing/technical schedules determine when work is eligible. Advisory wakes reduce latency but carry no correctness: startup and recovery polling always rediscover due work. A change arriving during an active same-key lease preserves its owner and expiry and coalesces one rerun.

Workers lease only available concurrency slots, filtering current instance/language routes and active media before ordering and limiting claims. Removing a route preserves durable work for re-enable. Leases are renewed and completed by owner compare-and-swap. Deletion retains active ownership; completion leaves deleted media terminal, while reimport during that lease coalesces one rerun. Shutdown stops new acquisition, waits one configured window, cancels active work, waits one more window, then returns even if a dependency ignores cancellation. Unfinished leases remain recoverable after expiry.

Mutating application assembly acquires the data-directory lock before database migration or durable initialization and retains it through close. Diagnostic assembly validates an existing current-schema database in read-only mode, without backfills or persistent cache construction. It can read the running daemon's WAL without taking its mutation lock.

## Media support boundary

Media fingerprints bind path, replaceable Arr file ID, size, and modification time. Embedded inventory requires a separately recorded completed-probe fingerprint, including empty probe results; catalog metadata alone cannot validate the cache. Inventory reads one catalog/probe/track snapshot and compares the original catalog identity when committing, so a stale probe cannot restore obsolete media. Expensive file hashes are also cached against exact physical identity; external sidecars are scanned live before every search. The separate stable entity ID is catalog identity, not evidence that two file versions have identical bytes.

A Sonarr media file associated with multiple episodes is currently outside the supported acquisition model. It is indexed as `unsupported_multi_episode` using the deterministically earliest episode only for display, while its searches are terminal. No provider, candidate, LAPSE, installation, or upgrade path may process it. This is a durable fail-closed boundary, not silent first-episode behavior.

## Acquisition and publication

Each canonical language owns an ordered provider chain. Candidates pass identity gates and deterministic release scoring before any download. Exact hashes can install directly; policy-qualified strong first installs may use an auditable score bypass; other candidates enter the lazy score-tier LAPSE tournament. Deterministic candidate failures are scoped and quarantined, while operational failures remain retryable.

Installation validates and stages a sidecar beside its destination, applies permissions, fsyncs, atomically renames, fsyncs the directory, and commits checksum-bound provenance. Managed replacement uses a retained rollback copy. If cleanup, restoration, or directory sync also fails, that error is joined with the initiating error and the surviving file is protected rather than automatically adopted. Operator inspection is required for a valid-looking but untracked sidecar.

A committed installation may enqueue a checksum-deduplicated Silo notification. Notification leases and retries are independent, so notification failure never rolls back subtitle acquisition.
