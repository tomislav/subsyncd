# Provider review repair plan

Approved by the user's request to fix all findings and push to GitHub. Base: `39f5e20`. Source review: `docs/provider-review-2026-09-05.md`.

## Scope and constraints

Repair the eleven reviewed defects with focused RED/GREEN tests. Retain release weights, exact-candidate completeness, broad shortlist size, LAPSE bypass anchors, language policy, credential redaction, provider response-lifetime permits, and candidate-local rejection semantics. No production inspection, provider contract calls, service restart, or deployment. GitHub push is explicitly authorized after verification.

Old normalized candidate caches must not retain fabricated Titlovi identity or incorrect SubDL episode evidence. Advance the cache version without rewriting installed subtitles or resetting schedules. The optional improvements in the review are follow-up observations, not grounds for changing pagination completeness or scoring policy in this repair.

## Implementation

1. Titlovi: remove query-derived IMDb evidence; preserve complete pagination through token refresh; persist definitive login rejection.
2. SubDL: use conservative episode-range evidence; preserve absolute single/range coordinates; select direct members using season and episode evidence.
3. Shared HTTP: recheck persistent availability after waiting for permits; account for genuine response-body network failures without poisoning candidate rejection state or interpreting local writer failures/cancellation as remote outages.
4. OpenSubtitles: persist definitive login rejection; validate and honor documented session API routing; separate already-issued file transfers from API download quota; omit API credentials from signed file transfers.
5. Invalidate old normalized search caches. Cover the changed metadata through scoring/selection consumer tests where adapter-only assertions cannot establish the behavior.
6. Update behavior docs and the resumable ledger, preserving the original review as historical evidence with resolution status.

## Verification and publication

- Each fix needs a permanent test observed failing for the original defect and passing with the repair.
- Run affected packages with `-race`; complete repository race tests, `go vet ./...`, and tagged local E2E tests after integration.
- Independently review the final diff against all eleven findings and unchanged invariants; resolve material findings before publication.
- Run formatting and diff checks, inspect the exact staged changes, fetch GitHub, and push the verified commit without force.
- Report commit, verification, and publication status. Hades is not redeployed by this task.

## Progress

- Implementation dispatched as one coherent Go-code/test task; controller owns documentation and integration review.
- Implementation and permanent regression coverage completed for all eleven findings. Independent review and scoped follow-up review approved. Final repository race tests, vet, tagged E2E, release-Dockerfile consistency, and diff/format checks passed.
- Publication: user-authorized commit and push follow this verification; production deployment remains separate.
