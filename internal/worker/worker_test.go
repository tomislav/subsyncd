package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
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
	if !completion.NextAttemptAt.Equal(reset.Add(30*time.Second)) || completion.AdvanceMissingAttempt || completion.AdvanceFailureAttempt {
		t.Fatalf("throttle completion = %#v", completion)
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
}

func testWorker(repository *workerRepository, service *workerWorkflow, clock *testutil.Clock) *Worker {
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
	enqueued                []store.NotificationRequest
	dedupe                  map[string]bool
	notifications           []store.NotificationLease
	notificationCompletions []store.NotificationCompletion
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

func (r *workerRepository) LeaseDueSearches(context.Context, time.Time, int, time.Duration) ([]store.SearchLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]store.SearchLease(nil), r.searches...)
	r.searches = nil
	return result, nil
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
func (r *workerRepository) CompleteSearch(_ context.Context, completion store.SearchCompletion) error {
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
		return err
	}
	r.searchCompletions = append(r.searchCompletions, completion)
	r.completingSearch = false
	r.mu.Unlock()
	return nil
}
func (r *workerRepository) GetMedia(_ context.Context, mediaID int64) (domain.Media, error) {
	return r.media[mediaID], nil
}
func (r *workerRepository) EnqueueNotification(_ context.Context, request store.NotificationRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedupe[request.DedupeKey] {
		return nil
	}
	r.dedupe[request.DedupeKey] = true
	r.enqueued = append(r.enqueued, request)
	r.notifications = append(r.notifications, store.NotificationLease{ID: int64(len(r.notifications) + 1), Notifier: request.Notifier, PayloadJSON: request.PayloadJSON, JobID: "notification-new"})
	return nil
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
	return result, nil
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
