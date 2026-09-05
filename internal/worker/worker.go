package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
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
	defaultMaxWorkflows      = 1
	maximumWorkflows         = 8
	defaultNotificationBatch = 10
)

type Clock interface{ Now() time.Time }

type Workflow interface {
	Run(context.Context, workflow.Request) (workflow.Result, error)
}

type Reconciler interface {
	Run(context.Context) error
}

type reconcileAttempt struct {
	LastAttempt time.Time
	Failures    int
}

var reconcileFailureDelays = [...]time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
}

type Repository interface {
	LeaseDueSearches(context.Context, time.Time, int, time.Duration) ([]store.SearchLease, error)
	RenewSearchLease(context.Context, string, time.Time, time.Duration) error
	CompleteSearch(context.Context, store.SearchCompletion) (store.SearchCompletionResult, error)
	GetMedia(context.Context, int64) (domain.Media, error)
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
	Events            *observability.Emitter

	reconcileMu       sync.Mutex
	reconcileAttempts map[string]reconcileAttempt
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.prepare(); err != nil {
		return err
	}
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	searchesDone := make(chan searchDone, w.MaxWorkflows)
	maintenanceDone := make(chan error, 1)
	activeSearches := 0
	maintenanceActive := false

	dispatch := func() {
		available := w.MaxWorkflows - activeSearches
		if available <= 0 {
			return
		}
		leases, err := w.Repository.LeaseDueSearches(workCtx, w.Clock.Now(), available, w.LeaseDuration)
		if err != nil {
			w.report(err)
			return
		}
		for _, lease := range leases {
			w.logLease(workCtx, lease)
			activeSearches++
			go func(lease store.SearchLease) {
				searchesDone <- searchDone{err: w.runSearchLease(workCtx, lease)}
			}(lease)
		}
	}
	startMaintenance := func() {
		if maintenanceActive {
			return
		}
		maintenanceActive = true
		go func() { maintenanceDone <- w.runMaintenance(workCtx) }()
	}

	dispatch()
	startMaintenance()
	timer := time.NewTimer(w.pollDelay())
	defer timer.Stop()
	for {
		select {
		case result := <-searchesDone:
			activeSearches--
			w.reportUnlessCanceled(result.err)
			dispatch()
		case err := <-maintenanceDone:
			maintenanceActive = false
			w.reportUnlessCanceled(err)
		case <-w.Wake:
			dispatch()
		case <-timer.C:
			dispatch()
			startMaintenance()
			timer.Reset(w.pollDelay())
		case <-ctx.Done():
			return w.drainDaemon(activeSearches, maintenanceActive, searchesDone, maintenanceDone, cancelWork)
		}
	}
}

type searchDone struct{ err error }

func (w *Worker) runMaintenance(ctx context.Context) error {
	var failures []error
	if err := w.reconcileDue(ctx); err != nil {
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
	} else {
		for _, lease := range searches {
			w.logLease(ctx, lease)
		}
		if err := w.processSearches(ctx, searches); err != nil {
			failures = append(failures, err)
		}
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
	select {
	case semaphore <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-semaphore }()
	return w.runSearchLease(ctx, lease)
}

func (w *Worker) runSearchLease(ctx context.Context, lease store.SearchLease) error {
	started := time.Now()
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	jobCtx = observability.WithAttrs(jobCtx,
		slog.String("job_id", lease.JobID),
		slog.Int64("media_id", lease.MediaID),
		slog.String("language", lease.Language),
		slog.String("priority", lease.Priority.String()),
		slog.Int("attempt", lease.Attempt),
		slog.Int("failure_attempt", lease.FailureAttempt),
	)
	events := w.Events.For("worker")
	media, err := w.Repository.GetMedia(jobCtx, lease.MediaID)
	workflowCtx := jobCtx
	if err == nil {
		jobCtx = observability.WithAttrs(jobCtx,
			slog.String("instance", media.Ref.Instance),
			slog.String("media_kind", string(media.Ref.Kind)),
			slog.Int64("file_id", media.Ref.FileID),
		)
		workflowCtx = jobCtx
		jobCtx = observability.WithAttrs(jobCtx, slog.String("media_title", observability.MediaTitle(media)))
	}
	events.Log(jobCtx, slog.LevelInfo, "job.started", "subtitle job started")
	renewal := w.renewSearchLease(jobCtx, cancelJob, lease.JobID)
	var result workflow.Result
	if err == nil {
		if media.UnsupportedReason != "" {
			if renewErr := renewal.finish(); renewErr != nil {
				events.Log(jobCtx, slog.LevelWarn, "job.lease_lost", "subtitle job lease was lost", events.ErrorAttrs("lease_renewal", renewErr)...)
				return renewErr
			}
			if jobCtx.Err() != nil {
				w.logJobCompleted(jobCtx, slog.LevelInfo, "canceled", "canceled", time.Time{}, started, nil)
				return jobCtx.Err()
			}
			completion := store.SearchCompletion{JobID: lease.JobID, Outcome: string(media.UnsupportedReason)}
			completionResult, completionErr := w.Repository.CompleteSearch(jobCtx, completion)
			if completionErr != nil {
				w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, completionErr)
				return completionErr
			}
			w.logRerun(jobCtx, completionResult)
			w.logJobCompleted(jobCtx, slog.LevelInfo, string(media.UnsupportedReason), string(media.UnsupportedReason), time.Time{}, started, nil)
			return nil
		}
		result, err = w.Workflow.Run(workflowCtx, workflow.Request{MediaID: lease.MediaID, Media: media, Language: domain.Language(lease.Language)})
	}
	if renewErr := renewal.finish(); renewErr != nil {
		events.Log(jobCtx, slog.LevelWarn, "job.lease_lost", "subtitle job lease was lost", events.ErrorAttrs("lease_renewal", renewErr)...)
		return renewErr
	}
	if jobCtx.Err() != nil {
		w.logJobCompleted(jobCtx, slog.LevelInfo, "canceled", "canceled", time.Time{}, started, nil)
		return jobCtx.Err()
	}
	if err != nil {
		completion := schedule.Scheduler{Clock: w.Clock}.Failure(lease.JobID, lease.FailureAttempt, "workflow_error")
		completionResult, completionErr := w.Repository.CompleteSearch(jobCtx, completion)
		if completionErr != nil {
			w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, completionErr)
			return errors.Join(err, completionErr)
		}
		events.Log(jobCtx, slog.LevelWarn, "job.retry_scheduled", "subtitle job retry scheduled",
			slog.String("reason", "workflow_error"),
			slog.Time("retry_at", completion.NextAttemptAt),
			slog.Int("next_failure_attempt", lease.FailureAttempt+1),
		)
		w.logRerun(jobCtx, completionResult)
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "workflow", completion.NextAttemptAt, started, err)
		return err
	}
	completion, err := w.workflowCompletion(lease, result)
	if err != nil {
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "workflow_outcome", time.Time{}, started, err)
		return err
	}
	completionResult, err := w.Repository.CompleteSearch(jobCtx, completion)
	if err != nil {
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, err)
		return err
	}
	w.logRerun(jobCtx, completionResult)
	w.logJobCompleted(jobCtx, slog.LevelInfo, string(result.Outcome), completionReason(result.Outcome), completion.NextAttemptAt, started, nil)
	return nil
}

func (w *Worker) workflowCompletion(lease store.SearchLease, result workflow.Result) (store.SearchCompletion, error) {
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
		return store.SearchCompletion{}, fmt.Errorf("workflow returned unsupported outcome %q", result.Outcome)
	}
	return completion, nil
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
	started := time.Now()
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	jobCtx = observability.WithAttrs(jobCtx,
		slog.String("notification_id", lease.JobID),
		slog.String("notifier", lease.Notifier),
		slog.Int("attempt", lease.Attempt),
	)
	renewal := w.renewNotificationLease(jobCtx, cancelJob, lease.JobID)
	select {
	case semaphore <- struct{}{}:
	case <-jobCtx.Done():
		return errors.Join(renewal.finish(), jobCtx.Err())
	}
	var payload workflow.NotificationPayload
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
	if err := w.Repository.CompleteNotification(jobCtx, completion); err != nil {
		events := w.Events.For("worker")
		events.Log(jobCtx, slog.LevelError, "notification.failed", "notification completion failed",
			append([]slog.Attr{slog.String("outcome", "failed"), slog.Int64("duration_ms", time.Since(started).Milliseconds())}, events.ErrorAttrs("notification_completion", err)...)...)
		return err
	}
	events := w.Events.For("worker")
	switch completion.Result {
	case "success":
		events.Log(jobCtx, slog.LevelInfo, "notification.delivered", "subtitle notification delivered",
			slog.String("outcome", "success"), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	case "retryable_error":
		attrs := []slog.Attr{slog.String("outcome", "retry_scheduled"), slog.Time("retry_at", completion.NextAttemptAt), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
		attrs = append(attrs, events.ErrorAttrs("notification_delivery", deliveryErr)...)
		events.Log(jobCtx, slog.LevelWarn, "notification.retry_scheduled", "subtitle notification retry scheduled", attrs...)
	default:
		attrs := []slog.Attr{slog.String("outcome", "failed"), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
		attrs = append(attrs, events.ErrorAttrs("notification_delivery", deliveryErr)...)
		events.Log(jobCtx, slog.LevelError, "notification.failed", "subtitle notification failed", attrs...)
	}
	return nil
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
	if w.reconcileAttempts == nil {
		w.reconcileAttempts = make(map[string]reconcileAttempt)
	}
	names := make([]string, 0, len(w.Reconcilers))
	for name := range w.Reconcilers {
		names = append(names, name)
	}
	sort.Strings(names)
	var failures []error
	now := w.Clock.Now()
	for _, name := range names {
		attempt := w.reconcileAttempts[name]
		delay := w.ReconcileInterval
		if attempt.Failures > 0 {
			index := attempt.Failures - 1
			if index >= len(reconcileFailureDelays) {
				index = len(reconcileFailureDelays) - 1
			}
			delay = reconcileFailureDelays[index]
		}
		if !attempt.LastAttempt.IsZero() && now.Before(attempt.LastAttempt.Add(delay)) {
			continue
		}
		started := time.Now()
		events := w.Events.For("worker")
		events.Log(ctx, slog.LevelInfo, "reconcile.started", "catalog reconciliation started", slog.String("instance", name))
		if err := w.Reconcilers[name].Run(ctx); err != nil {
			attempt.LastAttempt = now
			if attempt.Failures < len(reconcileFailureDelays) {
				attempt.Failures++
			}
			w.reconcileAttempts[name] = attempt
			retryAt := now.Add(reconcileFailureDelays[attempt.Failures-1])
			events.Log(ctx, slog.LevelError, "reconcile.failed", "catalog reconciliation failed",
				append([]slog.Attr{slog.String("instance", name), slog.Int("attempt", attempt.Failures), slog.Time("retry_at", retryAt), slog.Int64("duration_ms", time.Since(started).Milliseconds())}, events.ErrorAttrs("reconciliation", err)...)...)
			failures = append(failures, fmt.Errorf("reconcile %s: %w", name, err))
			continue
		}
		w.reconcileAttempts[name] = reconcileAttempt{LastAttempt: now}
		events.Log(ctx, slog.LevelInfo, "reconcile.completed", "catalog reconciliation completed",
			slog.String("instance", name), slog.String("outcome", "success"), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
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
	if w.MaxWorkflows > maximumWorkflows {
		w.MaxWorkflows = maximumWorkflows
	}
	if w.NotificationBatch <= 0 {
		w.NotificationBatch = defaultNotificationBatch
	}
	if w.NotificationBatch > defaultNotificationBatch {
		w.NotificationBatch = defaultNotificationBatch
	}
	if w.Events == nil {
		w.Events = observability.Discard()
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
		w.Events.For("worker").Log(context.Background(), slog.LevelWarn, "worker.drain_timed_out", "worker drain timed out",
			slog.Int("active_searches", 1), slog.Bool("maintenance_active", false))
		cancel()
		hardTimer := time.NewTimer(w.ShutdownTimeout)
		defer hardTimer.Stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				w.report(err)
			}
		case <-hardTimer.C:
			w.Events.For("worker").Log(context.Background(), slog.LevelWarn, "worker.drain_abandoned", "worker returning with work still active after cancellation",
				slog.Int("active_searches", 1), slog.Bool("maintenance_active", false))
		}
		return nil
	}
}

func (w *Worker) drainDaemon(activeSearches int, maintenanceActive bool, searchesDone <-chan searchDone, maintenanceDone <-chan error, cancel context.CancelFunc) error {
	gracefulTimer := time.NewTimer(w.ShutdownTimeout)
	defer gracefulTimer.Stop()
	graceful := gracefulTimer.C
	var hardTimer *time.Timer
	var hard <-chan time.Time
	defer func() {
		if hardTimer != nil {
			hardTimer.Stop()
		}
	}()
	for activeSearches > 0 || maintenanceActive {
		select {
		case result := <-searchesDone:
			activeSearches--
			w.reportUnlessCanceled(result.err)
		case err := <-maintenanceDone:
			maintenanceActive = false
			w.reportUnlessCanceled(err)
		case <-graceful:
			graceful = nil
			w.Events.For("worker").Log(context.Background(), slog.LevelWarn, "worker.drain_timed_out", "worker drain timed out",
				slog.Int("active_searches", activeSearches), slog.Bool("maintenance_active", maintenanceActive))
			cancel()
			hardTimer = time.NewTimer(w.ShutdownTimeout)
			hard = hardTimer.C
		case <-hard:
			w.Events.For("worker").Log(context.Background(), slog.LevelWarn, "worker.drain_abandoned", "worker returning with work still active after cancellation",
				slog.Int("active_searches", activeSearches), slog.Bool("maintenance_active", maintenanceActive))
			return nil
		}
	}
	return nil
}

func (w *Worker) logLease(ctx context.Context, lease store.SearchLease) {
	w.Events.For("worker").Log(ctx, slog.LevelInfo, "job.leased", "subtitle job leased",
		slog.String("job_id", lease.JobID),
		slog.Int64("media_id", lease.MediaID),
		slog.String("language", lease.Language),
		slog.String("priority", lease.Priority.String()),
		slog.Int("attempt", lease.Attempt),
		slog.Int("failure_attempt", lease.FailureAttempt),
	)
}

func (w *Worker) logRerun(ctx context.Context, result store.SearchCompletionResult) {
	if !result.RerunScheduled {
		return
	}
	w.Events.For("worker").Log(ctx, slog.LevelInfo, "job.rerun_requested", "subtitle job rerun requested",
		slog.String("priority", store.SearchPriorityImport.String()))
}

func (w *Worker) logJobCompleted(ctx context.Context, level slog.Level, outcome, reason string, next time.Time, started time.Time, err error) {
	attrs := []slog.Attr{
		slog.String("outcome", outcome),
		slog.String("reason", reason),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if !next.IsZero() {
		attrs = append(attrs, slog.Time("next_attempt_at", next))
	}
	events := w.Events.For("worker")
	if err != nil {
		attrs = append(attrs, events.ErrorAttrs(reason, err)...)
	}
	events.Log(ctx, level, "job.completed", "subtitle job completed", attrs...)
}

func completionReason(outcome workflow.Outcome) string {
	switch outcome {
	case workflow.OutcomeSatisfied:
		return "satisfied"
	case workflow.OutcomeInstalled:
		return "installed"
	case workflow.OutcomeNoResult:
		return "missing_backoff"
	case workflow.OutcomeRejected:
		return "candidate_rejected"
	case workflow.OutcomeThrottled:
		return "provider_throttle"
	default:
		return "unsupported_outcome"
	}
}

func (w *Worker) reportUnlessCanceled(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		w.report(err)
	}
}

func (w *Worker) report(err error) {
	if err != nil && w.OnError != nil {
		w.OnError(err)
	}
}

type leaseRenewal struct {
	stop context.CancelFunc
	done <-chan error
}

func (r leaseRenewal) finish() error {
	r.stop()
	return <-r.done
}
