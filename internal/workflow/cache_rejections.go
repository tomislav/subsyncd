package workflow

import (
	"context"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
)

// Keep ordinary cache failures best-effort, but never hide a failed rejection
// lookup behind provider fallback.
func (s *Service) findUnrejectedPack(ctx context.Context, request Request, result *Result) (member pack.CachedMember, found bool, cacheErr, rejectionErr error) {
	filtered, ok := s.PackCache.(interface {
		FindEligible(context.Context, domain.Media, domain.Language, func(pack.CachedMember) (bool, error)) (pack.CachedMember, bool, error)
	})
	if !ok {
		member, found, cacheErr = s.PackCache.Find(ctx, request.Media, request.Language)
		return
	}
	member, found, cacheErr = filtered.FindEligible(ctx, request.Media, request.Language, func(candidate pack.CachedMember) (bool, error) {
		rejection, rejected, err := s.candidateRejection(ctx, request, candidate.Candidate, candidate.Checksum)
		if err != nil {
			rejectionErr = err
			return false, err
		}
		if rejected {
			result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: candidate.Candidate.ProviderID, ResultID: candidate.Candidate.ResultID, Reason: rejection.ReasonCode})
		}
		return !rejected, nil
	})
	return
}
