package inventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

const privateName = "Private Show - S01E01 - Private Title"

func assertPathFree(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "Private") || strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Fatalf("error exposes a media path: %q", err)
	}
}

func TestRefreshErrorsOmitMediaPath(t *testing.T) {
	directory := t.TempDir()
	service := Service{Repository: &fakeRepository{}}

	missing := filepath.Join(directory, privateName+".mkv")
	_, err := service.Refresh(context.Background(), 1, domain.Media{Fingerprint: domain.MediaFingerprint{Path: missing}}, false)
	assertPathFree(t, err)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v; want it to remain a not-exist error", err)
	}

	notRegular := filepath.Join(directory, privateName)
	if err := os.Mkdir(notRegular, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = service.Refresh(context.Background(), 1, domain.Media{Fingerprint: domain.MediaFingerprint{Path: notRegular}}, false)
	assertPathFree(t, err)
}

func TestScanSidecarErrorsOmitMediaPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), privateName)
	_, err := ScanSidecars(filepath.Join(directory, privateName+".mkv"), nil)
	assertPathFree(t, err)

	if os.Geteuid() == 0 {
		t.Skip("root can read unreadable files")
	}
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(directory, privateName+".en.srt")
	if err := os.WriteFile(sidecar, []byte("1\n00:00:01,000 --> 00:00:02,000\nx\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err = ScanSidecars(filepath.Join(directory, privateName+".mkv"), nil)
	assertPathFree(t, err)
}
