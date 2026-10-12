package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
)

// embeddedEnglishProbe reports one embedded English subtitle stream.
type embeddedEnglishProbe struct{}

func (embeddedEnglishProbe) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "-version" {
		return []byte("ffprobe version 1"), nil, nil
	}
	return []byte(`{"streams":[{"index":2,"codec_name":"subrip","codec_type":"subtitle","tags":{"language":"eng"}}],"format":{"duration":"100"}}`), nil, nil
}

// replaceMedia makes the movie a new Arr file at name (as an upgrade or a
// re-import does) and reopens the application on it.
func (h *rejectHarness) replaceMedia(t *testing.T, name string, fileID int64, content string) {
	t.Helper()
	path := filepath.Join(h.cfg.MediaRoots[0], name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	h.media.Ref.FileID = fileID
	h.media.Fingerprint = domain.MediaFingerprint{Path: path, FileID: fileID, Size: info.Size(), ModTime: info.ModTime()}
	_ = h.app.Close()
	h.open(t)
}

func (h *rejectHarness) search(t *testing.T) string {
	t.Helper()
	result, err := h.app.Search(t.Context(), searchArgs("tv", "movie", h.media.Ref.FileID, "en", false))
	if err != nil || len(result.Languages) != 1 {
		t.Fatalf("Search() = %+v/%v", result, err)
	}
	return result.Languages[0].Outcome
}

func requireGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", filepath.Base(path), err)
	}
}

// TestUpgradeThenReimportLeavesNoOrphanedSidecar replays Mad Men S02E01: an
// upgrade to another release, then the old release imported again. Each
// change removes the managed sidecar of the file it replaced, and a subtitle
// subsyncd does not own is never taken over.
func TestUpgradeThenReimportLeavesNoOrphanedSidecar(t *testing.T) {
	h := newRejectHarness(t, nil)
	first := h.sidecar // Movie.en.srt, installed for Movie.mkv

	h.replaceMedia(t, "Movie.FUZEER.mkv", 8, "upgraded media")
	if outcome := h.search(t); outcome != "installed" {
		t.Fatalf("search after the upgrade = %s", outcome)
	}
	requireGone(t, first)
	upgraded := filepath.Join(h.cfg.MediaRoots[0], "Movie.FUZEER.en.srt")
	installation, found, err := h.app.Repository.GetInstallation(t.Context(), h.mediaID, "en")
	if err != nil || !found || installation.Path != upgraded || installation.MediaFileID != 8 {
		t.Fatalf("installation after the upgrade = %+v/%v/%v", installation, found, err)
	}

	// Someone else's subtitle now sits at the old release's name.
	theirs := subtitleText("theirs")
	if err := os.WriteFile(first, []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.cfg.MediaRoots[0], "Movie.FUZEER.mkv")); err != nil {
		t.Fatal(err)
	}
	h.replaceMedia(t, "Movie.mkv", 9, "re-imported media")
	if outcome := h.search(t); outcome != "satisfied" {
		t.Fatalf("search after the re-import = %s", outcome)
	}
	requireGone(t, upgraded)
	if payload, err := os.ReadFile(first); err != nil || string(payload) != theirs {
		t.Fatalf("their subtitle = %q/%v", payload, err)
	}
	if _, found, err := h.app.Repository.GetInstallation(t.Context(), h.mediaID, "en"); err != nil || found {
		t.Fatalf("installation found/error = %v/%v; the other subtitle is not subsyncd's", found, err)
	}
}

// TestStaleRecordOfAMissingSidecarIsDropped replays Mad Men S04E02: the record
// names a sidecar that does not exist (an older build rebased it to the new
// release's name), and the new release has embedded English.
func TestStaleRecordOfAMissingSidecarIsDropped(t *testing.T) {
	h := newRejectHarness(t, nil)
	orphan := h.sidecar
	h.replaceMedia(t, "Movie.WADU.mkv", 8, "upgraded media")
	installation, found, err := h.app.Repository.GetInstallation(t.Context(), h.mediaID, "en")
	if err != nil || !found {
		t.Fatalf("GetInstallation() = %+v/%v/%v", installation, found, err)
	}
	// What the older build left: the upgrade's import rebased the record and
	// cleared its provenance, while the file stayed under the old name.
	h.exec(t, `UPDATE installations SET path=?, media_path='', media_file_id=0, media_size=0, media_mod_time_ns=0`, filepath.Join(h.cfg.MediaRoots[0], "Movie.WADU.en.srt"))
	h.probe = embeddedEnglishProbe{}
	_ = h.app.Close()
	h.open(t)
	if outcome := h.search(t); outcome != "satisfied" {
		t.Fatalf("search = %s", outcome)
	}
	if _, found, err := h.app.Repository.GetInstallation(t.Context(), h.mediaID, "en"); err != nil || found {
		t.Fatalf("installation found/error = %v/%v", found, err)
	}
	// Without the old file's identity the orphan is not provably subsyncd's
	// to remove; it is left alone.
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan: %v", err)
	}
}
