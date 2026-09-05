# subsyncd

`subsyncd` is a small, headless subtitle service for one or more Sonarr and Radarr instances. It indexes embedded and external subtitles, searches ordered providers per language, scores release compatibility, uses LAPSE when release evidence is uncertain, installs sidecars atomically, and can notify Silo through its native scan API.

It intentionally has no browser UI and no management API. The HTTP surface is limited to Arr webhooks plus liveness/readiness checks; inspection and manual actions use the CLI.

## Features

- Any canonical BCP 47 language can be configured independently.
- Each language has an explicit ordered provider list. The example routes Croatian only to Titlovi and English to OpenSubtitles followed by SubDL.
- Adding a language later backfills an immediately due, low-priority search for every media item already indexed under a configured Arr instance. Existing schedules and installations are preserved.
- Embedded tracks are fingerprint-cached in SQLite; sidecars are rescanned before every search.
- OpenSubtitles file hashes are calculated lazily once per exact file fingerprint and persisted.
- Exact-hash matches skip LAPSE. A first install with a score of at least 75 plus identity, release-group, episode, and any required movie-edition evidence can also skip it; uncertain matches, packs, edition-unknown movies, and upgrades require LAPSE's strict `solid` verdict by default.
- LAPSE candidates run as a lazy score-tier tournament: lower-scored files are untouched after a higher tier installs, equal-score ties are compared by analysis confidence, and only the selected candidate is synchronized unless fallback is needed.
- Season packs use one bounded extractor and fail closed when the target member is ambiguous.
- Deterministic candidate failures are quarantined for the exact media/release evidence, allowing later-ranked results to advance without repeated downloads; operational failures remain retryable.
- Provider cooldowns, quotas, operation-scoped outage circuits, search schedules, leases, candidate evidence/rejections, install provenance, and notifications survive restarts. Transient provider failures back off globally instead of generating one request per media file.
- Persisted searches favor new imports, then missing subtitles, then upgrade checks. Webhooks wake free workers immediately, while coalesced signals and periodic polling recover safely after bursts or restarts.
- Sonarr/Radarr reconciliation tracks the stable movie or episode identity separately from the replaceable physical file identity. Imports, renames, upgrades, deletions, audit rows, and the cursor commit together, so a failed page is retried without losing changes.
- Deliberately unmapped Arr media is skipped safely, allowing narrow canaries without indexing or reading the rest of a library. Unsafe mappings and filesystem failures still fail closed.
- Sonarr files containing multiple episodes are indexed and explained as unsupported, but never sent to providers or LAPSE until combined-episode matching is implemented safely.
- Media workflow concurrency is configurable from one to eight and defaults to one, which is the conservative choice for LAPSE and network-mounted media.
- Shutdown is bounded even when an external tool ignores cancellation; unfinished durable leases are left for recovery after restart.

See [providers.md](docs/providers.md) for search/scoring behavior and [operations.md](docs/operations.md) for deployment, webhooks, commands, recovery, and upgrades.

For a deliberately narrow production trial, the [Hades manual canary](deploy/hades-canary/README.md) pins an immutable image, disables daemon/webhook/Silo behavior, and permits only four exact Arr media-file mappings.

## Private container image

GitHub Actions publishes `ghcr.io/tomislav/subsyncd` for `linux/amd64` and `linux/arm64`. The repository and package are private, so each Docker host must authenticate with a GitHub token that can read packages:

```bash
docker login ghcr.io --username tomislav
```

Enter the token through Docker's password prompt; do not put it in this repository or in a command argument.

Successful pushes to `main` publish `latest` and an immutable `sha-<commit>` tag. Stable `v*` tags also publish semantic-version aliases; prereleases publish only their full version and SHA. Set `SUBSYNCD_IMAGE_TAG` to pin Compose to a version or immutable SHA.

## Quick start with Compose

1. Copy `config.example.yaml` to `config/config.yaml`.
2. Set the credential variables used by that file.
3. Optionally set `PUID`, `PGID`, and `TZ` in `.env`; they default to `1000`, `1000`, and `Europe/Zagreb`.
4. Ensure that UID/GID can write the mounted data, movies, and TV directories.
5. Make the external `media` network (or change the network in the example) and start the service:

```bash
docker compose -f compose.example.yml pull
docker compose -f compose.example.yml up -d
docker compose -f compose.example.yml exec subsyncd subsyncd doctor --config /config/config.yaml
```

The `ghcr.io/tomislav/subsyncd:latest` production image supports Linux amd64 and arm64. It contains Go 1.27.1-built `subsyncd`, FFmpeg/FFprobe and timezone data from Debian 13.2, and checksummed LAPSE v2.0.5 release assets. Debian 13 is required because the upstream LAPSE binaries need glibc 2.38 or newer. The image defaults to unprivileged UID/GID `1000:1000`; Compose can select another existing host identity through `PUID` and `PGID` without starting the container as root. The Compose example uses a read-only root filesystem; only `/data`, `/tmp`, and the mapped media roots are writable.

## Structured logs

`subsyncd` writes one JSON object per line to stderr for Docker, Grafana Alloy, or another container-log collector. The default level is `info`:

```yaml
logging:
  level: info # debug, info, warn, or error
```

`SUBSYNCD_LOG_LEVEL` overrides the YAML value when set. Logging configuration is read at startup, so changing either value requires a restart. `debug` adds candidate scoring, bounded release diagnostics, cache decisions, and root-relative media paths; it should be enabled only for a short investigation. Logs never intentionally include credentials, provider URLs/bodies, absolute media paths, command arguments, or raw LAPSE output. Collection, labels, retention, and Loki credentials belong to Alloy rather than this service; see [Structured logging and Grafana Loki](docs/operations.md#structured-logging-and-grafana-loki).

## Native build

Requirements are Go 1.27.1, `ffprobe`, and a compatible LAPSE v2.0.5 executable.

```bash
go build -trimpath -o subsyncd ./cmd/subsyncd
./subsyncd --version
./subsyncd doctor --config ./config.yaml
./subsyncd serve --config ./config.yaml

docker build --build-arg VERSION=dev -t subsyncd:local .
```

The configuration requires absolute `data_dir`, `media_roots`, mapping destinations, and `sync.lapse_path` values.

## Arr webhooks

Configure a webhook/connection in each Arr instance using its configured instance name and secret:

```text
http://subsyncd:8097/webhooks/sonarr-main?token=THE_SONARR_WEBHOOK_TOKEN
http://subsyncd:8097/webhooks/radarr-main?token=THE_RADARR_WEBHOOK_TOKEN
```

Enable download/import, upgrade, rename, and file-delete events. Connections are configured manually; `subsyncd` does not create or modify Arr settings. Arr test events and imports/renames that are deliberately outside configured path scope return an ignored success and create no work. Unsafe path/filesystem failures still fail the request. Exact redeliveries are transactionally idempotent.

Reconciliation uses `github.com/cplieger/arrapi/v2` v2.0.5 for bounded history and current movie/episode requests. It runs immediately after startup without blocking startup itself, normally repeats every six hours, and backs failures off after 5 minutes, 15 minutes, 1 hour, then 6 hours. Second-resolution history requests deliberately overlap the fractional persisted cursor; stable Arr history event IDs make that replay harmless. Every persisted media row has a positive stable Arr entity ID, while physical file ID remains separate and replaceable. Startup performs no Arr request, identity backfill, or full-library scan. It does reconcile configured languages locally in SQLite: a newly added language gets one missing-priority search for each already-indexed media item, while every existing search row remains unchanged. Configuration changes require a restart.

subsyncd currently supports only databases created from `001_baseline.sql`. A database containing migration names from the pre-release `001_initial.sql`–`010_media_entity_ids.sql` lineage is rejected explicitly. Stop the service, preserve the old data directory if rollback matters, and start with an empty data directory. The migration runner remains in place for migrations added after the baseline.

## Development and tests

Default tests never contact Arr, subtitle providers, Silo, or LAPSE. They use injected runners, synthetic files, and loopback fake servers.

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -v
```

Real provider smoke tests are deliberately excluded from normal CI. They run only with `-tags=provider_contract` and the corresponding provider credentials in the environment.
