package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

func TestRunOnceLogsCorrelatedSearchJobLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.searches[0].Priority = store.SearchPriorityImport
	var logs bytes.Buffer
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
	if _, ok := completed["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms = %#v", completed["duration_ms"])
	}
}

func TestRunOnceLogsTechnicalRetryAndSameKeyRerun(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	repository.completionResults = []store.SearchCompletionResult{{RerunScheduled: true}}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "sanitized" }})
	if err != nil {
		t.Fatal(err)
	}
	worker := testWorker(repository, &workerWorkflow{err: errors.New("secret failure")}, testutil.NewClock(now))
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
	media := domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: 1}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: 1, Size: 100, ModTime: now}, Title: "Movie"}
	mediaID, _, err := repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.UpsertSearchState(context.Background(), mediaID, "en", now); err != nil {
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

func TestRunOncePersistsAndDeliversNotificationAfterInstallation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeInstalled, Installation: store.Installation{MediaID: 1, Language: "en", Path: "/media/movie.en.srt", Checksum: "sum"}}}
	delivery := &workerNotifier{}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.Notifiers = map[string]notifier.Notifier{"silo": delivery}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.enqueued) != 1 || delivery.calls != 1 || len(repository.notificationCompletions) != 1 || repository.notificationCompletions[0].Result != "success" {
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
	completingSearch        bool
	renewedDuringCompletion bool
	completeErrors          []error
	completionResults       []store.SearchCompletionResult
	enqueued                []store.NotificationRequest
	dedupe                  map[string]bool
	notifications           []store.NotificationLease
	notificationCompletions []store.NotificationCompletion
	leaseCalls              chan struct{}
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

func (r *workerRepository) LeaseDueSearches(_ context.Context, _ time.Time, limit int, _ time.Duration) ([]store.SearchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leaseCalls != nil {
		select {
		case r.leaseCalls <- struct{}{}:
		default:
		}
	}
	if limit > len(r.searches) {
		limit = len(r.searches)
	}
	result := append([]store.SearchLease(nil), r.searches[:limit]...)
	r.searches = append([]store.SearchLease(nil), r.searches[limit:]...)
	return result, nil
}

func (r *workerRepository) enqueueSearch(lease store.SearchLease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searches = append(r.searches, lease)
	r.media[lease.MediaID] = domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: lease.MediaID}, Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: lease.MediaID, Size: 100, ModTime: lease.LeaseUntil.Add(-5 * time.Minute)}, Title: "Movie"}
}
func (r *workerRepository) RenewSearchLease(context.Context, string, time.Time, time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchRenewals++
	if r.completingSearch {
		r.renewedDuringCompletion = true
		return errors.New("lease already completing")
	}
	return nil
}
func (r *workerRepository) CompleteSearch(_ context.Context, completion store.SearchCompletion) (store.SearchCompletionResult, error) {
	r.mu.Lock()
	r.completingSearch = true
	r.mu.Unlock()
	if r.completionDelay > 0 {
		time.Sleep(r.completionDelay)
	}
	r.mu.Lock()
	if len(r.completeErrors) > 0 {
		err := r.completeErrors[0]
		r.completeErrors = r.completeErrors[1:]
		r.completingSearch = false
		r.mu.Unlock()
		return store.SearchCompletionResult{}, err
	}
	r.searchCompletions = append(r.searchCompletions, completion)
	result := store.SearchCompletionResult{}
	if len(r.completionResults) > 0 {
		result = r.completionResults[0]
		r.completionResults = r.completionResults[1:]
	}
	r.completingSearch = false
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

func (w *workerWorkflow) Run(ctx context.Context, _ workflow.Request) (workflow.Result, error) {
	w.mu.Lock()
	w.active++
	w.calls++
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

func notificationJSON(t *testing.T, media domain.Media, subtitlePath string) []byte {
	t.Helper()
	payload, err := json.Marshal(notificationPayload{Media: media, SubtitlePath: subtitlePath})
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
