package workflow

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

// MediaUnavailableError marks a search that failed because its media file
// could not be found or read: a dropped (ENOENT), crashed (ENOTCONN, EIO) or
// stale (ESTALE) mount, or a file Arr removed. Only failures reading the
// media file itself carry it; a missing binary, cache or scratch file does
// not. The worker keeps such a search's queue position and pauses when
// several fail in a row.
type MediaUnavailableError struct{ Err error }

func (e *MediaUnavailableError) Error() string { return e.Err.Error() }
func (e *MediaUnavailableError) Unwrap() error { return e.Err }

// MediaUnavailable reports whether err is or wraps a MediaUnavailableError.
func MediaUnavailable(err error) bool {
	var unavailable *MediaUnavailableError
	return errors.As(err, &unavailable)
}

func mediaAccessErrno(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE)
}

// markIfMediaUnreadable wraps err as a MediaUnavailableError when the media
// file itself cannot be read now, whatever err says.
func markIfMediaUnreadable(err error, media domain.Media) error {
	if err == nil || MediaUnavailable(err) {
		return err
	}
	if _, statErr := os.Stat(media.Fingerprint.Path); mediaAccessErrno(statErr) {
		return &MediaUnavailableError{Err: err}
	}
	return err
}

// Check at acquisition boundaries: inventory may have succeeded before an Arr
// rename/removal. Media failures end this search, never reject a candidate.
func checkMediaAvailable(media domain.Media) error {
	info, err := os.Stat(media.Fingerprint.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &MediaUnavailableError{Err: errors.New("media file is missing; retry after catalog or filesystem recovery")}
	}
	if err != nil {
		// Keep only the errno, never the path.
		wrapped := fmt.Errorf("media file cannot be inspected: %w", observability.PathErrorCause(err))
		if mediaAccessErrno(err) {
			return &MediaUnavailableError{Err: wrapped}
		}
		return wrapped
	}
	if !info.Mode().IsRegular() {
		return errors.New("media path is not a regular file")
	}
	return nil
}
