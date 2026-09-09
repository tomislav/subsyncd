package workflow

import (
	"context"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

// Keep ordinary cache failures best-effort, but never hide a failed rejection
// lookup behind provider fallback.
func (s *Service) findUnrejectedPack(ctx context.Context, request Request, result *Result) (member pack.CachedMember, found bool, cacheErr, rejectionErr error) {
	filtered, ok := s.PackCache.(interface {
		FindEligible(context.Context, domain.Media, domain.Language, func(pack.CachedMember) (bool, error)) (pack.CachedMember, bool, error)
	})
	if !ok {
		member, found, cacheErr = s.PackCache.Find(ctx, request.Media, request.Language)
		found = found && s.cacheCandidateAllowed(member.Candidate)
		return
	}
	member, found, cacheErr = filtered.FindEligible(ctx, request.Media, request.Language, func(candidate pack.CachedMember) (bool, error) {
		if !s.cacheCandidateAllowed(candidate.Candidate) {
			return false, nil
		}
		scoped := request
		if candidate.MemberScoped {
			scoped = memberRequest(request, candidate.Checksum)
		}
		rejection, rejected, err := s.candidateRejection(ctx, scoped, candidate.Candidate, candidate.Checksum)
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

func (s *Service) cacheCandidateAllowed(candidate domain.Candidate) bool {
	return s.cacheProviderAllowed(candidate.ProviderID) && provider.CanReuseCachedCandidate(s.Providers[candidate.ProviderID], candidate)
}

// Cache lookup has already rerun strict episode selection and verified the
// immutable member checksum. Keep that evidence separate from provider identity.
func (s *Service) evaluateCachedMember(media domain.Media, member pack.CachedMember, language domain.Language) domain.Score {
	if !member.RuntimePack {
		return s.evaluate(media, member.Candidate, language)
	}
	score := match.EvaluateSelectedPackMember(media, member.Candidate, language)
	if member.Candidate.HearingImpaired && !s.AllowHearingImpaired {
		score.RejectedReasons = append(score.RejectedReasons, "hearing-impaired candidate is disabled by policy")
	}
	return score
}
