package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

const installSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"

func TestInstallerCreatesAtomicRecordedSidecar(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
	destination := filepath.Join(root, "Movie.en.srt")
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Mode: 0o640}
	request := installRequest(source, destination)
	installed, err := installer.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(destination)
	if err != nil || string(payload) != installSRT {
		t.Fatalf("installed payload = %q, %v", payload, err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("installed mode = %v, %v", info.Mode(), err)
	}
	if repository.recorded.Path != destination || repository.recorded.Checksum == "" || repository.recorded.MediaPath != request.Media.Fingerprint.Path || installed.Checksum != repository.recorded.Checksum {
		t.Fatalf("recorded installation = %#v", repository.recorded)
	}
	children, err := os.ReadDir(root)
	if err != nil || len(children) != 1 || strings.HasPrefix(children[0].Name(), ".subsyncd-") {
		t.Fatalf("destination directory contains temporary files: %#v, %v", children, err)
	}
}

func TestInstallerProtectsUnmanagedSymlinkAndModifiedManagedFiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, destination string, repository *installationRepository)
	}{
		{"unmanaged", func(t *testing.T, destination string, _ *installationRepository) {
			writeInstallFile(t, destination, "user file")
		}},
		{"symlink", func(t *testing.T, destination string, _ *installationRepository) {
			target := writeInstallFile(t, filepath.Join(t.TempDir(), "target.srt"), installSRT)
			if err := os.Symlink(target, destination); err != nil {
				t.Fatal(err)
			}
		}},
		{"modified managed", func(t *testing.T, destination string, repository *installationRepository) {
			writeInstallFile(t, destination, "locally changed")
			repository.installation = store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(installSRT))}
			repository.found = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
			destination := filepath.Join(root, "Movie.en.srt")
			repository := &installationRepository{}
			test.prepare(t, destination, repository)
			installer := Installer{Repository: repository, MediaRoots: []string{root}}
			_, err := installer.Install(context.Background(), installRequest(source, destination))
			if !errors.Is(err, ErrProtectedSubtitle) {
				t.Fatalf("Install() error = %T %v", err, err)
			}
			if repository.recordCalls != 0 {
				t.Fatal("protected file was recorded as replaced")
			}
		})
	}
}

func TestInstallerRestoresManagedSubtitleAcrossFaults(t *testing.T) {
	stages := []InstallStage{StageCreate, StageWrite, StageFileSync, StageChmod, StageRename, StageDirectorySync, StageDatabase}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			root := t.TempDir()
			destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
			if err := os.Chmod(destination, 0o640); err != nil {
				t.Fatal(err)
			}
			oldChecksum := checksumBytes([]byte(installSRT))
			repository := &installationRepository{found: true, installation: store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: oldChecksum}}
			newSubtitle := strings.Replace(installSRT, "Hello", "Replacement", 1)
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), newSubtitle)
			installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(current InstallStage) error {
				if current == stage {
					return errors.New("injected " + string(stage))
				}
				return nil
			}}
			if _, err := installer.Install(context.Background(), installRequest(source, destination)); err == nil {
				t.Fatal("Install() error = nil")
			}
			payload, err := os.ReadFile(destination)
			if err != nil || string(payload) != installSRT {
				t.Fatalf("previous subtitle was not restored: %q, %v", payload, err)
			}
			info, err := os.Stat(destination)
			if err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("restored mode = %v, %v", info.Mode(), err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".subsyncd-stage-") || strings.HasPrefix(entry.Name(), ".subsyncd-rollback-") {
					t.Fatalf("partial file remains after failure: %s", entry.Name())
				}
			}
		})
	}
}

func TestInstallerCleanupFailureDoesNotUndoCommittedReplacement(t *testing.T) {
	root := t.TempDir()
	destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
	oldRollback := writeInstallFile(t, filepath.Join(root, ".subsyncd-rollback-old"), "older")
	repository := &installationRepository{found: true, installation: store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(installSRT)), RollbackPath: oldRollback}}
	newSubtitle := strings.Replace(installSRT, "Hello", "Replacement", 1)
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), newSubtitle)
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
		if stage == StageCleanup {
			return errors.New("cleanup unavailable")
		}
		return nil
	}}
	if _, err := installer.Install(context.Background(), installRequest(source, destination)); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(destination)
	if err != nil || string(payload) != newSubtitle {
		t.Fatalf("committed subtitle = %q, %v", payload, err)
	}
	if _, err := os.Stat(oldRollback); err != nil {
		t.Fatalf("old rollback should remain after cleanup failure: %v", err)
	}
}

func TestInstallerRejectsOutsideRootAndCuePastMediaDuration(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), "1\n00:20:00,000 --> 00:20:01,000\nLate\n")
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root}}
	request := installRequest(source, filepath.Join(t.TempDir(), "outside.srt"))
	if _, err := installer.Install(context.Background(), request); err == nil {
		t.Fatal("outside-root Install() error = nil")
	}
	request.DestinationPath = filepath.Join(root, "Movie.en.srt")
	request.Media.Duration = 10 * time.Minute
	if _, err := installer.Install(context.Background(), request); err == nil {
		t.Fatal("late-cue Install() error = nil")
	}
}

func installRequest(source, destination string) InstallRequest {
	mediaPath := filepath.Join(filepath.Dir(destination), "Movie.mkv")
	return InstallRequest{MediaID: 1, Media: domain.Media{Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: 100, ModTime: time.Unix(10, 20)}, Duration: 90 * time.Minute}, Language: "en", SourcePath: source, DestinationPath: destination, Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate"}, Score: domain.Score{Total: 70}, SyncResult: domain.SyncResult{Verdict: "solid"}}
}

func writeInstallFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type installationRepository struct {
	installation store.Installation
	found        bool
	recorded     store.Installation
	recordCalls  int
	recordErr    error
}

func (r *installationRepository) GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error) {
	return r.installation, r.found, nil
}

func (r *installationRepository) RecordInstallation(_ context.Context, installation store.Installation) error {
	r.recordCalls++
	if r.recordErr != nil {
		return r.recordErr
	}
	r.recorded = installation
	r.installation = installation
	r.found = true
	return nil
}
