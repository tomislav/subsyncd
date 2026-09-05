# Implementation status and handoff log

This file is the resumable implementation ledger. The approved design and plan remain authoritative; this records what the code actually implements and why.

## Current state

- Branch: `main`
- Current task: the codebase correctness-repair design and implementation plan are approved and committed
- Next safe action: choose subagent-driven or inline execution, then implement Task 1 with strict red-green-refactor TDD; production remains out of scope
- Latest follow-up: `.agents/production.local.md` exists only in this checkout with mode `0600`; no subsyncd container is running on Hades
- Runtime module: `subsyncd` on Go 1.27.1
- Test caches: `GOCACHE=/tmp/subsyncd-gocache`, `GOMODCACHE=/tmp/subsyncd-gomodcache`

## Completed tasks

### Codebase correctness-repair design

- Commit `bdda491` records the approved architecture for the eight remaining findings from the 2026-09-05 conventional review. The two already-repaired findings—stable Arr history identity and OpenSubtitles parent-series identity—are explicitly outside its implementation scope.
- The design makes exact and broad provider searches explicit workflow phases, retains every exact candidate, filters and downloads them sequentially, falls back to broad search after exact exhaustion, and preserves distinct deterministic, technical, and throttled outcomes.
- It defines conservative hyphenated episode ranges, universal forced-only member policy, one atomic installation/outbox SQLite transaction with filesystem rollback, first-install semantics for a missing managed sidecar, pre-expansion multi-document YAML rejection, root-aware Silo mappings, and response-body-owned provider concurrency permits.
- No schema or configuration migration is planned. The existing notification outbox remains asynchronously delivered: inability to persist an intent fails and rolls back installation, while a remote Silo delivery failure after commit does not.
- Commit `a54bdfb` adds the nine-task TDD implementation plan: explicit provider modes, exact-first workflow fallback, archive/member safety, deleted-sidecar recovery, atomic installation/outbox persistence, YAML cardinality, root Silo mappings, response-lifetime permits, and final documentation/verification.
- Plan self-review mapped every design requirement to a task, checked the declared function/type names across task boundaries, and found no unfinished placeholders. It also clarified that filename/member selection supplies forced-only evidence while the existing candidate-level gate supplies hearing-impaired policy.
- `git diff --check` passed. No production code, image, provider, Silo, or Hades state changed.
- Next task: execute the plan with the selected execution workflow, beginning with Task 1's failing coordinator tests.

### Local production-access runbook convention

- Commit `7338264` adds the exact Git ignore rule for `.agents/production.local.md`, a sanitized tracked `.agents/production.example.md` schema, and an `AGENTS.md` instruction to read the local file before production debugging or deployment when present.
- The ignored local runbook records the Hades SSH alias, Arcane project and Compose paths, `/opt/subsyncd` configuration/data locations, service exposure, bounded read-only diagnostic commands, and explicit approval boundaries. It contains no credentials or `.env` values and is mode `0600`.
- Read-only discovery confirmed Arcane project `subsyncd` at `/var/lib/docker/volumes/arcane_arcane-data/_data/projects/subsyncd`, with runtime configuration `/opt/subsyncd/config.yaml` and data `/opt/subsyncd/data`. At observation time, Arcane resolved the service but no production container existed; agents must recheck time-sensitive state.
- Verification passed `git check-ignore -v .agents/production.local.md`, the local mode check, `git diff --check`, and focused credential-pattern scanning. No Hades state was changed and the ignored local file was not staged or committed.

### Human-readable media identity in structured logs

- Commit `5f03cf4` adds the sanitized `media_title` field after durable media resolution. Movie identities render as `Movie (Year)` and episode identities as `Show - S01E02 - Episode Title`; control characters collapse to spaces and output is bounded to 2,048 Unicode code points.
- Worker lifecycle records and the workflow context carry the title through provider, candidate, LAPSE, installation, and terminal events. A failed media lookup retains the existing ID-only records because no trusted title is available.
- The title remains ordinary JSON data and is explicitly excluded from the Alloy/Loki label set. Existing path privacy remains unchanged: absolute paths are still forbidden and root-relative paths remain debug-only.
- Focused tests first failed on the absent formatter and absent worker/workflow fields, then passed under the race detector. Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `go test ./test/providercontract -tags=provider_contract -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the credential-pattern scan.
- No image was built or deployed, and Hades remains without a running subsyncd container.

### Provider correctness and credential repair

- Commit `6f5dd3d` replaces the OpenSubtitles hash dependency with the canonical 64-bit first/last-64-KiB implementation. A sparse 14,836,065,099-byte regression proves hashing no longer fails at the former 9 GB limit and does not read the complete media file.
- Fresh and legacy-cached provider results now collapse duplicate `(provider_id, result_id)` rows before persistence, scoring, rejection filtering, and LAPSE shortlisting. The merge unions release names, fills only missing identity fields, retains the first nonempty identity on conflicts, and keeps the strongest exact-hash, rating, popularity, and download-count evidence. This prevents duplicate API rows from downloading or analyzing the same subtitle more than once.
- Successful HTTP responses discard `Retry-After` before common rate-limit parsing while retaining standard `RateLimit`/`X-RateLimit-*` windows. Error responses, including 429 and 5xx, keep existing `Retry-After`, fallback, and circuit behavior.
- SubDL normalization drops returned download-query credentials, reconstructs the configured key only on outbound requests, and emits credential-free result IDs. Cache, workflow provenance, pack-member copies, and candidate log correlation independently strip download references or query/fragment data.
- Migration `002_scrub_provider_credentials.sql` deletes only credential-bearing provider-derived cache/candidate/pack/installation/rejection rows. Regression fixtures prove that clean provider rows, media, active search leases, reconciliation cursors, and clean pack members survive, while members of deleted contaminated packs cascade. Existing subtitle files remain on disk if contaminated managed provenance is removed.
- TDD regressions failed against the prior size cap, duplicate retention, successful-response cooldown, SubDL query persistence, log fallback, cache pack-member reference, and migration boundary before the corresponding fixes. Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `go test ./test/providercontract -tags=provider_contract -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the credential-pattern scan.
- No subsyncd container is currently running on Hades. No image was built, published, deployed, or started. Rotate the SubDL key that was exposed by the legacy persisted candidate identity before using the corrected build.

### Comparative reference documentation cleanup

- Commit `863f5d7` removes the obsolete comparative reference document, its contributor-guide requirement, stale publication-plan checks, and every remaining mention from the current repository tree.
- Historical Git objects were deliberately left unchanged; rewriting published history would change commit identities and require a coordinated force-push.

### Newly configured language backfill

- Commit `2c64814` adds an offline startup reconciliation that inserts only absent search rows for every configured language and already-indexed media item belonging to a currently configured Arr instance. Supported media is immediately due at missing priority; unsupported multi-episode media receives its existing terminal outcome.
- The insert is transactional and conflict-ignoring. Existing attempts, technical-failure counters, due times, priorities, outcomes, leases, rerun state, installations, and other-language rows remain unchanged. Media retained for an instance no longer present in configuration is not backfilled.
- Adding a provider to an existing language changes its workflow route after restart without rewriting schedules. Adding a new Arr instance continues to use an empty reconciliation cursor plus retained Arr history; this change does not add a full-library Arr enumeration or startup network request.
- TDD evidence first failed on the absent repository method and then on the absent startup wiring. Focused repository and application tests passed under the race detector. Fresh full verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, Compose rendering, and `git diff --check`.

### Silo parent-directory notification correction and Hades test

- Live Hades evidence showed that authenticated `POST /api/v1/scan` uses Silo's native container listener on port 8080. A media-file target returned 202 but completed without walking sibling files; the exact Season 01 directory target performed a subtree scan and processed all 10 media files.
- The notifier now applies the boundary-aware path mapping first and sends the mapped media file's parent directory. Unit and tagged end-to-end assertions require that exact payload. Container examples and Hades mappings use `http://silo:8080`.
- Commit `4af206c` passed the full race suite, vet, tagged end-to-end tests, Compose rendering, and diff hygiene. Its local arm64 image is `sha256:20bbdf3fd59dc07418609336a0d0c8ddaca76040e54565db4ae6ca09acf0ef31`, version `sha-4af206c`, user `1000:1000`.
- A single synthetic SQLite notification exercised the real durable worker. subsyncd delivered it once in 14 ms; Silo returned 202 in 8 ms and completed a 10-file subtree scan. Silo's database retained exactly one external subtitle matching the S01E06 Croatian sidecar.
- The synthetic notification was deleted after verification. The temporary API key was removed from `.env`, Silo was returned to `enabled: false`, and doctor plus readiness passed. Hades remains healthy on `sha-4af206c` with zero notification rows.

### Clean schema baseline and Hades Stage 6 cutover

- The clean arm64 `sha-791916b` image was verified as user `1000:1000` and image ID `sha256:30549ef2bf6154dff57d4ce077cd2a7ba9eb85c1ab5b352156cb7c4c898fd69d`. The old database was moved to `data.before-schema-baseline-791916b`; configuration and the exact Croatian sidecar were backed up before the authorized reset.
- Doctor created a fresh database with exactly `001_baseline.sql`. Both instance cursors were seeded to the captured cutover time before startup, and immediate Radarr/Sonarr reconciliation completed successfully.
- Replaying the exact two movie fixtures and S01E06 produced three media rows with entity/file identities `338/1168`, `529/1440`, and `3913/10545`. Titlovi candidate `342548` scored 59, was the only download, received LAPSE `solid` confidence `0.859118`, and recreated the expected 29,034-byte Croatian sidecar.
- Sonarr connection 10's native test was ignored without mutation; the mapped rename added one event without a provider download or checksum change; an actual unmapped Download was ignored without mutation or worker wake. Final baseline counts were `3 media / 5 events / 6 search states / 2 candidates / 1 installation`, with zero leases, warnings, or errors.

### Clean schema baseline Tasks 1–4

- Replaced the ten pre-release migrations with one complete `001_baseline.sql` while retaining the ordered embedded migration runner. Startup now rejects applied migration names outside the embedded lineage with an explicit rebuild-from-empty error.
- The baseline enforces positive `media.entity_id` in SQLite and retains separate replaceable `file_id`, current event-local zero identities, fingerprints, queues, rejections, notifications, and all final indexes/foreign keys.
- Removed zero-entity adoption, import/rename entity fallback, the unused default-priority search wrapper, and obsolete old-cache compatibility tests. Current file-replacement conflict checks, candidate serialization, provider cache safety, and entity-addressed deletions remain covered.
- Focused red/green store, catalog, worker, provider, and domain suites passed under the race detector. Full race, vet, tagged end-to-end, image, and live Hades verification completed before the Stage 6 cutover was accepted.

### Hades Stage 3 Task 2 — direct image and real Radarr webhook

- GitHub `main` was pushed through `4ddbcaf`; workflow `33952193028` passed its Verify job and began multiarch publication, but deployment did not wait for it. A fresh local arm64 image was built from the same commit, returned `subsyncd sha-4ddbcaf`, and was streamed to Hades as `subsyncd:hades-stage3-4ddbcaf` (`sha256:0f2ead23eea7daa98628ce51f80c52ea61acf43674abd5ec8c22a74391443e3f`). Rollbacks are `compose.yml.before-stage3-4ddbcaf` and `data.before-stage3-4ddbcaf`.
- Radarr connection ID `8` (`subsyncd daemon canary`) enables download/upgrade, rename, and movie-file delete events. Its connection test returned 200 and subsyncd ignored the test payload with HTTP 204.
- A mapped file-1440 rename applied once, woke one import-priority job, and completed embedded `satisfied` in 6 ms. Exact redelivery was a 204 duplicate with no second job. One payload derived in memory from an actual current unmapped movie returned ignored 204 with no event/media/search mutation or wake.
- Final state remained two media rows, two completed searches, five audit events, zero candidates/installations/provider cache/hashes, healthy, and ready. The real connection and canary remain active pending one natural six-hour reconciliation; mappings and every other Hades service remain unchanged.

### Hades Stage 3 Task 1 — safe outside-scope webhooks

- Import/rename hydration now converts only wrapped `ErrOutsideScope` into `ErrIgnoredEvent`. The HTTP boundary returns 204, persists no event/media/search mutation, and emits no worker wake. Unsafe mapping/filesystem errors continue through the normal failure path; delete webhooks remain file-ID addressed.
- TDD RED evidence reproduced the existing wrapped scope error instead of the ignored sentinel. GREEN verification passed the focused test, affected catalog/http/app race suites, complete race suite, `go vet ./...`, tagged race-enabled end-to-end suite, and `git diff --check`.
- Code commit: `629a050`. Next: publish the documented boundary, deploy its immutable image to the isolated canary, and create the real Radarr webhook connection.

### Follow-up — stable-identity Hades Stage 2 hydration

- The daemon stopped cleanly and its complete post-Stage-1 state was retained at `/opt/subsyncd-daemon-canary/data.before-stage2-f04b3c9`. Stage 2 did not broaden the exact two-file mapping boundary.
- Sequential live Radarr searches for 1917 English and Arrival English completed as embedded `satisfied` outcomes in 8 ms and 5 ms, with zero candidates and provider errors. The existing rows adopted stable Radarr movie IDs `529` and `338` while retaining physical file IDs `1440` and `1168`; media remained exactly two rows.
- Candidates, installations, provider cache/state, hashes, and notifications remained zero. The daemon restarted healthy and ready, and its immediate reconciliation completed successfully in 6 ms. It remains running on `subsyncd:hades-arrapi-f04b3c9` with the manual canary, sidecars, mappings, credentials, and other Hades services unchanged.

### Follow-up — stable-identity Hades Stage 1 retest

- Commit `f04b3c9`'s locally verified arm64 image was streamed to Hades without publication and loaded as `subsyncd:hades-arrapi-f04b3c9`, image ID `sha256:9138534acd0cd9e4196abf7a95693656f50bf92aa7427551c427dbe98d66d4fb`. The isolated `/opt/subsyncd-daemon-canary` Compose file is pinned to that tag; its prior Compose and complete stopped data directory are retained as `compose.yml.before-f04b3c9` and `data.before-f04b3c9`.
- Migration 010 applied and the exact cursor that previously produced `movie history record has incomplete identity` completed successfully in 56 ms. The cursor advanced from `2026-09-04T17:44:39.142509216Z` to `2026-09-05T07:08:48.568200533Z`; two unrelated Radarr entities became zero-file outside-scope delete audits.
- The scope boundary held: media remained at the two mapped movies, search states remained two completed English `satisfied` rows, and embedded inventory remained 17 tracks. There were zero candidates, installations, provider cache/state, media hashes, rejections, notifications, or provider/LAPSE/install log events. The two legacy rows remain at zero entity ID pending a later live mapped hydration.
- Health and readiness returned `ok`/`ready`; logs contained one reconciliation start, one completion, and no failure. The updated canary was left running with `restart: "no"`. Mappings, secrets, `/opt/subsyncd-canary`, and every other Hades service were unchanged.

### Arrapi stable-identity reconciliation Task 7 — durable contract and release gate

- Durable README, architecture, operations, release, and contributor guidance now distinguishes stable movie/episode entity identity from replaceable physical file identity. It records arrapi v2.0.5 ownership of history/current-entity requests, the temporary detail-enrichment client, atomic entity lifecycle behavior, second-resolution cursor overlap, replay safety, narrow-canary outside-scope handling, lazy legacy adoption, and 5m/15m/1h/6h failure backoff.
- Startup remains offline and performs no full-library identity backfill. The compatibility limit remains explicit: a historical deletion for a legacy zero-entity row cannot be linked because Arr does not retain the deleted physical file ID; it is audited as unknown and the row remains until later live hydration or operator action.
- The first full race run exposed one pre-entity OpenSubtitles persistence fixture. Root-cause tracing showed production hydration already provided positive identity; the shared hydrated-media fixture was corrected at its source, its focused race suite passed, and the complete matrix was rerun afterward.
- Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `docker compose -f compose.example.yml config --quiet`, the obsolete-symbol/toolchain/version scans, and `git diff --check`. A native arm64 image `sha256:9138534acd0cd9e4196abf7a95693656f50bf92aa7427551c427dbe98d66d4fb` built with Go 1.27.1 and returned `subsyncd arrapi-local` from `--version` as UID/GID `1000:1000`.
- Implementation commits are `a2246e9` (bounded arrapi client), `8ef2561` (stable persistence), `4848059` (scoped detail identity), `485c188` (entity history/current state), `5ff7f54` (atomic entity deletion), and `43f398f` (failure backoff), followed by this documentation/verification boundary. Final scope review found no changes to provider routing, scoring, LAPSE, subtitle handling, Silo, automatic webhook management, publication behavior, or Hades deployment.
- Nothing was pushed, published, or deployed. The next action requires separate explicit authority.

### Arrapi stable-identity reconciliation Task 6 — failure backoff

- Worker reconciliation timing now records the last attempt and consecutive failure count independently per Arr instance. Failures retry after 5 minutes, 15 minutes, 1 hour, then 6 hours indefinitely; success resets that instance to the normal configured reconciliation interval.
- Attempt state is deliberately in memory. A process restart attempts reconciliation immediately, while the durable catalog cursor remains unchanged across every failure and still prevents a successful page gap.
- `reconcile.failed` now includes bounded `attempt` and `retry_at` fields. Error detail continues through the configured observability redactor; the regression test proves upstream body/path markers do not enter JSON logs.
- Clock-driven TDD evidence first reproduced retry on the 12:04:59 recovery poll. It now proves attempts exactly at 12:00, 12:05, 12:20, 13:20, and 19:20, then a six-hour post-success interval. A healthy second instance reconciles at its own six-hour boundary while the failing instance remains delayed.
- Verification passed with the focused backoff test, complete race-enabled worker and catalog suites, and `git diff --check`. One pre-entity SQLite worker fixture was updated with its stable ID.
- Next task: update README/architecture/operations/agent contracts, run the full unit/vet/e2e/container matrix, review the final diff against the approved design, and keep push/publication/Hades deployment out of scope.

### Arrapi stable-identity reconciliation Task 5 — atomic entity deletes

- The reconciler now validates history identity, state, hydrated-media consistency, and present event type before committing. Present state becomes a file-addressed import/rename; absent and outside-scope state become positive-entity, zero-file deletes. Unknown states fail before any store call or wake callback.
- Repository mutation validation distinguishes hydrated import/rename, file-addressed webhook delete, and entity-addressed reconciliation delete. A known entity delete resolves its current media/file inside the page transaction, completes searches as deleted, and enriches the audit with entity, file, and media linkage. An unknown entity remains an idempotent audit with entity ID, zero file ID, and no media link.
- Page mutations, audit linkage, search completion, replay deduplication, and cursor advancement remain one SQLite transaction. A later instance mismatch rolls back an earlier entity delete, its audit, and the cursor.
- The tagged Sonarr integration now uses live-shaped entity history with a file-less deletion, a normal import, a canonicalized combined file, and an outside-scope entity. It proves known deletion, unknown outside-scope audit, replay safety, support-state scheduling, and cursor advancement through the real arrapi-backed catalog and SQLite repository.
- Verification passed with focused RED/GREEN entity tests, race-enabled catalog/store suites, the tagged `TestSonarrReconciliation` integration, and `git diff --check`.
- Next task: replace success-only reconciliation timing with independent 5m/15m/1h/6h failure backoff per Arr instance.

### Arrapi stable-identity reconciliation Task 4 — entity history and current state

- The catalog contract now accepts both the durable `since` cursor and captured `through` page end. History changes carry stable entity/kind, typed current state, event type, and hydrated media only when present; no physical file ID is used as the history reduction key.
- Radarr and Sonarr use arrapi v2.0.5 for `/api/v3/history/since` and current movie/episode reads. Relevant history is reduced by `movieId`/`episodeId`, after-page records are filtered before collapse, and current state decides present, absent, or outside-scope behavior. Rename survives only as the latest present change; other present states normalize to import, while absent/outside normalize to delete.
- Sonarr verifies that each triggering episode belongs to the current file, caches detail hydration per file, and collapses attached multi-episode history to the deterministic canonical episode. The existing hardened client remains only for richer file/series/movie detail metadata.
- Sanitized fixtures mirror live API shapes: imports may contain `data.fileId`; deletes retain entity IDs and omit file IDs; unknown events are ignored. Tests cover RFC3339-second requests, page-end filtering, replacement resolution, no-file state, outside scope, malformed identity, attached-episode collapse, and membership failure.
- Verification passed with the complete race-enabled catalog and application suites, the focused history suites, `git diff --check`, and a negative scan proving the history fixtures contain no top-level `movieFileId` or `episodeFileId`.
- Next task: allow zero-file, positive-entity delete mutations, resolve known rows inside the reconciliation transaction, retain unknown audits, and advance the cursor only with the whole page.

### Arrapi stable-identity reconciliation Task 3 — detail identity and scope

- Radarr detail hydration now assigns the stable movie ID from the movie file and rejects a missing movie identity. Sonarr assigns the deterministic canonical episode ID, rejects attached episodes without IDs, and privately returns the complete sorted attached-episode set for later history membership checks.
- `ErrOutsideScope` identifies only deliberate scope exclusions: no boundary-aware mapping or a safely resolved mapped path outside configured media roots. Traversal, relative mappings, inaccessible/non-directory parents, dangling-symlink resolution, and media-root resolution remain hard failures.
- TDD evidence: focused tests first failed on missing entity assignments, Sonarr hydration evidence, and scope sentinel. Race-enabled `internal/catalog`, `internal/store`, and `internal/app` verification plus `git diff --check` passed using local fake Arr servers. One old history fixture was corrected to include the episode ID present in real hydrated responses.
- Next task: use arrapi v2.0.5 history and current-entity reads, reduce by stable entity, filter at the captured page end, and collapse combined-episode present state canonically.

### Arrapi stable-identity reconciliation Task 2 — stable entity persistence

- Migration `010_media_entity_ids.sql` adds zero-defaulted `entity_id` columns to media and audit events plus a positive-only unique `(instance, kind, entity_id)` index. Legacy rows survive migration with zero identity and adopt a positive entity ID lazily on successful hydration.
- `domain.Media` and media event mutations now carry stable entity identity separately from replaceable Arr file identity. Media reads round-trip it, events persist it, and `FindMediaByEntity` provides an explicit found/not-found lookup.
- Direct and transactional upserts resolve positive entity identity first, fall back only to a matching legacy zero-ID file row, and fail closed when entity and file identities point to different rows. A file replacement updates the original row and file lookup, invalidates candidate/installation provenance, and retains an active lease while scheduling one rerun.
- TDD evidence: focused tests first failed on the absent columns, fields, lookup, and file-only upsert. Race-enabled verification passed for `internal/store`, `internal/catalog`, and `internal/app`; the restricted run's local-server bind failures were environmental and the permitted local-only rerun passed. `git diff --check` passed.
- Next task: return entity identity from Radarr/Sonarr detail hydration and distinguish safe out-of-scope paths from hard mapping/filesystem failures.

### Arrapi stable-identity reconciliation Task 1 — bounded client boundary

- Pinned `github.com/cplieger/arrapi/v2` v2.0.5 and aligned the module, Docker builder, and GitHub verification job on Go 1.27.1.
- Added private Sonarr/Radarr interfaces and offline constructors in `internal/catalog`; arrapi concrete types do not escape into domain, store, workflow, provider, or application interfaces.
- Added a privacy boundary for arrapi retries and failures. Retry logging discards upstream messages, attributes, and groups and emits only bounded catalog events plus instance identity. Safe errors expose only instance, operation, failure class, status, and retryability.
- TDD evidence: the focused test first failed because arrapi and the private constructors were absent. Verification passed with the focused catalog race test, exact module-version inspection (`github.com/cplieger/arrapi/v2 v2.0.5`), and `git diff --check`.
- Next task: migration 010 and entity-first persistence, including upgrade-in-place, legacy adoption, conflict rejection, and lease/rerun preservation.

### Follow-up — arrapi stable-identity reconciliation design

- Live Hades evidence and the exact Radarr 6.3.0.10514 and Sonarr 4.0.19.2979 contracts show that history exposes stable `movieId`/`episodeId`, not the top-level `movieFileId`/`episodeFileId` assumed by subsyncd. Import records may carry `data.fileId`; deletion records do not. Nested current file IDs cannot serve as historical deletion identity.
- The approved design is `docs/superpowers/specs/2026-09-05-arrapi-stable-identity-reconciliation-design.md`. It pins `github.com/cplieger/arrapi/v2` behind the catalog adapter, persists stable entity identity separately from replaceable file identity, reduces history by entity, resolves authoritative current state, and commits entity adoption/deletion/audit/cursor changes atomically.
- Because arrapi's curated DTOs omit scoring/provenance fields subsyncd currently needs, the hardened custom detail requests remain temporarily as a narrow enrichment boundary; the custom history DTO and transport are removed.
- Deliberately unmapped history becomes a typed outside-scope result so the two-movie Hades canary can consume production Radarr history without indexing or reading the rest of the library. Unsafe paths and filesystem resolution failures still fail closed.
- Existing rows adopt `entity_id` lazily on their next successful hydration. No startup or full-library network backfill is introduced. Delete webhooks retain file-ID behavior; the one-time legacy limitation for a missed deletion before adoption is explicit.
- The design also prevents failed reconciliation from retrying every recovery poll while retaining the durable cursor for a later scheduled attempt. Publication, GitHub push, and Hades deployment remain separate actions requiring approval.
- Design-boundary verification: placeholder/ambiguity review and `git diff --check`. Next task: user review, then write the test-driven implementation plan.

### Follow-up — arrapi stable-identity reconciliation implementation plan

- The implementation plan is `docs/superpowers/plans/2026-09-05-arrapi-stable-identity-reconciliation.md`.
- Seven reviewable, test-driven boundaries cover the pinned arrapi/toolchain and safe error boundary, stable entity persistence and upgrade-in-place behavior, scope-aware detail hydration, real API-shaped entity history, atomic entity deletions/audits, reconciliation failure backoff, and final documentation/full verification.
- The plan keeps arrapi private to `internal/catalog`, retains only the narrow detail enrichment requests required for scoring/provenance fields absent from arrapi v2.0.5, and preserves offline startup plus file-oriented webhook/workflow behavior.
- Plan self-review covers every approved design section with exact interfaces, fixtures, failure expectations, commands, and commits. Push, publication, and Hades mutation remain excluded.
- Planning-boundary verification: spec coverage review, placeholder/type scan, and `git diff --check`. Next task: execute with the user's chosen workflow.

### Follow-up — isolated Hades daemon canary

- Commit `b3a2d45` passed the GitHub verification job and published native amd64/arm64 images as `latest` and immutable `sha-b3a2d45`. Hades pulled the private arm64 image using a temporary root-only Docker credential; that credential directory was removed immediately. The local image ID is `sha256:03c48f02a4778ff7f6df21b990d1d8a941d4711ede9377635ad1fcc16655dfac`, and the embedded version label is `sha-b3a2d45`.
- The isolated deployment lives at `/opt/subsyncd-daemon-canary`. It uses a fresh database, loopback-only port `18097`, one worker, English routed only to OpenSubtitles, Silo disabled, no automatic Arr connection, and exact file mappings for Radarr movie-file IDs `1440` (1917) and `1168` (Arrival). The existing `/opt/subsyncd-canary` manual deployment was not changed or run.
- Before first start, `doctor` created the schema and passed configuration, SQLite, three scoped roots, LAPSE compatibility, and FFprobe checks. The canary reconciliation cursor was then initialized to current UTC so historical production Radarr events were skipped. Startup and post-restart reconciliation each completed successfully.
- Both manual Download webhooks returned HTTP 204 and were processed sequentially at import priority. 1917 indexed six subtitle tracks, including five embedded; Arrival indexed eleven, including ten embedded. Their English searches completed as `satisfied` with zero attempts/failures because existing embedded English tracks fulfilled policy.
- Exact duplicate delivery returned HTTP 204 with `outcome=duplicate` and no leased job both before and after a daemon restart. After restart, the same SQLite state remained terminal with no pending or leased work.
- Final live audit at `2026-09-04T17:46:15Z`: container running and healthy, `/healthz=ok`, `/readyz=ready`, zero candidates/installations/provider-cache/provider-state rows, zero provider/LAPSE/install/notification/warn/error log events, unchanged Croatian sidecar checksums, and idle usage of about 6.84 MiB with eight PIDs. The canary was deliberately left running with Compose `restart: "no"` for observation.
- Deployment discovery: Arcane can access the private package, but its authorization is not automatically available to `sudo docker` over SSH. Also, host group `ubuntu` is GID 1001 while the container PGID is 1000; the deployment root and config must therefore be `root:1000` with modes `0750`/`0640`, data `1000:1000 0750`, and `.env` `root:root 0600`.
- Documentation/config verification passed with `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, the placeholder-secret daemon Compose render, and `git diff --check`. The tests require loopback-bind permission for local `httptest` servers; their first restricted-sandbox invocation failed only at `listen ... operation not permitted` and the unrestricted rerun passed.
- The reproducible host-specific Compose/config/webhook fixtures and operator evidence are in `deploy/hades-daemon-canary`. Stages 1–5 are complete: real Radarr and Sonarr connections are active, one exact Sonarr/Titlovi/LAPSE episode workflow installed successfully, and both Arr integrations have proven mapped and outside-scope delivery behavior. Next task: observe natural webhook and six-hour reconciliation traffic with the unchanged three-file scope before any further expansion.

### Follow-up — wrong-episode archive hardening

- Episode archives now always pass episode-member validation. A non-pack archive containing one generically named subtitle retains compatibility, but a filename with explicit season/episode, range, or absolute evidence must match the requested episode before LAPSE or installation.
- The concrete Titlovi catalog issue #104 case (`306201`, requested Ozark S03E01, archive containing only S03E03 members) remains a candidate-local `pack_selection` rejection. It does not alter provider cooldown state and the workflow continues through the remaining shortlist.
- Debug-only `candidate.rejected` events now add bounded `reason_code`, `selection_rule`, `archive_type`, `subtitle_member_count`, and `matching_member_count` fields. Filenames, subtitle content, and absolute paths remain excluded.
- TDD red evidence: the singleton regression installed `306201`, and the issue reproduction lacked every required diagnostic field. Fresh verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/pack ./internal/workflow -race -count=1`, `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check` pass. Next task: none inside this hardening change.

### Conventional repairs Task 7 — integration coverage and durable documentation

- A tagged integration test now drives typed Sonarr history through the real catalog adapter and SQLite repository. One atomic page imports a searchable episode, tombstones an existing episode, persists an out-of-order combined file as `unsupported_multi_episode`, advances the cursor, and exposes only the supported import as due work.
- README, operations, architecture, and contributor guidance now preserve the reconciliation transaction, failed-page cursor, active-lease rerun, fail-closed combined-episode, two-window shutdown, and incomplete-rollback recovery contracts for future agents and operators.
- Verification: fresh parallel runs of `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1`, `go vet ./...`, and `go test ./test/e2e -tags=e2e -race -count=1` all exited 0; `git diff --check` also passes. Next task: none inside this repair plan.

### Conventional repairs Task 6 — visible installation rollback failures

- Installer rollback now joins the initiating failure with removal, restoration, directory-sync, or unused-backup cleanup failures instead of discarding them.
- A first-install sidecar that cannot be removed is reported explicitly and remains protected as untracked inventory. A failed managed replacement restoration retains its last known-good rollback copy rather than deleting it.
- Joined rollback failures remain ordinary technical workflow errors; they do not produce `rejected` outcomes or candidate quarantine entries.
- Verification: the focused first-install test reproduced the swallowed removal failure; all three restoration/classification tests and `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1` plus `git diff --check` pass. Next task: integration and durable documentation.

### Conventional repairs Task 5 — bounded forced shutdown

- Daemon and deterministic drain paths now wait one graceful `ShutdownTimeout`, cancel once, then wait at most one additional `ShutdownTimeout` before returning.
- The first deadline emits `worker.drain_timed_out`; the second emits `worker.drain_abandoned` with bounded active-search and maintenance counts. Neither path clears durable leases.
- Completion channels remain buffered, and a race-safe regression harness proves a cancellation-insensitive workflow cannot hold daemon shutdown indefinitely or block on its late result.
- Verification: both focused tests reproduced the old unbounded wait; `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker -race -count=1` and `git diff --check` pass. Next task: installation rollback failure visibility.

### Conventional repairs Task 4 — fail-closed multi-episode indexing

- Sonarr sorts episode records by season, episode, absolute number, and ID. Zero episodes remains a catalog consistency failure; two or more use the earliest display identity and persist `unsupported_multi_episode`.
- Unleased unsupported searches are terminal at mutation time. A leased terminal rerun is intercepted immediately after current media load, completed without calling the workflow, and receives no provider, LAPSE, candidate, rejection, installation, or upgrade work.
- `explain` emits `unsupported_reason=unsupported_multi_episode`. The Task 1 installation transaction check remains the final guard against stale work already inside an external operation.
- Verification: the three focused tests failed on unsorted selection, workflow entry, and missing explanation; unrestricted `go test ./internal/catalog ./internal/store ./internal/worker ./internal/app ./internal/cli -race -count=1` and `git diff --check` pass. Next task: bounded canceled shutdown.

### Conventional repairs Task 3 — atomic typed reconciliation

- The reconciler converts typed catalog history into stable `reconcile:<instance>:<history-id>` mutations at missing priority and submits the full page once.
- Webhooks and reconciliation share one idempotent transaction helper for import, rename, delete, candidate/provenance invalidation, support-state scheduling, audit linking, and bounded event retention.
- A reconciled same-key change preserves lease owner/expiry and the greater existing priority, then coalesces one immediate rerun. Known and unknown deletes are audited; a later malformed/mismatched mutation rolls back all earlier page writes and the cursor.
- Verification: focused tests failed on the old media-only commit contract and missing mutation priority; unrestricted `go test ./internal/catalog ./internal/store -race -count=1` and `git diff --check` pass. Next task: multi-episode fail-closed dispatch and explanation.

### Conventional repairs Task 2 — typed Arr history decoding

- `Catalog.ListChangesSince` returns stable history identity, normalized import/rename/delete type, media reference, event time, and hydrated media only for live final states.
- Sonarr and Radarr sort history deterministically, ignore unrelated events, retain each file's newest relevant state before hydration, reject malformed relevant identity, and never hydrate explicit deletion tombstones.
- Sanitized contract tests cover rename/ignored-event reduction, import-then-delete, delete-then-import, missing history/file/date identity, stable ordering evidence, and request counts.
- Verification: the focused red build failed on the missing typed method; unrestricted loopback runs of `go test ./internal/catalog ./internal/app -race -count=1`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check` pass. The plan's untagged `test/e2e` command was corrected during execution because all files in that package require the `e2e` build tag. Next task: atomic typed reconciliation.

### Conventional repairs Task 1 — persisted unsupported media state

- Migration `009_media_unsupported_reason.sql` adds a backward-compatible empty-default support marker. `domain.Media` and every repository media read/write path round-trip the validated `unsupported_multi_episode` reason.
- Unsupported imports complete unleased language searches immediately. An event arriving during an active lease preserves ownership and coalesces one terminal rerun instead of starting concurrent work.
- `RecordInstallation` checks current persisted support status inside its transaction, preventing stale in-flight work from committing managed provenance after Sonarr marks a file unsupported.
- Verification: focused red tests failed on the missing domain/schema contract; `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -race -count=1` and `git diff --check` pass. Next task: typed Arr history decoding.

### Follow-up — conventional review repair design

- The approved design is `docs/superpowers/specs/2026-09-04-conventional-review-repairs-design.md`.
- Reconciliation will consume typed Arr history mutations, coalesce each file to its latest relevant state before hydration, and apply imports, renames, deletes, audit rows, and the cursor transactionally.
- Reconciled scheduling will preserve active lease ownership and coalesce one rerun. Sonarr multi-episode files will be indexed as `unsupported_multi_episode` and make no provider or LAPSE calls.
- Shutdown will use a graceful deadline followed by cancellation and one final bounded deadline. Installation rollback will return joined restoration failures instead of silently leaving an untracked sidecar.
- Full snapshots, combined-episode subtitle support, scoring/provider changes, and automatic takeover of rollback orphans remain out of scope.
- Verification for this design boundary: `git diff --check`. Next task: user review, then a test-driven implementation plan.

### Follow-up — conventional review repair implementation plan

- The implementation plan is `docs/superpowers/plans/2026-09-04-conventional-review-repairs.md`.
- Seven test-driven commit boundaries cover persisted unsupported status, typed Arr history decoding, atomic reconciliation and lease preservation, worker fail-closed dispatch, bounded shutdown, installation rollback errors, and final integration/documentation.
- The plan retains existing external APIs, provider/scoring/LAPSE behavior, SQLite durability, and structured redaction rules. Push, image publication, and Hades deployment remain explicitly separate.
- Plan self-review covers every approved design requirement with named interfaces, literal fixtures/outcomes, red-failure expectations, race verification, and resumable ledger updates.
- Verification for this planning boundary: placeholder scan and `git diff --check`. Next task: execute the plan using the user's selected workflow.

### Follow-up — structured Loki logging design

- Commit `5205f87` records the approved architecture for comprehensive newline-delimited JSON logs collected by Grafana Alloy and queried in Loki. The application owns stable event names, bounded common fields, severity policy, component ownership, central redaction, and correlation; it does not push to Loki directly.
- Normal successful health/readiness probes remain silent. Candidate-by-candidate scoring, release/file evidence, cache decisions, and proven root-relative paths are debug-only. Absolute paths, secrets, query strings, provider bodies/URLs, subtitle content, and raw LAPSE output are forbidden at every level.
- `logging.level` with a `SUBSYNCD_LOG_LEVEL` override defaults to `info` and requires restart. Only bounded `service`, `environment`, `level`, `component`, and `event` values are recommended Loki labels; dynamic identifiers remain JSON fields.
- Metrics, OpenTelemetry, direct Loki transport, runtime level reload, and behavior changes are explicitly out of scope. The implementation plan is `docs/superpowers/plans/2026-09-04-structured-loki-logging.md`.
- Verification for the design boundary: `git diff --check` passed. Next task: approve and execute the test-driven implementation plan.

### Follow-up — priority-aware continuous search dispatch

- Commits: `241b067 feat: prioritize persisted subtitle searches`, `6eca35b feat: classify search lifecycle priorities`, `f01ec7b feat: configure workflow concurrency`, `f7e85f7 feat: wake workers after catalog changes`, and `24f6a7c feat: continuously refill search workers`.
- Migration `008_search_priorities.sql` preserves existing rows as missing-priority work and adds durable priority plus same-key rerun state. Leasing is strict import (300), missing/rejected/reconciliation (200), then successful nonexact upgrade (100), followed by due time and stable identity. Technical failures and throttles retain priority.
- Import/rename arriving during an active lease preserves its owner and coalesces any number of arrivals into one immediately due import-priority rerun. The stale attempt's completion cannot overwrite that event schedule. A second completion without another event ends normally.
- `worker.max_concurrent` defaults to one, accepts 1–8, and controls media workflows. Daemon mode leases only free slots and refills on startup, a capacity-one nonblocking webhook/reconciliation wake, each completion, and the jittered recovery poll. `RunOnce` retains deterministic bounded-batch behavior. Reconciliation and notifications remain independently maintained.
- `explain` now exposes human-readable priority and whether a same-key rerun is pending. Unit and tagged end-to-end coverage proves migration compatibility, strict ordering, priority lifecycle, wake coalescing after partial webhook success, immediate free-slot fill, completion refill, lost-wake recovery, concurrency bounds, bounded shutdown, and persisted webhook-to-daemon dispatch.
- Verification through this boundary used race-enabled storage, schedule, worker, catalog, config, and application suites plus the focused tagged end-to-end webhook wake test. The final repository-wide gate is deferred until the LAPSE tournament is implemented in the same approved execution sequence.

### Follow-up — LAPSE score-tier tournament

- Commits: `cc7a24d feat: stop lapse after a winning score tier`, `ae78f05 feat: rank lapse score ties by confidence`, `87b4e66 feat: fall back across lapse score tiers`, and `ea3c07d test: protect lapse tournament invariants`.
- The top-three eligible cap is now lazy. Candidates are partitioned by descending release score, and the next tier is not downloaded until every viable candidate in the current tier fails. A unique successful leader therefore runs one download, one LAPSE analysis, and one synchronization before structurally skipping lower scores.
- Equal-score candidates are all analyzed before finalization. Solid analyses rank by confidence, configured provider priority, rating, provider ID, and result ID. Only the best is synchronized; a synchronization failure falls through already analyzed ties without repeating analysis, then opens the next score tier if necessary. Score and exact bypasses retain zero analysis confidence and invoke neither LAPSE method.
- Deterministic LAPSE/content/pack failures retain the existing exact candidate/media/tool quarantine. Process, protocol, filesystem, provider, network, and cancellation failures never create rejection records. Cancellation is checked before downloads, analysis, finalization, and tier transitions. Temporary workspaces are removed on every exit path.
- The prior production baselines remain the comparison evidence: Arrival Croatian completed in 12m11s, while 1917 Croatian took 24m55s and six LAPSE passes over about 22.1 GB. A distinct-score leader should now require two passes; tied top scores still require one analysis per tie plus one winning synchronization.
- Race-enabled workflow/syncer tests cover unique-leader early stopping, confidence ties and deterministic ordering, within-tier and lower-tier fallback, transient error isolation, cancellation, lazy season-pack extraction, bypass/upgrade invariants, and workspace cleanup. Tagged end-to-end coverage asserts the broad single-candidate path performs exactly one analysis plus one synchronization.

### Task 1 — domain and configuration

- Commit: `679035f feat: bootstrap subsyncd domain and configuration`
- Implemented canonical BCP 47 languages with explicit legacy aliases, strict single-document YAML, whole-scalar environment expansion, provider/language routing validation, path/root checks, and the approved example configuration.
- Verified with configuration/domain tests and `go vet ./...`.

### Task 2 — SQLite and schedules

- Commit: `6924360 feat: add persistent scheduling and provenance store`
- Implemented transactional embedded migrations, WAL/foreign-key/busy-timeout SQLite setup, typed media/inventory/search/provider/pack/installation persistence, recoverable leases, deterministic missing/failure/upgrade schedules, and an injected test clock.
- Important behavior: provider state is keyed by provider instance plus operation scope; pack eviction returns expired entries before live LRU entries; installation audit and provenance commit atomically.
- Verified with race-enabled `internal/store`, `internal/schedule`, and `internal/testutil` tests plus `go vet ./...`.

### Task 3 — Arr catalogs, webhooks, and path mapping

- Commit: `b047df2 feat: integrate Sonarr and Radarr catalogs`

- Webhook IDs are stable hashes of configured instance, normalized Arr event name, media kind, Arr file ID, size, path/previous-path, release evidence, and upgrade flag. Exact duplicate deliveries are database no-ops while later renames of the same file ID remain distinct.
- Imports and renames hydrate the authoritative file/episode-or-movie/series records from Arr before persistence. Deletes do not require a now-missing remote file resource.
- Content provenance invalidation compares Arr file ID, byte size, and nanosecond timestamp. A path-only rename retains candidate provenance while updating the path.
- Import/upgrade resets the missing schedule for every configured language. Delete events release/cancel pending jobs and remain in a bounded 10,000-row audit log.
- Reconciliation applies the full hydrated history page and cursor in one SQLite transaction, preventing a cursor gap after a partial failure.
- Path mapping uses the longest boundary-aware remote prefix, accepts both slash styles, preserves case, rejects traversal, and resolves existing parent symlinks before accepting media-root containment.
- Arr errors include only instance and status; untrusted upstream response bodies and API keys are never surfaced. Default client timeout is 15 seconds.
- Ordinary tests use sanitized fixture JSON and loopback fake servers only.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed. The full run exposed and retained a regression for SQLite partial unique indexes; targetless `ON CONFLICT DO NOTHING` is required for the partial `events(event_id)` index.

## Known follow-ups

- Reconciliation currently hydrates history rows that still identify a live file. Deletions are handled by delete webhooks; if an Arr history endpoint exposes reliable deletion tombstones, add them behind a contract fixture before changing this rule.

### Task 7 — Titlovi adapter

- Commit: `b803647 feat: add Titlovi provider`

- Strict provider configuration requires credentials, defaults to the HTTPS Kodi API and Titlovi download origin, and permits alternate origins only through an explicit host allowlist. Local HTTP is available solely through an unexported YAML test flag.
- Tokens and user IDs are cached until one minute before expiry. Search/download permit one 401-triggered refresh; common transport keeps credential-bearing URLs out of errors and persists no-header 429 responses as a five-minute provider-instance cooldown.
- The broad-only adapter maps every supported Titlovi language independently (`bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl`, `sl`), paginates to a configured ceiling, and retains title, release, rating, download count, and opaque result/download identity.
- Episode-zero results become season-pack evidence only when the returned season matches the target. They retain episode zero and never masquerade as a direct episode; wrong-season and wrong-episode rows are rejected.
- Downloads accept only configured HTTPS origins and redirects, stream through a compressed-byte ceiling, and leave ZIP/RAR member selection to the later common fail-closed extractor.
- Titlovi handling covers the token contract, provider language names, pagination, episode-zero pack signals, typed 429 responses, explicit pack scope, wrong-season rejection, bounded pages/bytes, host-safe redirects, and extraction outside the provider adapter.
- Verification: `go test ./... -race`, `go vet ./internal/provider/titlovi`, and scoped `git diff --check` passed.

### Task 8 — SubDL adapter

- Commit: `cd2afdf feat: add SubDL provider`

- Strict configuration requires an API key, defaults to the official HTTPS search and download origins, and uses an explicit download-host allowlist. The provider is broad-only and advertises season-pack plus direct-member capabilities.
- Search sends the strongest available IMDb/TMDB ID, original filename or title fallback, media type/year/language, release/HI/unpack/full-season flags, 30-result page size, and `client=custom_integration`.
- Episode searches run standard season/episode, optional absolute episode, and season-only variants; a title-only query runs only when all filtered variants are empty. Stable URL/name identities deduplicate the merged response.
- Provider media-result identity is retained for hard gating and scoring. All release names survive normalization. Explicit and release-text episode ranges must contain the standard or absolute target; direct unpack members are preferred, explicit full seasons become `PackSeason`, and unproven episode-zero rows fail closed.
- Search and download understand daily quota, rate-limit, and service-busy payloads without sleeping. Cooldowns persist by provider instance and operation. Downloads attach the API key only to an allowlisted origin, reject unsafe redirects, and stream through the compressed-byte ceiling.
- Current official contract was checked at <https://subdl.com/api-doc>. We retain the official `full_season`, `unpack_files`, `client`, and optional authenticated-download behavior while rejecting arbitrary archive-member fallback.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — score/evidence-gated LAPSE

- Commit: `a8887ef feat: gate LAPSE by release confidence`
- The default `sync.policy: confidence` avoids a LAPSE media read for a non-pack first install only when the internally computed release score is at least 75 and contains an identity anchor (`external_id` or `title_year`) plus `release_group`; TV also requires direct, absolute, or parsed release-name episode evidence. Provider rating/popularity points cannot replace the anchors.
- Exact hashes retain the existing `exact_hash` bypass. Confidence-qualified installs persist `SyncResult{Verdict: "score_bypass", Mode: "bypass", Reference: "release_evidence"}` with zero LAPSE metrics so `explain` remains honest about what was and was not measured.
- Candidates below threshold or missing required evidence still run LAPSE. Packs and managed-subtitle upgrades run LAPSE by default regardless of score. `sync.policy: always` restores the previous every-nonexact behavior; the threshold and evidence/pack/upgrade guards are configurable, default true, and strict YAML validated.
- LAPSE remains installed and capability-checked because any uncertain candidate can need it. Its persistent speech profile cache remains under `<data_dir>/lapse-cache`; on network media storage, the first required analysis may read much or nearly all of the media file, while a score bypass performs no LAPSE media read.
- The existing broad-search black-box test explicitly selects `policy: always`, preserving coverage of real LAPSE analysis/synchronization. Workflow tests cover the score bypass, adjustable threshold, `always` mode, missing anchors, TV ambiguity, packs, and upgrades. Configuration tests cover defaults, explicit switches, invalid policy, and invalid thresholds.
- Verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race`, `go vet ./...`, tagged e2e tests, and `git diff --check` passed on 2026-09-04.

### Follow-up — persistent candidate rejection quarantine

- Commit: `42943d3 feat: quarantine rejected subtitle candidates`
- Migration `007_candidate_rejections.sql` adds a cascade-owned rejection ledger keyed by media/language/provider result, stable candidate signature, optional artifact checksum, and tool/policy signature. Each row retains the exact media fingerprint, reason code, rejection time, and 30-day expiry.
- Candidate signatures deliberately omit download references, ratings, popularity, and download counts so volatile provider data cannot trigger another download. Release/identity/pack evidence changes do invalidate the signature. Media fingerprint, known member checksum, LAPSE compatibility version, synchronization policy, expiry, or a manual clear also invalidate the applicable rejection match.
- LAPSE `unsure` and `nothing`, invalid or oversized subtitle payloads, and ambiguous pack selection are deterministic rejections. They are persisted immediately, removed before the top-three shortlist, and allow later-ranked results to advance on later jobs. Cached pack members are checked with their checksum; rejecting one episode does not remove the pack or affect another episode/media row.
- Process timeouts/crashes, malformed LAPSE protocol, filesystem errors, provider/network failures, and cancellation never create rejection records. A shortlist containing only such failures returns an error so the worker uses technical-failure backoff. No-speech remains a media-validation rejection and does not poison an individual candidate.
- `explain` lists active rejection reason, artifact checksum, and expiry. `search --retry-rejected` clears only the selected media/language ledger before the manual workflow; deterministic failures during that run are reinserted.
- Red/green tests cover SQLite match/expiry/clear semantics, media/tool/artifact invalidation, top-three promotion, stable candidate signatures, invalid payload quarantine, cached-pack suppression, transient LAPSE classification, CLI forwarding, manual clearing, explain output, and the exported LAPSE compatibility version. Verification passed with the complete race suite, `go vet ./...`, tagged e2e tests, and `git diff --check` on 2026-09-04.

### Task 9 — candidate matching and scoring

- Commit: `dd09fe8 feat: add explainable subtitle matching`

- `github.com/chill-institute/torrentname` v1.4.1 is pinned behind the local release parser. The wrapper preserves raw evidence and normalizes title, season/episode/range, group, source, resolution, complete-season state, edition/cut, and common streaming-service tokens.
- Identity comparison is punctuation/case normalized but never fuzzy. Known language, media kind, external-ID, movie-year, season/episode, pack-season, and edition conflicts reject before scoring; unknown metadata remains neutral. Standard and absolute pack ranges are accepted only when they contain the target.
- Exact hash is terminal at 100 after identity gates. Non-hash contributions are emitted in stable order for external ID (20), title/year (15), release group (25), source (15), edition (10), service (5), resolution (5), rating (0–3), and popularity (0–2), capped at 100.
- `Eligible` applies the configured threshold independently from evaluation. `Rank` deterministically orders by score, provider priority, normalized rating, normalized popularity, then stable provider/result identity.
- Provider download counts now share a logarithmic `[0,1]` normalization saturating at four orders of magnitude. This prevents popularity from overpowering identity while making its two score points usable by OpenSubtitles, Titlovi, and SubDL.
- Scoring uses small known release-group equivalence sets with independently authored tests, retains explicit zero-point explanations, and rejects wrong-season pack metadata even when candidate top-level season is absent.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 10 — safe extraction and reusable season packs

- Commit: `8ff4405 feat: add safe reusable season-pack handling`

- One common extractor handles ZIP, RAR, and plain `.srt`, `.ass`, `.ssa`, or `.vtt` payloads. Defaults cap input at 20 MiB compressed, 100 MiB expanded, 100 files, and one directory level. Every archive member counts toward limits, including ignored extensions.
- Extraction rejects Unix and Windows absolute paths, traversal, symlinks and other special files, duplicate case-insensitive names, nested archives by extension or ZIP/RAR magic, excessive expansion, NUL/binary payloads, unsupported syntax, more than 100,000 cues, and negative or non-monotonic timestamps. Text is normalized to UTF-8/LF, accepting UTF-8 and Windows-1250 input, and is published from a same-parent staging directory.
- Subtitle syntax validation uses `github.com/asticode/go-astisub` v0.42.0. RAR streaming uses `github.com/nwaples/rardecode/v2` v2.4.1. ZIP, plain, and hostile archive branches have independent fixtures; the RAR decoder integration is compiled and bounded but still needs a provenance-safe valid RAR fixture in the later black-box suite.
- Member selection is fail-closed and ordered: provider direct member, unique `SxxEyy`/`xxXyy`, containing episode range, absolute `EP`/`ABS` token, then unique normalized episode-title similarity of at least 0.98 for titles longer than four characters. Range starts are not misclassified as single episodes, repeated direct evidence is deduplicated, and forced members are excluded from normal requests.
- Sonarr episode titles now hydrate into `domain.Media` and migration `004_episode_titles.sql` persists them. Both direct and webhook-transaction media upserts carry the field.
- The cache key hashes provider, result, language, and download checksum. Normalized members live below an exact 64-hex content directory with checksums and an SQLite manifest. Raw candidate and direct-member download references are scrubbed before persistence.
- Cache publication is immutable and rolls back if SQLite rejects the entry. Existing destinations must be real directories containing real regular files; cache reads reject symlinks and verify checksums. Expired entries are removed before live LRU entries, eviction renames to a tombstone before database deletion, and orphan cleanup is root-contained. The cache assumes one service process owns its configured cache root.
- Reuse matches any known TVDB, TMDB, IMDb, or title/year identity so a provider result keyed by one external ID can serve media carrying stronger additional IDs. Cached members remain subject to later matching/scoring and LAPSE validation.
- Pack handling includes format filtering, numbered/ranged evidence, universal bounded extraction, no arbitrary single-file fallback, strict ambiguity rejection, content addressing, raw-link scrubbing, checksum validation, symlink-safe reuse, and rollback across filesystem/database publication.
- Verification: focused red/green regressions plus `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 11 — LAPSE synchronization

- Commit: `d37f7e1 feat: integrate LAPSE subtitle synchronization`

- The compatibility baseline is official LAPSE v2.0.5 at tag commit `8d57e43`; current upstream HEAD inspected during implementation was `a0e99bd919e80f9f836cb4c6f6cc391c1a666dd6`. The CLI/JSON contract was verified from <https://github.com/Schwponaco-org/lapse>.
- Analysis runs LAPSE with the media path plus a private copy of the candidate and `--dry-run --json --strict --no-sidecar`. The original candidate is never supplied as LAPSE's writable input. LAPSE dry-run legitimately reports `written:true` as “would write,” so trust comes from the `solid` verdict and validated protocol, not file existence during analysis.
- Synchronization runs only to a caller-provided absent output using `--output PATH --no-backup --json --strict --no-sidecar`. It accepts only exit-0 `solid`, `written:true`, the exact reported output path, a real regular nonempty file, a supported text extension, valid UTF-8, parseable cues, and nonnegative monotonic timestamps.
- JSON decoding requires the complete v2.0.5 field set, rejects unknown/trailing fields, validates the documented modes and `vad`/`embedded`/`subtitle` references, requires finite bounded metrics, and checks cue/part/split consistency. A future LAPSE protocol addition intentionally fails closed until fixtures and the allowlist are reviewed.
- Strict `unsure` and `nothing` reports with exits 2/3 become typed `VerdictError` rejections. Early no-speech/no-audio failures without JSON become typed `NoSpeechError`. Nonzero failures, malformed/truncated JSON, missing output, invalid output, timeout, and protocol contradictions are rejected.
- Exact-hash candidate entry points return an `exact_hash` bypass result without invoking either LAPSE command. Task 12 must call these candidate-aware entry points rather than calling raw analysis/synchronization for exact results.
- The subprocess runner never uses a shell, caps stdout at 1 MiB and stderr at 64 KiB, replaces inherited `LAPSE_CACHE` with the configured persistent speech-cache directory, applies separate analysis/synchronization contexts, redacts media and temporary paths from surfaced errors, and kills the entire process group on cancellation.
- Verification: sanitized v2.0.5 JSON fixtures and fake runners cover solid/weak/nothing, unsafe metrics, malformed/trailing/oversized output, no speech, nonzero exits, timeouts, path redaction, output validation, exact-hash bypass, environment replacement, output caps, exit preservation, and descendant-process cancellation. `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 12 — workflow orchestration and atomic installation

- Commit: `fce31ab feat: orchestrate subtitle acquisition and upgrades`

- The workflow refreshes embedded/sidecar inventory before every attempt, stops for acceptable embedded or protected/user-managed subtitles, checks a reusable season pack before remote providers, and otherwise consumes the provider coordinator's exact-first/broad-fan-out result. A stale exact-phase error is cleared when that provider's broad phase succeeds.
- Every search result is identity-gated, explainably scored, and transactionally persisted without raw candidate or direct-member download references. Disallowed hearing-impaired candidates carry a persisted rejection reason. Task 12 originally defaulted `allow_hearing_impaired` to `true`; follow-up `5a42290` supersedes that policy, so the current default is `false` and only explicit `true` opts in.
- Exact hashes bypass LAPSE and reduce the download shortlist to that terminal result. Nonexact acquisition analyzes and synchronizes at most the top three eligible candidates. Final ordering is release score, LAPSE analysis confidence, configured provider priority, rating, then stable provider/result ID.
- Provider output is capped at 20 MiB at the workflow writer boundary even if an adapter ignores a short-write error, then sent through the common ZIP/RAR/plain extractor and strict episode selector. Ambiguous packs fail closed. Normalized season packs are cached only after successful selection; cache persistence failure is recorded but does not discard a valid candidate.
- Cache reuse now reloads the immutable manifest and reruns strict selection for the requested episode instead of trusting the first indexed member. Missing, malformed, symlinked, or checksum-mismatched entries are invalidated through root-contained cleanup. Ambiguity remains a typed rejection rather than a cache miss.
- Existing exact-hash installations are terminal. Managed nonexact files require a default 10-point improvement and schedule another upgrade check after 7, 30, or 90 days according to score. Media-content changes invalidate installed score, LAPSE result, and fingerprint provenance while retaining the old sidecar; path-only renames rebase managed sidecar/rollback paths and retain valid provenance.
- Installation validates a regular UTF-8 SRT/ASS/SSA/VTT source, cue order, and duration bounds; stages it beside a root-contained destination; fsyncs content; applies configured mode/ownership; rechecks ownership immediately before rename; atomically publishes and fsyncs the directory; and commits checksum, provider, candidate, score, sync result, and media fingerprint. A managed replacement keeps a rollback copy and restores it after any post-rename failure. User-modified, unmanaged, special, and symlink destinations are protected.
- Remote cooldown/quota/disabled results become a nonblocking throttled outcome with the earliest known reset. A partial provider outage can still yield a normal no-result or successful installation; a generic failure from every assigned provider returns a technical workflow error. Manual requests still honor provider cooldown and protected-file rules.
- Verification: focused red/green tests cover inventory ownership/HI policy, cached-pack fallback, exact bypass, three-candidate limit, confidence tie-breaks, outages, throttles, manual search, cancellation, ambiguity, LAPSE rejection, score upgrades, secret-free persistence, download limits, cache-write degradation, installation fault restoration, fingerprint invalidation, and rename rebasing. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 13 — durable workers and Silo notifications

- Commit: `f50b76c feat: add durable worker and Silo notifications`

- Search leasing now carries independent missing-result and technical-failure attempt indexes. SQLite compare-and-swap renewal/completion APIs prevent an old job owner from renewing or completing a lease reclaimed by another worker. Missing/rejected work advances only the jittered missing schedule; technical errors advance only the 1m/5m/15m/60m failure schedule; successful work resets both; provider throttles release immediately and reschedule at the reported reset plus up to 10% positive jitter without advancing either counter.
- Each cycle leases no more than 10 searches for five minutes, starts renewal for every claimed job (including jobs queued behind the semaphore), and runs no more than two media/language workflows concurrently. Renewal stops and joins before completion, closing the renewal-versus-completion race. Polling uses ±10% jitter. Graceful shutdown stops polling, allows a configurable drain, then cancels active work so unfinished leases remain recoverable.
- Multiple workers sharing SQLite cannot process the same lease. A crash/failure after a committed installation but before search completion leaves the lease recoverable; the next inventory pass sees the installed sidecar and does not perform a second installation. Repository media hydration now supplies the complete persisted `domain.Media` to workflow jobs.
- Per-instance reconcilers run immediately at startup and then every six hours. Each `catalog.Reconciler` continues to read and atomically advance its own persisted SQLite cursor, so a failed instance is retried independently while successful instances retain their progress.
- Migration `006_notification_leases.sql` adds partial unique notification deduplication plus a due-work index. Notification enqueue, lease, renewal, retry, and terminal completion are persistent. A committed installation enqueues one checksum-derived job per configured notifier before the search lease completes; deliveries run independently with bounded concurrency. Retryable failures use the technical backoff and never change acquisition state or revert a subtitle.
- The optional Silo adapter follows the documented current pre-1.0 native contract: `POST /api/v1/scan`, `Authorization: Bearer …`, one mapped parent-directory `path`, and 2xx success (Silo documents 202). Selecting the directory after boundary-aware longest-prefix rewriting makes Silo perform a subtree scan and discover newly installed sibling subtitles. It rejects redirects and credential-bearing/invalid base URLs, uses a 15-second default timeout, treats timeout/408/429/5xx as retryable, and never includes the API key or response body in errors. Container-network examples target the live Hades native listener on port 8080. See `docs/references/silo.md`.
- Official Silo documentation is authoritative. The worker design independently tightens the nonblocking cooldown and durable-lease requirements from the approved service design.
- Verification: race-enabled tests cover renewal, completion ordering, two-worker exclusion, two-workflow concurrency, crash recovery, missing/failure/throttle accounting, poll/reset jitter, six-hour reconciliation, bounded shutdown, notification dedupe/retry isolation, disabled Silo, request contract, path mapping, authentication/status classification, timeout, redirect rejection, and secret redaction. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — native Silo scan API and focused README

- Commit: `4a394b1 feat: use Silo native scan API`
- Replaced the Jellyfin-compatibility notification with Silo's current native `POST /api/v1/scan` contract on the main listener: bearer admin API key, JSON parent-directory `path`, and accepted 2xx response. Durable retry isolation, path mapping, redirect rejection, and credential redaction are unchanged.
- Reworked `README.md` around a general feature list. Arr webhook connections remain deliberately manual; `subsyncd` neither creates nor modifies Sonarr/Radarr settings, while startup and six-hour reconciliation cover missed deliveries.
- Silo's API-v2 program plans a dual-API bridge followed by a Silo 1.0 `/api/v1` tombstone. The current migration ledger says the scan operation will be ported but leaves its v2 method, path, and operation ID unset. Do not guess or automatically replay the mutating request across versions; add an explicit versioned adapter with contract tests once Silo publishes the route and schema.
- Verification on 2026-09-04: `go test ./internal/notifier -race -v`, `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -v`, and `git diff --check` passed. The end-to-end flow asserts the native route, bearer header, mapped media path, accepted response, durable deduplication, and restart behavior.
- Next task: monitor Silo API-v2 issue #135, cutover issue #886, and migration-ledger PR #902; implement v2 only after the scan contract is assigned and published.

### Follow-up — configurable rootless container identity

- Commit: `ce023d5 feat: make container identity configurable`
- The image's baked-in unprivileged account and owned runtime directories now default to UID/GID `1000:1000`. Both Compose definitions interpolate host-side `PUID` and `PGID` into `user:` and `/tmp` tmpfs ownership, also defaulting to `1000:1000`.
- This deliberately is not a LinuxServer-style root entrypoint: the service does not read `PUID`/`PGID` from its container environment, create users, retain `CHOWN`/`SETUID` capabilities, or mutate bind-mount ownership. Operators set the values in `.env` or the invoking shell and must prepare `/data` and media permissions for that numeric identity.
- Compose passes `TZ`, defaulting to `Europe/Zagreb`; the runtime image explicitly installs timezone data. Arbitrary numeric identities do not require passwd/home lookup because `subsyncd` uses explicit configured paths and LAPSE cache locations.
- Red/green Compose checks proved that standalone and root-stack definitions previously ignored `PUID=1234 PGID=2345 TZ=UTC`, then rendered `user: 1234:2345`, matching tmpfs ownership, and `TZ=UTC` after the change. Default renders were separately checked as `1000:1000` and `Europe/Zagreb`.
- Verification on 2026-09-04: native arm64 image build; image metadata and runtime UID/GID checks; Zagreb timezone offset; default and arbitrary-identity `subsyncd --version` smoke runs; `go test ./... -race`; `go vet ./...`; tagged e2e tests; and `git diff --check`. The local image ID is recorded in `docs/release-notes.md`; it is not a registry digest.
- Next task: none. Before publication, retain the existing native-amd64 build gate from the initial release notes.

### Follow-up — hearing-impaired subtitles default off

- Commit: `5a42290 fix: default hearing-impaired subtitles off`
- Omitted `allow_hearing_impaired` now resolves to `false`; the shipped example also sets it to `false`. Explicit `allow_hearing_impaired: true` remains the opt-in for users who want SDH/HI tracks and candidates.
- With the default policy, an embedded SDH track does not satisfy an ordinary language request and a provider result marked hearing-impaired is rejected during candidate evaluation. Explicit opt-in restores both behaviors. Forced-only and unknown-language behavior is unchanged.
- Red/green coverage changed the configuration contract test before production code: it failed because omission still yielded `true`, then passed after the default changed while also proving explicit `true` is honored. Existing inventory and workflow policy tests passed unchanged.
- Verification on 2026-09-04: focused configuration/workflow/inventory race tests, complete `go test ./... -race -count=1`, `go vet ./...`, tagged e2e tests with `-count=1`, and `git diff --check` all passed.
- Next task: none.

### Follow-up — absolute missing-subtitle milestones

- Commit: `acfc801 fix: use absolute missing-subtitle milestones`
- Missing-language searches now reach the intended elapsed milestones from import or schedule reset: immediately, 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, then every 14 days. The scheduler stores the adjacent intervals `30m`, `90m`, `6h`, `16h`, `48h`, `96h`, `168h`, then `336h`, preventing the former cumulative drift to 2h30m, 10h30m, 1d10h30m, and later dates.
- The existing ±10% interval jitter, technical-failure backoff, provider cooldowns, upgrade schedule, and six-hour normalized search cache are unchanged. Because cached empty results suppress provider access, the 30-minute and 2-hour jobs normally refresh only local sidecars; continuously missing subtitles normally generate provider searches around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days.
- A cumulative-elapsed-time regression test failed against the former delay table and passed after the interval correction. The scheduler test also proves that attempt two is scheduled 90 minutes after the preceding attempt.
- Verification on 2026-09-04: `go test ./internal/schedule -race -count=1 -v`, complete `go test ./... -race -count=1`, `go vet ./...`, tagged e2e tests with `-race -count=1`, and `git diff --check` passed. The complete and e2e suites required loopback permission solely for their local `httptest` servers.
- Next task: none.

### Task 14 — daemon, webhook API, and operational CLI

- Commit: `9d0d06a feat: expose subtitle daemon and CLI`

- `internal/app` now assembles strict configuration, media-root checks, SQLite/migrations, Arr catalogs and persisted instance rows, compiled-in providers, per-language ordered coordinators/workflows, embedded inventory, immutable pack cache, LAPSE, atomic installer, optional Silo notification, reconcilers, durable worker, and the HTTP handler. Assembly performs no Arr/provider/Silo network request, so those dependencies can be unavailable without blocking startup/readiness.
- Startup fails before serving when configuration/listen addresses are invalid, media roots do not exist as directories, SQLite cannot open/migrate, `ffprobe -version` fails, or LAPSE does not advertise `--json`, `--strict`, `--output`, `--no-sidecar`, and cache support (`--no-cache`). The pinned v2.0.5 executable prints usage to stderr and returns nonzero when invoked without arguments; it does not implement `--help` and treats that token as a media filename. A complete advertised capability set is therefore accepted regardless of the no-argument exit status; empty nonzero output fails. See <https://github.com/Schwponaco-org/lapse/tree/v2.0.5>.
- `serve` runs the worker and a time-bounded `net/http` server together. SIGINT/SIGTERM cancel the shared context; HTTP drains first with a 30-second ceiling and force-closes on timeout, then active worker work receives cancellation and recoverable leases remain in SQLite. Mutating processes are excluded by a nonblocking advisory lock at `<data_dir>/subsyncd.lock`.
- The HTTP surface is limited to `POST /webhooks/{instance}?token=...`, `GET /healthz`, and `GET /readyz`. Tokens are SHA-256-normalized before constant-time comparison; exactly one token is required. Bodies are capped at 1 MiB and must contain one complete JSON object. Unknown instances, invalid tokens, unsupported/missing event identity, test events, oversized input, and dependency failure have explicit generic status responses. Every response receives a bounded request ID, and structured logs contain the path without query strings or error bodies.
- Readiness rechecks only SQLite and local media roots. It never probes Arr, subtitle providers, or Silo. Arr non-2xx errors now omit untrusted response bodies from logs/CLI output. Configuration-aware CLI error redaction removes Arr/webhook/Silo/provider credentials and configured media-root prefixes.
- Webhook deduplication was tightened: exact redeliveries remain transactionally harmless, but stable IDs now include file/path/release/upgrade evidence so a later rename of the same Arr file ID is not suppressed. Catalog normalization exposes a typed invalid-webhook error for correct HTTP classification.
- The stable CLI surface is `serve`, `scan`, `search`, `retry`, `explain`, `doctor`, and `analyze-sync`, with exit codes 0/1/2 for success/operation failure/usage. Configuration defaults to `/config/config.yaml`, honors `SUBSYNCD_CONFIG`, and can be overridden by `--config` on every command. Flags unrelated to a command are rejected.
- `scan` reconciles one configured instance and can force-refresh embedded tracks for every indexed file. `search` hydrates current Arr metadata and calls the same protected-file/scoring/LAPSE/install workflow used by workers. `retry` clears every persisted throttle/auth scope for one configured provider. `explain` reports indexed identity, embedded/sidecar inventory, scheduling attempts/outcome, candidate score and identity evidence, installation/LAPSE provenance, reusable packs, and provider cooldowns. `analyze-sync` accepts only media inside configured roots, calls LAPSE analysis mode, and never installs output.
- The example configuration no longer advertises nonexistent `fallback_cooldowns` YAML; the documented provider-specific fallback values remain compiled policy in `provider.FallbackReset`.
- Verification: focused red/green tests cover HTTP authentication/body/JSON/status/readiness/request-ID/query-redaction behavior; CLI golden output, strict flags, exit codes, and backend redaction; real compiled provider assembly without network calls; startup diagnostics; local readiness; advisory-lock exclusion; and HTTP/worker drain. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 15 — packaging, end-to-end verification, and operator documentation

- Commit: `5619812 docs: package and document subsyncd`
- The multi-stage production image builds a static Go 1.27.0 service and installs checksum-pinned LAPSE v2.0.5 assets for Linux amd64/arm64. Debian 13.2 is intentional: the upstream LAPSE executable needs glibc 2.38+, which the initially tested Debian 12 runtime did not provide. The final runtime includes FFmpeg/FFprobe, runs as fixed UID/GID 10001, and declares a readiness health check.
- LAPSE capability discovery was corrected against the packaged executable: v2.0.5 exposes usage only on an empty invocation and interprets `--help` as a media filename. Startup now checks the real no-argument usage contract. The v2.0.5 release archive's executable reports internal version `2.0.0`; both values are recorded rather than conflated.
- Compose examples use a read-only root filesystem, read-only configuration, writable data/media mounts, bounded tmpfs, dropped capabilities, no-new-privileges, init, a 45-second stop grace period, resource limits, health checks, external media networking, and credential environment variables with no embedded secrets. The root stack keeps the service behind the opt-in `subsyncd` profile.
- Installation mode defaults to `0644` and accepts a nonzero octal mode without execute bits; optional nonnegative UID/GID ownership is applied by the atomic installer. The shipped example config has its own load test.
- Tagged black-box tests cover embedded-language provider avoidance, OpenSubtitles exact-hash terminal installation without broad search/LAPSE, broad search plus LAPSE plus atomic sidecar plus Silo, and restart/webhook deduplication without redownload or renotification. Credential-gated OpenSubtitles/SubDL/Titlovi contracts are excluded from ordinary CI and skip independently without credentials.
- The workflow now always replaces catalog fingerprint evidence with the freshly statted inventory fingerprint before hash-capable provider search. Hash calculation and persistence therefore use the authoritative current path, Arr file ID, size, and nanosecond mtime rather than a potentially stale catalog timestamp.
- `README.md`, `docs/providers.md`, and `docs/operations.md` document language routing, provider constraints, hash/embedded caches, live sidecars, exact/broad search, scoring, packs, throttling, backoffs, upgrades, LAPSE, Silo, webhooks, CLI, permissions, backup, restart, and recovery. `docs/release-notes.md` records resolved dependencies, checksums, image identity, and the remaining native-amd64 publication gate.
- Verification: `go test ./... -race`, `go vet ./...`, tagged e2e tests, provider-contract compilation/no-credential skips, all Compose renderings, native legacy-Docker build, version smoke, and real in-container `doctor` passed. The final verified local arm64 image ID is `sha256:2051a4f528c586dda1046436f3a0072612632fc8158ecc62dbbaa35ac5418384`.

### Task 4 — embedded and sidecar inventory

- Commit: `0f69f8d feat: cache embedded subtitle inventory`

- FFprobe is invoked through an injectable runner with the exact approved arguments. JSON stdout is bounded to 8 MiB; failure text does not expose captured stderr.
- Every subtitle stream is inventoried, including image codecs such as PGS. Unknown/missing languages remain visible with an empty canonical language but never satisfy a configured language.
- `Inventory.Satisfies` requires an equivalent known language and a full subtitle. Forced-only tracks never satisfy the normal-language request; SDH satisfies only when hearing-impaired subtitles are allowed.
- The cached embedded inventory is reused only when path, Arr file ID, byte size, and nanosecond modification time match. `forceProbe` always bypasses the cache.
- Sidecars are rescanned on every refresh, only for the exact media stem and `.srt`, `.ass`, `.ssa`, or `.vtt`; directory recursion and symlink following are forbidden.
- An external file is considered managed only when both normalized path and SHA-256 checksum match installation provenance. All other external tracks are protected.
- Fingerprint update and complete track replacement occur in one SQLite transaction so a crash cannot pair a new fingerprint with stale embedded rows.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 6 — OpenSubtitles adapter

- Commit: `53930be feat: add OpenSubtitles provider`
- Follow-up: `884c0fe feat: cache OpenSubtitles file hashes`

- The OpenSubtitles file hash is delegated to `github.com/opensubtitlescli/moviehash` v0.1.1 behind a local `Hasher`; independent little-endian reference tests verify the first/last 64-KiB plus size algorithm and unchanged file offset. Files below 128 KiB and non-random-access sources return a typed unsupported error.
- Strict provider YAML requires API key, username, password, and user agent. The configured base URL is injectable only for contract tests; the production default is `https://api.opensubtitles.com/api/v1`.
- API key and user agent accompany all calls. Login tokens cache until one minute before expiry. Search and download each allow exactly one immediate 401-triggered refresh; a second rejection disables only the named provider instance via persistent `auth` state.
- Exact search sends movie hash plus byte size. The hash is calculated lazily on the first exact search and persisted in SQLite with its path, Arr file ID, size, and nanosecond modification time. Later exact searches reuse it; a changed fingerprint is a cache miss, and a concurrent fingerprint change rejects the stale hash write. Broad search sends the strongest IMDb/TMDB identity, title/year, and episode coordinates. Results preserve release/file names, language, HI, rating, count, IDs, and explicit hash-match evidence; inferred season packs are forbidden.
- OpenSubtitles custom codes round-trip as `pt ↔ pt-PT`, `zh ↔ zh-CN`, and `es-MX ↔ ea`.
- A 406 response persists download-scope quota reset and returns `QuotaError`; 429 and rate headers use the common transport. Temporary links are request-local, safety-checked, and streamed through the configured size ceiling.
- Common transport errors deliberately omit upstream URLs and wrapped network text so signed queries, API keys, and tokens cannot leak through errors.
- Verification: the original task passed `go test ./... -race`, `go vet ./...`, and `git diff --check`; the hash-cache follow-up passed race-enabled tests and vet for `internal/store` and `internal/provider/opensubtitles` plus scoped `git diff --check`.

### Task 5 — provider registry, coordinator, and throttling

- Commit: `6cf5c97 feat: add provider registry and persistent throttling`

- Providers are compiled-in factories producing independently credentialed named instances. The registry validates duplicate IDs, unknown types, provider-specific strict YAML, and language capabilities before workers start.
- Exact-hash-capable providers run sequentially in configured order; the first exact result stops all further searches. If none succeeds, all providers assigned to that language receive one concurrent broad query. Results merge in configured order and failures remain provider-local.
- Normalized search results cache for six hours by provider instance, search mode, media/file fingerprint, language, episode identity, and release name. HTTP(S) download URLs are stripped before persistence; opaque provider IDs may be retained.
- Each provider instance owns a token bucket and active-request semaphore. A separate host-origin semaphore coordinates multiple accounts/types using the same upstream origin.
- Cooldowns and quotas persist by provider instance plus `all`, `search`, `download`, or `auth` scope. An exhausted future window prevents the HTTP call and returns a typed reset immediately; workers never sleep through remote cooldowns.
- Parsed response evidence includes named `RateLimit`/`RateLimit-Policy` windows, `X-RateLimit-*`, seconds/date `Retry-After`, and provider JSON resets. Past/skewed resets are ignored and the most restrictive future reset wins.
- No-header fallbacks are Titlovi rate limit 5 minutes; OpenSubtitles rate limit 1 minute and download quota 6 hours; SubDL rate limit 15 minutes, daily quota until next GMT midnight plus 15 minutes, and service busy 1 hour.
- Added `golang.org/x/time/rate` v0.15.0 for local token-bucket pacing.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — standalone GitHub publishing

- Commits: `6fc34d5 chore: prepare standalone subsyncd tree`, `f2deb60 docs: deploy subsyncd from private GHCR`, and `b054f50 ci: publish verified multiarch images`.
- The self-contained source root was published with subsystem history to private `https://github.com/tomislav/subsyncd`; its canonical local clone is `/Users/tomislav/Development/subsyncd` on default branch `main`. The associated container package is private `ghcr.io/tomislav/subsyncd`.
- The standalone root now excludes environment files, runtime configuration, SQLite state, caches, media, logs, coverage, and the compiled binary. `AGENTS.md` resolves only in-tree documentation and retains the correct default-off hearing-impaired policy.
- `compose.example.yml` pulls `ghcr.io/tomislav/subsyncd:${SUBSYNCD_IMAGE_TAG:-latest}` without changing its rootless identity, read-only filesystem, tmpfs ownership, mounts, health check, resource limits, or network. The README documents private GHCR authentication, immutable tag selection, Compose pulls, and an explicit local build path. The parent `arr-stack/docker-compose.yml` is unchanged.
- `.github/workflows/container.yml` runs race-enabled unit/package tests, `go vet`, and tagged end-to-end tests before publishing. It builds exactly `linux/amd64` and `linux/arm64`, uses the Actions cache, and emits maximum provenance plus an SBOM.
- Action pins are `actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1`, `actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`, `docker/setup-qemu-action@1f40c72289eff860ee54a304f1438e3cff362e0a`, `docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e`, `docker/login-action@dbcb813823bdd20940b903addbd779551569679f`, `docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302`, and `docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a`.
- Successful `main` pushes publish `latest` and `sha-<short-commit>`. Stable `vMAJOR.MINOR.PATCH` tags additionally publish `MAJOR.MINOR.PATCH`, `MAJOR.MINOR`, and `MAJOR`; prereleases publish only their full prerelease version and SHA. Manual runs always publish SHA and publish `latest` only when dispatched from `main`.
- Local verification on 2026-09-04 passed: credential-pattern scans of the subtree and relevant history; `go test ./... -race -count=1`; `go vet ./...`; `go test ./test/e2e -tags=e2e -race -count=1`; actionlint v1.7.12; `docker compose -f compose.example.yml config --quiet`; default, SHA-tag, and arbitrary PUID/PGID/TZ Compose renderings; a native `docker build --build-arg VERSION=prepublish -t subsyncd:prepublish .`; and `docker run --rm subsyncd:prepublish --version`, which returned `subsyncd prepublish`.
- GitHub CLI authorization was renewed through the interactive device flow without printing or committing a token. The first publication workflow, `https://github.com/tomislav/subsyncd/actions/runs/33857752027`, succeeded for head `607269e502c3c90d75403bcc82bbb497fc93b9d5`; both the `Verify` and `Publish multi-architecture image` jobs passed.
- `latest` and immutable `sha-607269e` resolve to manifest-list digest `sha256:d88884fab6e65fc4e657e3fbf0e2c5cc0c52b78d4d4f2f10c9d6438c60f4092f`. The index contains native `linux/amd64` (`sha256:e1e428b0fc1213bb993fad7281fbfa93d75ff7eedf7898702bed85dac737ff15`) and `linux/arm64` (`sha256:bd898356f1a86df98999c0b5230f8b897bf861a1fe8afe827bde5986e992712b`) images plus per-platform SBOM/provenance attestations. Runtime smoke output was `subsyncd sha-607269e`.
- GitHub reports package visibility `private` and repository association `tomislav/subsyncd`. The parent `/Users/tomislav/Development/arr-stack/docker-compose.yml` remained unchanged.

### Follow-up — Hades manual production canary

- Commit: `2535f43 docs: add Hades manual canary deployment`.
- `deploy/hades-canary` is a host-specific, manual-only trial package pinned to the known-good private image `ghcr.io/tomislav/subsyncd:sha-6f0683a`. Its Compose service is behind the `manual` profile, defaults to `doctor`, has `restart: "no"`, exposes no port, and must not be run with `scan` or `serve`.
- The configuration permits exactly four Arr media-file mappings: Radarr movie-file IDs `1440` (1917) and `1168` (Arrival), and Sonarr episode-file IDs `9864` (1883 S01E03) and `10146` (3 Body Problem S01E01). The selected library directories are writable for atomic sidecar creation; `/mnt` is separately mounted read-only with `rslave` propagation so Hades's absolute AltMount symlinks resolve. File-level remote mappings reject every other Arr file before inventory or installation even though the TV bind mounts contain complete season directories.
- Croatian routes only to Titlovi and English only to OpenSubtitles. SubDL, webhooks, daemon scheduling, and Silo notification are excluded. Provider pacing is reduced to one request every four seconds, hearing-impaired subtitles remain disallowed, the minimum release score is 50, and `sync.policy: always` deliberately exercises LAPSE for every non-hash candidate.
- A read-only FFprobe baseline on Hades established the expected matrix: 1917, Arrival, and 1883 S01E03 have full embedded English; 3 Body Problem S01E01 has embedded Croatian but only SDH English. The runbook therefore begins with three provider-free embedded skips, then uses 1883 Croatian as the first real Titlovi/LAPSE workflow before any larger-media test.
- The runbook documents private-GHCR login, rootless directory setup, Compose/doctor validation, before/after sidecar checksums, one-command-at-a-time searches, `explain` review, expected LAPSE failure semantics, network-read risk, and preservation of the isolated data directory. It contains no credentials and makes no change on Hades by itself.
- A deployment safety test fails when the image is unpinned, automatic execution is enabled, Silo/SubDL is introduced, provider routing broadens, LAPSE is not `always`, hearing-impaired subtitles are enabled, file mappings broaden beyond `.mkv`, or the root README loses the runbook link.
- Verification on 2026-09-04 passed: the safety test's missing-file red state; focused green test; `docker compose ... config --quiet` with non-secret placeholder credentials; `go test ./... -race -count=1`; `go vet ./...`; `go test -tags=e2e ./test/e2e -race -count=1`; credential-pattern scan; and `git diff --check`.
- Hades preflight was installed at `/opt/subsyncd-canary` on 2026-09-04. Existing Sonarr/Radarr API keys were copied in place without being displayed; independent unused webhook-validation tokens were generated. No reusable subtitle-provider credentials were available, so all provider fields remain explicit `REPLACE_BEFORE_SEARCH_*` placeholders. No provider search is safe until the operator replaces them.
- The private `sha-6f0683a` arm64 image was pulled using a temporary GHCR login and the host was logged out immediately. Hades reports digest `sha256:64403e0897ea5f3250dc07d4bdd8bd1b97b2a5d790f2b814bf8889ff91d1a688`; root Docker configuration contains no remaining `ghcr.io` credential.
- The first `doctor` run exposed a rootless permission boundary: `root:root 0600` protected the placeholder config from container UID/GID `1000:1000`. Commit `0de5d0c docs: fix Hades canary permissions` adds a regression assertion and documents the resolved split: deployment root `root:ubuntu 0750`, `.env` `root:root 0600`, config directory/file `root:1000 0750/0640`, and data `1000:1000 0750`.
- The repeated Hades preflight passed with `configuration: ok`, `sqlite: ok`, `media roots: ok (5)`, `LAPSE: compatible`, and `ffprobe: available`. The one-shot container removed itself, selected-sidecar count remained zero, provider placeholders remained in place, and the exact temporary upload directory was removed. No `search`, `scan`, `serve`, webhook, or Silo action was run.

### Follow-up — live Titlovi expiration compatibility

- A credentialed Hades canary on 2026-09-04 found that Titlovi currently returns token expiry without an offset, for example `2026-09-11T13:16:35.55`. The adapter's RFC 3339-only parser rejected the otherwise valid login response before search.
- `subsyncd` keeps bounded Titlovi token-expiry caching: it accepts RFC 3339 first, then the observed fractional local timestamp in `Europe/Zagreb`; the existing one-time 401 refresh remains the fallback for server-side expiry or clock drift.
- The regression fixture uses the observed response shape. Its focused test failed with `Titlovi token expiration is invalid` before the fix and passed afterward; the complete Titlovi package passed with `-race`.
- A temporary arm64 image (`subsyncd:hades-titlovi-expiry-fix`, image ID `sha256:99499cc300dcd5ad8886f3e3a1fffc42be782578ae7ff8dcf01a72adecda45f6`) was streamed directly to Hades without publishing it. `doctor` passed and the 1883 S01E03 Croatian search reached two Titlovi candidates, proving authentication and search. Both scored 37, below the canary minimum of 50, so no download, LAPSE run, rejection quarantine, or sidecar installation occurred. The original pinned `sha-6f0683a` local tag was restored afterward.

### Follow-up — authoritative inventory fingerprint retention

- Continuing the credentialed Hades canary with a temporary minimum release score of 35 installed Titlovi candidate `344536` for 1883 S01E03 Croatian. Its release score was 37: external identity 20, title/year 15, and popularity 2. LAPSE returned `solid` in `auto/shifted` mode against the embedded reference, with a 128 ms offset, ratio 1, confidence 0.875466, agreement 1, coverage 1, one part, and no splits. The installed SRT checksum is `c4169cab58c1e60a0e735c7729fea76eff35aef9dbfd2658690d8e7bee97ebce`.
- The first repeat exposed a catalog/inventory timestamp mismatch. Sonarr and Radarr supply `dateAdded` as catalog metadata, while inventory records the actual filesystem modification time. Rehydrating an unchanged file compared those unlike values, falsely classified the media as changed, and cleared the installation's score, LAPSE result, and media fingerprint before upgrade evaluation.
- Both catalog paths now retain the stored authoritative inventory modification time whenever Arr file ID and byte size are unchanged. A real file replacement still invalidates provenance when file ID or size changes, and a fresh filesystem inventory explicitly invalidates provenance when it detects an actual modification-time change even with the same ID and size.
- Regression coverage exercises direct catalog refresh, transactional Arr event ingestion, and inventory-side invalidation. The retention tests first replace the catalog timestamp with an inventory timestamp, record a scored installation, repeat unchanged catalog metadata, and verify that the full inventory fingerprint, score, and LAPSE provenance survive. The invalidation test changes only the authoritative filesystem modification time and verifies that stale score, LAPSE, and media-fingerprint provenance are cleared atomically with inventory replacement.
- A combined temporary arm64 image (`subsyncd:hades-canary-fixes-2`) passed `doctor`, cleanly reinstalled candidate `344536`, and returned `outcome: rejected` on the immediate repeat because the same-score result was not an upgrade. The repeat completed without changing the SRT checksum and retained the complete score, LAPSE result, and media fingerprint. Recovery copies remain at `/opt/subsyncd-canary/data/subsyncd.before-fingerprint-fix.db` and `/opt/subsyncd-canary/data/canary-backup/1883.S01E03.hr.srt`. After review added inventory-side invalidation, final image ID `sha256:a24c08ecebd8e1f45e2b69c2fcbfe7832c25badace01dd24dcc61d8655c26374` passed `doctor` on Hades. The temporary score-35 configuration was removed, the standard score-50 configuration was restored, and this final fixed image was left active for continued canary testing; the original image remains available as local rollback tag `subsyncd:hades-original-sha-6f0683a`.

### Follow-up — OpenSubtitles episode identity and evidence scoring

- The 3 Body Problem S01E01 English canary first exposed a successful-login pacing bug. OpenSubtitles returned `Retry-After` on HTTP 200 login responses; the transport correctly persisted that window under `auth`, but the provider gate incorrectly applied timed auth cooldowns to search and download operations. Timed auth cooldowns now suppress only authentication calls, while a disabled auth state still blocks every provider operation. The regression test preserves both behaviors.
- OpenSubtitles episode `feature_details.imdb_id` and `tmdb_id` identify the episode, while Sonarr's stored IDs identify the series. The adapter now uses `parent_imdb_id` and `parent_tmdb_id` as the episode candidate's comparable series identity; missing parent IDs remain neutral and episode feature IDs are never misrepresented as series IDs. Movie feature IDs remain comparable and are retained. Language, title/year, season, episode, pack scope, and other conflict gates remain active.
- Exact episode coordinates, parsed release ranges containing the target episode, and explicit containing packs now contribute 20 points. A title/year plus episode-evidence candidate therefore reaches the default 35-point identity baseline, while release group, source, service, resolution, rating, and popularity continue to rank compatible encodes. Non-hash totals remain capped at 100 and low-confidence candidates still require LAPSE.
- OpenSubtitles `foreign_parts_only` is now retained as candidate `forced` evidence and is a hard rejection for a normal full-language request. This prevents a filename/API result such as candidate `8602118` from installing only foreign-dialogue captions even if LAPSE can align it. A forced-aware cache refresh on Hades confirmed candidate `8602118` is rejected solely as forced-only and `8733253` is rejected as both forced-only and hearing-impaired.
- Normalized provider-cache keys now carry schema version `candidate-v2`, so six-hour rows written before parent identity and forced-only evidence existed miss automatically rather than silently acquiring zero-value safety fields. Ordinary candidates omit `forced: false` from JSON, preserving existing deterministic rejection signatures, while forced-only candidates persist the flag.
- The Hades canary configuration now uses the documented default minimum score 35. Candidate `8602117` originally installed for 3 Body Problem S01E01 English at score 37 (title/year 15, episode 20, popularity 2). LAPSE returned `solid` against the embedded reference with zero offset, ratio 1, confidence 0.830059, agreement 1, coverage 1, one part, and no splits. The installed checksum is `8d0fb123992c306f72370f762ee97f1a704ff7d4df01eb20b14e3d1dfd9c8236`.
- A live `candidate-v2` refresh then retained seven current results, hard-rejected forced-only candidates `8602118` and `8733253`, and rescored `8602117` to 57 by adding the parent-series IMDb match. That metadata-only increase exposed an unnecessary same-candidate upgrade: the unchanged provider/result ID was downloaded and passed through LAPSE before the installer rejected its identical checksum. An unchanged media fingerprint plus identical provider/result ID now refreshes score and exact-hash provenance without download, LAPSE, rewrite, install audit, or notification; the refreshed score becomes the upgrade baseline and non-exact candidates retain their future upgrade schedule. A changed media fingerprint still permits revalidation.
- Final arm64 image `subsyncd:hades-canary-fixes-6`, ID `sha256:206698d6e52d4bb5bf5a2ef4b9b44deb64b4861f76471ed3459ae279cc95930e`, passed `doctor` on Hades. The repeat completed in 0.706 seconds with `outcome: satisfied`, candidate `8602117`, stored score 57, and the original checksum unchanged, confirming that LAPSE and installation were skipped. Recovery databases are retained at `/opt/subsyncd-canary/data/subsyncd.before-opensubtitles-identity-fix.db`, `/opt/subsyncd-canary/data/subsyncd.before-forced-gate.db`, and `/opt/subsyncd-canary/data/subsyncd.before-scoring-v2.db`.
- The same reassessment path upgraded installed Titlovi season-pack candidate `344536` from score 37 to 57 in 4.626 seconds without rerunning LAPSE; its original checksum `c4169cab58c1e60a0e735c7729fea76eff35aef9dbfd2658690d8e7bee97ebce` and LAPSE provenance remained unchanged. The three embedded-language controls (1917 English, Arrival English, and 3 Body Problem Croatian) all returned `satisfied` without downloads.
- Arrival Croatian exposed a canary-runtime issue rather than a scoring failure: six Titlovi candidates scored 36–54, but the long LAPSE CLI container repeatedly received `context canceled` around the time its inherited `/readyz` health check became unhealthy. Manual canary Compose now disables the image HTTP health check because one-shot CLI commands never serve that endpoint, preventing unhealthy-container remediation from interrupting long media analysis. The corrected run survived 12 minutes 11 seconds, selected Titlovi candidate `248646` at score 54, and installed checksum `cf3ca5550db498a2d9492aa6f8e8a50d739f13b4fc3ff81effb1704998aa844e`. LAPSE returned `solid` against the embedded reference with offset -21107 ms, ratio 1, confidence 0.778132, agreement 1, coverage 1, one part, and no splits. Manual inspection found a structurally valid 926-cue Croatian subtitle spanning the feature. The failed attempts wrote neither a sidecar nor a deterministic candidate rejection.
- The final 1917 Croatian canary completed successfully in 24 minutes 55 seconds without a health-check interruption. Four Titlovi results were eligible and the workflow evaluated the top three with both a dry-run and synchronization pass. Candidate `304766` won at score 39 and installed checksum `3daef6dc3f67ea960563ce1cb0e027a40acd4af51bf4fd0c7fce51bd4a8a5088`. LAPSE returned `solid` against the embedded reference with offset 1028 ms, ratio 1, confidence 0.761333, agreement 1, coverage 1, one part, and no splits. Manual inspection found a structurally valid 745-cue Croatian subtitle spanning the feature. This six-pass, 22.1 GB baseline motivates a separate two-phase LAPSE tournament: analyze candidates in score order, stop once lower-scored candidates cannot win, synchronize only the selected candidate, and fall back only after a synchronization failure.

### Follow-up — Persisted provider outage circuits and SubDL authentication

- Commit `e620961` adds a shared, persisted transient circuit per configured provider instance and operation. Network failures and HTTP 5xx responses now return the existing typed cooldown outcome and back off for 1, 5, 15, then 60 minutes, capped at 60 minutes. A usable provider `Retry-After` value remains authoritative. Because the state is in `provider_states`, it applies to every media job and survives process restart without a schema migration.
- Any non-5xx HTTP response resets only the transient failure attempt. Rate-limit, quota, and disabled-authentication fields are preserved, and one operation's circuit does not suppress another operation. Provider/network failures remain operational and never create candidate rejections.
- SubDL HTTP 403 on either search or download now persists a disabled authentication scope. Later work fails locally without another provider request until the API key is corrected and `subsyncd retry --provider NAME` clears that provider's states.
- Regression coverage proves the restart boundary, HTTP-call suppression, complete 1/5/15/60-minute escalation and cap, success reset, `Retry-After` precedence for 503, operation isolation, quota-state preservation, and both SubDL 403 paths. Verification passed with `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race`, `go test ./test/providercontract -tags=provider_contract`, and `git diff --check`.
- Hades built committed head `ec2cbad` as local arm64 image `subsyncd:hades-provider-errors-ec2cbad` (`sha256:066a2400cdd0034cc16afa2c404a7bc82d5d5e5a4a1bcbfbeba73da10ee91704`). `/opt/subsyncd-canary/compose.yml` is pinned to that image, with the previous file recoverable at `compose.yml.before-ec2cbad`; Compose rendering and the offline `doctor` check passed configuration, SQLite, five media roots, LAPSE, and FFprobe. No provider search or media mutation was used for this deployment check. Replace the local pin with the immutable GHCR SHA tag after multiarch publication succeeds.

### Follow-up — Movie edition evidence

- Commit `92848eb` tightens non-hash movie-edition handling without changing the provider interface or configuration. Radarr's stored edition remains authoritative. A candidate with an explicit matching edition earns 10 points; an explicit mismatch is rejected; an edition-unknown candidate remains eligible but cannot use confidence-based score bypass and therefore requires LAPSE. A verified exact file hash overrides only a conflicting textual edition label, while language, media-kind, forced-only, external-ID, year, and episode/pack safety gates remain active.
- Edition normalization now explicitly recognizes Director's Cut, Extended, Remastered, Unrated, Theatrical, Final Cut, Special Edition, Ultimate Cut, Redux, and Anniversary Edition. When a four-digit movie year is present, fallback markers are read only from the following release descriptor, preventing titles such as *The Final Cut (2004)* and *Anniversary (2015)* from becoming false edition evidence. Structural release parsing still uses `github.com/chill-institute/torrentname` v1.4.1; the custom layer supplies canonical policy evidence and conservative fallbacks.
- Regression tests cover the expanded aliases, title false positives, exact-hash precedence, explicit conflict, unknown-edition eligibility with zero points, and both blocked and allowed LAPSE bypass. Verification passed with `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race`, and `git diff --check`.
- Hades built committed head `bcb3efb` as local arm64 image `subsyncd:hades-edition-bcb3efb` (`sha256:e4425762cf7354f6ff558d8be23868dea3ea6ca32595287129c3ca6f09a3d241`). `/opt/subsyncd-canary/compose.yml` is pinned to it, with the previous file recoverable at `compose.yml.before-bcb3efb`; Compose rendering and the offline `doctor` check passed configuration, SQLite, five media roots, LAPSE, and FFprobe. No provider search or media mutation was used. Replace the local pin with the immutable GHCR SHA tag after multiarch publication succeeds.

### In progress — Structured Loki-compatible logging (Tasks 1–6)

- Commits `91fc653`, `1ea794f`, `61b5d8e`, `54272da`, `debd8d5`, and `a58317b` add configurable synchronous NDJSON logging, centralized privacy-safe event emission, application lifecycle events, webhook/readiness transitions, durable worker/reconciliation/notification events, and provider activity/state transitions. `logging.level` or `SUBSYNCD_LOG_LEVEL` selects `debug`, `info`, `warn`, or `error`; the default is `info` and a change requires restart.
- Normal successful health/readiness probes remain silent. Candidate/cache detail is debug-only. Provider search completions report bounded outcomes and cache status without media paths, release names, cache keys, download references, or URLs. Provider downloads report provider/candidate correlation, duration, byte count, and bounded outcome; returned filenames appear only at debug.
- Provider cooldown, circuit, authentication-disable, and recovery records are emitted only after the corresponding state write succeeds. Identical persisted cooldown/auth states and locally suppressed requests do not repeat transition events. Adapter type, operation scope, safe reason class, reset time, and transient attempt are structured fields when applicable; origins, request URLs, and raw provider reasons remain absent.
- Verification for the provider boundary passed with `go test ./internal/provider/... ./internal/app -race -count=1` and `git diff --check`. Focused RED/GREEN coverage includes cache hit/miss, download byte accounting and outcome classification, info-level privacy, ordinary rate windows, deduplicated cooldown/auth transitions, transient escalation/recovery, and cancellation/deadline warning severity.
- Next task: instrument workflow summaries, candidate decisions, LAPSE analysis/synchronization, installation/provenance, and failure paths without duplicating the authoritative worker outcome.

### In progress — Structured Loki-compatible logging (Task 7)

- Commit `3a0fdee` adds the workflow observability boundary. Every workflow run emits one `search.started` and one terminal `search.completed`, including early embedded/sidecar satisfaction, provider throttling, rejection, cancellation, and technical failure paths. The terminal event uses bounded outcome/reason values and never repeats raw failure detail already owned by an inventory, provider, LAPSE, installation, or worker event.
- Inventory completion reports aggregate embedded/sidecar counts and satisfaction without paths. Candidate evaluation, score components, rejected reasons, release names, tournament tiers, fallbacks, rejection decisions, and early stops are debug-only; a root-relative media path is added only when containment can be proven. The selected candidate is safe at info with provider/result ID, score, exact-hash state, and selection mode.
- LAPSE analysis and synchronization are timed at the workflow boundary and expose only typed verdict/mode/offset/ratio/confidence/agreement/coverage/part/split/version fields. Exact-hash and score-bypass paths emit no LAPSE phase event. Installation and provenance events are emitted only after their writes commit; installation checksum correlation is capped to 12 characters.
- Verification passed with `go test ./internal/workflow ./internal/app -race -count=1`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check`. Next task: publish the event/field/privacy contract, add Alloy/Loki guidance and queries, update the agent handoff, and run the full repository verification matrix before any Hades deployment.

### Follow-up — Structured Loki-compatible logging completed

- Implementation commits are `91fc653`, `1ea794f`, `61b5d8e`, `54272da`, `debd8d5`, `a58317b`, and `3a0fdee`; documentation checkpoints are `dd954d3` and `c7354a5`. Together they implement the approved synchronous NDJSON lifecycle, privacy, correlation, transition-only provider state, debug candidate detail, typed LAPSE result, committed install/provenance, and durable worker/notification contracts.
- The README documents `logging.level`, `SUBSYNCD_LOG_LEVEL` precedence, restart behavior, info/debug policy, and Alloy ownership. Operations documentation defines common and correlation fields, level behavior, successful-probe silence, privacy guarantees, a bounded-label Docker pipeline, and practical failed-job/provider/LAPSE/selection/job-trail LogQL queries. `AGENTS.md` now requires the logging design and plan before changing this contract.
- Hades reports host Alloy `v1.19.2` and a managed Grafana Cloud Loki destination. The documented dedicated Docker source selects `/subsyncd` before JSON parsing and promotes only `service`, `environment`, `level`, `component`, and `event`; dynamic identifiers remain JSON fields. An isolated copy of the exact River pipeline passed `alloy validate` on Hades and was removed afterward. The live Alloy configuration was not changed.
- Adding the environment override exposed an overbroad Hades canary test fake that returned a credential placeholder for every environment variable, including `SUBSYNCD_LOG_LEVEL`. The test now leaves the log override unset while continuing to synthesize canary credentials; the production canary configuration was already unaffected and defaults to `info`.
- Final local verification on 2026-09-04 passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the repository-wide focused logging/privacy/readiness test selection. Ordinary and tagged tests used local fixtures only.
- No image was published and no Hades service, canary, Alloy configuration, or Loki destination was mutated. Next step requires separate approval: publish the immutable multi-architecture image, pin the Hades service/canary to its SHA tag at `info`, add the validated Alloy route without duplicate ingestion, and inspect one bounded workflow for complete correlation and privacy before considering temporary debug.
