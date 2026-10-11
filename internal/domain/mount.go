package domain

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// MountUnavailable reports an error from storage that is down rather than a
// file that is gone: a crashed FUSE mount (ENOTCONN), failing I/O (EIO), a
// stale or unreachable network share (ESTALE, EHOSTDOWN, EHOSTUNREACH,
// ETIMEDOUT), or a device that disappeared (ENODEV, ENXIO). Such errors are
// expected to clear when the mount recovers.
func MountUnavailable(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ENOTCONN, syscall.EIO, syscall.ESTALE, syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.ENODEV, syscall.ENXIO} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// LinkTarget returns the path a symlink points to, made absolute against the
// link's directory, and false when path is not a symlink or cannot be read
// within timeout. It reads the link only and never follows it, so it does not
// touch the storage the link points into; the wait is bounded in case path
// itself sits on a hung mount.
func LinkTarget(path string, timeout time.Duration) (string, bool) {
	done := make(chan string, 1)
	go func() {
		target, err := os.Readlink(path)
		if err != nil {
			target = ""
		}
		done <- target
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case target := <-done:
		if target == "" {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return filepath.Clean(target), true
	case <-timer.C:
		return "", false
	}
}
