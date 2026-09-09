package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/match"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

type Outcome string

const (
	OutcomeSatisfied Outcome = "satisfied"
	OutcomeInstalled Outcome = "installed"
	OutcomeNoResult  Outcome = "no_result"
	OutcomeRejected  Outcome = "rejected"
	OutcomeThrottled Outcome = "throttled"
)

type Request struct {
	memberScope string
	MediaID     int64
	Media       domain.Media
	Language    domain.Language
	Manual      bool
	ForceProbe  bool
}

type Decision struct {
	Stage               string
	ProviderID          string
	ResultID            string
	Reason              string
	ReasonCode          string
	SelectionRule       string
	ArchiveType         string
	SubtitleMemberCount int
	MatchingMemberCount int
}

type Result struct {
	Outcome        Outcome
	Candidate      domain.Candidate
	Score          domain.Score
	SyncResult     domain.SyncResult
	Installation   store.Installation
	RetryAt        time.Time
	NextUpgrade    time.Time
	Decisions      []Decision
	ProviderErrors map[string]error
}

type InventoryRefresher interface {
	Refresh(context.Context, int64, domain.Media, bool) (inventory.Inventory, error)
}

type Searcher interface {
	Search(context.Context, provider.SearchQuery) provider.SearchResult
}

type PackCache interface {
	Find(context.Context, domain.Media, domain.Language) (pack.CachedMember, bool, error)
	Put(context.Context, pack.Manifest, time.Time) error
}

type CandidateSynchronizer interface {
	SynchronizeCandidate(context.Context, domain.Candidate, string, string, string) (domain.SyncResult, error)
}

type CandidateInstaller interface {
	Install(context.Context, InstallRequest) (store.Installation, error)
}

type WorkflowRepository interface {
	GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error)
	UpdateInstallationAssessment(context.Context, store.Installation) error
	RecordCandidates(context.Context, int64, domain.Language, []store.CandidateRecord) error
	GetCandidateRejection(context.Context, store.CandidateRejectionLookup) (store.CandidateRejection, bool, error)
	PutCandidateRejection(context.Context, store.CandidateRejection) error
}

type WorkflowClock interface{ Now() time.Time }

type LapsePolicy struct {
	Mode                   string
	BypassScore            int
	RequireIdentityAnchor  bool
	RequireEpisodeEvidence bool
	RequireReleaseGroup    bool
	LapseForPacks          bool
	LapseForUpgrades       bool
}

func DefaultLapsePolicy() LapsePolicy {
	return LapsePolicy{
		Mode:                   "confidence",
		BypassScore:            75,
		RequireIdentityAnchor:  true,
		RequireEpisodeEvidence: true,
		RequireReleaseGroup:    true,
		LapseForPacks:          true,
		LapseForUpgrades:       true,
	}
}

type Service struct {
	Inventory              InventoryRefresher
	Searcher               Searcher
	FallbackSearcher       Searcher
	FallbackProviderOrder  []string
	preferredProviderOrder []string
	fallbackTier           bool
	PackCache              PackCache
	Synchronizer           CandidateSynchronizer
	Installer              CandidateInstaller
	Repository             WorkflowRepository
	Providers              map[string]provider.Provider
	ProviderOrder          []string
	MinimumScore           int
	MinimumUpgradeDelta    int
	PackTTL                time.Duration
	LapsePolicy            LapsePolicy
	AllowHearingImpaired   bool
	Clock                  WorkflowClock
	Events                 *observability.Emitter
}

type downloadedCandidate struct {
	memberIndex int
	memberCount int
	fromCache   bool
	memberScope string
	runtimePack bool
	candidate   domain.Candidate
	score       domain.Score
	priority    int
	path        string
}

// preparedCandidate retains the immutable selected source for rejection identity
// separately from the installable artifact and the result that produced it.
type preparedCandidate struct {
	groupMayReject bool
	downloadedCandidate
	sync   domain.SyncResult
	output string
	bypass bool
}

func (s *Service) Run(ctx context.Context, request Request) (result Result, runErr error) {
	events := s.workflowEvents()
	ctx = observability.WithAttrs(ctx, slog.String("media_title", observability.MediaTitle(request.Media)))
	startedAt := time.Now()
	candidateCount := 0
	events.Log(ctx, slog.LevelInfo, "search.started", "subtitle workflow started",
		slog.Int64("media_id", request.MediaID),
		slog.String("media_kind", string(request.Media.Ref.Kind)),
		slog.Int64("file_id", request.Media.Ref.FileID),
		slog.String("language", request.Language.String()),
		slog.Bool("manual", request.Manual),
	)
	defer func() {
		outcome, reason, level := workflowCompletion(result, runErr)
		attrs := []slog.Attr{
			slog.String("outcome", outcome),
			slog.String("reason", reason),
			slog.Int("candidate_count", candidateCount),
			slog.Int("provider_error_count", len(result.ProviderErrors)),
			slog.Int("score", result.Score.Total),
			slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
		}
		if !result.RetryAt.IsZero() {
			attrs = append(attrs, slog.Time("retry_at", result.RetryAt.UTC()))
		}
		if !result.NextUpgrade.IsZero() {
			attrs = append(attrs, slog.Time("next_upgrade_at", result.NextUpgrade.UTC()))
			events.Log(ctx, slog.LevelInfo, "upgrade.scheduled", "subtitle upgrade scheduled", slog.Time("next_upgrade_at", result.NextUpgrade.UTC()), slog.Int("score", result.Score.Total), slog.Bool("exact_hash", result.Candidate.ExactHash))
		}
		for _, decision := range result.Decisions {
			s.logWorkflowDecision(ctx, decision)
		}
		events.Log(ctx, level, "search.completed", "subtitle workflow completed", attrs...)
	}()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := s.validate(request); err != nil {
		return Result{}, err
	}
	result = Result{ProviderErrors: map[string]error{}}
	current, err := s.Inventory.Refresh(ctx, request.MediaID, request.Media, request.ForceProbe)
	if err != nil {
		events.Log(ctx, slog.LevelError, "inventory.refresh_failed", "subtitle inventory refresh failed", events.ErrorAttrs("inventory", err)...)
		return result, fmt.Errorf("refresh subtitle inventory: %w", err)
	}
	if current.Fingerprint.Path != "" {
		request.Media.Fingerprint = current.Fingerprint
	}
	existing, installed, err := s.Repository.GetInstallation(ctx, request.MediaID, request.Language)
	if err != nil {
		return result, err
	}
	activeInstallation := installed && managedInstallationPresent(current, existing)
	inventorySatisfied := inventoryStopsSearch(current, request.Language, s.AllowHearingImpaired, activeInstallation, existing)
	embeddedCount, sidecarCount := inventoryTrackCounts(current)
	events.Log(ctx, slog.LevelInfo, "inventory.refresh_completed", "subtitle inventory refreshed",
		slog.Int("track_count", len(current.Tracks)),
		slog.Int("embedded_count", embeddedCount),
		slog.Int("sidecar_count", sidecarCount),
		slog.Bool("satisfied", inventorySatisfied),
	)
	if inventorySatisfied {
		result.Outcome = OutcomeSatisfied
		return result, nil
	}
	if activeInstallation && InstallationMatchesMedia(existing, request.Media) {
		_, exact, scoreErr := installedScore(existing)
		if scoreErr != nil {
			return result, scoreErr
		}
		if exact && !s.isFallbackProvider(existing.ProviderID) {
			result.Outcome = OutcomeSatisfied
			return result, nil
		}
	}

	return s.runProviderTiers(ctx, request, existing, activeInstallation, &candidateCount)
}

// acquire performs one provider tier after inventory has been checked once.
func (s *Service) acquire(ctx context.Context, request Request, existing store.Installation, activeInstallation bool, candidateCount *int) (result Result, runErr error) {
	result = Result{ProviderErrors: map[string]error{}}
	var candidateFailures []error
	var exactRecords []store.CandidateRecord
	sameCandidateAssessed := false
	var err error
	workspace, err := os.MkdirTemp("", ".subsyncd-work-")
	if err != nil {
		return result, fmt.Errorf("create candidate workspace: %w", err)
	}
	defer os.RemoveAll(workspace)

	if request.Media.Ref.Kind == domain.MediaEpisode && s.PackCache != nil {
		cached, found, cacheErr, lookupErr := s.findUnrejectedPack(ctx, request, &result)
		cacheOutcome := "miss"
		if found {
			cacheOutcome = "hit"
		}
		if cacheErr != nil || lookupErr != nil {
			cacheOutcome = "error"
		}
		s.workflowEvents().Log(ctx, slog.LevelDebug, "pack_cache.lookup", "pack cache lookup completed", slog.String("cache_outcome", cacheOutcome), slog.Bool("runtime_pack", cached.RuntimePack))
		if lookupErr != nil {
			return result, lookupErr
		}
		if cacheErr != nil {
			result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", Reason: cacheErr.Error()})
		} else if found {
			scoped := request
			if cached.MemberScoped {
				scoped = memberRequest(request, cached.Checksum)
			}
			rejection, rejected, rejectionErr := s.candidateRejection(ctx, scoped, cached.Candidate, cached.Checksum)
			if rejectionErr != nil {
				return result, rejectionErr
			}
			if rejected {
				result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: rejection.ReasonCode})
			} else {
				score := s.evaluateCachedMember(request.Media, cached, request.Language)
				s.logCandidateEvaluation(ctx, request, cached.Candidate, score)
				if !match.Eligible(score, s.minimumScore()) {
					result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: "cached candidate score is below threshold or identity was rejected"})
				} else if activeInstallation && sameInstalledCandidate(existing, request.Media, cached.Candidate) {
					existing, err = s.updateInstallationAssessment(ctx, existing, cached.Candidate, score)
					if err != nil {
						return result, err
					}
					sameCandidateAssessed = true
					s.setReassessmentResult(&result, existing, cached.Candidate, score, s.Clock.Now())
					result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: "refreshed assessment for installed provider candidate"})
				} else {
					downloaded := downloadedCandidate{candidate: cached.Candidate, score: score, runtimePack: cached.RuntimePack, fromCache: true}
					if cached.MemberScoped {
						downloaded.memberScope = "versions"
					}
					paths := []string{cached.Path}
					for _, alternative := range cached.Alternatives {
						paths = append(paths, alternative.Path)
					}
					prepared, prepareErr := s.prepareMembers(ctx, request, downloaded, paths, activeInstallation, existing, workspace, -100, &candidateFailures, &result)
					if prepareErr != nil {
						return result, prepareErr
					}
					sortPrepared(prepared)
					for _, item := range prepared {
						result, err = s.install(ctx, request, item, existing, activeInstallation, result)
						if err != nil {
							if _, rejected := err.(*subtitleValidationError); !rejected {
								return result, err
							}
							if ctxErr := ctx.Err(); ctxErr != nil {
								return result, ctxErr
							}
							if handleErr := s.handleCandidateFailure(ctx, memberRequest(request, item.memberScope), item.candidate, item.path, err, &candidateFailures); handleErr != nil {
								return result, handleErr
							}
						} else if result.Outcome == OutcomeInstalled || result.Outcome == OutcomeSatisfied {
							return result, nil
						}
						if cleanupErr := removeWorkflowArtifact(workspace, item.output); cleanupErr != nil {
							return result, cleanupErr
						}
						result.Outcome = ""
					}
					if err := s.rejectExhaustedVersions(ctx, request, prepared); err != nil {
						return result, err
					}
				}
			}
		}
	}

	exact := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language, Mode: provider.SearchExactHash})
	s.logSearchPhase(ctx, provider.SearchExactHash, exact)
	if err := searchPersistenceFailure(exact.Errors); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkMediaAvailable(request.Media); err != nil {
		return result, err
	}
	terminal, err := s.tryExactCandidates(ctx, request, workspace, existing, activeInstallation, exact, &result, &candidateFailures, &exactRecords)
	*candidateCount = len(exactRecords)
	if err != nil || terminal {
		return result, err
	}

	search := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language, Mode: provider.SearchBroad})
	s.logSearchPhase(ctx, provider.SearchBroad, search)
	if err := searchPersistenceFailure(search.Errors); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := checkMediaAvailable(request.Media); err != nil {
		return result, err
	}
	result.ProviderErrors = search.Errors
	if len(search.Candidates) == 0 {
		*candidateCount = len(exactRecords)
		if len(exactRecords) != 0 || len(s.ProviderOrder) == 0 || len(search.Errors) < len(s.ProviderOrder) {
			if err := s.Repository.RecordCandidates(ctx, request.MediaID, request.Language, exactRecords); err != nil {
				return result, err
			}
		}
		if sameCandidateAssessed {
			result.Outcome = OutcomeSatisfied
			result.Installation = existing
			return result, nil
		}
		if retry, allUnavailable := allProvidersUnavailable(search.Errors, len(s.ProviderOrder)); allUnavailable {
			result.Outcome = OutcomeThrottled
			result.RetryAt = retry
			return result, nil
		}
		if len(s.ProviderOrder) > 0 && len(search.Errors) >= len(s.ProviderOrder) {
			return result, &acquisitionExhaustedError{fmt.Errorf("all %d assigned subtitle providers failed", len(s.ProviderOrder))}
		}
		if classified, failureErr, found := classifyCandidateFailures(result, candidateFailures); found {
			return classified, failureErr
		}
		if len(exactRecords) != 0 {
			result.Outcome = OutcomeRejected
			return result, nil
		}
		result.Outcome = OutcomeNoResult
		return result, nil
	}

	evaluated := make([]match.EvaluatedCandidate, 0, len(search.Candidates))
	records := make([]store.CandidateRecord, 0, len(search.Candidates))
	priorities := s.priorities()
	for _, candidate := range search.Candidates {
		score := s.evaluate(request.Media, candidate, request.Language)
		s.logCandidateEvaluation(ctx, request, candidate, score)
		priority, exists := priorities[candidate.ProviderID]
		if !exists {
			priority = len(s.ProviderOrder)
		}
		evaluated = append(evaluated, match.EvaluatedCandidate{Candidate: candidate, Score: score, ProviderPriority: priority})
		record, recordErr := candidateRecord(candidate, score, match.Eligible(score, s.minimumScore()))
		if recordErr != nil {
			return result, recordErr
		}
		records = append(records, record)
	}
	records = mergeCandidateRecords(exactRecords, records)
	*candidateCount = len(records)
	if err := s.Repository.RecordCandidates(ctx, request.MediaID, request.Language, records); err != nil {
		return result, err
	}
	match.Rank(evaluated)
	if activeInstallation && InstallationMatchesMedia(existing, request.Media) {
		for _, item := range evaluated {
			if !match.Eligible(item.Score, s.minimumScore()) || !sameInstalledCandidate(existing, request.Media, item.Candidate) {
				continue
			}
			existing, err = s.updateInstallationAssessment(ctx, existing, item.Candidate, item.Score)
			if err != nil {
				return result, err
			}
			sameCandidateAssessed = true
			s.setReassessmentResult(&result, existing, item.Candidate, item.Score, s.Clock.Now())
			result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: "refreshed assessment for installed provider candidate"})
			break
		}
	}
	eligible := evaluated[:0]
	for _, item := range evaluated {
		if !match.Eligible(item.Score, s.minimumScore()) {
			continue
		}
		if activeInstallation {
			if sameInstalledCandidate(existing, request.Media, item.Candidate) {
				result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: "provider candidate is already installed"})
				continue
			}
			allowed, upgradeErr := s.shouldUpgrade(existing, request.Media, item.Candidate, item.Score)
			if upgradeErr != nil {
				return result, upgradeErr
			}
			if !allowed {
				continue
			}
		}
		rejection, rejected, rejectionErr := s.candidateRejection(ctx, request, item.Candidate, "")
		if rejectionErr != nil {
			return result, rejectionErr
		}
		if rejected {
			result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: rejection.ReasonCode})
			continue
		}
		eligible = append(eligible, item)
	}
	if len(eligible) == 0 {
		if sameCandidateAssessed {
			result.Outcome = OutcomeSatisfied
			result.Installation = existing
			return result, nil
		}
		if classified, failureErr, found := classifyCandidateFailures(result, candidateFailures); found {
			return classified, failureErr
		}
		result.Outcome = OutcomeRejected
		return result, nil
	}
	for _, item := range eligible {
		if item.Candidate.ExactHash {
			eligible = []match.EvaluatedCandidate{item}
			break
		}
	}
	if len(eligible) > 3 {
		eligible = eligible[:3]
	}

	for tierStart := 0; tierStart < len(eligible); {
		tierEnd := tierStart + 1
		for tierEnd < len(eligible) && eligible[tierEnd].Score.Total == eligible[tierStart].Score.Total {
			tierEnd++
		}
		result.Decisions = append(result.Decisions, Decision{Stage: "tournament_tier", Reason: fmt.Sprintf("evaluating score %d", eligible[tierStart].Score.Total)})
		preparedTier := make([]preparedCandidate, 0, tierEnd-tierStart)
		for index := tierStart; index < tierEnd; index++ {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := checkMediaAvailable(request.Media); err != nil {
				return result, err
			}
			item := eligible[index]
			paths, runtimePack, decisions, prepareErr := s.downloadAndSelectMembers(ctx, request, item.Candidate, workspace, index)
			result.Decisions = append(result.Decisions, decisions...)
			if prepareErr != nil {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				recorded, rejectionErr := s.recordCandidateRejection(ctx, request, item.Candidate, "", prepareErr)
				if rejectionErr != nil {
					return result, rejectionErr
				}
				if !recorded && !isMediaValidationRejection(prepareErr) {
					candidateFailures = append(candidateFailures, prepareErr)
				}
				result.Decisions = append(result.Decisions, candidateFailureDecision("candidate", item.Candidate, prepareErr))
				continue
			}
			downloaded := downloadedCandidate{candidate: item.Candidate, score: item.Score, priority: item.ProviderPriority, runtimePack: runtimePack}
			prepared, prepareErr := s.prepareMembers(ctx, request, downloaded, paths, activeInstallation, existing, workspace, index*4, &candidateFailures, &result)
			if prepareErr != nil {
				return result, prepareErr
			}
			preparedTier = append(preparedTier, prepared...)
		}
		sortPrepared(preparedTier)
		for index, prepared := range preparedTier {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result, err = s.install(ctx, request, prepared, existing, activeInstallation, result)
			if err != nil {
				// Only the installer's direct pre-publication content rejection
				// permits fallback. Wrapped infrastructure/rollback errors are terminal.
				if _, rejected := err.(*subtitleValidationError); !rejected {
					return result, err
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return result, ctxErr
				}
				// Rejection identity follows the selected source, not the LAPSE
				// derivative, just as it does for preparation failures.
				if handleErr := s.handleCandidateFailure(ctx, memberRequest(request, prepared.memberScope), prepared.candidate, prepared.path, err, &candidateFailures); handleErr != nil {
					return result, handleErr
				}
				result.Decisions = append(result.Decisions, candidateFailureDecision("installation", prepared.candidate, err))
			} else if result.Outcome == OutcomeInstalled || result.Outcome == OutcomeSatisfied {
				result.Decisions = append(result.Decisions, Decision{Stage: "early_stop", ProviderID: prepared.candidate.ProviderID, ResultID: prepared.candidate.ResultID, Reason: fmt.Sprintf("installed from score %d; lower tiers skipped", prepared.score.Total)})
				return result, nil
			}
			if cleanupErr := errors.Join(removeWorkflowArtifact(workspace, prepared.path), removeWorkflowArtifact(workspace, prepared.output)); cleanupErr != nil {
				return result, cleanupErr
			}
			result.Outcome = ""
			if index+1 < len(preparedTier) {
				result.Decisions = append(result.Decisions, Decision{Stage: "fallback", Reason: "next candidate in tier"})
			}
		}
		if err := s.rejectExhaustedVersions(ctx, request, preparedTier); err != nil {
			return result, err
		}
		if tierEnd < len(eligible) {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			result.Decisions = append(result.Decisions, Decision{Stage: "fallback", Reason: "next score tier"})
		}
		tierStart = tierEnd
	}
	if classified, failureErr, found := classifyCandidateFailures(result, candidateFailures); found {
		return classified, failureErr
	}
	result.Outcome = OutcomeRejected
	return result, nil
}

func (s *Service) workflowEvents() *observability.Emitter {
	if s.Events == nil {
		return observability.Discard().For("workflow")
	}
	return s.Events.For("workflow")
}

func workflowCompletion(result Result, err error) (string, string, slog.Level) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "canceled", "canceled", slog.LevelWarn
		}
		return "failed", workflowErrorKind(err), slog.LevelError
	}
	switch result.Outcome {
	case OutcomeSatisfied:
		if result.Candidate.ResultID != "" {
			return string(result.Outcome), "provenance_refreshed", slog.LevelInfo
		}
		return string(result.Outcome), "existing_subtitle", slog.LevelInfo
	case OutcomeInstalled:
		return string(result.Outcome), "installed", slog.LevelInfo
	case OutcomeNoResult:
		return string(result.Outcome), "no_candidate", slog.LevelInfo
	case OutcomeRejected:
		return string(result.Outcome), "no_eligible_candidate", slog.LevelInfo
	case OutcomeThrottled:
		return string(result.Outcome), "provider_unavailable", slog.LevelWarn
	default:
		return "failed", "incomplete_result", slog.LevelError
	}
}

func workflowErrorKind(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "inventory") || strings.Contains(message, "ffprobe"):
		return "inventory"
	case strings.Contains(message, "provider"):
		return "provider_search"
	case strings.Contains(message, "LAPSE"):
		return "lapse"
	case strings.Contains(message, "install") || strings.Contains(message, "subtitle"):
		return "installation"
	default:
		return "workflow"
	}
}

func inventoryTrackCounts(current inventory.Inventory) (embedded, sidecar int) {
	for _, track := range current.Tracks {
		if track.Embedded {
			embedded++
		} else {
			sidecar++
		}
	}
	return embedded, sidecar
}

func (s *Service) logCandidateEvaluation(ctx context.Context, request Request, candidate domain.Candidate, score domain.Score) {
	components := make([]map[string]any, 0, len(score.Contributions))
	for _, item := range score.Contributions {
		components = append(components, map[string]any{
			"signal": observability.SafeText(item.Signal),
			"points": item.Points,
			"reason": observability.SafeText(item.Reason),
		})
	}
	attrs := []slog.Attr{
		slog.String("provider", observability.SafeText(candidate.ProviderID)),
		slog.String("candidate_id", observability.SafeText(candidate.ResultID)),
		slog.Int("score", score.Total),
		slog.Bool("eligible", match.Eligible(score, s.minimumScore())),
		slog.Any("score_components", components),
		slog.Any("rejected_reasons", safeStrings(score.RejectedReasons)),
		slog.Any("release_names", safeStrings(candidate.ReleaseNames)),
	}
	if relative, ok := s.workflowEvents().RelativePath(request.Media.Fingerprint.Path); ok {
		attrs = append(attrs, slog.String("relative_path", relative))
	}
	s.workflowEvents().Log(ctx, slog.LevelDebug, "candidate.evaluated", "subtitle candidate evaluated", attrs...)
}

func (s *Service) logSearchPhase(ctx context.Context, mode provider.SearchMode, search provider.SearchResult) {
	s.workflowEvents().Log(ctx, slog.LevelDebug, "search.phase_completed", "subtitle search phase completed",
		slog.String("search_mode", string(mode)),
		slog.Int("candidate_count", len(search.Candidates)),
		slog.Int("provider_error_count", len(search.Errors)),
	)
}

func (s *Service) logWorkflowDecision(ctx context.Context, decision Decision) {
	event := "candidate.decision"
	level := slog.LevelDebug
	switch decision.Stage {
	case "candidate_rejection":
		event = "candidate.skipped"
		level = slog.LevelInfo
		switch decision.Reason {
		case "pack_selection", "invalid_subtitle", "lapse_unsure", "lapse_nothing", "lapse_invalid_output":
			decision.ReasonCode = decision.Reason
		default:
			decision.ReasonCode = "retained_rejection"
		}
	case "candidate":
		event = "candidate.rejection_details"
	case "tournament_tier":
		event = "candidate.tier_started"
	case "fallback":
		event = "candidate.fallback"
	case "early_stop":
		event = "candidate.early_stopped"
	}
	attrs := []slog.Attr{slog.String("stage", observability.SafeText(decision.Stage))}
	if decision.ProviderID != "" {
		attrs = append(attrs, slog.String("provider", observability.SafeText(decision.ProviderID)))
	}
	if decision.ResultID != "" {
		attrs = append(attrs, slog.String("candidate_id", observability.SafeText(decision.ResultID)))
	}
	if decision.ReasonCode != "" {
		attrs = append(attrs,
			slog.String("reason_code", observability.SafeText(decision.ReasonCode)),
			slog.String("selection_rule", observability.SafeText(decision.SelectionRule)),
			slog.String("archive_type", observability.SafeText(decision.ArchiveType)),
			slog.Int("subtitle_member_count", decision.SubtitleMemberCount),
			slog.Int("matching_member_count", decision.MatchingMemberCount),
		)
	}
	s.workflowEvents().Log(ctx, level, event, "subtitle candidate decision", attrs...)
}

func candidateFailureDecision(stage string, candidate domain.Candidate, failure error) Decision {
	decision := Decision{Stage: stage, ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: failure.Error()}
	var selection *pack.SelectionError
	if errors.As(failure, &selection) {
		decision.ReasonCode = "pack_selection"
		decision.SelectionRule = selection.Rule
		decision.ArchiveType = selection.ArchiveType
		decision.SubtitleMemberCount = selection.MemberCount
		decision.MatchingMemberCount = selection.MatchingMemberCount
	}
	return decision
}

func safeStrings(values []string) []string {
	result := make([]string, 0, min(len(values), 8))
	for _, value := range values {
		if len(result) == 8 {
			break
		}
		if safe := observability.SafeText(value); safe != "" {
			result = append(result, safe)
		}
	}
	return result
}

func (s *Service) logLapseCompleted(ctx context.Context, phase string, candidate domain.Candidate, result domain.SyncResult, duration time.Duration) {
	level := slog.LevelInfo
	if result.Verdict != "solid" {
		level = slog.LevelWarn
	}
	event := "lapse." + phase + "_completed"
	s.workflowEvents().Log(ctx, level, event, "LAPSE phase completed", lapseAttrs(s, phase, candidate, result, duration)...)
}

func (s *Service) logLapseStarted(ctx context.Context, phase string, candidate domain.Candidate) {
	s.workflowEvents().Log(ctx, slog.LevelInfo, "lapse."+phase+"_started", "LAPSE phase started",
		slog.String("phase", phase),
		slog.String("provider", observability.SafeText(candidate.ProviderID)),
		slog.String("candidate_id", observability.SafeText(candidate.ResultID)),
		slog.String("compatibility_version", lapseCompatibilityVersion(s)),
	)
}

func (s *Service) logLapseFailure(ctx context.Context, phase string, candidate domain.Candidate, duration time.Duration, err error) {
	result := domain.SyncResult{}
	level := slog.LevelError
	var verdict *syncer.VerdictError
	if errors.As(err, &verdict) {
		result.Verdict = observability.SafeText(verdict.Verdict)
		level = slog.LevelWarn
	}
	attrs := lapseAttrs(s, phase, candidate, result, duration)
	attrs = append(attrs, s.workflowEvents().ErrorAttrs("lapse_"+phase, err)...)
	s.workflowEvents().Log(ctx, level, "lapse.failed", "LAPSE phase failed", attrs...)
}

func lapseAttrs(s *Service, phase string, candidate domain.Candidate, result domain.SyncResult, duration time.Duration) []slog.Attr {
	version := lapseCompatibilityVersion(s)
	return []slog.Attr{
		slog.String("phase", phase),
		slog.String("provider", observability.SafeText(candidate.ProviderID)),
		slog.String("candidate_id", observability.SafeText(candidate.ResultID)),
		slog.Int64("duration_ms", duration.Milliseconds()),
		slog.String("verdict", observability.SafeText(result.Verdict)),
		slog.String("mode", observability.SafeText(result.Mode)),
		slog.Int64("offset_ms", result.OffsetMS),
		slog.Float64("ratio", result.Ratio),
		slog.Float64("confidence", result.Confidence),
		slog.Float64("agreement", result.Agreement),
		slog.Float64("coverage", result.Coverage),
		slog.Int("parts", result.Parts),
		slog.Int("splits", result.Splits),
		slog.String("compatibility_version", version),
	}
}

func lapseCompatibilityVersion(s *Service) string {
	version := "unknown"
	if versioned, ok := s.Synchronizer.(interface{ CompatibilityVersion() string }); ok {
		if safe := observability.SafeText(versioned.CompatibilityVersion()); safe != "" {
			version = safe
		}
	}
	return version
}

func lapseFailureDecision(failure error) string {
	var verdict *syncer.VerdictError
	if errors.As(failure, &verdict) {
		return "non-solid"
	}
	return "error"
}

func (s *Service) handleCandidateFailure(ctx context.Context, request Request, candidate domain.Candidate, path string, failure error, candidateFailures *[]error) error {
	checksum, checksumErr := fileChecksum(path)
	if checksumErr != nil {
		return checksumErr
	}
	recorded, rejectionErr := s.recordCandidateRejection(ctx, request, candidate, checksum, failure)
	if rejectionErr != nil {
		return rejectionErr
	}
	if !recorded && !isMediaValidationRejection(failure) {
		*candidateFailures = append(*candidateFailures, failure)
	}
	return nil
}

func (s *Service) candidateRejection(ctx context.Context, request Request, candidate domain.Candidate, artifactChecksum string) (store.CandidateRejection, bool, error) {
	signature, err := candidateSignature(candidate, request.Media)
	if request.memberScope != "" {
		signature += "/member-" + request.memberScope
	}
	if err != nil {
		return store.CandidateRejection{}, false, err
	}
	fingerprint := request.Media.Fingerprint
	return s.Repository.GetCandidateRejection(ctx, store.CandidateRejectionLookup{
		MediaID: request.MediaID, Language: request.Language.String(), ProviderID: candidate.ProviderID, ResultID: candidate.ResultID,
		CandidateSignature: signature, ArtifactChecksum: artifactChecksum, ToolSignature: s.rejectionToolSignature(),
		MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano(), Now: s.Clock.Now(),
	})
}

func (s *Service) recordCandidateRejection(ctx context.Context, request Request, candidate domain.Candidate, artifactChecksum string, failure error) (bool, error) {
	var verdict *syncer.VerdictError
	var selection *pack.SelectionError
	var content *pack.ContentError
	_, installContent := failure.(*subtitleValidationError)
	_, invalidOutput := failure.(*syncer.InvalidOutputError)
	reasonCode := ""
	switch {
	case invalidOutput:
		reasonCode = "lapse_invalid_output"
	case errors.As(failure, &verdict) && (verdict.Verdict == "unsure" || verdict.Verdict == "nothing"):
		reasonCode = "lapse_" + verdict.Verdict
	case errors.As(failure, &selection):
		reasonCode = "pack_selection"
	case errors.As(failure, &content) || installContent:
		reasonCode = "invalid_subtitle"
	default:
		return false, nil
	}
	signature, err := candidateSignature(candidate, request.Media)
	if request.memberScope != "" {
		signature += "/member-" + request.memberScope
	}
	if err != nil {
		return false, err
	}
	now := s.Clock.Now()
	fingerprint := request.Media.Fingerprint
	rejection := store.CandidateRejection{
		MediaID: request.MediaID, Language: request.Language.String(), ProviderID: candidate.ProviderID, ResultID: candidate.ResultID,
		CandidateSignature: signature, ArtifactChecksum: artifactChecksum, ReasonCode: reasonCode, ToolSignature: s.rejectionToolSignature(),
		MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano(), RejectedAt: now,
	}
	if err := s.Repository.PutCandidateRejection(ctx, rejection); err != nil {
		return false, err
	}
	decision := candidateFailureDecision("candidate", candidate, failure)
	decision.ReasonCode = reasonCode
	s.logCandidateRejection(ctx, decision)
	return true, nil
}

func isMediaValidationRejection(failure error) bool {
	var noSpeech *syncer.NoSpeechError
	return errors.As(failure, &noSpeech)
}

func candidateSignature(candidate domain.Candidate, media ...domain.Media) (string, error) {
	safe := candidate
	safe.DownloadRef = ""
	safe.Rating = 0
	safe.Popularity = 0
	safe.DownloadCount = 0
	safe.AlternateTitles = slices.Clone(safe.AlternateTitles)
	slices.Sort(safe.AlternateTitles)
	safe.AlternateTitles = slices.Compact(safe.AlternateTitles)
	safe.ReleaseNames = slices.Clone(safe.ReleaseNames)
	slices.Sort(safe.ReleaseNames)
	safe.ReleaseNames = slices.Compact(safe.ReleaseNames)
	safe.ReleaseGroups = slices.Clone(safe.ReleaseGroups)
	slices.Sort(safe.ReleaseGroups)
	safe.ReleaseGroups = slices.Compact(safe.ReleaseGroups)
	safe.Resolutions = slices.Clone(safe.Resolutions)
	slices.Sort(safe.Resolutions)
	safe.Resolutions = slices.Compact(safe.Resolutions)
	if safe.Pack != nil {
		packInfo := *safe.Pack
		packInfo.DirectMembers = append([]domain.PackMemberRef(nil), safe.Pack.DirectMembers...)
		for index := range packInfo.DirectMembers {
			packInfo.DirectMembers[index].DownloadRef = ""
		}
		slices.SortFunc(packInfo.DirectMembers, func(a, b domain.PackMemberRef) int {
			left, _ := json.Marshal(a)
			right, _ := json.Marshal(b)
			return strings.Compare(string(left), string(right))
		})
		packInfo.DirectMembers = slices.Compact(packInfo.DirectMembers)
		safe.Pack = &packInfo
	}
	payload, err := json.Marshal(safe)
	if err != nil {
		return "", fmt.Errorf("encode candidate signature: %w", err)
	}
	if len(media) > 0 && media[0].Ref.Kind == domain.MediaEpisode {
		// Sonarr can correct selection evidence without replacing the physical file.
		evidence, _ := json.Marshal(struct {
			Season          int
			Episode         int
			AbsoluteEpisode int
			EpisodeTitle    string
		}{media[0].Season, media[0].Episode, media[0].AbsoluteEpisode, media[0].EpisodeTitle})
		payload = append(payload, evidence...)
		payload = append(payload, []byte("/episode-selection-v2")...)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:]), nil
}

func (s *Service) rejectionToolSignature() string {
	version := "unknown"
	if versioned, ok := s.Synchronizer.(interface{ CompatibilityVersion() string }); ok {
		version = versioned.CompatibilityVersion()
	}
	payload, _ := json.Marshal(s.LapsePolicy.normalized())
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("lapse-%s/policy-%x", version, sum[:8])
}

func fileChecksum(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("checksum candidate artifact: %w", err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:]), nil
}

func (s *Service) validate(request Request) error {
	if s.Inventory == nil || s.Searcher == nil || s.Synchronizer == nil || s.Installer == nil || s.Repository == nil || s.Clock == nil {
		return fmt.Errorf("workflow dependencies are incomplete")
	}
	if request.MediaID <= 0 || request.Language == "" || request.Media.Fingerprint.Path == "" {
		return fmt.Errorf("workflow request identity is incomplete")
	}
	return nil
}

func (s *Service) downloadAndSelect(ctx context.Context, request Request, candidate domain.Candidate, workspace string, index int) (string, bool, []Decision, error) {
	paths, runtime, decisions, err := s.downloadAndSelectMembers(ctx, request, candidate, workspace, index)
	if err != nil {
		return "", runtime, decisions, err
	}
	return paths[0], runtime, decisions, nil
}

func (s *Service) downloadAndSelectMembers(ctx context.Context, request Request, candidate domain.Candidate, workspace string, index int) (selected []string, runtimePack bool, decisions []Decision, retErr error) {
	adapter := s.Providers[candidate.ProviderID]
	if adapter == nil {
		return nil, runtimePack, nil, fmt.Errorf("provider %q is not available for download", candidate.ProviderID)
	}
	payloadPath := filepath.Join(workspace, fmt.Sprintf("download-%d", index))
	payload, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, runtimePack, nil, err
	}
	extractionPath := filepath.Join(workspace, fmt.Sprintf("extracted-%d", index))
	defer func() {
		if cleanupErr := errors.Join(os.RemoveAll(payloadPath), os.RemoveAll(extractionPath)); cleanupErr != nil {
			// Cleanup failure is infrastructure failure, even when processing also
			// rejected content. Do not persist it as a deterministic rejection.
			selected = nil
			retErr = fmt.Errorf("cleanup candidate scratch: %v; processing error: %v", cleanupErr, retErr)
		}
	}()
	limits := pack.DefaultLimits()
	bounded := &boundedDownloadWriter{writer: payload, remaining: limits.MaxCompressed, limit: limits.MaxCompressed}
	metadata, downloadErr := adapter.Download(ctx, candidate, bounded)
	syncErr := payload.Sync()
	closeErr := payload.Close()
	if localErr := errors.Join(bounded.writeErr, syncErr, closeErr); localErr != nil {
		return nil, runtimePack, nil, fmt.Errorf("write or flush provider download: %w", localErr)
	}
	if bounded.exceeded {
		return nil, runtimePack, nil, &pack.ContentError{Err: fmt.Errorf("provider download exceeds %d bytes", bounded.limit)}
	}
	if downloadErr != nil {
		return nil, runtimePack, nil, downloadErr
	}
	info, err := os.Stat(payloadPath)
	if err != nil {
		return nil, runtimePack, nil, err
	}
	reader, err := os.Open(payloadPath)
	if err != nil {
		return nil, runtimePack, nil, err
	}
	extractionCandidate := candidate
	if metadata.Filename != "" {
		extractionCandidate.DownloadRef = metadata.Filename
	}
	manifest, err := pack.Extract(ctx, extractionCandidate, reader, info.Size(), extractionPath, pack.DefaultLimits())
	closeErr = reader.Close()
	removeErr := os.Remove(payloadPath)
	if closeErr != nil || removeErr != nil {
		return nil, runtimePack, nil, fmt.Errorf("release downloaded payload: %v", errors.Join(closeErr, removeErr))
	}
	if err != nil {
		return nil, runtimePack, nil, err
	}
	runtimePack = manifest.RuntimePack
	classification := "single"
	if candidate.Pack != nil {
		classification = "provider_pack"
	} else if runtimePack {
		classification = "runtime_pack"
	}
	s.workflowEvents().Log(ctx, slog.LevelDebug, "archive.classified", "subtitle archive classified", slog.String("archive_type", manifest.ArchiveType), slog.Int("subtitle_member_count", len(manifest.Members)), slog.String("classification", classification))
	var member pack.Member
	var members []pack.Member
	if request.Media.Ref.Kind == domain.MediaEpisode && candidate.Pack == nil && !runtimePack && len(manifest.Members) == 1 {
		member, err = pack.SelectSingleEpisode(manifest, extractionCandidate, request.Media, false)
		if err != nil {
			return nil, runtimePack, nil, err
		}
	} else if request.Media.Ref.Kind == domain.MediaEpisode {
		members, err = pack.SelectAlternatives(manifest, extractionCandidate, request.Media, false)
		if err != nil {
			return nil, runtimePack, nil, err
		}
	} else if len(manifest.Members) == 1 {
		member, err = pack.SelectSingleMovie(manifest, extractionCandidate, false)
		if err != nil {
			return nil, runtimePack, nil, err
		}
	} else {
		return nil, runtimePack, nil, &pack.SelectionError{Reason: "movie candidate archive contains multiple subtitle files"}
	}
	cacheable := candidate.Pack != nil
	if runtimePack {
		_, cacheable = pack.RuntimeCacheSeason(manifest)
	}
	cacheOutcome := "not_pack"
	if candidate.Pack != nil || runtimePack {
		cacheOutcome = "unsafe_identity"
	}
	if cacheable && s.PackCache == nil {
		cacheOutcome = "disabled"
	}
	if cacheable && s.PackCache != nil {
		cacheOutcome = "published"
		if err := s.PackCache.Put(ctx, manifest, s.Clock.Now().Add(s.packTTL())); err != nil {
			cacheOutcome = "write_failed"
			decisions = append(decisions, Decision{Stage: "pack_cache", ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: fmt.Sprintf("cache normalized pack: %v", err)})
		}
	}
	s.workflowEvents().Log(ctx, slog.LevelDebug, "pack_cache.publication", "pack cache publication considered", slog.String("cache_outcome", cacheOutcome))
	if len(members) == 0 {
		members = []pack.Member{member}
	}
	s.workflowEvents().Log(ctx, slog.LevelInfo, "archive.members_selected", "subtitle archive members selected", slog.String("provider", observability.SafeText(candidate.ProviderID)), slog.String("candidate_id", observability.SafeText(candidate.ResultID)), slog.String("selection_rule", members[0].SelectionRule), slog.Int("matching_member_count", len(members)), slog.Int("subtitle_member_count", len(manifest.Members)))
	for n, member := range members {
		path := filepath.Join(workspace, fmt.Sprintf("selected-%d-%d%s", index, n, filepath.Ext(member.NormalizedPath)))
		if err := os.Rename(member.NormalizedPath, path); err != nil {
			return nil, runtimePack, decisions, err
		}
		selected = append(selected, path)
	}
	return selected, runtimePack, decisions, nil
}

func (s *Service) prepareCandidate(ctx context.Context, request Request, item downloadedCandidate, installed bool, existing store.Installation, workspace string, index int) (ready preparedCandidate, retErr error) {
	if !match.Eligible(item.score, s.minimumScore()) {
		return preparedCandidate{}, fmt.Errorf("candidate score is below threshold or identity was rejected")
	}
	if installed {
		if sameInstalledCandidate(existing, request.Media, item.candidate) {
			return preparedCandidate{}, fmt.Errorf("provider candidate is already installed")
		}
		allowed, err := s.shouldUpgrade(existing, request.Media, item.candidate, item.score)
		if err != nil || !allowed {
			if err != nil {
				return preparedCandidate{}, err
			}
			return preparedCandidate{}, fmt.Errorf("candidate does not satisfy upgrade policy")
		}
	}
	if item.candidate.ExactHash {
		return preparedCandidate{downloadedCandidate: item, output: item.path, sync: domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1}, bypass: true}, nil
	}
	if !item.runtimePack && canBypassLapse(request.Media, item.candidate, item.score, installed, s.LapsePolicy.normalized()) {
		return preparedCandidate{downloadedCandidate: item, output: item.path, sync: domain.SyncResult{Verdict: "score_bypass", Mode: "bypass", Reference: "release_evidence"}, bypass: true}, nil
	}
	// Cached preparation uses a negative range; broad candidates each reserve
	// four slots for their bounded versions. Exact work runs between these phases.
	// Each preparation therefore owns a distinct output even across fallback.
	extension := strings.ToLower(filepath.Ext(item.path))
	output := filepath.Join(workspace, fmt.Sprintf("synchronized-%d%s", index, extension))
	defer func() {
		if retErr != nil {
			if cleanupErr := removeWorkflowArtifact(workspace, output); cleanupErr != nil {
				retErr = fmt.Errorf("cleanup synchronized scratch: %v; processing error: %v", cleanupErr, retErr)
			}
		}
	}()
	s.logLapseStarted(ctx, "sync", item.candidate)
	startedAt := time.Now()
	synchronized, err := s.Synchronizer.SynchronizeCandidate(ctx, item.candidate, request.Media.Fingerprint.Path, item.path, output)
	if err != nil {
		s.logLapseFailure(ctx, "sync", item.candidate, time.Since(startedAt), err)
		return preparedCandidate{}, err
	}
	s.logLapseCompleted(ctx, "sync", item.candidate, synchronized, time.Since(startedAt))
	if synchronized.Verdict != "solid" {
		return preparedCandidate{}, fmt.Errorf("LAPSE synchronization was not solid")
	}
	return preparedCandidate{downloadedCandidate: item, sync: synchronized, output: output}, nil
}

func sameInstalledCandidate(existing store.Installation, media domain.Media, candidate domain.Candidate) bool {
	return InstallationMatchesMedia(existing, media) &&
		existing.ProviderID == candidate.ProviderID &&
		existing.CandidateID == candidate.ResultID
}

func (s *Service) updateInstallationAssessment(ctx context.Context, existing store.Installation, candidate domain.Candidate, score domain.Score) (store.Installation, error) {
	startedAt := time.Now()
	scoreJSON, err := json.Marshal(score)
	if err != nil {
		return existing, fmt.Errorf("encode refreshed installed score: %w", err)
	}
	existing.ScoreJSON = scoreJSON
	if candidate.ExactHash {
		syncJSON, marshalErr := json.Marshal(domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1})
		if marshalErr != nil {
			return existing, fmt.Errorf("encode refreshed exact-hash provenance: %w", marshalErr)
		}
		existing.SyncResultJSON = syncJSON
	}
	if err := s.Repository.UpdateInstallationAssessment(ctx, existing); err != nil {
		s.workflowEvents().Log(ctx, slog.LevelError, "subtitle.provenance_refresh_failed", "subtitle provenance refresh failed", append([]slog.Attr{slog.String("provider", candidate.ProviderID), slog.String("candidate_id", candidate.ResultID), slog.Int("score", score.Total), slog.Int64("duration_ms", time.Since(startedAt).Milliseconds())}, s.workflowEvents().ErrorAttrs("persistence", err)...)...)
		return existing, fmt.Errorf("refresh installed candidate assessment: %w", err)
	}
	s.workflowEvents().Log(ctx, slog.LevelInfo, "subtitle.provenance_refreshed", "subtitle provenance refreshed", slog.String("provider", candidate.ProviderID), slog.String("candidate_id", candidate.ResultID), slog.Int("score", score.Total), slog.Bool("exact_hash", candidate.ExactHash), slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()))
	return existing, nil
}

func (s *Service) setReassessmentResult(result *Result, installation store.Installation, candidate domain.Candidate, score domain.Score, now time.Time) {
	result.Candidate = candidate
	result.Score = score
	result.Installation = installation
	result.NextUpgrade = s.nextUpgradeAt(now, score, candidate)
}

func (p LapsePolicy) normalized() LapsePolicy {
	if p.Mode == "" {
		return DefaultLapsePolicy()
	}
	if p.BypassScore == 0 {
		p.BypassScore = DefaultLapsePolicy().BypassScore
	}
	return p
}

func canBypassLapse(media domain.Media, candidate domain.Candidate, score domain.Score, installed bool, policy LapsePolicy) bool {
	if policy.Mode != "confidence" ||
		(installed && policy.LapseForUpgrades) ||
		(candidate.Pack != nil && policy.LapseForPacks) ||
		score.Total < policy.BypassScore {
		return false
	}
	identity, releaseGroup := false, false
	for _, contribution := range score.Contributions {
		switch contribution.Signal {
		case "external_id", "title_year":
			identity = identity || contribution.Points > 0
		case "release_group":
			releaseGroup = contribution.Points > 0
		}
	}
	if (policy.RequireIdentityAnchor && !identity) || (policy.RequireReleaseGroup && !releaseGroup) {
		return false
	}
	if !match.HasMatchingEdition(media, candidate) {
		return false
	}
	return !policy.RequireEpisodeEvidence || media.Ref.Kind != domain.MediaEpisode || candidateHasEpisodeEvidence(media, candidate)
}

func candidateHasEpisodeEvidence(media domain.Media, candidate domain.Candidate) bool {
	return match.HasEpisodeEvidence(media, candidate)
}

func (s *Service) install(ctx context.Context, request Request, prepared preparedCandidate, existing store.Installation, installed bool, result Result) (Result, error) {
	if prepared.memberIndex > 0 {
		ctx = observability.WithAttrs(ctx, slog.Int("member_index", prepared.memberIndex), slog.Int("member_count", prepared.memberCount))
	}
	destination := subtitleDestination(request.Media.Fingerprint.Path, request.Language, prepared.output)
	if installed {
		if !strings.EqualFold(filepath.Ext(existing.Path), filepath.Ext(prepared.output)) {
			result.Outcome = OutcomeRejected
			result.Decisions = append(result.Decisions, Decision{Stage: "installation", ProviderID: prepared.candidate.ProviderID, ResultID: prepared.candidate.ResultID, Reason: "format-changing upgrades are not supported"})
			return result, nil
		}
		destination = existing.Path
	}
	selectionMode := prepared.sync.Verdict
	if selectionMode != "exact_hash" && selectionMode != "score_bypass" {
		selectionMode = "lapse"
	}
	s.workflowEvents().Log(ctx, slog.LevelInfo, "candidate.selected", "subtitle candidate selected", slog.String("provider", prepared.candidate.ProviderID), slog.String("candidate_id", prepared.candidate.ResultID), slog.Int("score", prepared.score.Total), slog.Bool("exact_hash", prepared.candidate.ExactHash), slog.String("selection_mode", selectionMode))
	startedAt := time.Now()
	installation, err := s.Installer.Install(ctx, InstallRequest{MediaID: request.MediaID, Media: request.Media, Language: request.Language, Fallback: s.fallbackTier, SourcePath: prepared.output, DestinationPath: destination, Candidate: prepared.candidate, Score: prepared.score, SyncResult: prepared.sync})
	if err != nil {
		attrs := append([]slog.Attr{slog.String("provider", prepared.candidate.ProviderID), slog.String("candidate_id", prepared.candidate.ResultID), slog.Int64("duration_ms", time.Since(startedAt).Milliseconds())}, s.workflowEvents().ErrorAttrs("installation", err)...)
		s.workflowEvents().Log(ctx, slog.LevelError, "subtitle.install_failed", "subtitle installation failed", attrs...)
		return result, err
	}
	checksum := observability.SafeText(installation.Checksum)
	if len(checksum) > 12 {
		checksum = checksum[:12]
	}
	attrs := []slog.Attr{slog.String("provider", prepared.candidate.ProviderID), slog.String("candidate_id", prepared.candidate.ResultID), slog.Int("score", prepared.score.Total), slog.Bool("replaced", installed), slog.Bool("fallback", s.fallbackTier), slog.Int64("duration_ms", time.Since(startedAt).Milliseconds())}
	if checksum != "" {
		attrs = append(attrs, slog.String("checksum_prefix", checksum))
	}
	s.workflowEvents().Log(ctx, slog.LevelInfo, "subtitle.installed", "subtitle installed", attrs...)
	result.Outcome = OutcomeInstalled
	result.Candidate = prepared.candidate
	result.Score = prepared.score
	result.SyncResult = prepared.sync
	result.Installation = installation
	result.NextUpgrade = s.nextUpgradeAt(s.Clock.Now(), prepared.score, prepared.candidate)
	return result, nil
}

func inventoryStopsSearch(current inventory.Inventory, language domain.Language, allowHearingImpaired bool, installed bool, installation store.Installation) bool {
	for _, track := range current.Tracks {
		if track.Language == "" || !domain.EquivalentLanguage(track.Language, language) || track.Forced {
			continue
		}
		if track.SDH && !allowHearingImpaired {
			continue
		}
		if track.Embedded || track.Protected || !installed || filepath.Clean(track.Path) != filepath.Clean(installation.Path) || track.Checksum != installation.Checksum {
			return true
		}
	}
	return false
}

func managedInstallationPresent(current inventory.Inventory, installation store.Installation) bool {
	for _, track := range current.Tracks {
		if track.Embedded || track.Path == "" {
			continue
		}
		if filepath.Clean(track.Path) == filepath.Clean(installation.Path) && track.Checksum == installation.Checksum {
			return true
		}
	}
	return false
}

type boundedDownloadWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
	exceeded  bool
	writeErr  error
}

func (w *boundedDownloadWriter) Write(payload []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if int64(len(payload)) > w.remaining {
		w.exceeded = true
		if w.remaining == 0 {
			return 0, fmt.Errorf("provider download exceeds %d bytes", w.limit)
		}
		payload = payload[:w.remaining]
	}
	written, err := w.writer.Write(payload)
	w.remaining -= int64(written)
	if err == nil && written != len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.writeErr = err
		return written, err
	}
	if w.exceeded {
		return written, fmt.Errorf("provider download exceeds %d bytes", w.limit)
	}
	return written, nil
}

func candidateRecord(candidate domain.Candidate, score domain.Score, eligible bool) (store.CandidateRecord, error) {
	safe := candidate
	safe.ResultID = credentialFreeResultID(safe.ResultID)
	safe.DownloadRef = ""
	if safe.Pack != nil {
		packInfo := *safe.Pack
		packInfo.DirectMembers = append([]domain.PackMemberRef(nil), safe.Pack.DirectMembers...)
		for index := range packInfo.DirectMembers {
			packInfo.DirectMembers[index].DownloadRef = ""
		}
		slices.SortFunc(packInfo.DirectMembers, func(a, b domain.PackMemberRef) int {
			left, _ := json.Marshal(a)
			right, _ := json.Marshal(b)
			return strings.Compare(string(left), string(right))
		})
		packInfo.DirectMembers = slices.Compact(packInfo.DirectMembers)
		safe.Pack = &packInfo
	}
	metadata, err := json.Marshal(safe)
	if err != nil {
		return store.CandidateRecord{}, err
	}
	scoreJSON, err := json.Marshal(score)
	if err != nil {
		return store.CandidateRecord{}, err
	}
	validation, _ := json.Marshal(map[string]bool{"eligible": eligible})
	return store.CandidateRecord{ProviderID: candidate.ProviderID, ResultID: safe.ResultID, MetadataJSON: metadata, ScoreJSON: scoreJSON, ValidationJSON: validation}, nil
}

func credentialFreeResultID(value string) string {
	value, _, _ = strings.Cut(value, "?")
	value, _, _ = strings.Cut(value, "#")
	return value
}

func sortPrepared(candidates []preparedCandidate) {
	sort.SliceStable(candidates, func(left, right int) bool {
		a, b := candidates[left], candidates[right]
		if a.sync.Confidence != b.sync.Confidence {
			return a.sync.Confidence > b.sync.Confidence
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if a.candidate.Rating != b.candidate.Rating {
			return a.candidate.Rating > b.candidate.Rating
		}
		if a.candidate.ProviderID != b.candidate.ProviderID {
			return a.candidate.ProviderID < b.candidate.ProviderID
		}
		return a.candidate.ResultID < b.candidate.ResultID
	})
}

func (s *Service) priorities() map[string]int {
	priorities := make(map[string]int, len(s.ProviderOrder))
	for index, id := range s.ProviderOrder {
		priorities[id] = index
	}
	return priorities
}

func (s *Service) evaluate(media domain.Media, candidate domain.Candidate, language domain.Language) domain.Score {
	score := match.Evaluate(media, candidate, language)
	if candidate.HearingImpaired && !s.AllowHearingImpaired {
		score.RejectedReasons = append(score.RejectedReasons, "hearing-impaired candidate is disabled by policy")
	}
	return score
}

func (s *Service) minimumScore() int {
	if s.MinimumScore <= 0 {
		return 35
	}
	return s.MinimumScore
}

func (s *Service) packTTL() time.Duration {
	if s.PackTTL <= 0 {
		return 24 * time.Hour
	}
	return s.PackTTL
}

func subtitleDestination(mediaPath string, language domain.Language, sourcePath string) string {
	stem := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath))
	return stem + "." + language.String() + strings.ToLower(filepath.Ext(sourcePath))
}

func allProvidersUnavailable(failures map[string]error, expected int) (time.Time, bool) {
	if len(failures) == 0 || expected > 0 && len(failures) < expected {
		return time.Time{}, false
	}
	values := make([]error, 0, len(failures))
	for _, failure := range failures {
		values = append(values, failure)
	}
	return unavailableErrors(values)
}

func unavailableErrors(failures []error) (time.Time, bool) {
	if len(failures) == 0 {
		return time.Time{}, false
	}
	var earliest time.Time
	for _, failure := range failures {
		var cooldown *provider.CooldownError
		var quota *provider.QuotaError
		var disabled *provider.DisabledError
		var reset time.Time
		switch {
		case errors.As(failure, &cooldown):
			reset = cooldown.ResetAt
		case errors.As(failure, &quota):
			reset = quota.ResetAt
		case errors.As(failure, &disabled):
		default:
			return time.Time{}, false
		}
		if !reset.IsZero() && (earliest.IsZero() || reset.Before(earliest)) {
			earliest = reset
		}
	}
	return earliest, true
}

func classifyCandidateFailures(result Result, failures []error) (Result, error, bool) {
	if len(failures) == 0 {
		return result, nil, false
	}
	if retry, unavailable := unavailableErrors(failures); unavailable {
		result.Outcome = OutcomeThrottled
		result.RetryAt = retry
		return result, nil, true
	}
	return result, &acquisitionExhaustedError{errors.Join(failures...)}, true
}

// removeWorkflowArtifact releases only a file directly owned by this private
// workflow. Cached pack source paths are deliberately outside this namespace.
func removeWorkflowArtifact(workspace, path string) error {
	if filepath.Dir(path) != filepath.Clean(workspace) {
		return nil
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "selected-") && !strings.HasPrefix(base, "synchronized-") {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove candidate scratch: %w", err)
	}
	return nil
}
