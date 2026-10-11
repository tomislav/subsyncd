package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// ForgetInstance deletes the database records of an Arr instance that has been
// removed from the configuration. Without apply it only reports what it would
// delete, and works on a read-only application. Subtitle files and their hidden
// rollback copies are left on disk and become unmanaged.
func (a *App) ForgetInstance(ctx context.Context, instance string, apply bool) (string, error) {
	for _, configured := range a.Config.Instances {
		if configured.Name == instance {
			return "", fmt.Errorf("instance %q is still configured; remove it from the configuration first", instance)
		}
	}
	if apply {
		release, err := a.acquireMutationLock()
		if err != nil {
			return "", err
		}
		defer release()
	}
	result, err := a.Repository.ForgetInstance(ctx, instance, apply)
	if errors.Is(err, store.ErrUnknownInstance) {
		return "", fmt.Errorf("unknown instance %q: the database has no records for it", instance)
	}
	if err != nil {
		return "", err
	}
	counts := fmt.Sprintf("media=%d searches=%d installations=%d rollback_copies=%d candidates=%d rejections=%d notifications=%d events=%d replays=%d",
		result.Media, result.Searches, result.Installations, result.RollbackCopies, result.Candidates, result.Rejections, result.Notifications, result.Events, result.Replays)
	leftOnDisk := "subtitle files installed for this instance stay on disk and will no longer be managed"
	if result.RollbackCopies > 0 {
		leftOnDisk += fmt.Sprintf(", as do %d hidden .subsyncd-rollback-* copies beside them, which you can delete by hand", result.RollbackCopies)
	}
	if !apply {
		return fmt.Sprintf("forget-instance preview: instance=%s %s\n%s\nnothing was deleted; rerun with --yes to delete these records", instance, counts, leftOnDisk), nil
	}
	a.Events.For("app").Log(ctx, slog.LevelInfo, "instance.forgotten", "removed instance records",
		slog.String("instance", observability.SafeText(instance)),
		slog.Int64("media_count", result.Media),
		slog.Int64("search_count", result.Searches),
		slog.Int64("installation_count", result.Installations),
		slog.Int64("rollback_copy_count", result.RollbackCopies),
		slog.Int64("candidate_count", result.Candidates),
		slog.Int64("rejection_count", result.Rejections),
		slog.Int64("notification_count", result.Notifications),
		slog.Int64("event_count", result.Events),
		slog.Int64("replay_count", result.Replays),
	)
	return fmt.Sprintf("forget-instance complete: instance=%s %s\n%s", instance, counts, leftOnDisk), nil
}
