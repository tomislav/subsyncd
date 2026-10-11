package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/store"
)

// pendingRepository is the fake installation store plus the pending-install
// records the real store keeps.
type pendingRepository struct {
	*installationRepository
	pending map[int64]store.PendingInstall
	nextID  int64
}

func newPendingRepository(installations *installationRepository) *pendingRepository {
	return &pendingRepository{installationRepository: installations, pending: map[int64]store.PendingInstall{}}
}

func (r *pendingRepository) BeginPendingInstall(_ context.Context, pending store.PendingInstall) (int64, error) {
	r.nextID++
	pending.ID = r.nextID
	r.pending[pending.ID] = pending
	return pending.ID, nil
}

func (r *pendingRepository) FinishPendingInstall(_ context.Context, id int64) error {
	delete(r.pending, id)
	return nil
}

func (r *pendingRepository) ListPendingInstalls(context.Context) ([]store.PendingInstall, error) {
	var items []store.PendingInstall
	for _, item := range r.pending {
		items = append(items, item)
	}
	return items, nil
}

func TestInstallClearsItsPendingRecord(t *testing.T) {
	for _, fail := range []bool{false, true} {
		root := t.TempDir()
		destination := filepath.Join(root, "Movie.en.srt")
		source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
		repository := newPendingRepository(&installationRepository{})
		installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
			if fail && stage == StageDatabase {
				return errors.New("database unavailable")
			}
			return nil
		}}
		_, err := installer.Install(context.Background(), installRequest(t, source, destination))
		if (err != nil) != fail {
			t.Fatalf("Install() error = %v", err)
		}
		if len(repository.pending) != 0 {
			t.Fatalf("fail=%t left pending installs: %+v", fail, repository.pending)
		}
	}
}

func TestInstallLeavesPendingRecordWhenItCannotUndo(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "Movie.en.srt")
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
	repository := newPendingRepository(&installationRepository{})
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
		if stage != StageDatabase {
			return nil
		}
		lockDirectory(t, root)
		return errors.New("database unavailable")
	}}
	if _, err := installer.Install(context.Background(), installRequest(t, source, destination)); err == nil {
		t.Fatal("Install() succeeded")
	}
	if len(repository.pending) != 1 {
		t.Fatalf("pending installs = %+v, want the unrecorded publication kept for the startup sweep", repository.pending)
	}
}

func TestRecoverPendingInstalls(t *testing.T) {
	const previous = "1\n00:00:01,000 --> 00:00:02,000\nPrevious\n"
	for _, test := range []struct {
		name        string
		replacing   bool
		recorded    bool
		wantContent string // "" means the destination is removed
		wantOutcome string
	}{
		{name: "unrecorded first install is removed", wantOutcome: "undone"},
		{name: "unrecorded replacement restores the previous subtitle", replacing: true, wantContent: previous, wantOutcome: "undone"},
		{name: "recorded install is kept", replacing: true, recorded: true, wantContent: installSRT, wantOutcome: "recorded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
			staged := writeInstallFile(t, filepath.Join(root, ".subsyncd-stage-1"), installSRT)
			installations := &installationRepository{}
			pending := store.PendingInstall{MediaID: 1, Language: "en", Destination: destination, Checksum: checksumBytes([]byte(installSRT)), StagedPath: staged}
			if test.replacing {
				pending.RollbackPath = writeInstallFile(t, filepath.Join(root, ".subsyncd-rollback-1"), previous)
				installations.found = true
				installations.installation = store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(previous))}
			}
			if test.recorded {
				installations.installation.Checksum = pending.Checksum
				installations.installation.RollbackPath = pending.RollbackPath
			}
			repository := newPendingRepository(installations)
			if _, err := repository.BeginPendingInstall(context.Background(), pending); err != nil {
				t.Fatal(err)
			}
			installer := Installer{Repository: repository, MediaRoots: []string{root}, Now: func() time.Time { return time.Unix(0, 0) }}
			settled, err := installer.RecoverPendingInstalls(context.Background())
			if err != nil || settled != 1 || len(repository.pending) != 0 {
				t.Fatalf("settled %d, err %v, pending %+v", settled, err, repository.pending)
			}
			payload, err := os.ReadFile(destination)
			if test.wantContent == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unrecorded subtitle remains: %q/%v", payload, err)
				}
			} else if err != nil || string(payload) != test.wantContent {
				t.Fatalf("destination = %q/%v, want %q", payload, err, test.wantContent)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				name := entry.Name()
				if strings.HasPrefix(name, ".subsyncd-stage-") || (!test.recorded && strings.HasPrefix(name, ".subsyncd-rollback-")) {
					t.Fatalf("%s remains after recovery", name)
				}
			}
			if test.recorded {
				if _, err := os.Stat(pending.RollbackPath); err != nil {
					t.Fatalf("recorded install lost its rollback copy: %v", err)
				}
			}
		})
	}
}

func TestRollbackCleanupErrorNamesTheRollbackCopy(t *testing.T) {
	root := t.TempDir()
	destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
	repository := &installationRepository{found: true, installation: store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(installSRT))}}
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), strings.Replace(installSRT, "Hello", "Replacement", 1))
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
		if stage != StageRename {
			return nil
		}
		// Make the unused rollback copy impossible to remove.
		matches, _ := filepath.Glob(filepath.Join(root, ".subsyncd-rollback-*"))
		for _, match := range matches {
			if err := os.Remove(match); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(match, "keep"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return errors.New("rename refused")
	}}
	_, err := installer.Install(context.Background(), installRequest(t, source, destination))
	if err == nil || !strings.Contains(err.Error(), "remove rollback copy of previous subtitle") || strings.Contains(err.Error(), "unused") {
		t.Fatalf("Install() error = %v", err)
	}
}
