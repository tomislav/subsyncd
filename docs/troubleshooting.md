# Troubleshooting

Start with the commands, then the logs. Commands are written as `subsyncd COMMAND`; run them with `docker compose exec subsyncd subsyncd COMMAND`. [Commands](commands.md) describes each one.

- [Where to start](#where-to-start)
- [Why is there no subtitle for this file?](#why-is-there-no-subtitle-for-this-file)
- [What did subsyncd do to this episode?](#what-did-subsyncd-do-to-this-episode)
- [Problems by symptom](#problems-by-symptom)
- [Provider credentials rejected](#provider-credentials-rejected)
- [Searches are paused](#searches-are-paused)
- [Webhook problems](#webhook-problems)
- [Sonarr, Radarr or Silo don't show a change](#sonarr-radarr-or-silo-dont-show-a-change)
- [Hidden .subsyncd files](#hidden-subsyncd-files)

## Where to start

| Command | Answers |
| --- | --- |
| `subsyncd missing` | What has no subtitle, why, and the command that fixes it. |
| `subsyncd providers` | Which providers are cooling down, out of quota or have rejected credentials, and until when. |
| `subsyncd explain --title "..."` | Everything subsyncd knows about one file: tracks, candidates, rejections, installation, next search. |
| `subsyncd recent` | What finished lately: installs, empty searches, rejections, removals. |
| `subsyncd status` | Whether the daemon runs, the queue, and each Arr instance's reconciliation and webhook. |
| `docker compose logs subsyncd` | The detail behind all of the above. See [Logs](logging.md). |

## Why is there no subtitle for this file?

1. **Is it listed by `missing`?**

   ```bash
   subsyncd missing --title "Show"
   ```

   If the file is not listed, a subtitle or track already covers the language (`find` shows `satisfied` or `installed`), or subsyncd does not know the file. For an unknown file, check that Arr imported it inside your `path_mappings`, then run `subsyncd scan --instance NAME`.

2. **Read the `reason` and run the `action`.** The [reason table](commands.md#missing) explains each one. Most often:
   - `not_searched` or `no_candidates`: the next scheduled search may find one; `search` tries now. New subtitles often appear days after release, which is why subsyncd keeps retrying ([schedule](providers.md#when-subsyncd-searches)).
   - `providers_cooling_down`: run `subsyncd providers`. The search resumes at the reset, except for rejected credentials ([below](#provider-credentials-rejected)).
   - `all_rejected`: every candidate failed a check. Step 3 shows why.
   - `search_failing`: a technical failure. Step 3 and the logs show the cause.

3. **Look at the file's candidates and rejections.**

   ```bash
   subsyncd explain --title "Show S01E02" --language en
   ```

   Rejection reasons tell you what went wrong: `lapse_unsure` or `lapse_nothing` (timing could not be verified), `language_mismatch`, `partial_coverage` (too short for the file; `lapse_partial_coverage` when LAPSE's timing made it so), and so on. `search --retry-rejected` reconsiders subsyncd's own rejections once the cause is fixed.

4. **Check the logs** for that search: see the next section.

## What did subsyncd do to this episode?

`explain` shows the current state. For the history, combine `recent` and the logs.

1. Get the file's IDs:

   ```bash
   subsyncd explain --title "Show S01E02" --json
   ```

   Note `instance`, `kind`, `file_id` and `media_id` in the result.

2. List its search history (kept 30 days). `recent` cannot filter by file, so filter its JSON with `jq` on the host:

   ```bash
   docker compose exec -T subsyncd subsyncd recent --since 720h --instance sonarr-main --limit 10000 --json \
     | jq '.result.items[] | select(.file_id == 1234 and .kind == "episode")'
   ```

   Each row is one finished search, rejection or removal, with its outcome, provider, release and reason.

3. Find its log lines. Search and install lines carry its `media_id` and a `media_title` such as `Show - S01E02 - Title`:

   ```bash
   docker compose logs subsyncd | grep '"media_id":5678'
   ```

   Look for `install.*` (installed, replaced, renamed, removed), `candidate.rejected` with its `reason_code`, and `job.completed` with its `outcome`. Docker keeps only as much log as its logging driver allows, so old lines may be gone. `logging.level: debug` adds candidate scores.

## Problems by symptom

| Symptom | What to check |
| --- | --- |
| Container is unhealthy | `/readyz` failed: the database or a media folder is unreadable. Look for `readiness.unhealthy` in the logs, check mounts and [permissions](running.md#permissions). |
| Permission denied | `PUID`/`PGID` must read the configuration and media, and write `data/` and the subtitle folders. |
| Files of one instance are never searched, with no error | Arr's path for them matches no `path_mappings` entry, or maps outside the media roots. Such files are skipped without an error; a webhook carrying only such files logs `webhook.applied` with `outcome` `ignored`. Compare `path_mappings` with Arr's paths and the Compose volumes. |
| `analyze-sync` says "media path is outside configured roots" | The `--media` path must be a container path inside a media root. |
| Startup fails on a config key | Unknown keys stop startup. Check spelling against [Configuration](configuration.md). |
| `busy` from a command | Another process holds the data directory. Stop the daemon only for `rotate-webhook-token` and `forget-instance --yes`. Never delete `data/subsyncd.lock`. |
| A provider shows `auth_failed` | See [Provider credentials rejected](#provider-credentials-rejected). |
| Nothing searches for one language | See [Searches are paused](#searches-are-paused). |
| LAPSE takes a long time | The first check of a file decodes its whole audio track; later checks are cached. See [Resources](running.md#resources-and-lapse-run-time). |
| `search.completed` with reason `lapse_timeout` | A LAPSE run hit its limit. Raise `sync.timeout` or `sync.max_timeout`, or lower `sync.min_read_rate`. |
| A subtitle is out of sync or wrong | [`reject`](commands.md#a-subtitle-is-wrong) it. |
| Subtitles of re-imported files vanish slowly | Up to 20 old subtitles are deleted per hour (`install.removal_deferred`). Check that Arr was meant to replace that many files. |
| `reconcile.snapshot_deletions_withheld` | Arr reported a library missing most of what subsyncd tracks. Check the instance `url` and Arr's database. |
| `reconcile.deferred` | Arr reports a file subsyncd cannot read, such as a broken symlink. Fix or replace it in Arr; it is checked hourly. |
| `webhook.failed` and similar | See [Webhook problems](#webhook-problems). |
| Arr or Silo doesn't show a new subtitle | See [below](#sonarr-radarr-or-silo-dont-show-a-change). |

## Provider credentials rejected

A provider that rejects its credentials is switched off (`auth_failed` in `providers`, `provider.auth_disabled` in the logs) and stays off until you act:

1. Correct the credentials in `config/config.yaml`.
2. Restart: `docker compose restart subsyncd`.
3. Clear the saved state, which also makes waiting searches run:

   ```bash
   subsyncd retry --provider opensubtitles-main
   ```

The daemon carries out `retry`, so nothing needs stopping.

## Searches are paused

**One language and media kind** (`queue.route_paused`): no provider for that language can download, because of an exhausted quota, a cooldown or rejected credentials. Other languages continue. Searches resume within about 30 seconds of `reset_at`. With `reason` `disabled` there is no reset: fix the credentials and run `retry` as above. With `episode_ranges_only`, only multi-episode files wait, because the remaining provider (Gestdown) has per-episode subtitles only.

**Everything** (`queue.media_paused`): three searches in a row could not read their media and the storage itself did not read either, usually a remounting or crashed mount. Searches pause for a minute, doubling each time it happens again up to 30 minutes, until one reads its media. The failed searches keep their place. One missing file on otherwise readable storage never pauses the queue. Check the mount.

## Webhook problems

| Log event | Meaning and fix |
| --- | --- |
| `webhook.failed` (warn) | A saved webhook could not be applied, usually because Arr did not answer. It is retried at `retry_at`. |
| `webhook.failed` (error, `outcome` `abandoned`) | Retries ran out after about 31 hours (gaps of 1m, 5m, 15m, 1h, 6h and 24h). The six-hourly history reconciliation applies the change anyway. |
| `webhook.inbox_orphaned` | Webhooks are stored for an instance no longer configured. Run [`forget-instance`](commands.md#remove-an-instance-for-good) for it. |
| `webhook.connection_ambiguous` | Several Arr connections look like this subsyncd's, so none was changed. Delete the extras in Arr. |
| `webhook.connection_foreign` (info) | Connections for the same instance name point at another subsyncd. They are left alone; no action needed. |
| `webhook.connection_sync_failed` | subsyncd could not keep its managed connection correct. It retries by itself; the `reason` is below. |

`webhook.connection_sync_failed` reasons:

| `reason` | Fix |
| --- | --- |
| `arr_unreachable` | Arr did not answer within 15 seconds. Check the instance `url`. While saving a connection, Arr may instead be waiting on its own test call to `server.webhook_base_url` (for example a firewall that silently drops it). |
| `arr_status` | Arr answered with an unexpected HTTP `status`, such as 401, 404 or 5xx. Check the `url` and API key. |
| `arr_response_invalid` | Arr's answer was not understood. Check that it is Sonarr or Radarr v3 or later. |
| `arr_test_rejected` | Arr refused to save the connection, typically because its test of the URL got an error from subsyncd. Check `server.webhook_base_url` and any reverse-proxy path prefix. |
| `arr_test_unreachable` | Arr could not reach subsyncd at that address. Fix `server.webhook_base_url`, and let it through Arr's HTTP proxy if one is set. |
| `name_unavailable` | Nine connection names are taken. Delete a stale `subsyncd (...)` connection in Arr. |
| `schema_missing_webhook` | This Arr offers no Webhook connection type. |
| `internal` | A local failure, such as an unreadable database. `error_kind` and the redacted `error` say what failed; fix it and restart. |

## Sonarr, Radarr or Silo don't show a change

**Sonarr or Radarr.** Look for `notification.delivered`, `notification.retry_scheduled` or `notification.abandoned` with `notifier` `arr_rescan`. Check the instance `url` and API key, and that `rescan_after_changes` is not `false`. `notification.skipped` with `reason` `rescan_disabled` means rescans are off for the instance; `unknown_arr_id` means subsyncd does not know the episode's Sonarr series yet, which the next import, rename or `scan` fixes.

**Silo.** Look for the same events with `notifier` `silo`. `notification.failed` means Silo refused the request for good (for example 404 or 422): check `silo.url` (without `/api/v2`), the admin key and `silo.path_mappings`. See [Silo refresh](running.md#silo-refresh).

A failed notification never undoes a subtitle change.

## Hidden .subsyncd files

While installing or removing a subtitle, subsyncd briefly keeps hidden files beside it: `.subsyncd-stage-*`, `.subsyncd-rollback-*` and `.subsyncd-removing-*`. Its database records each one, and the next start finishes or undoes any that a crash interrupted.

Never delete them while subsyncd runs. If you see them pile up, start subsyncd once, check the logs for `install.recovery_failed`, and inspect the file with `explain` before changing anything by hand. To remove leftovers after uninstalling, see [Uninstall](upgrade-and-backup.md#uninstall).
