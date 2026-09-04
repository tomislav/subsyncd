# Hades daemon canary

This isolated deployment tests daemon lifecycle, health, reconciliation, webhook deduplication, persisted queue dispatch, restart recovery, and structured logs against only two Radarr movies with full embedded English subtitles. It uses a fresh database, one worker, English only, OpenSubtitles only, no Silo, no automatic restart, and an immutable image tag.

The two exact path mappings are the safety boundary. Before the first daemon start, run `doctor` once to create the database, stop all containers using it, and initialize `radarr-daemon-canary`'s reconciliation cursor to the current UTC time. This deliberately skips historical production Radarr events. A future unrelated Radarr history event cannot map and makes the complete reconciliation page fail atomically without advancing its cursor or scheduling partial work.

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
