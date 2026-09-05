# Conventional Review Repairs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix typed Arr deletion reconciliation, reconciliation lease safety, fail-closed Sonarr multi-episode indexing, bounded forced shutdown, and visible installation rollback failures.

**Architecture:** Arr adapters reduce history to one typed final-state change per file, and the reconciler converts those changes into the repository's transactional media mutations. SQLite persists unsupported media status and applies webhook/reconciliation scheduling through one lease-safe mutation path; worker and installer boundaries then enforce fail-closed dispatch, bounded cancellation, and complete rollback errors.

**Tech Stack:** Go 1.27, `net/http`, modernc SQLite, standard-library contexts/concurrency/filesystem APIs, structured `log/slog` events.

**Spec:** `docs/superpowers/specs/2026-09-04-conventional-review-repairs-design.md`

## Global Constraints

- Preserve current provider routing, scoring, LAPSE tournament, inventory protection, durable schedules, and notification behavior.
- Reconciliation commits every retained history mutation and its cursor in one SQLite transaction.
- A reconciled import or rename never clears an active lease and coalesces exactly one rerun.
- A multi-episode Sonarr file is indexed as `unsupported_multi_episode` and starts no new provider or LAPSE operation.
- Shutdown uses at most one graceful `ShutdownTimeout` plus one canceled `ShutdownTimeout` grace period.
- Rollback failures are technical failures and never candidate rejections.
- Logs never expose absolute paths, Arr response bodies, credentials, subtitle content, provider URLs, or raw LAPSE output.
- Follow strict red-green-refactor TDD for every production behavior change.

---

### Task 1: Persist fail-closed media support status

**Files:**
- Create: `internal/store/migrations/009_media_unsupported_reason.sql`
- Modify: `internal/domain/media.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces: `domain.UnsupportedReason` and `domain.UnsupportedMultiEpisode`.
- Produces: `domain.Media.UnsupportedReason` persisted in `media.unsupported_reason`.
- Produces: lease-safe terminal scheduling for media with a nonempty unsupported reason.

- [ ] **Step 1: Write failing migration, round-trip, and scheduling tests**

Add a migration compatibility test that opens a database through migration 008, inserts an existing media row, reopens through `store.Open`, and proves the row survives with an empty unsupported reason. Add a round-trip test using this literal:

```go
media := testMedia()
media.UnsupportedReason = domain.UnsupportedMultiEpisode
```

Assert `GetMedia` returns `unsupported_multi_episode`. Add two event tests:

```text
unsupported import without a lease -> search state complete, last_outcome unsupported_multi_episode, no due lease
unsupported import during a lease -> owner/expiry preserved, rerun_requested true, exactly one later lease that terminalizes without another rerun
```

The production mutation that must make these tests fail is omission of the new column or clearing/queuing an unsupported row incorrectly.

Add an installation-store test that starts with searchable media, then updates the same media to `unsupported_multi_episode` before calling `RecordInstallation`. Assert the transaction rejects the record and `GetInstallation` remains absent. This is the stale in-flight workflow publication guard.

- [ ] **Step 2: Run the focused storage tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -run 'TestMediaUnsupportedReasonMigration|TestMediaUnsupportedReasonRoundTrips|TestUnsupportedMediaEvent|TestRecordInstallationRejectsUnsupportedMedia' -count=1
```

Expected: FAIL because the domain field, migration, and unsupported scheduling branch do not exist.

- [ ] **Step 3: Add the domain type and migration**

Add:

```go
type UnsupportedReason string

const UnsupportedMultiEpisode UnsupportedReason = "unsupported_multi_episode"

UnsupportedReason UnsupportedReason `json:"unsupported_reason,omitempty"`
```

Create migration 009 with:

```sql
ALTER TABLE media ADD COLUMN unsupported_reason TEXT NOT NULL DEFAULT '';
```

Extend every media insert, update, select, and scan in `repository.go` with `unsupported_reason`. Validate stored values through a helper accepting empty or `domain.UnsupportedMultiEpisode`; return an error for unknown database values. In `RecordInstallation`, read the current media support status inside the existing transaction before inserting provenance and reject a nonempty reason. The caller's installer rollback then removes or restores the already published sidecar.

- [ ] **Step 4: Implement lease-safe unsupported scheduling**

Extract the import/rename search-state conflict logic into one private transaction helper used by later reconciliation work. Its searchable branch retains current priority/rerun behavior. Its unsupported branch uses these semantics:

```text
no lease: state=complete, last_outcome=<reason>, attempts reset, rerun_requested=0
active lease: preserve owner/expiry, reset attempts, set rerun_requested=1
```

When completion consumes that rerun, the row becomes immediately due once. The worker defense in Task 4 will lease it and terminalize it without calling the workflow. Do not introduce a new search priority.

- [ ] **Step 5: Run storage tests with race detection and commit**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -race -count=1
git diff --check
git add internal/domain/media.go internal/store/migrations/009_media_unsupported_reason.sql internal/store/repository.go internal/store/repository_test.go docs/implementation-status.md
git commit -m "fix: persist unsupported media state"
```

Before running the shown `git add`, update `docs/implementation-status.md` with the behavior, test command, commit boundary, and Task 2 as next. Expected: all commands pass.

---

### Task 2: Decode typed Arr history changes

**Files:**
- Create: `internal/catalog/history.go`
- Modify: `internal/catalog/catalog.go`
- Modify: `internal/catalog/sonarr.go`
- Modify: `internal/catalog/sonarr_test.go`
- Modify: `internal/catalog/radarr.go`
- Modify: `internal/catalog/radarr_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `test/e2e/e2e_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces: `catalog.HistoryChange`.
- Replaces: `ListMediaChangedSince(context.Context, time.Time) ([]domain.Media, error)` with `ListChangesSince(context.Context, time.Time) ([]HistoryChange, error)`.
- Preserves: `Catalog.GetMedia` for webhook and one-shot hydration.

- [ ] **Step 1: Write failing Sonarr and Radarr history contract tests**

Use complete sanitized history records containing `id`, `eventType`, `date`, and the appropriate file ID. Cover these literal event types:

```text
Sonarr: downloadFolderImported, episodeFileRenamed, episodeFileDeleted
Radarr: downloadFolderImported, movieFileRenamed, movieFileDeleted
```

For each adapter prove:

- import and rename return hydrated media;
- delete returns only a media reference and makes no file-hydration request;
- grab/download-failure records are ignored;
- repeated events for one file retain only the newest relevant event;
- import followed by delete does not hydrate;
- delete followed by import hydrates exactly once;
- missing positive history ID, file ID, or date on a relevant record returns an error;
- hydration HTTP 404 returns an error.

Assert literal operation, file ID, history ID, timestamp, and request counts. Do not compute expected ordering with the production sorter.

- [ ] **Step 2: Run adapter tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog -run 'TestSonarrListChangesSince|TestRadarrListChangesSince' -count=1
```

Expected: FAIL because `HistoryChange` and `ListChangesSince` do not exist.

- [ ] **Step 3: Define the typed history boundary**

Add to `catalog.go`:

```go
type HistoryChange struct {
    HistoryID int64
    Type      EventType
    Ref       domain.MediaRef
    Media     domain.Media
    OccurredAt time.Time
}

type Catalog interface {
    GetMedia(context.Context, domain.MediaRef) (domain.Media, error)
    ListChangesSince(context.Context, time.Time) ([]HistoryChange, error)
}
```

In `history.go`, define the shared decoded fields and deterministic reduction helpers. Normalize event types with strict, case-insensitive switches. Filter irrelevant types before validating relevant-record identity. Sort by `date`, then history ID; retain the last relevant record per `(kind,file_id)`; finally sort retained records by date, history ID, kind, and file ID before hydration.

- [ ] **Step 4: Implement Sonarr and Radarr adapters**

Sonarr maps relevant records to `domain.MediaEpisode`; Radarr maps them to `domain.MediaMovie`. Hydrate only retained import/rename changes. Preserve the original `since.UTC().Format(time.RFC3339Nano)` query and safe Arr error handling.

Replace interface methods in test fakes with the new signature, returning empty typed changes where history is irrelevant to the test.

- [ ] **Step 5: Run catalog and compile-dependent tests, then commit**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/app ./test/e2e -race -count=1
git diff --check
git add internal/catalog internal/app test/e2e docs/implementation-status.md
git commit -m "fix: model Arr reconciliation changes"
```

Expected: all commands pass. The implementation-status entry records fixture cases, race command, commit boundary, and Task 3 as next.

---

### Task 3: Apply reconciliation mutations transactionally and preserve leases

**Files:**
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `[]catalog.HistoryChange` and `store.MediaEventMutation`.
- Changes: `CommitReconciliation(context.Context, string, time.Time, []store.MediaEventMutation) error`.
- Produces: stable reconciliation event IDs `reconcile:<instance>:<history-id>`.

- [ ] **Step 1: Write failing reconciler conversion tests**

Configure a fake catalog with one import and one delete. Assert the store receives two mutations with:

```text
EventID: reconcile:sonarr-main:41 / reconcile:sonarr-main:42
Type: import / delete
At: the individual Arr event timestamp
Languages: the reconciler's configured canonical languages
```

Assert the store receives the captured page-end cursor separately. Preserve the existing callback rule: wake only after a successful commit.

- [ ] **Step 2: Write failing repository transaction tests**

Use the real SQLite repository to prove:

- import plus known delete commits both changes and the cursor;
- deletion of an unknown reference is audited and still advances the cursor;
- a forced later mutation failure rolls back earlier media/search/event writes and the cursor;
- replaying identical history IDs is a no-op;
- reconciliation during an active lease preserves owner/expiry, retains the greater priority, and creates exactly one immediate rerun after completion.

Inspect the actual stored lease owner and cursor. The production change that must fail the lease test is the old reconciliation conflict clause setting `lease_owner=NULL`.

- [ ] **Step 3: Run focused tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/store -run 'TestReconcilerConvertsHistoryChanges|TestCommitReconciliation' -count=1
```

Expected: FAIL because reconciliation still accepts only hydrated media and clears active leases.

- [ ] **Step 4: Share transactional mutation application**

Extract the body after event insertion from `ApplyMediaEvent` into:

```go
func applyMediaMutationTx(ctx context.Context, tx *sql.Tx, mutation MediaEventMutation) (bool, error)
```

Add `Priority SearchPriority` to `MediaEventMutation`. Normalize zero to `SearchPriorityImport` in `ApplyMediaEvent`; the reconciler sets `SearchPriorityMissing`. The helper inserts the event with targetless `ON CONFLICT DO NOTHING`, returns `false` for replay, applies import/rename/delete state, links known media, and uses Task 1's search-state helper. Keep the 10,000-row event pruning inside the surrounding transaction.

- [ ] **Step 5: Implement page conversion and atomic commit**

`Reconciler.Run` converts every history change to a mutation with the stable event ID and calls `CommitReconciliation` once. `Repository.CommitReconciliation` opens one transaction, validates that every mutation reference matches the configured instance, applies mutations in supplied order, prunes events, updates exactly one instance cursor row, and commits.

Do not call public `ApplyMediaEvent` from inside the transaction. Do not advance the cursor after a failed or malformed mutation.

- [ ] **Step 6: Run race-enabled catalog/store tests and commit**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/store -race -count=1
git diff --check
git add internal/catalog/reconcile.go internal/catalog/reconcile_test.go internal/store/repository.go internal/store/repository_test.go docs/implementation-status.md
git commit -m "fix: reconcile Arr mutations atomically"
```

Expected: all commands pass. Record transactional behavior, replay behavior, lease evidence, commit boundary, and Task 4 in the ledger.

---

### Task 4: Index and terminalize multi-episode Sonarr files

**Files:**
- Modify: `internal/catalog/sonarr.go`
- Modify: `internal/catalog/sonarr_test.go`
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `domain.UnsupportedMultiEpisode` and persisted `Media.UnsupportedReason`.
- Produces: search completion outcome `unsupported_multi_episode` without invoking `workflow.Service.Run`.
- Produces: `unsupported_reason=unsupported_multi_episode` in `explain`.

- [ ] **Step 1: Write failing Sonarr multi-episode tests**

Return two episode records in reverse order from the fake Sonarr server. Assert `GetMedia` succeeds, selects the lower season/episode as display identity, and sets `UnsupportedReason` exactly to `domain.UnsupportedMultiEpisode`. Retain the zero-episode test and assert it fails with the sanitized file identity.

- [ ] **Step 2: Write failing worker and explain tests**

Persist unsupported media and one due search lease. Run the worker and assert:

```text
workflow calls = 0
search state = complete
last outcome = unsupported_multi_episode
no next due lease
```

Add an `explain` test that asserts the stable line `unsupported_reason=unsupported_multi_episode`. The worker test exercises the real completion repository where practical; its workflow fake only counts the forbidden external boundary.

- [ ] **Step 3: Run focused tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/worker ./internal/app -run 'MultiEpisode|UnsupportedReason' -count=1
```

Expected: FAIL because Sonarr selects `episodes[0]`, the worker invokes the workflow, and explain omits support status.

- [ ] **Step 4: Implement deterministic indexing and worker defense**

Sort Sonarr episodes by season, episode, absolute episode, then ID. Keep zero as an error. Use the first sorted episode for display fields and set `UnsupportedReason` when `len(episodes) > 1`.

In `runSearchLease`, immediately after loading current media and before creating workflow request state, branch on `media.UnsupportedReason`. Complete the owned lease with:

```go
store.SearchCompletion{
    JobID: lease.JobID,
    Outcome: string(media.UnsupportedReason),
}
```

Log the ordinary authoritative `job.completed` event at info with the bounded reason. Do not create a workflow result, provider decision, candidate rejection, or upgrade schedule.

- [ ] **Step 5: Expose support status and run race tests**

Add the unsupported reason to the existing repository explain record and formatted application output. Empty reason remains omitted or renders as the existing neutral value so normal output does not become noisy.

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/store ./internal/worker ./internal/app ./internal/cli -race -count=1
git diff --check
git add internal/catalog/sonarr.go internal/catalog/sonarr_test.go internal/store internal/worker internal/app docs/implementation-status.md
git commit -m "fix: reject multi-episode media safely"
```

Expected: all commands pass, with zero workflow calls in the unsupported test. Record the behavior and Task 5 next.

---

### Task 5: Put a hard bound on canceled shutdown

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Preserves: `Worker.ShutdownTimeout` as the duration of each phase.
- Produces: `worker.drain_abandoned` after the second deadline.

- [ ] **Step 1: Write failing cancellation-insensitive shutdown tests**

Create a workflow fake that signals start and then blocks on a test-owned channel without selecting on `ctx.Done()`. With `ShutdownTimeout` set to 20 milliseconds, cancel daemon context and assert `Run` returns within a generous 250-millisecond test ceiling even though the workflow remains blocked. Release the fake after the assertion for test cleanup.

Add the same behavior test around the deterministic `drain` helper using a buffered `done` channel. Assert cancellation occurs after the first phase and return occurs after the second. Use synchronization channels for state; elapsed time is asserted only as an outer safety bound.

- [ ] **Step 2: Run focused worker tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/worker -run 'TestRunReturnsAfterCanceledDrainDeadline|TestDrainReturnsAfterCanceledDeadline' -count=1 -timeout=2s
```

Expected: FAIL by test timeout because both drain loops currently wait forever after cancellation.

- [ ] **Step 3: Implement a two-phase drain**

For both drain paths:

1. Start a graceful timer for `ShutdownTimeout`.
2. Return normally if all work completes.
3. On expiry, invoke cancellation exactly once and log `worker.drain_timed_out`.
4. Start a fresh timer for another `ShutdownTimeout`.
5. Continue consuming completions until all work ends or the second timer fires.
6. On the second expiry, log `worker.drain_abandoned` with `active_searches` and `maintenance_active`, then return without clearing leases.

Keep search completion capacity equal to `MaxWorkflows` and maintenance completion capacity one so late sends cannot block. Suppress `context.Canceled` through the existing `reportUnlessCanceled` path.

- [ ] **Step 4: Run worker race tests and commit**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/worker -race -count=1
git diff --check
git add internal/worker/worker.go internal/worker/worker_test.go docs/implementation-status.md
git commit -m "fix: bound forced worker shutdown"
```

Expected: all commands pass. Record both deadlines, late-send safety, event name, and Task 6 next.

---

### Task 6: Surface incomplete installation rollback

**Files:**
- Modify: `internal/workflow/install.go`
- Modify: `internal/workflow/install_test.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Preserves: `Installer.Install` signature and atomic publication path.
- Produces: joined initiating/restoration errors for incomplete rollback.

- [ ] **Step 1: Write failing first-install removal test**

Use the existing `StageDatabase` fault callback. After publication, replace the destination with a nonempty directory and return `errors.New("database unavailable")`. Assert the returned error contains both `database unavailable` and `remove newly published subtitle`; assert no installation was recorded.

The filesystem mutation intentionally makes real `os.Remove(destination)` fail with a nonempty-directory error. No filesystem mock is added.

- [ ] **Step 2: Write failing managed-replacement restoration test**

Start with a provenance-owned sidecar. At `StageDatabase`, replace the published destination with a nonempty directory and return the database error. Assert the returned error contains `restore previous subtitle`, and assert the rollback copy still exists with the original subtitle bytes. This catches deletion of the last known-good rollback after a failed rename.

- [ ] **Step 3: Run installer tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -run 'TestInstallReportsFirstInstallRemovalFailure|TestInstallPreservesRollbackWhenRestoreFails' -count=1
```

Expected: FAIL because removal is discarded and failed replacement restoration later deletes the rollback copy.

- [ ] **Step 4: Accumulate safe restoration errors**

Refactor the local restore closure to collect errors in this order:

```go
restoreErrors := []error{cause}
restoreErrors = append(restoreErrors, fmt.Errorf("remove newly published subtitle: %w", err))
restoreErrors = append(restoreErrors, fmt.Errorf("restore previous subtitle: %w", err))
restoreErrors = append(restoreErrors, fmt.Errorf("sync subtitle directory after rollback: %w", err))
return store.Installation{}, errors.Join(restoreErrors...)
```

Append only operations that actually fail. Attempt directory sync after a successful removal or rollback rename. If rollback rename fails, preserve the rollback file and do not run cleanup that could destroy the last known-good copy. Continue removing only disposable staged files.

- [ ] **Step 5: Prove workflow classification remains technical and commit**

Add or extend a service test whose installer returns the joined rollback error. Assert `Service.Run` returns an error with no `OutcomeRejected` and records no candidate rejection.

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -race -count=1
git diff --check
git add internal/workflow/install.go internal/workflow/install_test.go internal/workflow/service_test.go docs/implementation-status.md
git commit -m "fix: report installation rollback failures"
```

Expected: all commands pass. Record rollback preservation and Task 7 next.

---

### Task 7: Integration coverage and durable documentation

**Files:**
- Modify: `test/e2e/e2e_test.go`
- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `docs/operations.md`
- Modify: `docs/implementation-status.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Documents: typed reconciliation, unsupported multi-episode behavior, two-phase shutdown, and rollback recovery.
- Preserves: existing CLI and HTTP surface.

- [ ] **Step 1: Add the persisted reconciliation end-to-end test**

Extend the tagged test harness with sanitized Sonarr/Radarr history responses. Exercise one imported file, one deleted indexed file, and one Sonarr multi-episode file. Run reconciliation and assert:

```text
cursor advances once
import search becomes due
deleted search completes as deleted
multi-episode search completes as unsupported_multi_episode
provider/download/LAPSE/install call counts remain zero for the unsupported file
```

- [ ] **Step 2: Run the tagged test and verify its integration assumptions**

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -run TestTypedReconciliationAndUnsupportedMedia -race -count=1
```

Expected: PASS against the completed Tasks 1–6. If it fails, fix the owning task's production behavior and rerun that task's focused race suite before continuing.

- [ ] **Step 3: Update operator and future-agent documentation**

Document these exact operational facts:

- reconciliation consumes final-state Arr history mutations and includes deletions;
- a failed page never advances its cursor;
- same-key reconciliation preserves a lease and coalesces one rerun;
- Sonarr multi-episode files appear in `explain` as unsupported and make no new provider/LAPSE calls;
- shutdown can return after two timeout windows while durable leases recover later;
- incomplete rollback errors can leave a protected untracked sidecar that requires operator inspection.

Update `AGENTS.md` invariants only after all behavior tests are green. The implementation-status final entry lists every repair commit, verification command, remaining limitation, and the next publication/deployment task.

- [ ] **Step 4: Run the complete verification gate**

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
git diff --check
```

Expected: every command exits zero with no race report. If the sandbox denies loopback binding, rerun the identical test command in the already approved unrestricted execution context and record that fact in the ledger.

- [ ] **Step 5: Commit the completed repair documentation**

```bash
git add test/e2e/e2e_test.go README.md docs AGENTS.md
git commit -m "docs: document reliability repairs"
```

Do not push, publish an image, or deploy to production in this plan. Those remain separate user-approved actions after local verification.
