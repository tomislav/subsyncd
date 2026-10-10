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

Logs show searches, downloads, archive-member selection, rejection reasons, synchronization, installations, and errors. See the [logging guide](logging.md) for debug settings and how to follow a particular search.

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

Instance settings, path mappings, and webhooks are set up during installation: see [set paths and credentials](../README.md#2-set-paths-and-credentials) and [add Sonarr and Radarr webhooks](../README.md#4-add-sonarr-and-radarr-webhooks). Each instance needs a unique name, reachable URL, API key, webhook secret, and path mapping.

Besides import/download, upgrade, rename, and file-delete events, Radarr movie-delete and Sonarr series-delete events are supported. Sonarr per-file imports/upgrades (`episodeFile`) and import-complete batches (`episodeFiles`) are both accepted. Test events do not start searches.

Whole-movie and whole-series deletion retires indexed media and its searches even when you keep the files on disk. Sonarr series deletion requires the stored Sonarr series ID: episode records created before this feature acquire it on their next normal import, rename, manual search, or history hydration. Until then, file-delete events and history reconciliation retain their existing behavior; series deletion does not guess ownership from titles or paths. Startup does not scan the library to backfill these IDs.

Reconciliation also retires movies and series that are missing from Arr's current catalog. If one reconciliation would retire every tracked movie or series for that instance, or more than half of them and at least 10, subsyncd withholds those deletions, still applies history, and logs `reconcile.snapshot_deletions_withheld`. This protects the catalog when Arr starts on an empty or restored database, or its URL points at a different instance (which numbers its items from 1 and so shares some IDs by coincidence). Fix the Arr instance or its configuration; the warning repeats on each reconciliation until the catalogs match again. Smaller removals, and removing a single tracked movie or series, apply normally, and Arr's movie-delete and series-delete webhooks always apply. If you really removed most of a library at once while the webhooks were not connected, those removals are not applied automatically and the warning keeps repeating.

After starting the daemon, subsyncd imports the existing Sonarr and Radarr libraries in the background once per instance. This also happens for already-configured instances after upgrading to a version with full-library discovery. It discovers movies and episode files within your path mappings even when their import history is gone, and queues subtitle checks for configured languages. Files already indexed keep their existing searches and installation records; suitable embedded or sidecar subtitles can satisfy checks without a download. Files containing several consecutive episodes of one season are searched as one range and only accept a subtitle covering all of them; other multi-episode files remain unsupported.

Searches run in a fixed order: imports and renames first, then missing subtitles, then upgrade checks. Within each of those, instances with a higher `queue_priority` go first, then the oldest queued work. Work queued together, such as a library scan or a newly added language, runs show by show from S01E01 with specials last, and movies alphabetically (remakes oldest first). Titles are compared ignoring case for unaccented letters only, so a title starting with an accented letter sorts after Z. For example, to cover the main libraries before lower-quality copies:

```yaml
instances:
  - name: radarr-main
    queue_priority: 30
    # ...
  - name: sonarr-main
    queue_priority: 20
    # ...
  - name: radarr-lq
    queue_priority: 10
    # ...
```

Instances without `queue_priority` rank 0. A provider cooldown does not change this order; see [Retries and provider limits](providers.md#retries-and-provider-limits).

Completed discovery is remembered across restarts. Changed path mappings or media roots trigger another discovery pass. A failed or interrupted pass is retried with reconciliation backoff. If a webhook commits during enumeration, the snapshot is discarded and retried to avoid importing stale state. Startup validation and readiness stay local/offline; library API requests happen only during background work or an explicit scan. After discovery, retained Arr history is reconciled normally every six hours, with webhooks providing immediate updates.

If a webhook updates the catalog while history is being fetched, subsyncd discards the stale history snapshot and retries later. Newer deletions, replacements and renames remain intact, and the saved history position advances only after a successful reconciliation. Reconciliation also reads back the imports and renames its webhooks already delivered; when the file is unchanged since then, it does not search for that file's subtitles again.

If Arr reports a current file that subsyncd cannot read, such as a symlink whose target is missing or unavailable, reconciliation applies every other change, keeps its history position, and logs `reconcile.deferred` (warn) naming up to ten affected movies or episodes. Library discovery treats such a file the same way: it indexes every readable file, names the unreadable ones, and stays incomplete so the next check enumerates the library again. It checks again every hour and moves on once the file is readable or Arr has replaced or removed it. Until then the warning repeats, so replace or remove the broken file in Arr. `scan` lists the deferred items in its summary and still completes.

After adding an instance, provider, or language, restart subsyncd. A new language schedules checks for media already indexed under your configured instances. Existing subtitles may satisfy those checks without a download.

## Missing subtitles and upgrades

subsyncd checks embedded tracks and separate subtitle files before searching. A full matching subtitle can satisfy a language; forced-only tracks cannot. Hearing-impaired tracks count only when `allow_hearing_impaired: true` is enabled.

Embedded tracks also count as forced when their title contains an explicit label such as `English [Forced]`, even if the container's forced disposition is absent.

When nothing suitable is found, searches continue automatically with longer intervals. Known deterministic rejections are retained without expiry; searches can try new candidates but do not periodically redownload rejected ones. Provider limits and outages can delay them. A scheduled check may reuse recent results rather than contact a provider again.

subsyncd can upgrade subtitles it installed when a better match becomes available. Manually added or edited subtitles are protected. Unsuccessful upgrade checks retain the installed subtitle and gradually back off to approximately 90 days, with jitter to spread searches. After `upgrade_checks` unsuccessful checks in a row (default 4; `0` never stops) the search is marked complete; fallback installations keep checking for a preferred provider. If you delete a managed subtitle, it can be downloaded again on a later search; unsuccessful reacquisition restarts the normal missing-subtitle retries.

Optional per-language `fallback_providers` supply subtitles when preferred providers have no installable result or are unavailable. Fallback installations get weekly preferred-provider checks, including exact matches; a known preferred cooldown can bring the first check forward. `explain` displays the stored `fallback` flag. See [provider tiers](providers.md#choose-languages) for configuration and promotion rules.

A first search that could not try every provider because one was in a cooldown is retried after that cooldown resets, and only the providers that had not yet finished are asked again. See [retries and provider limits](providers.md#retries-and-provider-limits).

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

## Commands

The container runs `serve` by default. The other commands are run through Compose. Commands that change saved state need the daemon stopped, because only one process may write at a time; read-only commands work while it runs.

| Command | What it does | Daemon |
| --- | --- | --- |
| `doctor` | Checks configuration, database, media folders, and bundled tools. | May run |
| `explain` | Shows what subsyncd knows about one file and language: existing subtitles, candidates, rejections, installation, and the next search. | May run |
| `analyze-sync` | Runs a LAPSE timing check of a subtitle file against a media file without installing anything. | May run |
| `search` | Searches one file and language now. | Stop first |
| `scan` | Discovers unindexed files in one instance's library and reconciles its history. Add `--force-probe` to reread embedded subtitle tracks. | Stop first |
| `retry` | Clears one provider's saved cooldown and authentication state. | Stop first |

Run read-only commands with `docker compose exec subsyncd subsyncd COMMAND ...`, and the others with the stop/run/start sequence shown below. File paths given to `analyze-sync` are paths inside the container. Put the subtitle you want to test in the host `data/` folder rather than beside the media, where subsyncd would treat it as your own subtitle and protect it:

```bash
docker compose exec subsyncd subsyncd analyze-sync --media /media/movies/Movie/Movie.mkv --subtitle /data/candidate.srt
```

Delete the test file from `data/` afterwards.

Exit status is 0 for success, 1 for an operational failure, and 2 for invalid command usage. `subsyncd --version` prints the build version.

## Inspect or search one file

To see why a subtitle was installed, skipped, or delayed:

```bash
docker compose exec subsyncd subsyncd explain --instance radarr-main --kind movie --file-id 42 --language en
```

Replace the example values with your configured instance, Arr **media-file ID**, and language. For TV, use `--kind episode` and the episode-file ID. These are file IDs, not movie or series IDs.

To find a file ID, search the logs for its title. The `job.started` record for that file carries `instance`, `media_kind`, and `file_id`:

```bash
docker compose logs subsyncd | grep job.started | grep 'Show Name - S01E02'
```

Movies appear as `Title (Year)` and episodes as `Show - S01E02 - Episode Title`. You can also read the ID from the Sonarr or Radarr API (`/api/v3/episodefile` or `/api/v3/moviefile`).

`explain` can run while the daemon is active. Manual searches need it stopped so two processes cannot write at once:

```bash
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd search --instance radarr-main --kind movie --file-id 42 --language en
docker compose up -d subsyncd
```

Review the command's result before restarting. A manual search clears any saved provider-resume progress for that media and language, then runs the complete current preferred/fallback route; it does not reuse a stale partial-cooldown route after a configuration change. Successful manual searches save any future upgrade check, including fallback promotion, while preserving earlier queued work and retained leases. Searching a file subsyncd does not know yet, or one it recorded as deleted, first adds it to the catalog the way an Arr import would, so every configured language is scheduled. The language you searched is marked complete when that run installs or finds a subtitle, so it is not searched again. Add `--retry-rejected` only when you deliberately want to reconsider previously rejected candidates for that file and language.

To discover any unindexed files in the full library and reconcile retained history, use `scan --instance sonarr-main` or `scan --instance radarr-main` in place of `search ...` in the same stop/run/start sequence. The command queues new work; the daemon processes it after restarting. Rescanning does not reset existing search schedules, reconsider retained deletions, or overwrite installation provenance.

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

A failed Silo refresh does not undo an installed subtitle. This integration requires Silo API v2 and sends `POST /api/v2/scan`; v1 is not supported. Set `silo.url` to the server base URL without `/api/v2`. Existing URL, admin-key, and path-mapping settings remain valid when upgrading. See [the API contract](references/silo.md).

Replacing a movie or episode file queues a fresh refresh when subsyncd installs its subtitle, even when the subtitle bytes match the previous installation. Repeated notifications for the same media file, subtitle destination, language and content are sent once.

## Back up and update

Stop subsyncd and back up the complete host `data/` folder, along with your configuration and `.env`. Keep credential backups private. Back up subtitle files through your normal media-library backup.

```bash
docker compose stop subsyncd
# Back up data/, config/, and .env before continuing.
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To pin a build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or SHA tag. For rollback, use the matching image and complete stopped-data backup together: an older image refuses a database that a newer one has upgraded. Databases from unsupported pre-release builds are rejected at startup and need a fresh data directory.

Allow the container to stop fully before starting another process against the same media. The supplied Compose file allows 75 seconds for shutdown, which covers both fixed 30-second shutdown windows; do not lower it.

## Troubleshooting

| Problem | What to check |
| --- | --- |
| Permission denied | The configured UID/GID must read configuration and media, and write `data/` and subtitle folders. |
| Media path is outside configured roots | Match the Arr path mapping to the actual container mounts. |
| No subtitle download | Use `explain` to check existing subtitles, the next search time, provider limits, or rejected matches. |
| Provider authentication rejected | Correct the credentials in `config/config.yaml`, restart the service, then clear the affected provider's state as shown below. |
| Another mutation process is active | Stop the daemon before a manual search, scan, or retry. Do not delete the lock file. |
| LAPSE takes a long time | Its first run may read much of the media file, especially noticeable on network storage. Results are cached for later use. |
| Media file is missing | The file disappeared after inventory refresh. The search stops before further candidate work and retries with technical-failure backoff, keeping its place in the queue; check Arr renames/deletions and filesystem availability. When three searches in a row cannot find or read their media (for example while a mount is remounting), searches pause for a minute (`queue.media_paused`); if the next searches fail the same way, the pause doubles each time, up to 30 minutes, until one reads its media. |
| LAPSE reports `unsure` or `nothing` | The candidate did not pass timing verification; subsyncd will consider other matches. |
| Silo does not refresh | Check the API address, admin key, path mapping, and notification logs. |
| `queue.route_paused` warning | Every provider for that language and media kind is in a cooldown, out of quota, or has rejected credentials. Searches resume after `reset_at`; for rejected credentials, fix them and run `retry`. |
| `reconcile.deferred` warning | Sonarr or Radarr reports a file subsyncd cannot read, such as a broken symlink. Fix or replace the file in Arr; the check repeats hourly. |
| `reconcile.snapshot_deletions_withheld` warning | Arr reported a library missing most of what subsyncd tracks, so the deletions were not applied. Check the instance URL and the Arr database. |
| `config.weak_webhook_token` warning | That instance's webhook secret is shorter than 16 characters. Replace it with `openssl rand -hex 32` output and update the Arr webhook URL. |

After fixing credentials, clear the named provider's saved authentication and cooldown state:

```bash
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd retry --provider opensubtitles-main
docker compose up -d subsyncd
```

For filesystem recovery errors, inspect the logs and `explain` before changing files. Do not delete `.subsyncd-*` staging or rollback files while the service is running.

Implementation details are kept in the [developer operations reference](development/operations.md).
