package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

const installSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"

func TestInstallerRejectsMediaChangedBeforePublication(t *testing.T) {
	for _, change := range []string{"size", "mtime", "deleted", "symlink"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
			request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
			writeInstallFile(t, request.Media.Fingerprint.Path, "original media")
			info, err := os.Stat(request.Media.Fingerprint.Path)
			if err != nil {
				t.Fatal(err)
			}
			request.Media.Fingerprint.Size, request.Media.Fingerprint.ModTime = info.Size(), info.ModTime()
			repository := &installationRepository{}
			installer := Installer{Repository: repository, MediaRoots: []string{root}, NotifierNames: []string{"silo"}, Fault: func(stage InstallStage) error {
				if stage != StageRename {
					return nil
				}
				switch change {
				case "size":
					return os.WriteFile(request.Media.Fingerprint.Path, []byte("replacement media with different bytes"), 0o600)
				case "mtime":
					changed := info.ModTime().Add(time.Minute)
					return os.Chtimes(request.Media.Fingerprint.Path, changed, changed)
				case "deleted":
					return os.Remove(request.Media.Fingerprint.Path)
				case "symlink":
					target := filepath.Join(t.TempDir(), "replacement.mkv")
					if err := os.Rename(request.Media.Fingerprint.Path, target); err != nil {
						return err
					}
					return os.Symlink(target, request.Media.Fingerprint.Path)
				}
				return nil
			}}
			_, err = installer.Install(context.Background(), request)
			var content *subtitleValidationError
			if err == nil || errors.As(err, &content) {
				t.Fatalf("media change must fail technically: %v", err)
			}
			if repository.recordCalls != 0 || len(repository.requests) != 0 {
				t.Fatal("stale installation reached provenance/outbox commit")
			}
			if _, err := os.Stat(request.DestinationPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale subtitle remains: %v", err)
			}
		})
	}
}

func TestInstallerAcceptsUnchangedMediaSymlinkWithinRoot(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
	target := filepath.Join(root, "target.mkv")
	if err := os.Rename(request.Media.Fingerprint.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, request.Media.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root}}
	if _, err := installer.Install(context.Background(), request); err != nil {
		t.Fatalf("unchanged contained media symlink rejected: %v", err)
	}
	if repository.recordCalls != 1 {
		t.Fatal("installation not recorded")
	}
}

func TestInstallerRollsBackWhenMediaRowChangesBeforeCommit(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		t.Run(fmt.Sprint(replacing), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			dbPath := filepath.Join(t.TempDir(), "subsyncd.db")
			database, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			audit, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer audit.Close()
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
			request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
			request.Media.EntityID = 1
			request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			installer := Installer{Repository: database.Repository(), MediaRoots: []string{root}}
			var previous store.Installation
			if replacing {
				previous, err = installer.Install(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				request.SourcePath = writeInstallFile(t, filepath.Join(t.TempDir(), "replacement.srt"), strings.Replace(installSRT, "Hello", "Replacement", 1))
			}
			installer.NotifierNames = []string{"silo"}
			installer.Fault = func(stage InstallStage) error {
				if stage != StageDatabase {
					return nil
				}
				_, err := audit.Exec(`UPDATE media SET file_id=file_id+1 WHERE id=?`, request.MediaID)
				return err
			}
			_, err = installer.Install(ctx, request)
			var content *subtitleValidationError
			if err == nil || errors.As(err, &content) || !strings.Contains(err.Error(), "media changed") {
				t.Fatalf("stale media must fail technically: %v", err)
			}
			got, found, err := database.Repository().GetInstallation(ctx, request.MediaID, "en")
			if err != nil || found != replacing {
				t.Fatalf("installation found/error = %v/%v", found, err)
			}
			if replacing {
				payload, err := os.ReadFile(request.DestinationPath)
				if err != nil || string(payload) != installSRT || got.Checksum != previous.Checksum {
					t.Fatalf("prior sidecar/provenance not restored: %q/%v/%+v", payload, err, got)
				}
			} else if _, err := os.Stat(request.DestinationPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale sidecar remains: %v", err)
			}
			for _, table := range []string{"notifications", "candidate_rejections", "events"} {
				var count int
				want := 0
				if table == "events" && replacing {
					want = 1
				}
				if err := audit.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count/error = %d/%v", table, count, err)
				}
			}
		})
	}
}

func TestInstallSourceContentFailuresAreCandidateRejections(t *testing.T) {
	for _, test := range []struct{ name, filename, payload string }{
		{"empty", "source.srt", ""},
		{"binary", "source.srt", "\x00" + installSRT},
		{"syntax", "source.srt", "not subtitles"},
		{"timestamps", "source.srt", "1\n00:00:02,000 --> 00:00:01,000\nHello\n"},
		{"duration", "source.srt", "1\n00:00:01,000 --> 01:36:00,000\nHello\n"},
		{"extension", "source.txt", installSRT},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := serviceRequest(t)
			path := writeInstallFile(t, filepath.Join(t.TempDir(), test.filename), test.payload)
			_, failure := validatedSubtitle(path, request.Media.Duration)
			service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
			recorded, err := service.recordCandidateRejection(context.Background(), request, exactCandidate("invalid"), "", failure)
			if failure == nil || err != nil || !recorded {
				t.Fatalf("content failure/rejection/error = %v/%v/%v", failure, recorded, err)
			}
		})
	}
}

func TestInstallerRollsBackPublishedFileWhenNotificationIntentCommitFails(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		t.Run(fmt.Sprint(replacing), func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "Movie.en.srt")
			repository := &installationRepository{recordErr: errors.New("outbox unavailable")}
			if replacing {
				writeInstallFile(t, destination, installSRT)
				repository.found = true
				repository.installation = store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(installSRT))}
			}
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), strings.Replace(installSRT, "Hello", "Replacement", 1))
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info"})
			if err != nil {
				t.Fatal(err)
			}
			installer := Installer{Repository: repository, MediaRoots: []string{root}, NotifierNames: []string{"silo"}, Now: func() time.Time { return time.Unix(100, 0).UTC() }, Events: events}
			_, err = installer.Install(context.Background(), installRequest(t, source, destination))
			if err == nil || !strings.Contains(err.Error(), "outbox unavailable") {
				t.Fatalf("Install() error = %v", err)
			}
			if len(repository.requests) != 1 {
				t.Fatalf("atomic requests = %#v", repository.requests)
			}
			if replacing {
				payload, err := os.ReadFile(destination)
				if err != nil || string(payload) != installSRT {
					t.Fatalf("restored file = %q, %v", payload, err)
				}
			} else if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination stat = %v", err)
			}
			if strings.Contains(logs.String(), "notification.queued") {
				t.Fatalf("uncommitted notification logged: %s", logs.String())
			}
		})
	}
}

func TestInstallerCreatesDeterministicNotificationIntents(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "Movie.en.srt")
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	repository := &installationRepository{dedupedNotifiers: map[string]bool{"archive": true}}
	now := time.Unix(100, 0).UTC()
	names := []string{"silo", "archive"}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	installer := Installer{Repository: repository, MediaRoots: []string{root}, NotifierNames: names, Now: func() time.Time { return now }, Events: events}
	request := installRequest(t, source, destination)
	installed, err := installer.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.requests) != 2 || repository.requests[0].Notifier != "archive" || repository.requests[1].Notifier != "silo" || names[0] != "silo" {
		t.Fatalf("requests = %#v, input names = %#v", repository.requests, names)
	}
	for _, intent := range repository.requests {
		var payload NotificationPayload
		if err := json.Unmarshal(intent.PayloadJSON, &payload); err != nil {
			t.Fatal(err)
		}
		expectedPayload := NotificationPayload{Media: request.Media, SubtitlePath: destination}
		if !payload.Media.Fingerprint.ModTime.Equal(expectedPayload.Media.Fingerprint.ModTime) {
			t.Fatalf("payload modification time = %v, want instant %v", payload.Media.Fingerprint.ModTime, expectedPayload.Media.Fingerprint.ModTime)
		}
		payload.Media.Fingerprint.ModTime = time.Time{}
		expectedPayload.Media.Fingerprint.ModTime = time.Time{}
		if !reflect.DeepEqual(payload, expectedPayload) || !intent.NextAttemptAt.Equal(now) {
			t.Fatalf("payload/time = %#v/%v", payload, intent.NextAttemptAt)
		}
	}
	queued := workflowEvents(workflowLogRecords(t, logs.String()), "notification.queued")
	if len(queued) != 1 || queued[0]["notifier"] != "silo" {
		t.Fatalf("queue logs = %s", logs.String())
	}
	if strings.Contains(logs.String(), root) || strings.Contains(logs.String(), "subtitle_path") {
		t.Fatalf("payload leaked: %s", logs.String())
	}
	first := repository.requests
	// Time and destination changes must not defeat checksum deduplication.
	installed.Path = filepath.Join(root, "Renamed.en.srt")
	again, err := notificationRequests(request.Media, installed, names, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for index := range first {
		if first[index].DedupeKey != again[index].DedupeKey {
			t.Fatal("unchanged content changed dedupe key")
		}
	}
	for _, mutate := range []func(*store.Installation){func(i *store.Installation) { i.Checksum = "changed" }, func(i *store.Installation) { i.MediaID++ }, func(i *store.Installation) { i.Language = "hr" }} {
		changed := installed
		mutate(&changed)
		intents, err := notificationRequests(request.Media, changed, names, now)
		if err != nil || intents[0].DedupeKey == first[0].DedupeKey {
			t.Fatalf("changed identity intents/error = %#v/%v", intents, err)
		}
	}
	if first[0].DedupeKey == first[1].DedupeKey {
		t.Fatal("notifiers share a dedupe key")
	}
}

func TestInstallerOutboxSQLFailureRollsBackFileAndProvenance(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		t.Run(fmt.Sprint(replacing), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			dbPath := filepath.Join(t.TempDir(), "subsyncd.db")
			database, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			audit, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer audit.Close()
			source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
			request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
			request.Media.EntityID = 1
			request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			installer := Installer{Repository: database.Repository(), MediaRoots: []string{root}}
			var previous store.Installation
			if replacing {
				previous, err = installer.Install(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				request.SourcePath = writeInstallFile(t, filepath.Join(t.TempDir(), "replacement.srt"), strings.Replace(installSRT, "Hello", "Replacement", 1))
			}
			if _, err := audit.Exec(`CREATE TRIGGER fail_notification BEFORE INSERT ON notifications WHEN NEW.notifier='silo' BEGIN SELECT RAISE(ABORT, 'injected outbox failure'); END`); err != nil {
				t.Fatal(err)
			}
			// The first intent is inserted before the trigger rejects the second.
			installer.NotifierNames = []string{"archive", "silo"}
			if _, err := installer.Install(ctx, request); err == nil || !strings.Contains(err.Error(), "injected outbox failure") {
				t.Fatalf("install error = %v", err)
			}
			got, found, err := database.Repository().GetInstallation(ctx, request.MediaID, "en")
			if err != nil || found != replacing {
				t.Fatalf("provenance found/error = %v/%v", found, err)
			}
			if replacing {
				if got.Checksum != previous.Checksum {
					t.Fatalf("replaced original provenance: %#v", got)
				}
				if payload, err := os.ReadFile(request.DestinationPath); err != nil || string(payload) != installSRT {
					t.Fatalf("restored file = %q/%v", payload, err)
				}
			} else if _, err := os.Stat(request.DestinationPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("first-install file remains: %v", err)
			}
			var intents, events int
			if err := audit.QueryRow(`SELECT count(*) FROM notifications`).Scan(&intents); err != nil {
				t.Fatal(err)
			}
			if err := audit.QueryRow(`SELECT count(*) FROM events WHERE event_type='subtitle_installed'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			wantEvents := 0
			if replacing {
				wantEvents = 1
			}
			if intents != 0 || events != wantEvents {
				t.Fatalf("intents/events = %d/%d", intents, events)
			}
		})
	}
}

func TestInstallerCreatesAtomicRecordedSidecar(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
	destination := filepath.Join(root, "Movie.en.srt")
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Mode: 0o640}
	request := installRequest(t, source, destination)
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
	if err != nil || len(children) != 2 || children[0].Name() != "Movie.en.srt" || children[1].Name() != "Movie.mkv" {
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
			_, err := installer.Install(context.Background(), installRequest(t, source, destination))
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
			if _, err := installer.Install(context.Background(), installRequest(t, source, destination)); err == nil {
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

func TestInstallReportsFirstInstallRemovalFailure(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "Movie.en.srt")
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
		if stage != StageDatabase {
			return nil
		}
		if err := os.Remove(destination); err != nil {
			return err
		}
		if err := os.Mkdir(destination, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, "blocker"), []byte("x"), 0o600); err != nil {
			return err
		}
		return errors.New("database unavailable")
	}}
	_, err := installer.Install(context.Background(), installRequest(t, source, destination))
	if err == nil || !strings.Contains(err.Error(), "database unavailable") || !strings.Contains(err.Error(), "remove newly published subtitle") {
		t.Fatalf("Install() error = %v", err)
	}
	if repository.recordCalls != 0 {
		t.Fatalf("record calls = %d, want 0", repository.recordCalls)
	}
}

func TestInstallPreservesRollbackWhenRestoreFails(t *testing.T) {
	root := t.TempDir()
	destination := writeInstallFile(t, filepath.Join(root, "Movie.en.srt"), installSRT)
	repository := &installationRepository{found: true, installation: store.Installation{MediaID: 1, Language: "en", Path: destination, Checksum: checksumBytes([]byte(installSRT))}}
	replacement := strings.Replace(installSRT, "Hello", "Replacement", 1)
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), replacement)
	installer := Installer{Repository: repository, MediaRoots: []string{root}, Fault: func(stage InstallStage) error {
		if stage != StageDatabase {
			return nil
		}
		if err := os.Remove(destination); err != nil {
			return err
		}
		if err := os.Mkdir(destination, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, "blocker"), []byte("x"), 0o600); err != nil {
			return err
		}
		return errors.New("database unavailable")
	}}
	_, err := installer.Install(context.Background(), installRequest(t, source, destination))
	if err == nil || !strings.Contains(err.Error(), "database unavailable") || !strings.Contains(err.Error(), "restore previous subtitle") {
		t.Fatalf("Install() error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var rollback string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".subsyncd-rollback-") {
			rollback = filepath.Join(root, entry.Name())
		}
	}
	if rollback == "" {
		t.Fatal("last known-good rollback was removed")
	}
	payload, err := os.ReadFile(rollback)
	if err != nil || string(payload) != installSRT {
		t.Fatalf("rollback payload = %q, %v", payload, err)
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
	if _, err := installer.Install(context.Background(), installRequest(t, source, destination)); err != nil {
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
	request := installRequest(t, source, filepath.Join(t.TempDir(), "outside.srt"))
	if _, err := installer.Install(context.Background(), request); err == nil {
		t.Fatal("outside-root Install() error = nil")
	}
	request.DestinationPath = filepath.Join(root, "Movie.en.srt")
	request.Media.Duration = 10 * time.Minute
	if _, err := installer.Install(context.Background(), request); err == nil {
		t.Fatal("late-cue Install() error = nil")
	}
}

func installRequest(t *testing.T, source, destination string) InstallRequest {
	t.Helper()
	mediaPath := writeInstallFile(t, filepath.Join(filepath.Dir(destination), "Movie.mkv"), "media")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	return InstallRequest{MediaID: 1, Media: domain.Media{Ref: domain.MediaRef{FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}, Duration: 90 * time.Minute}, Language: "en", SourcePath: source, DestinationPath: destination, Candidate: domain.Candidate{ProviderID: "provider", ResultID: "candidate"}, Score: domain.Score{Total: 70}, SyncResult: domain.SyncResult{Verdict: "solid"}}
}

func writeInstallFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type installationRepository struct {
	installation     store.Installation
	found            bool
	recorded         store.Installation
	recordCalls      int
	recordErr        error
	requests         []store.NotificationRequest
	dedupedNotifiers map[string]bool
}

func (r *installationRepository) RecordInstallationWithNotifications(ctx context.Context, installation store.Installation, requests []store.NotificationRequest) ([]store.NotificationEnqueueResult, error) {
	r.requests = append([]store.NotificationRequest(nil), requests...)
	if err := r.RecordInstallation(ctx, installation); err != nil {
		return nil, err
	}
	results := make([]store.NotificationEnqueueResult, len(requests))
	for index, request := range requests {
		results[index] = store.NotificationEnqueueResult{Notifier: request.Notifier, DedupeKey: request.DedupeKey, Inserted: !r.dedupedNotifiers[request.Notifier]}
	}
	return results, nil
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
