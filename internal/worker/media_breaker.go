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
	// mediaBreakerPause is how long leasing then waits before trying again.
	mediaBreakerPause = time.Minute
)

// recordSearchResult feeds the media breaker: a search that failed on
// unreadable media counts towards a pause, anything else resets the count.
func (w *Worker) recordSearchResult(ctx context.Context, err error) {
	w.breakerMu.Lock()
	defer w.breakerMu.Unlock()
	if !workflow.MediaUnavailable(err) {
		w.mediaFailures = 0
		return
	}
	w.mediaFailures++
	if w.mediaFailures < mediaBreakerThreshold {
		return
	}
	w.mediaFailures = 0
	w.mediaPausedUntil = w.Clock.Now().Add(mediaBreakerPause)
	events := w.Events.For("worker")
	events.Log(ctx, slog.LevelWarn, "queue.media_paused", "several searches in a row could not read their media; subtitle searches paused",
		slog.Int("failure_count", mediaBreakerThreshold),
		slog.Time("resume_at", w.mediaPausedUntil.UTC()))
}

// mediaPaused reports whether leasing waits after a run of media failures.
func (w *Worker) mediaPaused() bool {
	w.breakerMu.Lock()
	defer w.breakerMu.Unlock()
	return w.Clock.Now().Before(w.mediaPausedUntil)
}
