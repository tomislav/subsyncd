# Architecture

`subsyncd` is a headless, SQLite-authoritative subtitle acquisition service. Sonarr and Radarr supply media identity and lifecycle events; language-specific provider chains supply candidates; inventory, scoring, LAPSE, installation, and optional Silo notification form the processing pipeline. There is no management HTTP API or browser UI.

## Durable state and event flow

Webhooks and periodic Arr reconciliation converge on the same transactional media-mutation helper. It records an idempotent audit event, applies an import, rename, or deletion, invalidates stale evidence when content changes, and updates every configured language search. Reconciliation first reduces history to the newest relevant final state per Arr file, hydrates only live files, then commits the complete page and cursor in one transaction. Any page failure leaves the prior cursor intact.

Search states in SQLite are the queue. Import, missing, and upgrade priorities affect dispatch order; provider cooldowns and the independent missing/technical schedules determine when work is eligible. Advisory wakes reduce latency but carry no correctness: startup and recovery polling always rediscover due work. A change arriving during an active same-key lease preserves its owner and expiry and coalesces one rerun.

Workers lease only available concurrency slots. Leases are renewed and completed by owner compare-and-swap. Shutdown stops new acquisition, waits one configured window, cancels active work, waits one more window, then returns even if a dependency ignores cancellation. Unfinished leases remain recoverable after expiry.

## Media support boundary

Media fingerprints bind path, Arr file ID, size, and modification time. Embedded inventory and expensive file hashes are cached against that exact identity; external sidecars are scanned live before every search.

A Sonarr media file associated with multiple episodes is currently outside the supported acquisition model. It is indexed as `unsupported_multi_episode` using the deterministically earliest episode only for display, while its searches are terminal. No provider, candidate, LAPSE, installation, or upgrade path may process it. This is a durable fail-closed boundary, not silent first-episode behavior.

## Acquisition and publication

Each canonical language owns an ordered provider chain. Candidates pass identity gates and deterministic release scoring before any download. Exact hashes can install directly; policy-qualified strong first installs may use an auditable score bypass; other candidates enter the lazy score-tier LAPSE tournament. Deterministic candidate failures are scoped and quarantined, while operational failures remain retryable.

Installation validates and stages a sidecar beside its destination, applies permissions, fsyncs, atomically renames, fsyncs the directory, and commits checksum-bound provenance. Managed replacement uses a retained rollback copy. If cleanup, restoration, or directory sync also fails, that error is joined with the initiating error and the surviving file is protected rather than automatically adopted. Operator inspection is required for a valid-looking but untracked sidecar.

A committed installation may enqueue a checksum-deduplicated Silo notification. Notification leases and retries are independent, so notification failure never rolls back subtitle acquisition.
