# Hades manual canary

This deployment exercises `subsyncd` against four known Hades media files without starting its daemon. The image is pinned to `sha-6f0683a`, the Compose service exists only in the `manual` profile, has no restart policy, exposes no port, installs no webhook, and keeps Silo disabled.

Do not run `scan` or `serve` with this configuration. Reconciliation would inspect Arr history outside the four exact file mappings. Use only the targeted commands below until the canary results have been reviewed.

## Scope and expected inventory

| Item | Arr reference | Safe initial check | Expected result from the 2026-09-04 FFprobe baseline |
| --- | --- | --- | --- |
| 1917 | `radarr-canary`, movie file `1440` | English | Full embedded English SRT satisfies the request; no provider search |
| Arrival | `radarr-canary`, movie file `1168` | English | Full embedded English SRT satisfies the request; no provider search |
| 1883 S01E03 | `sonarr-canary`, episode file `9864` | Croatian | English is embedded, Croatian is missing; Titlovi candidate should require LAPSE |
| 3 Body Problem S01E01 | `sonarr-canary`, episode file `10146` | Croatian, then English | Embedded Croatian satisfies `hr`; the only English track is SDH, so `en` remains missing while hearing-impaired tracks are disallowed |

The two TV mounts contain their complete Season 01 directories because sidecars require directory-level write access. The configuration still maps only the two exact episode-file paths above; any other Sonarr file ID fails path mapping before inventory or installation. `/mnt` is mounted read-only so the absolute AltMount video symlinks resolve, while new sidecars are written into the selected `/srv/media` library directories.

## Install on Hades

Copy this directory to `/opt/subsyncd-canary`, then work only from that directory:

```bash
cd /opt/subsyncd-canary
cp .env.example .env
chmod 600 .env
sudo install -d -o 1000 -g 1000 -m 0750 data
```

Fill every blank value in `.env`. Generate separate unused webhook validation values even though the manual canary never listens for webhooks:

```bash
openssl rand -hex 32
```

The package is private. Authenticate Hades once using a GitHub token with `read:packages`, entering it through Docker's password prompt rather than a command argument:

```bash
sudo docker login ghcr.io --username tomislav
```

Validate rendering, pull the pinned image, and run offline startup diagnostics:

```bash
sudo docker compose --env-file .env --profile manual config --quiet
sudo docker compose --env-file .env --profile manual pull
sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary
```

The last command uses the service's safe default command: `doctor --config /config/config.yaml`. It validates configuration, SQLite, the five mounted roots, FFprobe, and LAPSE without contacting Arr or subtitle providers.

## Capture the baseline

Record existing sidecars before each targeted search:

```bash
find \
  '/srv/media/movies/1917 (2019) [tmdbid-530915]' \
  '/srv/media/movies/Arrival (2016) [tmdbid-329865]' \
  '/srv/media/tv/1883 (2021) [tvdbid-396390]/Season 01' \
  '/srv/media/tv/3 Body Problem (2024) [tvdbid-411959]/Season 01' \
  -maxdepth 1 -type f \
  \( -iname '*.srt' -o -iname '*.ass' -o -iname '*.ssa' -o -iname '*.vtt' \) \
  -print0 | sort -z | xargs -0 -r sha256sum | tee sidecars.before.sha256
```

Keep this file until the canary is complete. `subsyncd` protects existing sidecars unless their path and checksum match its own installation provenance.

## Phase 1: prove embedded-track skips

Run each command separately. These should contact only Radarr/Sonarr and should not contact a subtitle provider or invoke LAPSE:

```bash
sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  search --config /config/config.yaml --instance radarr-canary --kind movie --file-id 1440 --language en

sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  search --config /config/config.yaml --instance radarr-canary --kind movie --file-id 1168 --language en

sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  search --config /config/config.yaml --instance sonarr-canary --kind episode --file-id 10146 --language hr
```

Inspect every decision:

```bash
sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  explain --config /config/config.yaml --instance radarr-canary --kind movie --file-id 1440 --language en
```

Repeat `explain` with the corresponding instance, kind, file ID, and language. Stop if a supposedly embedded language triggers a provider call or creates a sidecar.

## Phase 2: one real Titlovi plus LAPSE workflow

Start with 1883 because its media file is about 4.1 GB. `sync.policy: always` means a non-hash Titlovi candidate must receive LAPSE's strict `solid` verdict. LAPSE can read most of the media file on its first analysis, so watch AltMount/network traffic and container resource use:

```bash
sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  search --config /config/config.yaml --instance sonarr-canary --kind episode --file-id 9864 --language hr

sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  explain --config /config/config.yaml --instance sonarr-canary --kind episode --file-id 9864 --language hr
```

A successful result should create one Croatian sidecar beside S01E03 and record score, provider, checksum, and `solid` LAPSE provenance. `unsure` or `nothing` must leave no new subtitle and quarantine only that candidate. A process/network/filesystem failure must remain retryable and must not blacklist it.

## Phase 3: OpenSubtitles and larger media

For `3 Body Problem` English, an exact OpenSubtitles movie-hash match can bypass LAPSE; otherwise the always policy requires LAPSE. Run this before either large movie:

```bash
sudo docker compose --env-file .env --profile manual run --rm --no-deps subsyncd-canary \
  search --config /config/config.yaml --instance sonarr-canary --kind episode --file-id 10146 --language en
```

Only after reviewing those results, test Croatian on Arrival (`1168`, about 16.9 GB) and 1917 (`1440`, about 22.1 GB) one at a time. Use the same targeted `search` and `explain` pattern. Do not run them concurrently.

## Review and stop

Re-run the baseline command with `sidecars.after.sha256`, compare it with the before file, and inspect any new subtitle text manually. Silo remains disabled, so request a targeted Silo scan manually only after accepting the result.

The manual containers remove themselves after every command; there is no canary daemon to stop. Preserve `data/` while evaluating the canary because it contains inventory, rejection, score, hash, LAPSE-cache, and installation provenance. To abandon the canary, remove only `/opt/subsyncd-canary` after separately reviewing or removing sidecars that the canary created.
