package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"subsyncd/internal/store"
)

// rollbackStore forgets rollback copies once they are deleted. Repositories
// without it (test doubles) keep the copy recorded.
type rollbackStore interface {
	ClearInstallationRollback(context.Context, int64, string, string) error
}

type rollbackSweepStore interface {
	rollbackStore
	ListRollbackCopies(context.Context) ([]store.RollbackCopy, error)
	ListPendingInstalls(context.Context) ([]store.PendingInstall, error)
}

// discardRollback deletes the rollback copy of the subtitle an install just
// replaced, once the install is recorded: nothing restores it after that, so
// it is not kept. It returns the rollback path the record still names: empty
// when the copy is gone and forgotten. A failure is logged and left to the
// startup sweep (DiscardRollbackCopies); it never fails the install.
func (i Installer) discardRollback(ctx context.Context, installation store.Installation, parent string) string {
	path := installation.RollbackPath
	if path == "" {
		return ""
	}
	recorder, ok := i.Repository.(rollbackStore)
	if !ok {
		return path
	}
	err := i.inject(StageCleanup)
	if err == nil {
		err = removeRollbackCopy(path, parent)
	}
	if err == nil {
		// The install is recorded: forgetting its copy is not part of an
		// abandoned job's work and must not be skipped for it.
		err = recorder.ClearInstallationRollback(context.WithoutCancel(ctx), installation.MediaID, installation.Language, path)
	}
	if err != nil {
		i.Events.For("workflow").Log(ctx, slog.LevelWarn, "install.rollback_cleanup_failed", "could not delete the rollback copy of the replaced subtitle; retried at the next start",
			i.Events.ErrorAttrs("rollback_cleanup", err)...)
		return path
	}
	return ""
}

// RollbackSweep counts the rollback copies DiscardRollbackCopies deleted
// (Removed, including records whose copy was already gone) and left alone
// (Skipped).
type RollbackSweep struct {
	Removed int
	Skipped int
}

// DiscardRollbackCopies deletes the rollback copies older builds kept beside
// installed subtitles until the next replacement, and forgets them. It runs
// at startup after RecoverPendingInstalls. A copy is deleted only when it is
// a regular file named .subsyncd-rollback-* directly in the folder of its
// installation's sidecar, inside a media root, and that sidecar is installed
// (a regular file at its recorded path); an installation with an unsettled
// pending install is left for its recovery. A copy that is already gone is
// only forgotten. Anything else is skipped and counted.
//
// It is idempotent, and it stops before the next filesystem or database
// change once ctx ends: startup gives up on a sweep stuck on a hung mount,
// and the abandoned call must not change anything after that.
func (i Installer) DiscardRollbackCopies(ctx context.Context) (RollbackSweep, error) {
	var sweep RollbackSweep
	repository, ok := i.Repository.(rollbackSweepStore)
	if !ok {
		return sweep, nil
	}
	copies, err := repository.ListRollbackCopies(ctx)
	if err != nil {
		return sweep, err
	}
	if len(copies) == 0 {
		return sweep, nil
	}
	pending, err := repository.ListPendingInstalls(ctx)
	if err != nil {
		return sweep, err
	}
	unsettled := make(map[string]bool, len(pending))
	for _, item := range pending {
		unsettled[pendingKey(item.MediaID, item.Language)] = true
	}
	for _, item := range copies {
		if err := ctx.Err(); err != nil {
			return sweep, err
		}
		if unsettled[pendingKey(item.MediaID, item.Language)] {
			sweep.Skipped++
			continue
		}
		parent, ok := i.rollbackCopyParent(item)
		if !ok {
			sweep.Skipped++
			continue
		}
		// The checks above can block on a hung mount.
		if err := ctx.Err(); err != nil {
			return sweep, err
		}
		if err := removeRollbackCopy(item.RollbackPath, parent); err != nil {
			sweep.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return sweep, err
		}
		if err := repository.ClearInstallationRollback(ctx, item.MediaID, item.Language, item.RollbackPath); err != nil {
			return sweep, err
		}
		sweep.Removed++
	}
	return sweep, nil
}

// rollbackCopyParent validates a recorded rollback copy and returns the
// folder it must be deleted from.
func (i Installer) rollbackCopyParent(item store.RollbackCopy) (string, bool) {
	sidecar, parent, err := containedDestination(item.Path, i.MediaRoots)
	if err != nil {
		return "", false
	}
	if info, err := os.Lstat(sidecar); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	rollback, err := filepath.Abs(item.RollbackPath)
	if err != nil || filepath.Dir(rollback) != parent || !strings.HasPrefix(filepath.Base(rollback), rollbackPrefix) {
		return "", false
	}
	return parent, true
}

// removeRollbackCopy deletes a rollback copy: a regular file (never a
// symlink) named .subsyncd-rollback-* directly in parent. One already gone
// counts as deleted.
func removeRollbackCopy(path, parent string) error {
	if !strings.HasPrefix(filepath.Base(path), rollbackPrefix) {
		return fmt.Errorf("rollback copy has an unexpected name")
	}
	if err := removeContainedFile(path, parent); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func pendingKey(mediaID int64, language string) string {
	return fmt.Sprintf("%d\x00%s", mediaID, language)
}
