# Hades daemon canary

This isolated deployment now covers two exact Radarr movies plus one exact Sonarr episode. English routes only to OpenSubtitles and Croatian only to Titlovi. It uses one worker, confidence-gated LAPSE, no Silo, no automatic restart, and an immutable image tag. Radarr connection ID `8` and Sonarr connection ID `10` are active.

The application-level safety boundary is the three exact file mappings. The Sonarr episode shares a directory with the rest of its season, so Docker must mount that directory writable to support atomic sidecar creation; subsyncd maps, indexes, and schedules only episode-file `10545`. The database must remain at exactly three media rows unless the canary is deliberately broadened.

## Initial two-file acceptance plan (completed)

The deployment began by testing daemon lifecycle, health, reconciliation, webhook deduplication, persisted queue dispatch, restart recovery, and structured logs against two Radarr movies with full embedded English subtitles.

The original two exact path mappings were the initial safety boundary. Before the first daemon start, `doctor` created the database and `radarr-daemon-canary`'s reconciliation cursor was initialized to current UTC. This deliberately skipped historical production Radarr events. Reconciliation consumes later unrelated entities as outside-scope audits without indexing or reading their media files; unsafe mappings and filesystem failures still fail the complete page atomically.

The HTTP listener is published only at Hades loopback port `18097`. The two sanitized movie fixtures were posted manually during this initial phase, using the secret from the root-owned `.env` file without printing it.

Expected results:

- `/healthz` returns `ok` and `/readyz` returns `ready`.
- Both webhook requests return HTTP 204 and dispatch sequentially at import priority.
- Each search completes as `satisfied` from embedded English inventory.
- Reposting one identical fixture is logged as `duplicate` and starts no workflow.
- OpenSubtitles search/download, LAPSE, installation, and notification events remain absent.
- No sidecar checksum changes.
- A stop/start using the same data directory creates no duplicate work.

Keep the existing `/opt/subsyncd-canary` manual deployment untouched. Stop this daemon before running any manual canary command because both deployments mount the same media directories.

## Hades deployment record

The canary was deployed on 2026-09-04 from immutable image `ghcr.io/tomislav/subsyncd:sha-b3a2d45`. Hades selected the native arm64 image with local image ID `sha256:03c48f02a4778ff7f6df21b990d1d8a941d4711ede9377635ad1fcc16655dfac`. GitHub Actions run `33900156628` passed race tests, vet, tagged end-to-end tests, and the multi-architecture publication job.

Arcane is authorized for the private GHCR package, but that credential is not shared with the root Docker client used through SSH. The initial pull used a temporary root-only `DOCKER_CONFIG`; its directory was removed immediately after the pull. Do not store a GHCR token in this deployment's `.env`.

Hades uses GID 1001 for `ubuntu` and GID 1000 for `opc`, while this container runs as `1000:1000`. Use this ownership split:

- deployment root and `config/`: `root:1000`, mode `0750`
- `config/config.yaml`: `root:1000`, mode `0640`
- `data/`: `1000:1000`, mode `0750`
- `.env`: `root:root`, mode `0600`

Because the deployment root is intentionally not traversable by the SSH account, run Compose through a root shell:

```bash
sudo sh -c 'cd /opt/subsyncd-daemon-canary && docker compose --env-file .env -f compose.yml ps'
sudo sh -c 'cd /opt/subsyncd-daemon-canary && docker compose --env-file .env -f compose.yml logs --no-color --tail=200'
sudo sh -c 'cd /opt/subsyncd-daemon-canary && docker compose --env-file .env -f compose.yml stop'
```

Before the first `serve`, `doctor` created the fresh database and the instance cursor was advanced to current UTC with the daemon stopped. This skipped old Radarr history. Do not delete or reset that cursor while this remains a two-file canary; an old unrelated history page cannot pass the exact path mappings and will repeatedly fail closed.

Two manual Download fixtures returned HTTP 204. Both English jobs ran serially at import priority and completed as `satisfied` from embedded inventory: 1917 had five embedded subtitle streams and Arrival had ten. Duplicate 1917 delivery was a no-op before and after restart. Final checks found no pending lease, candidate, installation, provider cache/state, provider call, LAPSE run, notification, warning, or error. The two pre-existing Croatian SRT checksums remained unchanged.

The canary was left running with `restart: "no"` and no automatic Radarr connection. Continue observing it before adding a real webhook or a third file. Stop it before using `/opt/subsyncd-canary` against either mounted movie.

## Stable-identity Stage 1 retest — 2026-09-05

The locally verified arm64 image from commit `f04b3c9` was streamed directly to Hades without registry publication and loaded as `subsyncd:hades-arrapi-f04b3c9`. Its image ID is `sha256:9138534acd0cd9e4196abf7a95693656f50bf92aa7427551c427dbe98d66d4fb`, and the embedded development version is `arrapi-local`. The canary Compose file is pinned to this local tag. The prior Compose file is recoverable at `/opt/subsyncd-daemon-canary/compose.yml.before-f04b3c9`; the complete stopped data directory is recoverable at `/opt/subsyncd-daemon-canary/data.before-f04b3c9`.

This historical run used the now-unsupported pre-baseline migration lineage. Its previously failing cursor `2026-09-04T17:44:39.142509216Z` reconciled successfully in 56 ms and advanced atomically to `2026-09-05T07:08:48.568200533Z`. Two unrelated Radarr entities were recorded as zero-file outside-scope delete audits; no additional media row was created. The historical database is retained only as rollback evidence and must not be opened by the clean-baseline build.

The bounded post-start audit found one `reconcile.started`, one `reconcile.completed`, and no `reconcile.failed`. Candidate, installation, provider cache/state, media hash, rejection, notification, provider, LAPSE, and installation activity all remained zero. `/healthz` returned `ok`, `/readyz` returned `ready`, and the container was left running healthy with `restart: "no"`. The deployment did not change mappings, credentials, the manual `/opt/subsyncd-canary`, or any other Hades service.

## Stable-identity Stage 2 retest — 2026-09-05

Stage 2 retained the exact two-file Radarr scope. The daemon was stopped cleanly and the post-Stage-1 data directory was preserved at `/opt/subsyncd-daemon-canary/data.before-stage2-f04b3c9`. Sequential manual English searches hydrated movie-file `1440` (1917) and `1168` (Arrival) through the live Radarr catalog. Both completed as `satisfied` from existing embedded inventory in 8 ms and 5 ms respectively, with zero candidates or provider errors.

Lazy stable-ID adoption updated the existing rows in place: 1917 retained file ID `1440` and adopted Radarr movie entity ID `529`; Arrival retained file ID `1168` and adopted entity ID `338`. The media-row count remained two. Candidates, installations, provider cache/state, file hashes, and notifications remained zero. After restart, health/readiness returned `ok`/`ready` and immediate reconciliation completed successfully in 6 ms. The daemon was left running on `subsyncd:hades-arrapi-f04b3c9`; scope, sidecars, and every other service remained unchanged.

## Scoped real-webhook Stage 3 — 2026-09-05

Commit `4ddbcaf` includes the tested rule that a typed outside-scope import/rename returns an ignored HTTP 204 without a media/event mutation or worker wake, while unsafe failures remain errors. Its locally built arm64 image was streamed directly to Hades as `subsyncd:hades-stage3-4ddbcaf`, image ID `sha256:0f2ead23eea7daa98628ce51f80c52ea61acf43674abd5ec8c22a74391443e3f`, UID/GID `1000:1000`, version `sha-4ddbcaf`. The prior Compose and complete stopped data directory are retained as `compose.yml.before-stage3-4ddbcaf` and `data.before-stage3-4ddbcaf`.

Radarr connection ID `8`, named `subsyncd daemon canary`, targets the canary service over the shared `apps` network. It enables download/import, upgrade, rename, movie-file deletion, and upgrade-file deletion only. Radarr's connection test returned 200; its `Test` webhook was ignored by subsyncd with 204.

A controlled real-shaped rename for mapped file `1440` returned 204, committed one event, woke one import-priority job, and completed as embedded `satisfied` in 6 ms. Exact redelivery returned 204 as `duplicate` and started no second job. A payload built in memory from an actual current but unmapped Radarr movie returned 204 as `ignored` with `event_count=1` and `applied_count=0`; its path was not printed or persisted. Media remained exactly two rows, events increased only for the mapped rename, and candidates, installations, provider cache, and media hashes remained zero. The container was left healthy and ready on the Stage 3 image. Observe one natural six-hour reconciliation before broadening scope.

## Sonarr/Titlovi/LAPSE Stage 4 — 2026-09-05

Stage 4 added only `1883` S01E06, Sonarr episode entity `3913` and episode-file `10545`. Before deployment, the exact file had no external subtitle and one embedded English SubRip stream. The Season 01 directory is mounted writable because a file-only bind cannot support atomic creation/rename of the sibling `.hr.srt`; the path mapping still admits only file `10545`. The two Radarr mappings were unchanged.

The daemon was stopped and recoverable copies were created at `compose.yml.before-stage4-4ddbcaf`, `config.before-stage4-4ddbcaf`, `.env.before-stage4-4ddbcaf`, and `data.before-stage4-4ddbcaf`. Four existing Sonarr/Titlovi variables were copied from `/opt/subsyncd-canary/.env` without printing their values. `doctor` passed with two instances, two providers, two languages, four roots, LAPSE compatibility, and FFprobe availability. Only the new Sonarr cursor was initialized to current UTC before startup; the Radarr cursor was not modified. Initial reconciliations for both instances completed successfully in 6 ms.

The sanitized `webhook-1883-s01e06.json` fixture returned HTTP 204 and applied once. English completed as `satisfied` from the embedded stream. Croatian searched Titlovi and received two candidates in 4.062 seconds. Candidate `342548` scored 59, below the confidence bypass, so it was the only candidate downloaded (13,021 bytes) and sent to LAPSE. LAPSE analysis took 56.345 seconds and returned `solid`, confidence `0.859118`, agreement and coverage `1`, ratio `1`, offset `-1 ms`; sync took 8.009 seconds. The candidate was selected in `lapse` mode and installed in 2.610 seconds. Total Croatian workflow time was 75.059 seconds, with the next upgrade check scheduled for `2026-09-12T07:39:31.284641855Z`.

Final state: exactly three media rows; four completed search outcomes (the Croatian row is scheduled/pending for its upgrade date with last outcome `installed`); two candidate metadata rows; one provider download; one LAPSE analysis; one installation from `titlovi-main` candidate `342548`, score 59, verdict `solid`; no provider errors. The sidecar is `0644`, owned `1000:1000`, 29,034 bytes, SHA-256 `85299991788b0e8803a43d43a7b34a7a99a075b3c3a0c197e75ad4e29185244d`. `/healthz` and `/readyz` both passed and the daemon was left running. No global Sonarr connection was created in this stage.

## Scoped real Sonarr webhook Stage 5 — 2026-09-05

Sonarr connection ID `10`, named `subsyncd daemon canary`, now targets the daemon over the shared `apps` network. It is global at the delivery layer and enables download/import, upgrade, rename, episode-file deletion, and upgrade-file deletion; Grab is disabled. Exact path mappings remain the application-level authorization boundary.

Sonarr's native connection test returned HTTP 200. subsyncd accepted its Test payload, classified it as ignored HTTP 204 with zero events, and left the database unchanged at three media, seven events, and four search states.

The unique mapped `webhook-1883-s01e06-rename.json` fixture returned HTTP 204 and committed exactly one event. English satisfied from the embedded stream in 3 ms. Croatian satisfied from the installed sidecar/provenance in 8 ms, retained score 59, and advanced its upgrade check to `2026-09-12T07:45:50.235691291Z`. The sidecar checksum did not change, no provider download occurred, no lease remained, and media stayed at three rows.

An outside-scope Download payload was then constructed in memory from actual current Sonarr episode-file `10543`; its path was neither printed nor persisted and the temporary payload was deleted. subsyncd returned ignored HTTP 204 in 12 ms with `event_count=1` and `applied_count=0`. Database counts remained exactly `3 media / 8 events / 4 search states / 2 candidates / 1 installation`, no lease remained, and no search started. The daemon was left running with both real Arr connections active and the three-file scope unchanged.

## Clean schema baseline Stage 6 — 2026-09-05

The verified arm64 image `ghcr.io/tomislav/subsyncd:sha-791916b` had image ID `sha256:30549ef2bf6154dff57d4ce077cd2a7ba9eb85c1ab5b352156cb7c4c898fd69d`, embedded version `sha-791916b`, and user `1000:1000`. The pre-cutover database was retained at `data.before-schema-baseline-791916b`; Compose, config, and `.env` received matching suffix backups. The exact S01E06 Croatian sidecar was copied root-only under `sidecar-backups/schema-baseline-791916b` and checksum-verified before the authorized live copy was removed.

Doctor created a fresh database containing only `001_baseline.sql`. Both Arr cursors were initialized to the captured cutover instant while the daemon was stopped. Startup health/readiness and immediate reconciliation passed for both instances. The exact two Radarr fixtures and S01E06 Sonarr fixture returned HTTP 204 and recreated only the three mapped media rows: `338/1168`, `529/1440`, and `3913/10545`.

English and existing Croatian inventory required no provider traffic. Missing S01E06 Croatian returned two Titlovi candidates; candidate `342548` scored 59 and was the only 13,021-byte download. LAPSE analysis took 6.722 seconds, returned `solid` with confidence `0.859118`, agreement/coverage `1`, ratio `1`, and offset `-1 ms`; synchronization took 7.070 seconds. The installed sidecar retained its expected 29,034-byte size, `0644` mode, `1000:1000` ownership, and SHA-256 `85299991788b0e8803a43d43a7b34a7a99a075b3c3a0c197e75ad4e29185244d`.

Sonarr connection 10's native test returned 200 and produced an ignored 204 without mutation. The mapped rename added exactly one event, performed no second download, and left the sidecar unchanged. A payload built in memory from actual unmapped episode-file `10543` returned ignored 204 without a database mutation or worker wake. Acceptance ended at `3 media / 5 events / 6 search states / 2 candidates / 1 installation`, zero leases, and zero warning/error events. Both Arr connections remained active.

## Native Silo subtree notification Stage 7 — 2026-09-05

Live testing against the healthy Hades Silo container established that the native API is reachable within the `apps` network at `http://silo:8080`; port 8090 is not a listener in this deployment, while 8096 remains the compatibility surface. An authenticated media-file scan returned 202 but processed no files because it did not walk the newly written sibling subtitle. Scanning the mapped Season 01 parent directory returned 202, processed 10 media files, and left the exact S01E06 Croatian sidecar indexed as Silo external-subtitle metadata.

Commit `4af206c` changes the notifier to select the parent directory after path mapping. The verified arm64 image `ghcr.io/tomislav/subsyncd:sha-4af206c` has image ID `sha256:20bbdf3fd59dc07418609336a0d0c8ddaca76040e54565db4ae6ca09acf0ef31`, embedded version `sha-4af206c`, and user `1000:1000`. Rollback copies of the prior Compose, config, and root-only `.env` use suffix `before-silo-test-4af206c`.

With Silo temporarily enabled, one synthetic due SQLite notification exercised the actual daemon worker. It was delivered once in 14 ms, Silo accepted the native request in 8 ms, and the resulting subtree scan processed all 10 Season 01 media files to completion. The test row and temporary key were then removed, the checked-in disabled Silo configuration was restored, and doctor passed with Silo disabled. The daemon remains healthy and ready on `sha-4af206c` with zero notification rows. Use a new permanent API key before enabling Silo; never reuse the temporary test credential.
