package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
)

func TestCheckMediaAvailableMarksAMissingFileUnavailable(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Fingerprint.Path += ".gone"
	if err := checkMediaAvailable(request.Media); !MediaUnavailable(err) {
		t.Fatalf("checkMediaAvailable() = %v, want a media-unavailable error", err)
	}
}

func TestRefreshFailureIsMediaUnavailableOnlyWhenTheMediaCannotBeRead(t *testing.T) {
	notFound := fmt.Errorf("probe media: %w", os.ErrNotExist) // e.g. a missing ffprobe binary
	request := serviceRequest(t)
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
	service.Inventory = &fakeInventory{err: notFound}
	if _, err := service.Run(t.Context(), request); err == nil || MediaUnavailable(err) {
		t.Fatalf("Run() with readable media = %v; want a failure that is not media-unavailable", err)
	}
	if err := os.Remove(request.Media.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(t.Context(), request); !MediaUnavailable(err) {
		t.Fatalf("Run() with the media gone = %v; want media-unavailable", err)
	}
}

func TestUnrelatedNotFoundErrorsAreNotMediaUnavailable(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("open pack cache: %w", os.ErrNotExist),
		errors.Join(errors.New("candidate failed"), fmt.Errorf("scratch: %w", os.ErrNotExist)),
		nil,
	} {
		if MediaUnavailable(err) {
			t.Errorf("MediaUnavailable(%v) = true, want false", err)
		}
	}
	if !MediaUnavailable(fmt.Errorf("refresh: %w", &MediaUnavailableError{Err: filepath.ErrBadPattern})) {
		t.Error("a wrapped MediaUnavailableError was not recognised")
	}
}

func TestProbeTimeoutIsMediaUnavailable(t *testing.T) {
	request := serviceRequest(t)
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
	service.Inventory = &fakeInventory{err: fmt.Errorf("probe media: %w", inventory.ErrProbeTimedOut)}
	if _, err := service.Run(t.Context(), request); !MediaUnavailable(err) {
		t.Fatalf("Run() = %v; want media-unavailable for a hung mount", err)
	}
}

func TestOnlyNotFoundFailuresAreMediaMissing(t *testing.T) {
	request := serviceRequest(t)
	if err := checkMediaReadable(t.Context(), request.Media); err != nil {
		t.Fatalf("readable media = %v", err)
	}
	gone := request.Media
	gone.Fingerprint.Path += ".gone"
	if err := checkMediaReadable(t.Context(), gone); !MediaMissing(err) {
		t.Fatalf("missing media = %v, want the not-found class", err)
	}
	if err := markIfMediaUnreadable(fmt.Errorf("probe media: %w", inventory.ErrProbeTimedOut), request.Media); !MediaUnavailable(err) || MediaMissing(err) {
		t.Fatalf("probe timeout = %v, want media-unavailable but not missing", err)
	}
}

func TestHungMediaReadIsMediaUnavailable(t *testing.T) {
	previousTimeout, previousProbe := mediaReadTimeout, readMediaProbe
	release := make(chan struct{})
	mediaReadTimeout = 20 * time.Millisecond
	readMediaProbe = func(domain.Media) error { <-release; return nil }
	t.Cleanup(func() { close(release); mediaReadTimeout, readMediaProbe = previousTimeout, previousProbe })
	err := checkMediaReadable(t.Context(), serviceRequest(t).Media)
	if !MediaUnavailable(err) || MediaMissing(err) {
		t.Fatalf("hung read = %v, want media-unavailable (a deferral) but not missing", err)
	}
}
