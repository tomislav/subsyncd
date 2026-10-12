# Queue and control

The durable search queue, the worker that leases from it, shutdown, and the control socket the CLI uses to reach a running daemon. For the commands themselves as an operator uses them, see [Commands](../commands.md).

- [Queue order](#queue-order)
- [Worker](#worker)
- [Leases](#leases)
- [Shutdown](#shutdown)
- [Job time limit](#job-time-limit)
- [Unreadable media and the media breaker](#unreadable-media-and-the-media-breaker)
- [Control socket](#control-socket)
- [Routed and read-only commands](#routed-and-read-only-commands)
- [Media selection](#media-selection)
- [Manual search lease](#manual-search-lease)
- [Scan fence](#scan-fence)
- [Search history](#search-history)

## Queue order

`search_states` rows are the queue. `LeaseDueSearchesExcept` leases due rows in this order:

1. **Class** (`priority`): `import` (an imported or renamed file), then `missing` (missing or rejected subtitles, discovery and new languages), then `upgrade` (scheduled checks of retained managed subtitles).
2. **Instance rank**: the instance's `queue_priority` (0–1000, default 0), highest first.
3. **Queue position** (`queue_order_ns`), oldest first.
4. **Due time** (`next_attempt_at_ns`), oldest first.
5. **Media order**: title, then Sonarr series ID, then season (movies use their release year; specials, season 0, follow every regular season), then episode.
6. Media ID and language.

Details:

- Queue position equals the due time on every write except a throttled completion, which keeps it. A provider reset with jitter therefore changes when a search becomes eligible but not where it sits. A manual-search upgrade schedule keeps an earlier queue position.
- Media order matters because discovery, `scan` or a new language queues its whole batch at one queue position. The batch then runs show by show from S01E01 with specials last, and movies alphabetically with remakes oldest first.
- Titles sort with SQLite's `lower()`, which folds ASCII letters only, so accented initials sort after `z`.
- Migration 017 copies the media order onto each search (`sort_title`, `sort_series`, `sort_season`, `sort_episode`). The `search_states_queue_order` trigger fills them and `instance_rank` on insert, and `media_search_order` keeps them in step when a media row's title, series, year, season or episode changes. `search_due_idx` covers the whole order, so leasing reads it from the index.
- Startup stores each configured instance's `queue_priority` and re-ranks existing searches in one transaction (unlisted instances rank 0).
- Technical failures and provider throttles keep the job's class. Priority changes dispatch order only; it never bypasses rate limits or cooldowns.

When each search becomes due is described in [Scheduling](providers/scheduling.md).

## Worker

`worker.Worker` runs one loop for searches, notifications and maintenance.

- `worker.max_concurrent` (default 1, range 1–8) bounds concurrent media workflows. Raise it only when the host and media storage can sustain concurrent provider preparation and LAPSE reads; each workflow also uses `/tmp` space.
- The worker leases at most 10 searches per dispatch and only into free slots. Claims include only configured instance/language routes and active media.
- Before each lease, the [route gate](providers/limits.md#route-gate) excludes language and media-kind routes whose providers all cannot download.
- A webhook sends a nonblocking advisory wake after its commit, so a free slot is filled without waiting for the 30-second poll. Wakes may coalesce; startup and the poll read SQLite again, so correctness never depends on a wake.
- A new event for a media and language already being processed requests exactly one rerun instead of a concurrent duplicate.
- Maintenance runs on the poll: due reconciliations (see [Reconciliation](reconciliation.md#timing)), notification delivery and pruning (see [Notifications](notifications.md)), and, hourly, LAPSE cache and search history pruning.

Removing a route keeps its stored work without retrying it; re-adding the route makes it claimable again. `forget-instance` is the only way to discard a removed instance's records.

## Leases

- A search lease lasts 5 minutes and is renewed every minute. Notification leases work the same way.
- Leases are renewed and completed by owner compare-and-swap (`CompleteSearch` checks the owner), so a lost lease writes nothing.
- An expired lease counts as no lease everywhere: claiming it clears a rerun the dead owner left requested, and `retry --provider`, a manual search's claim and its settlement treat it as unleased.
- Deleting media keeps any active lease until its owner completes; the completion leaves the deleted media terminal. A re-import during that lease requests one rerun.
- A job that returns because of shutdown gives its lease back (`ReleaseSearchLease`, `ReleaseNotificationLease`, best effort within 5 seconds), so the next start leases it at once. An undelivered notification group releases every member.

## Shutdown

The shutdown window is fixed at 30 seconds (`defaultShutdownTimeout` in `internal/worker/worker.go`); it is not configurable.

1. New acquisition stops.
2. Workers get one 30-second graceful drain window.
3. Active work is canceled.
4. Workers get one more 30-second window, then the daemon returns even if a dependency ignores cancellation.

The control server and canceled manual searches drain inside these windows; no component adds its own. The Compose example's 75-second stop grace covers both windows with margin. Raise it if the window ever changes.

A cancellation-insensitive operation may outlive the daemon. Its lease is deliberately not cleared and becomes claimable after expiry, so do not start a replacement process against the same media mounts until the old one has stopped. `worker.drain_timed_out` and `worker.drain_abandoned` report a drain that did not finish.

## Job time limit

Each search job has an overall limit, `Worker.JobTimeout`: twice the longest single LAPSE run, `2 × max(sync.max_timeout, sync.timeout)`, so 8 hours by default. Later LAPSE runs on the same media reuse the speech cache, so a legitimate job stays well inside it.

- Past the limit the workflow's context is canceled. The worker runs the workflow in its own goroutine and waits at most 2 more minutes (`defaultJobGrace`). Lease renewal runs on the worker's context and continues meanwhile.
- A workflow that returned an error by then, or that is abandoned because it is blocked in a call that ignores cancellation (a read on a hung mount), is recorded as a timeout: `job.timed_out` (warn), the search completes as a technical failure on the failure schedule, and the slot is released.
- A workflow that returned a result in time keeps it.
- An abandoned workflow keeps running with its canceled context. `Installer.Install` checks the context immediately before publishing, so it cannot install after its search completed.
- If renewal fails during the grace period (`job.lease_lost`), the timeout is still recorded while the lease is ours; otherwise the search would be leased again beside the stuck workflow.

## Unreadable media and the media breaker

`workflow.MediaUnavailableError` marks a search whose media file could not be found or read: `ENOENT`, or a mount failure from `domain.MountUnavailable` (`ENOTCONN`, `EIO`, `ESTALE`, `EHOSTDOWN`, `EHOSTUNREACH`, `ETIMEDOUT`, `ENODEV`, `ENXIO`). The errno is kept, never the path.

### Where it comes from

- `checkMediaAvailable` returns it directly.
- An ffprobe run that does not finish within 5 minutes (`inventory.ErrProbeTimedOut`) is marked too. The probe is awaited only until then, since a process blocked in a FUSE read cannot be killed.
- A timed-out run is tracked until its process returns. While one is blocked for a path, probing that path returns `ErrProbeTimedOut` at once. While four are blocked on one mount (`maxStuckProbes`), no new probe starts on that mount; other mounts still probe.
- The mount (`probeMount`) is the longest media root containing the path plus the next directory below it (`/media` + `tv` is `/media/tv`), or else the path's first two directories. A media path that is a symlink is grouped by its link target (`mediaMount`), read with `domain.LinkTarget` (bounded to 5 seconds, never followed). No stat is used, because it would block on the hung mount.
- A probe ended by the job's cancellation does not count. Any other inventory refresh failure is marked only when a fresh stat of the media file fails the same way, so a missing ffprobe, LAPSE binary or scratch file never counts.
- Before a LAPSE `nothing` or `unsure` verdict becomes a rejection, the workflow re-checks the media (`checkMediaReadable`: stat, then a 64 KiB read at the start and the middle, bounded by one minute). LAPSE hears no speech in a file it cannot read, so unavailable media ends the search with `MediaUnavailableError` instead of rejecting the candidate.

Such a failure takes the technical-failure backoff but completes with `PreserveQueueOrder`, so a storage blip does not send searches to the back of the queue.

### The breaker

The worker counts consecutive media-unavailable failures; any other completion resets the count and the pause length.

- A not-found failure (`MediaUnavailableError.Missing`) may be one deleted file. Before counting it, the finished job probes the storage behind it (`Worker.MediaRoots`): the file's directory, or the nearest existing directory above it up to its media root. For a symlink, the probe examines the link target's directory instead, up to the deeper of the target's media root and its first two directories.
- A directory that lists at least one entry within 10 seconds means the mount is up and only this file is missing. The failure is logged as `queue.media_file_unavailable` (info) and neither counts nor resets the breaker.
- An empty mount point, a failed listing other than a missing directory, or a probe timeout counts.
- Timeouts, read errors and mount errnos always count, whatever the listing says: some FUSE mounts serve listings from metadata while reads hang.
- Without media roots every failure counts.
- At three counted failures the breaker holds all leasing (daemon dispatch and `RunOnce`) and logs `queue.media_paused` (warn). The pause starts at one minute and doubles, up to 30 minutes, each time it trips again before any search has read its media.
- The probe runs in the finished job's goroutine with its own timeout, never in the dispatcher.

## Control socket

A running daemon serves `<data_dir>/subsyncd.sock` (`internal/control`).

- Mode 0600, created after the process lock is held. A stale file is replaced at start and the socket is removed on shutdown.
- A Unix socket path is limited to about 104 bytes on macOS and 108 on Linux, so a very long `data_dir` makes the listen fail. Tests use short temporary directories for this reason.
- Each connection carries one protocol-version-1 JSON request (at most 64 KiB) and one response. A version mismatch is `protocol_mismatch`.
- It is one more component in `serve`'s shutdown: it stops with the run context, answers `daemon_stopping` meanwhile, and drains in-flight requests for at most 30 seconds (`Server.DrainTimeout`). A handler still running then is left behind with its connection closed.
- Readiness does not depend on it, but a failure to listen or accept ends `serve` with an error, like the HTTP server's.
- `App.HandleControl` decodes arguments strictly: unknown fields or an unknown command are `usage` errors. It reuses the `App` method; because the daemon holds the lock, that method takes the no-op lock path.
- Each request logs `control.request_completed` (info) or `control.request_failed` (warn).

## Routed and read-only commands

| Kind | Commands | How they run |
| --- | --- | --- |
| Routed | `retry`, `search`, `reject`, `scan` (`control.Routed`) | The CLI dials the socket after loading the configuration for `data_dir`. With no daemon (`control.ErrNoDaemon`: no socket or nothing accepting) or an unloadable configuration, it opens the backend locally and takes the lock. Any other dial error fails the command |
| Read-only | `explain`, `find`, `missing`, `recent`, `status`, `providers`, `doctor`, `analyze-sync`, `webhook-url`, `forget-instance` without `--yes` (`forget-instance-preview`) (`readOnlyCommand` in `internal/app/app.go`) | Open the database read-only, without the lock, migrations or startup writes; they run beside the daemon |
| Stop-first | `serve`, `rotate-webhook-token`, `forget-instance --yes` | Take the lock and fail with `busy` while the daemon runs |

Notes:

- Without a daemon, the routed methods take the lock before checking `--timeout` and `--no-wait`. A daemon still starting (lock held, socket not yet listening) therefore reads as `busy` with a message that names that case (`App.acquireRoutedLock`). `--timeout` and `--no-wait` without a daemon fail with `daemon_required`.
- `retry` in the daemon signals the worker's wake channel without blocking.
- `status` is read-only and never routed. It reads saved state, then asks the daemon for live state (`statusLive`: daemon state and media pause) over the socket with a 2-second timeout (`addDaemonState` in `internal/cli/cli.go`). No daemon reads as not running; an unreachable one sets `daemon unreachable`. It never fails because of the daemon.
- `missing`, `recent` and `providers` read only the database and touch no media file, so a hung mount cannot stall them.
- Startup failures already emitted as structured events are not printed again as raw CLI errors.
- Exit status is 0 for success, 1 for a failure, and 2 when the CLI rejects the command line. Scripts should branch on `error.code` in `--json` output. Codes are defined in `internal/control`.

## Media selection

`find`, `explain`, `search`, `reject` and `missing --title` share selection (`control.Selection`; `App.Resolve` and `App.Find` in `internal/app/find.go`).

- Exactly one of `--file-id` (with `--instance` and `--kind`), `--title` or `--path`. `--instance` and `--kind` narrow the other two.
- `--title` is parsed by `selection.ParseTitle` and matched by `Repository.MatchMedia`. SQL narrows by instance, kind, year and season/episode (a range that covers the episode matches). Titles and alternate titles are then normalized in Go (`selection.Normalize`: lowercase, a fold table for Latin-1 and Latin Extended-A, apostrophes and periods dropped, other punctuation a space), because SQLite's `lower()` folds ASCII only.
- An exact normalized title ranks before a substring, and only the best rank is returned. Deleted media never matches.
- `--path` is compared exactly with `media.path`, both as given (cleaned) and after `catalog.MapPath` through each instance's mappings.
- `Resolve` returns one file or `media_not_found` / `ambiguous_media` (at most 25 candidates, each with its episode range). `find` returns all matches and an empty list when none match.
- Language state for `find` comes from `Repository.LanguageStates`, in precedence order: running (a live lease), installed (an installation row), satisfied (last outcome `satisfied`), missing.
- `search --title` or `--path` resolves only indexed media and then follows the `--file-id` path, including the Arr lookup (a connection failure or timeout is `arr_unreachable`). `--file-id` alone indexes an unknown file.
- Without `--language`, `search` takes the configured languages that are missing. When none is, nothing runs and each language reports `satisfied`, or `running` for a live lease.

## Manual search lease

Every manual search, in the daemon or in process, goes through `App.runManualSearch` (`internal/app/manual_search.go`), so the worker never runs the same media and language beside it.

### Claim

1. Take one of two manual slots (`manualSlots`, a FIFO semaphore separate from `worker.max_concurrent`, shared with `reject` follow-ups). A waiter whose context ends leaves the queue holding nothing.
2. Claim the search's lease with `Repository.ClaimSearch` under a fresh `manual-<hex>` owner: one upsert that creates a missing row as a pending missing-priority search, or sets the lease only when the row is unleased or expired.
3. While another owner holds a live lease (normally the worker running that search), the claim reports false and is retried every second. The manual search waits for the worker and then runs with full manual semantics. It never sets `rerun_requested`.

### Run

- The lease is renewed every third of its 5-minute duration; a failed renewal cancels the run.
- The run clears provider resume progress, clears subsyncd's own rejections for `--retry-rejected`, and runs the workflow with `Manual: true`.
- It uses the worker's [job time limit](#job-time-limit) and abandonment rule. A timed-out run logs `job.timed_out` and, while it owns the lease, completes the search as a technical failure (`workflow_error`).
- Once the workflow has returned, the bookkeeping (completion, settle, release, upgrade schedule, history row) runs under `context.WithoutCancel`, so a shutdown right after an install still records it.
- It logs `job.started` and `job.completed` (`component=app`, `priority=manual`, the manual `job_id`).

### Completion

- **Installed**: `App.completeManualInstall` completes the search under the manual lease with `CompleteSearch`, as the worker does: the new subtitle's next upgrade check (or `complete` for a preferred exact hash), counters reset, lease released in the same owner-guarded update. A rerun requested during the claim keeps the search pending.
- **Other outcomes**: when the claim created the row, or the search just added or revived the media, a `satisfied` outcome settles that pending search with `SettlePendingSearch`. Otherwise a search queued earlier keeps its class, due time and counters; `EnsureUpgradeSearch` can only bring its due time forward, and runs after the lease is released.
- **Failure paths** (claim failure, workflow error, cancellation) release the lease from a deferred call with a fresh 5-second context. Releasing a lease no longer held does nothing.

### In the daemon

`search` resolves and indexes the media on the request, then starts one `runManualSearch` per language on `serve`'s run context, so a client that disconnects cancels nothing.

- The handler waits for all of them or for `SearchArgs.Wait` (5 minutes when 0; not at all with `NoWait`) and reports `running` for each language not finished. A language that failed fails the request.
- The CLI sends `Wait` only for `--timeout` and `NoWait` only for `--no-wait`.
- On shutdown the run context is canceled with the worker's, the waiting handler answers `daemon_stopping`, and `serve` waits up to 30 seconds, inside the worker's windows, for the canceled searches to release their leases.

## Scan fence

A daemon `scan` (`App.scanInDaemon`) waits like a manual search (default 5 minutes, none with `--no-wait`, `running` when the wait ends first) but runs on the daemon's run context, so a vanished client never cancels it.

- It runs inside the reconcile fence: `Worker.WithReconcileLock` takes `reconcileMu`, which each scheduled reconciliation also holds. A scan and a scheduled reconciliation never overlap, and two scans serialize.
- The worker's own reconcile path uses `TryLock` and skips that tick while a scan holds the fence, so a scan never delays leasing or notification delivery.
- Only discovery and history reconciliation run inside the fence. The `--force-probe` loop runs after it; a file replaced or deleted since the listing (`store.ErrStaleInventory`) is counted in `probe_skipped` instead of failing the scan.
- Every daemon scan logs `scan.completed` or `scan.failed`, since the request may have stopped waiting first.
- Scans count in `manualRuns`, so shutdown cancels them and waits inside the manual drain window.
- Without a daemon, `App.Scan` takes the lock and runs to completion.

## Search history

`search_history` (migration 023) keeps one row per finished search for `recent` and `status`.

- Columns: the media reference (`instance`, `kind`, `file_id`) and `media_title` copied at the time, `language`, `outcome`, the winning or last candidate's `provider_id` and first `release_name`, `reason`, `manual`, `finished_at_ns`. `media_id` is `ON DELETE SET NULL`, so a row outlives its media.
- Rows hold no paths, provider text or credentials. Titles come from the same helper as the `media_title` log field.

Who writes the row:

| Path | Row |
| --- | --- |
| Worker completion | Inside the `CompleteSearch` transaction, after the owner guard, so a lost lease writes nothing. A `deleted` completion writes none |
| Manual search | `manual=1`, written last, after lease release and upgrade scheduling, outside the installer's transaction. A failed write logs `history.record_failed` (warn) and does not fail the search |
| Manual timeout | Through `CompleteSearch` |
| `reject` | `RecordUserRejection` writes a `rejected` row (`manual=1`, reason `user_<reason>`) in its own transaction |
| A reject's replacement run | Inside `CompleteSearch` |
| A removal | `RemoveManaged` writes a `removed` row in its commit; the search completion then writes none |

The daemon's hourly maintenance deletes rows older than 30 days (`store.HistoryRetention`). A failure logs `history.prune_failed` (warn) and is retried the next hour.
