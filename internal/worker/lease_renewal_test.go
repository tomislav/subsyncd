package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

// Block a real renewal call until finish cancels it, rather than relying on
// scheduler timing to reproduce the completion/renewal race.
type cancelingRenewalRepository struct {
	*workerRepository
	started chan struct{}
}

func (r *cancelingRenewalRepository) RenewSearchLease(ctx context.Context, _ string, _ time.Time, _ time.Duration) error {
	close(r.started)
	<-ctx.Done()
	return fmt.Errorf("renew search lease: %w", ctx.Err())
}

func TestInstalledSearchCompletesWhenFinishCancelsInFlightRenewal(t *testing.T) {
	now := time.Now()
	repository := newWorkerRepository(1, now)
	renewing := &cancelingRenewalRepository{workerRepository: repository, started: make(chan struct{})}
	nextUpgrade := now.Add(7 * 24 * time.Hour)
	service := &workerWorkflow{release: renewing.started, outcome: workflow.Result{Outcome: workflow.OutcomeInstalled, NextUpgrade: nextUpgrade}}
	w := testWorker(repository, service, testutil.NewClock(now))
	w.Repository = renewing
	w.RenewInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.runSearchLease(ctx, repository.searches[0]); err != nil {
		t.Fatalf("successful job abandoned: %v", err)
	}
	if len(repository.searchCompletions) != 1 {
		t.Fatalf("completions=%d", len(repository.searchCompletions))
	}
	completion := repository.searchCompletions[0]
	if completion.Outcome != "installed" || !completion.NextAttemptAt.Equal(nextUpgrade) || completion.Priority != store.SearchPriorityUpgrade {
		t.Fatalf("completion=%#v", completion)
	}
}

func TestRenewalFinishPreservesActualErrors(t *testing.T) {
	for _, failure := range []error{errors.New("lease no longer owned"), context.DeadlineExceeded, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &Worker{RenewInterval: time.Millisecond}
			renewal := w.renew(ctx, cancel, func(context.Context) error { return failure })
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("real renewal error did not cancel job")
			}
			if err := renewal.finish(); !errors.Is(err, failure) {
				t.Fatalf("renewal error lost: %v", err)
			}
		})
	}
}

func TestRenewalFinishDoesNotHideConcurrentFailure(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("parentCanceled=%t", parentCanceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			failure := errors.New("database failure")
			w := &Worker{RenewInterval: time.Millisecond}
			renewal := w.renew(ctx, cancel, func(renewCtx context.Context) error {
				close(started)
				<-renewCtx.Done()
				if parentCanceled {
					return fmt.Errorf("renew: %w", renewCtx.Err())
				}
				return failure
			})
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("renewal did not start")
			}
			if parentCanceled {
				cancel()
				failure = context.Canceled
			}
			if err := renewal.finish(); !errors.Is(err, failure) {
				t.Fatalf("concurrent failure hidden: %v", err)
			}
		})
	}
}

func (r *cancelingRenewalRepository) RenewNotificationLease(ctx context.Context, _ string, _ time.Time, _ time.Duration) error {
	close(r.started)
	<-ctx.Done()
	return fmt.Errorf("renew notification lease: %w", ctx.Err())
}

type renewalWaitingNotifier struct{ started <-chan struct{} }

func (n renewalWaitingNotifier) SubtitleChanged(ctx context.Context, _ domain.Media, _ string) error {
	select {
	case <-n.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNotificationCompletesWhenFinishCancelsInFlightRenewal(t *testing.T) {
	now := time.Now()
	repository := newWorkerRepository(1, now)
	renewing := &cancelingRenewalRepository{workerRepository: repository, started: make(chan struct{})}
	w := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	w.Repository = renewing
	w.RenewInterval = time.Millisecond
	w.Notifiers = map[string]notifier.Notifier{"silo": renewalWaitingNotifier{started: renewing.started}}
	lease := store.NotificationLease{ID: 1, Notifier: "silo", PayloadJSON: notificationJSON(t, repository.media[1], "/media/movie.en.srt"), JobID: "notification-1"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.processNotificationGroupContexts(ctx, ctx, w.coalesceNotifications([]store.NotificationLease{lease})[0], make(chan struct{}, 1)); err != nil {
		t.Fatalf("successful notification abandoned: %v", err)
	}
	if len(repository.notificationCompletions) != 1 || repository.notificationCompletions[0].Result != "success" {
		t.Fatalf("completions=%#v", repository.notificationCompletions)
	}
}

type losingRenewalRepository struct {
	*workerRepository
	lostJobID string
	lost      chan struct{}
	once      sync.Once
}

func (r *losingRenewalRepository) RenewNotificationLease(_ context.Context, jobID string, _ time.Time, _ time.Duration) error {
	if jobID != r.lostJobID {
		return nil
	}
	r.once.Do(func() { close(r.lost) })
	return errLeaseLost
}

var errLeaseLost = errors.New("notification lease lost")

func TestLostFollowerLeaseAbandonsOnlyThatFollower(t *testing.T) {
	now := time.Now()
	repository := newWorkerRepository(1, now)
	losing := &losingRenewalRepository{workerRepository: repository, lostJobID: "follower-lost", lost: make(chan struct{})}
	w := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	w.Repository = losing
	w.RenewInterval = time.Millisecond
	delivery := &coalescingNotifier{wait: losing.lost}
	w.Notifiers = map[string]notifier.Notifier{"silo": delivery}
	media := repository.media[1]
	leases := []store.NotificationLease{
		{ID: 1, Notifier: "silo", PayloadJSON: notificationJSON(t, media, "/media/movie.en.srt"), JobID: "leader"},
		{ID: 2, Notifier: "silo", PayloadJSON: notificationJSON(t, media, "/media/movie.hr.srt"), JobID: "follower-lost"},
		{ID: 3, Notifier: "silo", PayloadJSON: notificationJSON(t, media, "/media/movie.de.srt"), JobID: "follower-kept"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := w.processNotificationsContexts(ctx, ctx, leases)
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("lost lease error = %v", err)
	}
	if len(delivery.directories) != 1 {
		t.Fatalf("deliveries = %q", delivery.directories)
	}
	completed := map[string]string{}
	for _, completion := range repository.notificationCompletions {
		completed[completion.JobID] = completion.Result
	}
	if !reflect.DeepEqual(completed, map[string]string{"leader": "success", "follower-kept": "success"}) {
		t.Fatalf("completions = %#v", completed)
	}
}
