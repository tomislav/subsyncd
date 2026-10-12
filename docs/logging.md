# Logs and troubleshooting

subsyncd writes logs to the container output. No separate logging service is required.

## View logs

Show recent activity:

```bash
docker compose logs --tail=100 subsyncd
```

Follow new events as they happen:

```bash
docker compose logs -f subsyncd
```

Press Ctrl+C to stop following logs; subsyncd keeps running.

## Choose a log level

Set the level in `config/config.yaml`:

```yaml
logging:
  level: info
```

| Level | Use it for |
| --- | --- |
| `info` | Everyday use: searches, downloads, member selection, rejection reasons, installations, and service activity. This is the default. |
| `debug` | Investigating candidate scores, release evidence, and cache decisions. |
| `warn` | Warnings and errors, including provider limits and unsuccessful timing checks. |
| `error` | Errors only. |

Levels include more severe messages too. After editing the configuration, run `docker compose restart subsyncd`. Return to `info` when you finish debugging.

If `SUBSYNCD_LOG_LEVEL` is set in the container environment, it overrides the YAML setting. Remove or change that override if the configured level seems to have no effect.

## Follow a search

Each application log line is a JSON record. The useful fields are:

- `event`: what happened, such as `job.started` or `job.completed`.
- `media_title`: which movie or episode is being processed.
- `job_id`: connects records belonging to the same job.
- `outcome` and `reason`: the result and why it happened.

For a delayed or failed search, find its completion record and follow the same `job_id` backward. A manual `search` logs the same `job.started` and `job.completed` records with `priority` `manual`, also when it carries on in the daemon after the command reported `running`; its `job.completed` `outcome` is the search outcome, `failed`, or `canceled` when the daemon stopped. Provider events show limits or connection problems. `lapse.sync_started` means timing verification is running; it can take time on large files or network storage. A `search.completed` with outcome `failed` and reason `lapse_timeout` means a LAPSE run hit its time limit; the search is retried later, and raising `sync.timeout` or `sync.max_timeout`, or lowering `sync.min_read_rate`, gives it more time. A successful synchronization alone does not mean the subtitle was installed—look for the installation and final job outcome.

A `provider.download_completed` success confirms only the transfer. `archive.members_selected` shows how many episode versions passed selection. `candidate.rejected` immediately reports deterministic preparation or content rejection through `reason_code`, with bounded selection rules/counts for archive failures. `candidate.skipped` identifies a remembered rejection. For multiple versions of the same provider candidate, use `member_index` and `member_count` to connect LAPSE results with the selected and installed version. These info events contain no archive filenames or paths.

Already-active download cooldowns suppress provider searches and downloads. Search-only cooldowns suppress fresh searches while usable cached results remain available. These skips appear only at debug as `provider.search_skipped` and `provider.download_skipped`; the workflow completion retains the throttled outcome and retry time when acquisition is blocked. If the transport gate discovers a cooldown after preflight, its locally suppressed provider completion is also debug-only.

Use [`explain`](operations.md#inspect-or-search-one-file) for the saved result and next search time. Successful health checks are deliberately quiet.

At debug level, `archive.classified` reports archive type, subtitle-member count, and `single`, `provider_pack`, or `runtime_pack` classification. `pack_cache.publication` reports whether caching succeeded, failed, was disabled, or lacked safe identity; `pack_cache.lookup` reports a hit, miss, or error and whether the selected entry is a runtime pack. These events omit member filenames, paths, and download references.

## Warnings that need attention

| Event | Meaning |
| --- | --- |
| `queue.route_paused` | Every provider for a language and media kind is unavailable, so no searches for it start until `queue.route_resumed`. The record includes the language, media kind, and `reset_at`. |
| `candidate.early_accepted` | A LAPSE check came back solid with confidence of at least 0.9 and full agreement and coverage, so the other candidates with the same score were not checked. `confidence` is the accepted result's confidence and `skipped_count` how many tied candidates were not checked. |
| `queue.media_paused` | Three searches in a row could not find or read their media file, which usually means the storage behind the library is briefly unavailable (a remounting or crashed mount). No searches start until `resume_at`; `pause_ms` is a minute at first and doubles, up to 30 minutes, each time it happens again before any search reads its media. The failed searches keep their place in the queue and retry with the normal failure backoff. A file that is gone while the rest of its folder or library still reads (`queue.media_file_unavailable`, info) does not count, so one deleted or broken file never pauses the queue. |
| `webhook.failed` | A stored Sonarr or Radarr webhook could not be applied. With `outcome` `retry_scheduled` it is tried again at `retry_at`; with `abandoned` it was given up after about a day of retries, and the next reconciliation picks up the change from Arr's history. |
| `provider.response_unrecognized` | SubDL answered a search with a message subsyncd doesn't recognise. The search counts as no result (a message about the API key or authorisation fails it instead), and `excerpt` shows a short, cleaned-up part of SubDL's message. A new not-found wording repeating here means subsyncd should learn it. |
| `reconcile.deferred` | Sonarr or Radarr reported a file subsyncd cannot read; `deferred_media` names the movies or episodes. It repeats hourly until the file is readable or replaced in Arr. |
| `reconcile.snapshot_deletions_withheld` | Arr reported a catalog missing all, or most, of the movies or series subsyncd tracks, so those deletions were not applied. |
| `scan.completed` | A scan run by the daemon finished, with the instance, the number of media, how many were deferred or skipped by `--force-probe`, and how long it took. It is logged even when the command stopped waiting earlier. |
| `scan.failed` | A scan run by the daemon failed (warn) or was canceled by shutdown (info, `outcome=canceled`). Run `scan` again once the cause is fixed. |
| `history.record_failed` | A manual search finished but its entry could not be added to the search history; the search itself is unaffected. |
| `history.prune_failed` | The hourly cleanup of search history older than 30 days failed; it is retried the next hour. |
| `webhook.inbox_orphaned` | Logged at startup when webhooks are stored for an instance that is no longer in the configuration; `webhook_count` says how many. They are never processed. Run `forget-instance` for that instance to delete them. |
| `install.removed` | Info. A subtitle subsyncd had installed was deleted, along with its installation record; `provider` and `candidate_id` name the removed subtitle, and `rollback_deleted` says whether an older hidden copy beside it was deleted too. Without `reason`, `reject` found no acceptable replacement and the language counts as missing again; a rejection you make with `reject` is logged as `candidate.rejected` with a `reason_code` starting with `user_`. With `reason` `media_replaced`, Arr replaced the media file (an upgrade or a re-import of a different file) and the subtitle belonged to the old one; with `media_renamed`, Arr renamed the file, left the subtitle under the old name, and another file already had the new one. The search for the current file follows. |
| `install.record_dropped` | Info. subsyncd forgot a subtitle it had installed without touching any file: `reason` `sidecar_missing` (the file is gone), `sidecar_changed` (it was edited, replaced or is a symlink, and is not named after the current media file) `media_replaced` (the media file was replaced and the old subtitle was no longer subsyncd's to delete) or `sidecar_unmovable` (Arr renamed the media file and left the subtitle under the old name, and subsyncd could not move it without risk: the new name is on another filesystem, or the filesystem supports neither hard links nor a rename that refuses to overwrite). A file left behind is treated as your own from then on. The search for the current file follows. |
| `install.renamed` | Info. Arr renamed a media file and the subtitle subsyncd installed follows it: `moved` is `false` when Arr renamed the subtitle too, `true` when subsyncd moved it. |
| `install.rename_cleanup_failed` | A subtitle subsyncd moved to a renamed media file's new name is recorded there, but its old name could not be removed; until the next start the same file has both names. The next start removes the old name if it is still that file. |
| `install.reattached` | Info. Arr gave a media file a new file ID, but it is the same file (same size, and the same name or a subtitle already named after it), for example after a re-import or an Arr database restore. The subtitle stays and subsyncd keeps managing it. |
| `install.removal_deferred` | The subtitle of a replaced media file was not deleted because 20 had already been deleted in the past hour (`reason` `cap`, `limit_per_hour`). It stays, and the search for the new file waits: it completes as `throttled` with `reason` `removal_deferred` and tries again at `retry_at`, when the next deletion is allowed, deleting the old subtitle first. Nothing is installed for the new file until then. Many of these mean Arr replaced a lot of files at once; check that this was intended. |
| `install.rollback_sweep_completed` | Info, at startup. subsyncd deleted hidden `.subsyncd-rollback-` copies of replaced subtitles that earlier versions kept, or that a failed cleanup left (`removed_count`); `skipped_count` copies were left because they were not where expected or their subtitle is missing. |
| `notification.retry_scheduled` | Sonarr, Radarr or Silo could not be told about a subtitle change (no answer, a timeout or a server error); `notifier` is `arr_rescan` or `silo`, and it is tried again at `retry_at`. The subtitle change itself is done. |
| `notification.abandoned` | Sonarr or Radarr refused a rescan (`notifier` `arr_rescan`), for example with 404 because the series or movie was deleted, or 401 for a wrong API key; `instance` and `series_id` or `movie_id` say which. It is not retried; Arr's own scheduled refresh still picks the subtitle up. |
| `notification.delivered` | Info. Sonarr or Radarr was asked to rescan a series or movie (`notifier` `arr_rescan`, with `instance` and `series_id` or `movie_id`), or Silo to rescan a folder, after subsyncd changed a subtitle. `notification.skipped` (info) means no rescan was sent: `reason` `rescan_disabled` (`rescan_after_changes` was turned off) or `unknown_arr_id` (subsyncd does not know the episode's Sonarr series yet). |
| `install.rollback_cleanup_failed` | A replaced subtitle's hidden rollback copy could not be deleted after the new one was installed. The install is fine; the copy is deleted at the next start. |
| `install.recovery_timed_out` | At startup, finishing subtitle installs interrupted by a crash took longer than 30 seconds, usually because a media mount hangs. subsyncd starts anyway and tries again at the next start. `install.rollback_sweep_timed_out` means the same for deleting old rollback copies. |
| `webhook.connection_sync_failed` | subsyncd could not create or check an instance's Arr webhook connection (only with `server.webhook_base_url` set). `reason` is `arr_unreachable`, `arr_status`, `arr_response_invalid`, `arr_test_rejected`, `arr_test_unreachable`, `name_unavailable`, `schema_missing_webhook` or `internal`; `status` is Arr's HTTP status when there was one. For `internal` only, `error_kind` (`webhook_connection_sync`) and a redacted `error` describe the local failure. It is retried after 15 minutes, then 1 hour, then every 6 hours. |
| `control.request_completed` | Info. The running daemon carried out a command the CLI handed to it (for example `retry` or `search`); `command`, `outcome` and `duration_ms` say what and how long, and `media_title` which file a `search` named. A search that is still running when the command returns completes later under its own `job.completed`. |
| `control.request_failed` | Warning. The daemon refused or failed such a command; `command`, the error `code` the CLI shows (`usage`, `unknown_provider`, `internal`, ...) and a redacted `error` say why. |
| `webhook.connection_ambiguous` | More than one Arr webhook connection looks like this subsyncd's for the instance (`count`), so none was changed. Delete the extras in Arr. |
| `webhook.connection_foreign` | Info. Arr has webhook connections (`count`) for the instance name that belong to another subsyncd deployment; they were left unchanged. |
| `webhook.connection_created` | Info. subsyncd created the instance's Arr webhook connection (`connection_id`). |
| `webhook.connection_adopted` | Info. subsyncd took over an existing Arr webhook connection that already pointed at it (`connection_id`). |
| `webhook.connection_repaired` | Info. subsyncd corrected the connection (`connection_id`); `changed` lists which of `url`, `method` and `events` it set. |
| `webhook.connection_ok` | Debug. The connection was already correct (`connection_id`). |
| `webhook.token_rotated` | Info. `rotate-webhook-token` replaced the instance's webhook token. The token is never logged. |
| `instance.forgotten` | `forget-instance --yes` deleted a removed instance's records; carries the instance name and how many of each kind of record were removed. |

`reconcile.failed` (error) is reserved for real reconciliation failures. The [troubleshooting table](operations.md#troubleshooting) says what to do about each warning.

## Sharing logs

Credentials and absolute media paths are redacted, but logs can still include media titles and debug release details. Review excerpts before posting them publicly, and include only the relevant job and error messages.

The full event fields and privacy rules are documented in the [developer logging reference](development/logging.md).
