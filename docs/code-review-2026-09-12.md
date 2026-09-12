# Code review — 2026-09-12

Reviewed runtime baseline `6c1c105`, concentrating on acquisition/provider handling, inventory/publication, catalog reconciliation, and durable scheduling. This is a bounded correctness review, not an exhaustive proof. The original review accessed no production services and made no runtime fixes. The subsequent authorized repairs are recorded below and in the implementation ledger.

Repair status: R1, R2 and R3 are implemented with permanent regression coverage, approved independent task and combined reviews, and passing full race/E2E/vet verification. Final verification and publication are recorded in the implementation ledger.

## Findings

### R1 — High: stale reconciliation can overwrite a newer webhook

`internal/catalog/reconcile.go:34–73` hydrates history before committing it. `internal/store/repository.go:1710–1745` applies that snapshot without checking whether an intervening webhook changed the instance. A delete or replacement committed after hydration can therefore be overwritten by an older import. The media upsert clears deletion and can restore old file identity/path and runnable searches. A kept-file whole-movie/series deletion is particularly problematic because later file history need not repair it.

Capture the instance event revision before hydration and compare it inside the reconciliation transaction before mutations or cursor advancement. Reject and retry a stale snapshot, following the existing library-discovery fence pattern. Cover interleaved delete, replacement, and rename events.

A temporary SQLite regression reproduced the deletion case: after verifying the newer deletion, committing the stale import produced `deleted=false` and one runnable search. The temporary test was removed.

### R2 — Medium: identical content blocks upgrades and provider promotion

`internal/workflow/install.go:302–303` returns a generic error when the validated candidate bytes equal the unchanged managed subtitle. `internal/workflow/service.go:1243–1251` propagates that error; acquisition treats it as terminal infrastructure failure. Thus a preferred provider supplying the same bytes as an owned fallback installation cannot update provenance, and the workflow retries as a technical failure. Ordinary qualifying upgrades with identical bytes likewise stop further candidate processing.

Support an ownership- and fingerprint-checked transactional provenance refresh for valid promotions without rewriting bytes. Treat ordinary unchanged candidates as a nontechnical outcome so remaining candidates can proceed. A temporary focused test reproduced the existing rejection and was removed after review.

### R3 — Medium: cached upgrade policy rejection becomes technical failure

`internal/workflow/service.go:299–309` sends a different cached candidate through preparation without the upgrade eligibility prefilter used for broad results. `prepareCandidate` at line 1115 returns a generic error when the candidate does not satisfy the score delta. `prepareMembers` forwards it to candidate-failure handling, and `classifyCandidateFailures` turns it into an acquisition error if no subsequent candidate succeeds. A healthy upgrade check with only a lower-scoring cached pack therefore advances failure backoff instead of using the ordinary no-result schedule.

Apply the same upgrade eligibility filter before preparing cached candidates. Keep policy denials separate from repository and technical errors. Add a regression with an unchanged installation, a different eligible cached pack below the required delta, and no provider replacement. This finding was verified by tracing the call path, not by an executable regression.

## Verification

- Full `go test ./... -race` passed.
- `go vet ./...` passed.
- Used installed Go 1.27.1 with explicit `GOROOT`; the inherited environment referred to missing Go 1.27.0.
- Tests used local fixtures and fake servers. No live Arr, provider, Silo, or production calls.
- Existing user changes in the implementation ledger were preserved.

Implemented in the R1, R2, R3 order with focused failing regressions before production changes. The identical-content repair refreshes provenance for every eligible upgrade; it preserves the normal score and synchronization requirements.
