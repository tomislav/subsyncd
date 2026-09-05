# Hades daemon canary

This isolated deployment tests daemon lifecycle, health, reconciliation, webhook deduplication, persisted queue dispatch, restart recovery, and structured logs against only two Radarr movies with full embedded English subtitles. It uses a fresh database, one worker, English only, OpenSubtitles only, no Silo, no automatic restart, and an immutable image tag.

The two exact path mappings are the safety boundary. Before the first daemon start, run `doctor` once to create the database, stop all containers using it, and initialize `radarr-daemon-canary`'s reconciliation cursor to the current UTC time. This deliberately skips historical production Radarr events. Reconciliation consumes later unrelated entities as outside-scope audits without indexing or reading their media files; unsafe mappings and filesystem failures still fail the complete page atomically.

The HTTP listener is published only at Hades loopback port `18097`. Do not configure an automatic Radarr connection during this phase. Post the two sanitized fixtures manually, using the secret from the root-owned `.env` file without printing it.

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

Migration `010_media_entity_ids.sql` applied on startup. The previously failing cursor `2026-09-04T17:44:39.142509216Z` reconciled successfully in 56 ms and advanced atomically to `2026-09-05T07:08:48.568200533Z`. Two unrelated Radarr entities were recorded as zero-file outside-scope delete audits; no additional media row was created. The database remained at two mapped movies, two completed English search states, and 17 existing tracks. Both legacy media rows still have zero entity IDs because this page contained no live mapped event to hydrate them, which is the documented lazy-adoption behavior.

The bounded post-start audit found one `reconcile.started`, one `reconcile.completed`, and no `reconcile.failed`. Candidate, installation, provider cache/state, media hash, rejection, notification, provider, LAPSE, and installation activity all remained zero. `/healthz` returned `ok`, `/readyz` returned `ready`, and the container was left running healthy with `restart: "no"`. The deployment did not change mappings, credentials, the manual `/opt/subsyncd-canary`, or any other Hades service.

## Stable-identity Stage 2 retest — 2026-09-05

Stage 2 retained the exact two-file Radarr scope. The daemon was stopped cleanly and the post-Stage-1 data directory was preserved at `/opt/subsyncd-daemon-canary/data.before-stage2-f04b3c9`. Sequential manual English searches hydrated movie-file `1440` (1917) and `1168` (Arrival) through the live Radarr catalog. Both completed as `satisfied` from existing embedded inventory in 8 ms and 5 ms respectively, with zero candidates or provider errors.

Lazy stable-ID adoption updated the existing rows in place: 1917 retained file ID `1440` and adopted Radarr movie entity ID `529`; Arrival retained file ID `1168` and adopted entity ID `338`. The media-row count remained two. Candidates, installations, provider cache/state, file hashes, and notifications remained zero. After restart, health/readiness returned `ok`/`ready` and immediate reconciliation completed successfully in 6 ms. The daemon was left running on `subsyncd:hades-arrapi-f04b3c9`; scope, sidecars, and every other service remained unchanged.
