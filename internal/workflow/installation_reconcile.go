package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// InstallationReconciler settles an installation record against the media
// file it now belongs to, before a search uses it.
type InstallationReconciler interface {
	ReconcileInstallation(context.Context, RemovalRequest) (bool, error)
}

// installationRecordStore changes installation records without a file
// publication. Repositories without it (test doubles) skip reconciliation.
type installationRecordStore interface {
	DropInstallation(context.Context, store.Installation, string, time.Time) error
	RebaseInstallation(context.Context, store.Installation, string, string) error
}

// Reasons recorded when a media change settles an installation.
const (
	reconcileMediaReplaced = "media_replaced"
	reconcileMediaRenamed  = "media_renamed"
	reconcileMissing       = "sidecar_missing"
	reconcileChanged       = "sidecar_changed"
)

type sidecarState int

const (
	sidecarMatches sidecarState = iota
	sidecarChanged
	sidecarMissing
)

// ReconcileInstallation settles the installation in request against the
// current media file (request.Media, whose fingerprint is the live one) and
// reports whether it changed anything, in which case the caller refreshes the
// inventory and rereads the installation. The record names the media file it
// was installed for (media_path, media_file_id, media_size):
//
//   - The same file under a new path (an Arr rename): the store already
//     renamed the sidecar record. If the sidecar is there with the recorded
//     checksum, the record now follows the new media path. If Arr left it at
//     the old name, it is moved to the new one (never over another file). If
//     it is nowhere, the record is dropped and the search runs as for a
//     missing subtitle.
//   - The same file under the same path: a vanished sidecar drops the record,
//     and so does a changed one not named after the media file. A changed one
//     named after it is the owner's edit and stays protected.
//   - Another file (another Arr file ID or size: an upgrade, a re-import, a
//     replacement in place), or a record whose provenance an older build
//     cleared: the old file's sidecar, at its recorded path or renamed after
//     the new media file, is removed through RemoveManaged when it still has
//     the recorded checksum and lies in the media folder. Otherwise the record
//     is dropped and the file left alone. Only a record of unknown provenance
//     whose unchanged sidecar is named after the current media file is kept,
//     as before: nothing shows it belongs to another file.
//
// Files are only touched when they are regular files (never through a
// symlink) inside a media root and the media's folder, with the recorded
// checksum. Nothing is changed once ctx ends.
func (i Installer) ReconcileInstallation(ctx context.Context, request RemovalRequest) (bool, error) {
	records, ok := i.Repository.(installationRecordStore)
	if !ok {
		return false, nil
	}
	installation := request.Installation
	media := request.Media.Fingerprint
	sameFile := installation.MediaFileID > 0 && installation.MediaFileID == request.Media.Ref.FileID && installation.MediaSize == media.Size
	switch {
	case sameFile && installation.MediaPath != "" && filepath.Clean(installation.MediaPath) != filepath.Clean(media.Path):
		return true, i.followRename(ctx, records, request)
	case sameFile:
		state, err := inspectSidecar(installation.Path, installation.Checksum)
		if err != nil {
			return false, err
		}
		switch {
		case state == sidecarMissing:
			return true, i.dropInstallation(ctx, records, installation, reconcileMissing)
		case state == sidecarChanged && !sidecarNamedFor(installation.Path, media.Path):
			return true, i.dropInstallation(ctx, records, installation, reconcileChanged)
		}
		return false, nil
	default:
		return i.settleReplaced(ctx, records, request)
	}
}

// settleReplaced removes or forgets the sidecar of a file that is no longer
// the media file.
func (i Installer) settleReplaced(ctx context.Context, records installationRecordStore, request RemovalRequest) (bool, error) {
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
			return false, err
		}
		if index == 0 && !replaced && state != sidecarMissing {
			reason = reconcileChanged
		}
		if state != sidecarMatches {
			continue
		}
		if index == 0 && !replaced && sidecarNamedFor(path, media.Path) {
			return false, nil
		}
		if !i.inMediaFolder(path, installation.MediaPath, media.Path) {
			continue
		}
		if path != installation.Path {
			// Arr carried the old sidecar over to the new name; the record
			// follows it so the removal below is guarded by it.
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := records.RebaseInstallation(ctx, installation, path, installation.MediaPath); err != nil {
				return false, err
			}
			installation.Path = path
		}
		removal := request
		removal.Installation = installation
		removal.Reason = reconcileMediaReplaced
		err = i.RemoveManaged(ctx, removal)
		if errors.Is(err, ErrManagedSubtitleChanged) {
			// Edited in between: the owner's now, so only the record goes.
			return true, i.dropInstallation(ctx, records, installation, reconcileChanged)
		}
		return true, err
	}
	return true, i.dropInstallation(ctx, records, installation, reason)
}

// followRename settles an installation of the same media file after Arr
// renamed it. The store already named the sidecar record after the new media
// path (installation.Path); installation.MediaPath is still the old one.
func (i Installer) followRename(ctx context.Context, records installationRecordStore, request RemovalRequest) error {
	installation := request.Installation
	media := request.Media.Fingerprint
	expected := installation.Path
	old := store.RebaseSidecarPath(expected, media.Path, installation.MediaPath)
	state, err := inspectSidecar(expected, installation.Checksum)
	if err != nil {
		return err
	}
	oldState := sidecarMissing
	if old != expected {
		if oldState, err = inspectSidecar(old, installation.Checksum); err != nil {
			return err
		}
		if oldState == sidecarMatches && !i.inMediaFolder(old, installation.MediaPath) {
			oldState = sidecarChanged
		}
	}
	switch {
	case state == sidecarChanged && oldState != sidecarMatches && !sidecarNamedFor(expected, media.Path):
		return i.dropInstallation(ctx, records, installation, reconcileChanged)
	case state == sidecarMatches, state == sidecarChanged && oldState != sidecarMatches:
		// Arr moved the sidecar (an edited one stays the owner's, protected
		// as before). A hard link left at the old name is the same file.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := records.RebaseInstallation(ctx, installation, expected, media.Path); err != nil {
			return err
		}
		if old != expected && state == sidecarMatches {
			_ = removeSameFile(old, expected)
		}
		i.logRenamed(ctx, installation, false)
		return nil
	case oldState == sidecarMatches && state == sidecarChanged:
		// Arr left the sidecar behind and another file holds the new name:
		// the left-behind one is no longer next to its media and goes.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := records.RebaseInstallation(ctx, installation, old, installation.MediaPath); err != nil {
			return err
		}
		installation.Path = old
		removal := request
		removal.Installation = installation
		removal.Reason = reconcileMediaRenamed
		if err := i.RemoveManaged(ctx, removal); errors.Is(err, ErrManagedSubtitleChanged) {
			return i.dropInstallation(ctx, records, installation, reconcileChanged)
		} else if err != nil {
			return err
		}
		return nil
	case oldState == sidecarMatches:
		return i.moveRenamedSidecar(ctx, records, request.Media, installation, old, expected)
	default:
		reason := reconcileMissing
		if oldState == sidecarChanged {
			reason = reconcileChanged
		}
		return i.dropInstallation(ctx, records, installation, reason)
	}
}

// moveRenamedSidecar moves a sidecar Arr left at its old name to the name of
// the renamed media file and records it there. It links, so an existing file
// at the new name is never overwritten, then removes the old name; on a
// filesystem without hard links it renames after checking the new name is
// free. A crash after the link leaves the same file at both names, which the
// next reconciliation records at the new name, removing the old link. The
// record queues the owning Arr instance's rescan in the same transaction,
// since Arr knows the sidecar by its old name; Silo is not told, as before.
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
	if err := ctx.Err(); err != nil {
		return err
	}
	linked := true
	if err := os.Link(source, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("renamed sidecar destination appeared during the move")
		}
		if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("renamed sidecar destination appeared during the move")
		}
		if err := os.Rename(source, destination); err != nil {
			return fmt.Errorf("move renamed sidecar: %w", observability.PathErrorCause(err))
		}
		linked = false
	}
	if err := managedFileMatches(destination, installation.Checksum); err != nil {
		// Edited between the check and the move: put the name back.
		if linked {
			_ = removeSameFile(destination, source)
		} else {
			_ = restoreRemoved(destination, source, sourceParent)
		}
		return err
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	moved := installation
	moved.Path, moved.MediaPath = destination, mediaPath
	requests, err := changeNotificationRequests("moved", media, moved, arrRescanOnly(i.changeNotifierNames(ctx, media)), i.now())
	if err == nil {
		err = i.rebaseWithNotifications(context.WithoutCancel(ctx), records, installation, destination, mediaPath, requests)
	}
	if err != nil {
		if linked {
			_ = removeSameFile(destination, source)
		} else {
			_ = restoreRemoved(destination, source, sourceParent)
		}
		return err
	}
	if linked {
		if err := removeSameFile(source, destination); err != nil {
			// Recorded at the new name; the next reconciliation removes it.
			i.logRenamed(ctx, installation, true)
			return nil
		}
	}
	_ = syncDirectory(sourceParent)
	i.logRenamed(ctx, installation, true)
	return nil
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
// (a hard link) as other, so no content is lost.
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
	return os.Remove(path)
}

func checksumPrefix(checksum string) string {
	if len(checksum) > 12 {
		checksum = checksum[:12]
	}
	return observability.SafeText(checksum)
}
