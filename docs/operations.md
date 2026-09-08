# Running subsyncd

For installation, start with the [Docker Compose setup](../README.md#install-with-docker-compose). This guide covers everyday use after installation. Commands below assume you copied the supplied Compose example to `compose.yaml`.

## Check the service

```bash
docker compose ps
docker compose exec subsyncd subsyncd doctor
```

`doctor` checks your configuration, database, media folders, and bundled tools. It does not test connections or credentials for Sonarr, Radarr, subtitle providers, or Silo. Start the service once before using diagnostics so it can initialize the database.

The container health check runs automatically. `/healthz` reports whether the HTTP service is running; `/readyz` checks the database and media folders.

## Logging

To follow activity:

```bash
docker compose logs -f subsyncd
```

Logs show searches, downloads, synchronization, installations, and errors. See the [logging guide](logging.md) for debug settings and how to follow a particular search.

## Filesystem and container permissions

The container needs to read your configuration and media, write subtitles beside the media, and save its state in `data/`.

Set `PUID` and `PGID` in the Compose `.env` file to a host user and group with that access. Both default to `1000`. Run `id -u` and `id -g` to check your own IDs. The container does not change folder ownership automatically.

| Folder inside the container | Access needed |
| --- | --- |
| `/config` | Read configuration |
| `/data` | Read and write saved state and caches |
| `/media/movies` and `/media/tv` | Read media and write subtitles |
| `/tmp` | Temporary processing space, supplied by Compose |

Create the host folders before starting. Leave `install.uid` and `install.gid` unset unless you specifically need to change subtitle ownership. New subtitles use file mode `0644` by default.

Temporary downloads and synchronization files use `/tmp`. Keep it separate from your media folders. If you increase simultaneous searches, you may also need to increase the Compose temporary-storage allowance.

## Sonarr and Radarr setup

Each instance in `config/config.yaml` needs a unique name, reachable URL, API key, webhook secret, and path mapping. Replace the quoted API-key and webhook-secret placeholders directly in that file. The remote path is what Sonarr or Radarr reports; the local path is where the same folder appears inside subsyncd.

For example:

```yaml
path_mappings:
  - remote: /data/tv
    local: /media/tv
```

In each application's **Settings → Connect**, add a webhook:

```text
http://subsyncd:8097/webhooks/INSTANCE_NAME?token=YOUR_WEBHOOK_SECRET
```

Use the instance name and secret from your configuration. If the application runs outside Docker, use the Docker host address and a published port instead.

Enable import/download, upgrade, rename, and file-delete events where available. Radarr movie-delete and Sonarr series-delete events are also supported. Test the connection. Test events do not start searches. Sonarr per-file imports/upgrades (`episodeFile`) and import-complete batches (`episodeFiles`) are both accepted.

Whole-movie and whole-series deletion retires indexed media and its searches even when you keep the files on disk. Sonarr series deletion requires the stored Sonarr series ID: episode records created before this feature acquire it on their next normal import, rename, manual search, or history hydration. Until then, file-delete events and history reconciliation retain their existing behavior; series deletion does not guess ownership from titles or paths. Startup does not scan the library to backfill these IDs.

subsyncd checks retained Arr history on startup and normally every six hours. This is not a full-library scan: older files absent from that history need a later webhook or a manual search to be discovered.

After adding an instance, provider, or language, restart subsyncd. A new language schedules checks for media already indexed under your configured instances. Existing subtitles may satisfy those checks without a download.

## Missing subtitles and upgrades

subsyncd checks embedded tracks and separate subtitle files before searching. A full matching subtitle can satisfy a language; forced-only tracks cannot. Hearing-impaired tracks count only when `allow_hearing_impaired: true` is enabled.

When nothing suitable is found, searches continue automatically with longer intervals. Known deterministic rejections are retained without expiry; searches can try new candidates but do not periodically redownload rejected ones. Provider limits and outages can delay them. A scheduled check may reuse recent results rather than contact a provider again.

subsyncd can upgrade subtitles it installed when a better match becomes available. Manually added or edited subtitles are protected. If you delete a managed subtitle, it can be downloaded again on a later search. TV files containing multiple episodes are currently skipped.

See [matching and upgrades](providers.md#matching-and-upgrades) for more about selection and timing checks.

## LAPSE cache expiry

LAPSE caches detected speech timings in `/data/lapse-cache` to avoid rescanning the same media for every subtitle. Configure its retention separately from the season-pack cache:

```yaml
lapse_cache:
  ttl: 720h # 30 days; default
```

Use a positive duration such as `168h` or `720h`; zero and negative values are rejected. Expiry is measured from each profile's last write, not its last use. Reading a cached profile does not extend its lifetime.

subsyncd removes expired profiles at mutating startup and checks hourly while serving. A sweep skips the cache while LAPSE is running and tries again on the next check. Only recognized profile files and expired temporary profile files are removed; symlinks, directories, and unknown files are left alone. Read-only diagnostics do not clean the persistent cache. Cleanup failures log a warning and leave acquisition available.

Expired profiles rebuild automatically when needed. This may require another media scan, but it does not remove installed subtitles, reset search schedules, or clear candidate rejections. Restart after changing the configuration.

## Inspect or search one file

To see why a subtitle was installed, skipped, or delayed:

```bash
docker compose exec subsyncd subsyncd explain --instance radarr-main --kind movie --file-id 42 --language en
```

Replace the example values with your configured instance, Arr **media-file ID**, and language. For TV, use `--kind episode` and the episode-file ID. These are file IDs, not movie or series IDs.

`explain` can run while the daemon is active. Manual searches need it stopped so two processes cannot write at once:

```bash
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd search --instance radarr-main --kind movie --file-id 42 --language en
docker compose up -d subsyncd
```

Review the command's result before restarting. Add `--retry-rejected` only when you deliberately want to reconsider previously rejected candidates for that file and language.

To reconcile retained history for an instance, use `scan --instance sonarr-main` in place of `search ...` in the same stop/run/start sequence. This also is not a full-library enumeration.

## Optional Silo refresh

subsyncd can ask Silo to rescan the containing folder after a subtitle is installed. Enter the admin API key directly in `config/config.yaml`:

```yaml
silo:
  enabled: true
  url: http://silo:8080
  api_key: 'your-silo-admin-api-key'
  path_mappings:
    - from: /media
      to: /mnt/media
```

Use Silo's reachable native API address and an admin API key. The mapping translates subsyncd's media paths to Silo's paths. Restart with `docker compose restart subsyncd` after changing the configuration.

A failed Silo refresh does not undo an installed subtitle. This integration supports Silo's pre-1.0 API; check [compatibility](references/silo.md) before upgrading Silo.

## Back up and update

Stop subsyncd and back up the complete host `data/` folder, along with your configuration and `.env`. Keep credential backups private. Back up subtitle files through your normal media-library backup.

```bash
docker compose stop subsyncd
# Back up data/, config/, and .env before continuing.
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To pin a build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or SHA tag. For rollback, use the matching image and complete stopped-data backup together. Check [release notes](release-notes.md) before upgrading older builds; unsupported pre-release databases require a fresh data directory.

Allow the container to stop fully before starting another process against the same media. The supplied Compose file allows 75 seconds for shutdown. Increase this if you increase `worker.shutdown_timeout`.

## Troubleshooting

| Problem | What to check |
| --- | --- |
| Permission denied | The configured UID/GID must read configuration and media, and write `data/` and subtitle folders. |
| Media path is outside configured roots | Match the Arr path mapping to the actual container mounts. |
| No subtitle download | Use `explain` to check existing subtitles, the next search time, provider limits, or rejected matches. |
| Provider authentication rejected | Correct the credentials in `config/config.yaml`, restart the service, then clear the affected provider's state as shown below. |
| Another mutation process is active | Stop the daemon before a manual search, scan, or retry. Do not delete the lock file. |
| LAPSE takes a long time | Its first run may read much of the media file, especially noticeable on network storage. Results are cached for later use. |
| LAPSE reports `unsure` or `nothing` | The candidate did not pass timing verification; subsyncd will consider other matches. |
| Silo does not refresh | Check the API address, admin key, path mapping, and notification logs. |

After fixing credentials, clear the named provider's saved authentication and cooldown state:

```bash
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd retry --provider opensubtitles-main
docker compose up -d subsyncd
```

For filesystem recovery errors, inspect the logs and `explain` before changing files. Do not delete `.subsyncd-*` staging or rollback files while the service is running.

Implementation details are kept in the [developer operations reference](development/operations.md).
