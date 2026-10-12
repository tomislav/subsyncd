# Logs

subsyncd writes its logs to the container output. No separate logging service is needed.

- [View logs](#view-logs)
- [Choose a log level](#choose-a-log-level)
- [Follow a search](#follow-a-search)
- [Events to know](#events-to-know)
- [Sharing logs](#sharing-logs)

## View logs

```bash
docker compose logs --tail=100 subsyncd   # recent activity
docker compose logs -f subsyncd           # follow new events; Ctrl+C stops following
```

## Choose a log level

Set the level in `config/config.yaml` and restart with `docker compose restart subsyncd`:

```yaml
logging:
  level: info
```

| Level | Shows |
| --- | --- |
| `debug` | Everything below, plus candidate scores, release evidence and cache decisions. |
| `info` | Everyday activity: searches, downloads, rejection reasons, installations. The default. |
| `warn` | Warnings and errors only, such as provider limits and failed timing checks. |
| `error` | Errors only. |

Return to `info` when you finish debugging. The `SUBSYNCD_LOG_LEVEL` environment variable overrides the file; if a level change seems to have no effect, check for it.

## Follow a search

Each log line is a JSON record. The most useful fields:

| Field | Meaning |
| --- | --- |
| `event` | What happened, such as `job.started` or `search.completed`. |
| `media_title` | The movie or episode, such as `Dune (2021)` or `Show - S01E02 - Title`. |
| `media_id` | The file's ID in subsyncd, also shown by `explain`. |
| `job_id` | Connects the lines of one search. |
| `outcome`, `reason` | The result and why. |

For a delayed or failed search, find its `job.completed` line and follow the same `job_id` backward.

- `search.started` and `search.completed` frame the search itself. `search.completed` with `outcome` `failed` is logged at error; reason `lapse_timeout` means a LAPSE run hit its [time limit](running.md#resources-and-lapse-run-time).
- `candidate.rejected` gives each rejection's `reason_code`; `candidate.skipped` a candidate rejected earlier.
- `lapse.sync_started` means a timing check is running, which can take a while on large files.
- A successful download or timing check does not mean a subtitle was installed: look for the final `outcome`.
- A manual `search` logs the same lines with `priority` `manual`, also when it carries on in the daemon after the command returned `running`.
- At `debug`, `provider.search_skipped` and `provider.download_skipped` show requests held back by a cooldown, and `archive.classified`, `pack_cache.lookup` and `pack_cache.publication` show season-pack handling.

[Troubleshooting](troubleshooting.md#what-did-subsyncd-do-to-this-episode) shows how to collect everything about one file. Successful health checks are not logged.

## Events to know

Events marked warn or error usually need a look; info events are listed because they explain a change you may notice. [Troubleshooting](troubleshooting.md) says what to do about each problem.

### Searches and the queue

| Event | Level | Meaning |
| --- | --- | --- |
| `queue.route_paused` | warn | No provider for a language and media kind can download; its searches wait until `reset_at`, or with `reason` `disabled` until you run `retry`. |
| `queue.route_resumed` | info | Those searches run again. |
| `queue.media_paused` | warn | Three searches in a row could not read their media; all searches wait until `resume_at`. |
| `candidate.early_accepted` | info | A LAPSE check was so good (`confidence`) that `skipped_count` tied candidates were not checked. |
| `job.lease_lost` | warn | A search lost its claim on the queue, for example after a database stall; it runs again later. |
| `job.retry_scheduled` | warn | A search failed technically and retries at the normal failure delay. |
| `search.completed` | error | With `outcome` `failed`: the search failed (network, LAPSE, storage). |
| `history.record_failed` | warn | A finished search could not be added to the `recent` history; the search itself is fine. |
| `history.prune_failed` | warn | Deleting history older than 30 days failed; it is retried the next hour. |

### Providers

| Event | Level | Meaning |
| --- | --- | --- |
| `provider.auth_disabled` | warn | The provider rejected its credentials and is switched off. Fix them, restart, run `retry`. |
| `provider.cooldown_started` | warn | The provider is paused after a rate limit, quota or busy answer, until its reset. |
| `provider.circuit_opened` | warn | Repeated failures paused the provider for a while. |
| `provider.recovered` | info | The provider is usable again. |
| `provider.response_unrecognized` | warn | SubDL answered with a message subsyncd does not recognize; `excerpt` shows part of it. |

### Subtitle files

| Event | Level | Meaning |
| --- | --- | --- |
| `install.removed` | info | A subtitle subsyncd installed was deleted. Without `reason`: `reject` found no replacement. `media_replaced` or `media_renamed`: Arr replaced or renamed the media file. |
| `install.record_dropped` | info | subsyncd stopped managing a subtitle without touching it; `reason` is `sidecar_missing`, `sidecar_changed`, `media_replaced` or `sidecar_unmovable`. It counts as yours from then on. |
| `install.renamed` | info | A subtitle followed a renamed media file; `moved` is `true` when subsyncd moved it. |
| `install.reattached` | info | Arr gave the same file a new ID; the subtitle stays managed. |
| `install.removal_deferred` | warn | A replaced file's old subtitle waits because 20 were already deleted this hour (`reason` `cap`). |
| `install.rename_cleanup_failed` | warn | A moved subtitle still has its old name too; the next start removes it. |
| `install.rollback_skipped` | warn | A failed install was not undone because another subtitle was published at that path meanwhile. |
| `install.rollback_cleanup_failed` | warn | The hidden copy of a replaced subtitle could not be deleted; the next start deletes it. |
| `install.rollback_sweep_completed` | info | At startup, leftover hidden `.subsyncd-rollback-*` copies were deleted (`removed_count`). |
| `install.recovery_failed` | warn | At startup, an install a crash interrupted could not be settled; it is tried at the next start. |
| `install.recovery_timed_out` | warn | Settling interrupted installs took over 30 seconds, usually a hung mount; startup continued. `install.rollback_sweep_timed_out` is the same for old copies. |
| `inventory.refresh_failed` | error | subsyncd could not read a file's embedded tracks and sidecars. |

### Sonarr, Radarr and webhooks

| Event | Level | Meaning |
| --- | --- | --- |
| `webhook.failed` | warn | A saved webhook could not be applied and retries at `retry_at`. |
| `webhook.failed` | error | With `outcome` `abandoned`: retries ran out; history reconciliation applies the change. |
| `webhook.inbox_orphaned` | warn | At startup: webhooks stored for an instance no longer configured (`webhook_count`). |
| `webhook.connection_sync_failed` | warn | The managed Arr connection could not be checked or fixed; see the [reasons](troubleshooting.md#webhook-problems). Retried after 15 minutes, 1 hour, then every 6 hours. |
| `webhook.connection_ambiguous` | warn | Several Arr connections look like this subsyncd's (`count`); none was changed. |
| `webhook.connection_created`, `_adopted`, `_repaired` | info | subsyncd created, took over or corrected its Arr connection (`connection_id`, `changed`). |
| `webhook.connection_foreign` | info | Connections for the same instance name belong to another subsyncd; left alone. |
| `webhook.connection_ok` | debug | The connection was already correct. |
| `webhook.token_rotated` | info | `rotate-webhook-token` replaced a token. Tokens are never logged. |
| `arr.request_retries_exhausted` | warn | A request to Sonarr or Radarr failed after its retries (`instance`). |
| `reconcile.deferred` | warn | Arr reports files subsyncd cannot read (`deferred_media`); repeats hourly. |
| `reconcile.snapshot_deletions_withheld` | warn | Arr's library is missing most of what subsyncd tracks; those deletions were not applied. |
| `reconcile.failed` | error | A reconciliation with Arr failed; it is retried. |
| `scan.completed` | info | A `scan` run by the daemon finished, with `media`, `deferred` and `probe_skipped`. Logged even when the command stopped waiting. |
| `scan.failed` | warn | A daemon scan failed; at info with `outcome=canceled` when shutdown canceled it. |
| `instance.forgotten` | info | `forget-instance --yes` deleted a removed instance's records, with counts. |

### Notifications to Arr and Silo

`notifier` is `arr_rescan` (with `instance` and `series_id` or `movie_id`) or `silo`.

| Event | Level | Meaning |
| --- | --- | --- |
| `notification.delivered` | info | Arr was asked to rescan, or Silo to scan a folder. |
| `notification.skipped` | info | No rescan was sent: `reason` `rescan_disabled` or `unknown_arr_id`. |
| `notification.retry_scheduled` | warn | No answer, a timeout or a retryable status; tried again at `retry_at`. |
| `notification.abandoned` | warn | Arr refused the rescan (such as 404 or 401); not retried. |
| `notification.failed` | error | Silo refused the scan for good (such as 404, 410 or 422), or the result could not be saved. |
| `notification.prune_failed` | warn | Deleting notifications finished over 30 days ago failed; retried within a minute. `notification.pruned` (debug) reports each cleanup. |

A failed notification never undoes a subtitle change.

### Service and commands

| Event | Level | Meaning |
| --- | --- | --- |
| `service.start_failed` | error | subsyncd could not start; the error says why. |
| `readiness.unhealthy` | warn | `/readyz` failed: the database or a media folder is unavailable. `readiness.recovered` (info) follows. |
| `command.failed` | error | A one-shot command failed; the redacted error says why. |
| `control.request_completed` | info | The daemon carried out a command the CLI handed it (`command`, `outcome`, `duration_ms`). |
| `control.request_failed` | warn | The daemon refused or failed such a command, with the error `code` the CLI shows. |
| `worker.drain_timed_out`, `worker.drain_abandoned` | warn | At shutdown, running searches did not stop within the 30-second window; they run again after the restart. |

The developer [event catalogue](development/logging.md) lists every event with its fields.

## Sharing logs

Credentials and absolute media paths are redacted, but logs contain media titles and, at debug, release details. Review excerpts before posting them, and include only the relevant job and error lines.
