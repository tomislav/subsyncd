# Running subsyncd

This page covers how subsyncd runs after you [install it with Docker Compose](../README.md#install-with-docker-compose): what it needs from the host, how it follows Sonarr and Radarr, and what to protect. Commands assume you copied the Compose example to `compose.yaml`.

- [Check the service](#check-the-service)
- [Permissions](#permissions)
- [Resources and LAPSE run time](#resources-and-lapse-run-time)
- [Security](#security)
- [Following Sonarr and Radarr](#following-sonarr-and-radarr)
- [Queue order](#queue-order)
- [Webhook connections](#webhook-connections)
- [Sonarr and Radarr rescans](#sonarr-and-radarr-rescans)
- [Silo refresh](#silo-refresh)
- [Changing the configuration](#changing-the-configuration)

## Check the service

```bash
docker compose ps
docker compose exec subsyncd subsyncd doctor
docker compose exec subsyncd subsyncd status
```

- `doctor` checks the configuration, database, media folders, LAPSE and FFprobe. It does not contact Sonarr, Radarr, providers or Silo, so it cannot test their credentials. It reads the database without creating it, so start the service once first.
- `status` shows the daemon, the queue, providers that cannot be used and each Arr instance. See [Commands](commands.md#status-and-providers).
- The container health check calls `/readyz`, which checks the database and media folders. `/healthz` only reports that the HTTP service runs.

Follow activity with `docker compose logs -f subsyncd`. [Logs](logging.md) explains levels and events.

## Permissions

Set `PUID` and `PGID` in the Compose `.env` file to a host user and group that can do all of this (both default to `1000`; `id -u` and `id -g` print yours):

| Folder in the container | Access needed |
| --- | --- |
| `/config` | Read the configuration |
| `/data` | Read and write the database, caches and control socket |
| `/media/movies`, `/media/tv` | Read media and write subtitles beside it |
| `/tmp` | Scratch space for downloads and LAPSE, supplied by Compose |

- Create the host folders before the first start. Docker creates missing ones as root, and the container does not change ownership.
- New subtitles get mode `0644` (`install.file_mode`). Leave `install.uid` and `install.gid` unset unless you need another owner.
- Keep `/tmp` separate from your media folders.

## Resources and LAPSE run time

The Compose example limits the container to 2 GB of memory (`mem_limit`), two CPUs (`cpus`) and a 512 MB `/tmp`. LAPSE, the timing checker, uses most of it.

The image includes LAPSE and a minimal FFprobe 8.1; the general-purpose `ffmpeg` command is not included.

- **First check of a file is slow.** LAPSE decodes the media file's whole audio track the first time it checks a subtitle against it. It caches the result in `data/lapse-cache/`, so later checks of the same file take about a second. On network storage the first check reads the whole file over the network.
- **Time limit.** One LAPSE run may take `sync.timeout` (30 minutes). For a large file the limit grows to the time needed to read it at `sync.min_read_rate` (4 MiB/s), up to `sync.max_timeout` (4 hours). A run that times out fails the search with reason `lapse_timeout`, and it is retried later. On slow storage, lower `min_read_rate` below the speed you actually get.
- **`analyze-sync` always decodes.** It uses a throwaway cache, so every run reads the whole audio track.
- **Parallel searches.** Each search running at once (`worker.max_concurrent`, default 1) needs its own room in `/tmp` and its own LAPSE run. If you raise it, raise the tmpfs `size=512m` and probably `mem_limit` in `compose.yaml`.

## Security

| What | Why it matters | What to do |
| --- | --- | --- |
| `config/config.yaml` | Holds the Sonarr, Radarr, provider and Silo credentials. | Keep it private, or move values to environment variables with [`'${NAME}'`](configuration.md#credentials-and-environment-variables). |
| `data/` and its backups | The database holds each instance's webhook token. | Keep backups as private as the configuration. |
| Webhook URLs | The token travels in the URL's query string (`?token=…`), so it can appear in proxy, Sonarr or Radarr logs. `webhook-url` prints it by design. | Treat the URL like a password. If it leaks, run [`rotate-webhook-token`](#rotate-a-token). |
| Control socket `data/subsyncd.sock` | Commands such as `search` and `reject` reach the daemon through it. It is readable and writable only by the container user (mode `0600`). | Don't loosen its permissions. |
| `docker compose exec` | Anyone who can exec into the container can run every command, read the configuration and change subtitles. | Limit who can use Docker on the host. |

Logs redact credentials and absolute paths but contain media titles; see [Sharing logs](logging.md#sharing-logs).

## Following Sonarr and Radarr

Sonarr and Radarr supply the library; subsyncd never scans standalone folders. Each instance needs a unique name, a reachable URL, an API key and path mappings ([configuration](configuration.md#sonarr-and-radarr-instances)).

**Library discovery.** On first start, subsyncd reads each instance's library in the background and queues a subtitle check for every movie and episode file inside your path mappings. Embedded tracks and existing sidecar files can satisfy a check without a download. It discovers the library again when an instance's path mappings or the media roots change, and retries a failed discovery later.

**Webhooks and history.** Sonarr and Radarr report changes through webhooks right away. Every six hours subsyncd also reads each instance's history, which catches anything a webhook missed. That schedule survives restarts: a restart reconciles only an instance that is due, new, or has a changed URL or path mappings. Run `subsyncd scan --instance NAME` to reconcile one instance now. Startup and readiness never wait for Arr.

subsyncd follows these Arr events:

- **Import or upgrade.** A new file is searched. When a movie or episode gets a different file, the subtitle subsyncd installed for the old file is deleted first, and the language can be without a subtitle until the search finds one. A re-import of the very same file (same size, and the same name or a subtitle already named after it) keeps the subtitle.
- **Rename.** A subtitle subsyncd installed follows the media file. If Arr did not rename it too, subsyncd moves it, never over another file. If that is not safe (another filesystem, or no hard links or no-overwrite rename), subsyncd stops managing it and searches again.
- **File, movie or series deletion.** The media and its searches are retired, even if you keep the files on disk.
- **Multi-episode files.** A file with consecutive episodes of one season is searched as one range and only accepts a subtitle covering all of them. Other multi-episode files are not searched.

Safety limits:

- **Your subtitles are never deleted.** A subtitle you edited, added yourself or linked with a symlink is left alone; subsyncd only stops managing it.
- **At most 20 deletions per hour** for replaced media files, counted over a rolling hour in memory (a restart starts a new count). Past that, the old subtitle stays, `install.removal_deferred` is logged, and the search waits as `throttled` for the next free slot. This protects you when Arr re-imports a whole library at once.
- **Mass removals are withheld.** If one reconciliation would retire every tracked movie or series of an instance, or more than half and at least 10, subsyncd applies the rest of the history but not those deletions, and logs `reconcile.snapshot_deletions_withheld` each time. This happens when Arr starts on an empty or restored database, or its URL points at a different instance. Fix Arr or the URL. Movie-delete and series-delete webhooks always apply.
- **Unreadable files are deferred.** If Arr reports a file subsyncd cannot read, such as a broken symlink, reconciliation applies everything else, logs `reconcile.deferred` naming up to ten items, and checks again every hour. Replace or remove the file in Arr.

## Queue order

Searches run in this order:

1. Imports and renames.
2. Missing subtitles.
3. Upgrade checks.

Within each stage, instances with a higher `queue_priority` (0–1000, default 0) go first, then the oldest queued work. Work queued together, such as a library discovery or a new language, runs show by show from S01E01 with specials last, and movies alphabetically. A provider cooldown does not change the order.

For example, to cover the main libraries before lower-quality copies:

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

[When subsyncd searches](providers.md#when-subsyncd-searches) explains the retry and upgrade schedules.

## Webhook connections

subsyncd generates a random webhook token for each instance on first start and keeps it in its database. Sonarr and Radarr send events to `/webhooks/INSTANCE?token=TOKEN` on subsyncd's address. subsyncd saves each webhook, answers at once, then applies it. A webhook that cannot be applied, usually because Arr did not answer subsyncd's lookup, is retried after 1 minute, 5 and 15 minutes, 1 hour, 6 hours and 24 hours, then given up; the six-hourly history reconciliation still catches the change.

### Managed connections

With `server.webhook_base_url` set (the example sets `http://subsyncd:8097`), subsyncd creates and maintains one Webhook connection per instance in Arr, named `subsyncd (HOST:PORT)`.

- It checks the connection at start and every six hours, and retries a failure after 15 minutes, then 1 hour, then every six hours.
- It sets only that connection's URL, method and events (import, upgrade, rename, and the file and series or movie delete events). It never changes the connection's name or tags, never touches other connections and never deletes anything.
- A connection you made by hand at subsyncd's own address is adopted, not duplicated. One that points at another subsyncd deployment is left alone.
- Sonarr and Radarr test the URL when they save a connection, so they must reach `server.webhook_base_url`, including through their HTTP proxy if they use one.
- Removing an instance or unsetting `server.webhook_base_url` leaves the connection in Arr; delete it there if you want.

`status` shows each instance's webhook as `connected`, `pending` or `unmanaged`.

### Manual webhooks

Leave `server.webhook_base_url` unset and print each instance's URL (this works while the daemon runs):

```bash
docker compose exec subsyncd subsyncd webhook-url --instance sonarr-main --base http://subsyncd:8097
```

```text
http://subsyncd:8097/webhooks/sonarr-main?token=SONARR_WEBHOOK_TOKEN
```

Pass the address Sonarr or Radarr reach subsyncd at as `--base`; for an instance outside Docker, that is the Docker host's address and the published port. Without `--base` or `server.webhook_base_url`, only the path and query are printed.

In each application's **Settings → Connect**, add a Webhook connection with that URL and the `POST` method. Enable import (download), upgrade, rename, file delete, and series delete (Sonarr) or movie delete (Radarr). The connection's **Test** button checks the URL without starting a search; other event types are accepted and ignored.

### Rotate a token

```bash
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd rotate-webhook-token --instance sonarr-main
docker compose up -d
```

The daemon must be stopped. A managed connection gets the new URL at the next start; for a manual one, enter the new URL from `webhook-url` in Arr.

## Sonarr and Radarr rescans

Sonarr and Radarr notice subtitle files only when they rescan a series or movie. After subsyncd installs, replaces, moves or removes a subtitle, it asks that instance to rescan the series or movie (`RescanSeries` or `RescanMovie`, never a metadata refresh). The change then shows in Arr at once, a later rename moves the subtitle along, and a removed subtitle drops from Arr's list.

- The rescan is sent about a minute after the first change for a series or movie. Later changes join it while it waits, so a season pack sends about one rescan a minute, not one per episode.
- No answer, a timeout, 408, 429 or 5xx is retried after 1 minute, then 5 and 15 minutes, then hourly until Arr accepts it. Rescans survive restarts.
- Any other error, such as 404 for a deleted series or 401 for a wrong API key, gives the rescan up (`notification.abandoned`). Arr's own scheduled refresh catches up.
- A failed rescan never undoes a subtitle change.

It uses the instance's `url` and `api_key`. To turn it off for one instance, set `rescan_after_changes: false` under it and restart.

## Silo refresh

subsyncd can ask [Silo](https://github.com/Silo-Server/silo-server) to rescan a folder after it installs, replaces or removes a subtitle there (not after a move that follows an Arr rename):

```yaml
silo:
  enabled: true
  url: http://silo:8080
  api_key: 'your-silo-admin-api-key'
  path_mappings:
    - from: /media
      to: /mnt/media
```

- Use Silo's native API address without `/api/v2`, and an admin API key. Only Silo API v2 is supported.
- `path_mappings` translate subsyncd's media paths to Silo's paths.
- Changes in the same folder that are due together are sent as one scan, and repeats of the same change are sent once.
- No answer, 408, 409, 429 or 5xx is retried like a rescan. Any other status fails it for good (`notification.failed`).
- A failed refresh never undoes a subtitle change.

The API contract is in [the Silo reference](references/silo.md). Finished rescans and Silo refreshes are deleted from the queue 30 days later.

## Changing the configuration

Restart after any change to `config/config.yaml`; there is no reload:

```bash
docker compose restart subsyncd
```

- A new language schedules checks for media already indexed. Existing subtitles may satisfy them without a download.
- Removing a language stops its searches without deleting installed subtitles.
- A new instance is discovered in the background after the restart.
- Removing an instance keeps its records in case you add it back; [`forget-instance`](commands.md#remove-an-instance-for-good) deletes them.

[Configuration](configuration.md) lists every key.
