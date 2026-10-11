package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"subsyncd/internal/inventory"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func TestThrottleResetInThePastWaitsTheMinimum(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for name, reset := range map[string]time.Time{"past": now.Add(-time.Second), "now": now, "soon": now.Add(10 * time.Second)} {
		t.Run(name, func(t *testing.T) {
			repository := newWorkerRepository(1, now)
			service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: reset}}
			if err := testWorker(repository, service, testutil.NewClock(now)).RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := repository.searchCompletions[0].NextAttemptAt; !got.Equal(now.Add(minimumThrottleDelay)) {
				t.Fatalf("retry at %s, want %s", got, now.Add(minimumThrottleDelay))
			}
		})
	}
}

func TestPreservedResumeKeepsTheOldestAnswerDate(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	saved := now.Add(-2 * time.Hour)
	throttled := workflow.Result{Outcome: workflow.OutcomeThrottled, RetryAt: now.Add(time.Hour), ResumeProviders: []string{"a", "b"}, ResumeRouteSignature: "route"}
	for name, test := range map[string]struct {
		lease store.SearchLease
		want  time.Time
	}{
		"resumed": {store.SearchLease{ResumeProviders: []string{"a"}, ResumeRouteSignature: "route", ResumeSavedAt: saved}, saved},
		"fresh":   {store.SearchLease{}, now},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newWorkerRepository(1, now)
			repository.searches[0].ResumeProviders = test.lease.ResumeProviders
			repository.searches[0].ResumeRouteSignature = test.lease.ResumeRouteSignature
			repository.searches[0].ResumeSavedAt = test.lease.ResumeSavedAt
			if err := testWorker(repository, &workerWorkflow{outcome: throttled}, testutil.NewClock(now)).RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := repository.searchCompletions[0].ResumeSavedAt; !got.Equal(test.want) {
				t.Fatalf("resume saved at %s, want %s", got, test.want)
			}
		})
	}
}

type releasingRepository struct {
	*workerRepository
	mu                    sync.Mutex
	releasedSearches      []string
	releasedNotifications []string
	timings               map[string]store.ReconcileTiming
}

func (r *releasingRepository) ReleaseSearchLease(_ context.Context, jobID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releasedSearches = append(r.releasedSearches, jobID)
	return nil
}

func (r *releasingRepository) ReleaseNotificationLease(_ context.Context, jobID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releasedNotifications = append(r.releasedNotifications, jobID)
	return nil
}

func (r *releasingRepository) ReconcileTimings(context.Context) (map[string]store.ReconcileTiming, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]store.ReconcileTiming, len(r.timings))
	for name, timing := range r.timings {
		copied[name] = timing
	}
	return copied, nil
}

func (r *releasingRepository) RecordReconcileTiming(_ context.Context, name string, timing store.ReconcileTiming) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timings[name] = timing
	return nil
}

func TestShutdownReleasesTheRunningSearchLease(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	repository := &releasingRepository{workerRepository: newWorkerRepository(1, now), timings: map[string]store.ReconcileTiming{}}
	service := newControlledWorkflow()
	worker := &Worker{Repository: repository, Workflow: service, Clock: testutil.NewClock(now), PollInterval: time.Hour, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute, ShutdownTimeout: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-service.started
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if len(repository.releasedSearches) != 1 || repository.releasedSearches[0] != "job-1ns" {
		t.Fatalf("released searches = %v", repository.releasedSearches)
	}
	if len(repository.searchCompletions) != 0 {
		t.Fatalf("canceled search was completed: %+v", repository.searchCompletions)
	}
}

func TestReconcileTimingSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	repository := &releasingRepository{workerRepository: newWorkerRepository(0, now), timings: map[string]store.ReconcileTiming{
		"sonarr": {LastAttempt: now.Add(-time.Hour)},
		"radarr": {LastAttempt: now.Add(-10 * time.Minute), Failures: 1},
	}}
	sonarr, radarr := &workerReconciler{}, &workerReconciler{}
	worker := testWorker(repository.workerRepository, &workerWorkflow{}, clock)
	worker.Repository = repository
	worker.Reconcilers = map[string]Reconciler{"sonarr": sonarr, "radarr": radarr}
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sonarr.calls != 0 {
		t.Fatalf("restart reran a reconciliation from an hour ago")
	}
	if radarr.calls != 1 {
		t.Fatalf("restart forgot the 5-minute failure retry")
	}
	if saved := repository.timings["radarr"]; !saved.LastAttempt.Equal(now) || saved.Failures != 0 {
		t.Fatalf("reconciliation timing not saved: %+v", saved)
	}
	clock.Advance(defaultReconcileInterval - time.Hour)
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sonarr.calls != 1 {
		t.Fatalf("saved timing never came due")
	}
}

func TestOneDeadFileOnAReadableMountDoesNotPauseTheQueue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Show", "other.mkv"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	for name, test := range map[string]struct {
		root      string
		mediaPath string
		paused    bool
	}{
		"dead file in a listed directory":    {root, filepath.Join(root, "Show", "gone.mkv"), false},
		"removed directory on a listed root": {root, filepath.Join(root, "Gone", "gone.mkv"), false},
		"empty mount point":                  {empty, filepath.Join(empty, "Show", "gone.mkv"), true},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
			repository := newWorkerRepository(mediaBreakerThreshold+2, now)
			for id, media := range repository.media {
				media.Fingerprint.Path = test.mediaPath
				repository.media[id] = media
			}
			service := &workerWorkflow{err: missingMedia()}
			worker := testWorker(repository, service, testutil.NewClock(now))
			worker.MediaRoots = []string{test.root}
			worker.SearchBatch = 1
			for range mediaBreakerThreshold + 2 {
				_ = worker.RunOnce(t.Context())
			}
			if paused := service.calls < mediaBreakerThreshold+2; paused != test.paused {
				t.Fatalf("paused = %t after %d calls, want %t", paused, service.calls, test.paused)
			}
		})
	}
}

func TestHungOrFailingReadsPauseTheQueueEvenWhenTheMountLists(t *testing.T) {
	// altmount lists directories from its metadata while reads hang or fail.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(root, "Show", "episode.mkv")
	if err := os.WriteFile(mediaPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"probe timed out": &workflow.MediaUnavailableError{Err: inventory.ErrProbeTimedOut},
		"EIO":             &workflow.MediaUnavailableError{Err: fmt.Errorf("read media: %w", syscall.EIO)},
		"ENOTCONN":        &workflow.MediaUnavailableError{Err: fmt.Errorf("open media: %w", syscall.ENOTCONN)},
		"read timed out":  &workflow.MediaUnavailableError{Err: errors.New("media read timed out")},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
			repository := newWorkerRepository(mediaBreakerThreshold+2, now)
			for id, media := range repository.media {
				media.Fingerprint.Path = mediaPath
				repository.media[id] = media
			}
			service := &workerWorkflow{err: err}
			worker := testWorker(repository, service, testutil.NewClock(now))
			worker.MediaRoots = []string{root}
			worker.SearchBatch = 1
			for range mediaBreakerThreshold + 2 {
				_ = worker.RunOnce(t.Context())
			}
			if service.calls != mediaBreakerThreshold {
				t.Fatalf("workflow calls = %d, want the queue paused after %d", service.calls, mediaBreakerThreshold)
			}
		})
	}
}
