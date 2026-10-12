package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

// pendingInstallStore records an install from just before its file is
// published until the installation is recorded. Repositories without it (test
// doubles) install without crash recovery.
type pendingInstallStore interface {
	BeginPendingInstall(context.Context, store.PendingInstall) (int64, error)
	FinishPendingInstall(context.Context, int64) error
}

type pendingInstallRecovery interface {
	pendingInstallStore
	ListPendingInstalls(context.Context) ([]store.PendingInstall, error)
}

func (i Installer) beginPendingInstall(ctx context.Context, pending store.PendingInstall) (int64, error) {
	recorder, ok := i.Repository.(pendingInstallStore)
	if !ok {
		return 0, nil
	}
	pending.CreatedAt = i.now()
	id, err := recorder.BeginPendingInstall(ctx, pending)
	if err != nil {
		return 0, fmt.Errorf("record pending subtitle install: %w", err)
	}
	return id, nil
}

// finishPendingInstall clears the record once the install is fully undone
// (recording an install clears it in the same transaction). A failure leaves
// it to the startup sweep, which finds the install already settled and only
// clears the record.
func (i Installer) finishPendingInstall(ctx context.Context, id int64) {
	recorder, ok := i.Repository.(pendingInstallStore)
	if !ok || id == 0 {
		return
	}
	if err := recorder.FinishPendingInstall(context.WithoutCancel(ctx), id); err != nil {
		i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.pending_clear_failed", "could not clear the pending install record", i.Events.ErrorAttrs("pending_install", err)...)
	}
}

func (i Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now().UTC()
}

// RecoverPendingInstalls settles installs a crash interrupted between
// publishing the subtitle and recording it. One that was recorded keeps its
// subtitle; one that was not is undone, restoring the previous managed
// subtitle from its rollback copy or removing the unrecorded one, so it is not
// left as an untracked sidecar that blocks future installs. Staging and unused
// rollback copies are removed. A removal (RemoveManaged) whose record was not
// deleted gets its subtitle back; one whose record was deleted loses the
// set-aside subtitle and its old rollback copy. A sidecar move after a rename
// (moveRenamedSidecar) that left the file under both names loses the old
// one. It returns how many records it settled.
//
// Only the rows present when it starts are settled, and it stops before the
// next filesystem change once ctx ends: startup gives up on a recovery stuck
// on a hung mount and starts the daemon, and the abandoned call must not then
// undo anything behind the daemon's back. Unsettled rows are kept for the
// next start.
func (i Installer) RecoverPendingInstalls(ctx context.Context) (int, error) {
	recovery, ok := i.Repository.(pendingInstallRecovery)
	if !ok {
		return 0, nil
	}
	pending, err := recovery.ListPendingInstalls(ctx)
	if err != nil {
		return 0, err
	}
	settled := 0
	var failures []error
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		outcome, err := i.recoverPendingInstall(ctx, item)
		if err != nil {
			failures = append(failures, err)
			i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.recovery_failed", "could not settle an interrupted subtitle install",
				append([]slog.Attr{slog.Int64("media_id", item.MediaID), slog.String("language", item.Language)}, i.Events.ErrorAttrs("pending_install", err)...)...)
			continue
		}
		if err := recovery.FinishPendingInstall(ctx, item.ID); err != nil {
			failures = append(failures, err)
			continue
		}
		settled++
		i.Events.For("workflow").Log(ctx, slog.LevelInfo, "install.recovered", "interrupted subtitle install settled",
			slog.Int64("media_id", item.MediaID), slog.String("language", item.Language), slog.String("outcome", outcome))
	}
	return settled, errors.Join(failures...)
}

func (i Installer) recoverPendingInstall(ctx context.Context, item store.PendingInstall) (string, error) {
	destination, parent, err := containedDestination(item.Destination, i.MediaRoots)
	if err != nil {
		return "", err
	}
	if isPendingRemoval(item) {
		return i.recoverPendingRemoval(ctx, item, destination, parent)
	}
	if isPendingMove(item) {
		return i.recoverPendingMove(ctx, item, destination)
	}
	installation, found, err := i.Repository.GetInstallation(ctx, item.MediaID, domain.Language(item.Language))
	if err != nil {
		return "", fmt.Errorf("read installation: %w", err)
	}
	recorded := found && filepath.Clean(installation.Path) == destination && installation.Checksum == item.Checksum
	outcome := "cleaned"
	owned := false
	if recorded {
		outcome = "recorded"
	} else if owned, err = pendingPublicationOwned(destination, item); err != nil {
		return "", err
	}
	// The checks above can block on a hung mount; once recovery has been
	// given up on, change nothing.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if owned {
		outcome = "undone"
		if item.RollbackPath != "" && regularFile(item.RollbackPath) {
			if err := os.Rename(item.RollbackPath, destination); err != nil {
				return "", fmt.Errorf("restore previous subtitle: %w", err)
			}
		} else if err := os.Remove(destination); err != nil {
			return "", fmt.Errorf("remove unrecorded subtitle: %w", err)
		}
		if err := syncDirectory(parent); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var cleanup []error
	if item.StagedPath != "" {
		cleanup = append(cleanup, removeContainedFile(item.StagedPath, parent))
	}
	// A recorded install keeps its rollback copy here, as the install would
	// have until it deleted it; DiscardRollbackCopies, which runs next,
	// deletes it.
	if item.RollbackPath != "" && !recorded {
		cleanup = append(cleanup, removeContainedFile(item.RollbackPath, parent))
	}
	if err := errors.Join(cleanup...); err != nil {
		return "", err
	}
	return outcome, nil
}

// pendingPublicationOwned reports whether destination still holds the file a
// pending install published: the same inode and checksum. The inode tells it
// apart from a later publication of the same text; the checksum guards
// against a reused inode number. Rows without an inode compare the checksum
// alone. An unreadable destination is an error, so the row is kept and
// retried at the next start: no run is active then that could settle it, and
// undoing blind could overwrite a newer install.
func pendingPublicationOwned(destination string, item store.PendingInstall) (bool, error) {
	info, err := os.Lstat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect subtitle destination: %w", err)
	}
	if !info.Mode().IsRegular() || (item.PublishedInode != 0 && fileInode(info) != item.PublishedInode) {
		return false, nil
	}
	payload, err := os.ReadFile(destination)
	if err != nil {
		return false, fmt.Errorf("read subtitle destination: %w", err)
	}
	return checksumBytes(payload) == item.Checksum, nil
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
