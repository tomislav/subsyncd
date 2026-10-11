package domain

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

func TestMountUnavailable(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTCONN, syscall.EIO, syscall.ESTALE, syscall.EHOSTDOWN} {
		if !MountUnavailable(fmt.Errorf("wrapped: %w", &fs.PathError{Op: "stat", Path: "/media", Err: errno})) {
			t.Errorf("%v is not a mount failure", errno)
		}
	}
	for _, err := range []error{os.ErrNotExist, syscall.EACCES, nil} {
		if MountUnavailable(err) {
			t.Errorf("%v is a mount failure", err)
		}
	}
}
