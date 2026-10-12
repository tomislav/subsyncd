package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"subsyncd/internal/control"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// ForgetInstance deletes the database records of an Arr instance that has been
// removed from the configuration. Without apply it only reports what it would
// delete, and works on a read-only application. Subtitle files and their hidden
// rollback copies are left on disk and become unmanaged.
func (a *App) ForgetInstance(ctx context.Context, instance string, apply bool) (control.ForgetInstanceResult, error) {
	for _, configured := range a.Config.Instances {
		if configured.Name == instance {
			return control.ForgetInstanceResult{}, &control.Error{Code: control.CodeUsage, Message: fmt.Sprintf("instance %q is still configured; remove it from the configuration first", instance)}
		}
	}
	if apply {
		release, err := a.acquireMutationLock()
		if err != nil {
			return control.ForgetInstanceResult{}, err
		}
		defer release()
	}
	result, err := a.Repository.ForgetInstance(ctx, instance, apply)
	if errors.Is(err, store.ErrUnknownInstance) {
		return control.ForgetInstanceResult{}, &control.Error{Code: control.CodeUnknownInstance, Message: fmt.Sprintf("unknown instance %q: the database has no records for it", instance)}
	}
	if err != nil {
		return control.ForgetInstanceResult{}, err
	}
	forgotten := control.ForgetInstanceResult{Instance: instance, Applied: apply, Media: result.Media, Searches: result.Searches, Installations: result.Installations, RollbackCopies: result.RollbackCopies, Candidates: result.Candidates, Rejections: result.Rejections, Notifications: result.Notifications, Events: result.Events, Replays: result.Replays, History: result.History}
	if !apply {
		return forgotten, nil
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
		slog.Int64("history_count", result.History),
	)
	return forgotten, nil
}
