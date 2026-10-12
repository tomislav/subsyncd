package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// reconcileFixture is a managed subtitle installed for one media file through
// the real installer and store, whose media file can then be replaced or
// renamed as Arr does.
type reconcileFixture struct {
	ctx       context.Context
	root      string
	repo      *store.Repository
	audit     *sql.DB
	installer Installer
	logs      *bytes.Buffer
	media     domain.Media
	mediaID   int64
	sidecar   string
	content   string
	at        time.Time
	events    int
}

func newReconcileFixture(t *testing.T, mediaName string) *reconcileFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "subsyncd.db")
	database, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	audit, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	logs := &bytes.Buffer{}
	events, err := observability.New(logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	f := &reconcileFixture{ctx: ctx, root: root, repo: database.Repository(), audit: audit, logs: logs, at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	f.installer = Installer{Repository: f.repo, MediaRoots: []string{root}, NotifierNames: []string{"silo"}, Events: events}
	f.media = f.mediaFile(t, mediaName, 7, "media")
	f.import_(t, "import", f.media)
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	request := InstallRequest{MediaID: f.mediaID, Media: f.media, Language: "en", SourcePath: source, DestinationPath: f.sidecarFor(f.media), Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate"}, Score: domain.Score{Total: 70}, SyncResult: domain.SyncResult{Verdict: "solid"}}
	installation, err := f.installer.Install(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.sidecar, f.content = installation.Path, installSRT
	if _, err := audit.Exec(`DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
	return f
}

// mediaFile writes a media file in the fixture's folder and describes it as
// Arr file fileID of movie 1.
func (f *reconcileFixture) mediaFile(t *testing.T, name string, fileID int64, content string) domain.Media {
	t.Helper()
	path := writeInstallFile(t, filepath.Join(f.root, name), content)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Media{EntityID: 1, Title: "Movie", Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: fileID}, Fingerprint: domain.MediaFingerprint{Path: path, FileID: fileID, Size: info.Size(), ModTime: info.ModTime()}, Duration: 90 * time.Minute}
}

func (f *reconcileFixture) sidecarFor(media domain.Media) string {
	return strings.TrimSuffix(media.Fingerprint.Path, filepath.Ext(media.Fingerprint.Path)) + ".en.srt"
}

// import_ applies an Arr import or rename of media.
func (f *reconcileFixture) import_(t *testing.T, kind string, media domain.Media) {
	t.Helper()
	f.events++
	f.at = f.at.Add(time.Minute)
	if _, err := f.repo.ApplyMediaEvent(f.ctx, store.MediaEventMutation{EventID: kind + "-" + string(rune('a'+f.events)), Type: kind, EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: f.at}); err != nil {
		t.Fatal(err)
	}
	id, _, err := f.repo.FindMedia(f.ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	f.mediaID, f.media = id, media
}

// reconcile runs ReconcileInstallation for the recorded installation against
// the current media file.
func (f *reconcileFixture) reconcile(t *testing.T) (bool, error) {
	t.Helper()
	result, err := f.reconcileResult(t)
	return result.Changed, err
}

func (f *reconcileFixture) reconcileResult(t *testing.T) (Reconciliation, error) {
	t.Helper()
	installation := f.installation(t)
	return f.installer.ReconcileInstallation(f.ctx, RemovalRequest{Installation: installation, Media: f.media})
}

func (f *reconcileFixture) installation(t *testing.T) store.Installation {
	t.Helper()
	installation, found, err := f.repo.GetInstallation(f.ctx, f.mediaID, "en")
	if err != nil || !found {
		t.Fatalf("GetInstallation() = %#v/%v/%v", installation, found, err)
	}
	return installation
}

func (f *reconcileFixture) assertNoInstallation(t *testing.T) {
	t.Helper()
	if installation, found, err := f.repo.GetInstallation(f.ctx, f.mediaID, "en"); err != nil || found {
		t.Fatalf("installation = %#v/%v/%v, want none", installation, found, err)
	}
}

func (f *reconcileFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.audit.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// event returns the single log record with that event name.
func (f *reconcileFixture) event(t *testing.T, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == name {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s records = %#v; logs:\n%s", name, found, f.logs.String())
	}
	if strings.Contains(f.logs.String(), f.root) {
		t.Fatalf("logs carry the media path: %s", f.logs.String())
	}
	return found[0]
}

func assertFile(t *testing.T, path, content string) {
	t.Helper()
	if payload, err := os.ReadFile(path); err != nil || string(payload) != content {
		t.Fatalf("%s = %q/%v, want %q", filepath.Base(path), payload, err, content)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", filepath.Base(path), err)
	}
}

func TestReconcileRemovesTheSidecarOfAnUpgradedFile(t *testing.T) {
	for _, test := range []struct {
		name    string
		newName string
	}{
		{"different names", "Movie.FUZEER.mkv"},
		{"same name", "Movie.WADU.mkv"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReconcileFixture(t, "Movie.WADU.mkv")
			old := f.sidecar
			if test.newName == "Movie.WADU.mkv" {
				if err := os.Remove(f.media.Fingerprint.Path); err != nil {
					t.Fatal(err)
				}
			}
			f.import_(t, "import", f.mediaFile(t, test.newName, 8, "upgraded media"))
			changed, err := f.reconcile(t)
			if err != nil || !changed {
				t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
			}
			assertGone(t, old)
			f.assertNoInstallation(t)
			if n := f.count(t, `SELECT count(*) FROM search_history WHERE outcome='removed' AND reason='media_replaced'`); n != 1 {
				t.Fatalf("removal history rows = %d", n)
			}
			if n := f.count(t, `SELECT count(*) FROM notifications`); n != 1 {
				t.Fatalf("notifications = %d, want a rescan of the folder", n)
			}
			if record := f.event(t, "install.removed"); record["reason"] != "media_replaced" || record["provider"] != "provider" {
				t.Fatalf("install.removed = %#v", record)
			}
		})
	}
}

func TestReconcileRemovesAnUpgradedSidecarArrRenamed(t *testing.T) {
	// Arr carried the old sidecar over to the new file's name: it is still
	// the old file's subtitle and goes.
	f := newReconcileFixture(t, "Movie.playWEB.mkv")
	f.import_(t, "import", f.mediaFile(t, "Movie.SLiGNOME.mkv", 8, "upgraded media"))
	carried := f.sidecarFor(f.media)
	if err := os.Rename(f.sidecar, carried); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertGone(t, carried)
	f.assertNoInstallation(t)
}

func TestReconcileKeepsAnEditedSidecarOfAnUpgradedFile(t *testing.T) {
	for _, newName := range []string{"Movie.FUZEER.mkv", "Movie.WADU.mkv"} {
		t.Run(newName, func(t *testing.T) {
			f := newReconcileFixture(t, "Movie.WADU.mkv")
			writeInstallFile(t, f.sidecar, "1\n00:00:01,000 --> 00:00:02,000\nMine\n")
			if newName == "Movie.WADU.mkv" {
				if err := os.Remove(f.media.Fingerprint.Path); err != nil {
					t.Fatal(err)
				}
			}
			f.import_(t, "import", f.mediaFile(t, newName, 8, "upgraded media"))
			if changed, err := f.reconcile(t); err != nil || !changed {
				t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
			}
			assertFile(t, f.sidecar, "1\n00:00:01,000 --> 00:00:02,000\nMine\n")
			f.assertNoInstallation(t)
			if record := f.event(t, "install.record_dropped"); record["reason"] != "media_replaced" {
				t.Fatalf("install.record_dropped = %#v", record)
			}
		})
	}
}

func TestReconcileLeavesASymlinkedSidecarAlone(t *testing.T) {
	f := newReconcileFixture(t, "Movie.WADU.mkv")
	target := filepath.Join(f.root, "target.srt")
	if err := os.Rename(f.sidecar, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.sidecar); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "import", f.mediaFile(t, "Movie.FUZEER.mkv", 8, "upgraded media"))
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	if info, err := os.Lstat(f.sidecar); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink = %v/%v", info, err)
	}
	assertFile(t, target, f.content)
	f.assertNoInstallation(t)
}

func TestReconcileRemovesTheSidecarOfAFileReplacedInPlace(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	replaced := f.mediaFile(t, "Movie.mkv", 7, "replaced in place, longer")
	if err := f.repo.ReplaceTrackInventory(f.ctx, f.mediaID, f.media.Fingerprint, replaced.Fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	f.media = replaced
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertGone(t, f.sidecar)
	f.assertNoInstallation(t)
}

func TestReconcileKeepsTheSidecarWhenOnlyTheModificationTimeChanged(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	touched := f.media
	touched.Fingerprint.ModTime = touched.Fingerprint.ModTime.Add(time.Hour)
	if err := os.Chtimes(touched.Fingerprint.Path, touched.Fingerprint.ModTime, touched.Fingerprint.ModTime); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ReplaceTrackInventory(f.ctx, f.mediaID, f.media.Fingerprint, touched.Fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	f.media = touched
	if changed, err := f.reconcile(t); err != nil || changed {
		t.Fatalf("ReconcileInstallation() = %v/%v, want nothing to do", changed, err)
	}
	assertFile(t, f.sidecar, f.content)
	_ = f.installation(t)
}

func TestReconcileFollowsARenameArrMoved(t *testing.T) {
	f := newReconcileFixture(t, "Movie.Old.mkv")
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.sidecar, f.sidecarFor(renamed)); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	got := f.installation(t)
	if got.Path != f.sidecarFor(renamed) || got.MediaPath != renamed.Fingerprint.Path || !InstallationMatchesMedia(got, renamed) {
		t.Fatalf("installation = %#v", got)
	}
	assertFile(t, got.Path, f.content)
	if record := f.event(t, "install.renamed"); record["moved"] != false {
		t.Fatalf("install.renamed = %#v", record)
	}
	// Settled: a second pass has nothing to do.
	if changed, err := f.reconcile(t); err != nil || changed {
		t.Fatalf("second ReconcileInstallation() = %v/%v", changed, err)
	}
}

func TestReconcileMovesASidecarArrLeftBehindOnRename(t *testing.T) {
	f := newReconcileFixture(t, "Movie.Old.mkv")
	old := f.sidecar
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertGone(t, old)
	assertFile(t, f.sidecarFor(renamed), f.content)
	if got := f.installation(t); got.Path != f.sidecarFor(renamed) || got.MediaPath != renamed.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
	if record := f.event(t, "install.renamed"); record["moved"] != true {
		t.Fatalf("install.renamed = %#v", record)
	}
}

func TestReconcileRenameNeverOverwritesAnotherFile(t *testing.T) {
	f := newReconcileFixture(t, "Movie.Old.mkv")
	old := f.sidecar
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	theirs := "1\n00:00:01,000 --> 00:00:02,000\nTheirs\n"
	writeInstallFile(t, f.sidecarFor(renamed), theirs)
	f.import_(t, "rename", renamed)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertFile(t, f.sidecarFor(renamed), theirs)
	assertGone(t, old)
	f.assertNoInstallation(t)
	if record := f.event(t, "install.removed"); record["reason"] != "media_renamed" {
		t.Fatalf("install.removed = %#v", record)
	}
}

func TestReconcileDropsARenamedRecordWhoseSidecarIsGone(t *testing.T) {
	f := newReconcileFixture(t, "Movie.Old.mkv")
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.sidecar); err != nil {
		t.Fatal(err)
	}
	f.import_(t, "rename", renamed)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	f.assertNoInstallation(t)
	if record := f.event(t, "install.record_dropped"); record["reason"] != "sidecar_missing" {
		t.Fatalf("install.record_dropped = %#v", record)
	}
	if n := f.count(t, `SELECT count(*) FROM events WHERE event_type='subtitle_record_dropped' AND outcome='sidecar_missing'`); n != 1 {
		t.Fatalf("drop audit events = %d", n)
	}
}

func TestReconcileFinishesAMoveACrashInterrupted(t *testing.T) {
	// A crash after linking the new name and before recording it leaves the
	// same file under both names; the next pass records the new name and
	// removes the old link.
	f := newReconcileFixture(t, "Movie.Old.mkv")
	old := f.sidecar
	renamed := f.media
	renamed.Fingerprint.Path = filepath.Join(f.root, "Movie.New.mkv")
	if err := os.Rename(f.media.Fingerprint.Path, renamed.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(old, f.sidecarFor(renamed)); err != nil {
		t.Skipf("no hard links: %v", err)
	}
	f.import_(t, "rename", renamed)
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	assertGone(t, old)
	assertFile(t, f.sidecarFor(renamed), f.content)
	if got := f.installation(t); got.MediaPath != renamed.Fingerprint.Path {
		t.Fatalf("installation = %#v", got)
	}
}

func TestReconcileKeepsAnEditedSidecarOfTheCurrentFile(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	edited := "1\n00:00:01,000 --> 00:00:02,000\nMine\n"
	writeInstallFile(t, f.sidecar, edited)
	if changed, err := f.reconcile(t); err != nil || changed {
		t.Fatalf("ReconcileInstallation() = %v/%v, want the edit kept as the owner's", changed, err)
	}
	assertFile(t, f.sidecar, edited)
	_ = f.installation(t)
}

func TestReconcileDropsARecordWhoseSidecarVanished(t *testing.T) {
	f := newReconcileFixture(t, "Movie.mkv")
	if err := os.Remove(f.sidecar); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.reconcile(t); err != nil || !changed {
		t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
	}
	f.assertNoInstallation(t)
}

// clearProvenance makes the record look like one an older build left after a
// media change: the media path, file ID, size and time cleared, and the
// sidecar path renamed after the current file.
func (f *reconcileFixture) clearProvenance(t *testing.T, path, checksum string) {
	t.Helper()
	if _, err := f.audit.Exec(`UPDATE installations SET path=?, checksum=?, media_path='', media_file_id=0, media_size=0, media_mod_time_ns=0, score_json='{}', sync_result_json='{}'`, path, checksum); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRepairsRecordsAnOlderBuildRebased(t *testing.T) {
	t.Run("record names a sidecar that does not exist", func(t *testing.T) {
		// Mad Men S04E02: the record was rebased to the new release's name,
		// the file never moved, and its checksum is the old release's.
		f := newReconcileFixture(t, "Movie.BETTY.mkv")
		orphan := f.sidecar
		f.import_(t, "import", f.mediaFile(t, "Movie.WADU.mkv", 8, "upgraded media"))
		f.clearProvenance(t, f.sidecarFor(f.media), f.installation(t).Checksum)
		if changed, err := f.reconcile(t); err != nil || !changed {
			t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
		}
		f.assertNoInstallation(t)
		assertFile(t, orphan, f.content)
		if record := f.event(t, "install.record_dropped"); record["reason"] != "sidecar_missing" {
			t.Fatalf("install.record_dropped = %#v", record)
		}
	})
	t.Run("record names an older subtitle at the current name", func(t *testing.T) {
		// Mad Men S02E01: the record has the current name but the checksum
		// of another release's subtitle, so the file there is not an edit of
		// subsyncd's subtitle for this file.
		f := newReconcileFixture(t, "Movie.WADU.mkv")
		older := "1\n00:00:01,000 --> 00:00:02,000\nOlder\n"
		writeInstallFile(t, f.sidecar, older)
		if err := os.Remove(f.media.Fingerprint.Path); err != nil {
			t.Fatal(err)
		}
		f.import_(t, "import", f.mediaFile(t, "Movie.WADU.mkv", 9, "re-imported media"))
		f.clearProvenance(t, f.sidecar, checksumBytes([]byte("the FUZEER subtitle")))
		if changed, err := f.reconcile(t); err != nil || !changed {
			t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
		}
		f.assertNoInstallation(t)
		assertFile(t, f.sidecar, older)
		if record := f.event(t, "install.record_dropped"); record["reason"] != "sidecar_changed" {
			t.Fatalf("install.record_dropped = %#v", record)
		}
	})
	t.Run("unchanged sidecar at the current name stays managed", func(t *testing.T) {
		f := newReconcileFixture(t, "Movie.mkv")
		f.clearProvenance(t, f.sidecar, f.installation(t).Checksum)
		if changed, err := f.reconcile(t); err != nil || changed {
			t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
		}
		assertFile(t, f.sidecar, f.content)
		_ = f.installation(t)
	})
	t.Run("unchanged sidecar of another name is removed", func(t *testing.T) {
		f := newReconcileFixture(t, "Movie.playWEB.mkv")
		old := f.sidecar
		f.import_(t, "import", f.mediaFile(t, "Movie.SLiGNOME.mkv", 8, "upgraded media"))
		f.clearProvenance(t, old, f.installation(t).Checksum)
		if changed, err := f.reconcile(t); err != nil || !changed {
			t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
		}
		assertGone(t, old)
		f.assertNoInstallation(t)
	})
}

func TestReconcileRemovalIsCrashSafe(t *testing.T) {
	t.Run("failed commit keeps everything", func(t *testing.T) {
		f := newReconcileFixture(t, "Movie.WADU.mkv")
		f.import_(t, "import", f.mediaFile(t, "Movie.FUZEER.mkv", 8, "upgraded media"))
		f.installer.Fault = func(stage InstallStage) error {
			if stage == StageDatabase {
				return errors.New("injected")
			}
			return nil
		}
		if _, err := f.reconcile(t); err == nil {
			t.Fatal("ReconcileInstallation() succeeded through a failed commit")
		}
		assertFile(t, f.sidecar, f.content)
		_ = f.installation(t)
		if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
			t.Fatalf("pending rows = %d", n)
		}
	})
	t.Run("crash after the commit is finished at startup", func(t *testing.T) {
		f := newReconcileFixture(t, "Movie.WADU.mkv")
		f.import_(t, "import", f.mediaFile(t, "Movie.FUZEER.mkv", 8, "upgraded media"))
		f.installer.Fault = func(stage InstallStage) error {
			if stage == StageCleanup {
				return errors.New("crash")
			}
			return nil
		}
		if changed, err := f.reconcile(t); err != nil || !changed {
			t.Fatalf("ReconcileInstallation() = %v/%v", changed, err)
		}
		f.assertNoInstallation(t)
		if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 1 {
			t.Fatalf("pending rows = %d, want the removal's", n)
		}
		f.installer.Fault = nil
		if settled, err := f.installer.RecoverPendingInstalls(f.ctx); err != nil || settled != 1 {
			t.Fatalf("RecoverPendingInstalls() = %d/%v", settled, err)
		}
		entries, err := os.ReadDir(f.root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".subsyncd-") || strings.HasSuffix(entry.Name(), ".srt") {
				t.Fatalf("left behind: %s", entry.Name())
			}
		}
	})
}

// staleRecordInstaller reconciles like the real installer would: the record
// belongs to a replaced file and is dropped.
type staleRecordInstaller struct {
	fakeInstaller
	repository *workflowRepository
	calls      int
}

func (s *staleRecordInstaller) ReconcileInstallation(_ context.Context, request RemovalRequest) (Reconciliation, error) {
	s.calls++
	if request.Installation.MediaFileID == request.Media.Ref.FileID {
		return Reconciliation{}, nil
	}
	s.repository.found = false
	s.repository.installation = store.Installation{}
	return Reconciliation{Changed: true}, nil
}

func TestRunSettlesAStaleInstallationBeforeTheInventoryDecides(t *testing.T) {
	request := serviceRequest(t)
	existing := matchingInstallation(request, exactCandidate("old"), []byte(`{"total":100}`))
	existing.MediaFileID = request.Media.Ref.FileID + 1
	searcher := &fakeSearcher{}
	repository := &workflowRepository{found: true, installation: existing}
	installer := &staleRecordInstaller{repository: repository}
	current := managedSidecarInventory(existing)
	current.Fingerprint = request.Media.Fingerprint
	service := testService(t, current, searcher, nil, &fakeSynchronizer{}, installer)
	service.Repository = repository
	inventory := service.Inventory.(*fakeInventory)
	if _, err := service.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if installer.calls != 1 || inventory.calls != 2 {
		t.Fatalf("reconcile calls = %d, inventory refreshes = %d; want the inventory refreshed after the record went", installer.calls, inventory.calls)
	}
}
