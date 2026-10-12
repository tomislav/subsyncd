package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

type rollbackFixture struct {
	ctx       context.Context
	root      string
	repo      *store.Repository
	audit     *sql.DB
	installer Installer
	logs      *bytes.Buffer
	request   InstallRequest
}

// newRollbackFixture installs a managed subtitle through the real installer
// and store.
func newRollbackFixture(t *testing.T) *rollbackFixture {
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
	repo := database.Repository()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
	request.Media.EntityID = 1
	request.Media.Title = "Movie"
	if request.MediaID, _, err = repo.UpsertMedia(ctx, request.Media); err != nil {
		t.Fatal(err)
	}
	installer := Installer{Repository: repo, MediaRoots: []string{root}, Events: events}
	if _, err := installer.Install(ctx, request); err != nil {
		t.Fatal(err)
	}
	return &rollbackFixture{ctx: ctx, root: root, repo: repo, audit: audit, installer: installer, logs: logs, request: request}
}

// replace installs a different subtitle over the managed one.
func (f *rollbackFixture) replace(t *testing.T, installer Installer, text string) store.Installation {
	t.Helper()
	request := f.request
	request.SourcePath = writeInstallFile(t, filepath.Join(t.TempDir(), "replacement.srt"), strings.Replace(installSRT, "Hello", text, 1))
	installation, err := installer.Install(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return installation
}

func (f *rollbackFixture) rollbackFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.root, rollbackPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func (f *rollbackFixture) recorded(t *testing.T) store.Installation {
	t.Helper()
	installation, found, err := f.repo.GetInstallation(f.ctx, f.request.MediaID, "en")
	if err != nil || !found {
		t.Fatalf("GetInstallation() = %#v/%v/%v", installation, found, err)
	}
	return installation
}

// keepingRollback is the installer with the post-install deletion of the
// rollback copy failing, which leaves the copy recorded, as older builds did.
func keepingRollback(installer Installer) Installer {
	installer.Fault = func(stage InstallStage) error {
		if stage == StageCleanup {
			return errors.New("injected cleanup failure")
		}
		return nil
	}
	return installer
}

func TestReplacementDeletesItsRollbackCopyOnceRecorded(t *testing.T) {
	f := newRollbackFixture(t)
	installation := f.replace(t, f.installer, "Replacement")
	if installation.RollbackPath != "" {
		t.Fatalf("returned rollback path = %q", installation.RollbackPath)
	}
	if got := f.recorded(t); got.RollbackPath != "" {
		t.Fatalf("recorded rollback path = %q", got.RollbackPath)
	}
	if files := f.rollbackFiles(t); len(files) != 0 {
		t.Fatalf("rollback copies left: %v", files)
	}
	assertFile(t, installation.Path, strings.Replace(installSRT, "Hello", "Replacement", 1))
}

func TestFailedRollbackDeletionKeepsTheInstallAndIsSweptLater(t *testing.T) {
	f := newRollbackFixture(t)
	installation := f.replace(t, keepingRollback(f.installer), "Replacement")
	if installation.RollbackPath == "" || !regularFile(installation.RollbackPath) || f.recorded(t).RollbackPath != installation.RollbackPath {
		t.Fatalf("installation = %#v; want the copy kept and recorded", installation)
	}
	if !strings.Contains(f.logs.String(), `"event":"install.rollback_cleanup_failed"`) {
		t.Fatalf("no install.rollback_cleanup_failed warning: %s", f.logs.String())
	}
	sweep, err := f.installer.DiscardRollbackCopies(f.ctx)
	if err != nil || sweep != (RollbackSweep{Removed: 1}) {
		t.Fatalf("DiscardRollbackCopies() = %+v/%v", sweep, err)
	}
	if files := f.rollbackFiles(t); len(files) != 0 {
		t.Fatalf("rollback copies left: %v", files)
	}
	if got := f.recorded(t); got.RollbackPath != "" {
		t.Fatalf("recorded rollback path = %q", got.RollbackPath)
	}
	// Idempotent: nothing is left to do.
	if sweep, err := f.installer.DiscardRollbackCopies(f.ctx); err != nil || sweep != (RollbackSweep{}) {
		t.Fatalf("second DiscardRollbackCopies() = %+v/%v", sweep, err)
	}
}

func TestRollbackSweepForgetsACopyThatIsAlreadyGone(t *testing.T) {
	f := newRollbackFixture(t)
	installation := f.replace(t, keepingRollback(f.installer), "Replacement")
	if err := os.Remove(installation.RollbackPath); err != nil {
		t.Fatal(err)
	}
	if sweep, err := f.installer.DiscardRollbackCopies(f.ctx); err != nil || sweep != (RollbackSweep{Removed: 1}) {
		t.Fatalf("DiscardRollbackCopies() = %+v/%v", sweep, err)
	}
	if got := f.recorded(t); got.RollbackPath != "" {
		t.Fatalf("recorded rollback path = %q", got.RollbackPath)
	}
}

func TestRollbackSweepSkipsCopiesItCannotVouchFor(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, f *rollbackFixture, installation store.Installation) string
	}{
		{"pending install", func(t *testing.T, f *rollbackFixture, installation store.Installation) string {
			if _, err := f.repo.BeginPendingInstall(f.ctx, store.PendingInstall{MediaID: installation.MediaID, Language: "en", Destination: installation.Path, Checksum: installation.Checksum}); err != nil {
				t.Fatal(err)
			}
			return installation.RollbackPath
		}},
		{"sidecar missing", func(t *testing.T, f *rollbackFixture, installation store.Installation) string {
			if err := os.Remove(installation.Path); err != nil {
				t.Fatal(err)
			}
			return installation.RollbackPath
		}},
		{"another folder", func(t *testing.T, f *rollbackFixture, installation store.Installation) string {
			other := filepath.Join(f.root, "Other")
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(other, filepath.Base(installation.RollbackPath))
			if err := os.Rename(installation.RollbackPath, moved); err != nil {
				t.Fatal(err)
			}
			if _, err := f.audit.Exec(`UPDATE installations SET rollback_path=?`, moved); err != nil {
				t.Fatal(err)
			}
			return moved
		}},
		{"another name", func(t *testing.T, f *rollbackFixture, installation store.Installation) string {
			renamed := filepath.Join(f.root, "Movie.hr.srt")
			if err := os.Rename(installation.RollbackPath, renamed); err != nil {
				t.Fatal(err)
			}
			if _, err := f.audit.Exec(`UPDATE installations SET rollback_path=?`, renamed); err != nil {
				t.Fatal(err)
			}
			return renamed
		}},
		{"symlink", func(t *testing.T, f *rollbackFixture, installation store.Installation) string {
			target := filepath.Join(f.root, "target.srt")
			if err := os.Rename(installation.RollbackPath, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, installation.RollbackPath); err != nil {
				t.Fatal(err)
			}
			return installation.RollbackPath
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRollbackFixture(t)
			installation := f.replace(t, keepingRollback(f.installer), "Replacement")
			kept := test.setup(t, f, installation)
			recorded := f.recorded(t).RollbackPath
			sweep, err := f.installer.DiscardRollbackCopies(f.ctx)
			if err != nil || sweep != (RollbackSweep{Skipped: 1}) {
				t.Fatalf("DiscardRollbackCopies() = %+v/%v", sweep, err)
			}
			if _, err := os.Lstat(kept); err != nil {
				t.Fatalf("skipped copy was touched: %v", err)
			}
			if got := f.recorded(t); got.RollbackPath != recorded {
				t.Fatalf("recorded rollback path = %q, want %q", got.RollbackPath, recorded)
			}
		})
	}
}

func TestAbandonedRollbackSweepChangesNothing(t *testing.T) {
	f := newRollbackFixture(t)
	installation := f.replace(t, keepingRollback(f.installer), "Replacement")
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.installer.DiscardRollbackCopies(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscardRollbackCopies() error = %v", err)
	}
	if !regularFile(installation.RollbackPath) || f.recorded(t).RollbackPath != installation.RollbackPath {
		t.Fatal("an abandoned sweep changed the rollback copy or its record")
	}
}

func TestRecoveryKeepsARecordedInstallsRollbackCopyForTheSweep(t *testing.T) {
	// A crash after the install was recorded and before its copy was deleted:
	// recovery keeps the copy as before, and the sweep that follows deletes it.
	f := newRollbackFixture(t)
	installation := f.replace(t, keepingRollback(f.installer), "Replacement")
	if _, err := f.repo.BeginPendingInstall(f.ctx, store.PendingInstall{MediaID: installation.MediaID, Language: "en", Destination: installation.Path, Checksum: installation.Checksum, RollbackPath: installation.RollbackPath}); err != nil {
		t.Fatal(err)
	}
	if settled, err := f.installer.RecoverPendingInstalls(f.ctx); err != nil || settled != 1 {
		t.Fatalf("RecoverPendingInstalls() = %d/%v", settled, err)
	}
	if !regularFile(installation.RollbackPath) {
		t.Fatal("recovery deleted a recorded install's rollback copy")
	}
	if sweep, err := f.installer.DiscardRollbackCopies(f.ctx); err != nil || sweep != (RollbackSweep{Removed: 1}) {
		t.Fatalf("DiscardRollbackCopies() = %+v/%v", sweep, err)
	}
	if files := f.rollbackFiles(t); len(files) != 0 {
		t.Fatalf("rollback copies left: %v", files)
	}
}
