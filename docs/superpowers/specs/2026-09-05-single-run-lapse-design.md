# Single-run LAPSE candidate evaluation

Status: approved by the user's “do it and push to github” following the verified proposal. Supersedes the two-invocation acquisition portions of the 2026-09-04 score-tier tournament design; its scoring, ordering, safety and fallback contracts remain authoritative.

## Verified basis

LAPSE v2.0.5 source commit 8d57e43ad72c2d05794d83591cf311499ae64c22 computes its verdict and ranking metrics before the same save branches used by dry and output runs. The official native binary produced identical metrics for seeded shifted/uncertain subtitle fixtures. Strict output mode refuses non-solid results. Dry-run may report written=true without a file, so actual output checks remain mandatory.

## Behavior

Acquisition invokes SynchronizeCandidate exactly once for each LAPSE-required candidate evaluated, writing a unique new private system-temporary output with --output, --no-backup, --json, --strict and --no-sidecar. There is no acquisition dry-run and no second LAPSE call after selection. Preserve Analyze/AnalyzeCandidate for standalone diagnostics; analyze-sync remains dry-run/read-only.

The syncer remains the owner of exit/JSON/version/verdict, output path, regular-file, size, UTF-8 and subtitle-syntax validation. Only a validated solid output enters the viable tier. Keep source path/checksum separate from output and retain the SyncResult which actually produced that file, including its confidence for ranking and provenance. Never overwrite confidence with a separate analysis result.

Preserve exact-hash and qualified score bypass (zero LAPSE invocations), minimum score, upgrade delta, metadata-only same-installed-candidate reassessment, three-candidate broad cap, uncapped lazy exact handling and current providers/scoring. No new setting or migration is needed.

Within descending equal-release-score tiers, prepare every tied candidate serially, then rank viable results by synchronization confidence, provider priority, rating and stable identity. Install the winning already-prepared output without another process. A preparation failure advances within the tier; direct candidate-local installation rejection advances to the next retained artifact; only an exhausted tier permits lower-tier preparation. Stale-media/filesystem/database/rollback errors remain terminal, cancellation starts no later candidate, and technical failures never poison candidate rejections.

Apply the same single-run preparation to cached pack members. Cached sources/manifests remain immutable. Rejection identity uses the original selected subtitle/checksum, not the derivative. Unique output allocation must not collide after cached-pack failure, tied reorder or lower-tier fallback. Failed/discarded private source/output files are removed promptly; keep successful tied artifacts only as long as fallback needs them. Whole-workspace cleanup remains on return/cancellation. Keep output bounds, final fingerprint checks, root containment, syntax validation, staging/fsync/atomic rename, checksum provenance, rollback and notification outbox unchanged.

## Observability and acceptance

An acquisition invocation emits one info lapse.sync_started immediately before execution and matching lapse.sync_completed or lapse.failed. It emits no fictitious analysis phase. Selection and installation follow preparation; a sync-completed candidate need not be the winner. Keep bounded fields/privacy and standalone diagnostic analysis semantics. Update debug decisions and docs to distinguish prepared candidates from the selected install.

Tests must prove one required candidate = one synchronization/no analysis, three tied viable candidates = three synchronization/no analysis, no lower-tier work after installation, retained-output fallback with no rerun, cached-pack failure followed by provider success, original rejection checksums, immutable cache source, output cleanup, cancellation, invalid/missing output, bypass/upgrade/inventory/install guards and log ordering. Run full race/vet/tagged E2E plus packaging checks and a pinned Linux binary contract fixture if locally available. No production deployment or real provider/media access is authorized. Push reviewed changes to GitHub main.

Tradeoff: tied losing candidates write temporary output; one successful unique candidate reduces two invocations to one and three viable ties reduce four to three. Do not claim a fixed wall-clock or CPU improvement without measurement.
