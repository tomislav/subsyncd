package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/store"
)

// A re-import of the same bytes at the same path (an Arr database restore, a
// manual re-import) gives the file a new Arr file ID. It is the same file: the
// record takes the new ID and the sidecar stays.
func TestReconcileKeepsTheSidecarOfAByteIdenticalFileUnderANewFileID(t *testing.T) {
	f := newReconcileFixture(t, "Movie.WADU.mkv")
	same := f.mediaFile(t, "Movie.WADU.mkv", 99, "media")
	f.import_(t, "import", same)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v, want the record reattached", changed, err)
	}
	assertFile(t, f.sidecar, f.content)
	got := f.installation(t)
	if got.MediaFileID != 99 || got.Path != f.sidecar || got.MediaPath != same.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM search_history WHERE outcome='removed'`); n != 0 {
		t.Fatalf("removal history rows = %d", n)
	}
	if record := f.event(t, "install.reattached"); record["provider"] != "provider" {
		t.Fatalf("install.reattached = %#v", record)
	}
	if changed, err := f.reconcile(t); err != nil || changed {
		t.Fatalf("second ReconcileInstallation() = %v/%v, want nothing to do", changed, err)
	}
}

func TestReconcileKeepsAByteIdenticalFileArrRenamedUnderANewFileID(t *testing.T) {
	// Same size, new path and new file ID, and Arr renamed the sidecar after
	// it: the same file, so the record follows it.
	f := newReconcileFixture(t, "Movie.Old.mkv")
	if err := os.Remove(f.media.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	renamed := f.mediaFile(t, "Movie.New.mkv", 99, "media")
	f.import_(t, "import", renamed)
	moved := f.sidecarFor(renamed)
	if err := os.Rename(f.sidecar, moved); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertFile(t, moved, f.content)
	if got := f.installation(t); got.MediaFileID != 99 || got.Path != moved || got.MediaPath != renamed.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
	if changed, err := f.reconcile(t); err != nil || changed {
		t.Fatalf("second ReconcileInstallation() = %v/%v", changed, err)
	}
}

func TestReconcileRemovesTheSidecarOfASameSizeFileUnderANewPathAndFileID(t *testing.T) {
	// Same size only by chance: another path, another file ID, and the
	// sidecar still has the old name. That is a replacement.
	f := newReconcileFixture(t, "Movie.Old.mkv")
	f.import_(t, "import", f.mediaFile(t, "Movie.New.mkv", 99, "MEDIA"))
	result, err := f.reconcileResult(t)
	if err != nil || !result.Changed || !result.Removed {
		t.Fatalf("ReconcileInstallation() = %+v/%v", result, err)
	}
	assertGone(t, f.sidecar)
	f.assertNoInstallation(t)
}

// A mass replacement (100 movies upgraded at once) deletes at most
// MaxReplacedRemovalsPerHour sidecars per rolling hour; the rest stay, with
// their records, until a later run.
func TestReconcileCapsReplacedRemovalsPerHour(t *testing.T) {
	const files = 100
	f := newReconcileFixture(t, "Movie.mkv")
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	f.installer.ReplacedRemovals = NewReplacedRemovalBudget()
	f.installer.Now = func() time.Time { return now }
	type movie struct {
		id      int64
		media   domain.Media
		sidecar string
	}
	movies := make([]movie, files)
	for index := range movies {
		folder := filepath.Join(f.root, fmt.Sprintf("Movie %03d", index))
		if err := os.Mkdir(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		entity := int64(100 + index)
		describe := func(name string, fileID int64, content string) domain.Media {
			path := writeInstallFile(t, filepath.Join(folder, name), content)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			return domain.Media{EntityID: entity, Title: "Movie", Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: fileID}, Fingerprint: domain.MediaFingerprint{Path: path, FileID: fileID, Size: info.Size(), ModTime: info.ModTime()}, Duration: 90 * time.Minute}
		}
		original := describe("Movie.WADU.mkv", 1000+entity, "media")
		f.import_(t, "import", original)
		source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
		installed, err := f.installer.Install(f.ctx, InstallRequest{MediaID: f.mediaID, Media: original, Language: "en", SourcePath: source, DestinationPath: f.sidecarFor(original), Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate"}, Score: domain.Score{Total: 70}, SyncResult: domain.SyncResult{Verdict: "solid"}})
		if err != nil {
			t.Fatal(err)
		}
		upgraded := describe("Movie.FUZEER.mkv", 2000+entity, "upgraded media")
		f.import_(t, "import", upgraded)
		movies[index] = movie{id: f.mediaID, media: upgraded, sidecar: installed.Path}
	}
	deferredLogged := func() int { return strings.Count(f.logs.String(), `"install.removal_deferred"`) }
	pass := func() (removed, deferred int) {
		t.Helper()
		for _, m := range movies {
			installation, found, err := f.repo.GetInstallation(f.ctx, m.id, "en")
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				continue
			}
			before := deferredLogged()
			result, err := f.installer.ReconcileInstallation(f.ctx, RemovalRequest{Installation: installation, Media: m.media})
			if err != nil {
				t.Fatal(err)
			}
			if result.Removed {
				removed++
				assertGone(t, m.sidecar)
				continue
			}
			deferred++
			if result.Changed {
				t.Fatalf("deferred reconciliation reported a change: %+v", result)
			}
			assertFile(t, m.sidecar, installSRT)
			if kept, found, err := f.repo.GetInstallation(f.ctx, m.id, "en"); err != nil || !found || kept.MediaFileID != installation.MediaFileID {
				t.Fatalf("deferred installation = %#v/%v/%v", kept, found, err)
			}
			if n := deferredLogged() - before; n != 1 {
				t.Fatalf("install.removal_deferred records = %d, want 1", n)
			}
		}
		return removed, deferred
	}
	if removed, deferred := pass(); removed != MaxReplacedRemovalsPerHour || deferred != files-MaxReplacedRemovalsPerHour {
		t.Fatalf("first pass removed %d, deferred %d", removed, deferred)
	}
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "install.removal_deferred" && (record["level"] != "warn" || record["reason"] != "cap" || record["limit_per_hour"] != float64(MaxReplacedRemovalsPerHour)) {
			t.Fatalf("install.removal_deferred = %#v", record)
		}
	}
	now = now.Add(59 * time.Minute)
	if removed, _ := pass(); removed != 0 {
		t.Fatalf("removed %d within the hour", removed)
	}
	now = now.Add(2 * time.Minute)
	if removed, deferred := pass(); removed != MaxReplacedRemovalsPerHour || deferred != files-2*MaxReplacedRemovalsPerHour {
		t.Fatalf("second hour removed %d, deferred %d", removed, deferred)
	}
	if n := f.count(t, `SELECT count(*) FROM search_history WHERE outcome='removed' AND reason='media_replaced'`); n != 2*MaxReplacedRemovalsPerHour {
		t.Fatalf("removal history rows = %d", n)
	}
}

func TestRemovalBudgetGivesBackAnUnusedReservation(t *testing.T) {
	budget := &RemovalBudget{limit: 1, window: time.Hour}
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	release, ok := budget.reserve(now)
	if !ok {
		t.Fatal("first reservation refused")
	}
	if _, ok := budget.reserve(now); ok {
		t.Fatal("second reservation granted past the limit")
	}
	release()
	release()
	if _, ok := budget.reserve(now); !ok {
		t.Fatal("reservation after a release refused")
	}
	var unlimited *RemovalBudget
	if _, ok := unlimited.reserve(now); !ok {
		t.Fatal("a nil budget refused")
	}
}

func TestRemoveSameFileKeepsOneFileUnderTwoSpellings(t *testing.T) {
	// A case-only rename on a case-insensitive filesystem leaves one file
	// that both spellings name; removing either would delete it.
	root := t.TempDir()
	path := writeInstallFile(t, filepath.Join(root, "Movie.Old.en.srt"), installSRT)
	if err := removeSameFile(path, path); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, installSRT)
	lower := filepath.Join(root, "movie.old.en.srt")
	if info, err := os.Lstat(lower); err == nil {
		original, _ := os.Lstat(path)
		if !os.SameFile(info, original) {
			t.Fatal("lower-case name is another file")
		}
		if err := removeSameFile(path, lower); err != nil {
			t.Fatal(err)
		}
		assertFile(t, path, installSRT)
	}
	// A real second name is removed.
	link := filepath.Join(root, "Movie.New.en.srt")
	if err := os.Link(path, link); err != nil {
		t.Skipf("no hard links: %v", err)
	}
	if err := removeSameFile(path, link); err != nil {
		t.Fatal(err)
	}
	assertGone(t, path)
	assertFile(t, link, installSRT)
}

func TestReconcileCaseOnlyRenameKeepsTheSidecar(t *testing.T) {
	f := newReconcileFixture(t, "Movie.Old.mkv")
	if _, err := os.Lstat(filepath.Join(f.root, "movie.old.mkv")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "movie.old.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	if _, err := f.reconcile(t); err != nil {
		t.Fatal(err)
	}
	got := f.installation(t)
	assertFile(t, got.Path, f.content)
	if got.MediaPath != renamed.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
}

// renameFixture renames the media file and leaves the sidecar at its old
// name, as Arr does when it does not rename subtitles.
func renameFixture(t *testing.T) (*reconcileFixture, string, string) {
	t.Helper()
	f := newReconcileFixture(t, "Movie.Old.mkv")
	old := f.sidecar
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	return f, old, f.sidecarFor(renamed)
}

func TestReconcileFinishesAMoveWhoseOldNameCouldNotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f, old, moved := renameFixture(t)
	// The link succeeds; then the folder turns read-only, so removing the
	// old name after the commit fails, as a crash there would leave it.
	f.installer.Link = func(source, destination string) error {
		if err := os.Link(source, destination); err != nil {
			return err
		}
		return os.Chmod(f.root, 0o555)
	}
	t.Cleanup(func() { _ = os.Chmod(f.root, 0o755) })
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	if err := os.Chmod(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	assertFile(t, old, f.content)
	assertFile(t, moved, f.content)
	if got := f.installation(t); got.Path != moved || got.MediaPath != f.media.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 1 {
		t.Fatalf("pending rows = %d, want the move's", n)
	}
	f.event(t, "install.rename_cleanup_failed")
	if settled, err := f.installer.RecoverPendingInstalls(f.ctx); err != nil || settled != 1 {
		t.Fatalf("RecoverPendingInstalls() = %d/%v", settled, err)
	}
	assertGone(t, old)
	assertFile(t, moved, f.content)
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
		t.Fatalf("pending rows = %d", n)
	}
	_ = f.installation(t)
}

func TestRecoveryLeavesAnotherFileAtTheOldName(t *testing.T) {
	f, old, moved := renameFixture(t)
	// Crash after the link and the commit: simulated by the pending row and
	// both names, then the old name replaced by another file.
	if err := os.Link(old, moved); err != nil {
		t.Skipf("no hard links: %v", err)
	}
	info, err := os.Lstat(old)
	if err != nil {
		t.Fatal(err)
	}
	pending := store.PendingInstall{MediaID: f.mediaID, Language: "en", Destination: moved, Checksum: f.installation(t).Checksum, RollbackPath: old, PublishedInode: fileInode(info)}
	if _, err := f.installer.beginPendingInstall(f.ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	theirs := "1\n00:00:01,000 --> 00:00:02,000\nTheirs\n"
	writeInstallFile(t, old, theirs)
	if settled, err := f.installer.RecoverPendingInstalls(f.ctx); err != nil || settled != 1 {
		t.Fatalf("RecoverPendingInstalls() = %d/%v", settled, err)
	}
	assertFile(t, old, theirs)
	assertFile(t, moved, f.content)
}

func TestReconcileDropsARecordWhoseSidecarIsOnAnotherFilesystem(t *testing.T) {
	f, old, moved := renameFixture(t)
	links := 0
	f.installer.Link = func(source, destination string) error {
		links++
		return &os.LinkError{Op: "link", Old: source, New: destination, Err: syscall.EXDEV}
	}
	f.installer.RenameNoReplace = func(string, string) error {
		t.Fatal("rename tried across filesystems")
		return nil
	}
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertFile(t, old, f.content)
	assertGone(t, moved)
	f.assertNoInstallation(t)
	if record := f.event(t, "install.record_dropped"); record["reason"] != "sidecar_unmovable" {
		t.Fatalf("install.record_dropped = %#v", record)
	}
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
		t.Fatalf("pending rows = %d", n)
	}
	if links != 1 {
		t.Fatalf("links = %d", links)
	}
}

func TestReconcileDropsARecordItCannotMoveWithoutReplacing(t *testing.T) {
	f, old, moved := renameFixture(t)
	f.installer.Link = func(string, string) error { return syscall.EPERM }
	f.installer.RenameNoReplace = func(string, string) error { return errRenameNoReplaceUnsupported }
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertFile(t, old, f.content)
	assertGone(t, moved)
	f.assertNoInstallation(t)
}

func TestReconcileMovesWithoutHardLinks(t *testing.T) {
	f, old, moved := renameFixture(t)
	f.installer.Link = func(string, string) error { return syscall.EPERM }
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertGone(t, old)
	assertFile(t, moved, f.content)
	if got := f.installation(t); got.Path != moved || got.MediaPath != f.media.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
		t.Fatalf("pending rows = %d", n)
	}
}

func TestReconcileMoveWithoutHardLinksNeverOverwritesAFileCreatedMeanwhile(t *testing.T) {
	f, old, moved := renameFixture(t)
	theirs := "1\n00:00:01,000 --> 00:00:02,000\nTheirs\n"
	// The new name appears after the checks, just before the move.
	f.installer.Link = func(_, destination string) error {
		writeInstallFile(t, destination, theirs)
		return syscall.EPERM
	}
	if _, err := f.reconcile(t); err == nil {
		t.Fatal("ReconcileInstallation() moved over a file created meanwhile")
	}
	assertFile(t, moved, theirs)
	assertFile(t, old, f.content)
	_ = f.installation(t)
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
		t.Fatalf("pending rows = %d", n)
	}
}

func TestRenameNoReplaceRefusesAnExistingDestination(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(root, "a.srt"), "a")
	destination := writeInstallFile(t, filepath.Join(root, "b.srt"), "b")
	err := renameNoReplace(source, destination)
	if errors.Is(err, errRenameNoReplaceUnsupported) {
		t.Skip("no atomic no-replace rename here")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("renameNoReplace() = %v, want ErrExist", err)
	}
	assertFile(t, source, "a")
	assertFile(t, destination, "b")
	if err := renameNoReplace(source, filepath.Join(root, "c.srt")); err != nil {
		t.Fatal(err)
	}
	assertGone(t, source)
}

func TestArrRescanIsDueAfterTheCoalescingDelay(t *testing.T) {
	f := newRescanFixture(t, "Movie.mkv")
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	f.installer.Now = func() time.Time { return now }
	if err := f.install(t, "hr", installSRT); err != nil {
		t.Fatal(err)
	}
	rows, err := f.audit.Query(`SELECT notifier, next_attempt_at_ns FROM notifications ORDER BY notifier`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	due := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			t.Fatal(err)
		}
		due[name] = time.Unix(0, at).UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !due["silo"].Equal(now) || !due[notifier.ArrRescanName].Equal(now.Add(notifier.ArrRescanDelay)) || len(due) != 2 {
		t.Fatalf("due = %v, want Silo at once and the Arr rescan after %s", due, notifier.ArrRescanDelay)
	}
}
