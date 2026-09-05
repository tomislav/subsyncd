package workflow

import (
	"context"

	"subsyncd/internal/match"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func (s *Service) tryExactCandidates(
	ctx context.Context,
	request Request,
	workspace string,
	existing store.Installation,
	installed bool,
	search provider.SearchResult,
	result *Result,
	candidateFailures *[]error,
	exactRecords *[]store.CandidateRecord,
) (bool, error) {
	priorities := s.priorities()
	records := make([]store.CandidateRecord, 0, len(search.Candidates))
	for _, candidate := range search.Candidates {
		if !candidate.ExactHash {
			continue
		}
		score := s.evaluate(request.Media, candidate, request.Language)
		s.logCandidateEvaluation(ctx, request, candidate, score)
		record, err := candidateRecord(candidate, score, match.Eligible(score, s.minimumScore()))
		if err != nil {
			return false, err
		}
		records = append(records, record)
	}
	*exactRecords = mergeCandidateRecords(*exactRecords, records)
	if len(*exactRecords) != 0 {
		if err := s.Repository.RecordCandidates(ctx, request.MediaID, request.Language, *exactRecords); err != nil {
			return false, err
		}
	}

	for index, candidate := range search.Candidates {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !candidate.ExactHash {
			continue
		}
		score := s.evaluate(request.Media, candidate, request.Language)
		if !match.Eligible(score, s.minimumScore()) {
			continue
		}
		if installed && sameInstalledCandidate(existing, request.Media, candidate) {
			updated, err := s.updateInstallationAssessment(ctx, existing, candidate, score)
			if err != nil {
				return false, err
			}
			setReassessmentResult(result, updated, candidate, score, s.Clock.Now())
			result.Outcome = OutcomeSatisfied
			result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: "refreshed assessment for installed provider candidate"})
			return true, nil
		}
		rejection, rejected, err := s.candidateRejection(ctx, request, candidate, "")
		if err != nil {
			return false, err
		}
		if rejected {
			result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: rejection.ReasonCode})
			continue
		}
		path, decisions, err := s.downloadAndSelect(ctx, request, candidate, workspace, -index-1)
		result.Decisions = append(result.Decisions, decisions...)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			recorded, rejectionErr := s.recordCandidateRejection(ctx, request, candidate, "", err)
			if rejectionErr != nil {
				return false, rejectionErr
			}
			if !recorded && !isMediaValidationRejection(err) {
				*candidateFailures = append(*candidateFailures, err)
			}
			result.Decisions = append(result.Decisions, candidateFailureDecision("candidate", candidate, err))
			continue
		}
		priority, found := priorities[candidate.ProviderID]
		if !found {
			priority = len(s.ProviderOrder)
		}
		prepared, err := s.prepareCandidate(ctx, request, downloadedCandidate{candidate: candidate, score: score, priority: priority, path: path}, installed, existing, workspace, -index-2)
		if err != nil {
			if handleErr := s.handleCandidateFailure(ctx, request, candidate, path, err, candidateFailures); handleErr != nil {
				return false, handleErr
			}
			if cleanupErr := removeWorkflowArtifact(workspace, path); cleanupErr != nil {
				return false, cleanupErr
			}
			continue
		}
		*result, err = s.install(ctx, request, prepared, existing, installed, *result)
		if err != nil {
			// Accept only the installer's direct pre-publication validation
			// error. Wrapped/joined infrastructure or rollback errors remain
			// intact and terminal, including when cancellation caused them.
			if _, rejected := err.(*subtitleValidationError); !rejected {
				return false, err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			if handleErr := s.handleCandidateFailure(ctx, request, candidate, prepared.path, err, candidateFailures); handleErr != nil {
				return false, handleErr
			}
			if cleanupErr := removeWorkflowArtifact(workspace, path); cleanupErr != nil {
				return false, cleanupErr
			}
			result.Decisions = append(result.Decisions, candidateFailureDecision("installation", candidate, err))
			continue
		}
		if result.Outcome == OutcomeInstalled || result.Outcome == OutcomeSatisfied {
			return true, nil
		}
		if cleanupErr := removeWorkflowArtifact(workspace, path); cleanupErr != nil {
			return false, cleanupErr
		}
		result.Outcome = ""
	}
	return false, nil
}

func mergeCandidateRecords(existing, additional []store.CandidateRecord) []store.CandidateRecord {
	type identity struct {
		provider string
		result   string
	}
	merged := make([]store.CandidateRecord, 0, len(existing)+len(additional))
	seen := make(map[identity]struct{}, len(existing)+len(additional))
	for _, records := range [][]store.CandidateRecord{existing, additional} {
		for _, record := range records {
			key := identity{provider: record.ProviderID, result: record.ResultID}
			if _, found := seen[key]; found {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, record)
		}
	}
	return merged
}
