package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// mediaMountProbeTimeout bounds the mount check; a hung mount fails it.
	mediaMountProbeTimeout = 10 * time.Second
)

// recordSearchResult feeds the media breaker: a search that failed on
// unreadable media counts towards a pause, anything else resets both the
// count and the pause length. A missing file (not-found class) whose mount
// still lists its files is one dead file, one Arr removed, and counts neither
// way. Timeouts, read errors and mount errnos always count: altmount keeps
// listing directories from its metadata while reads hang or fail.
func (w *Worker) recordSearchResult(ctx context.Context, err error, mediaPath string) {
	unavailable := workflow.MediaUnavailable(err)
	if unavailable && workflow.MediaMissing(err) && w.mediaMountReadable(mediaPath) {
		w.Events.For("worker").Log(ctx, slog.LevelInfo, "queue.media_file_unavailable", "media file is missing but its mount is readable; queue not paused")
		return
	}
	w.breakerMu.Lock()
	defer w.breakerMu.Unlock()
	if !unavailable {
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

// mediaMountReadable probes the storage holding mediaPath: its directory, or,
// when that directory is gone, the configured media root above it. Only a
// directory that lists at least one entry counts; a dropped mount leaves an
// empty mount point, a crashed or stale one fails the listing, and a hung one
// times out. Without configured media roots it reports false, so every
// failure counts as before.
func (w *Worker) mediaMountReadable(mediaPath string) bool {
	if len(w.MediaRoots) == 0 || mediaPath == "" {
		return false
	}
	root := containingRoot(mediaPath, w.MediaRoots)
	if root == "" {
		return false
	}
	readable, missing := listsEntries(filepath.Dir(mediaPath))
	if readable {
		return true
	}
	if !missing || filepath.Clean(filepath.Dir(mediaPath)) == root {
		return false
	}
	readable, _ = listsEntries(root)
	return readable
}

// containingRoot returns the longest configured root that holds path.
func containingRoot(path string, roots []string) string {
	best := ""
	for _, root := range roots {
		root = filepath.Clean(root)
		relative, err := filepath.Rel(root, filepath.Clean(path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(best) {
			best = root
		}
	}
	return best
}

// listsEntries reports whether directory lists at least one entry within
// mediaMountProbeTimeout, and whether it failed because it does not exist.
func listsEntries(directory string) (readable, missing bool) {
	type outcome struct{ readable, missing bool }
	done := make(chan outcome, 1)
	go func() {
		file, err := os.Open(directory)
		if err != nil {
			done <- outcome{missing: errors.Is(err, os.ErrNotExist)}
			return
		}
		defer file.Close()
		names, err := file.Readdirnames(1)
		done <- outcome{readable: err == nil && len(names) > 0}
	}()
	timer := time.NewTimer(mediaMountProbeTimeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.readable, result.missing
	case <-timer.C:
		return false, false
	}
}
