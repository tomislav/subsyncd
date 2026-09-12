# Review Repairs Implementation Plan

> Execute the three approved review fixes with TDD and independent review.

**Goal:** Repair all findings in the September 12 codebase review and publish to GitHub.
**Architecture:** Fence reconciliation with persisted instance revisions; let validated identical owned content refresh installation provenance; filter cached upgrade policy denials before preparation.
**Tech Stack:** Go 1.27.1, SQLite, existing workflow/catalog/store services.
**Spec:** `docs/code-review-2026-09-12.md` (R1–R3), approved by the user with “Fix them all, commit and push to GitHub”.

## Global constraints

Preserve ownership, current-media fingerprint checks, atomic provenance/outbox behavior, provider ordering, score/LAPSE policy, and technical-error classification. Tests use local fixtures and fake servers only. No production access or deployment. User authorized GitHub push. Original checkout Git metadata is read-only; use a writable temporary clone, preserve the unrelated extraction-proposal ledger entry in the original checkout.

## Tasks

- [x] R1: Add a focused stale reconciliation regression (delete/replacement/rename while hydrating). Read cursor and event revision before history hydration and compare revision within the commit transaction before mutation/cursor advancement. Keep all callers explicitly fenced; no optional bypass. Test retry from unchanged cursor and unaffected normal replay. Run catalog/store race tests.
- [x] R2: Add a real installer/store regression with identical owned bytes and changed eligible provider provenance. Preserve file inode/mtime, previous rollback reference, media/ownership validation, and transaction failure semantics. For eligible identical upgrades, refresh provenance through the ordinary atomic record path without rewriting bytes or generating content-change notification. Add workflow promotion coverage with real installer. Run workflow/store race tests.
- [x] R3: Add regression for an eligible cached pack below the upgrade delta with no provider replacement. Apply the same shouldUpgrade gate as broad candidates before prepareMembers. Policy denial must not record rejection or advance failure backoff; true errors remain errors. Run workflow race tests.
- [x] Review each repaired area and the combined patch; fix actionable findings. Update review status, provider/operations behavior and implementation ledger. Run full race suite, tagged e2e race suite, vet and diff checks. Prepare the verified commit for the authorized push to origin/main without force.

## Progress

Baseline local and fetched origin/main: `6c1c105`. Root causes were traced in the review; R1 and R2 reproduced using temporary tests. R1 owns catalog/reconciliation APIs; R2 owns installer persistence use; R3 owns cached acquisition policy. Shared interfaces: R1 repository changes do not alter installation methods; R2 and R3 both exercise workflow and must be integrated before final race verification. No contract conflict identified.

R1 complete: focused delete/series-delete/replacement/rename interleavings and overlapping empty-page regressions reproduced RED, then catalog/store race suites passed. Independent spec and quality review approved. Required snapshot includes cursor and revision to cover empty pages without repurposing event revisions. Removed unused unfenced cursor setter.

R2 complete: identical owned content regression reproduced RED, then workflow race suite passed. Real installer/workflow promotion, immutable file metadata, retained rollback, guarded failure and changed-media notification tests pass. Independent spec and quality review approved. All eligible identical upgrades refresh provenance after normal score/LAPSE checks; this makes a validated same-tier provenance improvement successful as well as fallback promotion.

R3 complete: focused cached-policy RED reproduced the technical failure; cache skip/provider continuation/malformed provenance regressions and workflow race passed. Independent spec and quality review approved. Ordinary no-result scheduling remains unchanged; the fix only removes the spurious technical failure.

Final combined review approved with no actionable findings. Full race suite, tagged E2E race, vet, gofmt and diff checks passed on the final runtime code. Publication is authorized and follows the verified commit; no deployment.
