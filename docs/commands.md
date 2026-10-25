# Commands

subsyncd is controlled from the command line. The container runs `subsyncd serve`, the daemon. Run every other command beside it:

```bash
docker compose exec subsyncd subsyncd COMMAND [flags]
```

The rest of this page writes commands as `subsyncd COMMAND`; add the `docker compose exec subsyncd` prefix when you run them.

- [Getting help](#getting-help)
- [Command overview](#command-overview)
- [The daemon and one-shot commands](#the-daemon-and-one-shot-commands)
- [Choosing a file](#choosing-a-file)
- [find and explain](#find-and-explain)
- [search](#search)
- [scan](#scan)
- [coverage](#coverage)
- [missing](#missing)
- [recent](#recent)
- [Status and providers](#status-and-providers)
- [A subtitle is wrong](#a-subtitle-is-wrong)
- [analyze-sync](#analyze-sync)
- [Remove an instance for good](#remove-an-instance-for-good)
- [Machine-readable output](#machine-readable-output)
- [Scripting and automation](#scripting-and-automation)

## Getting help

- `subsyncd --help` (also `-h` or `help`) lists every command with a one-line summary. Running `subsyncd` with no command prints the same list after "command is required" and exits 2.
- `subsyncd COMMAND --help` shows the command's flags, result fields, outcomes and error codes, and exits 0.
- `subsyncd --version` (or `subsyncd version`) prints the build.
- Every command takes `--config PATH`. The default is the `SUBSYNCD_CONFIG` environment variable, else `/config/config.yaml`.

## Command overview

| You want to | Run |
| --- | --- |
| See how many subtitles each library has | `subsyncd coverage` |
| See why a file has no subtitle | `subsyncd missing`, then `subsyncd explain --title "Show S01E02"` |
| Look up a file | `subsyncd find --title "Dune (2021)"` |
| Search for a subtitle now | `subsyncd search --title "Movie (2019)"` |
| Get rid of a bad subtitle | `subsyncd reject --title "Movie (2019)" --reason out_of_sync` |
| See what happened lately | `subsyncd recent` |
| Check the service and providers | `subsyncd status`, `subsyncd providers`, `subsyncd doctor` |

| Command | What it does | With the daemon running |
| --- | --- | --- |
| `doctor` | Checks configuration, database, media folders, LAPSE and FFprobe, and shows the LAPSE version and rejection baseline. | Runs beside it |
| `status` | Overview of the daemon, queue, unusable providers and Arr instances. | Runs beside it |
| `providers` | Every provider with its languages, state and last 24 hours. | Runs beside it |
| `coverage` | Per library and language: files, subtitles installed or already present, and missing ones by reason. | Runs beside it |
| `missing` | Files and languages without a subtitle, with a reason and a fix. | Runs beside it |
| `recent` | Searches and rejections that finished in a time window. | Runs beside it |
| `find` | Files matching a title or path, with each language's state. | Runs beside it |
| `explain` | Everything subsyncd knows about one file and language. | Runs beside it |
| `analyze-sync` | Runs a LAPSE timing check without installing anything. | Runs beside it |
| `search` | Searches one file now and waits for the outcome. | The daemon runs it |
| `reject` | Marks a subtitle subsyncd installed as wrong and replaces or removes it. | The daemon runs it |
| `scan` | Discovers unindexed files of one instance and reconciles its history. | The daemon runs it |
| `retry` | Clears one provider's saved cooldown and credential state. | The daemon runs it |
| `webhook-url` | Prints an instance's webhook URL, token included. | Runs beside it |
| `rotate-webhook-token` | Replaces an instance's webhook token. | Stop it first |
| `forget-instance` | Previews, or with `--yes` deletes, a removed instance's records. | Preview beside it; stop it for `--yes` |

`webhook-url` and `rotate-webhook-token` are covered in [Webhook connections](running.md#webhook-connections).

## The daemon and one-shot commands

Only one process may change subsyncd's saved state at a time; `data/subsyncd.lock` enforces this. Read-only commands just read the database, so they run beside the daemon. Apart from `analyze-sync`, they never read a media file. Every command, read-only ones included, does check at startup that each configured media root exists: a missing root fails the command, and a hung mount can stall it.

`search`, `reject`, `scan` and `retry` hand their request to the running daemon through the control socket `data/subsyncd.sock`, so you never stop the daemon for them. Without a running daemon they run to completion in their own process:

```bash
docker compose run --rm --no-deps subsyncd search --title "Movie (2019)"
```

`rotate-webhook-token` and `forget-instance --yes` need the daemon stopped. Run them with `docker compose stop subsyncd`, then `docker compose run --rm --no-deps subsyncd COMMAND ...`, then `docker compose up -d`.

## Choosing a file

`find`, `explain`, `search` and `reject` select media in one of three ways:

- **`--title`** takes `Dune`, `Dune (2021)`, `Show S01E02`, `Show 1x02` or `Show - S01E02 - Episode Title`. Case, accents and punctuation do not matter (`amelie` finds Amélie), and an exact title wins over one that only contains your words.
- **`--path`** takes a path inside the container (such as `/media/tv/Show/Season 01/Show S01E01.mkv`), or the path Sonarr or Radarr shows for the file, translated with that instance's `path_mappings`. Silo's paths are not understood.
- **`--instance`, `--kind` and `--file-id`** name the Arr **media-file ID**: the ID of the file, not of the movie or series. For TV, use the episode-file ID.

`--instance` and `--kind` (`movie` or `episode`) also narrow `--title` and `--path`.

When several files match, `find` lists them all and succeeds. `explain`, `search` and `reject` need exactly one: they fail with `ambiguous_media` and list up to 25 candidates. Add a year, season and episode, use `--instance` or `--kind`, or take the `file_id` from the list.

## find and explain

```bash
subsyncd find --title "Dune (2021)"
subsyncd explain --title "Shameless S01E01" --language en
subsyncd explain --instance radarr-main --kind movie --file-id 42
```

`find` lists every match with the state of each configured language:

| State | Meaning |
| --- | --- |
| `installed` | subsyncd installed a subtitle. |
| `satisfied` | An embedded track or a sidecar already covers the language. |
| `running` | A search is running now. |
| `missing` | None of the above. |

A title with no match gives an empty list, not an error.

`explain` shows one file: existing tracks and sidecars, candidates, rejections, the installation, the next search, the pack cache and provider states. Without `--language` it covers every configured language. Rejections that do not apply any more (made under older rules, before a LAPSE update that can change timing results for rejections LAPSE decided, or for a file since replaced) are left out and only counted, as `candidate_rejections_superseded` in text and `rejections_superseded` in JSON. The result has a `media_id`, which log lines carry too.

## search

```bash
subsyncd search --title "Shameless S01E01" --language en
subsyncd search --instance radarr-main --kind movie --file-id 42
```

- **Languages.** Without `--language`, `search` covers every configured language that `find` reports `missing`. If none is, nothing runs and each language reports `satisfied` (or `running`, if a search is already running).
- **Unknown files.** `--title` and `--path` find only files subsyncd already knows. `--file-id` also works for a file it has not indexed, or one it recorded as deleted: it adds the file like an Arr import, so every configured language is scheduled.
- **Arr lookup.** `search` asks Sonarr or Radarr for the file's current details first, so an instance that does not answer fails it with `arr_unreachable`.
- **Providers.** A manual search asks every provider for the language afresh, preferred first, then fallbacks.
- **Rejections.** `--retry-rejected` reconsiders the candidates subsyncd rejected for that file and language. Subtitles you rejected with `reject` stay rejected; `reject --undo` removes those.
- **Schedule.** When it installs a subtitle, the file's next search becomes that subtitle's first upgrade check. Otherwise the file keeps its schedule.

**Waiting.** The command waits at most `--timeout` (default `5m`). A language still searching then reports `running` (exit status 0) and the search carries on in the daemon; check it later with `find` or `explain`. `--no-wait` returns at once with `running`. A dropped connection never cancels the search; only stopping the daemon does, and the search then stays queued. If the daemon is already searching that file and language, the manual search waits for it, and that wait counts against `--timeout`.

At most two manual searches run at once, shared with the searches that `reject` starts; more wait their turn. Without a daemon, `--timeout` and `--no-wait` fail with `daemon_required`.

**Outcomes:** `installed`, `satisfied`, `no_result`, `rejected`, `throttled`, `running`, and `removed`. `removed` happens when a replacement queued by `reject` was still pending and found nothing acceptable, so the rejected subtitle was removed as `reject` would.

## scan

```bash
subsyncd scan --instance sonarr-main
subsyncd scan --instance sonarr-main --force-probe
```

`scan` discovers files in the instance's library that subsyncd has not indexed and reconciles the instance's history. It queues new work, which the daemon processes as usual. It does not reset search schedules, reconsider retained deletions or overwrite installation records. `--force-probe` also rereads every file's embedded subtitle tracks.

The daemon runs the scan between its scheduled reconciliations and the command waits like `search` (`--timeout`, default `5m`, or `--no-wait`). Stopping the daemon cancels a scan it is running.

The summary reports:

| Field | Meaning |
| --- | --- |
| `media` | Media files subsyncd now tracks for the instance. |
| `history_deferred` | Some files could not be read; the scan completed for everything else. |
| `deferred`, `deferred_media` | How many files Arr reports that subsyncd cannot read (such as broken symlinks), and up to ten of their titles. They are checked again every hour. |
| `probe_skipped` | With `--force-probe`, files replaced or deleted while the scan ran. |

When a scan reports `running`, its result arrives later in the [logs](logging.md) as `scan.completed` (with `media`, `deferred` and `probe_skipped`) or `scan.failed`. Running `scan` again starts a new scan.

## coverage

```bash
subsyncd coverage
subsyncd coverage --instance sonarr --language hr
```

`coverage` gives one row for each instance and configured language:

- `media`: the instance's active files.
- `covered`: files that have the language, split into `installed` (subsyncd installed the subtitle) and `existing` (an embedded track or sidecar already covers it, or the last search found it `satisfied`).
- `missing`: exactly the rows `missing` lists, with `missing_by_reason` counting them by the reasons in the [missing](#missing) table, the largest first.

`media` is always `installed + existing + missing`: all the counts come from one database snapshot, so they agree even while the daemon is importing or installing. `--instance` (a configured instance) and `--language` narrow the rows. Like `missing`, it reads only the database.

An instance removed from the configuration keeps its records until [`forget-instance`](#remove-an-instance-for-good) deletes them, and `missing` still lists its files. `coverage` shows it after the configured instances, with `configured: false` and "not configured" in the text.

```text
sonarr hr: 500 files, 380 covered (300 installed, 80 existing), 120 missing
  missing: providers_cooling_down 90, no_candidates 20, all_rejected 10
```

## missing

```bash
subsyncd missing
subsyncd missing --language hr --title "Show" --limit 20
subsyncd missing --summary
```

`missing` lists every active file and language without a subtitle: no installation and no track or sidecar that covers the language. A forced-only track never counts; a hearing-impaired one counts only with `allow_hearing_impaired`.

- `--instance`, `--language` and `--title` narrow the list; `--limit` (default 50) cuts it, and `total` gives the full count.
- `--summary` lists no rows. It counts all of them by instance, language and reason in `summary[]` and takes no `--limit`. [coverage](#coverage) shows the same counts beside each library's totals.
- Rows follow the queue: what would run next comes first, languages never searched come last. A search running now is listed without a next attempt time.
- It reads no media file, only the database. Like every command, it first checks that each media root exists, so a missing root fails it and a hung mount can stall it.

Each row has a `reason` and an `action`. The action is a complete command with the real instance, kind, file ID and language.

| Reason | Meaning | Action |
| --- | --- | --- |
| `not_searched` | No search has run yet. | `subsyncd search ...` |
| `no_candidates` | The last search found nothing usable. | `subsyncd search ...` |
| `all_rejected` | Every candidate was rejected. | `subsyncd search ... --retry-rejected` |
| `providers_cooling_down` | The last search was throttled, or every provider for the language is cooling down or out of quota. It resumes by itself at the reset. | `subsyncd providers`; `subsyncd retry --provider NAME` when a provider rejected its credentials; `subsyncd search ...` when no provider is cooling down any more. |
| `search_failing` | The last search failed technically (network, LAPSE or media storage). It retries by itself at `next_attempt_at`. | `subsyncd explain ...` |
| `unsupported` | subsyncd does not search this file, such as a multi-episode file it cannot handle. | An instruction to replace the file in Sonarr or Radarr. |

## recent

```bash
subsyncd recent
subsyncd recent --since 72h --language hr --limit 20
subsyncd recent --outcome installed
```

`recent` lists finished searches and rejections, newest first, from the last 24 hours. `--since` takes a duration such as `90m` or `72h`; `--instance`, `--language` and `--outcome` narrow the list, and `--limit` (default 50) cuts it. `--outcome` takes one or more outcomes from the table below, comma-separated, such as `installed` or `installed,removed`. The limit counts only the rows the filters keep, so on a night with hundreds of `throttled` searches, `--outcome installed` still lists every install. History is kept for 30 days.

Each row has the title, language, outcome, provider and release when there was one, a `reason`, whether the search was `manual`, and when it finished. It also shows `instance` and `kind`; `file_id` is in the `--json` output only.

| Outcome | Meaning |
| --- | --- |
| `installed` | A subtitle was installed or replaced. |
| `satisfied` | Nothing needed doing. |
| `no_result` | Providers returned nothing usable. |
| `rejected` | Every candidate was rejected. A `reason` starting with `user_` is a rejection you made with `reject`; its replacement gets a row of its own. |
| `throttled` | Providers were rate limited or out of quota, so the search waits for their reset. That includes a search where other providers answered but nothing usable came back while one was skipped. Also used when the old subtitle of a replaced file is waiting for a deletion slot. |
| `removed` | A subtitle was removed: after `reject` found no replacement, or because Arr replaced or renamed the media file (`media_replaced`, `media_renamed`). |
| `workflow_error` | The search failed technically, for example on unreadable media. It retries by itself. |

A file removed in Arr still appears under its stored title. `forget-instance --yes` deletes an instance's history.

## Status and providers

```bash
subsyncd status
subsyncd providers
```

`status` works with or without the daemon. It asks the daemon for up to 2 seconds, and otherwise reports it as not running. It shows:

- `daemon`: whether it runs. Only a running daemon adds its `version`, `started_at` and any `media_pause`; `daemon.error` says why one that answered could not be asked.
- `queue`: pending searches as `new` (just imported or renamed, or queued by `reject`), `missing` and `upgrade`, each with a `total`, how many are `due`, and `next_at`, the earliest scheduled attempt of those not running (in the past while some are due). The text shows `next` only when none is due.
- `queue_by_library`: the same counts for each instance and language.
- `running`: searches in progress, with `lease_until` (when the search's claim expires; its start time is not kept).
- `providers`: only the providers that cannot be used now.
- `instances`: each Arr instance's last reconciliation (`reconcile_failures` is 0 when it succeeded), whether library discovery is complete, and its webhook state.
- `backlog`: waiting webhooks and notifications.

`providers` lists every configured provider with its languages, tier (`preferred` or `fallback`), state, last failure, last success and its search outcomes over the last 24 hours. The states:

| State | Meaning | What to do |
| --- | --- | --- |
| `available` | Usable now. | Nothing. |
| `cooling_down` | Paused after a rate limit (even a daily one), a busy service, a blocked client or repeated failures, until `until`. | Wait; searches resume by themselves. |
| `quota_exhausted` | A search or download quota is used up until `until`. | Wait; `retry` cannot refill a quota. |
| `auth_failed` | It rejected the credentials. It stays off until you act. | Fix the credentials, restart, then run `subsyncd retry --provider NAME`. |

`retry --provider NAME` clears the provider's saved cooldown and credential state and makes waiting searches due. Use it after fixing credentials or when a cooldown looks wrong.

## A subtitle is wrong

You are watching and the subtitle is off: out of sync, in the wrong language, for another release, or just poor. Tell subsyncd:

```bash
subsyncd reject --title "Movie (2019)" --language en --reason out_of_sync
```

`--reason` is `out_of_sync`, `wrong_language`, `wrong_release`, `poor_quality` or `other`. Select the file as for `search`. You may leave out `--language` when subsyncd manages a subtitle for the file in only one language.

What happens:

1. **The rejection is saved.** That provider's result is never picked again for this media file. Unlike subsyncd's own rejections, yours survives LAPSE upgrades and rule changes, and `search --retry-rejected` keeps it. It ends when Sonarr or Radarr replaces the media file, which gets a fresh search anyway.
2. **A replacement search runs at once**, waiting and reporting like `search`. The best acceptable remaining candidate replaces the subtitle even if it scores lower. A candidate with exactly the same text counts as the same subtitle, and one in another format (say `.ass` for an `.srt`) is not installed over it.
3. **If nothing acceptable turns up, the subtitle is deleted** (outcome `removed`). `find` then shows the language as `missing`, and the normal searches carry on.
4. **If another track already covers the language** (an embedded track or your own sidecar), the subtitle is removed without a search (outcome `removed`, reason `satisfied`). Your own subtitle is never touched.

Removal happens only when the search finished. If providers were throttled or any provider failed, the subtitle stays and the search retries later. Stopping subsyncd midway is safe: the rejection is saved before the search starts.

`reject` acts only on subtitles subsyncd installed that are unchanged on disk. Embedded tracks, your own sidecars and sidecars you edited fail with `not_managed`, and nothing changes. Sonarr, Radarr and Silo are told about a replacement or removal like any other change.

To take a rejection back:

```bash
subsyncd reject --title "Movie (2019)" --language en --undo
```

`--undo` removes your rejections for that file and language (reported as `undone`) and runs no search. A replacement still queued is called off, so the current subtitle stays. A subtitle already replaced or removed stays that way; run `search --retry-rejected` or wait for the next search. `recent` keeps the history. `--language` is required with `--undo` unless only one language is configured.

## analyze-sync

`analyze-sync` runs LAPSE on a media file and a subtitle file and prints LAPSE's verdict, without installing anything. Both paths are container paths, and the media must be inside a media root. Put the test subtitle in the host `data/` folder, not beside the media, where subsyncd would treat it as your own subtitle:

```bash
subsyncd analyze-sync --media /media/movies/Movie/Movie.mkv --subtitle /data/candidate.srt
```

Delete the test file from `data/` afterwards. Each run decodes the whole audio track; see [Resources](running.md#resources-and-lapse-run-time).

## Remove an instance for good

When you remove a Sonarr or Radarr instance from `config/config.yaml`, subsyncd keeps its records in case you add it back under the same name. If that library is gone for good, delete them. This can't be undone, so [back up `data/`](upgrade-and-backup.md#back-up) first.

```bash
docker compose exec subsyncd subsyncd forget-instance --instance sonarr-old
docker compose stop subsyncd
docker compose run --rm --no-deps subsyncd forget-instance --instance sonarr-old --yes
docker compose up -d
```

- The first command is a read-only preview. It counts the files, searches, installations, rejections, notifications, audit events and search history it would delete.
- With `--yes` it deletes them all in one step, or nothing if it fails.
- It refuses an instance that is still configured, and one the database has no records for.
- Subtitle files stay on disk. subsyncd has no record of writing them, so if those media files are indexed again it treats them as your own.
- `rollback_copies` counts hidden `.subsyncd-rollback-*` copies still recorded (normally 0). Nothing cleans them up afterwards; delete them by hand.

## Machine-readable output

Every command except `serve` and `version` accepts `--json`. Standard output then carries exactly one JSON object, on success or failure; log lines still go to standard error. Help (`--help`, `help COMMAND`) stays plain text even with `--json`.

```bash
subsyncd doctor --json
```

```json
{"schema":"subsyncd.cli/v1","command":"doctor","ok":true,"result":{"configuration":"ok","sqlite":"ok","media_roots":1,"lapse":"compatible","lapse_version":"2.3.1","lapse_rejection_baseline":"2.3.1","ffprobe":"available","webhooks":"manual"}}
```

A failure has `"ok":false` and an `error` with a stable `code` and a readable `message` instead of `result`. Times are RFC 3339 and field names are snake_case. Output never contains credentials or absolute paths, with two exceptions: `webhook-url` prints the token by design, and an error loading the configuration file can name that file's path.

**Exit status:** 0 for success (including `running`), 1 for a failure, 2 for a command-line error found before the database opens (an unknown command or flag, a flag the command does not take, a missing required flag such as `scan` without `--instance`, or an invalid value such as `--limit -1`). A `usage` error found later, once the database is open (such as `reject` without the `--language` it needs to pick one of several languages, an invalid `webhook-url --base`, or `forget-instance` for a configured instance), exits 1, with or without a running daemon. Scripts should branch on `error.code`, not the exit status.

| Code | Meaning |
| --- | --- |
| `usage` | A flag or value is missing, unknown or invalid for the command. |
| `unknown_instance` | The instance is not configured (for `forget-instance`: the database has no records for it; for `webhook-url` and `rotate-webhook-token`: it has no webhook token yet, because subsyncd has not started since it was added). |
| `unknown_provider` | The provider is not configured. |
| `unknown_language` | The language tag is invalid or not configured. |
| `media_not_found` | `--title` or `--path` matched no indexed file, or `--file-id` is not indexed (except for `search`). |
| `ambiguous_media` | Several files matched. `error.candidates` lists up to 25 (`instance`, `kind`, `file_id`, `title`, `year`, `season`, `episode`, `episode_end`). |
| `busy` | Another process holds the data directory: a stop-first command while the daemon runs, or a changing command while another one-shot command runs or the daemon is starting. Retry later. |
| `not_managed` | `reject` was pointed at a subtitle subsyncd does not manage. Nothing changed. |
| `daemon_required` | `--timeout` or `--no-wait` was given but no daemon runs. |
| `daemon_stopping` | The daemon is shutting down. A search or scan it was running is canceled (a search stays queued, and a `reject` stands). Retry once it is back. |
| `protocol_mismatch` | The CLI and the daemon are different builds. Run the CLI from the daemon's image. |
| `arr_unreachable` | Sonarr or Radarr did not answer `search`'s lookup. Check the instance `url`. |
| `internal` | Anything else. The message is redacted; the logs on standard error have the detail. |

Treat an unrecognized code as a failure.

## Scripting and automation

Every command except `serve` works from scripts and cron jobs, on the host or over SSH. Use `-T` so Compose does not allocate a terminal, and `--json`:

```bash
ssh HOST 'cd COMPOSE_DIR && docker compose exec -T subsyncd subsyncd search --title "Show S02E05" --timeout 4m --json'
```

- Commands never prompt and never pick a file on their own: an ambiguous title fails with `ambiguous_media`.
- Give `--timeout` a value inside your own time limit. A `running` result exits 0 and the work carries on in the daemon, even if the connection drops.
- Branch on `error.code`: `busy` and `daemon_stopping` are worth retrying later.
- `rotate-webhook-token` and `forget-instance --yes` need the daemon stopped, so a script has to stop and start it around them.
