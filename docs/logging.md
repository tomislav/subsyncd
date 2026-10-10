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

For a delayed or failed search, find its completion record and follow the same `job_id` backward. Provider events show limits or connection problems. `lapse.sync_started` means timing verification is running; it can take time on large files or network storage. A `search.completed` with outcome `failed` and reason `lapse_timeout` means a LAPSE run hit its time limit; the search is retried later, and raising `sync.timeout` or `sync.max_timeout`, or lowering `sync.min_read_rate`, gives it more time. A successful synchronization alone does not mean the subtitle was installed—look for the installation and final job outcome.

A `provider.download_completed` success confirms only the transfer. `archive.members_selected` shows how many episode versions passed selection. `candidate.rejected` immediately reports deterministic preparation or content rejection through `reason_code`, with bounded selection rules/counts for archive failures. `candidate.skipped` identifies a remembered rejection. For multiple versions of the same provider candidate, use `member_index` and `member_count` to connect LAPSE results with the selected and installed version. These info events contain no archive filenames or paths.

Already-active download cooldowns suppress provider searches and downloads. Search-only cooldowns suppress fresh searches while usable cached results remain available. These skips appear only at debug as `provider.search_skipped` and `provider.download_skipped`; the workflow completion retains the throttled outcome and retry time when acquisition is blocked. If the transport gate discovers a cooldown after preflight, its locally suppressed provider completion is also debug-only.

Use [`explain`](operations.md#inspect-or-search-one-file) for the saved result and next search time. Successful health checks are deliberately quiet.

At debug level, `archive.classified` reports archive type, subtitle-member count, and `single`, `provider_pack`, or `runtime_pack` classification. `pack_cache.publication` reports whether caching succeeded, failed, was disabled, or lacked safe identity; `pack_cache.lookup` reports a hit, miss, or error and whether the selected entry is a runtime pack. These events omit member filenames, paths, and download references.

## Warnings that need attention

| Event | Meaning |
| --- | --- |
| `queue.route_paused` | Every provider for a language and media kind is unavailable, so no searches for it start until `queue.route_resumed`. The record includes the language, media kind, and `reset_at`. |
| `candidate.early_accepted` | A LAPSE check came back solid with confidence of at least 0.9 and full agreement and coverage, so the other candidates with the same score were not checked. `reason` gives the confidence and how many were skipped. |
| `queue.media_paused` | Three searches in a row could not find or read their media file, which usually means the storage behind the library is briefly unavailable (a remounting or crashed mount). No searches start until `resume_at`; `pause_ms` is a minute at first and doubles, up to 30 minutes, each time it happens again before any search reads its media. The failed searches keep their place in the queue and retry with the normal failure backoff. |
| `provider.response_unrecognized` | SubDL answered a search with a message subsyncd doesn't recognise. The search counts as no result (a message about the API key or authorisation fails it instead), and `excerpt` shows a short, cleaned-up part of SubDL's message. A new not-found wording repeating here means subsyncd should learn it. |
| `reconcile.deferred` | Sonarr or Radarr reported a file subsyncd cannot read; `deferred_media` names the movies or episodes. It repeats hourly until the file is readable or replaced in Arr. |
| `reconcile.snapshot_deletions_withheld` | Arr reported a catalog missing all, or most, of the movies or series subsyncd tracks, so those deletions were not applied. |
| `config.weak_webhook_token` | Logged at daemon startup for an instance whose `webhook_token` is shorter than 16 characters. |

`reconcile.failed` (error) is reserved for real reconciliation failures. The [troubleshooting table](operations.md#troubleshooting) says what to do about each warning.

## Sharing logs

Credentials and absolute media paths are redacted, but logs can still include media titles and debug release details. Review excerpts before posting them publicly, and include only the relevant job and error messages.

The full event fields and privacy rules are documented in the [developer logging reference](development/logging.md).
