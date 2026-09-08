package workflow

import (
	"context"
	"errors"
	"slices"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

// Only exhausted acquisition failures permit trying another provider tier.
// Repository, inventory and publication failures return their original errors.
type acquisitionExhaustedError struct{ error }

func (e *acquisitionExhaustedError) Unwrap() error { return e.error }

// This request-local wrapper preserves scored evidence from every attempted tier.
// A write failure never changes the accumulated snapshot.
type tierRepository struct {
	WorkflowRepository
	records []store.CandidateRecord
	count   *int
}

func (r *tierRepository) RecordCandidates(ctx context.Context, mediaID int64, language domain.Language, records []store.CandidateRecord) error {
	merged := mergeCandidateRecords(r.records, records)
	if err := r.WorkflowRepository.RecordCandidates(ctx, mediaID, language, merged); err != nil {
		return err
	}
	r.records = merged
	*r.count = len(merged)
	return nil
}

func (s *Service) runProviderTiers(ctx context.Context, request Request, existing store.Installation, installed bool, count *int) (Result, error) {
	preferred := *s
	preferred.preferredProviderOrder = s.ProviderOrder
	repository := &tierRepository{WorkflowRepository: s.Repository, count: count}
	preferred.Repository = repository
	first, firstErr := preferred.acquire(ctx, request, existing, installed, count)
	defer func() { *count = len(repository.records) }()
	if ctx.Err() != nil {
		return first, errors.Join(firstErr, ctx.Err())
	}
	var exhausted *acquisitionExhaustedError
	if firstErr != nil && !errors.As(firstErr, &exhausted) {
		return first, firstErr
	}
	if firstErr == nil && (first.Outcome == OutcomeInstalled || first.Outcome == OutcomeSatisfied) {
		return first, nil
	}
	if s.FallbackSearcher == nil || len(s.FallbackProviderOrder) == 0 {
		return first, firstErr
	}
	// An existing preferred subtitle never downgrades to fallback.
	if installed && InstallationMatchesMedia(existing, request.Media) && !s.isFallbackProvider(existing.ProviderID) {
		return first, firstErr
	}
	if installed && InstallationMatchesMedia(existing, request.Media) {
		_, exact, err := installedScore(existing)
		if err != nil {
			return first, err
		}
		// The fallback exact subtitle is already the strongest same-tier match.
		if exact {
			return s.retainFallback(first, firstErr, existing)
		}
	}
	fallback := preferred
	fallback.Searcher = s.FallbackSearcher
	fallback.ProviderOrder = s.FallbackProviderOrder
	fallback.fallbackTier = true
	second, secondErr := fallback.acquire(ctx, request, existing, installed, count)
	second.Decisions = append(first.Decisions, second.Decisions...)
	for id, err := range first.ProviderErrors {
		second.ProviderErrors[id] = err
	}
	if ctx.Err() != nil {
		return second, errors.Join(secondErr, ctx.Err())
	}
	if secondErr != nil && !errors.As(secondErr, &exhausted) {
		return second, secondErr
	}
	if secondErr == nil && second.Outcome == OutcomeInstalled {
		// Retry a temporarily unavailable preferred tier promptly after its reset.
		second.NextUpgrade = preferredRetryAt(first, s.Clock.Now(), second.NextUpgrade)
		return second, nil
	}
	if firstErr != nil || secondErr != nil {
		return second, errors.Join(firstErr, secondErr)
	}
	if first.Outcome == OutcomeThrottled {
		second.Outcome = OutcomeThrottled
		if second.RetryAt.IsZero() || first.RetryAt.Before(second.RetryAt) {
			second.RetryAt = first.RetryAt
		}
	}
	if installed && InstallationMatchesMedia(existing, request.Media) {
		return s.retainFallback(second, nil, existing)
	}
	if second.Outcome == OutcomeNoResult && first.Outcome == OutcomeRejected {
		second.Outcome = OutcomeRejected
	}
	return second, nil
}

func (s *Service) retainFallback(result Result, err error, existing store.Installation) (Result, error) {
	if err != nil || result.Outcome == OutcomeThrottled {
		return result, err
	}
	// A same-candidate search may have refreshed provenance without replacing bytes.
	if result.Installation.MediaID != 0 {
		existing = result.Installation
	}
	score, exact, err := installedScore(existing)
	if err != nil {
		return result, err
	}
	result.Outcome = OutcomeSatisfied
	result.Installation = existing
	result.Score = score
	result.Candidate = domain.Candidate{ProviderID: existing.ProviderID, ResultID: existing.CandidateID, ExactHash: exact}
	result.NextUpgrade = s.Clock.Now().Add(7 * 24 * time.Hour)
	return result, nil
}

func (s *Service) isFallbackProvider(id string) bool {
	return slices.Contains(s.FallbackProviderOrder, id)
}

func (s *Service) nextUpgradeAt(now time.Time, score domain.Score, candidate domain.Candidate) time.Time {
	if s.isFallbackProvider(candidate.ProviderID) {
		return now.Add(7 * 24 * time.Hour)
	}
	return NextUpgradeAt(now, score, candidate.ExactHash)
}

func (s *Service) shouldUpgrade(existing store.Installation, media domain.Media, candidate domain.Candidate, score domain.Score) (bool, error) {
	if !InstallationMatchesMedia(existing, media) {
		return true, nil
	}
	preferred := s.preferredProviderOrder
	if preferred == nil {
		preferred = s.ProviderOrder
	}
	if s.isFallbackProvider(existing.ProviderID) && slices.Contains(preferred, candidate.ProviderID) {
		return true, nil
	}
	if slices.Contains(preferred, existing.ProviderID) && s.isFallbackProvider(candidate.ProviderID) {
		return false, nil
	}
	return ShouldUpgrade(existing, score, candidate.ExactHash, s.MinimumUpgradeDelta)
}

func (s *Service) cacheProviderAllowed(id string) bool {
	// Existing single-tier configurations retain their historical cache behavior.
	if len(s.FallbackProviderOrder) == 0 {
		return true
	}
	return slices.Contains(s.ProviderOrder, id)
}

// A partial outage still deserves a prompt promotion check after a fallback install.
func preferredRetryAt(result Result, now, next time.Time) time.Time {
	consider := func(reset time.Time) {
		if reset.After(now) && reset.Before(next) {
			next = reset
		}
	}
	consider(result.RetryAt)
	for _, err := range result.ProviderErrors {
		if reset, unavailable := unavailableErrors([]error{err}); unavailable {
			consider(reset)
		}
	}
	return next
}

func searchPersistenceFailure(failures map[string]error) error {
	var found []error
	for _, err := range failures {
		var persistence *provider.SearchPersistenceError
		if errors.As(err, &persistence) {
			found = append(found, err)
		}
	}
	return errors.Join(found...)
}
