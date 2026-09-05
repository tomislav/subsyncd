# Clean Schema Baseline Design

Date: 2026-09-05

## Objective

Reset subsyncd's pre-release SQLite lineage to one current baseline because every deployment is intentionally rebuilt from scratch. Remove compatibility-only runtime branches, tests, and active documentation while retaining the migration mechanism needed for future releases. Rebuild the isolated Hades canary from a fresh database, remove the authorized S01E06 Croatian canary sidecar, and repeat the three-file acceptance workflow.

## Compatibility contract

Databases created by the existing `001_initial.sql` through `010_media_entity_ids.sql` lineage are unsupported by the new build. There is no data migration, import, or automatic conversion path. Operators must stop subsyncd, preserve the old data directory if rollback matters, and start the new build with an empty data directory.

The store will retain `schema_migrations` and ordered embedded SQL migrations. The new first migration will be named `001_baseline.sql`. Startup will reject a database whose applied migration names do not belong to the embedded lineage, producing a clear unsupported-lineage error instead of falling through to duplicate-table SQL errors. This guard is not an old-schema adapter; it prevents accidental use of unsupported data. Future migrations can be added after the baseline normally.

## Baseline schema

`001_baseline.sql` will contain the complete schema and indexes currently produced by migrations 001 through 010. The ten existing migration files will be removed.

The `media` table will define `entity_id INTEGER NOT NULL CHECK (entity_id > 0)` without a zero default. Its stable identity remains `(instance, kind, entity_id)`, and its current physical-file identity remains `(instance, kind, file_id)`. Both unique indexes are required. `file_id` is not legacy: file replacement changes it while preserving the logical media row, and hashes, inventory, installations, webhooks, and fingerprints continue to use it.

The `events` table will continue to allow a null `event_id` and zero/default Arr identity fields. This is current behavior, not legacy behavior: local audit events such as `subtitle_installed` have no Arr event identity, and file-oriented delete webhooks can legitimately lack a stable entity ID. The partial unique event-ID index remains.

Optional external IDs, episode numbers, provider quota values, installation fingerprint values, and similar zero/empty defaults remain because absence and invalidation are current domain states. Installation fingerprint columns, `unsupported_reason`, search priority/rerun columns, candidate rejection evidence, notification leases/deduplication, media hashes, and episode titles all remain in the baseline.

## Runtime cleanup

The repository will remove zero-entity lazy adoption. `findMediaIdentityTx` will still look up both stable entity and physical file identities so it can update a replacement file in place and fail closed when two different rows conflict, but a file row with `entity_id = 0` will no longer be adoptable. All newly persisted media already require a positive entity ID.

`applyMediaMutationTx` will no longer fill a missing import/rename mutation entity ID from the hydrated media as a compatibility convenience. Webhook and reconciliation callers already provide the entity explicitly; validation will continue to reject an incomplete mutation.

The unused `UpsertSearchState` default-priority wrapper will be removed and tests will call `UpsertSearchStateWithPriority` explicitly. The test-only legacy provider-cache-key helper and old-cache fixture will be removed. Candidate JSON will continue omitting `forced: false`; its test will be renamed to describe current compact serialization rather than legacy compatibility.

Historical implementation plans remain historical records and will not be rewritten. Active README, architecture, operations, implementation-status, and `AGENTS.md` guidance will state the clean-baseline contract and remove lazy-adoption instructions.

## Tests

Test-driven implementation will cover these boundaries:

1. A fresh database applies exactly `001_baseline.sql` and exposes the complete expected tables, columns, indexes, foreign keys, and constraints.
2. An old-lineage database is rejected with the explicit unsupported-lineage error.
3. SQLite itself rejects a media row with a missing or non-positive `entity_id`.
4. Stable-entity upsert preserves one media row across file replacement and updates `file_id` and byte fingerprint state.
5. Conflicting entity/file identities still fail closed.
6. Known and unknown entity-addressed deletion audits retain their current behavior.
7. Webhook and reconciliation import/rename mutations remain required to provide a positive matching entity ID.
8. Search priority behavior and compact candidate serialization retain their current behavior without compatibility-named wrappers or tests.

Verification will include the relevant red/green tests, the complete race-enabled Go suite, `go vet`, tagged end-to-end tests, a clean container build, a fresh-container `doctor` run, Compose rendering, and `git diff --check`.

## Hades cutover and rollback

The cutover remains restricted to `/opt/subsyncd-daemon-canary` and the exact S01E06 Croatian sidecar. No other Hades service, database, subtitle, or media file is in scope.

Before mutation, record current health, image, database counts, both reconciliation cursors, sidecar checksum, and a UTC cutover timestamp. Stop the daemon. Preserve the complete old data directory and copy the exact sidecar into a root-only rollback directory, then delete only:

`/srv/media/tv/1883 (2021) [tvdbid-396390]/Season 01/1883.2021.S01E06.2160p.PMTP.WEB-DL.H265.SDR.DDP.5.1.English-HONE.hr.srt`

Deploy a clean, verified arm64 image and create an empty `data` directory with the existing `1000:1000` ownership. Run `doctor` to create the baseline database. With the daemon still stopped, initialize both Arr reconciliation cursors to the recorded cutover timestamp so events occurring during the short outage remain eligible for reconciliation without replaying older library history.

Start the daemon and require successful health, readiness, and reconciliation. Repost the two exact Radarr fixtures and the exact Sonarr S01E06 Download fixture so the fresh database contains exactly three media rows. The two movies must satisfy English from existing embedded inventory. S01E06 must satisfy English from its embedded stream, search Croatian through Titlovi, download candidates sequentially, require LAPSE unless current confidence policy permits a bypass, and atomically recreate the Croatian sidecar. Re-run Sonarr's native connection test plus mapped and actual outside-scope webhook checks. Verify no outside-scope media, no active lease, no unexpected provider download, and no warning/error log event.

Rollback stops the baseline daemon, preserves its failed/new data directory, restores the old data directory and backed-up sidecar, restores the prior image/config if changed, and starts the prior build. A new baseline database must never be opened by an older build because the migration lineages differ.

## Acceptance criteria

- The repository contains one baseline migration plus the general migration runner.
- Active code and documentation contain no zero-entity adoption contract.
- Current identity, fingerprint, audit, queue, rejection, and notification semantics remain intact.
- Old schema lineage fails explicitly and cleanly.
- Fresh local and container verification passes.
- Hades runs the new image with a fresh baseline database and exactly the same three mapped media entities.
- The authorized S01E06 Croatian sidecar is removed, then recreated only by the clean Titlovi/LAPSE workflow.
- Both real Arr webhook connections remain active and scoped behavior is reverified.
- Complete pre-cutover database and sidecar backups make rollback possible.
