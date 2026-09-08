package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Cleanup is best-effort maintenance, never a subtitle acquisition failure.
func (a *App) pruneLapseCache(ctx context.Context) {
	if a.readOnly || a.Lapse == nil || ctx.Err() != nil {
		return
	}
	count, err := a.Lapse.PruneCache(ctx, a.Config.LapseCache.TTL, a.Clock.Now())
	if a.Events == nil || errors.Is(err, context.Canceled) {
		return
	}
	events := a.Events.For("app")
	if err != nil {
		events.Log(ctx, slog.LevelWarn, "lapse.cache_cleanup_failed", "LAPSE cache cleanup failed", slog.Int("removed_count", count))
	} else if count > 0 {
		events.Log(ctx, slog.LevelDebug, "lapse.cache_cleaned", "expired LAPSE cache entries removed", slog.Int("removed_count", count))
	}
}

func (a *App) maintainLapseCache(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			a.pruneLapseCache(ctx)
		}
	}
}
