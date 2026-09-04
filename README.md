# subsyncd

`subsyncd` is a small, headless subtitle service for one or more Sonarr and Radarr instances. It indexes embedded and external subtitles, searches ordered providers per language, scores release compatibility, uses LAPSE when release evidence is uncertain, installs sidecars atomically, and can notify Silo through its Jellyfin-compatible API.

It intentionally has no browser UI and no management API. The HTTP surface is limited to Arr webhooks plus liveness/readiness checks; inspection and manual actions use the CLI.

## Why this is narrower than Bazarr

- Any canonical BCP 47 language can be configured independently.
- Each language has an explicit ordered provider list. The example routes Croatian only to Titlovi and English to OpenSubtitles followed by SubDL.
- Embedded tracks are fingerprint-cached in SQLite; sidecars are rescanned before every search.
- OpenSubtitles file hashes are calculated lazily once per exact file fingerprint and persisted.
- Exact-hash matches skip LAPSE. A first install with a score of at least 75 plus identity, release-group, and episode evidence can also skip it; uncertain matches, packs, and upgrades require LAPSE's strict `solid` verdict by default.
- Season packs use one bounded extractor and fail closed when the target member is ambiguous.
- Provider cooldowns, search schedules, leases, candidate evidence, install provenance, and notifications survive restarts.

See [providers.md](docs/providers.md) for search/scoring behavior and [operations.md](docs/operations.md) for deployment, webhooks, commands, recovery, and upgrades.

## Quick start with Compose

1. Copy `config.example.yaml` to `config/config.yaml`.
2. Set the credential variables used by that file.
3. Ensure UID/GID `10001:10001` can write the mounted data, movies, and TV directories.
4. Make the external `media` network (or change the network in the example) and start the service:

```bash
docker compose -f compose.example.yml build
docker compose -f compose.example.yml up -d
docker compose -f compose.example.yml exec subsyncd subsyncd doctor --config /config/config.yaml
```

The production image contains Go 1.27-built `subsyncd`, FFmpeg/FFprobe from Debian 13.2, and checksummed LAPSE v2.0.5 release assets for Linux amd64 and arm64. Debian 13 is required because the upstream LAPSE binaries need glibc 2.38 or newer. The image runs as fixed unprivileged UID/GID `10001:10001`. The Compose example uses a read-only root filesystem; only `/data`, `/tmp`, and the mapped media roots are writable.

The root stack exposes the same service behind an opt-in profile:

```bash
docker compose --profile subsyncd up -d subsyncd
```

## Native build

Requirements are Go 1.27, `ffprobe`, and a compatible LAPSE v2.0.5 executable.

```bash
go build -trimpath -o subsyncd ./cmd/subsyncd
./subsyncd --version
./subsyncd doctor --config ./config.yaml
./subsyncd serve --config ./config.yaml
```

The configuration requires absolute `data_dir`, `media_roots`, mapping destinations, and `sync.lapse_path` values.

## Arr webhooks

Configure a webhook/connection in each Arr instance using its configured instance name and secret:

```text
http://subsyncd:8097/webhooks/sonarr-main?token=THE_SONARR_WEBHOOK_TOKEN
http://subsyncd:8097/webhooks/radarr-main?token=THE_RADARR_WEBHOOK_TOKEN
```

Enable download/import, upgrade, rename, and file-delete events. Arr test events return success but create no work. Exact redeliveries are transactionally idempotent.

## Development and tests

Default tests never contact Arr, subtitle providers, Silo, or LAPSE. They use injected runners, synthetic files, and loopback fake servers.

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -v
```

Real provider smoke tests are deliberately excluded from normal CI. They run only with `-tags=provider_contract` and the corresponding provider credentials in the environment.

The project consulted Bazarr only as a pinned GPL-3.0 behavioral reference. No Bazarr source, fixtures, or runtime dependency are included; see [the reference ledger](docs/references/bazarr.md).
