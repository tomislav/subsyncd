package workflow

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames source to destination unless destination exists
// (os.ErrExist), atomically: renameat2 with RENAME_NOREPLACE. A filesystem
// without that flag answers errRenameNoReplaceUnsupported.
func renameNoReplace(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return errRenameNoReplaceUnsupported
	}
	return &os.LinkError{Op: "rename", Old: source, New: destination, Err: err}
}
