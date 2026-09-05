# Remaining system repairs implementation plan

User-authorized scope: fix all R1–R13 in `docs/remaining-systems-review-2026-09-05.md`, verify, independently review, and push to GitHub. Base `1a7a2ba`. Worktree `/tmp/subsyncd-remaining-repairs`, branch `codex/remaining-system-repairs`. No production access/deployment is included.

Use focused RED/GREEN tests with sanitized fixtures/local transports, affected-package race checks, then full race/vet/tagged local E2E. Preserve scoring, provider policy, strict archive selection, atomic installation/outbox, lease ownership, offline startup, and existing migration lineage. One implementation agent at a time; task review after each; final combined review before publication. Do not spawn subagents from implementation tasks. Implementers commit only their code/tests and task-local ledger entry, with no push; controller owns final docs/integration/publication.

## Task 1: Catalog transport, path mapping, webhook and bounded history repairs

Read AGENTS.md, docs/implementation-status.md, docs/providers.md, docs/operations.md and required Arr identity, correctness and logging specs/plans before changes. Own R1/R5/R6/R7/R8/R9. Relevant files internal/catalog, narrow store query for durable event dedup and its tests, and any affected interface fixtures.

- R1: clone supplied/default HTTP client policy and reject all redirects before X-Api-Key can be forwarded; test same-origin and cross-origin redirects and injected client behavior without network outside local fakes.
- R7: detail transport emits owned bounded operation/status/error-class messages only; preserve errors.Is cancellation/deadline and safe timeout semantics. Do not expose URLs, bodies, raw status strings or decoder values. Keep bounded JSON extraction, enforce complete single-document responses and size boundary if needed for safe decoding.
- R6: remote `/` mappings match root descendants while longest component-prefix matches, traversal rejection and symlink containment remain valid.
- R5: skip individual ErrOutsideScope items in mixed batches, still apply later relevant entries, return ignored success only when appropriate. Never generalize ignore to real failures.
- R9: read durable event identity before hydration; maintain transactional deduplication for races. A committed redelivery must avoid all hydration, including old missing file IDs; only record applied events.
- R8: use pinned arrapi paged History instead of unbounded HistorySince. Read dependency implementation for exact options. Bound per-response size using small pages; cover multiple pages, zero cursor, fractional overlap, events newer than through, early stopping at since, stable-entity reduction, failures leave cursor unchanged, and concurrent history insertion/duplicates as supported. Never guess a nonexistent time-window API or silently cap/drop historical events. No live requests.

Write report in task-1-report.md in this plan's SDD workspace with RED failures, GREEN commands/results, commit IDs, changes and unresolved concerns. Commit message `fix: harden Arr transport and reconciliation input`.

## Task 2: Guard inventory identity and completed probe cache

Read AGENTS/current implementation/provider/operations and required Arr/correctness specs/plans. Own R2/R3: internal/inventory, inventory-related store methods/migration/tests, related interfaces and fixtures.

- Add a separately persisted completed-probe fingerprint (migration 003; retain existing lineage and legacy refusal). A successful empty inventory is cache-valid. Old rows have no completed probe; no startup backfill/probe/network scan. First normal inventory refresh probes and records it.
- GetTrackInventory must not claim a completed probe based only on current catalog metadata. Reuse embedded tracks only for exact recorded probe fingerprint. New files, changed bytes or catalog replacement invalidate reuse. Sidecars remain live scans. Path-only rename may rebase a valid probe fingerprint if safely supported; otherwise reprobing is safe.
- ReplaceTrackInventory must compare the expected pre-refresh catalog identity in the same transaction before updating live size/mtime/tracks/provenance. A stale Arr replacement/rename or deletion aborts technically and cannot revert identity; retain allowed live-stat refinement when the catalog identity is unchanged.
- Keep catalog snapshot, completed-probe fingerprint and presence distinct; read them and tracks from one SQLite snapshot. Verify stored file ID/path still matches the requesting media before accepting the CAS expectation, including replacements before GetTrackInventory. Expected catalog mtime and observed stat mtime are separate because Arr DateAdded may differ. Update all callers explicitly; no compatibility overload bypass.
- Real catalog deletion retains the media row. Migration003 also records a durable deletion marker, set by delete and cleared by import/upsert. Inventory and installation/outbox commits reject deleted media technically; preserve retained history/provenance and leases. Test ApplyMediaEvent deletion during probe and before installation commit, not only physical row deletion.
- Recheck live media after probe to avoid caching results under a fingerprint that changed during probing; no full media content hash.
- Tests use real SQLite and controlled probe callbacks: fresh file, empty valid cache, repeat cache hit, replacement with old tracks, changed fingerprint during probe, Arr row replacement/rename during probe, deletion, legitimate live-stat refinement, and installation guards integration. No candidate rejection on technical staleness.

Report task-2-report.md with RED/GREEN evidence and commit IDs. Commit message `fix: bind embedded inventory to completed probes`.

## Task 3: Lock initialization and make diagnostic assembly read-only

Read AGENTS and operation/logging/current implementation docs. Own R4/R13: internal/app, internal/cli, narrow store open modes and tests; affected cache/syncer construction only if necessary.

- Acquire the mutation lock before migrations, configured-instance/search writes and durable initialization for serve/scan/search/retry. Retain it for command lifetime and release on every failed assembly/Close path; a method must not deadlock re-acquiring its own assembly lock. Reject a competing process before durable startup writes.
- explain/doctor/analyze-sync use read-only database assembly: no migrations, backfills, instance updates, cache initialization, or persistent diagnostic speech-cache writes. Read-only SQLite must reject writes and unknown/outdated schema safely. A missing or outdated database gets a bounded actionable error directing initialization through a mutating command, without creating it. Keep offline capability checks.
- OpenReadOnly uses mode=ro plus query_only, validates the complete embedded migration set without applying it, and does not use immutable=1 (must see daemon WAL). Diagnostic instances reject mutating methods. Do not expose a skip-lock fixture escape hatch.
- Explain distinguishes the new completed-probe marker from unprobed/stale cached tracks without performing a refresh.
- App.New tests/embedding must have an explicit safe lifecycle: do not silently keep pre-lock writes reachable through production Open. Coordinate low-level constructors as needed, preserve existing callers or update fixtures deliberately. No extra public HTTP routes.
- R13: once startup emits service.start_failed, CLI must not print the raw error again. Return sanitized typed reported failure; CLI recognizes already-reported failures. Direct Open errors must also be safe after configuration loads; include configured data/temp/media roots and executable paths in startup sanitization as needed. Test actual CLI/Open integration for paths and configured secrets, exactly one failure owner, ordinary pre-bootstrap usage behavior unchanged.
- Read-only commands can operate while daemon lock held. Test no state/migration/config backfill changes, failed second mutator no writes, lock released after failed startup/Close, and a mutable command initializes a fresh DB successfully.

Report task-3-report.md with RED/GREEN evidence and commit IDs. Commit message `fix: lock startup mutations and isolate diagnostics`.

## Task 4: Stop obsolete or canceled dispatch and align shutdown budget

Read AGENTS and priority-dispatch spec/plan, current implementation/operations. Own R10/R11/R12: worker/store lease scope/app assembly as needed, Compose and tests.

- Stop new search/maintenance dispatch after parent cancellation; initial canceled Run performs no work. Use parent-aware leasing and preserve separate graceful-drain context only for accepted active work. Check cancellation at wake/completion/timer dispatch and before starting newly returned leases; do not falsely complete abandoned leases.
- Only lease configured instance/language routes and active (not tombstoned) media from Task2. Prefer filtering claim eligibility with explicit active scope rather than destroying stored jobs/history. Filtering must happen in SQLite before priority ordering and LIMIT so obsolete rows cannot starve valid jobs. Preserve active lease ownership and safe automatic re-enable behavior. Cover removed languages and instances, no configured routes, priority and concurrency limits, and re-enable retaining valid history.
- Use a copied optional scope: nil retains generic unscoped repository callers, nonnil empty scopes claim nothing. Pass the scoped clone only to worker; renewal/completion remain owner-based and unfiltered. Reconciliation already uses configured Reconcilers. Notification intents represent committed installations and remain independent of search language/instance scope; do not filter them by removed search routes or widen R11 into a notification policy change.
- Compose stop_grace_period becomes 75s: full default two30s windows plus margin. Verify parsed Compose duration against actual worker default drain budget with a behavioral/config contract, not a grep assertion. Do not change runtime drain windows.

Report task-4-report.md with RED/GREEN evidence and commit IDs. Commit message `fix: scope worker dispatch and preserve shutdown drain`.

## Final integration

Independent combined review must address all R1–R13, tests, migrations, scope and error semantics. Run full race suite, vet, tagged E2E, release Dockerfile parity, Compose validation if available and diff checks. Update contributor/provider/operations guidance and implementation ledger; mark original report fixed with commit map. Fast-forward local main only when clean and remote unchanged, then push; no force pushes. Retain code/worktree until verified publication; cleanup only this plan's temporary artifacts after integration.
