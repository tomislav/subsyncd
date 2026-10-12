package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/schedule"
	"subsyncd/internal/store"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
)

const (
	// manualSearchSlots is how many manual searches run at once; more wait
	// in arrival order. They are separate from the worker's limit.
	manualSearchSlots = 2
	// manualLeaseDuration is a manual search's lease, renewed every third.
	manualLeaseDuration = 5 * time.Minute
	// defaultManualClaimPoll is how often a manual search retries the claim
	// while the worker holds the search.
	defaultManualClaimPoll = time.Second
	// defaultSearchWait is how long the daemon waits for a search's outcome
	// before answering running.
	defaultSearchWait = 5 * time.Minute
	// manualLeaseReleaseTimeout bounds the release after cancellation.
	manualLeaseReleaseTimeout = 5 * time.Second
	// manualDrainTimeout bounds how long shutdown waits, after canceling them,
	// for manual searches to give their leases back. It runs inside the
	// worker's own drain windows, so it adds none.
	manualDrainTimeout = 30 * time.Second
)

var errManualLeaseLost = errors.New("manual search lost its search lease")

// errManualTimedOut ends a manual search that ran past its time limit.
var errManualTimedOut = errors.New("manual search exceeded its time limit")

// manualOptions adjusts one manual search.
type manualOptions struct {
	// RetryRejected clears subsyncd's own candidate rejections first; the
	// owner's (reject) stay.
	RetryRejected bool
	// jobID is a lease owner the caller already holds the search's lease
	// for, as reject does; empty generates one.
	jobID string
	// replace is read from the search row once claimed: a queued reject
	// treats the installed subtitle as replaceable or removable.
	replace bool
	// revived reports that the search just added or revived the media, which
	// queued every configured language afresh.
	revived bool
}

// manualSlots is a counting semaphore that grants waiters strictly in
// arrival order.
type manualSlots struct {
	mu      sync.Mutex
	free    int
	waiters []chan struct{}
}

func newManualSlots(n int) *manualSlots { return &manualSlots{free: n} }

// acquire takes a slot, waiting behind earlier callers. It gives up, holding
// nothing, when ctx ends first.
func (s *manualSlots) acquire(ctx context.Context) error {
	s.mu.Lock()
	if s.free > 0 && len(s.waiters) == 0 {
		s.free--
		s.mu.Unlock()
		return nil
	}
	granted := make(chan struct{})
	s.waiters = append(s.waiters, granted)
	s.mu.Unlock()
	select {
	case <-granted:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for i, waiter := range s.waiters {
			if waiter == granted {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				s.mu.Unlock()
				return ctx.Err()
			}
		}
		s.mu.Unlock()
		// The slot was granted as ctx ended; pass it on.
		s.release()
		return ctx.Err()
	}
}

// release returns a slot, handing it to the longest waiter.
func (s *manualSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waiters) > 0 {
		next := s.waiters[0]
		s.waiters = s.waiters[1:]
		close(next)
		return
	}
	s.free++
}

// waiting reports how many callers wait for a slot.
func (s *manualSlots) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}

// searchTarget is the media a search resolved and the languages it runs.
type searchTarget struct {
	mediaID   int64
	media     domain.Media
	revived   bool
	languages []domain.Language
	// idle holds the outcome of every configured language when no language
	// was named and none is missing, so nothing runs.
	idle []control.LanguageOutcome
}

// Search runs a manual search in this process and waits for it. A running
// daemon executes the command itself (searchInDaemon); without one nothing
// would carry a search on after the CLI exits, so waiting limits are refused.
func (a *App) Search(ctx context.Context, args control.SearchArgs) (control.SearchResult, error) {
	// The lock comes first: a daemon still starting holds it before its
	// socket answers, and the command is then busy, not daemon_required.
	release, err := a.acquireRoutedLock()
	if err != nil {
		return control.SearchResult{}, err
	}
	defer release()
	if args.Wait != 0 || args.NoWait {
		return control.SearchResult{}, &control.Error{Code: control.CodeDaemonRequired, Message: "--timeout and --no-wait need a running daemon; without one, search runs to completion"}
	}
	target, err := a.prepareSearch(ctx, args)
	if err != nil {
		return control.SearchResult{}, err
	}
	result := control.SearchResult{MediaID: target.mediaID, Languages: target.idle}
	for _, language := range target.languages {
		outcome, err := a.runManualSearch(ctx, target.mediaID, target.media, language, manualOptions{RetryRejected: args.RetryRejected, revived: target.revived})
		if err != nil {
			return control.SearchResult{}, err
		}
		result.Languages = append(result.Languages, a.languageOutcome(language, outcome))
	}
	return result, nil
}

// daemonWait turns a request's waiting options into how long the daemon waits
// for the work: the default when neither is given, none with noWait.
func daemonWait(wait time.Duration, noWait bool) (time.Duration, error) {
	switch {
	case noWait && wait != 0:
		return 0, &control.Error{Code: control.CodeUsage, Message: "use only one of --timeout and --no-wait"}
	case wait < 0:
		return 0, &control.Error{Code: control.CodeUsage, Message: "--timeout must be positive"}
	case wait == 0 && !noWait:
		return defaultSearchWait, nil
	}
	return wait, nil
}

// searchInDaemon runs a manual search for each language on the daemon's run
// context, so neither the request nor a vanished client cancels it. It waits
// up to args.Wait (5 minutes when unset, none with NoWait) and reports
// running for each language not finished by then.
func (a *App) searchInDaemon(ctx context.Context, args control.SearchArgs) (control.SearchResult, []slog.Attr, error) {
	wait, err := daemonWait(args.Wait, args.NoWait)
	if err != nil {
		return control.SearchResult{}, nil, err
	}
	runCtx := a.serveCtx
	if runCtx == nil || runCtx.Err() != nil {
		return control.SearchResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down"}
	}
	target, err := a.prepareSearch(ctx, args)
	if err != nil {
		return control.SearchResult{}, nil, err
	}
	attrs := []slog.Attr{slog.String("media_title", observability.MediaTitle(target.media))}
	if len(target.languages) == 0 {
		return control.SearchResult{MediaID: target.mediaID, Languages: target.idle}, attrs, nil
	}
	type finished struct {
		index  int
		result workflow.Result
		err    error
	}
	done := make(chan finished, len(target.languages))
	outcomes := make([]control.LanguageOutcome, len(target.languages))
	for index, language := range target.languages {
		outcomes[index] = control.LanguageOutcome{Language: language.String(), Outcome: "running"}
		a.manualRuns.Add(1)
		go func() {
			defer a.manualRuns.Done()
			result, err := a.runManualSearch(runCtx, target.mediaID, target.media, language, manualOptions{RetryRejected: args.RetryRejected, revived: target.revived})
			done <- finished{index, result, err}
		}()
	}
	result := control.SearchResult{MediaID: target.mediaID, Languages: outcomes}
	if wait == 0 {
		return result, attrs, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var failure error
collect:
	for remaining := len(target.languages); remaining > 0; remaining-- {
		select {
		case item := <-done:
			if item.err != nil {
				failure = errors.Join(failure, item.err)
				continue
			}
			outcomes[item.index] = a.languageOutcome(target.languages[item.index], item.result)
		case <-timer.C:
			// What has not finished reports running and carries on.
			break collect
		case <-runCtx.Done():
			return control.SearchResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down; the search was canceled and stays queued"}
		}
	}
	if runCtx.Err() != nil {
		return control.SearchResult{}, nil, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down; the search was canceled and stays queued"}
	}
	if failure != nil {
		return control.SearchResult{}, nil, failure
	}
	return result, attrs, nil
}

// prepareSearch resolves the media, indexes or revives it, and picks the
// languages: the named one, or every configured language that is missing.
func (a *App) prepareSearch(ctx context.Context, args control.SearchArgs) (searchTarget, error) {
	instance, kind, fileID := args.Instance, args.Kind, args.FileID
	var named domain.Language
	if args.Language != "" {
		language, _, err := a.workflowFor(args.Language)
		if err != nil {
			return searchTarget{}, err
		}
		named = language
	}
	if args.Title != "" || args.Path != "" {
		// A title or path names only media subsyncd already indexes.
		_, known, err := a.Resolve(ctx, args.Selection)
		if err != nil {
			return searchTarget{}, err
		}
		instance, kind, fileID = known.Ref.Instance, string(known.Ref.Kind), known.Ref.FileID
	}
	arrCatalog := a.Catalogs[instance]
	if arrCatalog == nil {
		return searchTarget{}, unknownInstance(instance)
	}
	ref := domain.MediaRef{Instance: instance, Kind: domain.MediaKind(kind), FileID: fileID}
	media, err := arrCatalog.GetMedia(ctx, ref)
	if err != nil {
		if catalog.ArrUnreachable(err) {
			return searchTarget{}, &control.Error{Code: control.CodeArrUnreachable, Message: fmt.Sprintf("Arr instance %q is unreachable", instance)}
		}
		return searchTarget{}, fmt.Errorf("load media: %w", err)
	}
	active, err := a.Repository.MediaActive(ctx, media.Ref)
	if err != nil {
		return searchTarget{}, err
	}
	if !active {
		// An unknown file or a deletion tombstone enters the catalog through the
		// transactional media mutation, which schedules every configured
		// language and advances the reconciliation fence like an Arr import.
		now := a.Clock.Now().UTC()
		if _, err := a.Repository.ApplyMediaEvent(ctx, store.MediaEventMutation{
			EventID:   fmt.Sprintf("manual-search:%s:%s:%d:%d", media.Ref.Instance, media.Ref.Kind, media.Ref.FileID, now.UnixNano()),
			Type:      string(catalog.EventImport),
			EntityID:  media.EntityID,
			Ref:       media.Ref,
			Media:     media,
			Languages: sortedLanguages(a.Config),
			At:        now,
			Priority:  store.SearchPriorityMissing,
		}); err != nil {
			return searchTarget{}, err
		}
	}
	mediaID, _, err := a.Repository.UpsertMedia(ctx, media)
	if err != nil {
		return searchTarget{}, err
	}
	target := searchTarget{mediaID: mediaID, media: media, revived: !active}
	if named != "" {
		target.languages = []domain.Language{named}
		return target, nil
	}
	states, err := a.Repository.LanguageStates(ctx, mediaID, a.Clock.Now())
	if err != nil {
		return searchTarget{}, err
	}
	configured := sortedLanguages(a.Config)
	for _, language := range configured {
		if states[language.String()] == "" {
			target.languages = append(target.languages, language)
		}
	}
	if len(target.languages) == 0 {
		for _, language := range configured {
			outcome := "satisfied"
			if states[language.String()] == "running" {
				outcome = "running"
			}
			target.idle = append(target.idle, control.LanguageOutcome{Language: language.String(), Outcome: outcome})
		}
	}
	return target, nil
}

// runManualSearch runs one manual search under the search's lease. It waits
// for a manual slot, then claims the lease, retrying every second while the
// worker holds it, and renews it while the workflow runs. It clears provider
// resume progress (and, with RetryRejected, subsyncd's own rejections), runs
// the full route as a manual search, settles a satisfied or installed search,
// keeps the upgrade schedule, and gives the lease back on every path; after
// cancellation the release uses a fresh bounded context.
func (a *App) runManualSearch(ctx context.Context, mediaID int64, media domain.Media, language domain.Language, opts manualOptions) (workflow.Result, error) {
	jobID := opts.jobID
	if jobID == "" {
		generated, err := manualJobID()
		if err != nil {
			return workflow.Result{}, err
		}
		jobID = generated
	}
	defer a.releaseManualLease(ctx, jobID)
	if err := a.manualSlots.acquire(ctx); err != nil {
		return workflow.Result{}, err
	}
	defer a.manualSlots.release()
	// Only a search this run queued itself, or one the revival just queued,
	// may be settled by it; earlier queued work keeps its progress.
	_, statusErr := a.Repository.GetSearchStatus(ctx, mediaID, language)
	if statusErr != nil && !errors.Is(statusErr, sql.ErrNoRows) {
		return workflow.Result{}, statusErr
	}
	opts.revived = opts.revived || errors.Is(statusErr, sql.ErrNoRows)
	if err := a.claimManualSearch(ctx, mediaID, language, jobID); err != nil {
		return workflow.Result{}, err
	}
	// Under the lease, the row says whether a reject's replacement is still
	// queued; a run that finished it meanwhile has cleared it.
	claimed, err := a.Repository.GetSearchStatus(ctx, mediaID, language)
	if err != nil {
		return workflow.Result{}, err
	}
	opts.replace = claimed.ReplaceInstalled
	started := time.Now()
	workflowCtx := observability.WithAttrs(ctx,
		slog.String("job_id", jobID),
		slog.Int64("media_id", mediaID),
		slog.String("language", language.String()),
		slog.String("priority", "manual"),
		slog.String("instance", media.Ref.Instance),
		slog.String("media_kind", string(media.Ref.Kind)),
		slog.Int64("file_id", media.Ref.FileID),
	)
	logCtx := observability.WithAttrs(workflowCtx, slog.String("media_title", observability.MediaTitle(media)))
	events := a.events().For("app")
	events.Log(logCtx, slog.LevelInfo, "job.started", "subtitle job started")
	result, err := a.runClaimedSearch(workflowCtx, jobID, mediaID, media, language, opts)
	attrs := []slog.Attr{slog.String("outcome", string(result.Outcome)), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
	switch {
	case err != nil && ctx.Err() != nil:
		attrs[0] = slog.String("outcome", "canceled")
		events.Log(logCtx, slog.LevelInfo, "job.completed", "subtitle job completed", attrs...)
	case err != nil:
		attrs[0] = slog.String("outcome", "failed")
		events.Log(logCtx, slog.LevelError, "job.completed", "subtitle job completed", append(attrs, events.ErrorAttrs("manual_search", err)...)...)
	default:
		events.Log(logCtx, slog.LevelInfo, "job.completed", "subtitle job completed", attrs...)
	}
	return result, err
}

// claimManualSearch takes the search's lease for jobID, polling while another
// owner holds it.
func (a *App) claimManualSearch(ctx context.Context, mediaID int64, language domain.Language, jobID string) error {
	poll := a.manualClaimPoll
	if poll <= 0 {
		poll = defaultManualClaimPoll
	}
	for {
		claimed, err := a.Repository.ClaimSearch(ctx, mediaID, language, jobID, a.Clock.Now(), manualLeaseDuration)
		if err != nil {
			return err
		}
		if claimed {
			return nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// runClaimedSearch runs the manual semantics while jobID holds the lease.
func (a *App) runClaimedSearch(ctx context.Context, jobID string, mediaID int64, media domain.Media, language domain.Language, opts manualOptions) (workflow.Result, error) {
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	stopRenewal := a.renewManualLease(runCtx, cancelRun, jobID)
	result, err := a.runManualWorkflow(runCtx, mediaID, media, language, opts)
	if renewErr := stopRenewal(); renewErr != nil {
		return workflow.Result{}, fmt.Errorf("%w: %w", errManualLeaseLost, renewErr)
	}
	if errors.Is(err, errManualTimedOut) {
		return workflow.Result{}, a.recordManualTimeout(ctx, jobID, mediaID, media, language, err)
	}
	if err != nil {
		return workflow.Result{}, err
	}
	if opts.replace {
		// The replacement completes the queued search as the worker would,
		// owner-guarded, with its history row and the next schedule.
		return result, a.completeReplacement(ctx, jobID, media, result)
	}
	if result.Outcome == workflow.OutcomeInstalled {
		// The queued search's due time and counters were for the subtitle this
		// run replaced (or for a missing one), so it completes like a worker
		// install: the new subtitle's upgrade schedule, counters reset. A
		// change that asked for another run meanwhile keeps it pending.
		if err := a.completeManualInstall(ctx, jobID, result); err != nil {
			return workflow.Result{}, err
		}
	} else {
		if opts.revived && result.Outcome == workflow.OutcomeSatisfied {
			// The revival (or this run's claim) queued this language too; this
			// run already answered it, unless a change arrived meanwhile and
			// asked for another run. Earlier queued work keeps its schedule and
			// progress.
			if err := a.Repository.SettlePendingSearch(ctx, mediaID, language, string(result.Outcome), a.Clock.Now(), jobID); err != nil {
				return workflow.Result{}, err
			}
		}
		// The upgrade schedule is kept only for an unleased search, so the
		// lease goes back first. A worker that leases the search in between
		// schedules its own upgrade.
		if err := a.Repository.ReleaseSearchLease(ctx, jobID); err != nil {
			return workflow.Result{}, err
		}
		if !result.NextUpgrade.IsZero() && result.Outcome == workflow.OutcomeSatisfied {
			if err := a.Repository.EnsureUpgradeSearch(ctx, mediaID, language, result.NextUpgrade); err != nil {
				return workflow.Result{}, err
			}
		}
	}
	// The history row is not in the installer's transaction, and it comes last:
	// a crash between the install and this write loses one history row and
	// nothing else, and a failed write never undoes the finished search.
	detail := store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(media), ProviderID: result.Candidate.ProviderID, Manual: true}
	if len(result.Candidate.ReleaseNames) > 0 {
		detail.ReleaseName = result.Candidate.ReleaseNames[0]
	}
	if err := a.Repository.RecordSearchHistory(ctx, mediaID, language, string(result.Outcome), detail, a.Clock.Now()); err != nil {
		events := a.events().For("app")
		events.Log(ctx, slog.LevelWarn, "history.record_failed", "search history entry could not be recorded", events.ErrorAttrs("search_history", err)...)
	}
	return result, nil
}

func (a *App) runManualWorkflow(ctx context.Context, mediaID int64, media domain.Media, language domain.Language, opts manualOptions) (workflow.Result, error) {
	if err := a.Repository.ClearSearchResume(ctx, mediaID, language); err != nil {
		return workflow.Result{}, err
	}
	if opts.RetryRejected {
		if err := a.Repository.ClearCandidateRejections(ctx, mediaID, language); err != nil {
			return workflow.Result{}, err
		}
	}
	run := a.runWorkflow
	if run == nil {
		run = workflowRouter(a.Workflows).Run
	}
	request := workflow.Request{MediaID: mediaID, Media: media, Language: language, Manual: true, ReplaceInstalled: opts.replace}
	limit, grace := a.manualJobLimit, a.manualJobGrace
	if limit <= 0 {
		limit = jobTimeout(a.Config.Sync)
	}
	if grace <= 0 {
		grace = worker.JobGrace
	}
	if limit <= 0 {
		return run(ctx, request)
	}
	// Like a worker job: past the limit the workflow's context is canceled,
	// and one that still has not returned a grace period later is abandoned
	// with its canceled context, which stops it before it publishes.
	timeoutCtx, cancel := context.WithTimeoutCause(ctx, limit, errManualTimedOut)
	defer cancel()
	type outcome struct {
		result workflow.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := run(timeoutCtx, request)
		done <- outcome{result, err}
	}()
	timer := time.NewTimer(limit + grace)
	defer timer.Stop()
	select {
	case finished := <-done:
		if finished.err != nil && context.Cause(timeoutCtx) == errManualTimedOut && ctx.Err() == nil {
			// A workflow that returned a result in time keeps it.
			return workflow.Result{}, fmt.Errorf("%w (%s)", errManualTimedOut, limit)
		}
		return finished.result, finished.err
	case <-timer.C:
		return workflow.Result{}, fmt.Errorf("%w (%s, abandoned after a further %s)", errManualTimedOut, limit, grace)
	case <-ctx.Done():
		// Shutdown or a lost lease canceled the workflow too: give it the
		// grace to return, then abandon it.
		graceTimer := time.NewTimer(grace)
		defer graceTimer.Stop()
		select {
		case finished := <-done:
			return finished.result, finished.err
		case <-graceTimer.C:
			return workflow.Result{}, ctx.Err()
		}
	}
}

// completeManualInstall completes the search jobID owns after a manual run
// installed a subtitle, as the worker completes an install. The history row
// is written afterwards like any manual search's, so a failed write never
// fails the search.
func (a *App) completeManualInstall(ctx context.Context, jobID string, result workflow.Result) error {
	completion := store.SearchCompletion{JobID: jobID, Outcome: string(result.Outcome), NextAttemptAt: result.NextUpgrade, ResetMissingAttempt: true, ResetFailureAttempt: true}
	if !result.NextUpgrade.IsZero() {
		completion.Priority = store.SearchPriorityUpgrade
	}
	_, err := a.Repository.CompleteSearch(ctx, completion)
	return err
}

// completeReplacement completes the search a reject queued, while jobID owns
// it: like a worker completion, an installed or satisfied outcome schedules
// the upgrade, a removed, no_result or rejected one the missing-subtitle
// retry, and a throttle the provider's reset. Every outcome but a throttle
// ends the replacement. A removal's history row was written with it.
func (a *App) completeReplacement(ctx context.Context, jobID string, media domain.Media, result workflow.Result) error {
	now := a.Clock.Now()
	completion := store.SearchCompletion{JobID: jobID, Outcome: string(result.Outcome)}
	switch result.Outcome {
	case workflow.OutcomeInstalled, workflow.OutcomeSatisfied:
		completion.NextAttemptAt = result.NextUpgrade
		if !result.NextUpgrade.IsZero() {
			completion.Priority = store.SearchPriorityUpgrade
		}
		completion.ResetMissingAttempt = true
		completion.ResetFailureAttempt = true
	case workflow.OutcomeRemoved, workflow.OutcomeNoResult, workflow.OutcomeRejected:
		completion = schedule.Scheduler{Clock: a.Clock, AgedAfter: time.Duration(a.Config.AgedReleaseDays) * 24 * time.Hour}.Missing(jobID, 1, media.ReleasedAt)
		completion.Outcome = string(result.Outcome)
		completion.ResetMissingAttempt = true
	case workflow.OutcomeThrottled:
		completion.NextAttemptAt = result.RetryAt
		if completion.NextAttemptAt.Before(now) {
			completion.NextAttemptAt = now
		}
		completion.PreserveQueueOrder = true
	default:
		return fmt.Errorf("workflow returned unsupported outcome %q", result.Outcome)
	}
	completion.ClearReplaceInstalled = result.Outcome != workflow.OutcomeThrottled
	if result.Outcome != workflow.OutcomeRemoved {
		detail := store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(media), ProviderID: result.Candidate.ProviderID, Manual: true}
		if len(result.Candidate.ReleaseNames) > 0 {
			detail.ReleaseName = result.Candidate.ReleaseNames[0]
		}
		completion.History = &detail
	}
	completion.FinishedAt = now
	_, err := a.Repository.CompleteSearch(ctx, completion)
	return err
}

// recordManualTimeout completes a timed-out manual search as a technical
// failure on the failure schedule while jobID still owns it, so the worker
// does not lease it again at once beside a stuck workflow.
func (a *App) recordManualTimeout(ctx context.Context, jobID string, mediaID int64, media domain.Media, language domain.Language, cause error) error {
	events := a.events().For("app")
	events.Log(ctx, slog.LevelWarn, "job.timed_out", "subtitle job exceeded its time limit", slog.String("priority", "manual"))
	status, err := a.Repository.GetSearchStatus(ctx, mediaID, language)
	if err != nil {
		return errors.Join(cause, err)
	}
	completion := schedule.Scheduler{Clock: a.Clock}.Failure(jobID, status.FailureAttempt, "workflow_error")
	completion.History = &store.SearchHistoryDetail{MediaTitle: observability.MediaTitle(media), Manual: true}
	completion.FinishedAt = a.Clock.Now()
	if _, err := a.Repository.CompleteSearch(ctx, completion); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// renewManualLease renews jobID's lease every third of its duration until the
// returned stop is called. A failed renewal cancels the run and stop reports it.
func (a *App) renewManualLease(ctx context.Context, cancelRun context.CancelCauseFunc, jobID string) func() error {
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(manualLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				done <- nil
				return
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := a.Repository.RenewSearchLease(ctx, jobID, a.Clock.Now(), manualLeaseDuration); err != nil {
					if ctx.Err() != nil {
						done <- nil
						return
					}
					cancelRun(errManualLeaseLost)
					done <- err
					return
				}
			}
		}
	}()
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			close(stopped)
			err = <-done
		})
		return err
	}
}

// releaseManualLease gives jobID's lease back, best effort, even after ctx
// ended. Releasing a lease no longer held does nothing.
func (a *App) releaseManualLease(ctx context.Context, jobID string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manualLeaseReleaseTimeout)
	defer cancel()
	_ = a.Repository.ReleaseSearchLease(releaseCtx, jobID)
}

// waitManualSearches waits, until deadline, for the daemon's manual searches
// to end after their context was canceled.
func (a *App) waitManualSearches(deadline time.Time) {
	done := make(chan struct{})
	go func() {
		a.manualRuns.Wait()
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (a *App) events() *observability.Emitter {
	if a.Events == nil {
		return observability.Discard()
	}
	return a.Events
}

func manualJobID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	return "manual-" + hex.EncodeToString(value[:]), nil
}

// languageOutcome maps a workflow outcome to one language's search result.
// The installed subtitle is shown relative to its media root, never absolute.
func (a *App) languageOutcome(language domain.Language, result workflow.Result) control.LanguageOutcome {
	outcome := control.LanguageOutcome{Language: language.String(), Outcome: string(result.Outcome), NextAttemptAt: result.RetryAt}
	if result.Candidate.ProviderID != "" {
		outcome.Provider = result.Candidate.ProviderID
		outcome.Candidate = result.Candidate.ResultID
		outcome.Score = result.Score.Total
		if len(result.Candidate.ReleaseNames) > 0 {
			outcome.Release = result.Candidate.ReleaseNames[0]
		}
	}
	if result.Outcome == workflow.OutcomeRemoved {
		outcome.Reason = string(result.RemovedAfter)
	}
	if result.Outcome == workflow.OutcomeInstalled {
		outcome.SyncVerdict = result.SyncResult.Verdict
		outcome.OffsetMS = result.SyncResult.OffsetMS
		outcome.Subtitle = relativeToRoots(result.Installation.Path, a.Config.MediaRoots)
	}
	return outcome
}
