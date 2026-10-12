# Webhooks and reconciliation

How Sonarr and Radarr changes reach the catalog: the webhook inbox, history reconciliation, library discovery, and the managed Arr webhook connection. For setting up webhooks as an operator, see [Running subsyncd](../running.md).

- [Catalog identity](#catalog-identity)
- [Webhook intake](#webhook-intake)
- [Webhook inbox](#webhook-inbox)
- [History reconciliation](#history-reconciliation)
- [Deferred media](#deferred-media)
- [Snapshot-deletion guard](#snapshot-deletion-guard)
- [Replays](#replays)
- [Library discovery](#library-discovery)
- [Configured languages and instances](#configured-languages-and-instances)
- [Multi-episode files](#multi-episode-files)
- [Webhook tokens](#webhook-tokens)
- [Managed Arr connection](#managed-arr-connection)

## Catalog identity

Each instance needs a unique `name`, an API key, and one or more remote-to-local path mappings. The longest boundary-aware mapping wins. A mapping destination must lie inside a configured media root; existing parent symlinks are resolved before it is accepted.

Every media row has a positive stable Arr entity ID (`movieId` or `episodeId`). The physical Arr file ID is separate and replaceable; hashes, inventory, installations and webhook processing use it. See [Architecture](../architecture.md#catalog-flow) for why both exist.

Path mapping is also a scope boundary:

- An import or rename whose media is deliberately outside the configured mappings is ignored. It creates no audit row, media or search row, and no worker wake, so an Arr connection can target a narrow canary without retries for the rest of the library.
- Only that typed scope condition is ignored. Traversal, unsafe filesystem resolution, Arr transport or authentication failures, and SQLite failures still return an error.
- A path whose lookup fails with a mount error (`domain.MountUnavailable`: `ENOTCONN`, `EIO`, `ESTALE`, `EHOSTDOWN`, …), on the path, a parent or the media root, is `errMappedPathUnavailable`. The entity is deferred rather than failing the page or being treated as removed. Other errors (permissions, traversal) fail the page.

## Webhook intake

`httpapi` authenticates `POST /webhooks/INSTANCE?token=TOKEN`, limits the body to 1 MiB, and logs only `/webhooks/INSTANCE`, never the query string.

| Event | Answer |
| --- | --- |
| Download/import (including upgrades), rename, file delete, `MovieDelete`, `SeriesDelete` | HTTP 202 once the body is stored (`webhook.queued`) |
| Any other event type (grab, health, series or movie added, application update, the other app's delete events) | HTTP 204, ignored, so a connection with extra events enabled is not reported as failing |
| Malformed payload, or one missing the identity its event needs | HTTP 400 (`webhook.rejected`) |

Delete handling:

- File-delete webhooks are addressed by file ID.
- Radarr `MovieDelete` resolves `movie.id` as a stable movie entity inside the deletion transaction.
- Sonarr `SeriesDelete` resolves `series.id` against `media.series_id`; all matching episodes, searches and audit rows commit atomically. Rows without a series ID (`004_sonarr_series.sql` defaults it to 0) are never matched by title, path or external ID.
- Neither whole-delete event hydrates deleted Arr objects, reads media files, or uses the `deletedFiles` flag. Unknown identities are audited idempotently without changing other media.

## Webhook inbox

A stored webhook lives in `webhook_inbox` (migration 020) with its receipt time. `catalog.WebhookInbox` applies it in the background:

- One processor per instance applies that instance's webhooks in arrival order, so one media item's changes keep their order and concurrency is bounded by the number of instances. Arr lookups never hold the HTTP request.
- A webhook that cannot be applied (Arr unreachable, SQLite busy) is retried after 1 minute, 5 minutes, 15 minutes, 1 hour, 6 hours and 24 hours while later webhooks continue. After that it is abandoned: `webhook.failed` with `outcome=abandoned` at error level. The six-hourly history reconciliation still picks up the change.
- An invalid stored payload is abandoned at once.
- When the inbox itself fails (it cannot be read, or a webhook cannot be deleted or rescheduled: a full disk, a read-only database), the processor logs `webhook.failed` with `outcome=inbox_unavailable` and waits 5 seconds, doubling up to 5 minutes, before the next pass, ignoring wakes meanwhile. A successful pass resets the wait. Without this, a webhook that stayed due would be applied against Arr again in a tight loop.
- Webhooks stored for an instance that is no longer configured are not processed. Startup logs `webhook.inbox_orphaned` (warn) with the count per instance, and `forget-instance --yes` deletes them.
- Webhooks still stored at shutdown, or left by a crash, are applied at the next start.

### Ordering against deletes

Each change is dated by its receipt time. A delete records its time on the media row (`deleted_at_ns`, migration 021; set only by deletes).

- An import or rename is skipped when a delete applied after the change happened removed the same file, or removed the whole movie or episode. A delete without a file ID (`MovieDelete`, `SeriesDelete`, or history finding no current file) also records `entity_deleted_at_ns`.
- A delete of another file of the same movie or episode never blocks it: when an upgrade's import is retried after the old file's delete was applied, the new file still revives the row.
- A change that happened after the delete (a real re-import) applies normally.
- `updated_at_ns` is not compared, because inventory refreshes and other writes move it.

So a retried or late upsert cannot revive a deleted row.

## History reconciliation

Each instance reconciles from Arr history every six hours with its own persisted cursor. Reconciliation uses `github.com/cplieger/arrapi/v2` v2.0.5 for bounded, retried history and current movie and episode requests.

- History is fetched in descending 100-record pages with strict page-count, ordering and progress checks. Malformed or inconsistent pagination advances nothing.
- History is grouped by stable Arr entity (`movieId` or `episodeId`), not by physical file ID. Current state decides whether that entity is present, absent or outside the configured scope.
- Imports, renames, upgrades, deletion tombstones, audit rows, media and search changes, and the new cursor commit in one SQLite transaction. A failed page leaves the previous cursor intact.
- Arr history accepts whole-second timestamps, so the request rounds the cursor down and overlaps its fractional second. Stable history event IDs make the replay idempotent.
- A hardened detail client adds release, quality, edition, runtime and provenance fields that arrapi does not expose. It rejects redirects, even within the same origin, and reports only bounded error categories, preserving cancellation and timeout identity.
- When a media path changes, managed installation paths are handled as described in [Installation safety](installation-safety.md#replaced-and-renamed-media).
- Webhook batches skip outside-scope items individually and continue with later items. Committed event IDs are checked before hydration, so an exact redelivery does not depend on the old file still being there.

### Timing

- An instance never reconciled is due at once. So is one whose last reconciliation succeeded but whose library discovery is pending again at startup (its discovery scope changed).
- After a failure the cursor is kept and the attempt is retried after 5 minutes, 15 minutes, 1 hour, then every 6 hours until it succeeds (`reconcile.failed`, error). One failed instance neither advances nor delays another.
- The attempt time, failure count and deferral flag live on the instance row (`instances.reconcile_attempted_at_ns`, `reconcile_failures`, `reconcile_deferred`, migration 020). A restart therefore neither repeats a reconciliation that just ran nor forgets a failure backoff; the first check after a start runs only the instances that are due.
- Reconciliation runs instances in `queue_priority` order (highest first, ties by name), so on a fresh database the highest-ranked library is queued first.

### Leases during reconciliation

If history reports a change for media already being processed, the current lease owner and expiry stay intact and one immediate rerun is coalesced. An old completion cannot erase that rerun. See [Queue and control](queue-and-control.md#leases).

### Startup is offline

Startup assembly and readiness make no Arr request, run no full-library scan and do no identity backfill. Reconciliation, discovery and the [managed connection sync](#managed-arr-connection) are background work that starts after the service is up.

## Deferred media

A page that commits but defers entities whose current media cannot be read (`ErrHistoryDeferred`, typically a broken symlink into a FUSE mount) is not a failure.

- Catalogs return a typed `DeferredHistoryError` listing each entity with a log-safe title from `observability.MediaTitle`. Sonarr fetches series titles best effort, once per series, only for the ten entities that are logged, and stops after the first failed lookup.
- The readable mutations commit through the replay table; the cursor is kept.
- Library enumeration (`ListLibrary`) defers the same way: unreadable files are skipped and reported (a Sonarr file read before its episodes is labelled by file ID, series title and season), the readable media commit through `CommitPartialLibraryDiscovery`, and the discovery scope stays incomplete. A mount outage therefore cannot mark skipped files as discovered.
- A deferred discovery does not stop `Run`: history still reconciles, and both deferrals are merged into one `DeferredHistoryError`.
- The worker logs `reconcile.deferred` (warn) and retries after one hour, or the normal interval if that is shorter, without escalating the failure backoff. A later real failure starts the backoff at 5 minutes.
- There is no time limit: the cursor stays until every deferred entity's media is readable or Arr no longer reports it.
- `scan` reports deferred entities in its summary instead of failing.

The worker and `scan` decide on the `ErrHistoryDeferred` sentinel and use the typed error only for labels.

## Snapshot-deletion guard

Each run compares a complete top-level identity snapshot (Sonarr series or Radarr movies) with the active identities and retires the absent ones. Snapshot deletions are permanent: rediscovery never revives deleted rows.

The run withholds the snapshot's deletions (`withholdSnapshotDeletions`, `internal/catalog/reconcile.go`) when at least two identities are active and either:

- the snapshot would retire all of them, or
- it would retire more than half of them and at least `massDeletionMinimum` (10).

History mutations and the cursor still commit, and `reconcile.snapshot_deletions_withheld` (warn) is logged. Zero overlap alone is not the test: a rebuilt or different Arr instance numbers from 1 and shares low IDs by coincidence. An empty or foreign Arr database therefore cannot delete the catalog.

## Replays

History also contains every change a webhook already delivered, under a different event ID. Reconciliation marks those import and rename mutations `Replay` when:

- the file (same instance, kind, file ID and entity) is already active with the same path, size and unsupported reason, and
- the media row was last updated at or after the history record's time, as a webhook's apply time always is.

A replay is recorded and linked (`reconciliation_replays`, migration 010) but does not reopen searches. The check reads only the media row, so audit-log pruning cannot defeat it. Any difference, a deleted row, or a history record newer than the row reopens the searches. Webhook, library-discovery and multi-episode re-check events are never treated as replays.

## Library discovery

Concrete Sonarr and Radarr catalogs expose full-library enumeration through arrapi. Sonarr lists series then their episode files; Radarr lists movies with files. Both check scope before detail hydration.

- A new instance gets an empty reconciliation cursor and an incomplete discovery marker. Background maintenance enumerates all current in-scope files once before history reconciliation, including files that never appear in history.
- Completion is keyed by a scope digest (`libraryDiscoveryScope`, `internal/app/discovery.go`): instance type, URL, mappings, media roots and a scope version (`library-v3`). Changing any of them permits another pass. Credential and language changes do not.
- Discovery captures the instance's event revision before network work, inserts only unknown media with missing-priority searches, and records completion only if the revision still matches. Every inserted event advances the revision, so a concurrent webhook makes the snapshot retry without publication.
- Existing rows (including deleted rows), leases, schedules, rejections and installations are never changed, and a snapshot's omissions never delete rows.
- During discovery a known row also takes a release date it lacks, and a non-zero runtime that differs from the stored one, but only when the row still holds the listed file.
- Empty successful snapshots count as completed. A busy instance may delay discovery until a pass completes without a concurrent event.
- `scan` repeats additive discovery on demand. See [Queue and control](queue-and-control.md#scan-fence).

## Configured languages and instances

Language and instance configuration is read at startup only; operators restart after a change.

- When a configured language has no search row for an indexed media item of a configured instance, startup creates one, due at once at missing priority, using only SQLite. Existing rows (attempts, upgrade times, leases, rerun state) are never reset.
- Embedded or protected subtitles can satisfy those new searches without a provider request.
- Unsupported multi-episode files get the same terminal unsupported outcome as at import.
- Adding a provider to a language changes that language's future searches without rewriting its schedule.

## Multi-episode files

Sonarr can attach several episodes to one file. The earliest episode is the canonical entity.

- Consecutive episodes of one season form a range: `media.episode_end` and `media.absolute_episode_end` (migration 015; absolute numbers are kept only when every episode has one and they are consecutive). Searches run normally, and `explain` shows `episodes=S06E01-E02`. Matching rules are in [Matching](providers/matching.md#episode-ranges).
- Any other combination persists `unsupported_multi_episode` with `episode_end = -1`, marks every configured language terminal, and skips provider, LAPSE, installation and upgrade work.
- A row marked unsupported with `episode_end = 0` is re-read once by the next Sonarr reconciliation and committed as an import, which stores the range or the checked marker. A failed re-read logs `reconcile.multi_episode_recheck_failed` (warn) and is retried at the next reconciliation. The re-check's event ID is stable per file, so a retried commit applies once.
- A later import or rename re-evaluates the file.

## Webhook tokens

Webhook tokens are generated, never configured. `webhook_token` in the configuration is refused at load with an error that names the instance and never the value.

- Migration 022 adds `instances.webhook_token` (empty until generated), `webhook_connection_id`, `webhook_connection_arr_url` (the Arr URL the ID was recorded against) and `webhook_checked_at_ns`.
- The mutable assembly (`serve` and the mutating commands) calls `EnsureWebhookTokens` after migrations, once the instance rows exist: 32 bytes from `crypto/rand` as 64 lowercase hex characters, stored once and added to the redaction set.
- Read-only assemblies never generate or write a token. `webhook-url` on a database without one fails and asks for one start.
- `webhook-url` prints `<base>/webhooks/<instance>?token=<token>`, with the base from `--base`, else `server.webhook_base_url`. With neither, it prints only the path and query.
- `rotate-webhook-token` takes the mutation lock, so the daemon cannot be running, and replaces the token in one statement (`webhook.token_rotated`). With `server.webhook_base_url` set, the sync at the next start rewrites the managed connection; otherwise the operator updates Arr by hand.
- `doctor` reports `webhooks: manual` or `webhooks: managed (<base url>)`.

## Managed Arr connection

When `server.webhook_base_url` is set (one value for every instance), `WebhookConnectionMaintainer` runs beside the inbox processor and the worker. It never takes `reconcileMu` or touches reconciliation state.

### Finding the owned connection

`WebhookConnectionSync` looks for the one connection subsyncd owns for an instance, in this order:

1. **Remembered ID.** The stored connection ID, recorded against the same Arr URL, that still exists as a `Webhook` implementation whose unescaped URL path ends in `/webhooks/<instance>`. Host and token may differ and are repaired. If the path does not match (an ID reused after Arr's database was reset can name another webhook), the remembered ID is ignored.
2. **Adoption.** A `Webhook` connection whose URL path ends in `/webhooks/<instance>` and that is positively ours: it carries the stored token, or it has the same origin as the base URL (scheme, host, effective port) at exactly `<base path>/webhooks/<instance>`. A second deployment behind the same proxy differs only by path prefix and is never adopted. More than one match logs `webhook.connection_ambiguous` and changes nothing; path matches that are not ours are counted in `webhook.connection_foreign`.
3. **Create.** A copy of Arr's `Webhook` schema template named `subsyncd (<host:port of the base URL>)`, with every event flag off except the required ones the template supports (`supportsOn...`).

### Repair

The desired URL is `<base without trailing slashes>/webhooks/<PathEscape(instance)>?token=<token>`. Repair sets only the managed keys:

- `url`, `method` (POST), and the required event flags;
- Sonarr: `onDownload`, `onUpgrade`, `onRename`, `onSeriesDelete`, `onEpisodeFileDelete`, `onEpisodeFileDeleteForUpgrade`;
- Radarr: `onDownload`, `onUpgrade`, `onRename`, `onMovieDelete`, `onMovieFileDelete`, `onMovieFileDeleteForUpgrade`.

Resources are handled as raw JSON, so every other key (name, tags, other events, headers, unknown future keys) round-trips unchanged; `changed` in `webhook.connection_repaired` names only the groups that were set. Notification resources have no `enable` key: a connection is enabled when any event flag is on. `forceSave` is never sent.

### Arr's save-time test

Arr tests the webhook URL on every save with any event flag on; `forceSave` does not skip it.

| Arr answer | `reason` |
| --- | --- |
| 500 (Arr could not reach subsyncd) | `arr_test_unreachable` |
| 400 validation error other than a `Name` collision (typically a `Url` property error) | `arr_test_rejected` |
| 400 whose only property is `Name` | retried with ` 2` through ` 9` appended, then `name_unavailable` |
| Any other status | `arr_status` |

Both error bodies echo the URL with the token, so the client reads a 400 body only for `propertyName` values and discards every other non-2xx body unread. Arr's own HTTP proxy settings apply to the test call. A test call that hangs exhausts the client's 15-second request timeout and is reported as `arr_unreachable`.

### Schedule

- All instances are due at start. Each sync is bounded at 60 seconds and instances sync one after another.
- After success the next check is 6 hours away. Consecutive failures of one instance retry after 15 minutes, 1 hour, then every 6 hours until a success.
- Each failure logs `webhook.connection_sync_failed` once with `reason` and `status`. A failure that is not a `WebhookSyncError` logs `reason=internal` with `ErrorAttrs("webhook_connection_sync", err)`.
- After each successful create, adopt, repair or no-op check, the connection ID, Arr URL and check time are recorded. An ambiguous match records nothing, counts as success for the schedule, and warns again at the next check.
