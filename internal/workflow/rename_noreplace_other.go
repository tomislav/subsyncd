//go:build !linux && !darwin

package workflow

// renameNoReplace has no atomic form here, so a move without hard links
// gives up.
func renameNoReplace(_, _ string) error {
	return errRenameNoReplaceUnsupported
}
