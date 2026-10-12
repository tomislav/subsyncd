# Notifications

After a managed sidecar changes on disk, subsyncd tells Silo to scan the folder and asks the owning Sonarr or Radarr instance to rescan the series or movie. Both go through one durable outbox. For configuring them, see [Configuration](../configuration.md); the Silo wire contract is in the [Silo API reference](../references/silo.md).

- [Outbox](#outbox)
- [Which changes notify](#which-changes-notify)
- [Deduplication](#deduplication)
- [Delivery and retries](#delivery-and-retries)
- [Silo](#silo)
- [Arr rescan](#arr-rescan)
- [Retention](#retention)

## Outbox

Notification intents are rows in `notifications`, written in the same SQLite transaction as the change they report (installation, removal or move). Delivery happens later in the worker, so:

- a notification failure never rolls back a subtitle or its installation;
- a crash between commit and delivery loses nothing;
- delivery is at least once: an uncertain transport failure or a recovered lease can repeat a request.

Each intent names its notifier (`silo` or `arr_rescan`, free text in `notifications.notifier`) and carries a `workflow.NotificationPayload` with the media reference (`ref.instance`, `ref.kind`), `series_id` (Sonarr series), `entity_id` (Radarr movie) and paths. Payloads are never logged. `notification.queued` (info) reports each committed intent with `notifier` and `notification_key` (12 characters of the dedupe key).

The worker leases up to 10 due intents per maintenance tick (`NotificationBatch`), with the same 5-minute lease and renewal as searches.

## Which changes notify

`Installer.changeNotifierNames` (`internal/workflow/notification.go`) picks the notifiers for a change: the configured ones (`silo` when enabled), plus `arr_rescan` when the media's instance is in `Installer.ArrRescanInstances` (instances whose `rescan_after_changes` is not `false` and that have a rescan client) and `notifier.RescanID` knows the target (an episode's `series_id`, a movie's `entity_id`). Without the ID it logs `notification.skipped` (`reason=unknown_arr_id`, `component=workflow`) and queues the others.

| Change | Silo | Arr rescan |
| --- | --- | --- |
| `Install`: new, replacement, upgrade, promotion, manual | Yes | Yes |
| `RemoveManaged`: reject removal, replaced or renamed media | Yes | Yes |
| `moveRenamedSidecar`: a sidecar Arr left behind on a rename, queued with the record (`RebaseInstallationWithNotifications`) | No | Yes |
| Same-bytes provenance refresh (`refreshIdenticalInstallation`), reattached record | No | No |
| `RecoverPendingInstalls` undoing an interrupted install | No | No |
| `followRename` unlinking a link left at the old name after Arr moved the sidecar | No | No |
| `RecoverPendingInstalls` finishing a move whose commit already queued the rescan | No | No |

## Deduplication

`dedupe_key` holds while an intent is pending.

- **Silo** keys hash the notifier, media row, language, subtitle checksum, the committed media fingerprint and the subtitle destination. Retries of one installation share an intent; identical text installed for a replacement file triggers a new scan. Removals use a removal-specific key.
- Completing an intent with a terminal result clears its key, so publishing the same subtitle again later enqueues a new scan.
- **`arr_rescan`** keys name the target instead: a hash of `arr_rescan` and `notifier.RescanCoalesceKey` (instance, kind, series or movie ID), with `NotificationRequest.Coalesce` set. `enqueueNotification` looks the key up in the change's transaction:
  - an unleased row with the key (pending, or retry-scheduled after a failure) absorbs the change: nothing is inserted and its due time stays, so a steady trickle of changes cannot postpone the rescan;
  - a leased row, which may already have been sent, gives its key up (`dedupe_key=NULL`) and a new row is inserted, so the later change is rescanned too. If that in-flight delivery then fails and retries, both are delivered (one extra rescan).
  - Rows with per-change keys are not absorbed and are delivered individually.

## Delivery and retries

Intents leased in one batch whose notifier reports the same coalesce key (`notifier.Coalescer`) share one request, and each is completed with its result and its own attempt-based retry delay. Silo coalesces on the mapped scan directory; Arr rescans on (instance, kind, series or movie ID). Intents leased in different batches are not coalesced.

| Result | Stored result | Event |
| --- | --- | --- |
| Delivered (any 2xx) | `success` | `notification.delivered` (info) |
| Timeout, transport failure, HTTP 408 or 429 or 5xx (Silo also 409) | `retryable_error` | `notification.retry_scheduled` (warn, `retry_at`) |
| Arr: any other status (404 for a deleted series, 400, 401) | `refused` | `notification.abandoned` (warn) |
| Silo: any other status (404, 410, 422, …) | `permanent_error` | `notification.failed` (error) |
| Arr: an invalid request, or a media kind that does not match the instance | `permanent_error` | `notification.failed` (error) |
| Not applicable (`notifier.SkippedError`) | `skipped` | `notification.skipped` (info, `reason`) |
| The completion itself could not be stored | | `notification.failed` (error) |

Retries follow `schedule.FailureDelay`: 1 minute, 5 minutes, 15 minutes, then every hour, with no attempt limit. Failures carry `error_kind=notification_delivery` and a redacted `error` naming the status or `transport unavailable`, never a URL, key or body.

## Silo

- `POST <silo.url>/api/v2/scan` with `Authorization: Bearer <api_key>` and `{"path": "<mapped parent directory>"}`. The path is the media file's parent, mapped through `silo.path_mappings`, so a subtree scan finds the sibling subtitle.
- No redirects, 15-second timeout, one request per attempt, no API-version fallback.
- See the [Silo API reference](../references/silo.md) for the contract and path mapping.

## Arr rescan

`notifier.ArrRescan` (`internal/notifier/arr_rescan.go`) asks the owning instance to rescan, so Sonarr and Radarr learn about the sidecar before their own scheduled refresh.

- Request: `POST /api/v3/command` with `{"name":"RescanSeries","seriesId":ID}` (Sonarr) or `{"name":"RescanMovie","movieId":ID}` (Radarr), through `catalog.ArrRescanClient`. It uses the Arr client's bounded JSON write (`X-Api-Key`, 1 MiB request limit, no redirects, error bodies discarded except validation property names) with its 15-second timeout. It never sends `RefreshSeries`/`RefreshMovie`, which query metadata providers.
- Timing: an intent is first due `notifier.ArrRescanDelay` (60 seconds) after the change that queued it (`notificationDue`). Silo intents are due at once. Changes until the rescan is leased join it, so a series is rescanned at most about once per delay while changes keep coming.
- Skips (`skipped`, no request):
  - `rescan_disabled`: the instance's `rescan_after_changes` was turned off, or the instance was removed, after the intent was queued;
  - `unknown_arr_id`: no series or movie ID.
- `app.New` registers the `arr_rescan` notifier even when no instance rescans (an empty map), so leftover intents complete as `skipped` rather than failing as an unconfigured notifier.
- `app.New` builds the clients from each instance's `url` and `api_key`. `Options.ArrRescanners` replaces them in tests; tests that supply `Options.Catalogs` without it get none and make no Arr requests.
- Worker records carry `instance`, `media_kind` and `series_id` (Sonarr) or `movie_id` (Radarr).

## Retention

A finished intent (`next_attempt_at_ns = 0`: result `success`, `skipped`, `refused` or `permanent_error`) records `finished_at_ns` (`NotificationCompletion.FinishedAt`, the worker's clock).

- Each worker maintenance tick (`Worker.pruneNotifications`) deletes at most 500 (`notificationPruneBatch`) intents that finished more than 30 days ago (`store.NotificationRetention`), oldest first (`Repository.PruneFinishedNotifications`).
- Pending, retry-scheduled and leased intents are never deleted.
- The prune uses the partial index `notifications_finished_idx` on `(next_attempt_at_ns, finished_at_ns) WHERE next_attempt_at_ns = 0` (migration 024).
- `notification.pruned` (debug, `removed_count`, `retention_days`) reports a tick that deleted rows; `notification.prune_failed` (warn) a failed prune, retried on the next tick.
