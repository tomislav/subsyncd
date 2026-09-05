# LAPSE Score-Tier Tournament Implementation Plan

> Current contract: [single-run LAPSE preparation](../specs/2026-09-05-single-run-lapse-design.md) supersedes the acquisition dry-run/second synchronization sequence described below. Score-tier ordering remains; acquisition emits one synchronization event pair per required candidate.


> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop reading large media files for lower-scored candidates that cannot win and synchronize only the selected LAPSE candidate.

**Architecture:** The workflow lazily prepares candidates one metadata-score tier at a time. It analyzes all candidates tied in the current tier, ranks viable results by LAPSE confidence and existing tie-breakers, finalizes candidates in that order, and stops immediately after installation.

**Tech Stack:** Go 1.27, existing workflow/match/syncer/provider/store packages, LAPSE v2.0.5 process contract.

**Spec:** `docs/superpowers/specs/2026-09-04-lapse-score-tier-tournament-design.md`

## Global Constraints

- Preserve all metadata scores, eligibility gates, top-three cap, exact-hash bypass, configured score bypass, upgrade rules, fingerprint guards, and deterministic rejection signatures.
- Metadata score remains primary; LAPSE confidence compares candidates only inside an equal-score tier.
- A unique valid top scorer normally performs exactly one analysis and one synchronization invocation.
- Analyze tied candidates before finalization; synchronize only the best viable candidate unless fallback is required.
- Lower score tiers are lazy and remain untouched after a higher tier installs successfully.
- Technical failures never poison deterministic candidate rejection state.
- Follow strict red-green-refactor TDD for every production behavior change.

---

### Task 1: Prove and implement unique-leader early stopping

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`

**Interfaces:**
- Produces private workflow values `downloadedCandidate`, `analyzedCandidate`, and `finalizedCandidate` as defined by the design.
- Preserves public `Service.Run`, `Synchronizer`, and `Installer` interfaces.

- [ ] **Step 1: Write a failing unique-leader call-count test**

Construct three eligible candidates with distinct scores by giving the request matching release-group and source metadata, then adding only the corresponding release evidence to each candidate. Configure the fake synchronizer to return solid analysis/synchronization. Assert the result installs the highest-score candidate, the provider downloads only that result, and synchronizer call logs contain one analysis plus one synchronization.

```go
func TestRunStopsAfterUniqueHighestScoreInstalls(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "webdl"
	leader := broadCandidate("leader")
	leader.ReleaseNames = []string{"Movie.2024-GROUP"}
	runnerUp := broadCandidate("runner-up")
	runnerUp.ReleaseNames = []string{"Movie.2024.WEB-DL"}
	last := broadCandidate("last")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{last, runnerUp, leader}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "leader" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(providerFake.downloaded, []string{"leader"}) ||
		!slices.Equal(synchronizer.analyzed, []string{"leader"}) ||
		!slices.Equal(synchronizer.synchronized, []string{"leader"}) {
		t.Fatalf("download/analyze/sync = %#v/%#v/%#v", providerFake.downloaded, synchronizer.analyzed, synchronizer.synchronized)
	}
}
```

Extend `fakeSynchronizer` with `analyzed` and `synchronized` result-ID slices so assertions observe real workflow calls rather than implementation counters alone. Extract its verdict/confidence construction into a private helper used by both fake methods; `SynchronizeCandidate` must no longer call `AnalyzeCandidate` internally, because that would falsely append a second analysis event while creating synchronized output.

- [ ] **Step 2: Run the focused test and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -run TestRunStopsAfterUniqueHighestScoreInstalls -count=1
```

Expected: FAIL because the current workflow downloads, analyzes, and synchronizes all three finalists.

- [ ] **Step 3: Introduce phase types and score-tier iteration**

Add private types with complete ownership of phase data:

```go
type downloadedCandidate struct {
	candidate domain.Candidate
	score domain.Score
	priority int
	path string
}

type analyzedCandidate struct {
	downloadedCandidate
	analysis domain.SyncResult
	bypass bool
}

type finalizedCandidate struct {
	candidate domain.Candidate
	score domain.Score
	priority int
	sync domain.SyncResult
	path string
}
```

Partition the already ranked, capped shortlist by equal `Score.Total`. Process one slice at a time. Return immediately after a finalized candidate installs. Do not prepare the next tier in advance.

- [ ] **Step 4: Split candidate validation from LAPSE finalization**

Replace the combined `synchronize` helper with:

```go
func (s *Service) analyzeCandidate(ctx context.Context, request Request, item downloadedCandidate, installed bool, existing store.Installation) (analyzedCandidate, error)
func (s *Service) finalizeCandidate(ctx context.Context, request Request, item analyzedCandidate, workspace string, index int) (finalizedCandidate, error)
```

`analyzeCandidate` applies minimum score and upgrade policy, returns bypass provenance unchanged, or invokes only `AnalyzeCandidate`. `finalizeCandidate` returns bypass paths unchanged or invokes only `SynchronizeCandidate`; it copies analysis confidence into final synchronization provenance.

- [ ] **Step 5: Run the unique-leader test and the workflow package**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -run TestRunStopsAfterUniqueHighestScoreInstalls -count=1
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -race -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the early-stop boundary**

```bash
git add internal/workflow/service.go internal/workflow/service_test.go
git commit -m "feat: stop lapse after a winning score tier"
```

---

### Task 2: Rank equal-score candidates by analysis confidence

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`

**Interfaces:**
- Produces: private `sortAnalyzed([]analyzedCandidate)` using confidence, provider priority, rating, provider ID, and result ID.

- [ ] **Step 1: Write failing tied-tier selection tests**

Create two equal-score candidates in different provider order with analysis confidence `0.76` and `0.91`. Assert both are downloaded/analyzed, only the `0.91` candidate is synchronized, and it installs. Add a deterministic tie case proving provider priority, rating, provider ID, and result ID ordering remains stable.

- [ ] **Step 2: Run tied-tier tests and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -run 'TestRunAnalyzesEqualScoreTierBeforeFinalizing|TestAnalyzedTierOrderingIsDeterministic' -count=1
```

Expected: FAIL because the first green implementation stops after the first candidate in a tier.

- [ ] **Step 3: Implement tied-tier collection and ranking**

For the current tier, collect every bypass or solid analysis. Sort with:

```go
sort.SliceStable(items, func(i, j int) bool {
	if items[i].analysis.Confidence != items[j].analysis.Confidence {
		return items[i].analysis.Confidence > items[j].analysis.Confidence
	}
	if items[i].priority != items[j].priority {
		return items[i].priority < items[j].priority
	}
	if items[i].candidate.Rating != items[j].candidate.Rating {
		return items[i].candidate.Rating > items[j].candidate.Rating
	}
	if items[i].candidate.ProviderID != items[j].candidate.ProviderID {
		return items[i].candidate.ProviderID < items[j].candidate.ProviderID
	}
	return items[i].candidate.ResultID < items[j].candidate.ResultID
})
```

Keep bypass analysis confidence at zero to preserve existing behavior.

- [ ] **Step 4: Run workflow tests with race detection**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -race -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit tied-tier ranking**

```bash
git add internal/workflow/service.go internal/workflow/service_test.go
git commit -m "feat: rank lapse score ties by confidence"
```

---

### Task 3: Preserve fallback and rejection semantics

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`

**Interfaces:**
- Consumes: existing `recordCandidateRejection`, `candidateRejection`, `unavailableErrors`, and media-validation classification.
- Produces: fallback decisions without new persisted rejection categories.

- [ ] **Step 1: Write failing fallback matrix tests**

Add focused tests for:

```text
unique leader analysis non-solid -> next score tier wins
best tied candidate synchronization fails -> second analyzed tie wins without re-analysis
all candidates in top tier fail synchronization -> lower tier wins
transient top-tier error + lower success -> install lower without blacklisting top
all failures transient -> workflow returns error/throttled outcome
context cancellation -> no subsequent candidate starts
```

Assert download, analysis, synchronization, rejection-write, and decision-stage call logs exactly.

- [ ] **Step 2: Run the fallback matrix and verify red**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -run 'TestTournament.*Fallback|TestTournamentStopsOnCancellation' -count=1
```

Expected: FAIL at the first missing fallback or incorrect rejection behavior.

- [ ] **Step 3: Implement tier and finalization fallback**

Within a tier, continue finalization after any failure but call `recordCandidateRejection` only through existing classification. Preserve technical failures in `candidateFailures`. Move to the next score tier only after no candidate in the current tier installs. Before every download, analysis, finalization, and tier transition, return immediately when `ctx.Err()` is non-nil.

Emit decisions with exact stages:

```text
tournament_tier: evaluating score <n>
lapse_analysis: solid|non-solid|error
early_stop: installed from score <n>; lower tiers skipped
lapse_finalize: solid|non-solid|error
fallback: next candidate in tier|next score tier
```

- [ ] **Step 4: Run workflow and syncer tests with race detection**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow ./internal/syncer -race -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit fallback behavior**

```bash
git add internal/workflow/service.go internal/workflow/service_test.go
git commit -m "feat: fall back across lapse score tiers"
```

---

### Task 4: Protect bypass, packs, upgrades, and installation invariants

**Files:**
- Modify: `internal/workflow/service_test.go`
- Modify: `test/e2e/e2e_test.go`

**Interfaces:**
- Preserves existing public behavior only; introduces no production interface.

- [ ] **Step 1: Add regression tests around unchanged paths**

Prove:

- exact hash and score bypass invoke neither LAPSE method;
- an unchanged installed provider/result/fingerprint performs metadata reassessment only;
- a season pack lazily downloads/extracts once when its tier is reached;
- an ineligible upgrade is rejected before LAPSE;
- a stale media fingerprint prevents installation and does not fall through to another candidate;
- identical synchronized checksum remains idempotent;
- temporary workspaces are removed on success, rejection, error, and cancellation.

- [ ] **Step 2: Demonstrate each new regression test catches a production change**

Temporarily revert or disable the relevant tournament branch one test at a time, run that focused test to observe failure, then restore the implementation. Do not commit the temporary reversions.

- [ ] **Step 3: Run workflow and tagged e2e suites**

```bash
GOCACHE=/tmp/subsyncd-gocache go test ./internal/workflow -race -count=1
GOCACHE=/tmp/subsyncd-gocache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit invariant coverage**

```bash
git add internal/workflow/service_test.go test/e2e/e2e_test.go
git commit -m "test: protect lapse tournament invariants"
```

---

### Task 5: Document, verify, publish, and prepare a canary

**Files:**
- Modify: `README.md`
- Modify: `docs/operations.md`
- Modify: `docs/providers.md`
- Modify: `docs/implementation-status.md`
- Modify: `AGENTS.md`
- Modify: `docs/release-notes.md`

**Interfaces:**
- Documents the score-tier tournament, structural early stop, call-count expectations, and fallback behavior.

- [ ] **Step 1: Update durable documentation**

Replace statements that every top-three candidate is prepared/synchronized. Document lazy tiers, equal-score confidence comparison, one-winner synchronization, technical versus deterministic fallback, and the measured pre-change baselines: Example Movie B `12m11s` and Example Movie A `24m55s`.

- [ ] **Step 2: Run the complete verification gate**

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
git diff --check
```

Expected: all commands exit zero.

- [ ] **Step 3: Commit the completed tournament documentation**

```bash
git add README.md docs AGENTS.md
git commit -m "docs: document lapse score-tier tournament"
```

- [ ] **Step 4: Push and observe publication**

Push `main`; wait for GitHub verification and the multi-architecture image publication to finish. Record the commit, workflow URL, immutable image tag, and verification evidence.

- [ ] **Step 5: Prepare but do not automatically run the production comparison**

Create a recovery checkpoint and an explicit canary command that preserves the valid Example Movie A and Example Movie B sidecars. Because an unchanged managed candidate is correctly satisfied without LAPSE, a meaningful performance comparison requires a separately authorized test fixture, copied media, or reversible removal of installation state. Present that choice to the user before any production mutation.
