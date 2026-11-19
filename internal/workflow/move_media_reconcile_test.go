package workflow

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

// TestReconcileKeepsInstallationsAfterMoveMedia moves a managed movie from
// radarr to radarr-1080p as move-media does: the media file and its sidecar
// are renamed into another library folder, and the media row moves in place
// to the new instance, entity and file IDs. The target's catalog (library
// discovery and a history replay of its import) and the real installation
// reconciler must then find the installation unchanged: no media_replaced,
// no sidecar_missing, the sidecar kept. The source's catalog no longer
// listing the file, and its delete events, must leave the moved row alone.
func TestReconcileKeepsInstallationsAfterMoveMedia(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	for _, name := range []string{"radarr", "radarr-1080p"} {
		if err := f.repo.EnsureInstance(f.ctx, name, "radarr", "http://"+name+".invalid", f.at); err != nil {
			t.Fatal(err)
		}
	}
	oldMedia, oldSidecar, oldID := f.media.Fingerprint.Path, f.sidecar, f.mediaID

	// The rename on disk, as the operator does it.
	folder := filepath.Join(f.root, "movies-1080p", "Movie (2021)")
	if err := os.MkdirAll(folder, 0o750); err != nil {
		t.Fatal(err)
	}
	newMedia, newSidecar := filepath.Join(folder, "Movie.mkv"), filepath.Join(folder, filepath.Base(oldSidecar))
	for from, to := range map[string]string{oldMedia: newMedia, oldSidecar: newSidecar} {
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}

	move := store.MediaMoveEntry{Kind: domain.MediaMovie, FromFileID: 7, ToEntityID: 30, ToFileID: 70, ToPath: newMedia}
	if _, err := f.repo.MoveMedia(f.ctx, "radarr", "radarr-1080p", []store.MediaMoveEntry{move}, nil, true); err != nil {
		t.Fatal(err)
	}

	// radarr-1080p lists the file under its own IDs, with the stat Arr reports.
	info, err := os.Stat(newMedia)
	if err != nil {
		t.Fatal(err)
	}
	moved := f.media
	moved.EntityID = 30
	moved.Ref = domain.MediaRef{Instance: "radarr-1080p", Kind: domain.MediaMovie, FileID: 70}
	moved.Fingerprint = domain.MediaFingerprint{Path: newMedia, FileID: 70, Size: info.Size(), ModTime: info.ModTime()}
	_, revision, err := f.repo.LibraryDiscoveryState(f.ctx, "radarr-1080p")
	if err != nil {
		t.Fatal(err)
	}
	f.at = f.at.Add(time.Minute)
	if err := f.repo.CommitLibraryDiscovery(f.ctx, "radarr-1080p", "movies", revision, []domain.Media{moved}, []domain.Language{"en"}, f.at); err != nil {
		t.Fatal(err)
	}
	// Reconciliation replays radarr-1080p's import of the file from history.
	f.events++
	f.at = f.at.Add(time.Minute)
	if _, err := f.repo.ApplyMediaEvent(f.ctx, store.MediaEventMutation{EventID: "1080p-history-import", Type: "import", EntityID: moved.EntityID, Ref: moved.Ref, Media: moved, Languages: []domain.Language{"en"}, At: f.at, Replay: true}); err != nil {
		t.Fatal(err)
	}
	id, current, err := f.repo.FindMedia(f.ctx, moved.Ref)
	if err != nil || id != oldID {
		t.Fatalf("FindMedia(radarr-1080p/70) = %d/%v, want the moved row %d", id, err, oldID)
	}
	f.mediaID, f.media = id, current

	assertKept := func(stage string) {
		t.Helper()
		result, err := f.reconcileResult(t)
		if err != nil || result.Changed || result.Removed || result.Deferred {
			t.Fatalf("%s: ReconcileInstallation() = %#v/%v, want unchanged", stage, result, err)
		}
		installation := f.installation(t)
		if installation.Path != newSidecar || installation.MediaPath != newMedia || installation.MediaFileID != 70 {
			t.Fatalf("%s: installation = %#v", stage, installation)
		}
		assertFile(t, newSidecar, f.content)
		if n := f.count(t, `SELECT count(*) FROM events WHERE event_type='subtitle_record_dropped'`); n != 0 {
			t.Fatalf("%s: dropped records = %d", stage, n)
		}
		if n := f.count(t, `SELECT count(*) FROM search_history WHERE outcome='removed'`); n != 0 {
			t.Fatalf("%s: removals = %d", stage, n)
		}
	}
	assertKept("after the move")

	// radarr no longer lists the file: its discovery and delete events of the
	// old file and movie find nothing to change.
	_, revision, err = f.repo.LibraryDiscoveryState(f.ctx, "radarr")
	if err != nil {
		t.Fatal(err)
	}
	f.at = f.at.Add(time.Minute)
	if err := f.repo.CommitLibraryDiscovery(f.ctx, "radarr", "movies", revision, nil, []domain.Language{"en"}, f.at); err != nil {
		t.Fatal(err)
	}
	for index, ref := range []domain.MediaRef{{Instance: "radarr", Kind: domain.MediaMovie, FileID: 7}, {Instance: "radarr", Kind: domain.MediaMovie}} {
		f.at = f.at.Add(time.Minute)
		if _, err := f.repo.ApplyMediaEvent(f.ctx, store.MediaEventMutation{EventID: "radarr-delete-" + string(rune('a'+index)), Type: "delete", EntityID: 1, Ref: ref, At: f.at}); err != nil {
			t.Fatal(err)
		}
	}
	if active, err := f.repo.MediaActive(f.ctx, moved.Ref); err != nil || !active {
		t.Fatalf("moved media active = %v/%v after radarr's deletes", active, err)
	}
	if n := f.count(t, `SELECT count(*) FROM media WHERE instance='radarr'`); n != 0 {
		t.Fatalf("radarr media rows = %d, want none", n)
	}
	assertKept("after radarr's reconciliation")
}
