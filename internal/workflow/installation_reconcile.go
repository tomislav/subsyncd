package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// InstallationReconciler settles an installation record against the media
// file it now belongs to, before a search uses it.
type InstallationReconciler interface {
	ReconcileInstallation(context.Context, RemovalRequest) (Reconciliation, error)
}

// Reconciliation reports what ReconcileInstallation did.
type Reconciliation struct {
	// Changed is set when the record or a sidecar changed: the caller
	// refreshes the inventory and rereads the installation.
	Changed bool
	// Removed is set when the sidecar was deleted (RemoveManaged).
	Removed bool
	// Deferred is set when a replaced media file's sidecar had to be deleted
	// but MaxReplacedRemovalsPerHour deletions already happened in the rolling
	// hour: the sidecar and its record stay, and the search must not install
	// for the new file yet (its record would replace this one and leave the
	// old sidecar untracked). RetryAt is when a deletion slot is free again.
	Deferred bool
	RetryAt  time.Time
}

// installationRecordStore changes installation records without a file
// publication. Repositories without it (test doubles) skip reconciliation.
type installationRecordStore interface {
	DropInstallation(context.Context, store.Installation, string, time.Time) error
	RebaseInstallation(context.Context, store.Installation, string, string) error
	ReattachInstallation(context.Context, store.Installation, string, int64) error
}

// Reasons recorded when a media change settles an installation.
const (
	reconcileMediaReplaced = "media_replaced"
	reconcileMediaRenamed  = "media_renamed"
	reconcileMissing       = "sidecar_missing"
	reconcileChanged       = "sidecar_changed"
	// reconcileUnmovable: Arr renamed the media file and left the sidecar
	// at the old name, and it cannot be moved without risk (another
	// filesystem, or no hard links and no atomic no-replace rename).
	reconcileUnmovable = "sidecar_unmovable"
)

type sidecarState int

const (
	sidecarMatches sidecarState = iota
	sidecarChanged
	sidecarMissing
)

// ReconcileInstallation settles the installation in request against the
// current media file (request.Media, whose fingerprint is the live one) and
// reports what it changed; when anything did, the caller refreshes the
// inventory and rereads the installation. The record names the media file it
// was installed for (media_path, media_file_id, media_size):
//
//   - The same file under a new Arr file ID (a re-import of identical bytes,
//     an Arr database restore): the recorded size is known and unchanged,
//     and either the media path is unchanged or the sidecar with the
//     recorded checksum is named after the current media file. The record
//     takes the new file ID and the sidecar stays; it is then settled as the
//     same file below.
//   - The same file under a new path (an Arr rename): the store already
//     renamed the sidecar record. If the sidecar is there with the recorded
//     checksum, the record now follows the new media path. If Arr left it at
//     the old name, it is moved to the new one (never over another file). If
//     it is nowhere, or cannot be moved safely, the record is dropped and the
//     search runs as for a missing subtitle.
//   - The same file under the same path: a vanished sidecar drops the record,
//     and so does a changed one not named after the media file. A changed one
//     named after it is the owner's edit and stays protected.
//   - Another file (another size, or another Arr file ID under another path:
//     an upgrade, a replacement in place), or a record whose provenance an
//     older build cleared: the old file's sidecar, at its recorded path or
//     renamed after the new media file, is removed through RemoveManaged when
//     it still has the recorded checksum and lies in the media folder, at
//     most MaxReplacedRemovalsPerHour per rolling hour (past that it stays,
//     and the result is Deferred: the search waits for a slot, so it never
//     installs beside an old sidecar it would stop tracking). Otherwise the
//     record is dropped and the file left
//     alone. Only a record of unknown provenance whose unchanged sidecar is
//     named after the current media file is kept, as before: nothing shows it
//     belongs to another file.
//
// Files are only touched when they are regular files (never through a
// symlink) inside a media root and the media's folder, with the recorded
// checksum. Nothing is changed once ctx ends.
func (i Installer) ReconcileInstallation(ctx context.Context, request RemovalRequest) (Reconciliation, error) {
	records, ok := i.Repository.(installationRecordStore)
	if !ok {
		return Reconciliation{}, nil
	}
	reattached, err := i.reattachIdenticalFile(ctx, records, &request)
	if err != nil {
		return Reconciliation{}, err
	}
	installation := request.Installation
	media := request.Media.Fingerprint
	sameFile := installation.MediaFileID > 0 && installation.MediaFileID == request.Media.Ref.FileID && installation.MediaSize == media.Size
	switch {
	case sameFile && installation.MediaPath != "" && filepath.Clean(installation.MediaPath) != filepath.Clean(media.Path):
		result, err := i.followRename(ctx, records, request)
		result.Changed = true
		return result, err
	case sameFile:
		state, err := inspectSidecar(installation.Path, installation.Checksum)
		if err != nil {
			return Reconciliation{Changed: reattached}, err
		}
		switch {
		case state == sidecarMissing:
			return Reconciliation{Changed: true}, i.dropInstallation(ctx, records, installation, reconcileMissing)
		case state == sidecarChanged && !sidecarNamedFor(installation.Path, media.Path):
			return Reconciliation{Changed: true}, i.dropInstallation(ctx, records, installation, reconcileChanged)
		}
		return Reconciliation{Changed: reattached}, nil
	default:
		return i.settleReplaced(ctx, records, request)
	}
}

// reattachIdenticalFile gives the record the current Arr file ID when the
// media file is the one it was installed for under a new ID: the recorded
// size is known and unchanged, and the media path is unchanged or the
// unchanged sidecar is named after the current media file (at the recorded
// path, or renamed there by Arr). It updates request.Installation and
// reports whether it changed the record.
func (i Installer) reattachIdenticalFile(ctx context.Context, records installationRecordStore, request *RemovalRequest) (bool, error) {
	installation := request.Installation
	media := request.Media.Fingerprint
	fileID := request.Media.Ref.FileID
	if installation.MediaFileID <= 0 || installation.MediaFileID == fileID || installation.MediaSize <= 0 || installation.MediaSize != media.Size {
		return false, nil
	}
	path := ""
	if installation.MediaPath != "" && filepath.Clean(installation.MediaPath) == filepath.Clean(media.Path) {
		path = installation.Path
	} else {
		candidates := []string{installation.Path}
		if installation.MediaPath != "" {
			candidates = append(candidates, store.RebaseSidecarPath(installation.Path, installation.MediaPath, media.Path))
		}
		for _, candidate := range candidates {
			if !sidecarNamedFor(candidate, media.Path) || !i.inMediaFolder(candidate, media.Path) {
				continue
			}
			state, err := inspectSidecar(candidate, installation.Checksum)
			if err != nil {
				return false, err
			}
			if state == sidecarMatches {
				path = candidate
				break
			}
		}
	}
	if path == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := records.ReattachInstallation(ctx, installation, path, fileID); err != nil {
		return false, err
	}
	installation.Path, installation.MediaFileID = path, fileID
	request.Installation = installation
	i.Events.For("workflow").Log(ctx, slog.LevelInfo, "install.reattached", "managed subtitle kept: the media file is the same under a new Arr file ID",
		slog.String("provider", observability.SafeText(installation.ProviderID)), slog.String("candidate_id", observability.SafeText(installation.CandidateID)),
		slog.String("checksum_prefix", checksumPrefix(installation.Checksum)))
	return true, nil
}

// settleReplaced removes or forgets the sidecar of a file that is no longer
// the media file.
func (i Installer) settleReplaced(ctx context.Context, records installationRecordStore, request RemovalRequest) (Reconciliation, error) {
	installation := request.Installation
	media := request.Media.Fingerprint
	replaced := installation.MediaFileID > 0
	candidates := []string{installation.Path}
	if replaced && installation.MediaPath != "" {
		if renamed := store.RebaseSidecarPath(installation.Path, installation.MediaPath, media.Path); renamed != installation.Path {
			candidates = append(candidates, renamed)
		}
	}
	reason := reconcileMediaReplaced
	if !replaced {
		reason = reconcileMissing
	}
	for index, path := range candidates {
		state, err := inspectSidecar(path, installation.Checksum)
		if err != nil {
			return Reconciliation{}, err
		}
		if index == 0 && !replaced && state != sidecarMissing {
			reason = reconcileChanged
		}
		if state != sidecarMatches {
			continue
		}
		if index == 0 && !replaced && sidecarNamedFor(path, media.Path) {
			return Reconciliation{}, nil
		}
		if !i.inMediaFolder(path, installation.MediaPath, media.Path) {
			continue
		}
		now := i.now()
		release, ok := i.ReplacedRemovals.reserve(now)
		if !ok {
			// Too many this hour: the sidecar and the invalidated record
			// stay, and the search waits until a slot frees, when its retry
			// deletes the sidecar first.
			retryAt := i.ReplacedRemovals.nextSlot(now).Add(removalRetryMargin)
			i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.removal_deferred", "deleting the subtitle of a replaced media file is deferred",
				slog.String("reason", "cap"), slog.Int("limit_per_hour", MaxReplacedRemovalsPerHour), slog.Time("retry_at", retryAt.UTC()),
				slog.String("provider", observability.SafeText(installation.ProviderID)), slog.String("candidate_id", observability.SafeText(installation.CandidateID)),
				slog.String("checksum_prefix", checksumPrefix(installation.Checksum)))
			return Reconciliation{Deferred: true, RetryAt: retryAt}, nil
		}
		changed := false
		if path != installation.Path {
			// Arr carried the old sidecar over to the new name; the record
			// follows it so the removal below is guarded by it.
			if err := ctx.Err(); err != nil {
				release()
				return Reconciliation{}, err
			}
			if err := records.RebaseInstallation(ctx, installation, path, installation.MediaPath); err != nil {
				release()
				return Reconciliation{}, err
			}
			installation.Path = path
			changed = true
		}
		removal := request
		removal.Installation = installation
		removal.Reason = reconcileMediaReplaced
		err = i.RemoveManaged(ctx, removal)
		if err != nil {
			release()
		}
		if errors.Is(err, ErrManagedSubtitleChanged) {
			// Edited in between: the owner's now, so only the record goes.
			return Reconciliation{Changed: true}, i.dropInstallation(ctx, records, installation, reconcileChanged)
		}
		if err != nil {
			return Reconciliation{Changed: changed}, err
		}
		return Reconciliation{Changed: true, Removed: true}, nil
	}
	return Reconciliation{Changed: true}, i.dropInstallation(ctx, records, installation, reason)
}

// followRename settles an installation of the same media file after Arr
// renamed it. The store already named the sidecar record after the new media
// path (installation.Path); installation.MediaPath is still the old one.
func (i Installer) followRename(ctx context.Context, records installationRecordStore, request RemovalRequest) (Reconciliation, error) {
	installation := request.Installation
	media := request.Media.Fingerprint
	expected := installation.Path
	old := store.RebaseSidecarPath(expected, media.Path, installation.MediaPath)
	state, err := inspectSidecar(expected, installation.Checksum)
	if err != nil {
		return Reconciliation{}, err
	}
	oldState := sidecarMissing
	if old != expected {
		if oldState, err = inspectSidecar(old, installation.Checksum); err != nil {
			return Reconciliation{}, err
		}
		if oldState == sidecarMatches && !i.inMediaFolder(old, installation.MediaPath) {
			oldState = sidecarChanged
		}
	}
	switch {
	case state == sidecarChanged && oldState != sidecarMatches && !sidecarNamedFor(expected, media.Path):
		return Reconciliation{}, i.dropInstallation(ctx, records, installation, reconcileChanged)
	case state == sidecarMatches, state == sidecarChanged && oldState != sidecarMatches:
		// Arr moved the sidecar (an edited one stays the owner's, protected
		// as before). A hard link left at the old name is the same file; on
		// a case-insensitive filesystem a rename that only changed letter
		// case leaves one file under both spellings, which stays.
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if err := records.RebaseInstallation(ctx, installation, expected, media.Path); err != nil {
			return Reconciliation{}, err
		}
		if old != expected && state == sidecarMatches {
			_ = removeSameFile(old, expected)
		}
		i.logRenamed(ctx, installation, false)
		return Reconciliation{}, nil
	case oldState == sidecarMatches && state == sidecarChanged:
		// Arr left the sidecar behind and another file holds the new name:
		// the left-behind one is no longer next to its media and goes.
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if err := records.RebaseInstallation(ctx, installation, old, installation.MediaPath); err != nil {
			return Reconciliation{}, err
		}
		installation.Path = old
		removal := request
		removal.Installation = installation
		removal.Reason = reconcileMediaRenamed
		if err := i.RemoveManaged(ctx, removal); errors.Is(err, ErrManagedSubtitleChanged) {
			return Reconciliation{}, i.dropInstallation(ctx, records, installation, reconcileChanged)
		} else if err != nil {
			return Reconciliation{}, err
		}
		return Reconciliation{Removed: true}, nil
	case oldState == sidecarMatches:
		return Reconciliation{}, i.moveRenamedSidecar(ctx, records, request.Media, installation, old, expected)
	default:
		reason := reconcileMissing
		if oldState == sidecarChanged {
			reason = reconcileChanged
		}
		return Reconciliation{}, i.dropInstallation(ctx, records, installation, reason)
	}
}

// moveRenamedSidecar moves a sidecar Arr left at its old name to the name of
// the renamed media file and records it there. It links, so an existing file
// at the new name is never overwritten, then removes the old name; on a
// filesystem without hard links it renames atomically without replacing
// (renameNoReplace). When neither works (the new name is on another
// filesystem, or the filesystem has no atomic no-replace rename) the record
// is dropped (sidecar_unmovable) and the file left at the old name, so the
// search for the current file runs instead of failing every time.
//
// A pending_installs row names the move (destination the new name,
// rollback_path the old one, no staged path) from before the link until the
// old name is gone. A crash or a failed unlink after the commit leaves the
// same file at both names; RecoverPendingInstalls removes the old name at the
// next start, only while it is still that inode with the recorded checksum. A
// crash before the commit is also finished by the next reconciliation, which
// records the new name. The record queues the owning Arr instance's rescan
// in the same transaction, since Arr knows the sidecar by its old name; Silo
// is not told, as before.
func (i Installer) moveRenamedSidecar(ctx context.Context, records installationRecordStore, media domain.Media, installation store.Installation, old, expected string) error {
	mediaPath := media.Fingerprint.Path
	destination, parent, err := containedDestination(expected, i.MediaRoots)
	if err != nil {
		return err
	}
	if !i.inMediaFolder(destination, mediaPath) {
		return fmt.Errorf("renamed sidecar is outside the media folder")
	}
	source, sourceParent, err := containedDestination(old, i.MediaRoots)
	if err != nil {
		return err
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect renamed sidecar: %w", observability.PathErrorCause(err))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pendingID, err := i.beginPendingInstall(ctx, store.PendingInstall{MediaID: installation.MediaID, Language: installation.Language, Destination: destination, Checksum: installation.Checksum, RollbackPath: source, PublishedInode: fileInode(sourceInfo)})
	if err != nil {
		return err
	}
	link := i.Link
	if link == nil {
		link = os.Link
	}
	rename := i.RenameNoReplace
	if rename == nil {
		rename = renameNoReplace
	}
	linked := true
	if err := link(source, destination); err != nil {
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, syscall.EXDEV) {
			err = rename(source, destination)
			linked = false
		}
		switch {
		case err == nil:
		case errors.Is(err, os.ErrExist):
			i.finishPendingInstall(ctx, pendingID)
			return fmt.Errorf("renamed sidecar destination appeared during the move")
		case errors.Is(err, syscall.EXDEV), errors.Is(err, errRenameNoReplaceUnsupported):
			i.finishPendingInstall(ctx, pendingID)
			return i.dropInstallation(ctx, records, installation, reconcileUnmovable)
		default:
			i.finishPendingInstall(ctx, pendingID)
			return fmt.Errorf("move renamed sidecar: %w", observability.PathErrorCause(err))
		}
	}
	undo := func(cause error) error {
		var undoErr error
		if linked {
			undoErr = removeSameFile(destination, source)
		} else {
			undoErr = restoreRemoved(destination, source, sourceParent)
		}
		if undoErr == nil {
			i.finishPendingInstall(ctx, pendingID)
		}
		// Otherwise the pending row stays for the startup sweep.
		return cause
	}
	if err := managedFileMatches(destination, installation.Checksum); err != nil {
		// Edited between the check and the move: put the name back.
		return undo(err)
	}
	if err := syncDirectory(parent); err != nil {
		return undo(err)
	}
	moved := installation
	moved.Path, moved.MediaPath = destination, mediaPath
	requests, err := changeNotificationRequests("moved", media, moved, arrRescanOnly(i.changeNotifierNames(ctx, media)), i.now())
	if err == nil {
		err = i.rebaseWithNotifications(context.WithoutCancel(ctx), records, installation, destination, mediaPath, requests)
	}
	if err != nil {
		return undo(err)
	}
	if linked {
		if err := removeSameFile(source, destination); err != nil {
			// Recorded at the new name; the pending row stays, and the
			// startup sweep removes the old name.
			i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.rename_cleanup_failed", "the old name of a moved subtitle could not be removed; it is retried at the next start",
				i.Events.ErrorAttrs("rename_cleanup", observability.PathErrorCause(err))...)
			i.logRenamed(ctx, installation, true)
			return nil
		}
	}
	_ = syncDirectory(sourceParent)
	i.finishPendingInstall(ctx, pendingID)
	i.logRenamed(ctx, installation, true)
	return nil
}

// isPendingMove reports whether a pending_installs row records a sidecar
// move after a rename (moveRenamedSidecar): rollback_path is the old name, a
// visible sidecar, and there is no staged file. Installs always stage, and
// removals and installs name hidden files in rollback_path.
func isPendingMove(item store.PendingInstall) bool {
	base := filepath.Base(item.RollbackPath)
	return item.StagedPath == "" && item.RollbackPath != "" && !strings.HasPrefix(base, removalPrefix) && !strings.HasPrefix(base, rollbackPrefix)
}

// recoverPendingMove finishes a sidecar move a crash or a failed unlink left
// with the same file at both names: the old name is removed only while it is
// still the moved inode, with the recorded checksum, and a second name of the
// file at the new name. Anything else at the old name is left alone; the next
// reconciliation settles the record.
func (i Installer) recoverPendingMove(ctx context.Context, item store.PendingInstall, destination string) (string, error) {
	source, sourceParent, err := containedDestination(item.RollbackPath, i.MediaRoots)
	if err != nil {
		return "move_cleaned", nil
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return "move_cleaned", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect old subtitle name: %w", observability.PathErrorCause(err))
	}
	if !info.Mode().IsRegular() || (item.PublishedInode != 0 && fileInode(info) != item.PublishedInode) {
		return "move_cleaned", nil
	}
	if err := managedFileMatches(source, item.Checksum); errors.Is(err, ErrManagedSubtitleChanged) {
		return "move_cleaned", nil
	} else if err != nil {
		return "", err
	}
	destinationInfo, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return "move_cleaned", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect new subtitle name: %w", observability.PathErrorCause(err))
	}
	if !os.SameFile(info, destinationInfo) {
		return "move_cleaned", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := removeSameFile(source, destination); err != nil {
		return "", fmt.Errorf("remove old subtitle name: %w", observability.PathErrorCause(err))
	}
	if err := syncDirectory(sourceParent); err != nil {
		return "", err
	}
	return "move_finished", nil
}

// notifyingRebaser records a sidecar's new path together with notification
// intents. Repositories without it (test doubles) record the path only.
type notifyingRebaser interface {
	RebaseInstallationWithNotifications(context.Context, store.Installation, string, string, []store.NotificationRequest) ([]store.NotificationEnqueueResult, error)
}

func (i Installer) rebaseWithNotifications(ctx context.Context, records installationRecordStore, installation store.Installation, path, mediaPath string, requests []store.NotificationRequest) error {
	rebaser, ok := records.(notifyingRebaser)
	if !ok || len(requests) == 0 {
		return records.RebaseInstallation(ctx, installation, path, mediaPath)
	}
	enqueued, err := rebaser.RebaseInstallationWithNotifications(ctx, installation, path, mediaPath, requests)
	if err != nil {
		return err
	}
	i.logEnqueuedNotifications(ctx, enqueued)
	return nil
}

// dropInstallation deletes a stale record and the rollback copy it owned;
// the sidecar itself, if any, is left alone.
func (i Installer) dropInstallation(ctx context.Context, records installationRecordStore, installation store.Installation, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := records.DropInstallation(ctx, installation, reason, i.now()); err != nil {
		return err
	}
	rollbackDeleted := false
	if installation.RollbackPath != "" && strings.HasPrefix(filepath.Base(installation.RollbackPath), rollbackPrefix) {
		if _, parent, err := containedDestination(installation.RollbackPath, i.MediaRoots); err == nil {
			rollbackDeleted = removeContainedFile(installation.RollbackPath, parent) == nil
		}
	}
	i.Events.For("workflow").Log(ctx, slog.LevelInfo, "install.record_dropped", "stale installation record dropped",
		slog.String("reason", reason), slog.String("provider", observability.SafeText(installation.ProviderID)),
		slog.String("candidate_id", observability.SafeText(installation.CandidateID)), slog.String("checksum_prefix", checksumPrefix(installation.Checksum)),
		slog.Bool("rollback_deleted", rollbackDeleted))
	return nil
}

func (i Installer) logRenamed(ctx context.Context, installation store.Installation, moved bool) {
	i.Events.For("workflow").Log(ctx, slog.LevelInfo, "install.renamed", "managed subtitle follows the renamed media file",
		slog.Bool("moved", moved), slog.String("provider", observability.SafeText(installation.ProviderID)),
		slog.String("candidate_id", observability.SafeText(installation.CandidateID)), slog.String("checksum_prefix", checksumPrefix(installation.Checksum)))
}

// inMediaFolder reports whether path is inside a media root and directly in
// the folder of one of the media paths.
func (i Installer) inMediaFolder(path string, mediaPaths ...string) bool {
	if _, _, err := containedDestination(path, i.MediaRoots); err != nil {
		return false
	}
	directory := filepath.Dir(filepath.Clean(path))
	for _, mediaPath := range mediaPaths {
		if mediaPath != "" && filepath.Dir(filepath.Clean(mediaPath)) == directory {
			return true
		}
	}
	return false
}

// inspectSidecar reports whether path is a regular file with the recorded
// checksum, another file (edited, replaced, a symlink), or absent.
func inspectSidecar(path, checksum string) (sidecarState, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return sidecarMissing, nil
	} else if err != nil {
		return 0, fmt.Errorf("inspect managed subtitle: %w", observability.PathErrorCause(err))
	}
	err := managedFileMatches(path, checksum)
	switch {
	case err == nil:
		return sidecarMatches, nil
	case errors.Is(err, ErrManagedSubtitleChanged):
		return sidecarChanged, nil
	default:
		return 0, err
	}
}

// sidecarNamedFor reports whether path is a sidecar of mediaPath: in its
// folder, named after its stem.
func sidecarNamedFor(path, mediaPath string) bool {
	path, mediaPath = filepath.Clean(path), filepath.Clean(mediaPath)
	if filepath.Dir(path) != filepath.Dir(mediaPath) {
		return false
	}
	stem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	return strings.HasPrefix(filepath.Base(path), stem+".")
}

// removeSameFile removes path when it is a regular file that is the same file
// (a hard link) as other, so no content is lost. It removes nothing when the
// file has a single name or the two names differ only in letter case: on a
// case-insensitive filesystem both spell one name, and removing either would
// delete the only copy.
func removeSameFile(path, other string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	otherInfo, err := os.Lstat(other)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, otherInfo) {
		return fmt.Errorf("not the same file")
	}
	if strings.EqualFold(filepath.Clean(path), filepath.Clean(other)) || linkCount(info) < 2 {
		return nil
	}
	return os.Remove(path)
}

func checksumPrefix(checksum string) string {
	if len(checksum) > 12 {
		checksum = checksum[:12]
	}
	return observability.SafeText(checksum)
}
