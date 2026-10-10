package worker

import (
	"context"
	"log/slog"
	"time"

	"subsyncd/internal/workflow"
)

const (
	// mediaBreakerThreshold consecutive searches failing on unreadable media
	// look like a storage outage (such as a remounting FUSE mount) rather than
	// files Arr removed, so leasing pauses instead of failing the whole queue.
	mediaBreakerThreshold = 3
	// mediaBreakerPause is the first pause. It doubles each time the breaker
	// trips again before any search has read its media, up to
	// mediaBreakerMaxPause, so a long outage costs a few searches' backoff
	// rather than three every minute.
	mediaBreakerPause    = time.Minute
	mediaBreakerMaxPause = 30 * time.Minute
)

// recordSearchResult feeds the media breaker: a search that failed on
// unreadable media counts towards a pause, anything else resets both the
// count and the pause length.
func (w *Worker) recordSearchResult(ctx context.Context, err error) {
	w.breakerMu.Lock()
	defer w.breakerMu.Unlock()
	if !workflow.MediaUnavailable(err) {
		w.mediaFailures = 0
		w.mediaPause = 0
		return
	}
	w.mediaFailures++
	if w.mediaFailures < mediaBreakerThreshold {
		return
	}
	w.mediaFailures = 0
	w.mediaPause = min(max(2*w.mediaPause, mediaBreakerPause), mediaBreakerMaxPause)
	w.mediaPausedUntil = w.Clock.Now().Add(w.mediaPause)
	events := w.Events.For("worker")
	events.Log(ctx, slog.LevelWarn, "queue.media_paused", "several searches in a row could not read their media; subtitle searches paused",
		slog.Int("failure_count", mediaBreakerThreshold),
		slog.Int64("pause_ms", w.mediaPause.Milliseconds()),
		slog.Time("resume_at", w.mediaPausedUntil.UTC()))
}

// mediaPaused reports whether leasing waits after a run of media failures.
func (w *Worker) mediaPaused() bool {
	w.breakerMu.Lock()
	defer w.breakerMu.Unlock()
	return w.Clock.Now().Before(w.mediaPausedUntil)
}
