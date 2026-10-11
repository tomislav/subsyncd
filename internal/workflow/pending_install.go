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

// finishPendingInstall clears the record once the install is recorded or
// fully undone. A failure leaves it to the startup sweep, which finds the
// install already settled and only clears the record.
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
// rollback copies are removed. It returns how many records it settled.
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
	installation, found, err := i.Repository.GetInstallation(ctx, item.MediaID, domain.Language(item.Language))
	if err != nil {
		return "", fmt.Errorf("read installation: %w", err)
	}
	recorded := found && filepath.Clean(installation.Path) == destination && installation.Checksum == item.Checksum
	outcome := "cleaned"
	switch {
	case recorded:
		outcome = "recorded"
	case destinationHolds(destination, item.Checksum):
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
	var cleanup []error
	if item.StagedPath != "" {
		cleanup = append(cleanup, removeContainedFile(item.StagedPath, parent))
	}
	// A recorded install owns its rollback copy until the next replacement.
	if item.RollbackPath != "" && !recorded {
		cleanup = append(cleanup, removeContainedFile(item.RollbackPath, parent))
	}
	if err := errors.Join(cleanup...); err != nil {
		return "", err
	}
	return outcome, nil
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
