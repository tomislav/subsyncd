package domain

import (
	"errors"
	"syscall"
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
