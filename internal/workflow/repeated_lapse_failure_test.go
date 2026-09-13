package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestRepeatedSilentLapseFailureSurvivesRestartAndAdvancesCandidate(t *testing.T) {
	ctx := t.Context()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	request.Media.ReleaseGroup = "GROUP"
	candidate := broadCandidate("silent")
	candidate.ReleaseNames = []string{"Movie.2024-GROUP"}
	good := broadCandidate("good")
	databasePath := filepath.Join(t.TempDir(), "state.db")
	silentExit := &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: true}
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"silent": silentExit}}
	providerFake := &fakeProvider{id: "provider"}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	service := repeatedLapseTestService(t, request, database.Repository(), synchronizer, providerFake, []domain.Candidate{candidate})
	service.Events = events
	first, err := service.Run(ctx, request)
	var exhausted *acquisitionExhaustedError
	if err == nil || !errors.As(err, &exhausted) || first.Outcome != "" {
		t.Fatalf("first Run() = %+v/%T %v, want technical failure scheduling input", first, err, err)
	}
	if rejections, listErr := database.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now()); listErr != nil || len(rejections) != 0 {
		t.Fatalf("first-run rejections = %#v/%v, want none", rejections, listErr)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if strikes, rejections := repeatedLapseCounts(t, databasePath); strikes != 1 || rejections != 0 {
		t.Fatalf("first-run strikes/rejections = %d/%d, want 1/0", strikes, rejections)
	}

	database, err = store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service = repeatedLapseTestService(t, request, database.Repository(), synchronizer, providerFake, []domain.Candidate{candidate, good})
	service.Events = events
	second, err := service.Run(ctx, request)
	if err != nil || second.Outcome != OutcomeInstalled || second.Candidate.ResultID != "good" {
		t.Fatalf("second Run() = %+v/%v, want later candidate installed", second, err)
	}
	if !slices.Equal(synchronizer.synchronized, []string{"silent", "silent", "good"}) || !slices.Equal(providerFake.downloaded, []string{"silent", "silent", "good"}) {
		t.Fatalf("candidate progression sync/download = %v/%v", synchronizer.synchronized, providerFake.downloaded)
	}
	rejectedEvents := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.rejected")
	if len(rejectedEvents) != 1 || rejectedEvents[0]["candidate_id"] != "silent" || rejectedEvents[0]["reason_code"] != "lapse_repeated_empty_exit" {
		t.Fatalf("confirmed rejection events = %#v", rejectedEvents)
	}
	rejections, err := database.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now())
	if err != nil || len(rejections) != 1 || rejections[0].ResultID != "silent" || rejections[0].ReasonCode != "lapse_repeated_empty_exit" {
		t.Fatalf("confirmed rejection = %#v/%v", rejections, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if strikes, persistedRejections := repeatedLapseCounts(t, databasePath); strikes != 0 || persistedRejections != 1 {
		t.Fatalf("second-run strikes/rejections = %d/%d, want 0/1", strikes, persistedRejections)
	}
}

func TestRepeatedSilentLapseFailureEligibilityControls(t *testing.T) {
	for _, test := range []struct {
		name    string
		ctx     func() context.Context
		failure error
	}{
		{name: "nonempty stderr", failure: &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: false}},
		{name: "nonempty stdout", failure: &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: false, StderrEmpty: true}},
		{name: "different exit", failure: &syncer.ProcessExitError{ExitCode: 1, StdoutEmpty: true, StderrEmpty: true}},
		{name: "timeout", failure: context.DeadlineExceeded},
		{name: "canceled typed exit", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }, failure: &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.ctx != nil {
				ctx = test.ctx()
			}
			request := serviceRequest(t)
			artifact := writeInstallFile(t, filepath.Join(t.TempDir(), "selected.srt"), installSRT)
			repository := &workflowRepository{}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
			service.Repository = repository
			failures := []error{}
			if err := service.handleCandidateFailure(ctx, request, broadCandidate("silent"), artifact, test.failure, &failures); err != nil {
				t.Fatalf("handleCandidateFailure() error = %v", err)
			}
			if len(repository.lapseFailures) != 0 || len(failures) != 1 || !errors.Is(failures[0], test.failure) {
				t.Fatalf("strike/failures = %#v/%#v, want no strike and original technical failure", repository.lapseFailures, failures)
			}
		})
	}
}

func TestRepeatedSilentLapseFailureRequiresAvailableArtifactChecksum(t *testing.T) {
	request := serviceRequest(t)
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	failures := []error{}
	err := service.handleCandidateFailure(t.Context(), request, broadCandidate("silent"), filepath.Join(t.TempDir(), "missing.srt"), &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: true}, &failures)
	if err == nil || len(repository.lapseFailures) != 0 || len(failures) != 0 {
		t.Fatalf("missing checksum error/strikes/failures = %v/%#v/%#v", err, repository.lapseFailures, failures)
	}
}

func TestRepeatedSilentLapseFailureDoesNotCrossChangedIdentity(t *testing.T) {
	for _, change := range []struct {
		name   string
		second func(*Request, *domain.Candidate, string, **Service)
	}{
		{
			name: "artifact",
			second: func(_ *Request, _ *domain.Candidate, artifact string, _ **Service) {
				if err := os.WriteFile(artifact, []byte("1\n00:00:01,000 --> 00:00:02,000\nchanged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "candidate", second: func(_ *Request, candidate *domain.Candidate, _ string, _ **Service) {
			candidate.ReleaseNames = []string{"changed-release"}
		}},
		{name: "media", second: func(request *Request, _ *domain.Candidate, _ string, _ **Service) { request.Media.Fingerprint.FileID++ }},
		{name: "tool", second: func(_ *Request, _ *domain.Candidate, _ string, service **Service) {
			(*service).Synchronizer = &versionedWorkflowSynchronizer{version: "2.0.6"}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx := t.Context()
			request := serviceRequest(t)
			request.Media.EntityID = 1
			databasePath := filepath.Join(t.TempDir(), "state.db")
			database, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			artifact := writeInstallFile(t, filepath.Join(t.TempDir(), "selected.srt"), installSRT)
			service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &versionedWorkflowSynchronizer{version: "2.0.5"}, &fakeInstaller{})
			service.Repository = database.Repository()
			failure := &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: true}
			candidate := broadCandidate("silent")
			failures := []error{}
			if err := service.handleCandidateFailure(ctx, request, candidate, artifact, failure, &failures); err != nil {
				t.Fatal(err)
			}
			change.second(&request, &candidate, artifact, &service)
			if err := service.handleCandidateFailure(ctx, request, candidate, artifact, failure, &failures); err != nil {
				t.Fatal(err)
			}
			if len(failures) != 2 {
				t.Fatalf("technical failures = %d, want 2 independent first strikes", len(failures))
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			if strikes, rejections := repeatedLapseCounts(t, databasePath); strikes != 2 || rejections != 0 {
				t.Fatalf("changed identity strikes/rejections = %d/%d, want 2/0", strikes, rejections)
			}
		})
	}
}

func TestRepeatedSilentLapseFailureRepositoryErrorRemainsTechnical(t *testing.T) {
	request := serviceRequest(t)
	artifact := writeInstallFile(t, filepath.Join(t.TempDir(), "selected.srt"), installSRT)
	repositoryErr := errors.New("record strike failed")
	repository := &workflowRepository{lapseErr: repositoryErr}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	failures := []error{}
	err := service.handleCandidateFailure(t.Context(), request, broadCandidate("silent"), artifact, &syncer.ProcessExitError{ExitCode: 2, StdoutEmpty: true, StderrEmpty: true}, &failures)
	if !errors.Is(err, repositoryErr) || len(repository.lapseFailures) != 1 || len(failures) != 0 {
		t.Fatalf("repository failure = %v, strikes=%d failures=%d", err, len(repository.lapseFailures), len(failures))
	}
}

type versionedWorkflowSynchronizer struct {
	fakeSynchronizer
	version string
}

func (s versionedWorkflowSynchronizer) CompatibilityVersion() string { return s.version }

func repeatedLapseTestService(t *testing.T, request Request, repository WorkflowRepository, synchronizer CandidateSynchronizer, providerFake *fakeProvider, candidates []domain.Candidate) *Service {
	t.Helper()
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {},
		provider.SearchBroad:     {Candidates: candidates},
	}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.LapsePolicy.Mode = "always"
	return service
}

func repeatedLapseCounts(t *testing.T, path string) (int, int) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var strikes, rejections int
	if err := database.QueryRow(`SELECT count(*) FROM candidate_lapse_failures`).Scan(&strikes); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT count(*) FROM candidate_rejections`).Scan(&rejections); err != nil {
		t.Fatal(err)
	}
	return strikes, rejections
}
