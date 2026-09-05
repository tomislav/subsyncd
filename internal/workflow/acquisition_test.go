package workflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func TestServiceExactMediaDurationRejectionContinuesWithRealInstaller(t *testing.T) {
	for _, next := range []string{"exact", "broad", "exhausted", "install failure", "database failure", "outbox failure", "rejection persistence failure"} {
		t.Run(next, func(t *testing.T) {
			ctx := context.Background()
			request := serviceRequest(t)
			request.Media.EntityID = 1
			dbPath := filepath.Join(t.TempDir(), "subsyncd.db")
			database, err := store.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repository := database.Repository()
			request.MediaID, _, err = repository.UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
				provider.SearchExactHash: {Candidates: []domain.Candidate{exactCandidate("too-long")}},
				provider.SearchBroad:     {},
			}}
			wantDownloads := []string{"too-long"}
			wantModes := []provider.SearchMode{provider.SearchExactHash}
			switch next {
			case "exact", "install failure", "database failure", "outbox failure", "rejection persistence failure":
				searcher.results[provider.SearchExactHash] = provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("too-long"), exactCandidate("good"), exactCandidate("unused")}}
				wantDownloads = append(wantDownloads, "good")
			case "broad":
				searcher.results[provider.SearchBroad] = provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("good")}}
				wantDownloads = append(wantDownloads, "good")
				wantModes = append(wantModes, provider.SearchBroad)
			case "exhausted":
				wantModes = append(wantModes, provider.SearchBroad)
			}
			var trigger string
			switch next {
			case "database failure":
				trigger = `CREATE TRIGGER fail_installation BEFORE INSERT ON installations BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END`
			case "outbox failure":
				trigger = `CREATE TRIGGER fail_notification BEFORE INSERT ON notifications BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END`
			case "rejection persistence failure":
				trigger = `CREATE TRIGGER fail_rejection BEFORE INSERT ON candidate_rejections BEGIN SELECT RAISE(ABORT, 'injected persistence failure'); END`
				wantDownloads = []string{"too-long"}
			}
			if trigger != "" {
				audit, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer audit.Close()
				if _, err := audit.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			// Syntax and timestamps are valid, but 96 minutes exceeds the
			// 90-minute media's five-minute installation tolerance.
			invalid := "1\n00:00:01,000 --> 01:36:00,000\nToo long\n"
			adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"too-long": []byte(invalid)}}
			destination := filepath.Join(filepath.Dir(request.Media.Fingerprint.Path), "Movie.en.srt")
			created := 0
			failure := errors.New("staging unavailable")
			installer := Installer{Repository: repository, MediaRoots: []string{filepath.Dir(destination)}, NotifierNames: []string{"silo"}, Fault: func(stage InstallStage) error {
				if stage == StageCreate {
					created++
					if next == "install failure" {
						return failure
					}
				}
				if stage == StageDatabase {
					payload, err := os.ReadFile(destination)
					if err != nil || !strings.Contains(string(payload), "good") {
						t.Fatalf("published payload = %q, %v", payload, err)
					}
				}
				return nil
			}}
			service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, installer)
			service.Repository = repository
			service.Providers = map[string]provider.Provider{"provider": adapter}

			result, runErr := service.Run(ctx, request)
			if next == "install failure" {
				if !errors.Is(runErr, failure) || result.Outcome != "" {
					t.Fatalf("Run() outcome/error = %s/%v", result.Outcome, runErr)
				}
			} else if trigger != "" {
				if runErr == nil || !strings.Contains(runErr.Error(), "injected persistence failure") || result.Outcome != "" {
					t.Fatalf("Run() outcome/error = %s/%v", result.Outcome, runErr)
				}
			} else if next == "exhausted" {
				if runErr != nil || result.Outcome != OutcomeRejected {
					t.Fatalf("Run() outcome/error = %s/%v", result.Outcome, runErr)
				}
			} else if runErr != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "good" {
				t.Fatalf("Run() outcome/candidate/error = %s/%s/%v", result.Outcome, result.Candidate.ResultID, runErr)
			}
			if !slices.Equal(adapter.downloaded, wantDownloads) {
				t.Fatalf("downloads = %v; want %v", adapter.downloaded, wantDownloads)
			}
			var modes []provider.SearchMode
			for _, query := range searcher.queries {
				modes = append(modes, query.Mode)
			}
			if !slices.Equal(modes, wantModes) {
				t.Fatalf("search modes = %v; want %v", modes, wantModes)
			}
			rejections, err := repository.ListCandidateRejections(ctx, request.MediaID, "en", service.Clock.Now())
			if next == "rejection persistence failure" {
				if err != nil || len(rejections) != 0 {
					t.Fatalf("uncommitted rejections = %#v, %v", rejections, err)
				}
			} else if err != nil || len(rejections) != 1 || rejections[0].ResultID != "too-long" || rejections[0].ReasonCode != "invalid_subtitle" || rejections[0].ArtifactChecksum == "" {
				t.Fatalf("durable rejections = %#v, %v", rejections, err)
			}
			installed, found, err := repository.GetInstallation(ctx, request.MediaID, "en")
			wantInstalled := next == "exact" || next == "broad"
			if err != nil || found != wantInstalled || found && installed.CandidateID != "good" {
				t.Fatalf("durable installation = %#v, %v, %v", installed, found, err)
			}
			if !wantInstalled {
				if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("uncommitted sidecar exists: %v", err)
				}
			}
			wantCreates := 1
			if next == "exhausted" || next == "rejection persistence failure" {
				wantCreates = 0
			}
			if created != wantCreates {
				t.Fatalf("staged candidates = %d; want %d", created, wantCreates)
			}
		})
	}
}

func TestServiceExactJoinedInstallValidationFailureRemainsTerminal(t *testing.T) {
	request := serviceRequest(t)
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), "1\n00:00:01,000 --> 01:36:00,000\nToo long\n")
	_, validationErr := validatedSubtitle(source, request.Media.Duration)
	if validationErr == nil {
		t.Fatal("duration fixture must fail source validation")
	}
	failure := errors.Join(validationErr, errors.New("restore previous subtitle failed"))
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{exactCandidate("first"), exactCandidate("unused")}},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("broad")}},
	}}
	adapter := &fakeProvider{id: "provider"}
	installer := &fakeInstaller{err: failure}
	service := testService(t, inventory.Inventory{}, searcher, nil, nil, installer)
	service.Providers = map[string]provider.Provider{"provider": adapter}
	result, err := service.Run(context.Background(), request)
	if !errors.Is(err, failure) || result.Outcome != "" || installer.calls != 1 || !slices.Equal(adapter.downloaded, []string{"first"}) || len(searcher.queries) != 1 {
		t.Fatalf("Run() = %s, %v; installs/downloads/searches = %d/%v/%d", result.Outcome, err, installer.calls, adapter.downloaded, len(searcher.queries))
	}
	if got := service.Repository.(*workflowRepository).rejections; len(got) != 0 {
		t.Fatalf("joined installation failure created rejections: %#v", got)
	}
}

func TestServiceExactCancellationPreservesInstallerRollbackError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository := database.Repository()
	request.MediaID, _, err = repository.UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{exactCandidate("first"), exactCandidate("unused")}},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("broad")}},
	}}
	adapter := &fakeProvider{id: "provider"}
	destination := filepath.Join(filepath.Dir(request.Media.Fingerprint.Path), "Movie.en.srt")
	installer := Installer{Repository: repository, MediaRoots: []string{filepath.Dir(destination)}, NotifierNames: []string{"silo"}, Fault: func(stage InstallStage) error {
		if stage != StageDatabase {
			return nil
		}
		payload, err := os.ReadFile(destination)
		if err != nil || !strings.Contains(string(payload), "first") {
			t.Fatalf("published subtitle = %q, %v", payload, err)
		}
		// A nonempty directory makes the real rollback removal fail after
		// cancellation causes the SQLite installation transaction to fail.
		if err := os.Remove(destination); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		writeInstallFile(t, filepath.Join(destination, "blocker"), "x")
		cancel()
		return nil
	}}
	service := testService(t, inventory.Inventory{}, searcher, nil, nil, installer)
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": adapter}

	result, runErr := service.Run(ctx, request)
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run() lost cancellation: %v", runErr)
	}
	var rollbackErr *os.PathError
	if !errors.As(runErr, &rollbackErr) || rollbackErr.Op != "remove" || rollbackErr.Path != destination {
		t.Errorf("Run() lost rollback removal failure: %v", runErr)
	}
	if result.Outcome != "" || !slices.Equal(adapter.downloaded, []string{"first"}) || len(searcher.queries) != 1 || searcher.queries[0].Mode != provider.SearchExactHash {
		t.Fatalf("outcome/downloads/searches = %s/%v/%v", result.Outcome, adapter.downloaded, searcher.queries)
	}
	// Use an uncanceled context to inspect durable state after the failed run.
	inspection := context.Background()
	rejections, err := repository.ListCandidateRejections(inspection, request.MediaID, "en", service.Clock.Now())
	if err != nil || len(rejections) != 0 {
		t.Fatalf("cancellation created rejections: %#v, %v", rejections, err)
	}
	if _, found, err := repository.GetInstallation(inspection, request.MediaID, "en"); err != nil || found {
		t.Fatalf("uncommitted installation found/error = %v/%v", found, err)
	}
	if entries, err := os.ReadDir(destination); err != nil || len(entries) != 1 || entries[0].Name() != "blocker" {
		t.Fatalf("rollback obstruction = %v, %v", entries, err)
	}
}

func TestServiceExactDirectInstallCancellationRemainsTerminal(t *testing.T) {
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {Candidates: []domain.Candidate{exactCandidate("first"), exactCandidate("unused")}},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("broad")}},
	}}
	adapter := &fakeProvider{id: "provider"}
	installer := &fakeInstaller{err: context.Canceled}
	service := testService(t, inventory.Inventory{}, searcher, nil, nil, installer)
	service.Providers = map[string]provider.Provider{"provider": adapter}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if !errors.Is(err, context.Canceled) || result.Outcome != "" || installer.calls != 1 || !slices.Equal(adapter.downloaded, []string{"first"}) || len(searcher.queries) != 1 {
		t.Fatalf("outcome/error/installs/downloads/searches = %s/%v/%d/%v/%d", result.Outcome, err, installer.calls, adapter.downloaded, len(searcher.queries))
	}
	if got := service.Repository.(*workflowRepository).rejections; len(got) != 0 {
		t.Fatalf("cancellation created rejections: %#v", got)
	}
}
