package workflow

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames source to destination unless destination exists
// (os.ErrExist), atomically: renamex_np with RENAME_EXCL. A filesystem
// without that flag answers errRenameNoReplaceUnsupported.
func renameNoReplace(source, destination string) error {
	err := unix.RenamexNp(source, destination, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		return errRenameNoReplaceUnsupported
	}
	return &os.LinkError{Op: "rename", Old: source, New: destination, Err: err}
}
