# Missed Arr Delete Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Retire Sonarr series and Radarr movies whose whole-delete webhooks were missed while subsyncd was unavailable.

**Architecture:** Extend the catalog with a complete top-level identity snapshot and the store with active tracked identities. Reconciliation computes missing identities and commits their existing delete mutations atomically with history work under the current cursor/event-revision fence. Full-library discovery stays additive and individual file/episode absence remains history/webhook-driven.

**Tech Stack:** Go 1.27.1, arrapi v2.0.5, SQLite, table-driven tests and local fakes.

**Spec:** `docs/superpowers/specs/2026-09-13-missed-arr-delete-reconciliation-design.md`

## Global Constraints

- Snapshot-derived deletes require a complete, validated Arr response.
- A snapshot/read/validation/cancellation failure deletes nothing and does not advance the cursor.
- Concurrent instance events invalidate the complete reconciliation transaction.
- Sonarr deletes only positive stored series IDs absent from the current series set.
- Radarr deletes only positive stored movie entity IDs absent from the current movie set.
- Legacy Sonarr rows with `series_id = 0` remain active.
- Individual missing files or episodes are never inferred from snapshot omission.
- Startup/readiness remain offline and tests never contact production Arr or subtitle providers.

---

### Task 1: Complete Arr identity snapshot contract

**Files:**
- Modify: `internal/catalog/catalog.go`
- Modify: `internal/catalog/sonarr.go`
- Modify: `internal/catalog/radarr.go`
- Test: `internal/catalog/library_test.go`

**Interfaces:**
- Produces: `CatalogIdentitySnapshot` containing media kind and a validated set of positive top-level Arr IDs.
- Produces: optional `IdentitySnapshotCatalog.ListIdentitySnapshot(context.Context) (CatalogIdentitySnapshot, error)` implemented by Sonarr and Radarr.

- [ ] Add focused Sonarr and Radarr tests for complete positive identity enumeration, empty snapshots, invalid IDs, duplicate IDs, and adapter errors.
- [ ] Run the focused tests and confirm RED because the snapshot interface and methods do not exist.
- [ ] Implement the smallest adapter methods using existing arrapi `Series`/`Movies` clients and owned safe error wrapping.
- [ ] Run `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -run 'IdentitySnapshot' -race -count=1` and confirm GREEN.
- [ ] Commit the task.

### Task 2: Active tracked identity store query

**Files:**
- Modify: `internal/store/repository.go`
- Test: `internal/store/reconciliation_test.go` or a focused new store test file

**Interfaces:**
- Produces: `ListActiveCatalogIdentities(context.Context, instance string, kind domain.MediaKind) ([]int64, error)`.
- Sonarr returns distinct positive `series_id`; Radarr returns distinct positive `entity_id`.

- [ ] Add real-SQLite tests for active/deleted rows, duplicate series IDs, legacy zero IDs, kind isolation, instance isolation, and deterministic ordering.
- [ ] Run the focused store tests and confirm RED because the query does not exist.
- [ ] Implement the minimal validated read-only query with row and close error handling.
- [ ] Run the focused store package tests with `-race` and confirm GREEN.
- [ ] Commit the task.

### Task 3: Atomically reconcile missing top-level identities

**Files:**
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify as required: catalog/app test fakes implementing reconciliation contracts

**Interfaces:**
- Consumes: `IdentitySnapshotCatalog.ListIdentitySnapshot` from Task 1.
- Consumes: `ReconciliationStore.ListActiveCatalogIdentities` from Task 2.
- Produces: existing series-addressed and entity-addressed `store.MediaEventMutation` values with deterministic snapshot event IDs.

- [ ] Add a failing reconciler regression proving an empty complete Sonarr snapshot retires all episodes for a tracked series after a missed webhook.
- [ ] Add a failing Radarr regression proving a missing movie ID is retired while present movies remain active.
- [ ] Add controls for legacy zero series IDs, instance/kind isolation, idempotent replay, history plus snapshot mutation composition, and empty tracked state.
- [ ] Add failure controls proving snapshot errors and invalid kind/identity data cause no commit or cursor advancement.
- [ ] Extend the existing concurrent webhook test so a snapshot fetched before the event-revision change cannot delete or resurrect media.
- [ ] Run focused tests and confirm the expected RED failures.
- [ ] Implement set difference and deterministic mutation construction in the reconciler, requiring the snapshot capability for configured production catalogs.
- [ ] Run `go test ./internal/catalog ./internal/store ./internal/app -race -count=1` with writable caches and confirm GREEN.
- [ ] Commit the task.

### Task 4: Operational documentation and compatibility

**Files:**
- Modify: `docs/operations.md`
- Modify: `docs/implementation-status.md`
- Modify if applicable: `docs/release-notes.md`
- Modify: `AGENTS.md`
- Modify: `docs/superpowers/specs/2026-09-05-arrapi-stable-identity-reconciliation-design.md`
- Modify: `docs/superpowers/specs/2026-09-08-full-library-discovery-design.md`

- [ ] Document that complete top-level snapshots recover missed whole-delete webhooks for Sonarr and Radarr.
- [ ] Preserve and clarify that discovery remains additive and individual omissions are not tombstoned.
- [ ] Update the contributor invariant and implementation ledger with exact behavior, commits, tests, and next step.
- [ ] Run documentation/link-oriented checks available in the repository plus `git diff --check`.
- [ ] Commit the task.

### Task 5: Review and full verification

**Files:**
- Review all changes since design commit `971d36e`.

- [ ] Request independent code review of the complete implementation diff.
- [ ] Address findings with focused RED/GREEN regressions where behavior changes.
- [ ] Run `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1`.
- [ ] Run `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...`.
- [ ] Run `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1`.
- [ ] Run `gofmt -l cmd internal test` and `git diff --check`; both must be empty.
- [ ] Update `docs/implementation-status.md` with final exact commit and verification evidence, then commit.
- [ ] Do not deploy or mutate production as part of this plan.
