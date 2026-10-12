# Persistence

SQLite is the source of truth for media state, search scheduling, leases, reconciliation cursors, provider availability, rejections, installation provenance and the notification outbox. This page covers the database, the data directory, the process lock and migrations. For backing up and upgrading as an operator, see [Upgrade and backup](../upgrade-and-backup.md).

- [Data directory](#data-directory)
- [Opening the database](#opening-the-database)
- [Process lock](#process-lock)
- [Tables](#tables)
- [Migrations](#migrations)
- [Adding a migration](#adding-a-migration)

## Data directory

`data_dir` (`/data` in the container) holds:

| Entry | Defined in | Contents |
| --- | --- | --- |
| `subsyncd.db` | `internal/app/app.go` | The SQLite database |
| `subsyncd.db-wal`, `subsyncd.db-shm` | SQLite (WAL mode) | Write-ahead log and shared memory index; part of the database while it is open |
| `lapse-cache/` | `internal/app/app.go` (`syncer.Options.CacheDir`) | LAPSE speech profiles, pruned after `lapse_cache.ttl` (see [LAPSE](providers/lapse.md#speech-profile-cache)) |
| `pack-cache/` | `internal/app/app.go` (`pack.NewCache`) | Content-addressed season-pack manifests and members |
| `subsyncd.lock` | `internal/app/app.go` | The [process lock](#process-lock) |
| `subsyncd.sock` | `internal/control/protocol.go` (`SocketName`) | The daemon's [control socket](queue-and-control.md#control-socket) |

The caches can be rebuilt; the database cannot. Restoring only the database can leave pack manifests missing: those entries are detected and invalidated safely, but the cache benefit is lost. Copy the database only while subsyncd is stopped, because WAL mode keeps recent commits in the `-wal` file.

## Opening the database

The driver is `modernc.org/sqlite` (pure Go, no cgo). Both open paths use a single connection (`SetMaxOpenConns(1)`).

| | `store.Open` (mutable) | `store.OpenReadOnly` (diagnostic) |
| --- | --- | --- |
| Used by | `serve` and the mutating commands | The [read-only commands](queue-and-control.md#routed-and-read-only-commands) |
| Pragmas | `foreign_keys=ON`, `busy_timeout=5000`, `journal_mode=WAL` | URI `mode=ro` with `query_only(1)`, `foreign_keys(1)`, `busy_timeout(5000)` |
| Migrations | Applies pending ones | Never; requires every embedded migration to be applied |
| Lock | Holds the process lock first | None; reads the running daemon's WAL |

A read-only open that finds no database, an unknown applied version or an incomplete schema fails with `diagnostic database unavailable or schema not current; initialize with serve or scan before retrying`. Read-only commands therefore never create or migrate storage, add configured-language searches, or build persistent caches.

Startup never backfills data and never makes a network request. New columns get defaults; rows gain values through ordinary work (hydration, the next search, discovery).

## Process lock

Mutating assembly (`serve`, `scan`, `search`, `reject`, `retry`, `rotate-webhook-token`, `forget-instance --yes`) takes `subsyncd.lock` before migrations or any durable initialization and keeps it until the application closes.

- `(*App).acquireLock` (`internal/app/app.go`) opens the file with mode 0600 and takes an exclusive, nonblocking `flock` (`LOCK_EX|LOCK_NB`).
- A second mutator fails before changing anything, with code `busy`:
  - stop-first commands: `another subsyncd mutation process is active; stop the daemon first`;
  - routed commands (`retry`, `search`, `reject`, `scan`): `another subsyncd process holds the lock (a daemon still starting, or another command); try again shortly`.
- With a daemon running, routed commands go over the [control socket](queue-and-control.md#control-socket) instead, and the daemon's own lock covers them.
- Never delete the lock file to get around a live lock.

## Tables

No table is ever dropped; later migrations add columns, indexes and triggers.

| Table | Purpose |
| --- | --- |
| `schema_migrations` | Applied migration file names; created by the runner |
| `instances` | One row per Arr instance: reconciliation cursor and timing, discovery scope, event revision, `queue_priority`, webhook token and managed connection |
| `media` | One movie or episode file: identity (`instance`, `kind`, `file_id`, `entity_id`, `series_id`), fingerprint, metadata, deletion state, release date, series type |
| `tracks` | Embedded and sidecar subtitle tracks per media |
| `inventory_probes` | Completed-probe marker for the current fingerprint; a trigger invalidates it |
| `media_hashes` | File hashes per algorithm, bound to a fingerprint |
| `search_states` | The search queue: one row per media and language with state, attempts, schedule, lease, priority, order copies, resume progress and `replace_installed` |
| `provider_states` | Cooldowns, quotas, circuits and disabled authentication per provider and scope |
| `provider_cache` | Normalized search results (six hours) |
| `pack_cache`, `pack_members` | Cached pack manifests and their members |
| `candidates` | Scored candidates per media and language |
| `candidate_rejections` | Retained rejections, system and user (see [Rejections](providers/rejections.md)) |
| `candidate_lapse_failures` | The repeated-LAPSE-exit strike (see [LAPSE](providers/lapse.md#repeated-empty-exit)) |
| `installations` | The managed subtitle per media and language with provenance |
| `pending_installs` | Write-ahead record of a publication, move or removal in flight |
| `notifications` | The notification outbox |
| `events` | Audit log of Arr lifecycle events; inserts bump `instances.event_revision` |
| `reconciliation_replays` | History event IDs already applied, per instance |
| `webhook_inbox` | Stored webhooks waiting to be applied |
| `search_history` | One row per finished search, kept 30 days |

Triggers: `invalidate_inventory_probe` (003), `instances_event_revision` (005), `search_states_queue_order` and `media_search_order` (017).

## Migrations

Migrations live in `internal/store/migrations/` and are embedded with `//go:embed`.

- They run in file-name order (a lexicographic sort), each in its own transaction together with its `schema_migrations` row.
- Before any migration SQL runs, every applied version must be an embedded file. Otherwise the open fails with `unsupported database migration "NAME"`. Databases from the pre-release `001_initial.sql`–`010_media_entity_ids.sql` lineage fail this way too: the supported lineage starts at `001_baseline.sql`.
- Migrations are forward-only. An older build that meets a database with a newer migration refuses to start and tells the operator to restore the backup taken before the upgrade, with the matching older image.
- The runner does not check for gaps: an embedded file missing from `schema_migrations` is applied even when later ones are recorded. Several tests rely on this.

**DATA** marks a migration that rewrites or deletes existing rows.

| Migration | Change | Related code |
| --- | --- | --- |
| `001_baseline.sql` | Creates the 14 base tables and their indexes | `internal/store/repository.go` |
| `002_scrub_provider_credentials.sql` | **DATA.** Deletes cache, candidate, pack, installation and rejection rows containing `api_key=`. Sidecars whose installation row is deleted become protected, unmanaged files | Credential-free provider identities |
| `003_inventory_probes.sql` | Adds `inventory_probes`, `media.deleted` and the `invalidate_inventory_probe` trigger. **DATA.** Marks a media row deleted only on positive local evidence (every search terminal `deleted`, latest lifecycle event a delete) | `ReplaceTrackInventory`, `GetTrackInventory` |
| `004_sonarr_series.sql` | Adds `series_id` to `media` and `events` and `media_series_idx`; rows get the ID at their next hydration | `SeriesDelete` handling |
| `005_library_discovery.sql` | Adds `instances.library_scope`, `event_revision` and the `instances_event_revision` trigger | [Library discovery](reconciliation.md#library-discovery) |
| `006_fallback_installations.sql` | Adds `installations.fallback` (default 0) | [Scheduling](providers/scheduling.md) |
| `007_clear_lapse_invalid_output.sql` | **DATA.** Deletes `lapse_invalid_output` rejections so they are reconsidered once | [Rejections](providers/rejections.md) |
| `008_forced_track_probe_refresh.sql` | **DATA.** Deletes every `inventory_probes` row so files are probed again for forced-track labels | Inventory |
| `009_movie_duplicate_rejections.sql` | **DATA.** Deletes `pack_selection` rejections of movies | Movie archive selection |
| `010_reconciliation_replays.sql` | Creates `reconciliation_replays` | [Replays](reconciliation.md#replays) |
| `011_reschedule_unfinished_backfill.sql` | **DATA.** Makes pending missing-priority `no_result` searches without an installation due at once | Missing schedule |
| `012_provider_resume.sql` | Adds `search_states.resume_providers_json` and `resume_route_signature`. **DATA.** Makes pending missing-priority `throttled` searches due at once | [Resume progress](providers/limits.md#resume-progress) |
| `013_repeated_lapse_failures.sql` | Creates `candidate_lapse_failures` | [LAPSE strike](providers/lapse.md#repeated-empty-exit) |
| `014_search_queue_order.sql` | Adds `search_states.queue_order_ns` (**DATA**, copied from the due time), `instances.queue_priority`, `search_states.instance_rank`, a rank trigger, and rebuilds `search_due_idx` | [Queue order](queue-and-control.md#queue-order) |
| `015_multi_episode_range.sql` | Adds `media.episode_end` and `absolute_episode_end` | [Multi-episode files](reconciliation.md#multi-episode-files) |
| `016_release_finished_notification_keys.sql` | **DATA.** Clears `dedupe_key` on finished notifications | [Deduplication](notifications.md#deduplication) |
| `017_search_media_order.sql` | Adds `sort_title`, `sort_series`, `sort_season`, `sort_episode` (**DATA**, filled from `media`), replaces 014's trigger with `search_states_queue_order`, adds `media_search_order`, rebuilds `search_due_idx` | [Queue order](queue-and-control.md#queue-order) |
| `018_media_child_indexes.sql` | Adds `candidates_media_idx` and `events_media_idx` so media deletes do not scan whole tables | `forget-instance` |
| `019_media_released_at.sql` | Adds `media.released_at_ns` (0 is unknown) | [Aged releases](providers/scheduling.md#missing-subtitles) |
| `020_queue_robustness.sql` | Adds `search_states.resume_saved_at_ns`, the instance reconcile timing columns, `webhook_inbox` and `pending_installs` | Inbox, reconcile timing, pending installs |
| `021_pending_install_inode.sql` | Adds `pending_installs.published_inode`, `media.deleted_at_ns`, `entity_deleted_at_ns` and `series_type`. **DATA.** Dates already-deleted rows from `updated_at_ns` | [Ordering against deletes](reconciliation.md#ordering-against-deletes) |
| `022_managed_webhooks.sql` | Adds the instance webhook token and managed connection columns | [Webhook tokens](reconciliation.md#webhook-tokens) |
| `023_live_control.sql` | Creates `search_history`; adds `installations.candidate_signature`, `artifact_checksum` and `search_states.replace_installed` | [Search history](queue-and-control.md#search-history), [Reject](installation-safety.md#reject-replacement-and-removal) |
| `024_notification_finished_at.sql` | Adds `notifications.finished_at_ns` and `notifications_finished_idx`. **DATA.** Stamps finished rows with the migration time | [Retention](notifications.md#retention) |

## Adding a migration

1. **Back up first.** Copy the data directory of any database you test against while subsyncd is stopped; migrations cannot be undone.
2. **Name it** with the next three-digit number (`025_short_name.sql`). Order is lexicographic.
3. **Forward-only.** Never edit or rename an applied migration. Fix mistakes with a new one.
4. **Defaults, not backfills.** New columns take a default that keeps current behavior. A one-time data rewrite in SQL is allowed when it only uses local data; mark it in the table above. Values that need Arr, a provider or the filesystem arrive through ordinary work (hydration, the next search, discovery), never at startup.
5. **No network and no filesystem scan** at startup or in the migration.
6. **Test it** in `internal/store`: apply the embedded files with a prefix below yours, seed rows, `Open`, and assert. Existing cutoff tests keep working. `series_delete_test.go` drops a hand-written list of triggers and indexes that reference `series_id`; add yours there if it does too.
7. **Read-only opens** need no change: they compare against the embedded file count.
8. **Document it** in the table above and on the page of the behavior it serves, and tell operators in the release notes to back up before upgrading.
