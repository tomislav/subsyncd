# Fallback Providers Implementation Plan

> Execute task-by-task with test-driven development and final independent review.

**Goal:** Install safe fallback subtitles and promote them when preferred subtitles become available.
**Architecture:** Per-language tier coordinators feed sequential workflow acquisition passes after one inventory check; installation provenance records fallback independently of score.
**Tech Stack:** Go, SQLite migrations, existing provider/workflow/worker services.
**Spec:** `docs/superpowers/specs/2026-09-08-fallback-providers-design.md`

## Global Constraints

Preserve score/identity/LAPSE gates, protected files, leases, rollback/outbox atomicity,
cancellation and technical-vs-deterministic rejection rules. No production calls.

## Tasks

- [x] Configuration: add `LanguageConfig.FallbackProviders []string` and `AllProviders() []string`; test YAML loading plus unknown/duplicate/empty-primary validation. Run config with race.
- [x] Persistence: add `Installation.Fallback bool` and migration 006, roundtrip it through insert/upsert/read/assessment and clear on fingerprint invalidation; test reopen and replacement. Run store with race.
- [x] Acquisition: extract one acquisition pass from `Run`, add `FallbackSearcher` and `FallbackProviderOrder`; filter cache by active provider set, retain scored rows across passes and carry candidate-local exhausted errors separately from terminal errors. Test preferred broad before fallback exact; empty/rejected/outage fallback; cancellation and publication stop.
- [x] Promotion: use current tier membership in upgrade policy, preserve exact fallback promotion, block downgrade, pass `InstallRequest.Fallback` into atomic persistence; schedule fallback weekly and preserve future checks on no improvement. Test same-tier delta, exact promotion, protected/missing files and provenance.
- [x] Assembly/diagnostics: build and validate both coordinators, include fallback provider states in explain, display stored fallback flag. Test independent language routes and unsupported fallback language.
- [x] Manual scheduling: persist successful `NextUpgrade` results with `EnsureUpgradeSearch`, reopening completed rows while preserving leases and earlier pending work; test absent/complete/pending/leased/deleted/unsupported rows and manual fallback installation.
- [x] Documentation/review: update provider guides, example config, contributor invariants and handoff ledger. Run full race suite, vet, tagged e2e and diff check; independent review and fix findings.

Each production task starts with a focused failing test, confirms the failure, then
implements the smallest change and runs its affected package with `-race`.

## Verification and review

Focused config/store/workflow/app/provider tests demonstrated missing behavior before
implementation. Full `go test ./... -race -count=1`, `go vet ./...`, and tagged
`test/e2e` race tests passed. Workflow/app race tests were repeated after the final
refreshed-score reporting correction. All tests use local fixtures/fake services.
Review identified partial preferred cooldown scheduling and search-cache persistence
classification gaps; both were fixed and regression-tested. Manual NextUpgrade
persistence was added after tracing the CLI path; it preserves leases and earlier work.
