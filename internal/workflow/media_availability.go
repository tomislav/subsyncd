package workflow

import (
	"errors"
	"os"
	"subsyncd/internal/domain"
)

// Check at acquisition boundaries: inventory may have succeeded before an Arr
// rename/removal. Media failures end this search, never reject a candidate.
func checkMediaAvailable(media domain.Media) error {
	info, err := os.Stat(media.Fingerprint.Path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("media file is missing; retry after catalog or filesystem recovery")
	}
	if err != nil {
		return errors.New("media file cannot be inspected")
	}
	if !info.Mode().IsRegular() {
		return errors.New("media path is not a regular file")
	}
	return nil
}
