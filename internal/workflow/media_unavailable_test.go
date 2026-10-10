package workflow

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestMediaUnavailableRecognisesMissingAndUnreadableMedia(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("refresh subtitle inventory: stat media: %w", os.ErrNotExist),
		fmt.Errorf("refresh subtitle inventory: stat media: %w", syscall.ENOTCONN),
		fmt.Errorf("media file cannot be inspected: %w", syscall.EIO),
	} {
		if !MediaUnavailable(err) {
			t.Errorf("MediaUnavailable(%v) = false, want true", err)
		}
	}
	if MediaUnavailable(errors.New("provider offline")) || MediaUnavailable(nil) {
		t.Error("MediaUnavailable() = true for an unrelated error")
	}
}

func TestCheckMediaAvailableKeepsTheCause(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Fingerprint.Path += ".gone"
	if err := checkMediaAvailable(request.Media); !MediaUnavailable(err) {
		t.Fatalf("checkMediaAvailable() = %v, want a media-unavailable error", err)
	}
}
