package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/schedule"
	"subsyncd/internal/store"
	"subsyncd/internal/workflow"
)

const (
	defaultPollInterval      = 30 * time.Second
	defaultLeaseDuration     = 5 * time.Minute
	defaultRenewInterval     = time.Minute
	defaultShutdownTimeout   = 30 * time.Second
	defaultReconcileInterval = 6 * time.Hour
	defaultSearchBatch       = 10
	defaultMaxWorkflows      = 2
	defaultNotificationBatch = 10
)

type Clock interface{ Now() time.Time }

type Workflow interface {
	Run(context.Context, workflow.Request) (workflow.Result, error)
}

type Reconciler interface {
	Run(context.Context) error
}

type Repository interface {
	LeaseDueSearches(context.Context, time.Time, int, time.Duration) ([]store.SearchLease, error)
	RenewSearchLease(context.Context, string, time.Time, time.Duration) error
	CompleteSearch(context.Context, store.SearchCompletion) error
	GetMedia(context.Context, int64) (domain.Media, error)
	EnqueueNotification(context.Context, store.NotificationRequest) error
	LeaseDueNotifications(context.Context, time.Time, int, time.Duration) ([]store.NotificationLease, error)
	RenewNotificationLease(context.Context, string, time.Time, time.Duration) error
	CompleteNotification(context.Context, store.NotificationCompletion) error
}

type Worker struct {
	Repository        Repository
	Workflow          Workflow
	Clock             Clock
	Notifiers         map[string]notifier.Notifier
	Reconcilers       map[string]Reconciler
	RandomUnit        func() float64
	OnError           func(error)
	PollInterval      time.Duration
	LeaseDuration     time.Duration
	RenewInterval     time.Duration
	ShutdownTimeout   time.Duration
	ReconcileInterval time.Duration
	SearchBatch       int
	MaxWorkflows      int
	NotificationBatch int
	Wake              <-chan struct{}

	reconcileMu   sync.Mutex
	lastReconcile map[string]time.Time
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.prepare(); err != nil {
		return err
	}
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	for {
		cycleDone := make(chan error, 1)
		go func() { cycleDone <- w.RunOnce(workCtx) }()
		select {
		case err := <-cycleDone:
			w.report(err)
		case <-ctx.Done():
			return w.drain(cycleDone, cancelWork)
		}

		timer := time.NewTimer(w.pollDelay())
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	if err := w.prepare(); err != nil {
		return err
	}
	var failures []error
	if err := w.reconcileDue(ctx); err != nil {
		failures = append(failures, err)
	}
	searches, err := w.Repository.LeaseDueSearches(ctx, w.Clock.Now(), w.SearchBatch, w.LeaseDuration)
	if err != nil {
		failures = append(failures, err)
	} else if err := w.processSearches(ctx, searches); err != nil {
		failures = append(failures, err)
	}
	notifications, err := w.Repository.LeaseDueNotifications(ctx, w.Clock.Now(), w.NotificationBatch, w.LeaseDuration)
	if err != nil {
		failures = append(failures, err)
	} else if err := w.processNotifications(ctx, notifications); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (w *Worker) processSearches(ctx context.Context, leases []store.SearchLease) error {
	semaphore := make(chan struct{}, w.MaxWorkflows)
	errorsByIndex := make([]error, len(leases))
	var wait sync.WaitGroup
	for index, lease := range leases {
		wait.Add(1)
		go func(index int, lease store.SearchLease) {
			defer wait.Done()
			errorsByIndex[index] = w.processSearchLease(ctx, lease, semaphore)
		}(index, lease)
	}
	wait.Wait()
	return errors.Join(errorsByIndex...)
}

func (w *Worker) processSearchLease(ctx context.Context, lease store.SearchLease, semaphore chan struct{}) error {
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	renewal := w.renewSearchLease(jobCtx, cancelJob, lease.JobID)
	select {
	case semaphore <- struct{}{}:
	case <-jobCtx.Done():
		return errors.Join(renewal.finish(), jobCtx.Err())
	}
	media, err := w.Repository.GetMedia(jobCtx, lease.MediaID)
	var result workflow.Result
	if err == nil {
		result, err = w.Workflow.Run(jobCtx, workflow.Request{MediaID: lease.MediaID, Media: media, Language: domain.Language(lease.Language)})
	}
	<-semaphore
	if renewErr := renewal.finish(); renewErr != nil {
		return renewErr
	}
	if jobCtx.Err() != nil {
		return jobCtx.Err()
	}
	if err != nil {
		completion := schedule.Scheduler{Clock: w.Clock}.Failure(lease.JobID, lease.FailureAttempt, "workflow_error")
		return errors.Join(err, w.Repository.CompleteSearch(jobCtx, completion))
	}
	return w.completeWorkflow(jobCtx, lease, media, result)
}

func (w *Worker) completeWorkflow(ctx context.Context, lease store.SearchLease, media domain.Media, result workflow.Result) error {
	completion := store.SearchCompletion{JobID: lease.JobID, Outcome: string(result.Outcome)}
	switch result.Outcome {
	case workflow.OutcomeSatisfied:
		completion.NextAttemptAt = result.NextUpgrade
		if !result.NextUpgrade.IsZero() {
			completion.Priority = store.SearchPriorityUpgrade
		}
		completion.ResetMissingAttempt = true
		completion.ResetFailureAttempt = true
	case workflow.OutcomeInstalled:
		if err := w.enqueueNotifications(ctx, media, result.Installation); err != nil {
			return err
		}
		completion.NextAttemptAt = result.NextUpgrade
		if !result.NextUpgrade.IsZero() {
			completion.Priority = store.SearchPriorityUpgrade
		}
		completion.ResetMissingAttempt = true
		completion.ResetFailureAttempt = true
	case workflow.OutcomeNoResult, workflow.OutcomeRejected:
		completion = schedule.Scheduler{Clock: w.Clock, RandomUnit: w.RandomUnit}.Missing(lease.JobID, lease.Attempt+1)
		completion.Outcome = string(result.Outcome)
		completion.Priority = store.SearchPriorityMissing
	case workflow.OutcomeThrottled:
		completion.NextAttemptAt = w.throttleRetryAt(result.RetryAt)
	default:
		return fmt.Errorf("workflow returned unsupported outcome %q", result.Outcome)
	}
	return w.Repository.CompleteSearch(ctx, completion)
}

func (w *Worker) enqueueNotifications(ctx context.Context, media domain.Media, installation store.Installation) error {
	if len(w.Notifiers) == 0 {
		return nil
	}
	payload, err := json.Marshal(notificationPayload{Media: media, SubtitlePath: installation.Path})
	if err != nil {
		return fmt.Errorf("encode notification payload: %w", err)
	}
	names := make([]string, 0, len(w.Notifiers))
	for name := range w.Notifiers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rawKey := fmt.Sprintf("%s\x00%d\x00%s\x00%s", name, installation.MediaID, installation.Language, installation.Checksum)
		sum := sha256.Sum256([]byte(rawKey))
		request := store.NotificationRequest{Notifier: name, DedupeKey: hex.EncodeToString(sum[:]), PayloadJSON: payload, NextAttemptAt: w.Clock.Now()}
		if err := w.Repository.EnqueueNotification(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) processNotifications(ctx context.Context, leases []store.NotificationLease) error {
	semaphore := make(chan struct{}, w.MaxWorkflows)
	errorsByIndex := make([]error, len(leases))
	var wait sync.WaitGroup
	for index, lease := range leases {
		wait.Add(1)
		go func(index int, lease store.NotificationLease) {
			defer wait.Done()
			errorsByIndex[index] = w.processNotificationLease(ctx, lease, semaphore)
		}(index, lease)
	}
	wait.Wait()
	return errors.Join(errorsByIndex...)
}

func (w *Worker) processNotificationLease(ctx context.Context, lease store.NotificationLease, semaphore chan struct{}) error {
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	renewal := w.renewNotificationLease(jobCtx, cancelJob, lease.JobID)
	select {
	case semaphore <- struct{}{}:
	case <-jobCtx.Done():
		return errors.Join(renewal.finish(), jobCtx.Err())
	}
	var payload notificationPayload
	err := json.Unmarshal(lease.PayloadJSON, &payload)
	destination := w.Notifiers[lease.Notifier]
	if err == nil && destination == nil {
		err = fmt.Errorf("notifier %q is not configured", lease.Notifier)
	}
	if err == nil {
		err = destination.SubtitleChanged(jobCtx, payload.Media, payload.SubtitlePath)
	}
	<-semaphore
	if renewErr := renewal.finish(); renewErr != nil {
		return renewErr
	}
	if jobCtx.Err() != nil {
		return jobCtx.Err()
	}
	deliveryErr := err
	completion := store.NotificationCompletion{JobID: lease.JobID, Result: "success"}
	if deliveryErr != nil {
		completion.Result = "permanent_error"
		if notifier.IsRetryable(deliveryErr) {
			completion.Result = "retryable_error"
			completion.NextAttemptAt = w.Clock.Now().Add(schedule.FailureDelay(lease.Attempt))
		}
	}
	return w.Repository.CompleteNotification(jobCtx, completion)
}

func (w *Worker) renewSearchLease(ctx context.Context, cancel context.CancelFunc, jobID string) leaseRenewal {
	return w.renew(ctx, cancel, func(ctx context.Context) error {
		return w.Repository.RenewSearchLease(ctx, jobID, w.Clock.Now(), w.LeaseDuration)
	})
}

func (w *Worker) renewNotificationLease(ctx context.Context, cancel context.CancelFunc, jobID string) leaseRenewal {
	return w.renew(ctx, cancel, func(ctx context.Context) error {
		return w.Repository.RenewNotificationLease(ctx, jobID, w.Clock.Now(), w.LeaseDuration)
	})
}

func (w *Worker) renew(ctx context.Context, cancelJob context.CancelFunc, renew func(context.Context) error) leaseRenewal {
	renewCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(w.RenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := renew(renewCtx); err != nil {
					done <- err
					cancelJob()
					return
				}
			}
		}
	}()
	return leaseRenewal{stop: stop, done: done}
}

func (w *Worker) reconcileDue(ctx context.Context) error {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.lastReconcile == nil {
		w.lastReconcile = make(map[string]time.Time)
	}
	names := make([]string, 0, len(w.Reconcilers))
	for name := range w.Reconcilers {
		names = append(names, name)
	}
	sort.Strings(names)
	var failures []error
	now := w.Clock.Now()
	for _, name := range names {
		last := w.lastReconcile[name]
		if !last.IsZero() && now.Sub(last) < w.ReconcileInterval {
			continue
		}
		if err := w.Reconcilers[name].Run(ctx); err != nil {
			failures = append(failures, fmt.Errorf("reconcile %s: %w", name, err))
			continue
		}
		w.lastReconcile[name] = now
	}
	return errors.Join(failures...)
}

func (w *Worker) throttleRetryAt(reset time.Time) time.Time {
	if reset.IsZero() {
		return time.Time{}
	}
	delay := reset.Sub(w.Clock.Now())
	if delay <= 0 {
		return w.Clock.Now()
	}
	unit := 0.5
	if w.RandomUnit != nil {
		unit = w.RandomUnit()
	}
	if unit < 0 {
		unit = 0
	} else if unit > 1 {
		unit = 1
	}
	return reset.Add(time.Duration(float64(delay) * 0.1 * unit))
}

func (w *Worker) pollDelay() time.Duration {
	unit := 0.5
	if w.RandomUnit != nil {
		unit = w.RandomUnit()
	}
	if unit < 0 {
		unit = 0
	} else if unit > 1 {
		unit = 1
	}
	return time.Duration(float64(w.PollInterval) * (0.9 + 0.2*unit))
}

func (w *Worker) prepare() error {
	if w.Repository == nil || w.Workflow == nil || w.Clock == nil {
		return fmt.Errorf("worker repository, workflow, and clock are required")
	}
	if w.PollInterval <= 0 {
		w.PollInterval = defaultPollInterval
	}
	if w.LeaseDuration <= 0 {
		w.LeaseDuration = defaultLeaseDuration
	}
	if w.RenewInterval <= 0 {
		w.RenewInterval = defaultRenewInterval
	}
	if w.RenewInterval >= w.LeaseDuration {
		return fmt.Errorf("worker renewal interval must be shorter than lease duration")
	}
	if w.ShutdownTimeout <= 0 {
		w.ShutdownTimeout = defaultShutdownTimeout
	}
	if w.ReconcileInterval <= 0 {
		w.ReconcileInterval = defaultReconcileInterval
	}
	if w.SearchBatch <= 0 {
		w.SearchBatch = defaultSearchBatch
	}
	if w.SearchBatch > defaultSearchBatch {
		w.SearchBatch = defaultSearchBatch
	}
	if w.MaxWorkflows <= 0 {
		w.MaxWorkflows = defaultMaxWorkflows
	}
	if w.MaxWorkflows > defaultMaxWorkflows {
		w.MaxWorkflows = defaultMaxWorkflows
	}
	if w.NotificationBatch <= 0 {
		w.NotificationBatch = defaultNotificationBatch
	}
	if w.NotificationBatch > defaultNotificationBatch {
		w.NotificationBatch = defaultNotificationBatch
	}
	return nil
}

func (w *Worker) drain(done <-chan error, cancel context.CancelFunc) error {
	timer := time.NewTimer(w.ShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		w.report(err)
		return nil
	case <-timer.C:
		cancel()
		err := <-done
		if err != nil && !errors.Is(err, context.Canceled) {
			w.report(err)
		}
		return nil
	}
}

func (w *Worker) report(err error) {
	if err != nil && w.OnError != nil {
		w.OnError(err)
	}
}

type notificationPayload struct {
	Media        domain.Media `json:"media"`
	SubtitlePath string       `json:"subtitle_path"`
}

type leaseRenewal struct {
	stop context.CancelFunc
	done <-chan error
}

func (r leaseRenewal) finish() error {
	r.stop()
	return <-r.done
}
