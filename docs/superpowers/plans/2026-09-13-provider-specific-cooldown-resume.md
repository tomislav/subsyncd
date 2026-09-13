# Provider-Specific Cooldown Resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist clean no-result provider progress during a throttled first-install cycle so reset retries contact only unfinished providers.

**Architecture:** SQLite stores a bounded provider resume list and route signature on each search row. The provider coordinator reports per-phase clean-empty completion, the workflow combines exact/broad and preferred/fallback evidence, and the worker atomically carries valid resume state through throttled or technical retry completion while clearing it at lifecycle boundaries.

**Tech Stack:** Go 1.27.1, modernc SQLite, embedded forward-only SQL migrations, structured `slog` NDJSON, existing provider/workflow/worker interfaces.

**Spec:** `docs/superpowers/specs/2026-09-13-provider-specific-cooldown-resume-design.md`

## Global Constraints

- Apply only to first-install work; routine upgrades retain their existing cadence and full provider route.
- Skip only providers that completed every applicable exact/broad phase without candidates or errors in the same unfinished cycle.
- Never persist candidate/download references, provider responses, credentials, endpoints, cache keys, or media paths in resume state.
- A provider excluded by valid resume state performs no cache, availability, logging-lifecycle, or remote access.
- Provider cooldowns still advance neither missing nor failure attempts and remain scheduled at or after the earliest reset.
- Preserve preferred/fallback ordering, candidate scoring, deterministic rejections, LAPSE, installation atomicity, queue priority, leases, rerun coalescing, and provider-state gates.
- Every production change follows focused RED/GREEN TDD and each affected package runs with `-race`.
- The production container remains stopped; no production database, provider, Arr, Silo, cache, or media operation occurs during implementation.

---

### Task 1: Report clean-empty provider phases and honor exclusions

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/provider/coordinator.go`
- Modify: `internal/provider/coordinator_test.go`
- Modify: `internal/provider/download_availability_test.go`
- Modify: `internal/provider/cache_refresh_test.go`

**Interfaces:**
- Extend `provider.SearchQuery` with `SkipProviders []string`; adapters continue ignoring this coordinator-only field.
- Extend `provider.SearchResult` with `ApplicableProviders []string` and `EmptyProviders []string`, both in configured order.
- Preserve `Searcher.Search(context.Context, provider.SearchQuery) provider.SearchResult`.

- [ ] **Step 1: Write a failing exclusion boundary test**

Add `TestCoordinatorSkippedProviderHasNoObservableAccess`. Configure `skipped` and `active` providers, set `SkipProviders: []string{"skipped"}`, and give `skipped` a cache implementation/provider wrapper that fails the test on cache, availability, log lifecycle, or `Search`. Assert only `active` appears in `ApplicableProviders` and supplies candidates.

```go
result := coordinator.Search(t.Context(), SearchQuery{
	Media: testQueryMedia(), Language: "en", Mode: SearchBroad,
	SkipProviders: []string{"skipped"},
})
if got := result.ApplicableProviders; !slices.Equal(got, []string{"active"}) {
	t.Fatalf("applicable providers = %v", got)
}
```

- [ ] **Step 2: Run the exclusion test and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -run TestCoordinatorSkippedProviderHasNoObservableAccess -count=1
```

Expected: FAIL because `SearchQuery.SkipProviders` and phase evidence do not exist and the skipped provider is accessed.

- [ ] **Step 3: Implement exclusion before provider access**

Add a private normalized membership helper and apply it at the top of both exact and broad provider loops, before capability/language/media checks call provider methods that can access persisted state. Populate `ApplicableProviders` only for non-skipped providers that support the phase. Do not include exclusions in `providerCacheKey`; excluded providers never reach `searchProvider`.

- [ ] **Step 4: Write failing clean-empty accounting tests**

Cover exact and broad modes with four providers: empty remote result, empty cache hit, candidate result, and cooldown/error. Assert configured ordering, and assert `EmptyProviders` contains only the two successful empty providers. Add a control that an exact request returning only a non-exact candidate is not classified clean-empty.

- [ ] **Step 5: Run the accounting tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -run 'TestCoordinatorReportsCleanEmptyProviders|TestCoordinatorNonExactCandidateIsNotCleanEmpty' -count=1
```

Expected: FAIL because `SearchResult.EmptyProviders` is absent.

- [ ] **Step 6: Implement deterministic phase evidence**

For each applicable provider, append its ID to `EmptyProviders` only when `searchProvider` returns `err == nil` and `len(candidates) == 0`. Candidate-bearing, canceled, disabled, cooldown, quota, persistence, transport, and invalid-response cases remain absent. Preserve provider-order reduction after concurrent broad searches.

- [ ] **Step 7: Run provider tests with race detection**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... -race -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the coordinator boundary**

```bash
git add internal/provider
git commit -m "feat: expose resumable provider search progress"
```

---

### Task 2: Persist bounded resume state in the search queue

**Files:**
- Create: `internal/store/migrations/012_provider_resume.sql`
- Create: `internal/store/provider_resume_test.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/store/upgrade_search.go`
- Modify: `internal/store/upgrade_search_test.go`

**Interfaces:**
- Add `SearchLease.ResumeProviders []string` and `SearchLease.ResumeRouteSignature string`.
- Add `SearchCompletion.ResumeProviders []string`, `SearchCompletion.ResumeRouteSignature string`, and `SearchCompletion.PreserveResume bool`.
- Add `Repository.ClearSearchResume(ctx context.Context, mediaID int64, language domain.Language) error`.
- Add private strict helpers `decodeResumeProviders(string) ([]string, error)` and `encodeResumeProviders([]string) (string, error)`.

- [ ] **Step 1: Write the failing migration preservation/reschedule test**

Create a database through migration 011, seed active/deleted media and search rows covering: uninstalled missing-priority throttled, leased throttled, installed throttled, upgrade throttled, `no_result`, `rejected`, and complete. Seed installations, candidate rejections, provider state, and provider cache. Reopen with `store.Open` and assert:

```text
new columns default to [] / empty signature
only active uninstalled missing-priority throttled rows have next_attempt_at_ns = 0
attempts, failure attempts, priority, leases, installations, rejections,
provider state, and provider-cache expiry are byte-for-byte preserved
```

Close and reopen again to prove the migration is one-shot.

- [ ] **Step 2: Run the migration test and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -run TestProviderResumeMigrationPreservesStateAndReschedulesThrottle -count=1
```

Expected: FAIL because migration 012 and its columns do not exist.

- [ ] **Step 3: Add migration 012**

Use additive columns and a scoped update:

```sql
ALTER TABLE search_states ADD COLUMN resume_providers_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE search_states ADD COLUMN resume_route_signature TEXT NOT NULL DEFAULT '';

UPDATE search_states
SET next_attempt_at_ns = 0
WHERE state='pending' AND priority=200 AND last_outcome='throttled'
  AND NOT EXISTS (
    SELECT 1 FROM installations
    WHERE installations.media_id=search_states.media_id
      AND installations.language=search_states.language
  )
  AND EXISTS (
    SELECT 1 FROM media
    WHERE media.id=search_states.media_id AND media.deleted=0
  );
```

Do not clear or rewrite lease columns; a retained lease controls eligibility until expiry.

- [ ] **Step 4: Write failing repository round-trip and validation tests**

Lease a row containing `['titlovi-main','subdl-main']` and a signature, assert decoded order, complete it as throttled with replacement state, then lease after the next due time and assert persistence. Add corrupt JSON, duplicate ID, empty ID, non-string member, and oversized-list fixtures; each `LeaseDueSearches` call must return a bounded generic repository error before claiming any row.

- [ ] **Step 5: Run repository tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -run 'TestSearchResumeRoundTrip|TestSearchResumeRejectsMalformedState' -count=1
```

Expected: FAIL because lease/completion do not read or write resume fields.

- [ ] **Step 6: Implement strict storage and atomic completion**

Use a small constant bound (maximum eight configured providers) and reject duplicate/empty IDs. Select resume columns in the due query, decode before claims, and serialize completion state before opening its transaction. In `CompleteSearch`, set resume columns only when `rerun_requested=0 AND PreserveResume=1`; otherwise set them to `[]`/empty signature. The deleted and `rerun_requested=1` branches always clear resume state.

Update `UpsertSearchStateWithPriority`, `scheduleMediaSearchTx` for import/rename/unsupported, and `EnsureUpgradeSearch` transitions so authoritative resets and upgrade work clear resume state without disturbing active lease ownership/rerun semantics.

- [ ] **Step 7: Add and pass explicit clear/reset tests**

Test `ClearSearchResume`, import/rename during a lease, unsupported/deleted completion, ordinary upsert, and upgrade scheduling. Each must clear resume state at the specified boundary while preserving the pre-existing lease/rerun guarantees.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -race -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit durable queue state**

```bash
git add internal/store
git commit -m "feat: persist provider cooldown resume state"
```

---

### Task 3: Resume only clean-empty providers across workflow tiers

**Files:**
- Create: `internal/workflow/provider_resume.go`
- Create: `internal/workflow/provider_resume_test.go`
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/provider_tiers.go`
- Modify: `internal/workflow/provider_tiers_test.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `internal/workflow/media_kind_test.go`

**Interfaces:**
- Add request fields `ResumeProviders []string` and `ResumeRouteSignature string`.
- Add result fields `ResumeProviders []string` and `ResumeRouteSignature string`.
- Add `func (s *Service) RouteSignature(language domain.Language) string` using SHA-256 over canonical language and ordered preferred/fallback provider IDs.
- Add private phase accumulator methods that consume `provider.SearchResult` and return configured-order clean-empty IDs.

- [ ] **Step 1: Write failing route-signature validation tests**

Assert stable signatures for identical routes and different signatures for language, membership, ordering, or tier changes. Run a service with a mismatched signature and confirm it sends no exclusions; run with a matching signature plus unknown/duplicate/empty resume IDs and assert validation fails before inventory/provider access.

- [ ] **Step 2: Run signature tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -run 'TestProviderRouteSignature|TestServiceValidatesProviderResume' -count=1
```

Expected: FAIL because route signatures and request resume fields do not exist.

- [ ] **Step 3: Implement route identity and request validation**

Create `provider_resume.go` with length-prefixed input to avoid delimiter ambiguity:

```go
func providerRouteSignature(language domain.Language, preferred, fallback []string) string {
	h := sha256.New()
	writePart(h, language.String())
	for _, id := range preferred { writePart(h, "preferred"); writePart(h, id) }
	for _, id := range fallback { writePart(h, "fallback"); writePart(h, id) }
	return hex.EncodeToString(h.Sum(nil))
}
```

When the stored signature differs, ignore all exclusions. When it matches, validate unique nonempty IDs against current preferred/fallback membership before inventory refresh.

- [ ] **Step 4: Write the failing three-provider resume scenario**

Use preferred providers `open`, `titlovi`, `subdl`. First run: `open` cooldown, `titlovi` and `subdl` clean-empty. Assert throttled with resume providers `[titlovi, subdl]`. Second run supplies that state: assert only `open` is applicable/called. Cover both `open` still throttled (state retained, earliest reset) and `open` clean-empty (ordinary `no_result`, resume cleared).

- [ ] **Step 5: Run the scenario and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -run TestServiceResumesOnlyUnfinishedProviders -count=1
```

Expected: FAIL because the workflow neither accumulates phase progress nor excludes completed providers.

- [ ] **Step 6: Implement exact/broad clean-empty intersection**

Track, per tier, exact applicable/empty/candidate/error provider IDs and broad applicable/empty/candidate/error IDs. Mark a provider newly resumable only when broad is empty-success and its applicable exact phase, if any, was also empty-success. Build the result list by filtering the full configured preferred-then-fallback order against the union of prior valid resume state and new clean-empty providers.

Set resume output before every throttled or technical return. Never add candidate-bearing providers, including deterministic rejections. Clear result resume fields for installed, satisfied, ordinary no-result/rejected after all unfinished providers ran, and any active installation/upgrade path.

- [ ] **Step 7: Add failing tier/error controls**

Cover:

```text
preferred cooldown + fallback empty -> fallback skipped on preferred retry
preferred cooldown + fallback install -> install and no resume state
exact empty + broad empty -> resumable
exact candidate/rejection + broad empty -> not resumable
exact error + broad empty -> not resumable
clean-empty peer + candidate download cooldown -> peer resumable
clean-empty peer + candidate technical failure -> peer retained for technical retry
active installation + partial cooldown -> no resume and normal upgrade result
```

- [ ] **Step 8: Run controls RED, implement minimal tier merge, then GREEN**

Run the new tests first and confirm the expected failures. Update `runProviderTiers` to merge resume evidence without changing candidate/decision/error merging, fallback eligibility, or `preferredRetryAt`.

Then run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit workflow resume semantics**

```bash
git add internal/workflow
git commit -m "feat: resume unfinished provider searches"
```

---

### Task 4: Carry resume state through worker, manual search, and assembly

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/app/fallback_test.go`
- Modify: `test/e2e/e2e_test.go`

**Interfaces:**
- Consume lease resume fields in the daemon request.
- Persist result resume fields through `SearchCompletion` for throttled and retryable technical outcomes.
- Call `ClearSearchResume` before a manual media/language search.

- [ ] **Step 1: Write failing worker round-trip tests**

Give `runSearchLease` a lease with resume providers/signature and assert the workflow request receives them. Return throttled with an updated list and assert `CompleteSearch.PreserveResume=true`; return a technical error with clean-empty progress and assert the failure completion also preserves it. For installed, satisfied, no-result, rejected, upgrade-priority, deleted, and unsupported outcomes, assert completion clears resume state.

- [ ] **Step 2: Run worker tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker -run 'TestWorkerPassesProviderResume|TestWorkerPersistsProviderResume|TestWorkerClearsProviderResume' -count=1
```

Expected: FAIL because worker request/completion plumbing is absent.

- [ ] **Step 3: Implement worker plumbing and bounded logging**

Copy lease slices into `workflow.Request`. Copy result slices into throttle/failure completions only when nonempty with a nonempty signature. Add `resume_provider_count` to `job.leased` and `job.completed`; never log provider lists. Preserve existing completion schedules, priorities, counters, and errors.

- [ ] **Step 4: Write failing manual and assembly tests**

Persist resume state, invoke `App.Search`, and assert it clears state before the full provider route runs even when the workflow later fails. Verify each assembled per-language service computes distinct stable signatures from that language's preferred/fallback route.

- [ ] **Step 5: Run app tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/app -run 'TestManualSearchClearsProviderResume|TestLanguageWorkflowRouteSignatures' -count=1
```

Expected: FAIL because manual search does not clear resume state and assembly has no route identity coverage.

- [ ] **Step 6: Implement manual reset and assembly validation**

Call `Repository.ClearSearchResume` after resolving/upserting the media and before `service.Run`. Keep provider-wide `retry --provider` behavior unchanged. Confirm each service derives its route signature from its own canonical language and configured preferred/fallback order; do not use a global provider list.

- [ ] **Step 7: Add restart-focused E2E coverage**

Create a SQLite-backed E2E flow: first daemon run records two clean-empty providers and one cooldown; close/reopen the application; advance the fake clock to reset; second run proves only the unavailable provider is contacted. Then let it return no result and assert normal missing backoff with empty persisted resume state. Use local fake providers only.

- [ ] **Step 8: Run affected packages with race detection**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker ./internal/app ./internal/store ./internal/provider/... ./internal/workflow -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -run TestProviderResumeSurvivesRestart -race -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit integration behavior**

```bash
git add internal/worker internal/app test/e2e
git commit -m "feat: integrate durable provider resume"
```

---

### Task 5: Document, review, and verify the complete change

**Files:**
- Modify: `AGENTS.md`
- Modify: `docs/providers.md`
- Modify: `docs/operations.md`
- Modify: `docs/architecture.md`
- Modify: `docs/implementation-status.md`
- Modify: `docs/superpowers/specs/2026-09-13-provider-specific-cooldown-resume-design.md`

**Interfaces:**
- No new runtime interface; this task makes the shipped contract and operational rollout explicit.

- [ ] **Step 1: Update authoritative documentation**

Document provider-specific restart-safe resume, exact/broad completion requirements, route invalidation, migration 012's one-time throttled-row reschedule, manual/full-route behavior, count-only logs, and the fact that ordinary provider cache TTL remains six hours. Mark the design implemented only after runtime tests pass. Record commit SHAs and RED/GREEN evidence in the implementation ledger.

- [ ] **Step 2: Run the complete verification gate**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
gofmt -l cmd internal test
git diff --check
```

Expected: every command exits zero and `gofmt -l` prints nothing.

- [ ] **Step 3: Request independent review**

Ask a fresh reviewer to inspect the complete diff from `58b511c`, focusing on provider suppression safety, restart/config/media invalidation, exact/broad and preferred/fallback accounting, technical error precedence, migration scope, lease-owner atomicity, log privacy, and permanent regression coverage. Resolve every actionable finding through a new RED/GREEN cycle and repeat affected race tests.

- [ ] **Step 4: Commit the verified documentation boundary**

```bash
git add AGENTS.md docs
git commit -m "docs: document provider-specific cooldown resume"
```

- [ ] **Step 5: Publication handoff**

Report the exact commit chain, verification evidence, migration behavior, and that the production container remains stopped. Do not push, deploy, start the container, or mutate production unless the operator explicitly authorizes those actions after reviewing the completed implementation.
