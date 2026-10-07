package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"subsyncd/internal/store"
)

// RouteGate reports language routes that cannot acquire subtitles right now.
type RouteGate interface {
	PausedRoutes(context.Context) ([]RoutePause, error)
}

// RoutePause is one paused (language, media kind) route. ResetAt is the
// earliest provider reset; it is zero when every provider is disabled.
type RoutePause struct {
	store.RouteKey
	ResetAt       time.Time
	ProviderCount int
}

// pausedRouteKeys asks the gate which routes are paused, logs pause and resume
// transitions, and returns the keys to exclude from the next search lease. A
// nil gate pauses nothing.
func (w *Worker) pausedRouteKeys(ctx context.Context) ([]store.RouteKey, error) {
	if w.Routes == nil {
		return nil, nil
	}
	events := w.Events.For("worker")
	pauses, err := w.Routes.PausedRoutes(ctx)
	if err != nil {
		// Cancellation during shutdown is not a failed check.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		events.Log(ctx, slog.LevelError, "queue.route_check_failed", "route availability check failed; no searches leased",
			events.ErrorAttrs("route_check", err)...)
		return nil, err
	}

	w.routeMu.Lock()
	defer w.routeMu.Unlock()
	current := make(map[store.RouteKey]RoutePause, len(pauses))
	keys := make([]store.RouteKey, 0, len(pauses))
	for _, pause := range pauses {
		current[pause.RouteKey] = pause
		keys = append(keys, pause.RouteKey)
		// Log a new pause, and again when its reset time or reason changes.
		if previous, known := w.pausedRoutes[pause.RouteKey]; known && previous.ResetAt.Equal(pause.ResetAt) {
			continue
		}
		attrs := []slog.Attr{
			slog.String("language", pause.Language.String()),
			slog.String("media_kind", string(pause.Kind)),
			slog.Int("provider_count", pause.ProviderCount),
		}
		if pause.RangesOnly {
			attrs = append(attrs, slog.Bool("episode_ranges_only", true))
		}
		if pause.ResetAt.IsZero() {
			attrs = append(attrs, slog.String("reason", "disabled"))
		} else {
			attrs = append(attrs, slog.Time("reset_at", pause.ResetAt.UTC()))
		}
		events.Log(ctx, slog.LevelWarn, "queue.route_paused", "subtitle searches paused: every provider for this route is unavailable", attrs...)
	}
	for key := range w.pausedRoutes {
		if _, still := current[key]; !still {
			attrs := []slog.Attr{slog.String("language", key.Language.String()), slog.String("media_kind", string(key.Kind))}
			if key.RangesOnly {
				attrs = append(attrs, slog.Bool("episode_ranges_only", true))
			}
			events.Log(ctx, slog.LevelInfo, "queue.route_resumed", "subtitle searches resumed", attrs...)
		}
	}
	w.pausedRoutes = current
	return keys, nil
}
