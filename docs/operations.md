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

Instance settings, path mappings, and webhooks are set up during installation: see [set paths and credentials](../README.md#2-set-paths-and-credentials) and [add Sonarr and Radarr webhooks](../README.md#4-add-sonarr-and-radarr-webhooks). Each instance needs a unique name, reachable URL, API key, and path mapping.

Besides import/download, upgrade, rename, and file-delete events, Radarr movie-delete and Sonarr series-delete events are supported. Sonarr per-file imports/upgrades (`episodeFile`) and import-complete batches (`episodeFiles`) are both accepted. Test events do not start searches.

When Arr upgrades or re-imports a movie or episode, the subtitle subsyncd installed for the old file is deleted before it searches for the new one, whether Arr left it under the old name or renamed it for the new file. A subtitle you edited, added yourself, or linked with a symlink is never deleted; subsyncd only stops managing it. When Arr renames a file, a subtitle it installed follows: if Arr did not rename the subtitle too, subsyncd moves it to the new name (never over another file), and if it is gone, subsyncd searches again.

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

Completed discovery is remembered across restarts. Changed path mappings or media roots trigger another discovery pass. A failed or interrupted pass is retried with reconciliation backoff. If a webhook commits during enumeration, the snapshot is discarded and retried to avoid importing stale state. Startup validation and readiness stay local/offline; library API requests happen only during background work or an explicit scan. After discovery, retained Arr history is reconciled every six hours, with webhooks providing immediate updates. The schedule survives restarts: a restart or update does not trigger a reconciliation unless one is due, the instance is new, or its URL or path mappings changed. Run `scan --instance NAME` to reconcile one instance now.

If a webhook updates the catalog while history is being fetched, subsyncd discards the stale history snapshot and retries later. Newer deletions, replacements and renames remain intact, and the saved history position advances only after a successful reconciliation. Reconciliation also reads back the imports and renames its webhooks already delivered; when the file is unchanged since then, it does not search for that file's subtitles again.

If Arr reports a current file that subsyncd cannot read, such as a symlink whose target is missing or unavailable, reconciliation applies every other change, keeps its history position, and logs `reconcile.deferred` (warn) naming up to ten affected movies or episodes. Library discovery treats such a file the same way: it indexes every readable file, names the unreadable ones, and stays incomplete so the next check enumerates the library again. It checks again every hour and moves on once the file is readable or Arr has replaced or removed it. Until then the warning repeats, so replace or remove the broken file in Arr. `scan` lists the deferred items in its summary and still completes.

After adding an instance, provider, or language, restart subsyncd. A new language schedules checks for media already indexed under your configured instances. Existing subtitles may satisfy those checks without a download.

### Webhook connections

subsyncd generates a random webhook token for each instance on first start and keeps it in its database. `subsyncd webhook-url --instance NAME` prints the URL for a manual Arr webhook: complete when `--base URL` is given or `server.webhook_base_url` is set (`--base` wins), otherwise only the `/webhooks/...?token=...` path and query. For a manual webhook, pass the address Arr reaches subsyncd at, for example `--base http://subsyncd:8097`; it works while the daemon runs. The URL contains the token, so treat the output like a password.

With `server.webhook_base_url` set, the daemon creates and maintains one webhook connection per instance in Sonarr and Radarr, named `subsyncd (<host:port>)`. It checks once at start and then every six hours, retrying a failure after 15 minutes, then 1 hour. Startup and readiness never wait for Arr. Management changes only that connection's URL, method and required events (import, upgrade, rename, and the file and series or movie delete events). It never changes the connection's name, tags or other events, never touches other connections, and never deletes anything. A connection you created by hand at subsyncd's own address is adopted rather than duplicated; one that points at another subsyncd deployment is left alone.

Removing an instance, or unsetting `server.webhook_base_url`, leaves the connection in Arr. Delete it there if you no longer want it.

`rotate-webhook-token --instance NAME` replaces an instance's token. Run it with the daemon stopped, then start the daemon: a managed connection is updated at that start, and a manual one needs the new URL from `webhook-url --base URL` entered in Arr. Backups of the data directory contain the webhook tokens, so keep them private.

## Missing subtitles and upgrades

subsyncd checks embedded tracks and separate subtitle files before searching. A full matching subtitle can satisfy a language; forced-only tracks cannot. Hearing-impaired tracks count only when `allow_hearing_impaired: true` is enabled.

Embedded tracks also count as forced when their title contains an explicit label such as `English [Forced]`, even if the container's forced disposition is absent.

When nothing suitable is found, searches continue automatically with longer intervals. For media released more than `aged_release_days` ago (default 30) the next search is a week later, then every two weeks, instead of after 6 hours, 12 hours, 24 hours and 3 days. Known deterministic rejections are retained without expiry; searches can try new candidates but do not periodically redownload rejected ones. Provider limits and outages can delay them. A scheduled check may reuse recent results rather than contact a provider again.

subsyncd can upgrade subtitles it installed when a better match becomes available. Manually added or edited subtitles are protected. Unsuccessful upgrade checks retain the installed subtitle and gradually back off to approximately 90 days, with jitter to spread searches. After `upgrade_checks` unsuccessful checks in a row (default 4; `0` never stops) the search is marked complete; fallback installations keep checking for a preferred provider. If you delete a managed subtitle, it can be downloaded again on a later search; unsuccessful reacquisition restarts the normal missing-subtitle retries. Once a search is complete (its upgrade checks have ended, or it holds a preferred exact match), no later search runs on its own, so a deleted subtitle stays missing until the media file changes or you run a manual `search` for it.

Optional per-language `fallback_providers` supply subtitles when preferred providers have no installable result or are unavailable. Fallback installations get weekly preferred-provider checks, including exact matches; for media released less than `aged_release_days` ago the first four come after about 6 hours, 12 hours, 24 hours and 3 days instead, and a known preferred cooldown can bring the first check forward. `explain` displays the stored `fallback` flag. See [provider tiers](providers.md#choose-languages) for configuration and promotion rules.

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

The container runs `serve` by default. Run every other command beside it with `docker compose exec subsyncd subsyncd COMMAND ...`.

| You want to | Run |
| --- | --- |
| See why a file has no subtitle | `find --title "Show S01E02"`, then `explain` |
| Search for a subtitle now | `search --title "Movie (2019)"` |
| Get rid of a bad subtitle | `reject --title "Movie (2019)" --reason out_of_sync` |
| See everything still missing | `missing` |
| See what happened lately | `recent` |
| Check the service and providers | `status`, `providers`, `doctor` |

Only one process may write saved state at a time. Read-only commands simply read it. `search`, `reject`, `scan` and `retry` hand their request to the running daemon over a local socket (`subsyncd.sock` in the data folder, readable only by the daemon's user), so they need no stop; without a running daemon they run in their own process instead. Only `rotate-webhook-token` and `forget-instance --yes` need the daemon stopped.

| Command | What it does | Daemon |
| --- | --- | --- |
| `doctor` | Checks configuration, database, media folders, and bundled tools. | May run |
| `find` | Lists the files that match a title or path, with the state of each configured language (`installed`, `satisfied`, `running`, `missing`). | May run |
| `missing` | Lists the files and languages that have no subtitle yet, each with a reason and a command to fix it. See [What is missing](#what-is-missing). | May run |
| `recent` | Lists what happened in a time window, newest first: subtitles installed or replaced, searches that found nothing or only rejected candidates, your rejections, and removed subtitles. See [What happened recently](#what-happened-recently). | May run |
| `status` | One overview of the daemon and queue: whether the daemon runs (version, start time), the queue by class with how many searches are due, running searches, providers that cannot be used, the media-unavailable pause, each Arr instance, and the webhook and notification backlogs. See [Status and providers](#status-and-providers). | May run; asks the daemon when it can, and works without it |
| `providers` | Shows every configured provider: the languages it serves and at which tier, its state (`available`, `cooling_down`, `auth_failed`, `quota_exhausted`), its last failure and last success, and what its searches did in the last 24 hours. | May run |
| `explain` | Shows what subsyncd knows about one file, for one language or for every configured language: existing subtitles, candidates, rejections, installation, and the next search. | May run |
| `analyze-sync` | Runs a LAPSE timing check of a subtitle file against a media file without installing anything. | May run |
| `search` | Searches one file now, for one language or for every configured language it is missing, and waits for the outcome. | May run; the daemon carries it out |
| `reject` | Marks a subtitle subsyncd installed as wrong so it is never chosen again, and searches for a replacement now; removes the subtitle when nothing acceptable is found. `--undo` takes the rejection back. See [A subtitle is wrong](#a-subtitle-is-wrong). | May run; the daemon carries it out |
| `scan` | Discovers unindexed files in one instance's library and reconciles its history. Add `--force-probe` to reread embedded subtitle tracks. See [Scanning a library](#scanning-a-library). | May run; the daemon carries it out |
| `retry` | Clears one provider's saved cooldown and authentication state, and wakes the worker so waiting searches run. | May run; the daemon carries it out |
| `webhook-url` | Prints an instance's webhook URL, including its token, for manual Arr setup. Pass `--base URL` (or set `server.webhook_base_url`) for a complete URL; otherwise only the path and query are printed. | May run |
| `rotate-webhook-token` | Replaces an instance's webhook token. Start subsyncd afterwards; the URL in Arr is updated at that start with `server.webhook_base_url`, otherwise update it by hand from `webhook-url --base URL`. | Stop first |
| `forget-instance` | Shows what subsyncd has saved for a Sonarr or Radarr instance you removed from the configuration. Add `--yes` to delete those records. See [Remove an instance for good](#remove-an-instance-for-good). | May run to preview; stop first with `--yes` |

Run the stop-first commands with a stop/run/start sequence like the one under [Remove an instance for good](#remove-an-instance-for-good). File paths given to `analyze-sync` are paths inside the container. Put the subtitle you want to test in the host `data/` folder rather than beside the media, where subsyncd would treat it as your own subtitle and protect it:

```bash
docker compose exec subsyncd subsyncd analyze-sync --media /media/movies/Movie/Movie.mkv --subtitle /data/candidate.srt
```

Delete the test file from `data/` afterwards.

Exit status is 0 for success, 1 for an operational failure, and 2 for invalid command usage. `subsyncd --version` prints the build version.

### Machine-readable output

Every command except `serve` accepts `--json`. With it, standard output carries exactly one JSON object and nothing else, whether the command succeeded or failed; log lines still go to standard error. Without it, the output is the plain text described above.

```bash
docker compose exec subsyncd subsyncd doctor --json
```

A success looks like this, where `result` holds the command's own fields:

```json
{"schema":"subsyncd.cli/v1","command":"doctor","ok":true,"result":{"configuration":"ok","sqlite":"ok","media_roots":1,"lapse":"compatible","ffprobe":"available","webhooks":"manual"}}
```

A failure has `"ok":false` and an `error` with a stable `code` and a human-readable `message` in place of `result`. Usage mistakes are reported the same way, so `search --json` without the required flags prints a `usage` error on standard output and exits 2. Messages never contain credentials or absolute paths. The only exception to path-free output is `webhook-url`, which prints the token by design.

Run `subsyncd COMMAND --help` for a command's flags, result fields, and error codes. Times are RFC 3339 and field names are snake_case.

| Code | Meaning |
| --- | --- |
| `usage` | A flag is missing, unknown, or invalid for the command. Exit status 2. |
| `unknown_instance` | The instance is not configured (or, for `forget-instance`, not in the database). |
| `unknown_provider` | The provider is not configured. |
| `unknown_language` | The language tag is invalid or not configured. |
| `media_not_found` | `--title` or `--path` matched no indexed file, or `--file-id` is not indexed. Check the spelling with `find`. |
| `ambiguous_media` | `--title` or `--path` matched several files. The text output lists them one per line; with `--json` they are in `error.candidates` (`instance`, `kind`, `file_id`, `title`, and year, season or episode). Narrow the selection or use `--file-id`. |
| `busy` | Another subsyncd process holds the data directory and the command cannot go through it: a stop-first command while the daemon runs, or any changing command while another one-shot command runs or the daemon is still starting. Wait, or stop the daemon for a stop-first command, and retry. |
| `not_managed` | `reject` was pointed at a subtitle subsyncd does not manage: an embedded track, your own sidecar, or a sidecar subsyncd installed that has since been edited or replaced. Nothing was changed. |
| `daemon_required` | `--timeout` or `--no-wait` was given but no daemon is running. Without one, `search`, `reject` and `scan` run to completion in their own process; drop the flag or start subsyncd. |
| `daemon_stopping` | The daemon is shutting down, or closed the connection without answering. A search or scan it was running is canceled (a search stays queued); retry once subsyncd is back. |
| `protocol_mismatch` | The CLI and the running daemon are different builds that speak different socket protocols. Run the CLI from the same image as the daemon. |
| `arr_unreachable` | Sonarr or Radarr did not answer when `search` looked up the file (with `--file-id`, `--title` or `--path`). Check the instance `url`. |
| `internal` | Anything else. The `message` is redacted; the logs on standard error hold the detail. |

Treat an unrecognized code as a failure.

## Inspect or search one file

A subtitle is missing in Silo and you want to know why. Name the file by title or path:

```bash
docker compose exec subsyncd subsyncd find --title "Dune (2021)"
docker compose exec subsyncd subsyncd explain --title "Shameless S01E01" --language en
docker compose exec subsyncd subsyncd explain --path /tv/Show/S01E01.mkv
```

`--title` takes `Dune`, `Dune (2021)`, `Show S01E02`, `Show 1x02`, or `Show - S01E02 - Episode Title`. Case, accents, and punctuation do not matter (`amelie` finds Amélie), and a title that matches exactly wins over one that only contains your words. `--path` takes a path inside the container, or the path Sonarr or Radarr shows for the file; subsyncd translates it with that instance's `path_mappings`. Silo's paths are not understood. `--instance` and `--kind` narrow either form.

`find` lists every match with the state of each configured language, so it is also the way to look something up: `installed` means subsyncd put a subtitle there, `satisfied` means a track or sidecar already covers the language, `running` means a search is in progress now, and `missing` means none of those. When several files match, `find` prints them all and succeeds. `explain` and `search` need one file: if several match they stop with `ambiguous_media` and list the candidates, so add a year, season and episode, or `--instance`/`--kind`, or take the `file_id` from the list. `explain` without `--language` covers every configured language. Rejections made under older rules, another LAPSE version, or for the file before it was replaced no longer count; `explain` leaves them out and only counts them (`candidate_rejections_superseded`). With `--json` its result then holds the media and its tracks at the top level and one full result per language in `languages`.

You can still select by Arr **media-file ID**, which is the ID of the file, not of the movie or series. For TV use the episode-file ID:

```bash
docker compose exec subsyncd subsyncd explain --instance radarr-main --kind movie --file-id 42 --language en
```

To search one file now, run `search` beside the daemon. The daemon runs it next to its own queue and the command waits for the outcome:

```bash
docker compose exec subsyncd subsyncd search --title "Shameless S01E01" --language en
docker compose exec subsyncd subsyncd search --instance radarr-main --kind movie --file-id 42
```

Without `--language`, `search` covers every configured language that `find` reports `missing`; if none is, it searches nothing and reports each language `satisfied`. `search` takes `--title` or `--path` only for files subsyncd already knows. A file it has not indexed yet can be searched only by `--file-id`. Either way, `search` then asks Sonarr or Radarr for the file's current details, so an instance that does not answer fails it with `arr_unreachable`.

The command waits at most `--timeout` (default `5m`). A language still searching then reports outcome `running` (exit status 0) and the search carries on in the daemon; check on it later with `find` or `explain`. `--no-wait` returns at once with `running`. Losing the connection, for example when an SSH session drops, never cancels the search; only stopping the daemon does, and the search then stays queued. At most two manual searches run at once; more wait their turn in the order they arrived. If the daemon is already searching that file and language, the manual search waits for it to finish and then runs, and that wait counts against `--timeout`.

A search can also report `removed`. That happens when a replacement queued by `reject` is still pending for the language (for example, because providers were throttled when you rejected the subtitle): `search` runs that replacement, and when it finds nothing acceptable it removes the rejected subtitle, as `reject` would (see [A subtitle is wrong](#a-subtitle-is-wrong)).

When the daemon is not running, `search` runs in its own process to completion instead, and `--timeout` and `--no-wait` fail with `daemon_required`:

```bash
docker compose run --rm --no-deps subsyncd search --instance radarr-main --kind movie --file-id 42 --language en
```

A manual search asks every provider for the language afresh, preferred ones first, then fallbacks. When it installs a subtitle, the file's next search becomes that subtitle's first upgrade check, as after an automatic install; otherwise it keeps the file's existing schedule, including later upgrade checks. Searching a file subsyncd does not know yet, or one it recorded as deleted, first adds it the way an Arr import would, so every configured language is scheduled. Add `--retry-rejected` to reconsider the candidates subsyncd itself rejected for that file and language; subtitles you rejected with `reject` stay rejected (use `reject --undo` for those).

### Scanning a library

To discover any unindexed files in the full library and reconcile retained history, run `scan` beside the daemon:

```bash
docker compose exec subsyncd subsyncd scan --instance sonarr-main
```

The daemon carries the scan out between its own scheduled reconciliations, never during one, and the command waits for the summary. Like `search`, it waits at most `--timeout` (default `5m`); a scan that is still running then reports `running` (exit status 0) and finishes in the daemon, and `--no-wait` returns at once. Stopping the daemon cancels a scan it is running. A scan with `--force-probe` skips, and counts as `probe_skipped=N`, any file that was replaced or deleted while it ran. When a scan reports `running`, its result comes later as `scan.completed` (or `scan.failed`) in the [logs](logging.md); re-run the command to see the summary. Review the summary: the command queues new work, which the daemon processes as usual. Rescanning does not reset existing search schedules, reconsider retained deletions, or overwrite installation provenance.

Without a running daemon, `scan` runs in its own process to completion, and `--timeout` and `--no-wait` fail with `daemon_required`.

## What is missing

```bash
docker compose exec subsyncd subsyncd missing
docker compose exec subsyncd subsyncd missing --language hr --title "Show" --limit 20
```

`missing` lists every active file and configured language that has no subtitle: no installation, and no track or sidecar that covers the language (a forced-only track does not count, and a hearing-impaired one counts only with `allow_hearing_impaired`). `--instance`, `--language` and `--title` narrow the list, and `--limit` (default 50) cuts it. The result gives the full `total`, so a caller knows when the list was cut. Rows follow the queue, so what would be searched next comes first; languages with no search yet come last. A search running right now is listed too, without a next attempt time. The command only reads saved state, so it runs beside the daemon.

Each row has a `reason` and an `action`:

| Reason | Meaning | Action |
| --- | --- | --- |
| `not_searched` | No search has run for it yet. | `subsyncd search --instance … --kind … --file-id … --language …` |
| `no_candidates` | The last search found no usable candidate. | The same `search` command. |
| `all_rejected` | Every candidate was rejected. | The `search` command with `--retry-rejected`, which reconsiders the ones subsyncd rejected (not those you rejected with `reject`). |
| `providers_cooling_down` | The last search was throttled, or every provider of the language's route is cooling down or out of quota. `providers` names them. The search resumes by itself at the reset. | `subsyncd providers`, which shows when each resets. When a provider rejected its credentials: `subsyncd retry --provider NAME`, after fixing them. |
| `search_failing` | The last search failed technically (network, LAPSE, or a media outage: the saved state cannot tell which). It retries by itself at `next_attempt_at`. | `subsyncd explain --instance … --kind … --file-id … --language …` shows the cause; the logs have the details. |
| `unsupported` | subsyncd does not search this file, for example a multi-episode file. | A short instruction: replace the file in Sonarr or Radarr. |

Actions are complete commands with the real instance, kind, file ID and language; run them through `docker compose exec subsyncd` like the others. `unsupported` gives an instruction instead of a command. `missing` never reads media files, so it works even while storage is down.

## What happened recently

```bash
docker compose exec subsyncd subsyncd recent
docker compose exec subsyncd subsyncd recent --since 72h --language hr --limit 20
```

`recent` lists finished searches and rejections from the last 24 hours, newest first. `--since` takes a Go duration such as `90m` or `72h`, `--instance` and `--language` narrow the list, and `--limit` (default 50) cuts it. Each row gives the title, language, outcome (`installed`, `no_result`, `rejected`, `removed`, and so on), the provider and candidate release when there was one, a `reason` (a code starting with `user_` marks a rejection you made with `reject`, which gets its own `rejected` row before the replacement's result), whether the search was `manual`, and when it finished. The history is kept for 30 days, so a longer `--since` shows no more than that, and a file subsyncd has since forgotten still appears under its stored title. The command only reads saved state, so it runs beside the daemon.

## Status and providers

```bash
docker compose exec subsyncd subsyncd status
docker compose exec subsyncd subsyncd providers
```

`status` reads saved state, so it works with or without the daemon and never fails because the daemon is unreachable. It shows:

- `daemon`: whether it is running. Only a running daemon adds its `version`, `started_at` and any `media_pause`; `daemon.error` says why one that answered wrongly could not be asked.
- `queue`: pending searches as `new` (just imported or renamed, or queued by `reject`), `missing` and `upgrade`, each with a `total` and how many are `due` now.
- `running`: searches in progress, with title, language and `lease_until` (when the search's claim expires; its start time is not kept).
- `providers`: only the providers that cannot be used right now.
- `instances`: each Arr instance's last reconciliation (`reconcile_failures` is 0 when it succeeded), whether library discovery is complete, and its webhook state (`connected`, `pending`, or `unmanaged` without `server.webhook_base_url`).
- `backlog`: waiting webhooks and Silo notifications.

`providers` lists every configured provider with the languages it serves and its state:

- `available`: usable now.
- `cooling_down`: after a rate limit, a busy service, a blocked client or a transient failure, until the time in `until`.
- `quota_exhausted`: its daily download quota is used up, until the reset in `until`.
- `auth_failed`: it rejected the credentials. Fix them, restart, then run `retry --provider ID`.

Cooldowns and quotas end by themselves, and waiting searches resume then; `retry` cannot refill a quota, so use it only after fixing credentials or when a cooldown looks wrong. `last_failure` is the stored reason, and `last_success_at` and `last_24h` come from the 30-day search history.

## A subtitle is wrong

You are watching in Silo and the subtitle is off: out of sync, in the wrong language, made for another release, or just poor. Tell subsyncd, and it drops that subtitle and looks for another one right away:

```bash
docker compose exec subsyncd subsyncd reject --title "Movie (2019)" --language en --reason out_of_sync
```

`--reason` is one of `out_of_sync`, `wrong_language`, `wrong_release`, `poor_quality` or `other`. Select the file as for `search` (`--title`, `--path` or `--file-id`). You may leave out `--language` when subsyncd manages a subtitle for that file in only one language.

What happens:

- subsyncd records that this subtitle (that provider's result, for this media file) is wrong and never picks it again. Unlike the rejections subsyncd makes on its own, yours does not expire, survives LAPSE upgrades and policy changes, and is kept by `search --retry-rejected`. It does end when Sonarr or Radarr replaces the media file, since the new file gets a fresh search anyway.
- It then searches for that language at once, waiting and reporting like `search`. The rejected subtitle no longer counts as installed, so the best acceptable remaining candidate replaces it even if it scores lower; the usual "only upgrade to something clearly better" rule does not apply. The replacement is installed the normal way, atomically and with its provenance recorded, and Silo is asked to rescan the folder. A candidate with exactly the same text as the rejected subtitle counts as the same subtitle. A replacement in another subtitle format (say `.ass` for an `.srt`) is not installed over it.
- **If no acceptable replacement turns up, the rejected subtitle is deleted** (outcome `removed`, with `reason` saying whether the search found nothing or only unsuitable candidates). Silo is asked to rescan the folder, `find` shows the language as `missing` again, and the normal missing-subtitle searches carry on from there. A hidden `.subsyncd-rollback-` copy of an older subtitle that an earlier version left beside it is deleted too, not restored: you already moved past that one.
- If another subtitle track already covers the language (an embedded track or a subtitle file of your own), nothing will replace the rejected file, so it is removed without a search (outcome `removed`, reason `satisfied`). Your own subtitle is never touched.
- Removal only happens when the search actually finished. If providers were throttled, or any provider's search failed (a timeout or a server error, even while the others answered), the subtitle stays; the search stays queued and retries the replacement later. The same is true if subsyncd is stopped in the middle: the rejection is saved before the search starts, and the next run still replaces or removes the subtitle.

`reject` only acts on subtitles subsyncd installed and that are unchanged on disk. Embedded tracks, your own sidecars, and sidecars you edited are refused with `not_managed`, and nothing is changed. Removing a subtitle follows the same rules as installing one: only inside your media folders, and only while the file still matches what subsyncd wrote.

With the daemon running, `reject` waits like `search` (`--timeout`, default `5m`, or `--no-wait`) and the search carries on in the daemon if the wait ends first. Without a daemon it runs to completion in its own process, like `search`.

Rejected the wrong file or language? Undo it:

```bash
docker compose exec subsyncd subsyncd reject --title "Movie (2019)" --language en --undo
```

`--undo` removes your rejections for that file and language (it reports how many as `undone`) and runs no search; `recent` keeps its history of the earlier rejection. A replacement that was still queued (say, because providers were throttled) is called off too, so the subtitle you took the rejection back for is kept. A subtitle that was already replaced or removed stays that way; run `search --retry-rejected` or wait for the next scheduled search if you want the old one considered again. `--language` is required with `--undo` unless only one language is configured.

## Scripting and automation

Every command except `serve` (the daemon itself) works from scripts, cron jobs and other tools, on the host or from another machine over SSH. Use `-T` so Compose does not allocate a terminal, and `--json` for output a program can read:

```bash
ssh media-host 'cd /srv/subsyncd && docker compose exec -T subsyncd subsyncd search --title "Show S02E05" --timeout 4m --json'
```

(`media-host` and `/srv/subsyncd` stand for your host and Compose folder.) Commands never prompt and never pick a file on their own: an ambiguous title fails with `ambiguous_media` and lists the candidates. `search`, `reject` and `scan` wait at most `--timeout`; give it a value inside your own command time limit. A result of `running` still exits 0, and the work carries on in the daemon even if the connection drops. Branch on `error.code`, not the message, as listed under [Machine-readable output](#machine-readable-output): `busy` and `daemon_stopping` are worth retrying later. `rotate-webhook-token` and `forget-instance --yes` need the daemon stopped, so a script has to stop and start it around them.

## Remove an instance for good

When you remove a Sonarr or Radarr instance from the configuration, subsyncd keeps its records in case you add it back under the same name. If that library is gone for good, delete the records once you have removed the instance from `config.yaml`:

```bash
docker compose exec subsyncd subsyncd forget-instance --instance sonarr-old
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd forget-instance --instance sonarr-old --yes
docker compose up -d
```

The first command is a read-only preview, like `doctor`: it lists how many files, searches, installations, rejections, notifications, audit history, and recent search history it would delete, changes nothing, and works while subsyncd runs. The run with `--yes` deletes them all in one step, or nothing if it fails. subsyncd refuses an instance that is still configured or one the database has no records for.

Subtitle files it installed for that library stay on disk. It no longer knows it wrote them, so if those media files are ever indexed again it treats them as your own subtitles and won't replace them. When subsyncd replaces a subtitle it keeps a hidden backup copy, named `.subsyncd-rollback-` plus random characters, in the same folder only until the new one is recorded; earlier versions kept it until the next replacement, and the daemon deletes those at startup. The output counts any still recorded as `rollback_copies` (normally 0); nothing will clean them up after this, so delete them by hand if you want them gone. This can't be undone, so back up `data/` first if you might want the records back.

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

Replacing a movie or episode file queues a fresh refresh when subsyncd installs its subtitle, even when the subtitle bytes match the previous installation. Repeated notifications for the same media file, subtitle destination, language and content are sent once. Notifications due together for files in the same folder are sent as one Silo scan.

## Back up and update

Stop subsyncd and back up the complete host `data/` folder, along with your configuration and `.env`. Keep credential backups private. Back up subtitle files through your normal media-library backup.

```bash
docker compose stop subsyncd
# Back up data/, config/, and .env before continuing.
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To pin a build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or SHA tag. For rollback, use the matching image and complete stopped-data backup together, and remove config keys the older image does not know (for example `install.strip_watermarks`, added after `sha-25a2eae`), since unknown keys stop it from starting: an older image refuses a database that a newer one has upgraded. Databases from unsupported pre-release builds are rejected at startup and need a fresh data directory.

Allow the container to stop fully before starting another process against the same media. The supplied Compose file allows 75 seconds for shutdown, which covers both fixed 30-second shutdown windows; do not lower it.

## Troubleshooting

| Problem | What to check |
| --- | --- |
| Permission denied | The configured UID/GID must read configuration and media, and write `data/` and subtitle folders. |
| Media path is outside configured roots | Match the Arr path mapping to the actual container mounts. |
| No subtitle download | Use `explain` to check existing subtitles, the next search time, provider limits, or rejected matches. |
| Provider authentication rejected | Correct the credentials in `config/config.yaml`, restart the service, then clear the affected provider's state as shown below. |
| Another mutation process is active | Stop the daemon before `rotate-webhook-token` or `forget-instance --yes`; `search`, `reject`, `scan` and `retry` do not need a stop. Do not delete the lock file. |
| LAPSE takes a long time | Its first run may read much of the media file, especially noticeable on network storage. Results are cached for later use. |
| Media file is missing | The file disappeared after inventory refresh. The search stops before further candidate work and retries with technical-failure backoff, keeping its place in the queue; check Arr renames/deletions and filesystem availability. When three searches in a row cannot find or read their media and the storage itself does not read either (for example while a mount is remounting), searches pause for a minute (`queue.media_paused`); if the next searches fail the same way, the pause doubles each time, up to 30 minutes, until one reads its media. A single missing file on otherwise readable storage never pauses the queue. |
| `webhook.failed` warning | A saved Sonarr or Radarr webhook could not be applied, usually because Arr was unreachable. It is retried automatically; if it is given up (`outcome` `abandoned`), the next reconciliation applies the change from Arr's history. |
| LAPSE reports `unsure` or `nothing` | The candidate did not pass timing verification; subsyncd will consider other matches. |
| Silo does not refresh | Check the API address, admin key, path mapping, and notification logs. |
| `queue.route_paused` warning | Every provider for that language and media kind is in a cooldown, out of quota, or has rejected credentials. Searches resume after `reset_at`; for rejected credentials, fix them and run `retry`. |
| `reconcile.deferred` warning | Sonarr or Radarr reports a file subsyncd cannot read, such as a broken symlink. Fix or replace the file in Arr; the check repeats hourly. |
| `webhook.connection_sync_failed` warning | subsyncd could not keep an instance's Arr webhook connection correct; it retries by itself. The `reason` says why. `arr_unreachable`: Arr did not answer within 15 seconds; check the instance `url`. When this happens while saving the connection, Arr may instead be waiting on its own test call to `server.webhook_base_url` (for example a firewall that silently drops it); check that Arr can reach that address. `arr_status`: Arr answered with an unexpected status (`status`), such as 401, 404 or 5xx; check the instance `url` and API key. `arr_response_invalid`: Arr answered with something subsyncd does not understand; check that the instance is Sonarr or Radarr v3 or later. `arr_test_rejected`: Arr refused to save the connection (a 400 validation error, typically that its test of the webhook URL got an error from subsyncd); check `server.webhook_base_url` and any reverse-proxy path prefix. `arr_test_unreachable`: Arr could not reach subsyncd at that address; fix `server.webhook_base_url`, and allow subsyncd's address through Arr's HTTP proxy if one is set. `name_unavailable`: nine connection names are taken; delete a stale `subsyncd (...)` connection in Arr. `schema_missing_webhook`: this Arr offers no Webhook connection type. `internal`: a local failure such as an unreadable database; the redacted `error` and `error_kind` fields on the same line say what failed; fix it and restart. |
| `webhook.connection_ambiguous` warning | More than one Arr connection looks like this subsyncd's webhook for an instance, so none was changed. Delete the extras in Arr; the next check picks up the rest. |
| `webhook.connection_foreign` info | Arr has webhook connections for the same instance name that point at another subsyncd deployment. They are left alone and a connection of your own is created; no action needed. |
| `reconcile.snapshot_deletions_withheld` warning | Arr reported a library missing most of what subsyncd tracks, so the deletions were not applied. Check the instance URL and the Arr database. |

After fixing credentials and restarting the service, clear the named provider's saved authentication and cooldown state; this also makes searches that were waiting on a provider run again right away. The running daemon carries it out, so nothing needs stopping:

```bash
docker compose exec subsyncd subsyncd retry --provider opensubtitles-main
```

For filesystem recovery errors, inspect the logs and `explain` before changing files. Do not delete `.subsyncd-*` staging or rollback files while the service is running.

Implementation details are kept in the [developer operations reference](development/operations.md).
