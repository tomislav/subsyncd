# Codebase Correctness Repairs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Repair the eight remaining conventional-review correctness gaps without increasing provider download waste or weakening fail-closed installation behavior.

**Architecture:** The provider coordinator will honor one explicit search mode per call, while the workflow owns exact-first fallback and the existing broad LAPSE tournament. Archive parsing and selection remain evidence-driven; the installer commits provenance and notification intents in one SQLite transaction; configuration, Silo mapping, deleted-sidecar recovery, and response-lifetime permits are tightened within their existing packages.

**Tech Stack:** Go 1.27.1, SQLite, `gopkg.in/yaml.v3`, `net/http`, existing provider adapters, existing LAPSE v2.0.5 integration, race-enabled Go tests, tagged end-to-end and provider-contract tests, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-09-05-codebase-correctness-repairs-design.md`

## Global Constraints

- Read the spec, `AGENTS.md`, `docs/implementation-status.md`, `docs/providers.md`, `docs/operations.md`, and `docs/references/silo.md` before implementation.
- Use strict red-green-refactor TDD for every behavior: one focused failing test, observe the expected failure, make the smallest production change, and rerun the affected package with `-race -count=1`.
- Preserve configured provider order, lazy downloads, duplicate collapse by `(provider_id, result_id)`, the broad-search top-three limit, current scoring, and LAPSE policy.
- Exact candidates are tried sequentially and are not capped at three; broad search starts only after exact candidates are exhausted.
- Keep deterministic candidate rejection separate from technical errors and provider cooldowns.
- Never log or persist credentials, provider URLs, download references, response bodies, absolute media paths, or raw LAPSE output.
- Filesystem publication plus installation/outbox persistence is one logical commit. Failure to persist the outbox rolls back the file; remote delivery failure after commit does not.
- Do not add a schema migration or configuration key.
- Tests must use sanitized fixtures, local fake servers, and temporary media only. Do not contact Arr applications, providers, Silo, or production.
- Do not build, publish, deploy, start, stop, or otherwise mutate production during this plan.
- Update `docs/implementation-status.md` with the commit, behavior, tests, and next task after every task boundary.

## File structure

- `internal/provider/coordinator.go`: execute exactly the requested search mode and preserve provider-order results.
- `internal/provider/coordinator_test.go`: exact/broad mode, ordering, cache, and error-isolation regressions.
- `internal/workflow/service.go`: top-level inventory, installation-state, phase sequencing, result precedence, and installation logging.
- `internal/workflow/acquisition.go`: new exact-candidate loop and reusable phase bookkeeping, keeping `service.go` from growing further.
- `internal/workflow/service_test.go`: exact fallback, lazy download, deleted-sidecar, forced-member, and outcome regressions.
- `internal/workflow/install.go`: filesystem transaction orchestration and durable notification intent creation.
- `internal/workflow/notification.go`: new shared notification payload and deterministic request builder used by installation and delivery.
- `internal/workflow/install_test.go`: filesystem rollback and outbox request tests.
- `internal/pack/select.go`: explicit episode-range parsing and a single-member selector that enforces forced policy.
- `internal/pack/select_test.go`: accepted/rejected range forms and forced-only selection tests.
- `internal/store/repository.go`: atomic installation/audit/outbox SQLite operation.
- `internal/store/repository_test.go`: transaction rollback, dedupe, and persistence tests.
- `internal/worker/worker.go`: delivery only; remove post-install enqueue ownership.
- `internal/worker/worker_test.go`: prove installed jobs complete without enqueueing and durable notification delivery remains independent.
- `internal/app/app.go`: assemble notifiers before workflows and inject notifier names/clock/events into the installer.
- `internal/app/app_test.go`: prove configured notifier names reach every language workflow installer.
- `internal/config/config.go`: validate YAML document cardinality before environment expansion.
- `internal/config/config_test.go`: multi-document and trailing-separator regressions.
- `internal/notifier/silo.go`: normalize and rewrite root mappings safely.
- `internal/notifier/silo_test.go`: root endpoint, longest-prefix, boundary, and unsafe mapping tests.
- `internal/provider/transport.go`: response-body wrapper that owns concurrency permit release.
- `internal/provider/transport_test.go`: EOF, early close, error, provider-instance, and shared-origin permit lifetime tests.
- `test/e2e/workflow_test.go` or the existing matching tagged E2E file: manual/daemon outbox and deleted-sidecar recovery acceptance coverage.
- `README.md`, `docs/providers.md`, `docs/operations.md`, `AGENTS.md`, `docs/implementation-status.md`: final behavior and handoff documentation.

---

### Task 1: Make provider search mode explicit

**Files:**
- Modify: `internal/provider/coordinator.go`
- Modify: `internal/provider/coordinator_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `Search(context.Context, SearchQuery) SearchResult`, `SearchQuery.Mode`, `Provider.Capabilities()`, and `Provider.SupportsLanguage()`.
- Produces: the same `Coordinator.Search` signature, with the contract that one call executes only `query.Mode`; `SearchExactHash` is sequential and returns all genuine exact candidates, while `SearchBroad` remains concurrent and provider-ordered.

- [ ] **Step 1: Replace the first-exact-result regression with explicit-mode failures**

In `internal/provider/coordinator_test.go`, replace `TestCoordinatorRunsExactProvidersSequentiallyAndStopsOnExactResult` and `TestCoordinatorClearsExactPhaseFailureAfterSuccessfulBroadSearch` with focused tests containing these assertions:

```go
func TestCoordinatorExactModeReturnsEveryExactCandidateInProviderOrder(t *testing.T) {
	first := &fakeProvider{id: "first", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchExactHash: {
			{ProviderID: "first", ResultID: "not-exact"},
			{ProviderID: "first", ResultID: "exact-a", ExactHash: true},
		},
	}}
	second := &fakeProvider{id: "second", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchExactHash: {{ProviderID: "second", ResultID: "exact-b", ExactHash: true}},
	}}
	result := newTestCoordinator(first, second).Search(context.Background(), SearchQuery{
		Media: testQueryMedia(), Language: "en", Mode: SearchExactHash,
	})
	if got := candidateIDs(result.Candidates); !slices.Equal(got, []string{"exact-a", "exact-b"}) {
		t.Fatalf("exact candidates = %#v", got)
	}
	if !slices.Equal(first.calls, []SearchMode{SearchExactHash}) || !slices.Equal(second.calls, []SearchMode{SearchExactHash}) {
		t.Fatalf("calls = %#v / %#v", first.calls, second.calls)
	}
}

func TestCoordinatorBroadModeNeverCallsExactSearch(t *testing.T) {
	item := &fakeProvider{id: "only", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchBroad: {{ProviderID: "only", ResultID: "broad"}},
	}}
	result := newTestCoordinator(item).Search(context.Background(), SearchQuery{
		Media: testQueryMedia(), Language: "en", Mode: SearchBroad,
	})
	if len(result.Candidates) != 1 || !slices.Equal(item.calls, []SearchMode{SearchBroad}) {
		t.Fatalf("result/calls = %#v / %#v", result, item.calls)
	}
}
```

Add this test helper at file scope:

```go
func candidateIDs(candidates []domain.Candidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ResultID)
	}
	return ids
}
```

- [ ] **Step 2: Run the focused tests and observe RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -race -count=1 -run 'TestCoordinator(ExactModeReturnsEveryExactCandidateInProviderOrder|BroadModeNeverCallsExactSearch)'
```

Expected: FAIL because the coordinator ignores `query.Mode`, stops at `exact-a`, or invokes an exact phase before the requested broad phase.

- [ ] **Step 3: Implement one-mode coordinator dispatch**

In `internal/provider/coordinator.go`, make `Search` reject an unknown mode, dispatch exact mode to a sequential helper, and broad mode to the current concurrent helper. The exact helper must retain only candidates whose `ExactHash` field is true:

```go
func (c *Coordinator) Search(ctx context.Context, query SearchQuery) SearchResult {
	switch query.Mode {
	case SearchExactHash:
		return c.searchExact(ctx, query)
	case SearchBroad:
		return c.searchBroad(ctx, query)
	default:
		return SearchResult{Errors: map[string]error{"coordinator": fmt.Errorf("unsupported search mode %q", query.Mode)}}
	}
}

func (c *Coordinator) searchExact(ctx context.Context, query SearchQuery) SearchResult {
	result := SearchResult{Errors: make(map[string]error)}
	for _, item := range c.Providers {
		if !item.Capabilities().ExactFileHash || !item.SupportsLanguage(query.Language) {
			continue
		}
		candidates, err := c.searchProvider(ctx, item, query)
		if err != nil {
			result.Errors[item.ID()] = err
			continue
		}
		for _, candidate := range candidates {
			if candidate.ExactHash {
				result.Candidates = append(result.Candidates, candidate)
			}
		}
	}
	return result
}
```

Move the existing concurrent broad body into `searchBroad` without changing its ordering, cache, or error behavior. Do not have either helper rewrite `query.Mode`.

- [ ] **Step 4: Run the complete provider package**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... -race -count=1
```

Expected: PASS, including cache privacy, provider ordering, cooldown, and logging tests.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md` with the explicit-mode contract and test command, then run `git diff --check` and commit:

```bash
git add internal/provider/coordinator.go internal/provider/coordinator_test.go docs/implementation-status.md
git commit -m "fix: honor explicit provider search modes"
```

---

### Task 2: Add exact-first workflow fallback

**Files:**
- Create: `internal/workflow/acquisition.go`
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `internal/workflow/logging_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: Task 1's explicit `Searcher.Search(ctx, SearchQuery{Mode: SearchExactHash|SearchBroad})` behavior, existing `downloadAndSelect`, `analyzeCandidate`, `finalizeCandidate`, `install`, rejection persistence, and result scheduling helpers.
- Produces: `func (s *Service) tryExactCandidates(context.Context, Request, string, store.Installation, bool, provider.SearchResult, *Result, *[]error) (bool, error)`, where the boolean is true only when the returned `Result` is terminal; broad tournament logic remains in `Service.Run`.

- [ ] **Step 1: Make the workflow fake return phase-specific results**

In `internal/workflow/service_test.go`, expand `fakeSearcher` without breaking tests that still set `result`:

```go
type fakeSearcher struct {
	result  provider.SearchResult
	results map[provider.SearchMode]provider.SearchResult
	calls   int
	queries []provider.SearchQuery
}

func (f *fakeSearcher) Search(_ context.Context, query provider.SearchQuery) provider.SearchResult {
	f.calls++
	f.queries = append(f.queries, query)
	result, found := f.results[query.Mode]
	if !found {
		result = f.result
	}
	if result.Errors == nil {
		result.Errors = map[string]error{}
	}
	return result
}
```

Update existing single-phase assertions to inspect `queries` where needed; their fake `result` may be returned for both phases, so tests expecting one search must give `results` explicitly with an empty exact result and the former result under `SearchBroad`.

- [ ] **Step 2: Add RED exact fallback and lazy-download tests**

Add table-driven coverage in `internal/workflow/service_test.go` for a first exact candidate that is forced, already rejected, malformed, or a wrong episode member. Include one direct lazy-download test:

```go
func TestServiceTriesExactCandidatesSequentiallyAndSkipsBroadAfterSuccess(t *testing.T) {
	bad := exactCandidate("bad")
	bad.Forced = true
	good := exactCandidate("good")
	unused := exactCandidate("unused")
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{bad, good, unused}},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("broad")}},
	}}
	adapter := &fakeProvider{id: "provider"}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": adapter}

	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "good" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(adapter.downloaded, []string{"good"}) {
		t.Fatalf("downloads = %#v", adapter.downloaded)
	}
	if len(searcher.queries) != 1 || searcher.queries[0].Mode != provider.SearchExactHash {
		t.Fatalf("queries = %#v", searcher.queries)
	}
}

func TestServiceFallsBackToBroadAfterExactCandidateFailure(t *testing.T) {
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{exactCandidate("broken")}},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("broad")}},
	}}
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"broken": []byte("not subtitles")}}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": adapter}

	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "broad" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if got := []provider.SearchMode{searcher.queries[0].Mode, searcher.queries[1].Mode}; !slices.Equal(got, []provider.SearchMode{provider.SearchExactHash, provider.SearchBroad}) {
		t.Fatalf("search modes = %#v", got)
	}
}
```

Add separate assertions that an exact-phase technical error followed by a successful broad phase does not remain in `Result.ProviderErrors`, deterministic exact failures create candidate rejection records, technical failures do not, and a fully unavailable broad phase retains existing throttled/technical precedence.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1 -run 'TestService(TriesExactCandidatesSequentiallyAndSkipsBroadAfterSuccess|FallsBackToBroadAfterExactCandidateFailure|Exact)'
```

Expected: FAIL because `Service.Run` sends no explicit mode and its current exact collapse prevents later candidates or broad fallback.

- [ ] **Step 3: Implement the exact phase and preserve the broad tournament**

Create `internal/workflow/acquisition.go` with the exact-candidate loop. It must score and persist every exact result before filtering, consult deterministic rejection state before download, call `downloadAndSelect`, then use the existing analysis/finalization/install pipeline. Its skeleton and terminal contract are:

```go
func (s *Service) tryExactCandidates(
	ctx context.Context,
	request Request,
	workspace string,
	existing store.Installation,
	installed bool,
	search provider.SearchResult,
	result *Result,
	candidateFailures *[]error,
) (bool, error) {
	priorities := s.priorities()
	records := make([]store.CandidateRecord, 0, len(search.Candidates))
	for _, candidate := range search.Candidates {
		if !candidate.ExactHash {
			continue
		}
		score := s.evaluate(request.Media, candidate, request.Language)
		s.logCandidateEvaluation(ctx, request, candidate, score)
		record, err := candidateRecord(candidate, score, match.Eligible(score, s.minimumScore()))
		if err != nil {
			return false, err
		}
		records = append(records, record)
	}
	if err := s.Repository.RecordCandidates(ctx, request.MediaID, request.Language, records); err != nil {
		return false, err
	}
	for index, candidate := range search.Candidates {
		if !candidate.ExactHash {
			continue
		}
		score := s.evaluate(request.Media, candidate, request.Language)
		if !match.Eligible(score, s.minimumScore()) {
			continue
		}
		rejection, rejected, err := s.candidateRejection(ctx, request, candidate, "")
		if err != nil {
			return false, err
		}
		if rejected {
			result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: rejection.ReasonCode})
			continue
		}
		path, decisions, err := s.downloadAndSelect(ctx, request, candidate, workspace, index)
		result.Decisions = append(result.Decisions, decisions...)
		if err != nil {
			if handleErr := s.handleCandidateFailure(ctx, request, candidate, path, err, candidateFailures); handleErr != nil {
				return false, handleErr
			}
			continue
		}
		priority, found := priorities[candidate.ProviderID]
		if !found {
			priority = len(s.ProviderOrder)
		}
		analyzed, err := s.analyzeCandidate(ctx, request, downloadedCandidate{candidate: candidate, score: score, priority: priority, path: path}, installed, existing)
		if err != nil {
			if handleErr := s.handleCandidateFailure(ctx, request, candidate, path, err, candidateFailures); handleErr != nil {
				return false, handleErr
			}
			continue
		}
		finalized, err := s.finalizeCandidate(ctx, request, analyzed, workspace, index)
		if err != nil {
			if handleErr := s.handleCandidateFailure(ctx, request, candidate, path, err, candidateFailures); handleErr != nil {
				return false, handleErr
			}
			continue
		}
		*result, err = s.install(ctx, request, finalized, existing, installed, *result)
		return err == nil, err
	}
	return false, nil
}
```

During implementation, retain the existing same-installed-candidate reassessment path before downloading. Do not persist the same exact result twice when broad search later returns it; merge phase candidate records by stable identity before the second `RecordCandidates` call.

In `Service.Run`, call exact mode first:

```go
exact := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language, Mode: provider.SearchExactHash})
installedResult, err := s.tryExactCandidates(ctx, request, workspace, existing, installed, exact, &result, &candidateFailures)
if err != nil || installedResult {
	return result, err
}
broad := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language, Mode: provider.SearchBroad})
result.ProviderErrors = broad.Errors
```

Feed `broad.Candidates` into the unchanged ranking/top-three/tier tournament. Add a `mergeCandidateRecords(existing, additional []store.CandidateRecord) []store.CandidateRecord` helper keyed by provider/result identity, and make the broad persistence call write the union of exact and broad records so the repository's replace operation does not erase exact-phase evidence. Set `candidateCount` to the size of that unique union. Once broad runs, use broad errors as the final provider error set so a successful later call clears an earlier error from the same provider. Preserve exact-only candidate failures in `candidateFailures` for final deterministic/technical classification.

- [ ] **Step 4: Run workflow, app, and E2E regressions**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow ./internal/app -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: PASS. Verify debug logs contain bounded `search_mode`/phase evidence and info logs still exclude release names, download references, and absolute paths.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/workflow/acquisition.go internal/workflow/service.go internal/workflow/service_test.go internal/workflow/logging_test.go docs/implementation-status.md
git commit -m "fix: exhaust exact candidates before broad search"
```

---

### Task 3: Tighten archive range and forced-member selection

**Files:**
- Modify: `internal/pack/select.go`
- Modify: `internal/pack/select_test.go`
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `pack.Select`, `pack.SelectSingleEpisode`, `Manifest`, `Member`, and workflow `downloadAndSelect`.
- Produces: `func SelectSingleMovie(Manifest, domain.Candidate, bool) (Member, error)` and conservative internal `episodeRange(name string) (season, from, to int, found bool)` behavior.

- [ ] **Step 1: Add failing explicit-range tests**

Add to `internal/pack/select_test.go`:

```go
func TestEpisodeRangeRequiresExplicitHyphenatedEndpoint(t *testing.T) {
	accepted := map[string][3]int{
		"Show.S01E01-E03.srt":    {1, 1, 3},
		"Show.S01E01-S01E03.srt": {1, 1, 3},
		"Show.1x01-1x03.srt":      {1, 1, 3},
	}
	for name, want := range accepted {
		season, from, to, found := episodeRange(name)
		if !found || [3]int{season, from, to} != want {
			t.Errorf("episodeRange(%q) = %d,%d,%d,%v", name, season, from, to, found)
		}
	}
	for _, name := range []string{
		"Show.S01E01.1080p.srt",
		"Show.S01E01.2024.srt",
		"Show.S01E01 E03.srt",
		"Show.S01E03-E01.srt",
		"Show.S01E01-S02E03.srt",
	} {
		if _, _, _, found := episodeRange(name); found {
			t.Errorf("episodeRange(%q) unexpectedly matched", name)
		}
	}
}
```

Also prove `memberEvidence("Show.S01E01.1080p.srt", "Show")` has `EpisodeFrom == 1`, `EpisodeTo == 1`, and no `episode_range` selection.

- [ ] **Step 2: Add a failing forced-only movie-member test**

Add to `internal/pack/select_test.go`:

```go
func TestSelectSingleMovieEnforcesForcedPolicy(t *testing.T) {
	manifest := Manifest{ArchiveType: "plain", Members: []Member{{SafeName: "Movie.forced.srt", Forced: true}}}
	_, err := SelectSingleMovie(manifest, domain.Candidate{Forced: true}, false)
	var selection *SelectionError
	if !errors.As(err, &selection) || selection.Rule != "forced_policy" {
		t.Fatalf("error = %T %v", err, err)
	}

	selected, err := SelectSingleMovie(Manifest{ArchiveType: "plain", Members: []Member{{SafeName: "Movie.srt"}}}, domain.Candidate{}, false)
	if err != nil || selected.SelectionRule != "single_movie" {
		t.Fatalf("selection = %#v, %v", selected, err)
	}
}
```

Add a workflow regression whose first exact download is a plain `Movie.forced.srt` payload and whose second exact candidate installs; assert both candidate order and the first `pack.SelectionError` rejection.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/pack ./internal/workflow -race -count=1 -run '(EpisodeRange|SelectSingleMovie|ForcedOnlyMovie)'
```

Expected: FAIL because the old regex reads `1080` as the range endpoint and the movie shortcut bypasses member policy.

- [ ] **Step 3: Implement explicit range parsing and one movie selector**

Replace `episodeRangePattern` with separate explicit forms that capture both seasons when supplied:

```go
var episodeRangePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)s(\d{1,3})e(\d{1,4})-e(\d{1,4})`),
	regexp.MustCompile(`(?i)s(\d{1,3})e(\d{1,4})-s(\d{1,3})e(\d{1,4})`),
	regexp.MustCompile(`(?i)(\d{1,3})x(\d{1,4})-(\d{1,3})x(\d{1,4})`),
}
```

Implement `episodeRange` so it accepts only positive, same-season, non-reversed bounds. Update `filenameTitle` to remove each accepted range pattern before removing a single episode token.

Add this selector to `internal/pack/select.go`:

```go
func SelectSingleMovie(manifest Manifest, candidate domain.Candidate, wantForced bool) (Member, error) {
	members := eligibleMembers(manifest.Members, wantForced)
	if candidate.Forced && !wantForced || len(members) == 0 {
		return Member{}, selectionError(manifest, "forced_policy", 0, "no members satisfy the forced-subtitle policy")
	}
	if len(members) != 1 || len(manifest.Members) != 1 {
		return Member{}, selectionError(manifest, "single_movie", len(members), "movie candidate must contain exactly one eligible subtitle member")
	}
	selected := members[0]
	selected.SelectionRule = "single_movie"
	selected.SelectionEvidence = "single movie subtitle member satisfies subtitle policy"
	return selected, nil
}
```

In `workflow.downloadAndSelect`, replace direct `manifest.Members[0]` assignment with `pack.SelectSingleMovie(manifest, extractionCandidate, false)`. Preserve the existing fail-closed multi-member movie behavior.

- [ ] **Step 4: Run archive and workflow packages**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/pack ./internal/workflow -race -count=1
```

Expected: PASS, including wrong-episode singleton, season-pack, cache, forced-only, and bounded archive diagnostics tests.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/pack/select.go internal/pack/select_test.go internal/workflow/service.go internal/workflow/service_test.go docs/implementation-status.md
git commit -m "fix: tighten subtitle member evidence"
```

---

### Task 4: Reacquire deleted managed sidecars

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `internal/workflow/upgrade_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: live `inventory.Inventory`, stored `store.Installation`, and `InstallationMatchesMedia`.
- Produces: `func managedInstallationPresent(inventory.Inventory, store.Installation) bool`; `Service.Run` uses its result as the `installed` flag for satisfaction, reassessment, and upgrade policy.

- [ ] **Step 1: Add RED missing-sidecar recovery coverage**

Add to `internal/workflow/service_test.go`:

```go
func TestServiceReacquiresDeletedManagedExactSubtitle(t *testing.T) {
	request := serviceRequest(t)
	candidate := exactCandidate("same")
	existing := matchingInstallation(request, candidate, []byte(`{"total":100,"contributions":[{"signal":"exact_hash","points":100}]}`))
	existing.Checksum = "old-checksum"
	repository := &workflowRepository{installation: existing, found: true}
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{candidate}},
	}}
	installer := &fakeInstaller{}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, installer)
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || installer.calls != 1 {
		t.Fatalf("Run() = %#v, %v; installs=%d", result, err, installer.calls)
	}
}
```

Add a control with a present track at `existing.Path` but a different checksum and `Protected: true`; it must return `OutcomeSatisfied`, make no provider call, and never invoke the installer.

- [ ] **Step 2: Run the focused test and observe RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1 -run 'TestService(ReacquiresDeletedManagedExactSubtitle|ProtectsModifiedPresentSidecar)'
```

Expected: FAIL because the stale exact installation row currently returns `OutcomeSatisfied` even with no sidecar track.

- [ ] **Step 3: Make live inventory gate stored provenance**

Add the helper next to `inventoryStopsSearch`:

```go
func managedInstallationPresent(current inventory.Inventory, installation store.Installation) bool {
	for _, track := range current.Tracks {
		if track.Embedded || track.Path == "" {
			continue
		}
		if filepath.Clean(track.Path) == filepath.Clean(installation.Path) && track.Checksum == installation.Checksum {
			return true
		}
	}
	return false
}
```

Immediately after `GetInstallation`, compute:

```go
activeInstallation := installed && managedInstallationPresent(current, existing)
```

Pass `activeInstallation`—not raw row existence—to `inventoryStopsSearch`, exact-terminal checks, same-candidate reassessment, `ShouldUpgradeForMedia`, candidate analysis, and `s.install`'s replacement flag. Retain `existing` so the installer can protect a present mismatched sidecar and overwrite stale provenance only after a successful new-file commit.

- [ ] **Step 4: Run workflow and E2E tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: PASS. Confirm a missing sidecar follows first-install semantics and a modified present sidecar remains protected.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/workflow/service.go internal/workflow/service_test.go internal/workflow/upgrade_test.go docs/implementation-status.md
git commit -m "fix: reacquire deleted managed subtitles"
```

---

### Task 5: Commit installation and notification intents atomically

**Files:**
- Create: `internal/workflow/notification.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/workflow/install.go`
- Modify: `internal/workflow/install_test.go`
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: tagged E2E notification test under `test/e2e`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces in `store`: `type NotificationEnqueueResult struct { Notifier, DedupeKey string; Inserted bool }` and `RecordInstallationWithNotifications(context.Context, Installation, []NotificationRequest) ([]NotificationEnqueueResult, error)`; existing `RecordInstallation` delegates with no notifications.
- Produces in `workflow`: exported `NotificationPayload`, internal `notificationRequests`, and installer fields `NotifierNames []string`, `Now func() time.Time`, `Events *observability.Emitter`.
- Changes worker ownership: `worker.Repository` no longer requires `EnqueueNotification`; the worker only leases and delivers already-durable rows.

- [ ] **Step 1: Add RED repository transaction tests**

In `internal/store/repository_test.go`, add a successful atomic persistence test using a real temporary SQLite store. After recording, query `GetInstallation` and `LeaseDueNotifications`, then verify both notifier rows and dedupe behavior.

Add an injected failure without a new production hook by passing one invalid request after one valid request:

```go
func TestRecordInstallationWithNotificationsRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`CREATE TRIGGER fail_notification BEFORE INSERT ON notifications BEGIN SELECT RAISE(ABORT, 'fail'); END`); err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/movie.en.srt", Checksum: "sum"}
	requests := []NotificationRequest{{
		Notifier: "silo", DedupeKey: "valid", PayloadJSON: []byte(`{"subtitle_path":"/media/movie.en.srt"}`), NextAttemptAt: time.Now().UTC(),
	}}
	if _, err := repo.RecordInstallationWithNotifications(ctx, installation, requests); err == nil {
		t.Fatal("RecordInstallationWithNotifications() error = nil")
	}
	if _, found, err := repo.GetInstallation(ctx, mediaID, "en"); err != nil || found {
		t.Fatalf("installation found/error = %v/%v", found, err)
	}
	if leases, err := repo.LeaseDueNotifications(ctx, time.Now().Add(time.Hour), 10, time.Minute); err != nil || len(leases) != 0 {
		t.Fatalf("notifications = %#v, %v", leases, err)
	}
}
```

Use existing repository test helpers rather than introducing duplicate setup names if the exact helpers differ.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -race -count=1 -run 'TestRecordInstallationWithNotifications'
```

Expected: FAIL because the atomic repository method does not exist.

- [ ] **Step 2: Implement the atomic repository operation**

Add reusable validation and a transaction-local enqueue helper:

```go
type NotificationEnqueueResult struct {
	Notifier  string
	DedupeKey string
	Inserted  bool
}

func validateNotificationRequest(request NotificationRequest) error {
	if request.Notifier == "" || request.DedupeKey == "" || !json.Valid(request.PayloadJSON) || request.NextAttemptAt.IsZero() {
		return fmt.Errorf("notification notifier, dedupe key, valid payload, and due time are required")
	}
	return nil
}
```

Implement `RecordInstallationWithNotifications` by moving the complete support check, audit insert, default JSON handling, installation upsert, and commit from `RecordInstallation` into one transaction. Validate all requests before the first SQL mutation. Insert each notification through the transaction with the existing `ON CONFLICT DO NOTHING`, record `RowsAffected()`, and return results only after commit.

Keep compatibility:

```go
func (r *Repository) RecordInstallation(ctx context.Context, installation Installation) error {
	_, err := r.RecordInstallationWithNotifications(ctx, installation, nil)
	return err
}
```

Refactor `EnqueueNotification` to use the same validation and SQL semantics outside an installation transaction; do not change notification leasing or delivery.

- [ ] **Step 3: Add RED installer rollback and deterministic-request tests**

Create `internal/workflow/notification.go` with the shared payload type declaration first so tests compile:

```go
type NotificationPayload struct {
	Media        domain.Media `json:"media"`
	SubtitlePath string       `json:"subtitle_path"`
}
```

Change the install test fake to implement the new atomic method and capture requests. Add tests asserting:

```go
func TestInstallerRollsBackPublishedFileWhenNotificationIntentCommitFails(t *testing.T) {
	repository := &installationRepository{recordErr: errors.New("outbox unavailable")}
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(root, "source.srt"), installSRT)
	destination := filepath.Join(root, "Movie.en.srt")
	installer := Installer{Repository: repository, MediaRoots: []string{root}, NotifierNames: []string{"silo"}, Now: func() time.Time { return time.Unix(100, 0).UTC() }}
	_, err := installer.Install(context.Background(), installRequest(source, destination))
	if err == nil || !strings.Contains(err.Error(), "outbox unavailable") {
		t.Fatalf("Install() error = %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination stat error = %v", statErr)
	}
}
```

Also assert the generated JSON decodes to the exact `NotificationPayload`, notifier names are sorted, each due time equals `Now()`, and the dedupe key is stable for notifier/media/language/checksum but changes when the checksum changes.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1 -run 'TestInstaller|TestNotification'
```

Expected: FAIL because the installer still calls installation-only persistence.

- [ ] **Step 4: Move outbox ownership into the installer and delivery payload into workflow**

In `internal/workflow/notification.go`, implement request construction with the existing dedupe formula:

```go
func notificationRequests(media domain.Media, installation store.Installation, notifierNames []string, now time.Time) ([]store.NotificationRequest, error) {
	payload, err := json.Marshal(NotificationPayload{Media: media, SubtitlePath: installation.Path})
	if err != nil {
		return nil, fmt.Errorf("encode notification payload: %w", err)
	}
	names := append([]string(nil), notifierNames...)
	sort.Strings(names)
	requests := make([]store.NotificationRequest, 0, len(names))
	for _, name := range names {
		raw := fmt.Sprintf("%s\x00%d\x00%s\x00%s", name, installation.MediaID, installation.Language, installation.Checksum)
		sum := sha256.Sum256([]byte(raw))
		requests = append(requests, store.NotificationRequest{
			Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: now,
		})
	}
	return requests, nil
}
```

Change `InstallationStore` to require `RecordInstallationWithNotifications`. In `Installer.Install`, build requests only after the final `Installation` value and checksum exist, call the atomic repository method at `StageDatabase`, and log `notification.queued` only for returned results with `Inserted == true`. Inject `Events` and use its privacy-safe worker/workflow conventions.

In `internal/worker/worker.go`:

- remove `enqueueNotifications` and its crypto/sorting imports;
- remove `EnqueueNotification` from the worker repository interface;
- make `OutcomeInstalled` only populate successful search completion;
- decode delivery rows into `workflow.NotificationPayload`.

In `internal/app/app.go`, construct the notifier map before the installer, sort its keys, and assemble:

```go
installer := workflow.Installer{
	Repository: repository,
	MediaRoots: cfg.MediaRoots,
	Mode: cfg.Install.FileMode,
	UID: cfg.Install.UID,
	GID: cfg.Install.GID,
	NotifierNames: notifierNames,
	Now: clock.Now,
	Events: events,
}
```

Update worker tests so an installed workflow result does not call enqueue. Seed a durable notification row before `RunOnce` when testing delivery. Add an app assembly test that enables the fake Silo notifier configuration and verifies the actual installer queues a Silo intent for both manual and daemon installation paths.

- [ ] **Step 5: Run subsystem and tagged acceptance tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store ./internal/workflow ./internal/worker ./internal/app -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: PASS. The tagged test must prove manual and daemon installations each leave a durable deduplicated intent, restart can deliver it, and a retryable Silo delivery failure leaves the installation row and subtitle file intact.

- [ ] **Step 6: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/store/repository.go internal/store/repository_test.go internal/workflow/install.go internal/workflow/install_test.go internal/workflow/notification.go internal/worker/worker.go internal/worker/worker_test.go internal/app/app.go internal/app/app_test.go test/e2e docs/implementation-status.md
git commit -m "fix: commit subtitle notifications atomically"
```

---

### Task 6: Reject trailing YAML documents before expansion

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `Load(path, lookupEnv)` and existing `ensureSingleDocument` behavior.
- Produces: `func decodeSingleDocument([]byte) (yaml.Node, error)`, called before `expandEnv`; strict known-field decoding remains unchanged.

- [ ] **Step 1: Add RED document-cardinality and secret-redaction tests**

Add to `internal/config/config_test.go`:

```go
func TestLoadRejectsSecondYAMLDocumentBeforeEnvironmentExpansion(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n") + "\n---\nsecret: ${SECOND_DOCUMENT_SECRET}\n"
	_, err := loadTextWithLookup(t, text, func(name string) (string, bool) {
		if name == "TEST_SUBDL_KEY" {
			return "provider-secret", true
		}
		if name == "SECOND_DOCUMENT_SECRET" {
			return "must-not-appear", true
		}
		return "", false
	})
	assertErrorContains(t, err, "exactly one", "yaml", "document")
	if strings.Contains(strings.ToLower(err.Error()), "must-not-appear") {
		t.Fatalf("error leaked expanded trailing document: %v", err)
	}
}

func TestLoadAcceptsEmptyTrailingYAMLSeparator(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n") + "\n---\n"
	if _, err := loadText(t, text); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
```

- [ ] **Step 2: Run the focused tests and observe RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/config -race -count=1 -run 'TestLoad(RejectsSecondYAMLDocumentBeforeEnvironmentExpansion|AcceptsEmptyTrailingYAMLSeparator)'
```

Expected: the non-empty second document is silently discarded by `yaml.Unmarshal`, or the empty separator behavior is not classified consistently.

- [ ] **Step 3: Decode the original stream exactly once before expansion**

Implement:

```go
func decodeSingleDocument(data []byte) (yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return yaml.Node{}, fmt.Errorf("parse config: %w", err)
	}
	for {
		var extra yaml.Node
		err := decoder.Decode(&extra)
		if err == io.EOF {
			return document, nil
		}
		if err != nil {
			return yaml.Node{}, fmt.Errorf("decode trailing config document: %w", err)
		}
		if !emptyYAMLDocument(extra) {
			return yaml.Node{}, fmt.Errorf("config must contain exactly one YAML document")
		}
	}
}

func emptyYAMLDocument(node yaml.Node) bool {
	if len(node.Content) == 0 {
		return true
	}
	return node.Kind == yaml.DocumentNode && len(node.Content) == 1 &&
		node.Content[0].Kind == yaml.ScalarNode && node.Content[0].Tag == "!!null" && node.Content[0].Value == ""
}
```

Call it on `data` before `expandEnv`. After expansion/marshal, keep `KnownFields(true)` for the one normalized document and remove the now-redundant post-round-trip `ensureSingleDocument` call. Do not include YAML values in errors.

- [ ] **Step 4: Run all configuration tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/config -race -count=1
```

Expected: PASS, including environment override, secret expansion, unknown field, and example configuration tests.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/config/config.go internal/config/config_test.go docs/implementation-status.md
git commit -m "fix: reject trailing configuration documents"
```

---

### Task 7: Support safe root Silo mappings

**Files:**
- Modify: `internal/notifier/silo.go`
- Modify: `internal/notifier/silo_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `rewritePath(string, []PathMapping) (string, error)` and `NewSilo` mapping validation.
- Produces: `func normalizeMappingPath(string) (string, error)` that preserves `/`; longest-component-prefix mapping remains deterministic.

- [ ] **Step 1: Add RED root and boundary table tests**

Add to `internal/notifier/silo_test.go`:

```go
func TestRewritePathSupportsRootMappingsAndLongestPrefix(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		mappings []PathMapping
		want     string
	}{
		{"remote root", "/media/Movies/A/file.mkv", []PathMapping{{From: "/media", To: "/"}}, "/Movies/A/file.mkv"},
		{"local root", "/Movies/A/file.mkv", []PathMapping{{From: "/", To: "/mnt/media"}}, "/mnt/media/Movies/A/file.mkv"},
		{"exact local root", "/", []PathMapping{{From: "/", To: "/mnt/media"}}, "/mnt/media"},
		{"longest prefix", "/media/tv/Show/file.mkv", []PathMapping{{From: "/media", To: "/library"}, {From: "/media/tv", To: "/shows"}}, "/shows/Show/file.mkv"},
		{"component boundary", "/media2/file.mkv", []PathMapping{{From: "/media", To: "/library"}}, "/media2/file.mkv"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rewritePath(test.path, test.mappings)
			if err != nil || got != test.want {
				t.Fatalf("rewritePath() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
```

Add construction/rewrite rejection cases for relative endpoints and traversal-normalized unsafe input; assert the fake server receives no request.

- [ ] **Step 2: Run notifier tests and observe RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/notifier -race -count=1 -run 'TestRewritePathSupportsRootMappingsAndLongestPrefix|TestSiloRejectsUnsafeMapping'
```

Expected: FAIL because trimming `/` produces an empty endpoint and skips the mapping.

- [ ] **Step 3: Normalize roots without losing component boundaries**

Implement:

```go
func normalizeMappingPath(value string) (string, error) {
	raw := filepath.ToSlash(strings.ReplaceAll(value, `\`, `/`))
	for _, component := range strings.Split(raw, "/") {
		if component == ".." {
			return "", fmt.Errorf("mapping path contains traversal")
		}
	}
	normalized := filepath.ToSlash(filepath.Clean(raw))
	if normalized == "." || !strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("mapping endpoint must be absolute")
	}
	return normalized, nil
}

func pathHasPrefix(path, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
```

Normalize mappings once in `NewSilo` and store the normalized copy. Normalize the target through the same helper so explicit traversal fails before cleaning. In `rewritePath`, choose the longest `From` for which `pathHasPrefix` is true. Compute the suffix without indexing past root, and join with:

```go
suffix := strings.TrimPrefix(normalized, from)
rewritten := to
if suffix != "" {
	rewritten = pathpkg.Join(to, strings.TrimPrefix(suffix, "/"))
}
if !strings.HasPrefix(rewritten, "/") {
	return "", fmt.Errorf("rewritten path is not absolute")
}
```

Alias the standard `path` import as `pathpkg` to ensure Silo namespace paths use slash semantics on every host. Preserve no-match behavior by returning the clean original absolute path.

- [ ] **Step 4: Run all notifier tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/notifier -race -count=1
```

Expected: PASS, including native endpoint, parent-directory payload, redirect refusal, timeout, and credential/body privacy.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/notifier/silo.go internal/notifier/silo_test.go docs/implementation-status.md
git commit -m "fix: preserve root Silo path mappings"
```

---

### Task 8: Hold provider permits through response-body lifetime

**Files:**
- Modify: `internal/provider/transport.go`
- Modify: `internal/provider/transport_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: `Gate.Acquire`'s idempotent release callback and `Client.Do`'s `*http.Response` contract.
- Produces: private `permitBody` implementing `io.ReadCloser`, releasing its callback once on EOF or Close.

- [ ] **Step 1: Add RED provider-instance and shared-origin lifetime tests**

In `internal/provider/transport_test.go`, add a helper body whose reads block until released and whose close is observable. Test one provider with `maxConcurrent=1`, then two providers sharing one origin with `sharedMax=1`:

```go
func TestTransportPermitLivesUntilResponseBodyClose(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("one", 1000, 10, 1)
	calls := make(chan struct{}, 2)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls <- struct{}{}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	client := Client{HTTP: httpClient, Gate: gate, Clock: clock, ProviderID: "one", ProviderType: "subdl"}
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/search", nil)
	first, err := client.Do(context.Background(), OperationSearch, request)
	if err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan *http.Response, 1)
	go func() {
		second, _ := client.Do(context.Background(), OperationSearch, request)
		secondDone <- second
	}()
	select {
	case <-secondDone:
		t.Fatal("second request acquired before first body closed")
	case <-time.After(20 * time.Millisecond):
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-secondDone:
		_ = second.Body.Close()
	case <-time.After(time.Second):
		t.Fatal("second request did not acquire after close")
	}
}
```

Add the shared-origin variant with provider IDs `one` and `two`. Add an EOF test that fully reads without an explicit close before starting the next request, and a repeated-close test that proves no double release/deadlock.

- [ ] **Step 2: Run the lifetime tests and observe RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -race -count=1 -run 'TestTransportPermit'
```

Expected: FAIL because `defer release()` releases immediately after response headers.

- [ ] **Step 3: Transfer release ownership to a body wrapper**

Add:

```go
type permitBody struct {
	body    io.ReadCloser
	release func()
	once    sync.Once
}

func (b *permitBody) Read(payload []byte) (int, error) {
	n, err := b.body.Read(payload)
	if err == io.EOF {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *permitBody) Close() error {
	err := b.body.Close()
	b.once.Do(b.release)
	return err
}
```

Remove `defer release()` from `Client.Do`. Use a local `releaseNow := true` guard deferred immediately after acquisition. Immediately after `c.HTTP.Do` returns a response with a non-nil body—before any HTTP status or cooldown branch—wrap it and set `releaseNow = false`; the deferred guard releases all transport-error and no-body paths. A response returned alongside an HTTP/cooldown error therefore still owns the permit until its caller consumes or closes the body.

Add `io` and `sync` imports. Preserve body read and close errors and every existing cooldown/auth/rate-header decision.

- [ ] **Step 4: Run all provider tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... -race -count=1
```

Expected: PASS. Confirm every existing adapter closes its response body on all returned-response paths; add a focused adapter test if inspection finds one that does not.

- [ ] **Step 5: Record and commit the boundary**

Update `docs/implementation-status.md`, run `git diff --check`, and commit:

```bash
git add internal/provider/transport.go internal/provider/transport_test.go docs/implementation-status.md
git commit -m "fix: retain provider permits through response bodies"
```

---

### Task 9: Document the repaired contracts and run final verification

**Files:**
- Modify: `README.md`
- Modify: `docs/providers.md`
- Modify: `docs/operations.md`
- Modify: `docs/references/silo.md`
- Modify: `AGENTS.md`
- Modify: `docs/superpowers/specs/2026-09-05-codebase-correctness-repairs-design.md`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: all implemented and verified task contracts.
- Produces: authoritative user/operator/agent documentation and a complete verification ledger; no runtime API changes.

- [ ] **Step 1: Update behavior documentation**

Document these exact points:

- `README.md`: exact hashes are tried sequentially before broad search; downloads stop at the first installed candidate; a deleted managed subtitle is reacquired; notification delivery is durable and asynchronous.
- `docs/providers.md`: explicit exact/broad phases, exact candidates uncapped, broad candidates capped at three, candidate-local fallback, explicit range syntax, and response-body permit lifetime.
- `docs/operations.md`: installation plus notification-intent atomicity, remote delivery independence, deleted-sidecar recovery, and single-document configuration rejection.
- `docs/references/silo.md`: `/` is valid on either side of a path mapping, longest component prefix wins, and the mapped media parent directory is scanned.
- `AGENTS.md`: replace the design-only wording for this repair with the final invariants, without duplicating full prose from the reference docs.
- Design spec: set `**Status:** Implemented and locally verified` only after all verification below passes.

Do not introduce comparisons to other projects, future API guesses, credentials, real production paths, or host-specific state.

- [ ] **Step 2: Run focused regression packages**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... ./internal/workflow ./internal/pack ./internal/store ./internal/worker ./internal/config ./internal/notifier ./internal/app -race -count=1
```

Expected: PASS.

- [ ] **Step 3: Run the full repository verification matrix**

Run each command separately and preserve its exit status:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/providercontract -tags=provider_contract -count=1
docker compose -f compose.example.yml config --quiet
git diff --check
```

Expected: every command exits zero. Provider-contract tests may skip when credentials are absent; they must not make live calls without explicitly supplied credentials.

- [ ] **Step 4: Run privacy and repository-state checks**

Run:

```bash
rg -n '(api[_-]?key|token|authorization|bearer)[[:space:]]*[:=][[:space:]]*[^${[:space:]]' README.md docs config.example.yaml compose.example.yml
git status --short
```

Expected: the credential scan finds no literal secret values; `git status` shows only the intentional documentation changes for this task. Review any matches manually rather than suppressing them blindly.

- [ ] **Step 5: Finalize the implementation ledger and commit**

Update `docs/implementation-status.md` with every implementation commit, exact verification commands/results, no-live-network statement, and the next safe action. Then commit:

```bash
git add README.md docs/providers.md docs/operations.md docs/references/silo.md AGENTS.md docs/superpowers/specs/2026-09-05-codebase-correctness-repairs-design.md docs/implementation-status.md
git commit -m "docs: record correctness repair contracts"
```

- [ ] **Step 6: Verify the committed tree without claiming deployment**

Run:

```bash
git status --short --branch
git log -10 --oneline
```

Expected: the worktree is clean and the task commits are present. Report local completion only. Pushing, image publication, and any production deployment require a separate explicit request.
