# Remaining systems code review — 2026-09-05

Reviewed `9d292c4` after the temporary workspace and LAPSE downstream repairs were verified and pushed. This is a review report, not an implementation of the findings below.

Scope: catalog reconciliation and Arr detail clients, webhook batches and identity, worker dispatch/leases/shutdown, inventory caching and ownership, application initialization/CLI locking and errors, configuration, migrations, and container publication. Provider adapters, scoring, archive extraction, LAPSE analysis/finalization, installer rollback, and notification delivery had already received separate reviews; this pass examined their integration where relevant.

## Findings

### R1 — P1: Arr detail redirects forward the API key

Location: `internal/catalog/arrclient.go:26–43`.

The detail client follows redirects with `X-Api-Key`, including redirects to a different origin. Both Sonarr and Radarr hydration still use this client, independently of the safer arrapi client. An in-memory transport reproduced the credential reaching the redirect destination.

Reject redirects at this client boundary, including when a client is injected, or enforce an explicitly reviewed same-origin policy before forwarding credentials.

### R2 — P1: A stale inventory refresh can restore an obsolete Arr identity

Location: `internal/store/repository.go:523–553`, especially the unconditional update at line 538.

Inventory refresh snapshots a media file before probing, then writes its path/file ID/size/mtime back by stable media row ID. If Arr replaces the file while the probe runs, the old refresh overwrites the newer catalog identity. A local reproduction persisted file 8 during the probe; refresh succeeded and restored file 7 and its old path. This can undermine the installation transaction's media fingerprint guard by making the database stale again.

Carry the expected pre-refresh catalog identity into the transaction and reject stale refreshes before changing media, tracks, or provenance. A legitimate live-stat update must remain distinguishable from an Arr identity replacement.

### R3 — P2: Embedded inventory has no completed-probe fingerprint

Location: `internal/inventory/inventory.go:63–81`, `internal/store/repository.go:569–576`.

The cache fingerprint is read from the current catalog media row, rather than a separately recorded completed probe. A newly indexed file whose metadata matches live stat skips its first ffprobe call. A replaced file can also inherit old embedded tracks under its new catalog fingerprint. Reproductions observed zero probe calls for a fresh file containing English subtitles, and old English satisfaction surviving replacement without probing.

Persist an explicit completed-probe fingerprint, including successfully probed empty inventories. Invalidate that record when media identity changes; catalog metadata alone must not establish cache validity.

### R4 — P2: Initialization writes before acquiring the mutation lock

Location: `internal/app/app.go:117`, `internal/app/app.go:153–198`; command-level locks run later.

Every CLI command constructs the writable application before its command method runs. Construction opens/migrates SQLite, updates instances, and backfills configured-language searches. Thus read-only commands and mutators that will subsequently fail to acquire the daemon lock can already change durable state. With another application holding the lock, a local `Open(Command: "explain")` using a configuration that added Croatian created pending Croatian search work.

Separate read-only assembly from mutating initialization. Acquire the mutation lock before migrations, backfills, and other durable initialization for mutating commands; read-only commands must not perform those writes.

### R5 — P2: One outside-scope webhook item discards later valid items

Location: `internal/catalog/webhook.go:149–163`.

A multi-file Sonarr rename/download batch returns `ErrIgnoredEvent` as soon as the first outside-scope item is encountered. HTTP acknowledges 204, but later in-scope items are never hydrated or scheduled. A local mixed-batch reproduction confirmed the loss.

Skip only the outside-scope item, continue processing the batch, and preserve the single-item ignored-success behavior. Other hydration failures must retain their existing failure behavior.

### R6 — P2: Arr remote root mappings match nothing

Location: `internal/catalog/pathmap.go:27–35`.

Configuration accepts remote `/`, but `MapPath` trims its slash to an empty prefix and skips it. Files under that namespace are all treated as outside scope. A local mapping reproduction confirmed this; reconciliation can consequently remove previously indexed work from scope.

Preserve root prefixes and handle their component boundary explicitly, keeping longest-match selection and traversal/symlink containment checks.

### R7 — P2: Arr detail errors expose URLs and upstream text

Location: `internal/catalog/arrclient.go:41–51`.

Transport errors wrap `url.Error`, which includes the complete request URL. Status and JSON decoder errors also expose upstream text instead of the owned, bounded error categories used by the arrapi path. A fake transport reproduced the URL in the resulting error; worker/webhook error logging does not remove it.

Return operation/status/error-class information without raw transport, status-line, or decoder text. Preserve typed cancellation and timeout behavior. This is separate from redirect credential forwarding.

### R8 — P2: Large retained history can stall reconciliation indefinitely

Location: `internal/catalog/radarr.go:98`, `internal/catalog/sonarr.go:164`, `internal/store/repository.go:1567`.

New instances begin with a zero cursor. Both adapters call `HistorySince` for the entire retained history. The pinned arrapi v2.0.5 implementation documents `/history/since` as unbounded (`history.go:172`) and limits list bodies to 64 MiB (`client.go:35`). A response exceeding that limit fails without advancing the cursor, so every retry repeats the same oversized request.

Use bounded history pagination or a server-supported bounded acquisition strategy while preserving atomic cursor commits, fractional overlap, and stable-entity reduction. This finding is supported by static call/limit/cursor evidence; no large live Arr response was requested.

### R9 — P2: Completed webhook redelivery still depends on successful hydration

Location: `internal/catalog/webhook.go:158–168`.

Import/rename hydration happens before durable event-ID deduplication. A previously acknowledged event redelivered after its old file ID disappears fails hydration and returns 503 instead of recognizing completed work. A local reproduction confirmed that exact redelivery fails before reaching the store's deduplication.

Check durable event identity before hydration, while retaining transactional deduplication when applying a new event so concurrent deliveries remain safe.

### R10 — P2: A canceled worker can dispatch new work

Location: `internal/worker/worker.go:91–155`, especially leasing at line 104 and initial dispatch at line 125.

The worker creates a detached work context and dispatches before checking parent cancellation. Wake/completion/timer paths can also dispatch without first ruling out cancellation. A pre-canceled `Run` reproduction still executed one workflow and completed its lease.

Check shutdown intent before initial and subsequent leasing/dispatch. Preserve the separate graceful drain context for work that was already active.

### R11 — P2: Removed-language jobs continue retrying

Location: `internal/store/repository.go:1532–1545` and worker leasing/router integration.

Configured-language reconciliation only adds jobs. Persisted work for a removed language remains leaseable, but the workflow router no longer supports that language. A real SQLite/router reproduction removed Croatian from the configuration, then leased its job and persisted `failure_attempt=1` with another retry one minute later.

Stop leasing work outside the active language/instance routes, or retire obsolete work transactionally while preserving active lease ownership and a safe re-enable policy. Apply the same scope rule to any unconfigured instance work.

### R12 — P2: Compose cuts the shutdown drain short

Location: `compose.example.yml:33`, `internal/worker/worker.go:25`, `internal/worker/worker.go:603–626`.

Compose allows 45 seconds before SIGKILL, while the worker deliberately allows 30 seconds of graceful drain followed by 30 seconds of cancellation drain. A slow cancellation cleanup can therefore be killed 15 seconds before the worker's documented bound, interrupting temporary-file cleanup or installation rollback.

Set the container grace period above the complete application shutdown budget, with margin, and keep its documented value consistent with worker drain defaults. This is a static configuration/code mismatch.

### R13 — P2: Startup CLI errors bypass the sanitized event

Location: `internal/app/app.go:117–120`, `internal/cli/cli.go:120–122`.

Application startup logs a sanitized `service.start_failed` event, then returns the original error. The CLI prints it again as plain text without a redactor because no backend was returned. A local missing-media-root reproduction emitted the protected path correctly redacted in JSON, followed by the same absolute path in an unsanitized `subsyncd:` line.

Return a sanitized startup error and establish one error-output owner once structured logging is initialized. Test the actual CLI-to-Open path, not just the captured application logger.

## Verification and limits

The reviewed repair baseline passed race-enabled tests, vet, tagged local E2E, formatting/diff checks, and the release-Dockerfile parity script. Additional review reproductions used temporary Go overlays, real temporary SQLite databases, sanitized media fixtures, and in-memory transports; they did not edit repository code or contact production, Arr, providers, or Silo. The probes intentionally exposed missing expectations in the current code. R8 and R12 are static findings rather than runtime reproductions.

No additional confirmed findings were identified in the bounded review of hash-cache ownership, upgrade guards, migration lineage checks, or Dockerfile parity/publication wiring. Passing existing tests does not resolve the findings above. Implement R1 and R2 first, then inventory cache validity and initialization locking before the remaining scheduling/webhook/privacy repairs. Each repair should receive a permanent focused regression test and affected-package race verification.
