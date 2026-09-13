# Repeated Silent LAPSE Failure Quarantine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Quarantine one immutable subtitle artifact after two separate, identical LAPSE exit-2 failures with completely empty stdout and stderr, while retaining technical handling for every other process failure.

**Architecture:** The syncer exposes a credential-free typed process exit. SQLite stores one bounded strike keyed by the existing rejection identity and atomically promotes strike two into a normal candidate rejection. The workflow invokes that transition only after it has checksummed the selected artifact, then reuses existing deterministic-candidate progression.

**Tech Stack:** Go 1.27.1, modernc SQLite, embedded SQL migrations, existing workflow/syncer/store packages.

**Spec:** `docs/superpowers/specs/2026-09-13-repeated-silent-lapse-failure-design.md`

## Global Constraints

- Use TDD: add a focused failing test and observe the intended failure before production code.
- Never expose LAPSE stdout, stderr, paths, command arguments, provider references, or raw runner errors.
- Only exit code 2 with both stdout and stderr empty is strike-eligible.
- Strike identity includes the exact media fingerprint, stable candidate signature, selected artifact checksum, tool/policy signature, language, provider/result, and failure signature.
- The first strike remains technical; the second strike and candidate rejection are one SQLite transaction.
- All other process/protocol/filesystem/cancellation failures remain technical.
- Do not perform provider, Arr, Silo, or production requests in tests.

---

### Task 1: Sanitized typed LAPSE process exit

**Files:**
- Modify: `internal/syncer/lapse.go`
- Modify: `internal/syncer/lapse_test.go`

**Interfaces:**
- Produces: `syncer.ProcessExitError` with `ExitCode int`, `StdoutEmpty bool`, and `StderrEmpty bool` plus an `Error()` string that includes only the exit code.
- Preserves: `NoSpeechError`, `VerdictError`, invalid-output validation, timeouts, cancellations, and runner failures.

- [ ] **Step 1: Write failing adapter tests**

Add table-driven tests that execute `interpret` through the public synchronization path and assert `errors.As` finds `*ProcessExitError` for undecodable nonzero process output. Cover exit 2 with two empty buffers, exit 2 with stdout only, exit 2 with stderr only, and another nonzero exit. Assert the fields exactly and assert `Error()` contains neither fixture buffer nor any path/token fixture. Retain a control proving the existing no-speech stderr becomes `NoSpeechError`, not `ProcessExitError`.

- [ ] **Step 2: Verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/syncer -race -count=1
```

Expected: FAIL because `ProcessExitError` does not exist or the generic exit error cannot be inspected safely.

- [ ] **Step 3: Implement the minimal typed error**

Define the exported type in `internal/syncer/lapse.go`. In `interpret`, after no-speech classification and before generic protocol failure, return it for any undecodable nonzero exit. Derive emptiness from `len(execution.Stdout) == 0` and `len(execution.Stderr) == 0`; do not retain the buffers on the error.

- [ ] **Step 4: Verify GREEN and commit**

Run the Task 1 command, `gofmt -w internal/syncer/lapse.go internal/syncer/lapse_test.go`, rerun the package, then commit:

```bash
git add internal/syncer/lapse.go internal/syncer/lapse_test.go
git commit -m "feat: classify silent LAPSE process exits"
```

---

### Task 2: Durable atomic strike promotion

**Files:**
- Create: `internal/store/migrations/013_repeated_lapse_failures.sql`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/store/provider_resume_test.go`

**Interfaces:**
- Consumes: the existing `CandidateRejection` full identity.
- Produces: `CandidateLapseFailure` containing the rejection identity, `FailureSignature string`, and observation time.
- Produces: `Repository.RecordCandidateLapseFailure(context.Context, CandidateLapseFailure) (confirmed bool, error)`.
- Tightens: `Repository.ClearCandidateRejections` atomically clears both rejection and strike rows for a media/language.

- [ ] **Step 1: Write failing repository and migration tests**

Cover: first record returns false and survives reopen; second identical record returns true, inserts exactly one rejection with reason `lapse_repeated_empty_exit`, and removes the strike in the same transaction; every individual fingerprint/signature/artifact/tool/failure field change starts at strike one; a trigger that rejects the candidate-rejection insert rolls back strike two; clear removes both tables; media deletion cascades; migration 013 preserves representative rows from every existing durable table, including provider-resume state.

- [ ] **Step 2: Verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -race -count=1
```

Expected: FAIL because migration 013 and the repository transition do not exist.

- [ ] **Step 3: Add schema and atomic repository transition**

Create a table with a cascading `media_id` foreign key, all stable identity columns, `failure_signature`, `occurrence_count` constrained to 1..2, and first/last timestamps. Use a unique constraint spanning the complete identity. In one transaction, insert strike one or increment the existing row; when the count reaches two, upsert the corresponding `candidate_rejections` row with reason `lapse_repeated_empty_exit`, delete only the matching strike, and commit. Validate all required identity fields and timestamps before opening the transaction. Update `ClearCandidateRejections` to delete both tables in one transaction.

- [ ] **Step 4: Verify GREEN and commit**

Run the Task 2 package with `-race`, format changed Go, run `git diff --check`, then commit:

```bash
git add internal/store/migrations/013_repeated_lapse_failures.sql internal/store/repository.go internal/store/repository_test.go internal/store/provider_resume_test.go
git commit -m "feat: persist repeated silent LAPSE failures"
```

---

### Task 3: Workflow quarantine and candidate progression

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/member_preparation.go` if version aggregation requires it
- Modify: `internal/workflow/service_test.go`
- Modify or create: `internal/workflow/repeated_lapse_failure_test.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**
- Consumes: `*syncer.ProcessExitError` and `Repository.RecordCandidateLapseFailure`.
- Eligibility predicate: exit code 2, empty stdout, empty stderr, nonempty selected artifact checksum.
- Confirmed transition behaves exactly like an existing deterministic candidate rejection during the current workflow.

- [ ] **Step 1: Write failing workflow tests**

Use a persistent SQLite repository for the core regression. First run the same media/candidate/artifact with silent exit 2 and assert a technical error, failure scheduling input, no candidate rejection, and one strike. Run again and assert the confirmed rejection exists, the strike is gone, the bad candidate is not treated as technical, and the workflow advances to and installs a later candidate. Add controls for nonempty stderr, nonempty stdout, exit 1, timeout/cancellation, unavailable checksum, changed artifact/media/tool signature, and repository failure. Add an app/manual-retry test proving rejected-candidate reset clears an unconfirmed strike as well as confirmed rejections.

- [ ] **Step 2: Verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow ./internal/app -race -count=1
```

Expected: FAIL because workflow does not record or promote silent failures and its repository interface lacks the transition.

- [ ] **Step 3: Implement narrow workflow integration**

Extend the workflow repository interface. In `handleCandidateFailure`, compute the checksum first as today, detect only the eligible typed error, build the same candidate/member/tool/media identity used by `candidateRejection`, and call the atomic store transition. On strike one append the original technical failure. On confirmation log a `candidate.rejected` decision with reason `lapse_repeated_empty_exit`, do not append the technical failure, and allow existing candidate/version/provider progression. Leave `recordCandidateRejection` unchanged for all established deterministic failures.

- [ ] **Step 4: Verify GREEN and commit**

Run the Task 3 packages with `-race`, format changed Go, run `git diff --check`, then commit:

```bash
git add internal/workflow internal/app/app_test.go
git commit -m "feat: quarantine repeated silent LAPSE failures"
```

---

### Task 4: Contract documentation and complete verification

**Files:**
- Modify: `AGENTS.md`
- Modify: `docs/development/providers.md`
- Modify: `docs/development/operations.md`
- Modify: `docs/implementation-status.md`
- Modify: `test/e2e/e2e_test.go` only if package-level integration cannot prove restart persistence through assembled services

**Interfaces:**
- Documents migration 013, the exact two-strike exception, reset semantics, and unchanged technical failure classes.

- [ ] **Step 1: Add or confirm assembled restart coverage**

Add the smallest necessary assembled-app or tagged E2E regression proving strike one survives close/reopen and strike two permits a later candidate. Do not duplicate lower-level coverage if the persistent workflow test already crosses the real assembly boundary.

- [ ] **Step 2: Update authoritative documentation**

Amend the process-failure invariants in all listed documents. In `docs/implementation-status.md`, record the final commit(s), adopted narrow exception, rejected broader first-exit quarantine, tests run, and next task. Do not claim review or verification before those gates run.

- [ ] **Step 3: Run complete verification**

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
gofmt -l cmd internal test
git diff --check
```

Expected: every command exits 0; `gofmt -l` prints nothing.

- [ ] **Step 4: Commit documentation**

```bash
git add AGENTS.md docs/development/providers.md docs/development/operations.md docs/implementation-status.md test/e2e/e2e_test.go
git commit -m "docs: record repeated LAPSE quarantine"
```

