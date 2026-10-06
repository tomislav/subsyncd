package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func TestRunOnceLimitsWorkflowConcurrencyAndRenewsLeases(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(5, now)
	// Keep completions overlapping other live jobs so their lease renewals
	// exercise the same concurrency allowed by the real per-job repository.
	repository.completionDelay = 20 * time.Millisecond
	service := &workerWorkflow{delay: 35 * time.Millisecond, outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.RenewInterval = 5 * time.Millisecond
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.maxActive != 2 {
		t.Fatalf("maximum workflow concurrency = %d, want 2", service.maxActive)
	}
	if repository.searchRenewals == 0 {
		t.Fatal("long-running search leases were not renewed")
	}
	if len(repository.searchCompletions) != 5 {
		t.Fatalf("search completions = %d, want 5", len(repository.searchCompletions))
	}
}

func TestRunSearchLeaseCompletesUnsupportedMediaWithoutWorkflow(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	media := repository.media[1]
	media.UnsupportedReason = domain.UnsupportedMultiEpisode
	repository.media[1] = media
	service := &workerWorkflow{}
	worker := testWorker(repository, service, testutil.NewClock(now))
	if err := worker.runSearchLease(context.Background(), repository.searches[0]); err != nil {
		t.Fatal(err)
	}
	if service.calls != 0 {
		t.Fatalf("workflow calls = %d, want 0", service.calls)
	}
	if len(repository.searchCompletions) != 1 || repository.searchCompletions[0].Outcome != string(domain.UnsupportedMultiEpisode) || !repository.searchCompletions[0].NextAttemptAt.IsZero() {
		t.Fatalf("search completions = %#v", repository.searchCompletions)
	}
}

func TestWorkerPassesProviderResume(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.searches[0].Priority = store.SearchPriorityMissing
	repository.searches[0].ResumeProviders = []string{"titlovi-main", "subdl-main"}
	repository.searches[0].ResumeRouteSignature = "route-v1"
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeNoResult}}
	w := testWorker(repository, service, testutil.NewClock(now))

	if err := w.runSearchLease(t.Context(), repository.searches[0]); err != nil {
		t.Fatal(err)
	}
	if len(service.requests) != 1 {
		t.Fatalf("workflow requests = %d, want 1", len(service.requests))
	}
	request := service.requests[0]
	if !slices.Equal(request.ResumeProviders, []string{"titlovi-main", "subdl-main"}) || request.ResumeRouteSignature != "route-v1" {
		t.Fatalf("workflow resume request = %v/%q", request.ResumeProviders, request.ResumeRouteSignature)
	}
}

func TestWorkerPersistsProviderResume(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	resumeProviders := []string{"titlovi-main", "subdl-main"}

	for _, test := range []struct {
		name         string
		result       workflow.Result
		workflowErr  error
		wantErr      bool
		wantPreserve bool
	}{
		{
			name:         "throttled",
			result:       workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: now.Add(8 * time.Hour), ResumeProviders: resumeProviders, ResumeRouteSignature: "route-v2"},
			wantPreserve: true,
		},
		{
			name:         "technical failure",
			result:       workflow.Result{ResumeProviders: resumeProviders, ResumeRouteSignature: "route-v2"},
			workflowErr:  errors.New("temporary workflow failure"),
			wantErr:      true,
			wantPreserve: true,
		},
		{
			name:        "technical providers without signature",
			result:      workflow.Result{ResumeProviders: resumeProviders},
			workflowErr: errors.New("temporary workflow failure"),
			wantErr:     true,
		},
		{
			name:        "technical signature without providers",
			result:      workflow.Result{ResumeRouteSignature: "route-v2"},
			workflowErr: errors.New("temporary workflow failure"),
			wantErr:     true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newWorkerRepository(1, now)
			repository.searches[0].Priority = store.SearchPriorityMissing
			service := &workerWorkflow{outcome: test.result, err: test.workflowErr}
			w := testWorker(repository, service, testutil.NewClock(now))
			w.RandomUnit = func() float64 { return 0 }

			err := w.runSearchLease(t.Context(), repository.searches[0])
			if test.wantErr && !errors.Is(err, test.workflowErr) {
				t.Fatalf("runSearchLease() error = %v, want %v", err, test.workflowErr)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
			if len(repository.searchCompletions) != 1 {
				t.Fatalf("search completions = %d, want 1", len(repository.searchCompletions))
			}
			completion := repository.searchCompletions[0]
			if completion.PreserveResume != test.wantPreserve {
				t.Fatalf("persisted resume completion = %+v", completion)
			}
			if test.wantPreserve && (!slices.Equal(completion.ResumeProviders, resumeProviders) || completion.ResumeRouteSignature != "route-v2") {
				t.Fatalf("persisted resume completion = %+v", completion)
			}
			if !test.wantPreserve && (len(completion.ResumeProviders) != 0 || completion.ResumeRouteSignature != "") {
				t.Fatalf("incomplete resume completion = %+v", completion)
			}
			if test.result.Outcome == workflow.OutcomeThrottled {
				if !completion.NextAttemptAt.Equal(test.result.RetryAt) || completion.AdvanceMissingAttempt || completion.AdvanceFailureAttempt || completion.Priority != 0 || !completion.PreserveQueueOrder {
					t.Fatalf("throttled scheduling changed = %+v", completion)
				}
			} else if !completion.NextAttemptAt.Equal(now.Add(time.Minute)) || !completion.AdvanceFailureAttempt || completion.AdvanceMissingAttempt || completion.Priority != 0 || completion.PreserveQueueOrder {
				t.Fatalf("technical scheduling changed = %+v", completion)
			}
		})
	}
}

func TestWorkerClearsProviderResume(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	resumeResult := workflow.Result{ResumeProviders: []string{"titlovi-main"}, ResumeRouteSignature: "route-v2"}
	for _, test := range []struct {
		name     string
		priority store.SearchPriority
		result   workflow.Result
	}{
		{name: "installed", priority: store.SearchPriorityMissing, result: workflow.Result{Outcome: workflow.OutcomeInstalled, ResumeProviders: resumeResult.ResumeProviders, ResumeRouteSignature: resumeResult.ResumeRouteSignature}},
		{name: "satisfied", priority: store.SearchPriorityMissing, result: workflow.Result{Outcome: workflow.OutcomeSatisfied, ResumeProviders: resumeResult.ResumeProviders, ResumeRouteSignature: resumeResult.ResumeRouteSignature}},
		{name: "no result", priority: store.SearchPriorityMissing, result: workflow.Result{Outcome: workflow.OutcomeNoResult, ResumeProviders: resumeResult.ResumeProviders, ResumeRouteSignature: resumeResult.ResumeRouteSignature}},
		{name: "rejected", priority: store.SearchPriorityMissing, result: workflow.Result{Outcome: workflow.OutcomeRejected, ResumeProviders: resumeResult.ResumeProviders, ResumeRouteSignature: resumeResult.ResumeRouteSignature}},
		{name: "upgrade priority", priority: store.SearchPriorityUpgrade, result: workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: now.Add(time.Hour), ResumeProviders: resumeResult.ResumeProviders, ResumeRouteSignature: resumeResult.ResumeRouteSignature}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newWorkerRepository(1, now)
			lease := repository.searches[0]
			lease.Priority = test.priority
			lease.ResumeProviders = []string{"stale-provider"}
			lease.ResumeRouteSignature = "stale-route"
			service := &workerWorkflow{outcome: test.result}
			w := testWorker(repository, service, testutil.NewClock(now))

			if err := w.runSearchLease(t.Context(), lease); err != nil {
				t.Fatal(err)
			}
			completion := repository.searchCompletions[0]
			if completion.PreserveResume || len(completion.ResumeProviders) != 0 || completion.ResumeRouteSignature != "" {
				t.Fatalf("terminal completion retained resume state: %+v", completion)
			}
			if test.priority == store.SearchPriorityUpgrade {
				request := service.requests[0]
				if len(request.ResumeProviders) != 0 || request.ResumeRouteSignature != "" {
					t.Fatalf("upgrade request retained resume state: %v/%q", request.ResumeProviders, request.ResumeRouteSignature)
				}
			}
		})
	}

	t.Run("unsupported", func(t *testing.T) {
		repository := newWorkerRepository(1, now)
		lease := repository.searches[0]
		lease.ResumeProviders = []string{"stale-provider"}
		lease.ResumeRouteSignature = "stale-route"
		media := repository.media[lease.MediaID]
		media.UnsupportedReason = domain.UnsupportedMultiEpisode
		repository.media[lease.MediaID] = media
		service := &workerWorkflow{}
		w := testWorker(repository, service, testutil.NewClock(now))

		if err := w.runSearchLease(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
		completion := repository.searchCompletions[0]
		if completion.PreserveResume || len(completion.ResumeProviders) != 0 || completion.ResumeRouteSignature != "" {
			t.Fatalf("unsupported completion retained resume state: %+v", completion)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "subsyncd.db")
		database, err := store.Open(t.Context(), databasePath)
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		repository := database.Repository()
		media := domain.Media{
			EntityID: 1,
			Ref:      domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: 1},
			Fingerprint: domain.MediaFingerprint{
				Path: "/media/movie.mkv", FileID: 1, Size: 100, ModTime: now,
			},
			Title: "Movie",
		}
		mediaID, _, err := repository.UpsertMedia(t.Context(), media)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.UpsertSearchStateWithPriority(t.Context(), mediaID, "en", now, store.SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
		leases, err := repository.LeaseDueSearches(t.Context(), now, 1, time.Minute)
		if err != nil || len(leases) != 1 {
			t.Fatalf("initial lease = %+v/%v", leases, err)
		}
		if _, err := repository.CompleteSearch(t.Context(), store.SearchCompletion{
			JobID:                leases[0].JobID,
			Outcome:              "throttled",
			NextAttemptAt:        now.Add(time.Hour),
			ResumeProviders:      []string{"stale-provider"},
			ResumeRouteSignature: "stale-route",
			PreserveResume:       true,
		}); err != nil {
			t.Fatal(err)
		}
		leases, err = repository.LeaseDueSearches(t.Context(), now.Add(time.Hour), 1, time.Minute)
		if err != nil || len(leases) != 1 {
			t.Fatalf("resumed lease = %+v/%v", leases, err)
		}
		if applied, err := repository.ApplyMediaEvent(t.Context(), store.MediaEventMutation{EventID: "deleted", Type: "delete", Ref: media.Ref, At: now.Add(2 * time.Hour)}); err != nil || !applied {
			t.Fatalf("delete event = %v/%v", applied, err)
		}
		service := &workerWorkflow{outcome: workflow.Result{
			Outcome:              workflow.OutcomeThrottled,
			RetryAt:              now.Add(3 * time.Hour),
			ResumeProviders:      []string{"new-provider"},
			ResumeRouteSignature: "new-route",
		}}
		w := testWorker(nil, service, testutil.NewClock(now.Add(time.Hour)))
		w.Repository = repository
		var logs bytes.Buffer
		w.Events, err = observability.New(&logs, observability.Options{Level: "info", Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.runSearchLease(t.Context(), leases[0]); err != nil {
			t.Fatal(err)
		}
		status, err := repository.GetSearchStatus(t.Context(), mediaID, "en")
		if err != nil || status.State != "complete" || status.LastOutcome != "deleted" {
			t.Fatalf("deleted status = %+v/%v", status, err)
		}
		audit, err := sql.Open("sqlite", databasePath)
		if err != nil {
			t.Fatal(err)
		}
		defer audit.Close()
		var providersJSON, signature string
		if err := audit.QueryRow(`SELECT resume_providers_json,resume_route_signature FROM search_states WHERE media_id=? AND language='en'`, mediaID).Scan(&providersJSON, &signature); err != nil {
			t.Fatal(err)
		}
		if providersJSON != `[]` || signature != "" {
			t.Fatalf("deleted completion retained resume = %s/%q", providersJSON, signature)
		}
		completed := findWorkerEvent(t, workerLogRecords(t, logs.String()), "job.completed")
		if completed["resume_provider_count"] != float64(0) {
			t.Fatalf("deleted completion logged resume_provider_count = %v, want 0", completed["resume_provider_count"])
		}
	})
}

func TestRunOnceLogsCorrelatedSearchJobLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.searches[0].Priority = store.SearchPriorityImport
	media := repository.media[1]
	media.Title = "Blade\nRunner"
	media.Year = 1982
	repository.media[1] = media
	var logs synchronizedBuffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	worker := testWorker(repository, &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}, testutil.NewClock(now))
	worker.Events = events
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := workerLogRecords(t, logs.String())
	for _, event := range []string{"job.leased", "job.started", "job.completed"} {
		record := findWorkerEvent(t, records, event)
		if record["job_id"] != "job-1ns" || record["media_id"] != float64(1) || record["language"] != "en" || record["priority"] != "import" {
			t.Fatalf("%s correlation = %#v", event, record)
		}
	}
	completed := findWorkerEvent(t, records, "job.completed")
	if completed["outcome"] != "satisfied" || completed["instance"] != "sonarr" || completed["media_kind"] != "movie" || completed["file_id"] != float64(1) {
		t.Fatalf("completion fields = %#v", completed)
	}
	for _, event := range []string{"job.started", "job.completed"} {
		if record := findWorkerEvent(t, records, event); record["media_title"] != "Blade Runner (1982)" {
			t.Fatalf("%s media title = %#v", event, record)
		}
	}
	if _, ok := completed["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms = %#v", completed["duration_ms"])
	}
}

func TestWorkerLogsProviderResumeCountsWithoutProviderLists(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.searches[0].Priority = store.SearchPriorityMissing
	repository.searches[0].ResumeProviders = []string{"prior-provider"}
	repository.searches[0].ResumeRouteSignature = "route-v1"
	service := &workerWorkflow{outcome: workflow.Result{
		Outcome:              workflow.OutcomeThrottled,
		RetryAt:              now.Add(time.Hour),
		ResumeProviders:      []string{"completed-provider"},
		ResumeRouteSignature: "route-v1",
	}}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	w := testWorker(repository, service, testutil.NewClock(now))
	w.Events = events
	w.RandomUnit = func() float64 { return 0 }

	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	records := workerLogRecords(t, logs.String())
	if got := findWorkerEvent(t, records, "job.leased")["resume_provider_count"]; got != float64(1) {
		t.Fatalf("leased resume_provider_count = %#v, want 1", got)
	}
	if got := findWorkerEvent(t, records, "job.completed")["resume_provider_count"]; got != float64(1) {
		t.Fatalf("completed resume_provider_count = %#v, want 1", got)
	}
	if strings.Contains(logs.String(), "prior-provider") || strings.Contains(logs.String(), "completed-provider") {
		t.Fatalf("provider resume list leaked into logs: %s", logs.String())
	}
}

func TestRunOnceLogsTechnicalRetryAndSameKeyRerun(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.searches[0].ResumeProviders = []string{"prior-provider"}
	repository.searches[0].ResumeRouteSignature = "route-v1"
	repository.completionResults = []store.SearchCompletionResult{{RerunScheduled: true}}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "sanitized" }})
	if err != nil {
		t.Fatal(err)
	}
	worker := testWorker(repository, &workerWorkflow{outcome: workflow.Result{ResumeProviders: []string{"completed-provider"}, ResumeRouteSignature: "route-v1"}, err: errors.New("secret failure")}, testutil.NewClock(now))
	worker.Events = events
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil")
	}
	records := workerLogRecords(t, logs.String())
	findWorkerEvent(t, records, "job.retry_scheduled")
	findWorkerEvent(t, records, "job.rerun_requested")
	completed := findWorkerEvent(t, records, "job.completed")
	if completed["outcome"] != "failed" || completed["error"] != "sanitized" || completed["error_kind"] != "workflow" {
		t.Fatalf("failed completion = %#v", completed)
	}
	if completed["resume_provider_count"] != float64(0) {
		t.Fatalf("rerun completion resume_provider_count = %#v, want 0", completed["resume_provider_count"])
	}
	if strings.Contains(logs.String(), "secret failure") {
		t.Fatalf("unsanitized workflow error leaked: %s", logs.String())
	}
}

func TestRunOnceStopsLeaseRenewalBeforeCompletingJob(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.completionDelay = 20 * time.Millisecond
	worker := testWorker(repository, &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}, testutil.NewClock(now))
	worker.RenewInterval = 5 * time.Millisecond
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.renewedDuringCompletion {
		t.Fatal("lease renewal raced job completion")
	}
}

func TestTwoWorkersCannotProcessTheSameSQLiteLease(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository := database.Repository()
	media := domain.Media{EntityID: 1, Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: 1}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: 1, Size: 100, ModTime: now}, Title: "Movie"}
	mediaID, _, err := repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.UpsertSearchStateWithPriority(context.Background(), mediaID, "en", now, store.SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	service := &workerWorkflow{delay: 20 * time.Millisecond, outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}
	clock := testutil.NewClock(now)
	first := &Worker{Repository: repository, Workflow: service, Clock: clock, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute}
	second := &Worker{Repository: repository, Workflow: service, Clock: clock, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute}
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, candidate := range []*Worker{first, second} {
		wait.Add(1)
		go func(candidate *Worker) {
			defer wait.Done()
			errors <- candidate.RunOnce(context.Background())
		}(candidate)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if service.calls != 1 {
		t.Fatalf("workflow calls = %d, want 1", service.calls)
	}
}

func TestExpiredLeaseRecoversAfterCrashBetweenInstallAndCompletion(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.completeErrors = []error{errors.New("simulated completion crash")}
	service := &workerWorkflow{outcomes: []workflow.Result{
		{Outcome: workflow.OutcomeInstalled, Installation: store.Installation{MediaID: 1, Language: "en", Path: "/media/movie.en.srt", Checksum: "sum"}},
		{Outcome: workflow.OutcomeSatisfied},
	}}
	worker := testWorker(repository, service, testutil.NewClock(now))
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("first RunOnce() error = nil")
	}
	repository.mu.Lock()
	repository.searches = []store.SearchLease{{MediaID: 1, Language: "en", JobID: "recovered", LeaseUntil: now.Add(10 * time.Minute)}}
	repository.mu.Unlock()
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.calls != 2 || service.installOutcomes != 1 {
		t.Fatalf("workflow calls/install outcomes = %d/%d, want 2/1", service.calls, service.installOutcomes)
	}
}

func TestRunOnceSchedulesThrottleAfterResetWithoutAdvancingAttempts(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	reset := now.Add(10 * time.Minute)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: reset}}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.RandomUnit = func() float64 { return 0.5 }
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion := repository.searchCompletions[0]
	if !completion.NextAttemptAt.Equal(reset.Add(30*time.Second)) || completion.AdvanceMissingAttempt || completion.AdvanceFailureAttempt || completion.Priority != 0 {
		t.Fatalf("throttle completion = %#v", completion)
	}
}

func TestRunOnceSchedulesNoResultAsMissingPriority(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeNoResult}}
	worker := testWorker(repository, service, testutil.NewClock(now))

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion := repository.searchCompletions[0]
	if completion.Priority != store.SearchPriorityMissing || !completion.AdvanceMissingAttempt {
		t.Fatalf("no-result completion = %#v", completion)
	}
}

func TestRunOnceSchedulesNonExactSatisfiedReassessmentForUpgrade(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nextUpgrade := now.Add(30 * 24 * time.Hour)
	repository := newWorkerRepository(1, now)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied, NextUpgrade: nextUpgrade}}
	worker := testWorker(repository, service, testutil.NewClock(now))

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion := repository.searchCompletions[0]
	if !completion.NextAttemptAt.Equal(nextUpgrade) || !completion.ResetMissingAttempt || !completion.ResetFailureAttempt || completion.Priority != store.SearchPriorityUpgrade {
		t.Fatalf("satisfied reassessment completion = %#v", completion)
	}
}

func TestPollDelayUsesTenPercentJitter(t *testing.T) {
	worker := &Worker{PollInterval: time.Minute, RandomUnit: func() float64 { return 0 }}
	if got := worker.pollDelay(); got != 54*time.Second {
		t.Fatalf("low poll delay = %s", got)
	}
	worker.RandomUnit = func() float64 { return 1 }
	if got := worker.pollDelay(); got != 66*time.Second {
		t.Fatalf("high poll delay = %s", got)
	}
}

func TestRunOnceDeliversPersistedNotificationWithoutEnqueueAfterInstallation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.notifications = []store.NotificationLease{{ID: 1, Notifier: "silo", PayloadJSON: notificationJSON(t, repository.media[1], "/media/movie.en.srt"), JobID: "persisted-before-workflow-completion"}}
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeInstalled, Installation: store.Installation{MediaID: 1, Language: "en", Path: "/media/movie.en.srt", Checksum: "sum"}}}
	delivery := &workerNotifier{}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.Notifiers = map[string]notifier.Notifier{"silo": delivery}
	worker.MaxWorkflows = 1
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.enqueued) != 0 || delivery.calls != 1 || len(repository.notificationCompletions) != 1 || repository.notificationCompletions[0].Result != "success" {
		t.Fatalf("notifications = enqueued %#v calls %d completions %#v", repository.enqueued, delivery.calls, repository.notificationCompletions)
	}
}

func TestRunOnceLogsReconciliationAndNotificationLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(0, now)
	repository.notifications = []store.NotificationLease{{ID: 1, Notifier: "silo", PayloadJSON: notificationJSON(t, repository.media[1], "/private/media/movie.en.srt"), Attempt: 1, JobID: "notification-1"}}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	worker := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	worker.Events = events
	worker.Reconcilers = map[string]Reconciler{"sonarr": &workerReconciler{}}
	worker.Notifiers = map[string]notifier.Notifier{"silo": &workerNotifier{}}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := workerLogRecords(t, logs.String())
	findWorkerEvent(t, records, "reconcile.started")
	findWorkerEvent(t, records, "reconcile.completed")
	delivered := findWorkerEvent(t, records, "notification.delivered")
	if delivered["notification_id"] != "notification-1" || delivered["notifier"] != "silo" || delivered["attempt"] != float64(1) {
		t.Fatalf("notification fields = %#v", delivered)
	}
	if strings.Contains(logs.String(), "/private/media") || strings.Contains(logs.String(), "subtitle_path") {
		t.Fatalf("notification payload leaked: %s", logs.String())
	}
}

func TestRunOnceRetriesTemporaryNotificationWithoutChangingSearchState(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(0, now)
	repository.notifications = []store.NotificationLease{{ID: 1, Notifier: "silo", PayloadJSON: notificationJSON(t, repository.media[1], "/media/movie.en.srt"), Attempt: 1, JobID: "notification-1"}}
	worker := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	worker.Notifiers = map[string]notifier.Notifier{"silo": &workerNotifier{err: &notifier.DeliveryError{StatusCode: 502, Retryable: true, Reason: "502"}}}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion := repository.notificationCompletions[0]
	if completion.Result != "retryable_error" || !completion.NextAttemptAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("notification completion = %#v", completion)
	}
	if len(repository.searchCompletions) != 0 {
		t.Fatalf("notification retry changed search state: %#v", repository.searchCompletions)
	}
}

func TestRunOnceReconcilesEachInstanceAtSixHourIntervals(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	repository := newWorkerRepository(0, now)
	first, second := &workerReconciler{}, &workerReconciler{}
	worker := testWorker(repository, &workerWorkflow{}, clock)
	worker.Reconcilers = map[string]Reconciler{"sonarr": first, "radarr": second}
	for _, advance := range []time.Duration{0, 5 * time.Hour, time.Hour} {
		clock.Advance(advance)
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls != 2 || second.calls != 2 {
		t.Fatalf("reconcile calls = %d/%d, want 2/2", first.calls, second.calls)
	}
}

func TestRunOnceReconcileFailureBackoffAndSuccessReset(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	repository := newWorkerRepository(0, now)
	failing := &sequenceReconciler{clock: clock, errs: []error{
		errors.New("secret /private/media /api/v3/history/since"),
		errors.New("failure two"),
		errors.New("failure three"),
		errors.New("failure four"),
		nil,
		nil,
	}}
	steady := &sequenceReconciler{clock: clock}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "redacted" }})
	if err != nil {
		t.Fatal(err)
	}
	worker := testWorker(repository, &workerWorkflow{}, clock)
	worker.Events = events
	worker.Reconcilers = map[string]Reconciler{"failing": failing, "steady": steady}

	run := func(wantError bool) {
		t.Helper()
		err := worker.RunOnce(context.Background())
		if wantError && err == nil {
			t.Fatal("RunOnce() error = nil")
		}
		if !wantError && err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
	}
	run(true) // 12:00, failure 1
	clock.Advance(5*time.Minute - time.Second)
	run(false)
	clock.Advance(time.Second)
	run(true) // 12:05, failure 2
	clock.Advance(15*time.Minute - time.Second)
	run(false)
	clock.Advance(time.Second)
	run(true) // 12:20, failure 3
	clock.Advance(time.Hour - time.Second)
	run(false)
	clock.Advance(time.Second)
	run(true)                                   // 13:20, failure 4
	clock.Advance(4*time.Hour + 40*time.Minute) // 18:00; steady is independently due
	run(false)
	clock.Advance(time.Hour + 20*time.Minute - time.Second)
	run(false)
	clock.Advance(time.Second)
	run(false) // 19:20, failing succeeds
	clock.Advance(6*time.Hour - time.Second)
	run(false)
	clock.Advance(time.Second)
	run(false) // success restored the normal six-hour interval

	wantFailing := []time.Time{
		now,
		now.Add(5 * time.Minute),
		now.Add(20 * time.Minute),
		now.Add(80 * time.Minute),
		now.Add(7*time.Hour + 20*time.Minute),
		now.Add(13*time.Hour + 20*time.Minute),
	}
	if len(failing.calls) != len(wantFailing) {
		t.Fatalf("failing reconcile calls = %v, want %v", failing.calls, wantFailing)
	}
	for index := range wantFailing {
		if !failing.calls[index].Equal(wantFailing[index]) {
			t.Fatalf("failing reconcile calls = %v, want %v", failing.calls, wantFailing)
		}
	}
	if len(steady.calls) != 3 || !steady.calls[0].Equal(now) || !steady.calls[1].Equal(now.Add(6*time.Hour)) || !steady.calls[2].Equal(now.Add(13*time.Hour+20*time.Minute-time.Second)) {
		t.Fatalf("steady reconcile calls = %v", steady.calls)
	}

	records := workerLogRecords(t, logs.String())
	failed := findWorkerEvent(t, records, "reconcile.failed")
	if failed["attempt"] != float64(1) || failed["retry_at"] != now.Add(5*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("first reconcile failure fields = %#v", failed)
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "/private/media") || strings.Contains(logs.String(), "/api/v3/history") {
		t.Fatalf("reconciliation log leaked upstream detail: %s", logs.String())
	}
}

func TestRunWakeFillsFreeSlotBeforeActiveWorkflowCompletes(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := newControlledWorkflow()
	wake := make(chan struct{}, 1)
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.MaxWorkflows = 2
	worker.PollInterval = time.Hour
	worker.Wake = wake
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	first := waitForWorkflowStart(t, service.started)
	repository.enqueueSearch(testSearchLease(2, now))
	wake <- struct{}{}
	second := waitForWorkflowStart(t, service.started)
	if first == second {
		t.Fatalf("started media IDs = %d and %d", first, second)
	}
	service.release(first)
	service.release(second)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if service.maximumActive() > 2 {
		t.Fatalf("maximum active workflows = %d, want at most 2", service.maximumActive())
	}
}

func TestRunCompletionRefillsSingleSlotWithoutWaitingForPoll(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := newControlledWorkflow()
	wake := make(chan struct{}, 1)
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.MaxWorkflows = 1
	worker.PollInterval = time.Hour
	worker.Wake = wake
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	first := waitForWorkflowStart(t, service.started)
	repository.enqueueSearch(testSearchLease(2, now))
	wake <- struct{}{}
	service.release(first)
	second := waitForWorkflowStart(t, service.started)
	if second == first {
		t.Fatalf("completion restarted media %d instead of queued media", first)
	}
	service.release(second)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if service.maximumActive() != 1 {
		t.Fatalf("maximum active workflows = %d, want 1", service.maximumActive())
	}
}

func TestRunRecoveryPollFindsSearchWithoutWake(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(0, now)
	repository.leaseCalls = make(chan struct{}, 8)
	service := newControlledWorkflow()
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.MaxWorkflows = 1
	worker.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-repository.leaseCalls:
	case <-time.After(time.Second):
		t.Fatal("startup dispatch did not check persisted searches")
	}
	repository.enqueueSearch(testSearchLease(2, now))
	started := waitForWorkflowStart(t, service.started)
	service.release(started)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunAllowsBoundedGracefulDrain(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	started := make(chan struct{})
	release := make(chan struct{})
	service := &workerWorkflow{started: started, release: release, outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.PollInterval = time.Hour
	worker.ShutdownTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("worker exited before active workflow drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunCancelsWorkflowAfterDrainTimeout(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	started := make(chan struct{})
	service := &workerWorkflow{started: started, release: make(chan struct{})}
	worker := testWorker(repository, service, testutil.NewClock(now))
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	worker.Events = events
	worker.ShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel active workflow after drain timeout")
	}
	findWorkerEvent(t, workerLogRecords(t, logs.String()), "worker.drain_timed_out")
}

func TestRunReturnsAfterCanceledDrainDeadline(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := &stubbornWorkflow{started: make(chan struct{}), release: make(chan struct{})}
	worker := testWorker(repository, service, testutil.NewClock(now))
	var logs synchronizedBuffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	worker.Events = events
	worker.ShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-service.started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		close(service.release)
		<-done
		t.Fatal("worker remained blocked after canceled drain deadline")
	}
	close(service.release)
	findWorkerEvent(t, workerLogRecords(t, logs.String()), "worker.drain_timed_out")
	findWorkerEvent(t, workerLogRecords(t, logs.String()), "worker.drain_abandoned")
}

func TestDrainReturnsAfterCanceledDeadline(t *testing.T) {
	worker := &Worker{ShutdownTimeout: 20 * time.Millisecond, Events: observability.Discard()}
	done := make(chan error, 1)
	canceled := make(chan struct{})
	returned := make(chan error, 1)
	go func() { returned <- worker.drain(done, func() { close(canceled) }) }()
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("drain did not cancel after graceful deadline")
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(150 * time.Millisecond):
		done <- nil
		<-returned
		t.Fatal("drain remained blocked after canceled deadline")
	}
}

func testWorker(repository *workerRepository, service Workflow, clock *testutil.Clock) *Worker {
	return &Worker{Repository: repository, Workflow: service, Clock: clock, PollInterval: 10 * time.Millisecond, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute, ShutdownTimeout: time.Second, SearchBatch: 10, MaxWorkflows: 2, NotificationBatch: 10}
}

type workerRepository struct {
	mu                      sync.Mutex
	searches                []store.SearchLease
	media                   map[int64]domain.Media
	searchCompletions       []store.SearchCompletion
	searchRenewals          int
	completionDelay         time.Duration
	completingSearch        map[string]bool
	renewedDuringCompletion bool
	completeErrors          []error
	completionResults       []store.SearchCompletionResult
	enqueued                []store.NotificationRequest
	dedupe                  map[string]bool
	notifications           []store.NotificationLease
	notificationCompletions []store.NotificationCompletion
	leaseCalls              chan struct{}
	searchLeaseCalls        int
	pausedRoutes            []store.RouteKey
}

func newWorkerRepository(searches int, now time.Time) *workerRepository {
	repository := &workerRepository{media: map[int64]domain.Media{}, dedupe: map[string]bool{}}
	for index := 1; index <= searches; index++ {
		repository.searches = append(repository.searches, store.SearchLease{MediaID: int64(index), Language: "en", JobID: "job-" + time.Duration(index).String(), LeaseUntil: now.Add(5 * time.Minute)})
		repository.media[int64(index)] = domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: int64(index)}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: int64(index), Size: 100, ModTime: now}, Title: "Movie"}
	}
	if searches == 0 {
		repository.media[1] = domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: 1}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: 1, Size: 100, ModTime: now}, Title: "Movie"}
	}
	return repository
}

func (r *workerRepository) LeaseDueSearchesExcept(_ context.Context, _ time.Time, limit int, _ time.Duration, paused []store.RouteKey) ([]store.SearchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchLeaseCalls++
	r.pausedRoutes = append([]store.RouteKey(nil), paused...)
	if r.leaseCalls != nil {
		select {
		case r.leaseCalls <- struct{}{}:
		default:
		}
	}
	var result, remaining []store.SearchLease
	for _, lease := range r.searches {
		route := store.RouteKey{Language: domain.Language(lease.Language), Kind: r.media[lease.MediaID].Ref.Kind}
		if len(result) < limit && !slices.Contains(paused, route) {
			result = append(result, lease)
		} else {
			remaining = append(remaining, lease)
		}
	}
	r.searches = remaining
	return result, nil
}

func (r *workerRepository) enqueueSearch(lease store.SearchLease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searches = append(r.searches, lease)
	r.media[lease.MediaID] = domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: lease.MediaID}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: lease.MediaID, Size: 100, ModTime: lease.LeaseUntil.Add(-5 * time.Minute)}, Title: "Movie"}
}
func (r *workerRepository) RenewSearchLease(_ context.Context, jobID string, _ time.Time, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchRenewals++
	if r.completingSearch[jobID] {
		r.renewedDuringCompletion = true
		return errors.New("lease already completing")
	}
	return nil
}
func (r *workerRepository) CompleteSearch(_ context.Context, completion store.SearchCompletion) (store.SearchCompletionResult, error) {
	r.mu.Lock()
	if r.completingSearch == nil {
		r.completingSearch = make(map[string]bool)
	}
	r.completingSearch[completion.JobID] = true
	r.mu.Unlock()
	if r.completionDelay > 0 {
		time.Sleep(r.completionDelay)
	}
	r.mu.Lock()
	if len(r.completeErrors) > 0 {
		err := r.completeErrors[0]
		r.completeErrors = r.completeErrors[1:]
		delete(r.completingSearch, completion.JobID)
		r.mu.Unlock()
		return store.SearchCompletionResult{}, err
	}
	r.searchCompletions = append(r.searchCompletions, completion)
	result := store.SearchCompletionResult{}
	if completion.PreserveResume {
		result.ResumeProviderCount = len(completion.ResumeProviders)
	}
	if len(r.completionResults) > 0 {
		result = r.completionResults[0]
		r.completionResults = r.completionResults[1:]
	}
	delete(r.completingSearch, completion.JobID)
	r.mu.Unlock()
	return result, nil
}
func (r *workerRepository) GetMedia(_ context.Context, mediaID int64) (domain.Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.media[mediaID], nil
}
func (r *workerRepository) EnqueueNotification(_ context.Context, request store.NotificationRequest) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedupe[request.DedupeKey] {
		return false, nil
	}
	r.dedupe[request.DedupeKey] = true
	r.enqueued = append(r.enqueued, request)
	r.notifications = append(r.notifications, store.NotificationLease{ID: int64(len(r.notifications) + 1), Notifier: request.Notifier, PayloadJSON: request.PayloadJSON, JobID: "notification-new"})
	return true, nil
}
func (r *workerRepository) LeaseDueNotifications(context.Context, time.Time, int, time.Duration) ([]store.NotificationLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]store.NotificationLease(nil), r.notifications...)
	r.notifications = nil
	return result, nil
}
func (r *workerRepository) RenewNotificationLease(context.Context, string, time.Time, time.Duration) error {
	return nil
}
func (r *workerRepository) CompleteNotification(_ context.Context, completion store.NotificationCompletion) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notificationCompletions = append(r.notificationCompletions, completion)
	return nil
}

type workerWorkflow struct {
	mu              sync.Mutex
	active          int
	maxActive       int
	calls           int
	installOutcomes int
	delay           time.Duration
	started         chan struct{}
	release         <-chan struct{}
	outcome         workflow.Result
	outcomes        []workflow.Result
	err             error
	requests        []workflow.Request
}

type stubbornWorkflow struct {
	started chan struct{}
	release chan struct{}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(payload)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (w *stubbornWorkflow) Run(context.Context, workflow.Request) (workflow.Result, error) {
	close(w.started)
	<-w.release
	return workflow.Result{Outcome: workflow.OutcomeSatisfied}, nil
}

type controlledWorkflow struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan int64
	releases  map[int64]chan struct{}
}

func newControlledWorkflow() *controlledWorkflow {
	return &controlledWorkflow{started: make(chan int64, 8), releases: make(map[int64]chan struct{})}
}

func (w *controlledWorkflow) Run(ctx context.Context, request workflow.Request) (workflow.Result, error) {
	w.mu.Lock()
	release := make(chan struct{})
	w.releases[request.MediaID] = release
	w.active++
	if w.active > w.maxActive {
		w.maxActive = w.active
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active--
		delete(w.releases, request.MediaID)
		w.mu.Unlock()
	}()
	w.started <- request.MediaID
	select {
	case <-release:
	case <-ctx.Done():
		return workflow.Result{}, ctx.Err()
	}
	return workflow.Result{Outcome: workflow.OutcomeSatisfied}, nil
}

func (w *controlledWorkflow) release(mediaID int64) {
	w.mu.Lock()
	release := w.releases[mediaID]
	w.mu.Unlock()
	close(release)
}

func (w *controlledWorkflow) maximumActive() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxActive
}

func testSearchLease(mediaID int64, now time.Time) store.SearchLease {
	return store.SearchLease{MediaID: mediaID, Language: "en", JobID: fmt.Sprintf("job-%d", mediaID), LeaseUntil: now.Add(5 * time.Minute), Priority: store.SearchPriorityImport}
}

func waitForWorkflowStart(t *testing.T, started <-chan int64) int64 {
	t.Helper()
	select {
	case mediaID := <-started:
		return mediaID
	case <-time.After(time.Second):
		t.Fatal("workflow did not start")
		return 0
	}
}

func (w *workerWorkflow) Run(ctx context.Context, request workflow.Request) (workflow.Result, error) {
	w.mu.Lock()
	w.active++
	w.calls++
	request.ResumeProviders = slices.Clone(request.ResumeProviders)
	w.requests = append(w.requests, request)
	call := w.calls
	result := w.outcome
	if call <= len(w.outcomes) {
		result = w.outcomes[call-1]
	}
	if w.active > w.maxActive {
		w.maxActive = w.active
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active--
		w.mu.Unlock()
	}()
	if w.started != nil {
		select {
		case w.started <- struct{}{}:
		default:
		}
	}
	if w.delay > 0 {
		select {
		case <-time.After(w.delay):
		case <-ctx.Done():
			return workflow.Result{}, ctx.Err()
		}
	}
	if w.release != nil {
		select {
		case <-w.release:
		case <-ctx.Done():
			return workflow.Result{}, ctx.Err()
		}
	}
	if result.Outcome == workflow.OutcomeInstalled {
		w.mu.Lock()
		w.installOutcomes++
		w.mu.Unlock()
	}
	return result, w.err
}

type workerNotifier struct {
	calls int
	err   error
}

func (n *workerNotifier) SubtitleChanged(context.Context, domain.Media, string) error {
	n.calls++
	return n.err
}

type workerReconciler struct{ calls int }

func (r *workerReconciler) Run(context.Context) error {
	r.calls++
	return nil
}

type sequenceReconciler struct {
	clock *testutil.Clock
	errs  []error
	calls []time.Time
}

func (r *sequenceReconciler) Run(context.Context) error {
	r.calls = append(r.calls, r.clock.Now())
	if len(r.errs) == 0 {
		return nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return err
}

func notificationJSON(t *testing.T, media domain.Media, subtitlePath string) []byte {
	t.Helper()
	payload, err := json.Marshal(workflow.NotificationPayload{Media: media, SubtitlePath: subtitlePath})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func workerLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func findWorkerEvent(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["event"] == event {
			return record
		}
	}
	t.Fatalf("event %q not found in %#v", event, records)
	return nil
}

func TestUpgradeCompletionAdvancesOnlyUnchangedChecks(t *testing.T) {
	now := time.Now()
	w := &Worker{Clock: testutil.NewClock(now), RandomUnit: func() float64 { return 0.5 }}
	lease := store.SearchLease{JobID: "upgrade", Priority: store.SearchPriorityUpgrade, Attempt: 3}
	retained, err := w.workflowCompletion(lease, workflow.Result{Outcome: workflow.OutcomeSatisfied, NextUpgrade: now.Add(90 * 24 * time.Hour)})
	if err != nil || !retained.AdvanceUpgradeAttempt || retained.ResetMissingAttempt || retained.Priority != store.SearchPriorityUpgrade {
		t.Fatalf("retained=%#v err=%v", retained, err)
	}
	missing, err := w.workflowCompletion(lease, workflow.Result{Outcome: workflow.OutcomeNoResult})
	if err != nil || !missing.ResetMissingAttempt || !missing.NextAttemptAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("missing=%#v err=%v", missing, err)
	}
	installed, err := w.workflowCompletion(lease, workflow.Result{Outcome: workflow.OutcomeInstalled, NextUpgrade: now.Add(7 * 24 * time.Hour)})
	if err != nil || !installed.ResetMissingAttempt || installed.AdvanceUpgradeAttempt {
		t.Fatalf("installed=%#v err=%v", installed, err)
	}
}

func TestUpgradeLeasePassesPersistedAttemptToWorkflow(t *testing.T) {
	now := time.Now()
	repo := newWorkerRepository(1, now)
	repo.searches[0].Priority = store.SearchPriorityUpgrade
	repo.searches[0].Attempt = 2
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied, NextUpgrade: now.Add(60 * 24 * time.Hour)}}
	w := testWorker(repo, service, testutil.NewClock(now))
	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(service.requests) != 1 || service.requests[0].UpgradeAttempt != 3 {
		t.Fatalf("requests=%#v", service.requests)
	}
}

func TestProviderSearchRetriesUseRandomJitterByDefault(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	w := &Worker{Clock: testutil.NewClock(now)}
	for _, tc := range []struct {
		name             string
		result           workflow.Result
		minimum, maximum time.Duration
	}{
		{"missing", workflow.Result{Outcome: workflow.OutcomeNoResult}, 27 * time.Minute, 33 * time.Minute},
		{"cooldown", workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: now.Add(10 * time.Minute)}, 10 * time.Minute, 11 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[time.Time]bool{}
			for i := 0; i < 32; i++ {
				completion, err := w.workflowCompletion(store.SearchLease{JobID: "job", Priority: store.SearchPriorityMissing}, tc.result)
				if err != nil {
					t.Fatal(err)
				}
				delay := completion.NextAttemptAt.Sub(now)
				if delay < tc.minimum || delay > tc.maximum {
					t.Fatalf("retry delay %s outside [%s, %s]", delay, tc.minimum, tc.maximum)
				}
				seen[completion.NextAttemptAt] = true
			}
			if len(seen) == 1 {
				t.Fatal("default provider retry times are fixed, not jittered")
			}
		})
	}
}
