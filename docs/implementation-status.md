# Implementation status and handoff log

This file is the resumable implementation ledger. The approved design and plan remain authoritative; this records what the code actually implements and why.

## Current state

- Branch: `feat/subsyncd`
- Current task: Task 4, embedded-track and live-sidecar inventory
- Next task: Task 5, provider registry, coordinator, and throttling
- Runtime module: `subsyncd` on Go 1.27
- Test caches: `GOCACHE=/tmp/subsyncd-gocache`, `GOMODCACHE=/tmp/subsyncd-gomodcache`

## Completed tasks

### Task 1 — domain and configuration

- Commit: `679035f feat: bootstrap subsyncd domain and configuration`
- Implemented canonical BCP 47 languages with explicit legacy aliases, strict single-document YAML, whole-scalar environment expansion, provider/language routing validation, path/root checks, and the approved example configuration.
- Added the pinned Bazarr behavior/decision index. Bazarr remains reference-only GPL-3.0 material.
- Verified with configuration/domain tests and `go vet ./...`.

### Task 2 — SQLite and schedules

- Commit: `6924360 feat: add persistent scheduling and provenance store`
- Implemented transactional embedded migrations, WAL/foreign-key/busy-timeout SQLite setup, typed media/inventory/search/provider/pack/installation persistence, recoverable leases, deterministic missing/failure/upgrade schedules, and an injected test clock.
- Important behavior: provider state is keyed by provider instance plus operation scope; pack eviction returns expired entries before live LRU entries; installation audit and provenance commit atomically.
- Verified with race-enabled `internal/store`, `internal/schedule`, and `internal/testutil` tests plus `go vet ./...`.

### Task 3 — Arr catalogs, webhooks, and path mapping

- Commit: `b047df2 feat: integrate Sonarr and Radarr catalogs`

- Webhook IDs are stable hashes of configured instance, normalized Arr event name, media kind, and Arr file ID. Duplicate deliveries are database no-ops.
- Imports and renames hydrate the authoritative file/episode-or-movie/series records from Arr before persistence. Deletes do not require a now-missing remote file resource.
- Content provenance invalidation compares Arr file ID, byte size, and nanosecond timestamp. A path-only rename retains candidate provenance while updating the path.
- Import/upgrade resets the missing schedule for every configured language. Delete events release/cancel pending jobs and remain in a bounded 10,000-row audit log.
- Reconciliation applies the full hydrated history page and cursor in one SQLite transaction, preventing a cursor gap after a partial failure.
- Path mapping tightens Bazarr behavior: longest boundary-aware remote prefix wins, both slash styles are accepted, case is preserved, traversal is rejected, and existing parent symlinks are resolved before media-root containment is accepted.
- Arr errors include instance/status and at most 4 KiB of response text; API keys are never included. Default client timeout is 15 seconds.
- Ordinary tests use sanitized fixture JSON and loopback fake servers only.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed. The full run exposed and retained a regression for SQLite partial unique indexes; targetless `ON CONFLICT DO NOTHING` is required for the partial `events(event_id)` index.

## Known follow-ups

- The application wiring will create/ensure configured Arr instance rows before reconciliation starts.
- HTTP webhook token validation belongs to the HTTP API task; catalog normalization deliberately accepts bytes only after transport authentication.
- Reconciliation currently hydrates history rows that still identify a live file. Deletions are handled by delete webhooks; if an Arr history endpoint exposes reliable deletion tombstones, add them behind a contract fixture before changing this rule.
