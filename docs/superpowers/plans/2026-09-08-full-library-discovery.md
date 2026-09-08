# Full-library discovery implementation plan

Spec: ../specs/2026-09-08-full-library-discovery-design.md

1. Catalog task: add `ListLibrary(context.Context) ([]domain.Media,error)` to concrete Sonarr/Radarr via an optional LibraryCatalog interface; arrapi enumeration and existing hydration, scope checks, strict IDs, deterministic dedup. Focused RED/GREEN and catalog race.
2. Store task: migration 005 adds per-instance library_scope and event revision trigger; snapshot state read and atomic additive import with optimistic revision fence, stable/file identity preservation, missing-priority scheduling, completion even for empty pass. Tests for rollback, existing rows/leases/deletes, races and restart.
3. Integration task: Reconciler optionally runs discovery once before history, explicit scan forces it, scope hash derived from mappings/roots/instance type/URL (never logged). Reuse worker backoff and wakes, keep assembly offline. Add application and E2E coverage for both empty-history libraries.
4. Review, documentation and final race/vet/tagged E2E verification. No commit/deployment.

Interface agreement: task 1 produces LibraryCatalog.ListLibrary for task 3. Task 2 produces LibraryDiscoveryState(ctx,instance) (scope string, revision int64,error) and CommitLibraryDiscovery(ctx,instance,scope string,revision int64,items []domain.Media,languages []domain.Language,at time.Time) error. Task 3 consumes both. Tasks 1 and 2 own separate catalog/store files; integration owns reconcile/app files. Existing Catalog fakes can omit optional library interface; actual adapters provide it. Each task's tests target its output contract. No shared-file conflict expected.

Ruling: use additive discovery, leaving existing catalog rows to existing reconciliation — preserves user content and durable schedules while filling missing entries. Revision fencing is conservative and can delay discovery on a busy instance, but cannot overwrite newer events.

All four tasks completed locally. Catalog/store/app regressions, full repository race tests, tagged E2E race, vet, formatting and diff checks passed. Independent final review found no remaining actionable issues. Commit and push authorized after verification; deployment remains separate.
