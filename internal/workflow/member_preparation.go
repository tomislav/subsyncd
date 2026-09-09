package workflow

import (
	"context"
	"log/slog"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/store"
)

func memberRequest(request Request, scope string) Request {
	request.memberScope = scope
	return request
}

// prepareMembers retains validated outputs for the enclosing score tier. Paths
// may be immutable cache sources; cleanup removes only private workspace files.
func (s *Service) prepareMembers(ctx context.Context, request Request, item downloadedCandidate, paths []string, installed bool, existing store.Installation, workspace string, index int, failures *[]error, result *Result) ([]preparedCandidate, error) {
	var ready []preparedCandidate
	allRejected := true
	groupMayReject := true
	variants := len(paths) > 1 || item.memberScope != ""
	for n, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := checkMediaAvailable(request.Media); err != nil {
			return nil, err
		}
		current := item
		current.path = path
		scoped := request
		memberCtx := ctx
		if variants {
			checksum, err := fileChecksum(path)
			if err != nil {
				return nil, err
			}
			current.memberScope = checksum
			current.memberIndex = n + 1
			current.memberCount = len(paths)
			scoped = memberRequest(request, checksum)
			memberCtx = observability.WithAttrs(ctx, slog.Int("member_index", n+1), slog.Int("member_count", len(paths)))
			rejection, rejected, err := s.candidateRejection(memberCtx, scoped, item.candidate, checksum)
			if err != nil {
				return nil, err
			}
			if rejected {
				result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: item.candidate.ProviderID, ResultID: item.candidate.ResultID, Reason: rejection.ReasonCode})
				if cleanupErr := removeWorkflowArtifact(workspace, path); cleanupErr != nil {
					return nil, cleanupErr
				}
				continue
			}
			current.runtimePack = true
		}
		prepared, err := s.prepareCandidate(memberCtx, scoped, current, installed, existing, workspace, index+n)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			if mediaErr := checkMediaAvailable(request.Media); mediaErr != nil {
				return nil, mediaErr
			}
			if item.fromCache {
				result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", ProviderID: item.candidate.ProviderID, ResultID: item.candidate.ResultID, Reason: lapseFailureDecision(err)})
			}
			result.Decisions = append(result.Decisions, Decision{Stage: "lapse_prepare", ProviderID: item.candidate.ProviderID, ResultID: item.candidate.ResultID, Reason: lapseFailureDecision(err)})
			if handleErr := s.handleCandidateFailure(memberCtx, scoped, item.candidate, path, err, failures); handleErr != nil {
				return nil, handleErr
			}
			checksum, checksumErr := fileChecksum(path)
			if checksumErr != nil {
				return nil, checksumErr
			}
			_, rejected, lookupErr := s.candidateRejection(memberCtx, scoped, item.candidate, checksum)
			if lookupErr != nil {
				return nil, lookupErr
			}
			allRejected = allRejected && rejected
			groupMayReject = groupMayReject && rejected
			if cleanupErr := removeWorkflowArtifact(workspace, path); cleanupErr != nil {
				return nil, cleanupErr
			}
			continue
		}
		allRejected = false
		if !prepared.bypass {
			result.Decisions = append(result.Decisions, Decision{Stage: "lapse_prepare", ProviderID: item.candidate.ProviderID, ResultID: item.candidate.ResultID, Reason: "solid"})
		}
		ready = append(ready, prepared)
	}
	if variants && allRejected {
		_, err := s.recordCandidateRejection(ctx, request, item.candidate, "", &pack.SelectionError{Rule: "versions_exhausted", Reason: "all identified episode versions were rejected", MatchingMemberCount: len(paths)})
		if err != nil {
			return nil, err
		}
	}
	for n := range ready {
		ready[n].groupMayReject = groupMayReject
	}
	return ready, nil
}

func (s *Service) logCandidateRejection(ctx context.Context, d Decision) {
	attrs := []slog.Attr{slog.String("provider", observability.SafeText(d.ProviderID)), slog.String("candidate_id", observability.SafeText(d.ResultID)), slog.String("reason_code", d.ReasonCode)}
	if d.SelectionRule != "" {
		attrs = append(attrs, slog.String("selection_rule", d.SelectionRule), slog.String("archive_type", d.ArchiveType), slog.Int("subtitle_member_count", d.SubtitleMemberCount), slog.Int("matching_member_count", d.MatchingMemberCount))
	}
	s.workflowEvents().Log(ctx, slog.LevelInfo, "candidate.rejected", "subtitle candidate rejected", attrs...)
}

// Only aggregate after every prepared version was deterministically rejected.
// A technical preparation failure keeps the provider candidate retryable.
func (s *Service) rejectExhaustedVersions(ctx context.Context, request Request, items []preparedCandidate) error {
	groups := map[[2]string][]preparedCandidate{}
	for _, item := range items {
		if item.memberScope != "" {
			key := [2]string{item.candidate.ProviderID, item.candidate.ResultID}
			groups[key] = append(groups[key], item)
		}
	}
	for _, group := range groups {
		rejected := true
		for _, item := range group {
			if !item.groupMayReject {
				rejected = false
				break
			}
			_, found, err := s.candidateRejection(ctx, memberRequest(request, item.memberScope), item.candidate, item.memberScope)
			if err != nil {
				return err
			}
			if !found {
				rejected = false
				break
			}
		}
		if rejected {
			_, err := s.recordCandidateRejection(ctx, request, group[0].candidate, "", &pack.SelectionError{Rule: "versions_exhausted", Reason: "all identified episode versions were rejected"})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
