# Temporary workspace repair plan

User-authorized scope: move processing scratch from media directories to system temporary storage, review related behavior, fix related gaps, and push to GitHub. Base: `9a35743`. No production lifecycle or cleanup action is authorized by this code task.

1. Move workflow workspace creation to `os.MkdirTemp("", ".subsyncd-work-")`; use isolated `TMPDIR` tests to check analysis/extraction/synchronization paths and cleanup on success, rejection, technical failure, and cancellation after work begins.
2. Preserve media-local final staging/rollback, cache-local atomic publication, and persistent LAPSE profiles. Review extraction and LAPSE working directories for cross-filesystem safety.
3. Repair related retention: remove raw downloads after extraction and discarded candidate artifacts before subsequent attempts; preserve viable tied candidates through finalization fallback. Never remove cached source paths or globally scan old media-root workspaces.
4. Keep exact fallback uncapped, broad shortlist/tournament behavior unchanged, errors correctly classified, and installation atomic.
5. Redact the effective temporary root from application errors. Keep path boundaries intact, including custom roots with spaces and `/`.
6. The user's follow-up extends review through LAPSE analysis, finalization, publication, rollback, and notification delivery. Repair broad candidate-local installation fallback, guard filesystem and database media fingerprints before publication/commit, and prevent raw LAPSE output from entering outward-facing errors. Preserve technical-error and rollback boundaries.
7. Verify affected packages with race detection, full repository race/vet and local tagged E2E; independently review the final diff. Update operations/provider/contributor docs and the implementation ledger, then commit and push without deploying.

Review found extraction/cache/installer publication boundaries already safe across filesystems. Related gaps were accumulation of rejected artifacts, temporary-path error privacy, broad installation rejection aborting fallback, missing current-media guards, and raw LAPSE error output. Existing rollback restoration and asynchronous notification delivery need no structural changes.
