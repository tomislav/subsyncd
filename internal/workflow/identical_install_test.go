package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func identicalInstallationFixture(t *testing.T) (Installer, InstallRequest, *store.Store, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), installSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.en.srt"))
	request.Media.Ref.Instance = "radarr"
	request.Media.Ref.Kind = domain.MediaMovie
	request.Media.EntityID = 1
	request.Fallback = true
	request.Candidate.ProviderID = "fallback"
	databasePath := filepath.Join(t.TempDir(), "state.db")
	database, err := store.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	request.MediaID, _, err = database.Repository().UpsertMedia(t.Context(), request.Media)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	installer := Installer{Repository: database.Repository(), MediaRoots: []string{root}, NotifierNames: []string{"silo"}}
	if _, err := installer.Install(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return installer, request, database, audit
}

func TestInstallerRefreshesIdenticalOwnedContentWithoutPublication(t *testing.T) {
	installer, request, database, audit := identicalInstallationFixture(t)
	existing, _, err := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
	if err != nil {
		t.Fatal(err)
	}
	rollback := writeInstallFile(t, filepath.Join(filepath.Dir(request.DestinationPath), ".subsyncd-rollback-retained"), "old version")
	existing.RollbackPath = rollback
	if err := database.Repository().RecordInstallation(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(request.DestinationPath)
	if err != nil {
		t.Fatal(err)
	}
	request.Fallback = false
	request.Candidate.ProviderID = "preferred"
	request.Candidate.ResultID = "better"
	request.Score = domain.Score{Total: 85}
	request.SyncResult = domain.SyncResult{Verdict: "solid", Confidence: 0.95}
	// A provenance refresh must never enter any file publication stage.
	installer.Fault = func(stage InstallStage) error {
		if stage != StageDatabase {
			return errors.New("unexpected filesystem publication")
		}
		return nil
	}
	got, err := installer.Install(t.Context(), request)
	if err != nil {
		t.Fatalf("identical promotion: %v", err)
	}
	persisted, found, err := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
	if err != nil || !found || !reflect.DeepEqual(got, persisted) {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if got.Fallback || got.ProviderID != "preferred" || got.CandidateID != "better" || got.RollbackPath != rollback || got.Checksum != existing.Checksum {
		t.Fatalf("provenance=%+v", got)
	}
	score, _ := json.Marshal(request.Score)
	syncResult, _ := json.Marshal(request.SyncResult)
	if string(got.ScoreJSON) != string(score) || string(got.SyncResultJSON) != string(syncResult) {
		t.Fatalf("assessment=%+v", got)
	}
	after, err := os.Stat(request.DestinationPath)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("identical subtitle was rewritten: %v", err)
	}
	if _, err := os.Stat(rollback); err != nil {
		t.Fatalf("rollback lost: %v", err)
	}
	var notifications int
	if err := audit.QueryRow(`SELECT count(*) FROM notifications`).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
}

func TestIdenticalRefreshPreservesGuardsAndDatabaseAtomicity(t *testing.T) {
	for _, scenario := range []string{"edited", "missing", "symlink", "media-file", "media-row", "deleted-row", "database", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			installer, request, database, audit := identicalInstallationFixture(t)
			previous, _, err := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
			if err != nil {
				t.Fatal(err)
			}
			request.Fallback = false
			request.Candidate.ProviderID = "preferred"
			reachedCommit := false
			installer.Fault = func(stage InstallStage) error {
				if stage != StageDatabase {
					return nil
				}
				reachedCommit = true
				switch scenario {
				case "edited":
					return os.WriteFile(request.DestinationPath, []byte("user edit"), 0600)
				case "missing":
					return os.Remove(request.DestinationPath)
				case "symlink":
					if err := os.Remove(request.DestinationPath); err != nil {
						return err
					}
					return os.Symlink(request.SourcePath, request.DestinationPath)
				case "media-file":
					return os.Chtimes(request.Media.Fingerprint.Path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
				case "media-row":
					_, err := audit.Exec(`UPDATE media SET file_id=file_id+1 WHERE id=?`, request.MediaID)
					return err
				case "deleted-row":
					_, err := audit.Exec(`UPDATE media SET deleted=1 WHERE id=?`, request.MediaID)
					return err
				case "database":
					_, err := audit.Exec(`CREATE TRIGGER fail_refresh BEFORE INSERT ON installations BEGIN SELECT RAISE(ABORT,'injected'); END`)
					return err
				}
				return nil
			}
			ctx := t.Context()
			if scenario == "canceled" {
				var cancel func()
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err = installer.Install(ctx, request)
			if err == nil {
				t.Fatal("refresh accepted changed content/media or failed transaction")
			}
			if scenario != "canceled" && !reachedCommit {
				t.Fatalf("did not exercise commit guard: %v", err)
			}
			got, found, readErr := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
			if readErr != nil || !found || !reflect.DeepEqual(previous, got) {
				t.Fatalf("failed refresh changed provenance: %+v, %v", got, readErr)
			}
			if scenario == "edited" {
				body, err := os.ReadFile(request.DestinationPath)
				if err != nil || string(body) != "user edit" {
					t.Fatalf("user edit rolled back: %q %v", body, err)
				}
			}
			var events, notifications int
			if err := audit.QueryRow(`SELECT count(*) FROM events WHERE event_type='subtitle_installed'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if err := audit.QueryRow(`SELECT count(*) FROM notifications`).Scan(&notifications); err != nil {
				t.Fatal(err)
			}
			if events != 1 || notifications != 1 {
				t.Fatalf("failed refresh committed events=%d notifications=%d", events, notifications)
			}
		})
	}
}

func TestWorkflowPromotesIdenticalContentWithRealInstaller(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			request := serviceRequest(t)
			request.Media.EntityID = 1
			database, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			request.MediaID, _, err = database.Repository().UpsertMedia(t.Context(), request.Media)
			if err != nil {
				t.Fatal(err)
			}
			preferred := &fakeSearcher{}
			fallback := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{provider.SearchExactHash: {Candidates: []domain.Candidate{fallbackCandidate("old", true)}}}}
			service, _ := tierService(t, preferred, fallback)
			service.Repository = database.Repository()
			service.Installer = Installer{Repository: database.Repository(), MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}}
			service.Providers["provider"].(*fakeProvider).payloads = map[string][]byte{"preferred": []byte(installSRT)}
			service.Providers["fallback"].(*fakeProvider).payloads = map[string][]byte{"old": []byte(installSRT)}
			first, err := service.Run(t.Context(), request)
			if err != nil || first.Outcome != OutcomeInstalled || !first.Installation.Fallback {
				t.Fatalf("initial fallback=%+v, %v", first, err)
			}
			before, err := os.Stat(first.Installation.Path)
			if err != nil {
				t.Fatal(err)
			}
			service.Inventory = &fakeInventory{current: managedSidecarInventory(first.Installation)}
			preferred.result = provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("preferred")}}
			preferred.result.Candidates[0].ExactHash = exact
			fallbackCalls := fallback.calls
			result, err := service.Run(t.Context(), request)
			if err != nil || result.Outcome != OutcomeInstalled || result.Installation.Fallback || result.Installation.ProviderID != "provider" {
				t.Fatalf("promotion=%+v, %v", result, err)
			}
			if result.Installation.Checksum != first.Installation.Checksum || fallback.calls != fallbackCalls {
				t.Fatal("changed bytes or searched fallback after promotion")
			}
			after, err := os.Stat(first.Installation.Path)
			if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("promotion rewrote file: %v", err)
			}
			wantSync := 1
			if exact {
				wantSync = 0
			}
			if service.Synchronizer.(*fakeSynchronizer).synchronizeCalls != wantSync {
				t.Fatal("promotion violated synchronization policy")
			}
			if exact != result.NextUpgrade.IsZero() {
				t.Fatalf("promotion schedule=%v exact=%v", result.NextUpgrade, exact)
			}
		})
	}
}

func TestIdenticalContentAfterMediaReplacementStillNotifies(t *testing.T) {
	installer, request, database, audit := identicalInstallationFixture(t)
	request.Media.Ref.FileID++
	request.Media.Fingerprint.FileID++
	var err error
	request.MediaID, _, err = database.Repository().UpsertMedia(t.Context(), request.Media)
	if err != nil {
		t.Fatal(err)
	}
	request.Candidate.ResultID = "replacement"
	if _, err := installer.Install(t.Context(), request); err != nil {
		t.Fatalf("same-content replacement: %v", err)
	}
	var notifications int
	if err := audit.QueryRow(`SELECT count(*) FROM notifications`).Scan(&notifications); err != nil || notifications != 2 {
		t.Fatalf("replacement notifications=%d, %v", notifications, err)
	}
}
