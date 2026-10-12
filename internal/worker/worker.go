package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"subsyncd/internal/catalog"
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
	// defaultJobTimeout bounds one search job. A LAPSE run is limited to
	// sync.max_timeout (4h by default) on the largest files, and later runs
	// on the same media reuse its speech cache, so twice that never cuts a
	// legitimate job; it only frees a worker stuck on media that stopped
	// answering.
	defaultJobTimeout = 8 * time.Hour
)

// errJobTimedOut ends a search job that ran past Worker.JobTimeout.
var errJobTimedOut = errors.New("subtitle job exceeded its time limit")

// defaultJobGrace is how long a search job may take to return after its time
// limit cancels it. A workflow blocked in a call that ignores cancellation (a
// read on a hung mount) is abandoned after it, so it cannot hold the worker
// slot forever.
const defaultJobGrace = 2 * time.Minute

// JobGrace is defaultJobGrace for other runners of search jobs, such as manual
// searches, which abandon a workflow the same way.
const JobGrace = defaultJobGrace

type Clock interface{ Now() time.Time }

type Workflow interface {
	Run(context.Context, workflow.Request) (workflow.Result, error)
}

type Reconciler interface {
	Run(context.Context) error
}

// reconcileAttempt is an instance's last reconciliation, saved in the
// database when the repository supports it so a restart keeps its schedule.
type reconcileAttempt = store.ReconcileTiming

// reconcileTimingStore persists reconciliation timing across restarts.
type reconcileTimingStore interface {
	ReconcileTimings(context.Context) (map[string]store.ReconcileTiming, error)
	RecordReconcileTiming(context.Context, string, store.ReconcileTiming) error
}

// leaseReleaser gives leases back on shutdown, so their work is leased again
// at the next start instead of after the lease expires.
type leaseReleaser interface {
	ReleaseSearchLease(context.Context, string) error
	ReleaseNotificationLease(context.Context, string) error
}

// leaseReleaseTimeout bounds the best-effort release after cancellation.
const leaseReleaseTimeout = 5 * time.Second

// reconcileDeferredDelay is the retry for a page that committed but had to
// wait for unreadable media, capped at the normal interval. It does not
// escalate like a failure.
const reconcileDeferredDelay = time.Hour

var reconcileFailureDelays = [...]time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
}

type Repository interface {
	LeaseDueSearchesExcept(context.Context, time.Time, int, time.Duration, []store.RouteKey) ([]store.SearchLease, error)
	RenewSearchLease(context.Context, string, time.Time, time.Duration) error
	CompleteSearch(context.Context, store.SearchCompletion) (store.SearchCompletionResult, error)
	GetMedia(context.Context, int64) (domain.Media, error)
	LeaseDueNotifications(context.Context, time.Time, int, time.Duration) ([]store.NotificationLease, error)
	RenewNotificationLease(context.Context, string, time.Time, time.Duration) error
	CompleteNotification(context.Context, store.NotificationCompletion) error
}

type Worker struct {
	Repository  Repository
	Workflow    Workflow
	Clock       Clock
	Notifiers   map[string]notifier.Notifier
	Reconcilers map[string]Reconciler
	// ReconcilePriorities orders reconciliation (and so first-run library
	// discovery) by instance queue_priority, highest first; ties run by name.
	ReconcilePriorities map[string]int
	RandomUnit          func() float64
	// AgedAfter lets missing searches for media released this long ago skip the
	// early retries; 0 keeps the full schedule.
	AgedAfter         time.Duration
	OnError           func(error)
	PollInterval      time.Duration
	LeaseDuration     time.Duration
	RenewInterval     time.Duration
	ShutdownTimeout   time.Duration
	ReconcileInterval time.Duration
	// JobTimeout cancels a search job that runs longer and schedules it as a
	// failure, abandoning it defaultJobGrace later if it has not returned;
	// 0 uses defaultJobTimeout.
	JobTimeout        time.Duration
	SearchBatch       int
	MaxWorkflows      int
	NotificationBatch int
	Wake              <-chan struct{}
	Events            *observability.Emitter
	// Routes pauses leasing for routes whose providers are all unavailable.
	Routes RouteGate
	// MediaRoots lets the media breaker tell one unreadable file from an
	// unreadable mount; without them every media failure counts.
	MediaRoots []string

	reconcileMu       sync.Mutex
	reconcileAttempts map[string]reconcileAttempt
	routeMu           sync.Mutex
	pausedRoutes      map[store.RouteKey]RoutePause
	breakerMu         sync.Mutex
	mediaFailures     int
	mediaPause        time.Duration
	mediaPausedUntil  time.Time
	// jobGrace overrides defaultJobGrace in tests.
	jobGrace time.Duration
}

func (w *Worker) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
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
		if ctx.Err() != nil {
			return
		}
		available := w.MaxWorkflows - activeSearches
		if available <= 0 || w.mediaPaused() {
			return
		}
		paused, err := w.pausedRouteKeys(ctx)
		if err != nil {
			w.report(err)
			return
		}
		leases, err := w.Repository.LeaseDueSearchesExcept(ctx, w.Clock.Now(), available, w.LeaseDuration, paused)
		if err != nil {
			w.report(err)
			return
		}
		for _, lease := range leases {
			if ctx.Err() != nil {
				return
			}
			w.logLease(workCtx, lease)
			activeSearches++
			go func(lease store.SearchLease) {
				searchesDone <- searchDone{err: w.runSearchLease(workCtx, lease)}
			}(lease)
		}
	}
	startMaintenance := func() {
		if ctx.Err() != nil || maintenanceActive {
			return
		}
		maintenanceActive = true
		go func() { maintenanceDone <- w.runMaintenance(ctx, workCtx) }()
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

func (w *Worker) runMaintenance(dispatchCtx, ctx context.Context) error {
	if err := dispatchCtx.Err(); err != nil {
		return err
	}
	var failures []error
	if err := w.reconcileDueContexts(dispatchCtx, ctx); err != nil {
		failures = append(failures, err)
	}
	if err := dispatchCtx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	notifications, err := w.Repository.LeaseDueNotifications(dispatchCtx, w.Clock.Now(), w.NotificationBatch, w.LeaseDuration)
	if err != nil {
		failures = append(failures, err)
	} else if err := w.processNotificationsContexts(dispatchCtx, ctx, notifications); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (w *Worker) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.prepare(); err != nil {
		return err
	}
	var failures []error
	if err := w.reconcileDue(ctx); err != nil {
		failures = append(failures, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	var searches []store.SearchLease
	paused, err := w.pausedRouteKeys(ctx)
	if err == nil && !w.mediaPaused() {
		searches, err = w.Repository.LeaseDueSearchesExcept(ctx, w.Clock.Now(), w.SearchBatch, w.LeaseDuration, paused)
	}
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
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.runSearchLease(ctx, lease)
}

func (w *Worker) runSearchLease(ctx context.Context, lease store.SearchLease) error {
	started := time.Now()
	limit := w.JobTimeout
	if limit <= 0 {
		limit = defaultJobTimeout
	}
	grace := w.jobGrace
	if grace <= 0 {
		grace = defaultJobGrace
	}
	timeoutCtx, cancelTimeout := context.WithTimeoutCause(ctx, limit, errJobTimedOut)
	defer cancelTimeout()
	jobCtx, cancelJob := context.WithCancel(timeoutCtx)
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
	// Renewal is not bound to the time limit: the lease stays ours until the
	// workflow returns or is abandoned, so the timeout is recorded as owner.
	renewal := w.renewSearchLease(ctx, cancelJob, lease.JobID)
	var result workflow.Result
	if err == nil {
		if media.UnsupportedReason != "" {
			if renewErr := renewal.finish(); renewErr != nil {
				events.Log(jobCtx, slog.LevelWarn, "job.lease_lost", "subtitle job lease was lost", events.ErrorAttrs("lease_renewal", renewErr)...)
				return renewErr
			}
			if jobCtx.Err() != nil {
				w.releaseSearchLease(jobCtx, lease.JobID)
				w.logJobCompleted(jobCtx, slog.LevelInfo, "canceled", "canceled", time.Time{}, started, len(lease.ResumeProviders), nil)
				return jobCtx.Err()
			}
			completion := store.SearchCompletion{JobID: lease.JobID, Outcome: string(media.UnsupportedReason)}
			completionResult, completionErr := w.Repository.CompleteSearch(jobCtx, completion)
			if completionErr != nil {
				w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, len(lease.ResumeProviders), completionErr)
				return completionErr
			}
			w.logRerun(jobCtx, completionResult)
			w.logJobCompleted(jobCtx, slog.LevelInfo, string(media.UnsupportedReason), string(media.UnsupportedReason), time.Time{}, started, 0, nil)
			return nil
		}
		// A queued reject treats the rejected subtitle as replaceable or
		// removable, also after a restart.
		request := workflow.Request{MediaID: lease.MediaID, Media: media, Language: domain.Language(lease.Language), ReplaceInstalled: lease.ReplaceInstalled}
		if lease.Priority == store.SearchPriorityUpgrade {
			request.UpgradeAttempt = lease.Attempt + 1
		}
		if lease.Priority != store.SearchPriorityUpgrade {
			request.ResumeProviders = append([]string(nil), lease.ResumeProviders...)
			request.ResumeRouteSignature = lease.ResumeRouteSignature
		}
		var abandoned bool
		result, abandoned, err = w.runWorkflow(workflowCtx, request, limit+grace)
		if abandoned {
			// The workflow ignored cancellation; its goroutine keeps running
			// with a canceled context, which stops it before it publishes.
			err = fmt.Errorf("%w (%s, abandoned after a further %s)", errJobTimedOut, limit, grace)
		}
	}
	timedOut := context.Cause(timeoutCtx) == errJobTimedOut && ctx.Err() == nil
	renewErr := renewal.finish()
	if renewErr != nil {
		events.Log(jobCtx, slog.LevelWarn, "job.lease_lost", "subtitle job lease was lost", events.ErrorAttrs("lease_renewal", renewErr)...)
		// A job that failed or was abandoned past its time limit still records
		// the timeout: completion is owner-checked, so it only lands while the
		// lease is ours, and otherwise the search would be leased again at once
		// beside the stuck workflow.
		if !timedOut || err == nil {
			return renewErr
		}
	}
	if timedOut {
		// A workflow that still returned a result in time keeps it; one that
		// failed or was abandoned is recorded as a timeout while the lease is
		// still ours, so the search retries on the failure schedule instead of
		// at once.
		if err != nil {
			if !errors.Is(err, errJobTimedOut) {
				err = fmt.Errorf("%w (%s)", errJobTimedOut, limit)
			}
			events.Log(jobCtx, slog.LevelWarn, "job.timed_out", "subtitle job exceeded its time limit", slog.Duration("limit", limit))
		}
		jobCtx = context.WithoutCancel(jobCtx)
	}
	if jobCtx.Err() != nil {
		w.releaseSearchLease(jobCtx, lease.JobID)
		w.logJobCompleted(jobCtx, slog.LevelInfo, "canceled", "canceled", time.Time{}, started, len(lease.ResumeProviders), nil)
		return jobCtx.Err()
	}
	w.recordSearchResult(jobCtx, err, media.Fingerprint.Path)
	if err != nil {
		completion := schedule.Scheduler{Clock: w.Clock}.Failure(lease.JobID, lease.FailureAttempt, "workflow_error")
		// Unreadable media is usually storage, not this search: keep its place.
		completion.PreserveQueueOrder = workflow.MediaUnavailable(err)
		completion.History = searchHistory(media, workflow.Result{})
		completion.FinishedAt = w.Clock.Now()
		preserveProviderResume(lease, result, &completion, w.Clock.Now())
		completionResult, completionErr := w.Repository.CompleteSearch(jobCtx, completion)
		if completionErr != nil {
			w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, len(lease.ResumeProviders), completionErr)
			return errors.Join(err, completionErr, renewErr)
		}
		events.Log(jobCtx, slog.LevelWarn, "job.retry_scheduled", "subtitle job retry scheduled",
			slog.String("reason", "workflow_error"),
			slog.Time("retry_at", completion.NextAttemptAt),
			slog.Int("next_failure_attempt", lease.FailureAttempt+1),
		)
		w.logRerun(jobCtx, completionResult)
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "workflow", completion.NextAttemptAt, started, completionResult.ResumeProviderCount, err)
		return err
	}
	completion, err := w.workflowCompletion(lease, result, media)
	if err != nil {
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "workflow_outcome", time.Time{}, started, len(lease.ResumeProviders), err)
		return err
	}
	completion.History = searchHistory(media, result)
	if result.Outcome == workflow.OutcomeRemoved {
		// The removal recorded its history row with the record's deletion.
		completion.History = nil
	}
	// A replacement is finished by any outcome but a throttle, which retries it.
	completion.ClearReplaceInstalled = lease.ReplaceInstalled && result.Outcome != workflow.OutcomeThrottled
	completion.FinishedAt = w.Clock.Now()
	completionResult, err := w.Repository.CompleteSearch(jobCtx, completion)
	if err != nil {
		w.logJobCompleted(jobCtx, slog.LevelError, "failed", "search_completion", time.Time{}, started, len(lease.ResumeProviders), err)
		return err
	}
	w.logRerun(jobCtx, completionResult)
	w.logJobCompleted(jobCtx, slog.LevelInfo, string(result.Outcome), completionReason(result.Outcome), completion.NextAttemptAt, started, completionResult.ResumeProviderCount, nil, w.releaseAttrs(media, completion)...)
	return nil
}

// runWorkflow runs the workflow and waits for it at most wait. abandoned
// reports that it had not returned by then.
func (w *Worker) runWorkflow(ctx context.Context, request workflow.Request, wait time.Duration) (result workflow.Result, abandoned bool, err error) {
	type outcome struct {
		result workflow.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := w.Workflow.Run(ctx, request)
		done <- outcome{result, err}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case finished := <-done:
		return finished.result, false, finished.err
	case <-timer.C:
		return workflow.Result{}, true, nil
	}
}

func (w *Worker) workflowCompletion(lease store.SearchLease, result workflow.Result, media domain.Media) (store.SearchCompletion, error) {
	completion := store.SearchCompletion{JobID: lease.JobID, Outcome: string(result.Outcome)}
	switch result.Outcome {
	case workflow.OutcomeSatisfied:
		completion.NextAttemptAt = result.NextUpgrade
		if !result.NextUpgrade.IsZero() {
			completion.Priority = store.SearchPriorityUpgrade
		}
		completion.AdvanceUpgradeAttempt = lease.Priority == store.SearchPriorityUpgrade && !result.NextUpgrade.IsZero()
		completion.ResetMissingAttempt = !completion.AdvanceUpgradeAttempt
		completion.ResetFailureAttempt = true
	case workflow.OutcomeInstalled:
		completion.NextAttemptAt = result.NextUpgrade
		if !result.NextUpgrade.IsZero() {
			completion.Priority = store.SearchPriorityUpgrade
		}
		completion.ResetMissingAttempt = true
		completion.ResetFailureAttempt = true
	case workflow.OutcomeNoResult, workflow.OutcomeRejected, workflow.OutcomeRemoved:
		// A removed subtitle leaves the language missing; normal retries go on.
		attempt := lease.Attempt + 1
		if lease.Priority == store.SearchPriorityUpgrade {
			attempt = 1
		}
		completion = w.scheduler().Missing(lease.JobID, attempt, media.ReleasedAt)
		completion.ResetMissingAttempt = lease.Priority == store.SearchPriorityUpgrade
		completion.Outcome = string(result.Outcome)
		completion.Priority = store.SearchPriorityMissing
	case workflow.OutcomeThrottled:
		// The jittered reset only delays eligibility; the search keeps its place.
		completion.NextAttemptAt = w.throttleRetryAt(result.RetryAt)
		completion.PreserveQueueOrder = true
		preserveProviderResume(lease, result, &completion, w.Clock.Now())
	default:
		return store.SearchCompletion{}, fmt.Errorf("workflow returned unsupported outcome %q", result.Outcome)
	}
	return completion, nil
}

// searchHistory describes a finished search for the history: the display
// title, and the candidate when the workflow chose one.
func searchHistory(media domain.Media, result workflow.Result) *store.SearchHistoryDetail {
	detail := &store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(media), ProviderID: result.Candidate.ProviderID}
	if len(result.Candidate.ReleaseNames) > 0 {
		detail.ReleaseName = result.Candidate.ReleaseNames[0]
	}
	return detail
}

func (w *Worker) scheduler() schedule.Scheduler {
	return schedule.Scheduler{Clock: w.Clock, RandomUnit: w.RandomUnit, AgedAfter: w.AgedAfter}
}

// releaseAttrs logs how long ago the media was released and whether its
// missing search skipped the early retries.
func (w *Worker) releaseAttrs(media domain.Media, completion store.SearchCompletion) []slog.Attr {
	if media.ReleasedAt.IsZero() {
		return nil
	}
	attrs := []slog.Attr{slog.Int("release_age_days", int(w.Clock.Now().Sub(media.ReleasedAt)/(24*time.Hour)))}
	if completion.MissingAttemptFloor > 0 {
		attrs = append(attrs, slog.String("schedule", "aged"))
	}
	return attrs
}

// preserveProviderResume keeps the clean-empty providers for the retry. The
// list is dated by its oldest answer: the leased list's date when the search
// resumed one, otherwise now.
func preserveProviderResume(lease store.SearchLease, result workflow.Result, completion *store.SearchCompletion, now time.Time) {
	if lease.Priority == store.SearchPriorityUpgrade || len(result.ResumeProviders) == 0 || result.ResumeRouteSignature == "" {
		return
	}
	completion.ResumeProviders = append([]string(nil), result.ResumeProviders...)
	completion.ResumeRouteSignature = result.ResumeRouteSignature
	completion.PreserveResume = true
	completion.ResumeSavedAt = now
	if len(lease.ResumeProviders) > 0 && !lease.ResumeSavedAt.IsZero() {
		completion.ResumeSavedAt = lease.ResumeSavedAt
	}
}

// releaseSearchLease gives a canceled job's lease back, best effort.
func (w *Worker) releaseSearchLease(ctx context.Context, jobID string) {
	releaser, ok := w.Repository.(leaseReleaser)
	if !ok {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	if err := releaser.ReleaseSearchLease(releaseCtx, jobID); err != nil {
		w.report(err)
	}
}

// releaseNotificationLeases gives undelivered notification leases back.
func (w *Worker) releaseNotificationLeases(ctx context.Context, members []groupMember) {
	releaser, ok := w.Repository.(leaseReleaser)
	if !ok {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	for _, member := range members {
		if err := releaser.ReleaseNotificationLease(releaseCtx, member.lease.JobID); err != nil {
			w.report(err)
		}
	}
}

func (w *Worker) processNotifications(ctx context.Context, leases []store.NotificationLease) error {
	return w.processNotificationsContexts(ctx, ctx, leases)
}

func (w *Worker) processNotificationsContexts(dispatchCtx, ctx context.Context, leases []store.NotificationLease) error {
	groups := w.coalesceNotifications(leases)
	semaphore := make(chan struct{}, w.MaxWorkflows)
	errorsByIndex := make([]error, len(groups))
	var wait sync.WaitGroup
	for index, group := range groups {
		wait.Add(1)
		go func(index int, group []notificationDelivery) {
			defer wait.Done()
			errorsByIndex[index] = w.processNotificationGroupContexts(dispatchCtx, ctx, group, semaphore)
		}(index, group)
	}
	wait.Wait()
	return errors.Join(errorsByIndex...)
}

// notificationDelivery is a leased intent with its payload decoded once.
type notificationDelivery struct {
	lease     store.NotificationLease
	payload   workflow.NotificationPayload
	decodeErr error
}

// coalesceNotifications groups leases that one delivery satisfies, keeping
// lease order. Every leased intent was committed after its subtitle was
// published, so one request covers all of a group's subtitles; sending one
// per lease made Silo reject the concurrent duplicates of an active scan.
func (w *Worker) coalesceNotifications(leases []store.NotificationLease) [][]notificationDelivery {
	groups := make([][]notificationDelivery, 0, len(leases))
	byKey := make(map[string]int)
	for _, lease := range leases {
		delivery := notificationDelivery{lease: lease}
		delivery.decodeErr = json.Unmarshal(lease.PayloadJSON, &delivery.payload)
		if key, ok := w.notificationCoalesceKey(delivery); ok {
			if index, found := byKey[key]; found {
				groups[index] = append(groups[index], delivery)
				continue
			}
			byKey[key] = len(groups)
		}
		groups = append(groups, []notificationDelivery{delivery})
	}
	return groups
}

func (w *Worker) notificationCoalesceKey(delivery notificationDelivery) (string, bool) {
	coalescer, ok := w.Notifiers[delivery.lease.Notifier].(notifier.Coalescer)
	if !ok || delivery.decodeErr != nil {
		return "", false
	}
	key, err := coalescer.CoalesceKey(delivery.payload.Media, delivery.payload.SubtitlePath)
	if err != nil {
		return "", false
	}
	return delivery.lease.Notifier + "\x00" + key, true
}

// groupMember is one lease of a coalesced group with its own lease context,
// so losing one member's lease abandons only that member.
type groupMember struct {
	lease   store.NotificationLease
	ctx     context.Context
	cancel  context.CancelFunc
	renewal leaseRenewal
}

// processNotificationGroupContexts delivers the first lease of a group and
// completes every member whose lease is still held with that outcome. Each
// member keeps its own renewal and its own attempt-based retry delay. If the
// first member's lease is lost, the group is abandoned like a single lease.
func (w *Worker) processNotificationGroupContexts(dispatchCtx, ctx context.Context, group []notificationDelivery, semaphore chan struct{}) error {
	if err := dispatchCtx.Err(); err != nil {
		return err
	}
	started := time.Now()
	members := make([]groupMember, len(group))
	for index, delivery := range group {
		memberCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		memberCtx = observability.WithAttrs(memberCtx,
			slog.String("notification_id", delivery.lease.JobID),
			slog.String("notifier", delivery.lease.Notifier),
			slog.Int("attempt", delivery.lease.Attempt),
		)
		members[index] = groupMember{lease: delivery.lease, ctx: memberCtx, cancel: cancel}
		members[index].renewal = w.renewNotificationLease(memberCtx, cancel, delivery.lease.JobID)
	}
	leader := members[0]
	finishAll := func() error {
		var failures []error
		for _, member := range members {
			failures = append(failures, member.renewal.finish())
		}
		return errors.Join(failures...)
	}
	select {
	case semaphore <- struct{}{}:
	case <-dispatchCtx.Done():
		err := errors.Join(finishAll(), dispatchCtx.Err())
		w.releaseNotificationLeases(ctx, members)
		return err
	case <-leader.ctx.Done():
		return errors.Join(finishAll(), leader.ctx.Err())
	}
	defer func() { <-semaphore }()
	if err := errors.Join(dispatchCtx.Err(), leader.ctx.Err()); err != nil {
		err = errors.Join(finishAll(), err)
		if dispatchCtx.Err() != nil {
			w.releaseNotificationLeases(ctx, members)
		}
		return err
	}
	err := group[0].decodeErr
	destination := w.Notifiers[leader.lease.Notifier]
	if err == nil && destination == nil {
		err = fmt.Errorf("notifier %q is not configured", leader.lease.Notifier)
	}
	if err == nil {
		err = destination.SubtitleChanged(leader.ctx, group[0].payload.Media, group[0].payload.SubtitlePath)
	}
	if renewErr := leader.renewal.finish(); renewErr != nil {
		return errors.Join(renewErr, finishFollowers(members[1:]))
	}
	if leader.ctx.Err() != nil {
		err := errors.Join(leader.ctx.Err(), finishFollowers(members[1:]))
		if ctx.Err() != nil {
			// Shutdown, not a lost lease: deliver again at the next start.
			w.releaseNotificationLeases(ctx, members)
		}
		return err
	}
	var failures []error
	for index, member := range members {
		if index > 0 {
			if renewErr := member.renewal.finish(); renewErr != nil {
				failures = append(failures, renewErr)
				continue
			}
			if member.ctx.Err() != nil {
				failures = append(failures, member.ctx.Err())
				continue
			}
		}
		failures = append(failures, w.completeNotification(member.ctx, member.lease, err, started))
	}
	return errors.Join(failures...)
}

func finishFollowers(followers []groupMember) error {
	var failures []error
	for _, follower := range followers {
		failures = append(failures, follower.renewal.finish())
	}
	return errors.Join(failures...)
}

func (w *Worker) completeNotification(jobCtx context.Context, lease store.NotificationLease, deliveryErr error, started time.Time) error {
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

var errRenewalFinished = errors.New("lease renewal finished")

func (w *Worker) renew(ctx context.Context, cancelJob context.CancelFunc, renew func(context.Context) error) leaseRenewal {
	renewCtx, stop := context.WithCancelCause(ctx)
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
					// finish stops renewal before the owner-checked completion.
					// Canceling that in-flight SQL call is normal cleanup, not
					// evidence that the job lost its lease.
					if errors.Is(err, context.Canceled) && context.Cause(renewCtx) == errRenewalFinished && ctx.Err() == nil {
						done <- nil
						return
					}
					done <- err
					cancelJob()
					return
				}
			}
		}
	}()
	return leaseRenewal{stop: func() { stop(errRenewalFinished) }, done: done}
}

func (w *Worker) reconcileDue(ctx context.Context) error {
	return w.reconcileDueContexts(ctx, ctx)
}

func (w *Worker) reconcileDueContexts(dispatchCtx, ctx context.Context) error {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.reconcileAttempts == nil {
		attempts := make(map[string]reconcileAttempt)
		if timings, ok := w.Repository.(reconcileTimingStore); ok {
			saved, err := timings.ReconcileTimings(ctx)
			if err != nil {
				return err
			}
			for name, timing := range saved {
				attempts[name] = timing
			}
		}
		w.releasePendingDiscoveries(ctx, attempts)
		w.reconcileAttempts = attempts
	}
	names := make([]string, 0, len(w.Reconcilers))
	for name := range w.Reconcilers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if pi, pj := w.ReconcilePriorities[names[i]], w.ReconcilePriorities[names[j]]; pi != pj {
			return pi > pj
		}
		return names[i] < names[j]
	})
	var failures []error
	now := w.Clock.Now()
	for _, name := range names {
		if err := dispatchCtx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		attempt := w.reconcileAttempts[name]
		delay := w.ReconcileInterval
		if attempt.Deferred {
			delay = min(delay, reconcileDeferredDelay)
		}
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
		err := w.Reconcilers[name].Run(ctx)
		// The sentinel decides; the typed error, when present, adds the names.
		if errors.Is(err, catalog.ErrHistoryDeferred) {
			deferred := &catalog.DeferredHistoryError{}
			errors.As(err, &deferred)
			w.setReconcileAttempt(ctx, name, reconcileAttempt{LastAttempt: now, Deferred: true})
			events.Log(ctx, slog.LevelWarn, "reconcile.deferred", "catalog reconciliation is waiting for unavailable media",
				slog.String("instance", name),
				slog.Int("deferred_count", len(deferred.Entities)),
				slog.Any("deferred_media", deferred.DeferredMedia()),
				slog.Time("retry_at", now.Add(min(w.ReconcileInterval, reconcileDeferredDelay))),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()))
			continue
		}
		if err != nil {
			attempt.LastAttempt = now
			attempt.Deferred = false
			if attempt.Failures < len(reconcileFailureDelays) {
				attempt.Failures++
			}
			w.setReconcileAttempt(ctx, name, attempt)
			retryAt := now.Add(reconcileFailureDelays[attempt.Failures-1])
			events.Log(ctx, slog.LevelError, "reconcile.failed", "catalog reconciliation failed",
				append([]slog.Attr{slog.String("instance", name), slog.Int("attempt", attempt.Failures), slog.Time("retry_at", retryAt), slog.Int64("duration_ms", time.Since(started).Milliseconds())}, events.ErrorAttrs("reconciliation", err)...)...)
			failures = append(failures, fmt.Errorf("reconcile %s: %w", name, err))
			continue
		}
		w.setReconcileAttempt(ctx, name, reconcileAttempt{LastAttempt: now})
		events.Log(ctx, slog.LevelInfo, "reconcile.completed", "catalog reconciliation completed",
			slog.String("instance", name), slog.String("outcome", "success"), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	}
	return errors.Join(failures...)
}

// discoveryPendingReconciler reports whether its next run enumerates the
// library because the configured discovery scope changed.
type discoveryPendingReconciler interface {
	DiscoveryPending(context.Context) (bool, error)
}

// releasePendingDiscoveries makes an instance due at startup when its last
// reconciliation succeeded but its library discovery is pending again: the
// scope changed with this start (new mappings or roots, another URL, a scope
// version bump), and waiting out the saved six-hour interval would delay the
// pass that change exists to trigger. A failure backoff or a deferral (a
// discovery still waiting for an unreadable mount) keeps its saved timing.
// Checked once per worker, so a discovery that stays incomplete cannot make
// every dispatch reconcile.
func (w *Worker) releasePendingDiscoveries(ctx context.Context, attempts map[string]reconcileAttempt) {
	for name, attempt := range attempts {
		if attempt.LastAttempt.IsZero() || attempt.Failures > 0 || attempt.Deferred {
			continue
		}
		reconciler, ok := w.Reconcilers[name].(discoveryPendingReconciler)
		if !ok {
			continue
		}
		pending, err := reconciler.DiscoveryPending(ctx)
		if err != nil {
			// The saved schedule stands; reconciliation reads the state again.
			w.report(err)
			continue
		}
		if pending {
			delete(attempts, name)
		}
	}
}

// setReconcileAttempt records an attempt in memory and, when supported, in
// the database. A failed save only costs an early reconciliation after a
// restart, so it is reported, not returned.
func (w *Worker) setReconcileAttempt(ctx context.Context, name string, attempt reconcileAttempt) {
	w.reconcileAttempts[name] = attempt
	if timings, ok := w.Repository.(reconcileTimingStore); ok {
		if err := timings.RecordReconcileTiming(context.WithoutCancel(ctx), name, attempt); err != nil {
			w.report(err)
		}
	}
}

// minimumThrottleDelay is the shortest wait after a throttle. A provider that
// reports a reset already past (Retry-After: 0, a stale reset header) would
// otherwise be asked again at once, in a loop.
const minimumThrottleDelay = time.Minute

// unknownThrottleDelay is how long a throttled search waits when no provider
// reported a reset, as when the only blocked provider is disabled. A
// throttled search is never complete: it stays pending until a provider can
// be asked again (`subsyncd retry --provider` makes it due at once).
const unknownThrottleDelay = 6 * time.Hour

func (w *Worker) throttleRetryAt(reset time.Time) time.Time {
	if reset.IsZero() {
		reset = w.Clock.Now().Add(unknownThrottleDelay)
	}
	delay := reset.Sub(w.Clock.Now())
	if delay < minimumThrottleDelay {
		return w.Clock.Now().Add(minimumThrottleDelay)
	}
	unit := rand.Float64()
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
		slog.Int("resume_provider_count", len(lease.ResumeProviders)),
	)
}

func (w *Worker) logRerun(ctx context.Context, result store.SearchCompletionResult) {
	if !result.RerunScheduled {
		return
	}
	w.Events.For("worker").Log(ctx, slog.LevelInfo, "job.rerun_requested", "subtitle job rerun requested",
		slog.String("priority", store.SearchPriorityImport.String()))
}

func (w *Worker) logJobCompleted(ctx context.Context, level slog.Level, outcome, reason string, next time.Time, started time.Time, resumeProviderCount int, err error, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("outcome", outcome),
		slog.String("reason", reason),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Int("resume_provider_count", resumeProviderCount),
	}
	attrs = append(attrs, extra...)
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
	case workflow.OutcomeRemoved:
		return "rejected_subtitle_removed"
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
