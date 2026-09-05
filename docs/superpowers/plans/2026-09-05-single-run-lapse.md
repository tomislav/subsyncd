# Single-run LAPSE Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Eliminate the winner's second LAPSE invocation without weakening candidate acceptance, selection or installation.

**Architecture:** Prepare each LAPSE-required candidate through the existing validated SynchronizeCandidate output path. Retain source identity, produced output and SyncResult through tier ranking and install/fallback. Keep standalone Analyze diagnostics.

**Tech Stack:** Go 1.27.1, SQLite, LAPSE v2.0.5, existing local fake-server and fixture tests.

**Spec:** docs/superpowers/specs/2026-09-05-single-run-lapse-design.md

## Global Constraints

- Exact-hash/qualified score bypass makes zero LAPSE calls; no scoring/config/provider change.
- Every acquisition LAPSE candidate makes one output-producing strict invocation, never dry-run plus synchronization.
- Preserve source rejection identity, cache immutability, lazy equal-score tiers, original installation/outbox/rollback/fingerprint guards and bounded temporary cleanup.
- Standalone analyze-sync remains read-only dry-run. No live provider or production activity.
- Worktree /tmp/subsyncd-single-run-lapse, base 3ad9aee; existing verification-note edit is carried forward.

### Task 1: Prepare installable candidates once, retain and rank artifacts

**Files:** internal/workflow/service.go and affected workflow tests; internal/syncer/lapse_test.go only if a missing boundary regression needs coverage; test/e2e/e2e_test.go and any directly affected app/worker fixtures. Controller owns general docs and upstream runtime verification.

**Interfaces:** consume existing SynchronizeCandidate(context.Context, domain.Candidate, string, string, string) (domain.SyncResult, error). Workflow no longer requires AnalyzeCandidate in its CandidateSynchronizer interface. Keep syncer's diagnostic Analyze entry points. A private prepared-candidate value must retain selected source path separately from output path and the single SyncResult; exact field naming may follow existing conventions.

- [x] Write and run a focused failing unique-leader regression before production edits. Adapt the real service test expectation to require the installed candidate plus:
```go
if len(synchronizer.analyzed) != 0 || !slices.Equal(synchronizer.synchronized, []string{"leader"}) {
    t.Fatalf("analysis/sync = %#v/%#v", synchronizer.analyzed, synchronizer.synchronized)
}
```
Run `go test ./internal/workflow -run TestRun -count=1` with writable caches, record the specific expected RED (analyzed leader remains). Narrow the command to the actual selected test when identified.

- [x] Replace analysis/finalization orchestration with one preparation call per required candidate. Reuse the existing safe syncer rather than parsing JSON or applying offsets in workflow. Retain source/output separately, allocate collision-free direct workspace outputs compatible with cleanup, rank by the produced SyncResult and install retained output. Apply to cached packs and broad tiers; preserve exact bypass and metadata-only reassessment. Remove obsolete duplicated confidence assignment and acquisition analysis/finalize events.

- [x] Add focused RED/GREEN regressions for three viable ties (sync list all three, analysis empty, highest-confidence artifact actually installed), tie/lower-tier preparation failure, installer-local fallback without repeated LAPSE, cached-pack/provider fallback output uniqueness and cache immutability, original-input rejection checksum, prompt failed-output cleanup, cancellation and terminal technical errors. Update old analysis-error fixtures to fail the operation actually used, retaining equivalent coverage rather than deleting assertions. Assert real installed bytes/provenance alongside process-count/ordering spies.

- [x] Update lifecycle and E2E assertions to one acquisition sync start/completion and no analysis events; keep diagnostic dry-run tests. Preserve solid/written/path/file/syntax validation tests. Run affected workflow/syncer/app/worker race packages and tagged E2E.

- [x] Self-review diff, append implementation-status task boundary with tests and next review, write task-1-report.md in this plan's SDD workspace with exact RED/GREEN evidence and commit SHA. Commit owned code/tests/ledger as `perf: prepare LAPSE candidates in a single run`. No push by implementer; no nested agents.

## Controller final integration

- [x] Task review, fix and scoped re-review as necessary; update operations/providers/architecture/AGENTS and relevant historical design status to point to the new approved contract.
- [x] Validate pinned Linux binary on synthetic subtitle fixtures without network/media mounts if local Docker is available; compare dry/output metrics and strict refusal, record platform/limits.
- [x] Full `go test ./... -race -count=1`, `go vet ./...`, tagged E2E race, Dockerfile parity, Compose config --quiet, formatting/diff checks.
- [x] Independent whole-branch review and integration ledger complete; original main patch verified to match the carried-forward note. Authorized fast-forward/push is the final publication action after this documentation commit. No production deployment.
