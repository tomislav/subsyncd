# Installation safety

How subsyncd writes, replaces, moves and removes subtitle files without losing the owner's files or leaving untracked ones behind. For permissions and mounts as an operator sets them up, see [Running subsyncd](../running.md).

- [Ownership and inventory](#ownership-and-inventory)
- [Temporary files](#temporary-files)
- [Publication](#publication)
- [Pending installs and crash recovery](#pending-installs-and-crash-recovery)
- [Rollback copies](#rollback-copies)
- [Replaced and renamed media](#replaced-and-renamed-media)
- [Reject, replacement and removal](#reject-replacement-and-removal)
- [Site watermark cues](#site-watermark-cues)
- [Permissions](#permissions)

## Ownership and inventory

Every write stays inside a configured media root, is syntax-validated, staged beside its destination, fsynced, atomically published, checksum-recorded, and tied to provenance and notification intent in one transaction.

### Embedded tracks

- FFprobe indexes every embedded subtitle stream, text and image codecs alike. A completed-probe marker (`inventory_probes`) records successful probes, including empty results; catalog metadata alone never makes the cache valid. The result is reused until path, Arr file ID, size or nanosecond mtime changes.
- Inventory commits compare the original catalog snapshot transactionally and recheck live media after probing, so stale work cannot restore a replaced or renamed file identity.
- A track counts as forced when the container says so or its title carries an explicit label such as `English [Forced]`. Negated labels (`not forced`, `forced subtitles removed`) do not, and the container disposition wins. Track titles are not stored, so changing this classification needs the probe markers invalidated (as migration 008 did).
- A matching full embedded track satisfies the language. Forced-only and unknown-language (`und`) tracks do not. SDH/HI tracks satisfy only with `allow_hearing_impaired: true`.

### Sidecars

- `.srt`, `.ass`, `.ssa` and `.vtt` files for the exact media stem are scanned and checksummed before every search; they are never cached.
- A sidecar is managed only while its path and checksum match an `installations` row. Any other sidecar is protected as the owner's: it satisfies its language and is never replaced or removed automatically.
- A sidecar that is a symbolic link to a regular file is always protected (`inventory.Track.Symlink`), even when its path and checksum match provenance. Nothing is written or removed through it. A dangling link is not a subtitle; the installer still refuses to publish over it.
- Provenance is evidence, not proof that a file exists. Each workflow refreshes live sidecars. If the managed file was deleted, the record is dropped (`install.record_dropped`) and acquisition runs with first-install semantics.
- An operator takes ownership by editing or replacing the sidecar: its checksum then differs and subsyncd protects it.

### Media changed in place

When the live inventory finds the media file changed under the same path and Arr file ID (different size or mtime, no Arr event), `ReplaceTrackInventory` clears installation provenance.

- A different **size** counts as a replacement: every other language with an installation is made due at once at import priority, unless its search is leased. Attempt counters and the last outcome are kept until that search replaces them.
- An **mtime-only** change re-queues nothing. A remount, metadata repair or mount upgrade can touch the whole library without changing any bytes, and re-searching would stream every file through LAPSE.

Replacements that Arr reports reschedule every language through the import event.

## Temporary files

| Files | Location | Lifetime |
| --- | --- | --- |
| Downloads, extracted subtitles, LAPSE output | A private `.subsyncd-work-*` directory in the system temporary directory (`TMPDIR`, else `/tmp`) | Removed on completion, rejection, error and cancellation. Rejected candidates' artifacts are released as processing advances |
| `analyze-sync` working copy | Its own private temporary directory | The command |
| `.subsyncd-stage-*` | Beside the destination subtitle | Until publication or recovery |
| `.subsyncd-rollback-*` | Beside the destination subtitle | Until the installation is recorded (see [Rollback copies](#rollback-copies)) |
| `.subsyncd-removing-*` | Beside the removed subtitle | Until the removal commits or is undone |
| Pack-cache staging | Inside `pack-cache/` | Until publication into the cache |

- Final staging stays media-local and pack-cache publication stays cache-local, so moving scratch to another filesystem never changes either atomic publication path.
- The Compose example's 512 MiB `/tmp` tmpfs is shared by active workflows; raising `worker.max_concurrent` may need more. Do not point `TMPDIR` into a media library.
- Abrupt termination can bypass deferred cleanup; a container tmpfs is discarded when its mount is recreated.
- Application errors redact the temporary root. LAPSE failures expose bounded verdict, exit-code and protocol categories, never raw stderr, JSON diagnostics or command errors.

## Publication

`Installer.Install` (`internal/workflow/install.go`):

1. Validates the subtitle and stages it beside its destination with `install.file_mode` (and `install.uid`/`install.gid` when set).
2. Fsyncs it and publishes atomically (`publishStaged`). A fresh sidecar is hard-linked, so a file created after the absence check (by Arr or the owner) is never overwritten; on mounts without hard links (FUSE, SMB, mergerfs, rclone) it falls back to a plain rename after that check. A managed replacement is renamed over the old file after a rollback copy is taken.
3. Fsyncs the directory.
4. Commits provenance, the checksum and the notification intents in one transaction (`RecordInstallationWithNotifications`).

Before publishing, the installer checks the context (so an abandoned job cannot install) and the media file. The commit rechecks the stored path, Arr file ID, size and mtime, so a media replacement during LAPSE cannot commit obsolete provenance.

### When the commit fails

- A new sidecar is removed; a replacement's previous managed copy is restored.
- Before rolling back, the installer checks that the destination is still the file this run published, by identity (`os.SameFile` against the staged file), not content. A run abandoned while publishing can fail its database write long after another run published a subtitle, possibly with the same text; that file is kept and only this run's rollback copy is removed (`install.rollback_skipped`, warn).
- A replacement is not rolled back when another run has recorded an installation for that media and language since this run began: this run's rename may have landed after that install.
- When the answer is unknown, the safer side is taken per case:
  - a first install whose destination cannot be inspected is still treated as this run's and removal is attempted, since no other run publishes over a sidecar it has not recorded;
  - a replacement whose destination or installation record cannot be read is not restored, since that could overwrite a newer install. The destination, rollback copy and `pending_installs` row are kept for the startup sweep.
- If removal, restoration or fsync also fails, the combined error is logged and returned. The surviving file may be valid but untracked, so it is protected; inspect it with `explain` before resolving it by hand.

A crash after publication but before search completion does not cause a second installation: the next inventory pass recognizes the checksum-recorded sidecar.

## Pending installs and crash recovery

Before publishing, the installer records a `pending_installs` row (migration 020) with the destination, checksum, staging and rollback paths, and the staged file's inode (`published_inode`, migration 021), which linking and renaming keep. Recording the installation in the same transaction (`Installation.PendingInstallID`), or a complete rollback, clears it. `install.pending_clear_failed` (warn) reports a row that could not be cleared; the next start clears it.

### The startup sweep

Only a mutable start (`serve` and the mutating commands), after it holds the lock and before the worker exists, settles leftover rows (`Installer.RecoverPendingInstalls`). Read-only commands never do, since without the lock a row may be the daemon's install in progress.

For each row:

| Situation | Action | `install.recovered` outcome |
| --- | --- | --- |
| An installation is recorded with that checksum | Keep the subtitle; the rollback sweep deletes the copy | `recorded` |
| The destination is still the published file (same inode and checksum; rows without an inode compare the checksum) | Restore the previous managed subtitle from the rollback copy, or remove the unrecorded new one, so it cannot linger as an untracked sidecar that blocks every future install | `undone` |
| Neither | Remove staging leftovers and unused rollback copies | `cleaned` |
| A sidecar move (see [below](#replaced-and-renamed-media)) | Unlink the old name if it is still a second link of the file at the new name | `move_finished` or `move_cleaned` |
| A removal (`.subsyncd-removing-*`) | See [Reject, replacement and removal](#reject-replacement-and-removal) | |
| The destination cannot be inspected or read | Leave the row for the next start (`install.recovery_failed`, warn), since undoing blind could overwrite a newer install | |

- The device number is not compared, because it can change across a restart (device-mapper, network and overlay mounts) while the destination path already fixes the filesystem.
- The sweep waits at most 30 seconds. One stuck on a hung mount is abandoned with `install.recovery_timed_out` (warn); startup continues and the rows are kept. It settles only the rows listed when it began and checks its context before each filesystem change, so once a blocked call returns it changes nothing more. The one call already in flight may land late, and only applies while the destination is that row's publication.
- `install.recovery_incomplete` (warn) summarizes a start that left rows unsettled.
- The search itself is retried when its lease expires.

## Rollback copies

A managed replacement keeps a `.subsyncd-rollback-*` copy of the previous subtitle only until the installation is recorded.

- A commit failure restores it at once.
- After `RecordInstallationWithNotifications` commits, `Installer.discardRollback` deletes it and clears `installations.rollback_path`. A failed deletion logs `install.rollback_cleanup_failed` (warn) and never fails the install; the copy stays recorded for the startup sweep.
- Nothing restores a copy after the commit; `reject` and `reject --undo` do not use it.
- Right after `RecoverPendingInstalls`, under the same 30-second bound and only in a mutable start, `Installer.DiscardRollbackCopies` deletes every copy still recorded. For each installation with a `rollback_path` and no `pending_installs` row, it deletes the copy only when the sidecar is a regular file at its recorded path inside a media root and the copy is a regular file named `.subsyncd-rollback-*` in that folder. A copy already gone is forgotten; anything else is skipped.
- Sweep events: `install.rollback_sweep_completed` (info, `removed_count`, `skipped_count`, only when there was work), `install.rollback_sweep_failed` (warn), `install.rollback_sweep_timed_out` (warn). Running it again is harmless.
- `forget-instance` reports copies its instance still records as `rollback_copies` (normally 0); they stay on disk without a record.

## Replaced and renamed media

The media row follows the stable Arr entity, so an upgrade, re-import or replacement in place reaches the same row with another file ID or size. Installations record the file they were installed for: `media_path`, `media_file_id`, `media_size` and `media_mod_time_ns`.

- When the content changes (`upsertMediaTx`, `UpsertMedia`, `ReplaceTrackInventory`), `invalidateInstallationTx` clears only the scoring and synchronization evidence, `fallback` and `media_mod_time_ns`. The file identity stays, so the workflow can tell a replaced file from the same file with a new mtime.
- Only a rename of the same file (same file ID and size, new path) renames sidecar records, record-only (`rebaseInstallationPathsTx`), and it leaves `media_path` at the old path until the workflow has checked the sidecar. A replaced file's records keep their old sidecar path.

### Settling the record

`Service.Run` settles the language's installation before the inventory decides anything: `Installer.ReconcileInstallation` (`internal/workflow/installation_reconcile.go`) compares the record with the live media fingerprint and returns a `Reconciliation` (`Changed`, `Removed`, `Deferred` with `RetryAt`).

| Case | Action | Event |
| --- | --- | --- |
| New Arr file ID, known unchanged size, and an unchanged media path or an unchanged sidecar named after the current file (a re-import of identical bytes, an Arr database restore) | `Repository.ReattachInstallation` sets the new file ID and sidecar path; then settled as the same file | `install.reattached` |
| New Arr file ID, known unchanged size and unchanged media path, sidecar gone (Arr deleted the old file's extra files on the re-import) | Drop the record without reattaching it; the search runs as for a missing subtitle | `install.record_dropped` (`sidecar_missing`) |
| Same file renamed, sidecar already at the new name with the recorded checksum (Arr moved it) | `Repository.RebaseInstallation` sets `media_path`; a hard link left at the old name is removed | `install.renamed` (`moved=false`) |
| Same file renamed, sidecar left at the old name | Hard-link it to the new name (never over an existing file; where hard links fail, `renameat2(RENAME_NOREPLACE)` on Linux or `renamex_np(RENAME_EXCL)` on macOS), record it there, unlink the old name | `install.renamed` (`moved=true`) |
| Same file renamed, the move fails with `EXDEV` or the exclusive rename is unsupported | Drop the record, leave the file at the old name, so the search runs instead of failing each pass | `install.record_dropped` (`sidecar_unmovable`) |
| Same file renamed, another file holds the new name | Remove the left-behind managed sidecar through `RemoveManaged` | `install.removed` (`media_renamed`) |
| Same file renamed, sidecar exists nowhere | Drop the record | `install.record_dropped` |
| Same file, same path, sidecar vanished | Drop the record | `install.record_dropped` (`sidecar_missing`) |
| Same file, sidecar changed and not named after the media | Drop the record | `install.record_dropped` (`sidecar_changed`) |
| Same file, sidecar changed and named after the media | Keep it as the owner's edit | |
| Replaced file (another size, or another file ID not reattached), or a record with `media_file_id=0` | Remove the old sidecar (at its recorded path, or renamed after the new file) through `RemoveManaged` when it is a regular file in the media folder with the recorded checksum; otherwise drop only the record | `install.removed` (`media_replaced`) or `install.record_dropped` (`media_replaced`) |

A record of unknown provenance whose unchanged sidecar has the current media file's name stays managed.

### Moves

A sidecar move is covered by a `pending_installs` row (`destination` the new name, `rollback_path` the old one, `staged_path` empty, `published_inode` the old name's inode) from before the link until the old name is unlinked.

- A crash before the commit is finished by the next pass: the record already names the new path.
- A crash or a failed unlink (`install.rename_cleanup_failed`, warn) after the commit is finished by `RecoverPendingInstalls`, which unlinks the old name only while it is a regular file with that inode and checksum and a second name of the file at the new name.
- `removeSameFile` unlinks a name only as a second link of the file at the other name: the names differ byte for byte, the file has at least two links, and both names are listed as spelled in their directory. A case-only rename on a case-sensitive filesystem therefore loses its old spelling like any rename. On a case-insensitive filesystem both spellings name one directory entry, so nothing is unlinked, the rename counts as already moved, and recovery reports `move_cleaned`.

### Removal cap

Deletions of a replaced file's sidecar are capped at `workflow.MaxReplacedRemovalsPerHour` (20) per rolling hour. The budget (`RemovalBudget`) is in memory, created by `app.New`, and starts fresh at each restart.

- Past the cap nothing changes: the sidecar and the invalidated record stay, `install.removal_deferred` (warn, `reason=cap`, `limit_per_hour`, `retry_at`) is logged, and the reconciliation is `Deferred`. `RetryAt` is when the window's oldest deletion leaves it (`RemovalBudget.nextSlot`) plus 1 minute.
- `Run` then searches nothing and returns `OutcomeThrottled` with that `RetryAt` and reason `removal_deferred`. The worker completes it like a provider throttle: queue order kept, attempts not advanced, `replace_installed` kept.
- Installing instead would record the new file's subtitle over the old record (one row per media and language) and leave the old sidecar untracked for good.
- The retry deletes the sidecar first, if it is still there and the record is unchanged, then searches. A reservation is given back when the removal does not happen.

### After settling

- Dropping (`Installer.dropInstallation`) is a guarded delete with a `subtitle_record_dropped` audit event whose outcome is the reason. It touches no sidecar and deletes the record's rollback copy.
- When anything changed, `Run` refreshes the inventory, rereads the installation, and searches for the current file as for a missing subtitle. The import or rename already queued that search at import priority.
- When the sidecar was removed during a reject's replacement run, a search that ends `no_result`, `rejected` or `satisfied` reports `removed`, with that outcome in `RemovedAfter`.

## Reject, replacement and removal

### Recording the rejection

`reject` (`internal/app/reject.go`) resolves the media like `search` and picks the language: `--language`, or the only one with an installation (several is `usage`, none is `not_managed`).

- `prepareReject` changes nothing. The installation must exist, lie inside a media root, and be a regular file whose SHA-256 still equals `installations.checksum`; otherwise `not_managed`. An invalid or missing `--reason` is `usage`.
- `Repository.RecordUserRejection` is one transaction:
  - it upserts a `candidate_rejections` row with reason `user_<reason>`, the installation's `candidate_signature` (`*` when empty) and `artifact_checksum`, the current media fingerprint, and the current tool signature (see [Rejections](providers/rejections.md#user-rejections));
  - it sets the search to pending, due now, import priority, attempts reset, resume progress cleared, `replace_installed=1`;
  - it writes a `rejected` history row;
  - it leases the row to the reject's `manual-<hex>` owner when unleased, so the worker cannot take it first; a row another owner holds gets `rerun_requested=1` instead.
- The manual run then claims the row (`ClaimSearch` accepts its current owner). If the process stops first, the rejection and the queued replacement stay; the manual lease expires after five minutes and the worker runs the search with `SearchLease.ReplaceInstalled`.
- `reject` logs `candidate.rejected` (info, `component=app`).

### The replacement run

With `workflow.Request.ReplaceInstalled`, `Service.Run` treats a managed installation that is still present and unedited as not installed:

- no exact-hash shortcut, no `ShouldUpgrade`, no same-candidate reassessment, and the fallback tier is allowed;
- the best acceptable non-rejected candidate is installed over it through the normal `Installer.Install`;
- a format-changing candidate, and one whose prepared text has the rejected checksum, are skipped for that run.

| Result | Action |
| --- | --- |
| Inventory already satisfied by another track (embedded or unmanaged sidecar) | Remove the rejected sidecar without searching; `RemovedAfter=satisfied` |
| `no_result` or `rejected`, and no provider on the route reported a technical error | `Installer.RemoveManaged`; outcome `removed`, reason `rejected_subtitle_removed` |
| Throttled, any error, or a provider error beside an otherwise empty search | Keep the subtitle and `replace_installed=1` for the retry |
| Sidecar edited, deleted or replaced | Not managed any more; neither replaced nor removed |

The run completes like a worker completion and clears `replace_installed` in the same owner-guarded `CompleteSearch` (ignored while a rerun is requested). Installed or satisfied schedules the upgrade check; removed, `no_result` or `rejected` the missing schedule; throttled keeps the flag.

`reject --undo` calls `ClearUserRejections` for the file and language, clears `replace_installed` in the same transaction, and runs no search. A run already under way finishes; no completion sets the flag again.

### Removing a managed subtitle

`Installer.RemoveManaged` (`internal/workflow/remove.go`) serves `reject` and replaced or renamed media.

1. Check containment (`containedDestination`) and the checksum. A changed file is `ErrManagedSubtitleChanged`, which callers treat as the owner's file: nothing changes.
2. Record a `pending_installs` row whose `rollback_path` is a hidden `.subsyncd-removing-<hex>` name in the subtitle's folder and whose `staged_path` is the old rollback copy.
3. Rename the subtitle to the hidden name and re-check its checksum (an edit in between restores it). Fsync the directory.
4. Commit `Repository.RemoveInstallation`: the guarded delete of the `installations` row (path, checksum, provider, candidate and rollback path must still match), a `subtitle_removed` audit event, a `removed` history row (reason `media_replaced` or `media_renamed` for a media change, empty for a reject), and one notification intent per notifier with a removal-specific dedupe key.
5. A failed commit links the subtitle back (never over a recreated destination) and clears the pending row.
6. After the commit, log `install.removed`, delete the hidden file and any recorded rollback copy, and clear the pending row.

At startup, while the installation row still matches, the removal did not commit and the subtitle is linked back (unless the destination was recreated). Once the row is gone, the hidden file and the old rollback copy are deleted.

Never delete arbitrary `.subsyncd-*` files while the daemon is running.

## Site watermark cues

With `install.strip_watermarks: true` (default false), `Installer.Install` passes an SRT payload through `watermark.Strip` after reading it and before it is validated, compared or written. An advert cue placed past the end of the media therefore does not fail the overrun check, and the validated text is the installed text.

Whole cues are dropped when their text, without `<...>`/`{...}` markup:

- is only a subtitle-site address: `titlovi.com`, `divx-titlovi.com`, `addic7ed.com` or a bare `.addic7ed.`, `podnapisi.net`, `subscene`, `prijevodi-online.org`, with hyphens accepted for dots and an optional `www.`, scheme, `@`, spaced dots or `Preuzeto sa`/`s` prefix;
- names `opensubtitles.org` or `.com` anywhere (its inserted adverts, in any language);
- or has every line a site address or a signature credit (`Synced and corrected by …`, `Subtitles by …`, `provided by …`, optionally after a release tag such as `WEB-DL`, or the Croatian `Transkript …` / `Hvala na transkriptu …`) with at least one address. Dashes are allowed (Addic7ed's signature).

In other cues, lines that are only a site address, and a `Preuzeto sa` line directly above one, are dropped unless that would empty the cue. The remaining cues are renumbered, keeping line endings and a leading byte order mark.

Other formats, payloads with nothing to remove, and a subtitle that would be left empty are untouched. The recorded checksum is of the stripped bytes, so ownership checks still see the file as managed. LAPSE input, candidate artifacts and rejection identities are unchanged.

## Permissions

The image runs as UID/GID `1000:1000` by default and stays rootless: it creates no users and changes no ownership at startup.

- `/config/config.yaml` must be readable; `/data` writable; every media root readable for probing and writable for sidecars; `/tmp` writable.
- `install.file_mode` (default `0644`, an octal non-executable mode) is applied before publication. `install.uid` and `install.gid` chown the sidecar only when set; unprivileged containers leave them unset.
- Compose resolves `PUID` and `PGID` before the container starts, from `.env` or the shell, and applies them to `user:` and the `/tmp` tmpfs.
