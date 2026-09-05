# Priority Search Dispatch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make persisted subtitle searches priority-aware and let daemon workers immediately fill free workflow slots after webhook arrivals.

**Architecture:** SQLite remains authoritative and gains priority plus same-key rerun state. A capacity-one wake channel accelerates discovery, while a capacity-aware daemon dispatcher leases only work it can start and retains polling for recovery.

**Tech Stack:** Go 1.27, modernc SQLite, `net/http`, YAML v3, standard-library concurrency primitives.

**Spec:** `docs/superpowers/specs/2026-09-04-priority-search-dispatch-design.md`

## Global Constraints

- Preserve SQLite durability, stable webhook-event deduplication, renewable leases, retry/backoff, notification delivery, and bounded shutdown.
- Search priority order is import `300`, missing `200`, upgrade `100`.
- `worker.max_concurrent` defaults to `1` and accepts only integers from `1` through `8`.
- Wake delivery is non-blocking and advisory; startup and periodic polling must recover a lost wake.
- Never lease more searches than the dispatcher can start immediately.
- A same-key event during an active lease requests one rerun and never permits concurrent execution for that key.
- Follow strict red-green-refactor TDD for every production behavior change.

---

### Task 1: Persist priorities and same-key reruns

**Files:**
- Create: `internal/store/migrations/008_search_priorities.sql`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`

**Interfaces:**
- Produces: `type SearchPriority int` and constants `SearchPriorityUpgrade`, `SearchPriorityMissing`, `SearchPriorityImport` in package `store`.
- Produces: `SearchLease.Priority SearchPriority`.
- Produces: `SearchCompletion.Priority SearchPriority` for schedules that change class; zero means retain the leased priority.
- Produces: `UpsertSearchStateWithPriority(ctx, mediaID, language, next, priority)` while retaining `UpsertSearchState(ctx, mediaID, language, next)` as a missing-priority compatibility wrapper.
- Preserves: `LeaseDueSearches`, `RenewSearchLease`, and compare-and-swap completion ownership.

- [ ] **Step 1: Write failing migration and ordering tests**

Add tests that insert one due row at each priority, lease with limit three, and assert import/missing/upgrade order. Add `openDatabaseThroughMigration(t, "007_candidate_rejections.sql")`, which executes embedded migrations in filename order through the requested version; insert a legacy search row, close it, reopen through `store.Open`, and assert migration 008 preserves the row with priority `200` and `rerun_requested=0`.

```go
func TestLeaseDueSearchesOrdersByPriorityBeforeDueTime(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	upgrade := insertTestMedia(t, repo, 1, now)
	missing := insertTestMedia(t, repo, 2, now)
	imported := insertTestMedia(t, repo, 3, now)
	requireSearchState(t, repo, upgrade, "en", now.Add(-3*time.Hour), SearchPriorityUpgrade)
	requireSearchState(t, repo, missing, "en", now.Add(-2*time.Hour), SearchPriorityMissing)
	requireSearchState(t, repo, imported, "en", now.Add(-time.Hour), SearchPriorityImport)

	leases, err := repo.LeaseDueSearches(context.Background(), now, 3, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{leases[0].MediaID, leases[1].MediaID, leases[2].MediaID}
	if !slices.Equal(got, []int64{imported, missing, upgrade}) {
		t.Fatalf("lease order = %v", got)
	}
}
```

- [ ] **Step 2: Run the storage tests and verify the expected compile/schema failure**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -run 'TestLeaseDueSearchesOrdersByPriorityBeforeDueTime|TestSearchPriorityMigrationDefaults' -count=1
```

Expected: FAIL because `SearchPriority`, the migration columns, and the priority-aware upsert do not exist.

- [ ] **Step 3: Add the migration and minimum priority plumbing**

Create the migration with exact SQL:

```sql
ALTER TABLE search_states ADD COLUMN priority INTEGER NOT NULL DEFAULT 200;
ALTER TABLE search_states ADD COLUMN rerun_requested INTEGER NOT NULL DEFAULT 0 CHECK (rerun_requested IN (0, 1));

DROP INDEX search_due_idx;
CREATE INDEX search_due_idx
ON search_states(state, priority DESC, next_attempt_at_ns, lease_until_ns);
```

Add the priority type and fields:

```go
type SearchPriority int

const (
	SearchPriorityUpgrade SearchPriority = 100
	SearchPriorityMissing SearchPriority = 200
	SearchPriorityImport  SearchPriority = 300
)

type SearchLease struct {
	MediaID        int64
	Language       string
	JobID          string
	Attempt        int
	FailureAttempt int
	LeaseUntil     time.Time
	Priority       SearchPriority
}
```

Implement `UpsertSearchStateWithPriority`; make the existing `UpsertSearchState` call it with `SearchPriorityMissing`. Update inserts, selects, scans, and the due query to order by `priority DESC, next_attempt_at_ns, media_id, language`. Validate priorities through a private helper accepting only the three constants.

- [ ] **Step 4: Run the focused storage tests and verify green**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -run 'TestLeaseDueSearchesOrdersByPriorityBeforeDueTime|TestSearchPriorityMigrationDefaults' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing same-key rerun tests**

Add a repository test that leases an import, applies a second non-duplicate import for the same media/language, and asserts the owner remains unchanged. Complete the first lease and assert exactly one immediately due import-priority lease is returned; complete that lease and assert no third run exists.

- [ ] **Step 6: Run the rerun test and verify red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -run TestApplyMediaEventDuringLeaseRequestsOneRerun -count=1
```

Expected: FAIL because the current event upsert clears the active lease.

- [ ] **Step 7: Implement rerun-preserving event and completion SQL**

For import/rename conflict handling, use this conditional assignment list in its `ON CONFLICT` update:

```sql
state='pending',
attempt=0,
failure_attempt=0,
next_attempt_at_ns=excluded.next_attempt_at_ns,
priority=MAX(search_states.priority, excluded.priority),
rerun_requested=CASE WHEN search_states.lease_owner IS NULL THEN 0 ELSE 1 END,
lease_owner=search_states.lease_owner,
lease_until_ns=search_states.lease_until_ns
```

In `CompleteSearch`, perform one owner-guarded update whose `rerun_requested=1` branch clears the lease/flag, remains pending, preserves the stored priority and immediate event time, and ignores the stale attempt's ordinary next schedule. The normal branch applies current outcome/counter behavior plus any nonzero completion priority.

- [ ] **Step 8: Run all storage tests with the race detector**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/store -race -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the storage boundary**

```bash
git add internal/store/migrations/008_search_priorities.sql internal/store/repository.go internal/store/repository_test.go
git commit -m "feat: prioritize persisted subtitle searches"
```

---

### Task 2: Assign lifecycle priorities

**Files:**
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/schedule/scheduler.go`
- Modify: `internal/schedule/scheduler_test.go`
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`

**Interfaces:**
- Consumes: `store.SearchPriorityImport`, `store.SearchPriorityMissing`, `store.SearchPriorityUpgrade`.
- Produces: lifecycle-correct `store.SearchCompletion.Priority` values.

- [ ] **Step 1: Write failing priority-transition tests**

Cover these exact transitions:

```text
webhook import/rename -> import
reconciliation insert -> missing
no_result/rejected -> missing
installed/satisfied with NextUpgrade -> upgrade
throttled/workflow_error -> retain lease priority
```

Assert the persisted row after completion, not only the constructed struct.

- [ ] **Step 2: Run focused tests and verify red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/schedule ./internal/worker ./internal/store -run 'Priority|Import|Missing|Upgrade' -count=1
```

Expected: FAIL because callers still create unclassified searches and completions.

- [ ] **Step 3: Implement lifecycle assignments**

Pass import priority from `ApplyMediaEvent`, missing priority from reconciliation and missing-result scheduling, and upgrade priority from successful nonterminal results:

```go
case workflow.OutcomeSatisfied, workflow.OutcomeInstalled:
	completion.NextAttemptAt = result.NextUpgrade
	if !result.NextUpgrade.IsZero() {
		completion.Priority = store.SearchPriorityUpgrade
	}
case workflow.OutcomeNoResult, workflow.OutcomeRejected:
	completion = schedule.Scheduler{Clock: w.Clock, RandomUnit: w.RandomUnit}.
		Missing(lease.JobID, lease.Attempt+1)
	completion.Priority = store.SearchPriorityMissing
```

Leave priority zero for throttle and failure completions so repository completion retains the lease priority.

- [ ] **Step 4: Run affected packages with race detection**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/schedule ./internal/worker ./internal/store -race -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit lifecycle priority behavior**

```bash
git add internal/catalog internal/schedule internal/store internal/worker
git commit -m "feat: classify search lifecycle priorities"
```

---

### Task 3: Add configurable workflow concurrency

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `config.example.yaml`

**Interfaces:**
- Produces: `config.WorkerConfig{MaxConcurrent int}` at `Config.Worker`.
- Consumes: `Config.Worker.MaxConcurrent` as `worker.Worker.MaxWorkflows`.

- [ ] **Step 1: Write failing default, explicit-value, and bounds tests**

Add table cases proving omitted config yields one, explicit `4` is retained, and `0`, `-1`, and `9` are rejected when explicitly supplied. Use a pointer in `rawWorkerConfig` so omitted and explicit zero remain distinguishable.

- [ ] **Step 2: Run configuration tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/config -run TestWorkerMaxConcurrent -count=1
```

Expected: FAIL because the worker section is currently an unknown YAML field.

- [ ] **Step 3: Implement configuration normalization and validation**

Add:

```go
type WorkerConfig struct {
	MaxConcurrent int `yaml:"max_concurrent"`
}

type rawWorkerConfig struct {
	MaxConcurrent *int `yaml:"max_concurrent"`
}
```

Normalize omission to one and validate the inclusive `1..8` range. Wire the value into the default worker constructed in `app.New`. Add the documented YAML block to `config.example.yaml`.

- [ ] **Step 4: Run configuration and application tests**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/config ./internal/app -race -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit configuration support**

```bash
git add internal/config internal/app config.example.yaml
git commit -m "feat: configure workflow concurrency"
```

---

### Task 4: Wake the daemon after committed catalog work

**Files:**
- Modify: `internal/catalog/webhook.go`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**
- Produces: `WebhookHandler.OnApplied func()`.
- Produces: `Reconciler.OnCommitted func()`.
- Produces: one capacity-one `chan struct{}` in `app.New`, exposed to the worker as receive-only.

- [ ] **Step 1: Write failing callback tests**

Prove webhook callback behavior for: one applied event, a duplicate event, an ignored test event, and a two-file event whose first mutation commits and second fails. Prove successful reconciliation invokes once and failed reconciliation does not invoke.

- [ ] **Step 2: Run catalog tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog -run 'OnApplied|OnCommitted|Wake' -count=1
```

Expected: FAIL because callback fields do not exist.

- [ ] **Step 3: Implement callbacks without coupling catalog to worker**

Track whether any `ApplyMediaEvent` returned `true`; arrange callback invocation with a defer so a later event error cannot suppress a wake for already committed work. Invoke the reconciliation callback only after `CommitReconciliation` succeeds.

In `app.New`, create:

```go
wake := make(chan struct{}, 1)
notify := func() {
	select {
	case wake <- struct{}{}:
	default:
	}
}
```

Pass `notify` to handlers/reconcilers and `wake` to the default worker. Tests must prove repeated notifications never block.

- [ ] **Step 4: Run catalog and app tests with race detection**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/catalog ./internal/app -race -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit advisory wake wiring**

```bash
git add internal/catalog internal/app
git commit -m "feat: wake workers after catalog changes"
```

---

### Task 5: Replace daemon batch barriers with continuous refill

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`

**Interfaces:**
- Consumes: `Worker.Wake <-chan struct{}` and `Worker.MaxWorkflows`.
- Produces: daemon-only capacity-aware search dispatch; `RunOnce` remains deterministic for existing direct tests.

- [ ] **Step 1: Write failing continuous-dispatch tests**

Use controlled workflow channels rather than sleeps. Test that with two slots, job A starts, a webhook-style repository enqueue plus wake starts job B before A is released. Test that with one slot B starts immediately after A completes despite a one-hour poll interval. Assert maximum active workflows never exceeds the configured value.

```go
func TestRunWakeFillsFreeSlotBeforeActiveWorkflowCompletes(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	workflow := newControlledWorkflow()
	wake := make(chan struct{}, 1)
	w := testWorker(repository, workflow, testutil.NewClock(now))
	w.MaxWorkflows, w.PollInterval, w.Wake = 2, time.Hour, wake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	first := <-workflow.started
	repository.enqueueSearch(testSearchLease(2, now))
	wake <- struct{}{}
	second := <-workflow.started
	if first == second {
		t.Fatalf("started media IDs = %d and %d", first, second)
	}
	workflow.release(first)
	workflow.release(second)
}
```

Add the shown `newControlledWorkflow`, `started <-chan int64`, `release(mediaID)`, `enqueueSearch`, and `testSearchLease` test helpers beside the existing worker fakes; they must use channels and mutexes, not timing sleeps.

- [ ] **Step 2: Run worker tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/worker -run 'TestRunWakeFillsFreeSlot|TestRunCompletionRefillsSingleSlot' -count=1
```

Expected: FAIL because current `Run` cannot observe wakeups during `RunOnce`.

- [ ] **Step 3: Implement a capacity-aware dispatcher**

Add a private completion value:

```go
type searchDone struct{ err error }
```

The daemon loop owns `active`, leases with `limit := MaxWorkflows-active`, starts every returned lease immediately, and selects over context cancellation, `Wake`, jittered recovery timer, reconciliation/notification maintenance triggers, and `done`. A `done` event decrements `active`, reports its error, and dispatches again without waiting for the timer. Do not use a per-batch semaphore in daemon mode.

Keep `processSearchLease` responsible for renewal and outcome completion. Keep `RunOnce` using its bounded batch helper so existing deterministic maintenance and lease tests retain a simple execution path.

- [ ] **Step 4: Add and pass graceful-shutdown and lost-wake tests**

Test that cancellation stops new leases, drains an active job within the timeout, and cancels it after timeout. Test a pending row with no wake starts on daemon startup and another becomes visible on the recovery poll.

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/worker -race -count=1
```

Expected: PASS with no goroutine leaks or races.

- [ ] **Step 5: Commit continuous dispatch**

```bash
git add internal/worker
git commit -m "feat: continuously refill search workers"
```

---

### Task 6: Explain output, integration coverage, and documentation

**Files:**
- Modify: `internal/store/repository.go`
- Modify: `internal/cli/cli.go`
- Modify: `internal/cli/cli_test.go`
- Modify: `test/e2e/e2e_test.go`
- Modify: `README.md`
- Modify: `docs/operations.md`
- Modify: `docs/providers.md`
- Modify: `docs/implementation-status.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `explain` fields `priority=<import|missing|upgrade>` and `rerun_pending=<true|false>`.

- [ ] **Step 1: Write failing explain and end-to-end tests**

Assert human-readable names rather than numeric priorities. Extend the daemon e2e path to send a webhook while one controlled workflow is active and verify the second media/language begins without waiting for the recovery poll.

- [ ] **Step 2: Run focused CLI and e2e tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/cli -run TestExplainShowsSearchPriority -count=1
GOCACHE=/tmp/subsyncd-gocache go test ./test/e2e -tags=e2e -run TestWebhookWakeDispatchesPersistedSearch -count=1
```

Expected: FAIL because priority/rerun status is not exposed and daemon wake dispatch is not covered end-to-end.

- [ ] **Step 3: Implement explain mapping and update durable documentation**

Map only the three known constants; return an error for corrupt stored values. Document configuration, strict priority ordering, coalescing, advisory wake recovery, default serial LAPSE execution, and the fact that priority never bypasses provider throttles. Replace the old `AGENTS.md` batch/two-workflow invariant only after all tests pass.

- [ ] **Step 4: Run the complete verification gate**

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
git diff --check
```

Expected: all commands exit zero.

- [ ] **Step 5: Commit the completed queue feature**

```bash
git add internal/cli internal/store test/e2e README.md docs AGENTS.md
git commit -m "docs: document priority search dispatch"
```

- [ ] **Step 6: Push and observe publication**

Push `main`, wait for verification and the multi-architecture image job, and report the commit, workflow URL, image tag, and test evidence to the user. Do not deploy the daemon to production as part of this plan.
