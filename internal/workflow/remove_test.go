package workflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/store"
)

type removalFixture struct {
	ctx          context.Context
	root         string
	repo         *store.Repository
	audit        *sql.DB
	installer    Installer
	request      InstallRequest
	installation store.Installation
	rollback     string
}

// newRemovalFixture installs a managed subtitle and then replaces it, so the
// installation owns a rollback copy of the first one.
func newRemovalFixture(t *testing.T) *removalFixture {
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
	repo := database.Repository()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
	request.Media.EntityID = 1
	request.Media.Title = "Movie"
	request.MediaID, _, err = repo.UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	installer := Installer{Repository: repo, MediaRoots: []string{root}, NotifierNames: []string{"silo"}}
	if _, err := installer.Install(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.SourcePath = writeInstallFile(t, filepath.Join(t.TempDir(), "replacement.srt"), strings.Replace(installSRT, "Hello", "Replacement", 1))
	installation, err := installer.Install(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if installation.RollbackPath == "" || !regularFile(installation.RollbackPath) {
		t.Fatalf("fixture has no rollback copy: %#v", installation)
	}
	// Deliver the install notifications so only a removal's are due.
	if _, err := audit.Exec(`DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
	return &removalFixture{ctx: ctx, root: root, repo: repo, audit: audit, installer: installer, request: request, installation: installation, rollback: installation.RollbackPath}
}

func (f *removalFixture) removal() RemovalRequest {
	return RemovalRequest{Installation: f.installation, Media: f.request.Media, Manual: true}
}

func (f *removalFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.audit.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertUntouched checks the managed subtitle, its record and its rollback
// copy are as they were, and nothing was queued or left pending.
func (f *removalFixture) assertUntouched(t *testing.T, content string) {
	t.Helper()
	if payload, err := os.ReadFile(f.installation.Path); err != nil || string(payload) != content {
		t.Fatalf("subtitle = %q/%v", payload, err)
	}
	if got, found, err := f.repo.GetInstallation(f.ctx, f.request.MediaID, "en"); err != nil || !found || got.Checksum != f.installation.Checksum {
		t.Fatalf("installation = %#v/%v/%v", got, found, err)
	}
	if !regularFile(f.rollback) {
		t.Fatal("rollback copy is gone")
	}
	if n := f.count(t, `SELECT count(*) FROM notifications`) + f.count(t, `SELECT count(*) FROM pending_installs`) + f.count(t, `SELECT count(*) FROM search_history`); n != 0 {
		t.Fatalf("%d notifications, pending rows or history rows left", n)
	}
	f.assertNoRemovalLeftovers(t)
}

func (f *removalFixture) assertNoRemovalLeftovers(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), removalPrefix) {
			t.Fatalf("leftover removal file %s", entry.Name())
		}
	}
}

func TestRemoveManagedDeletesSubtitleRecordAndRollbackCopy(t *testing.T) {
	f := newRemovalFixture(t)
	if err := f.installer.RemoveManaged(f.ctx, f.removal()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.installation.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle remains: %v", err)
	}
	if _, err := os.Lstat(f.rollback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback copy remains: %v", err)
	}
	if _, found, err := f.repo.GetInstallation(f.ctx, f.request.MediaID, "en"); err != nil || found {
		t.Fatalf("installation found/error = %v/%v", found, err)
	}
	entries, err := f.repo.ListHistory(f.ctx, store.HistoryQuery{})
	if err != nil || len(entries) != 1 || entries[0].Outcome != "removed" || !entries[0].Manual || entries[0].ProviderID != "provider" {
		t.Fatalf("history = %#v/%v", entries, err)
	}
	leases, err := f.repo.LeaseDueNotifications(f.ctx, time.Now().Add(time.Minute), 10, time.Minute)
	if err != nil || len(leases) != 1 || leases[0].Notifier != "silo" || !strings.Contains(string(leases[0].PayloadJSON), "Movie.en.srt") {
		t.Fatalf("notifications = %#v/%v", leases, err)
	}
	if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
		t.Fatalf("%d pending rows left", n)
	}
	f.assertNoRemovalLeftovers(t)
}

func TestRemoveManagedRefusesAnEditedSubtitle(t *testing.T) {
	f := newRemovalFixture(t)
	edited := "1\n00:00:01,000 --> 00:00:02,000\nMy own fix\n"
	writeInstallFile(t, f.installation.Path, edited)
	err := f.installer.RemoveManaged(f.ctx, f.removal())
	if !errors.Is(err, ErrManagedSubtitleChanged) {
		t.Fatalf("RemoveManaged() error = %v, want ErrManagedSubtitleChanged", err)
	}
	f.assertUntouched(t, edited)
}

func TestRemoveManagedRefusesOutsideMediaRoots(t *testing.T) {
	f := newRemovalFixture(t)
	f.installer.MediaRoots = []string{t.TempDir()}
	if err := f.installer.RemoveManaged(f.ctx, f.removal()); err == nil {
		t.Fatal("removal outside the media roots succeeded")
	}
	f.assertUntouched(t, strings.Replace(installSRT, "Hello", "Replacement", 1))
}

func TestRemoveManagedRestoresSubtitleWhenTheCommitFails(t *testing.T) {
	f := newRemovalFixture(t)
	if _, err := f.audit.Exec(`CREATE TRIGGER fail_removal BEFORE INSERT ON events WHEN NEW.event_type='subtitle_removed' BEGIN SELECT RAISE(ABORT, 'injected removal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.installer.RemoveManaged(f.ctx, f.removal()); err == nil || !strings.Contains(err.Error(), "injected removal failure") {
		t.Fatalf("RemoveManaged() error = %v", err)
	}
	f.assertUntouched(t, strings.Replace(installSRT, "Hello", "Replacement", 1))
}

func TestRemoveManagedChangesNothingForACanceledJob(t *testing.T) {
	f := newRemovalFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if err := f.installer.RemoveManaged(ctx, f.removal()); !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveManaged() error = %v", err)
	}
	f.assertUntouched(t, strings.Replace(installSRT, "Hello", "Replacement", 1))
}

func TestRecoverPendingRemoval(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted", true: "committed"}[committed], func(t *testing.T) {
			f := newRemovalFixture(t)
			// A crash after the subtitle was moved aside, before or after the
			// record was deleted.
			f.installer.Fault = func(stage InstallStage) error {
				if stage == StageDatabase && !committed {
					panic(errSimulatedCrash)
				}
				if stage == StageCleanup && committed {
					panic(errSimulatedCrash)
				}
				return nil
			}
			func() {
				defer func() {
					if recovered := recover(); recovered != errSimulatedCrash {
						panic(recovered)
					}
				}()
				_ = f.installer.RemoveManaged(f.ctx, f.removal())
			}()
			f.installer.Fault = nil
			if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 1 {
				t.Fatalf("pending rows after the crash = %d", n)
			}
			settled, err := f.installer.RecoverPendingInstalls(f.ctx)
			if err != nil || settled != 1 {
				t.Fatalf("RecoverPendingInstalls() = %d/%v", settled, err)
			}
			if n := f.count(t, `SELECT count(*) FROM pending_installs`); n != 0 {
				t.Fatalf("pending rows after recovery = %d", n)
			}
			f.assertNoRemovalLeftovers(t)
			_, statErr := os.Lstat(f.installation.Path)
			if committed {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("removed subtitle came back: %v", statErr)
				}
				return
			}
			if payload, err := os.ReadFile(f.installation.Path); err != nil || string(payload) != strings.Replace(installSRT, "Hello", "Replacement", 1) {
				t.Fatalf("restored subtitle = %q/%v", payload, err)
			}
		})
	}
}

var errSimulatedCrash = errors.New("simulated crash")
