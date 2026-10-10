package workflow

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

// MediaUnavailable reports whether a search failed because its media file
// could not be found or read: a dropped (ENOENT), crashed (ENOTCONN, EIO) or
// stale (ESTALE) mount, or a file Arr removed. The worker keeps such a search's
// queue position and pauses briefly when several fail in a row.
func MediaUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE)
}

// Check at acquisition boundaries: inventory may have succeeded before an Arr
// rename/removal. Media failures end this search, never reject a candidate.
func checkMediaAvailable(media domain.Media) error {
	info, err := os.Stat(media.Fingerprint.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("media file is missing; retry after catalog or filesystem recovery: %w", os.ErrNotExist)
	}
	if err != nil {
		// Keep only the errno, never the path, so MediaUnavailable can see it.
		return fmt.Errorf("media file cannot be inspected: %w", observability.PathErrorCause(err))
	}
	if !info.Mode().IsRegular() {
		return errors.New("media path is not a regular file")
	}
	return nil
}
