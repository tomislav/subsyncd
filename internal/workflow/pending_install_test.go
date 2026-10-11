package workflow

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/observability"
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
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range repository.pending {
		if item.PublishedInode == 0 || item.PublishedInode != fileInode(info) {
			t.Fatalf("pending inode = %d, want the published file's %d", item.PublishedInode, fileInode(info))
		}
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

func TestRecoverPendingInstallComparesFileIdentity(t *testing.T) {
	const previous = "1\n00:00:01,000 --> 00:00:02,000\nPrevious\n"
	for _, test := range []struct {
		name        string
		replaced    bool // another run published the same text since
		wantContent string
		wantOutcome string
	}{
		{name: "the published file is undone", wantContent: previous, wantOutcome: "undone"},
		{name: "a later file with the same text is kept", replaced: true, wantContent: installSRT, wantOutcome: "cleaned"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
			info, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			rollback := writeInstallFile(t, filepath.Join(root, ".subsyncd-rollback-1"), previous)
			installations := &installationRepository{found: true, installation: store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(previous))}}
			repository := newPendingRepository(installations)
			if _, err := repository.BeginPendingInstall(context.Background(), store.PendingInstall{MediaID: 1, Language: "en", Destination: destination, Checksum: checksumBytes([]byte(installSRT)), RollbackPath: rollback, PublishedInode: fileInode(info)}); err != nil {
				t.Fatal(err)
			}
			if test.replaced {
				publishOver(t, destination, installSRT)
			}
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info"})
			if err != nil {
				t.Fatal(err)
			}
			installer := Installer{Repository: repository, MediaRoots: []string{root}, Events: events}
			if settled, err := installer.RecoverPendingInstalls(context.Background()); err != nil || settled != 1 {
				t.Fatalf("RecoverPendingInstalls() = %d, %v", settled, err)
			}
			if !strings.Contains(logs.String(), `"outcome":"`+test.wantOutcome+`"`) {
				t.Fatalf("logs = %s, want outcome %s", logs.String(), test.wantOutcome)
			}
			if payload, err := os.ReadFile(destination); err != nil || string(payload) != test.wantContent {
				t.Fatalf("destination = %q, %v; want %q", payload, err, test.wantContent)
			}
			if _, err := os.Lstat(rollback); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback copy remains: %v", err)
			}
		})
	}
}

func TestRecoverPendingInstallKeepsRowWhenDestinationIsUninspectable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
	repository := newPendingRepository(&installationRepository{})
	if _, err := repository.BeginPendingInstall(context.Background(), store.PendingInstall{MediaID: 1, Language: "en", Destination: destination, Checksum: checksumBytes([]byte(installSRT))}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	installer := Installer{Repository: repository, MediaRoots: []string{root}}
	if settled, err := installer.RecoverPendingInstalls(context.Background()); err == nil || settled != 0 {
		t.Fatalf("RecoverPendingInstalls() = %d, %v; want an error", settled, err)
	}
	if len(repository.pending) != 1 {
		t.Fatalf("pending installs = %+v, want the row kept for the next start", repository.pending)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(destination); err != nil || string(payload) != installSRT {
		t.Fatalf("destination = %q, %v; want it left alone", payload, err)
	}
}
