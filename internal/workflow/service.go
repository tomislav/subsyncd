package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/match"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

type Outcome string

const defaultCandidateRejectionTTL = 30 * 24 * time.Hour

const (
	OutcomeSatisfied Outcome = "satisfied"
	OutcomeInstalled Outcome = "installed"
	OutcomeNoResult  Outcome = "no_result"
	OutcomeRejected  Outcome = "rejected"
	OutcomeThrottled Outcome = "throttled"
)

type Request struct {
	MediaID    int64
	Media      domain.Media
	Language   domain.Language
	Manual     bool
	ForceProbe bool
}

type Decision struct {
	Stage      string
	ProviderID string
	ResultID   string
	Reason     string
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
	AnalyzeCandidate(context.Context, domain.Candidate, string, string) (domain.SyncResult, error)
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
	Inventory            InventoryRefresher
	Searcher             Searcher
	PackCache            PackCache
	Synchronizer         CandidateSynchronizer
	Installer            CandidateInstaller
	Repository           WorkflowRepository
	Providers            map[string]provider.Provider
	ProviderOrder        []string
	MinimumScore         int
	MinimumUpgradeDelta  int
	PackTTL              time.Duration
	LapsePolicy          LapsePolicy
	AllowHearingImpaired bool
	Clock                WorkflowClock
}

type downloadedCandidate struct {
	candidate domain.Candidate
	score     domain.Score
	priority  int
	path      string
}

type analyzedCandidate struct {
	downloadedCandidate
	analysis domain.SyncResult
	bypass   bool
}

type finalizedCandidate struct {
	candidate domain.Candidate
	score     domain.Score
	priority  int
	sync      domain.SyncResult
	path      string
}

func (s *Service) Run(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := s.validate(request); err != nil {
		return Result{}, err
	}
	result := Result{ProviderErrors: map[string]error{}}
	var candidateFailures []error
	sameCandidateAssessed := false
	current, err := s.Inventory.Refresh(ctx, request.MediaID, request.Media, request.ForceProbe)
	if err != nil {
		return result, fmt.Errorf("refresh subtitle inventory: %w", err)
	}
	if current.Fingerprint.Path != "" {
		request.Media.Fingerprint = current.Fingerprint
	}
	existing, installed, err := s.Repository.GetInstallation(ctx, request.MediaID, request.Language)
	if err != nil {
		return result, err
	}
	if inventoryStopsSearch(current, request.Language, s.AllowHearingImpaired, installed, existing) {
		result.Outcome = OutcomeSatisfied
		return result, nil
	}
	if installed && InstallationMatchesMedia(existing, request.Media) {
		_, exact, scoreErr := installedScore(existing)
		if scoreErr != nil {
			return result, scoreErr
		}
		if exact {
			result.Outcome = OutcomeSatisfied
			return result, nil
		}
	}

	workspace, err := os.MkdirTemp(filepath.Dir(request.Media.Fingerprint.Path), ".subsyncd-work-")
	if err != nil {
		return result, fmt.Errorf("create candidate workspace: %w", err)
	}
	defer os.RemoveAll(workspace)

	if request.Media.Ref.Kind == domain.MediaEpisode && s.PackCache != nil {
		cached, found, cacheErr := s.PackCache.Find(ctx, request.Media, request.Language)
		if cacheErr != nil {
			result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", Reason: cacheErr.Error()})
		} else if found {
			rejection, rejected, rejectionErr := s.candidateRejection(ctx, request, cached.Candidate, cached.Checksum)
			if rejectionErr != nil {
				return result, rejectionErr
			}
			if rejected {
				result.Decisions = append(result.Decisions, Decision{Stage: "candidate_rejection", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: rejection.ReasonCode})
			} else {
				score := s.evaluate(request.Media, cached.Candidate, request.Language)
				if !match.Eligible(score, s.minimumScore()) {
					result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: "cached candidate score is below threshold or identity was rejected"})
				} else if installed && sameInstalledCandidate(existing, request.Media, cached.Candidate) {
					existing, err = s.updateInstallationAssessment(ctx, existing, cached.Candidate, score)
					if err != nil {
						return result, err
					}
					sameCandidateAssessed = true
					setReassessmentResult(&result, existing, cached.Candidate, score, s.Clock.Now())
					result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: "refreshed assessment for installed provider candidate"})
				} else {
					downloaded := downloadedCandidate{candidate: cached.Candidate, score: score, path: cached.Path}
					analyzed, prepareErr := s.analyzeCandidate(ctx, request, downloaded, installed, existing)
					if prepareErr == nil {
						var finalized finalizedCandidate
						finalized, prepareErr = s.finalizeCandidate(ctx, request, analyzed, workspace, 0)
						if prepareErr == nil {
							return s.install(ctx, request, finalized, existing, installed, result)
						}
					}
					recorded, recordErr := s.recordCandidateRejection(ctx, request, cached.Candidate, cached.Checksum, prepareErr)
					if recordErr != nil {
						return result, recordErr
					}
					if !recorded && !isMediaValidationRejection(prepareErr) {
						candidateFailures = append(candidateFailures, prepareErr)
					}
					result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: prepareErr.Error()})
				}
			}
		}
	}

	search := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language})
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.ProviderErrors = search.Errors
	if len(search.Candidates) == 0 {
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
			return result, fmt.Errorf("all %d assigned subtitle providers failed", len(s.ProviderOrder))
		}
		if len(candidateFailures) != 0 {
			return result, errors.Join(candidateFailures...)
		}
		result.Outcome = OutcomeNoResult
		return result, nil
	}

	evaluated := make([]match.EvaluatedCandidate, 0, len(search.Candidates))
	records := make([]store.CandidateRecord, 0, len(search.Candidates))
	priorities := s.priorities()
	for _, candidate := range search.Candidates {
		score := s.evaluate(request.Media, candidate, request.Language)
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
	if err := s.Repository.RecordCandidates(ctx, request.MediaID, request.Language, records); err != nil {
		return result, err
	}
	match.Rank(evaluated)
	if installed && InstallationMatchesMedia(existing, request.Media) {
		for _, item := range evaluated {
			if !match.Eligible(item.Score, s.minimumScore()) || !sameInstalledCandidate(existing, request.Media, item.Candidate) {
				continue
			}
			existing, err = s.updateInstallationAssessment(ctx, existing, item.Candidate, item.Score)
			if err != nil {
				return result, err
			}
			sameCandidateAssessed = true
			setReassessmentResult(&result, existing, item.Candidate, item.Score, s.Clock.Now())
			result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: "refreshed assessment for installed provider candidate"})
			break
		}
	}
	eligible := evaluated[:0]
	for _, item := range evaluated {
		if !match.Eligible(item.Score, s.minimumScore()) {
			continue
		}
		if installed {
			if sameInstalledCandidate(existing, request.Media, item.Candidate) {
				result.Decisions = append(result.Decisions, Decision{Stage: "upgrade", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: "provider candidate is already installed"})
				continue
			}
			allowed, upgradeErr := ShouldUpgradeForMedia(existing, request.Media, item.Score, item.Candidate.ExactHash, s.MinimumUpgradeDelta)
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
		finalized := make([]finalizedCandidate, 0, tierEnd-tierStart)
		for index := tierStart; index < tierEnd; index++ {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			item := eligible[index]
			path, decisions, prepareErr := s.downloadAndSelect(ctx, request, item.Candidate, workspace, index)
			result.Decisions = append(result.Decisions, decisions...)
			if prepareErr != nil {
				recorded, rejectionErr := s.recordCandidateRejection(ctx, request, item.Candidate, "", prepareErr)
				if rejectionErr != nil {
					return result, rejectionErr
				}
				if !recorded && !isMediaValidationRejection(prepareErr) {
					candidateFailures = append(candidateFailures, prepareErr)
				}
				result.Decisions = append(result.Decisions, Decision{Stage: "candidate", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: prepareErr.Error()})
				continue
			}
			downloaded := downloadedCandidate{candidate: item.Candidate, score: item.Score, priority: item.ProviderPriority, path: path}
			analyzed, syncErr := s.analyzeCandidate(ctx, request, downloaded, installed, existing)
			if syncErr != nil {
				if err := s.handleCandidateFailure(ctx, request, item.Candidate, path, syncErr, &candidateFailures); err != nil {
					return result, err
				}
				result.Decisions = append(result.Decisions, Decision{Stage: "synchronization", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: syncErr.Error()})
				continue
			}
			ready, syncErr := s.finalizeCandidate(ctx, request, analyzed, workspace, index)
			if syncErr != nil {
				if err := s.handleCandidateFailure(ctx, request, item.Candidate, path, syncErr, &candidateFailures); err != nil {
					return result, err
				}
				result.Decisions = append(result.Decisions, Decision{Stage: "synchronization", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: syncErr.Error()})
				continue
			}
			finalized = append(finalized, ready)
		}
		if len(finalized) != 0 {
			sortFinalized(finalized)
			return s.install(ctx, request, finalized[0], existing, installed, result)
		}
		tierStart = tierEnd
	}
	if len(candidateFailures) == 0 {
		result.Outcome = OutcomeRejected
		return result, nil
	}
	if retry, unavailable := unavailableErrors(candidateFailures); unavailable {
		result.Outcome = OutcomeThrottled
		result.RetryAt = retry
		return result, nil
	}
	return result, errors.Join(candidateFailures...)
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
	signature, err := candidateSignature(candidate)
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
	reasonCode := ""
	switch {
	case errors.As(failure, &verdict) && (verdict.Verdict == "unsure" || verdict.Verdict == "nothing"):
		reasonCode = "lapse_" + verdict.Verdict
	case errors.As(failure, &selection):
		reasonCode = "pack_selection"
	case errors.As(failure, &content):
		reasonCode = "invalid_subtitle"
	default:
		return false, nil
	}
	signature, err := candidateSignature(candidate)
	if err != nil {
		return false, err
	}
	now := s.Clock.Now()
	fingerprint := request.Media.Fingerprint
	rejection := store.CandidateRejection{
		MediaID: request.MediaID, Language: request.Language.String(), ProviderID: candidate.ProviderID, ResultID: candidate.ResultID,
		CandidateSignature: signature, ArtifactChecksum: artifactChecksum, ReasonCode: reasonCode, ToolSignature: s.rejectionToolSignature(),
		MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano(), RejectedAt: now, ExpiresAt: now.Add(defaultCandidateRejectionTTL),
	}
	if err := s.Repository.PutCandidateRejection(ctx, rejection); err != nil {
		return false, err
	}
	return true, nil
}

func isMediaValidationRejection(failure error) bool {
	var noSpeech *syncer.NoSpeechError
	return errors.As(failure, &noSpeech)
}

func candidateSignature(candidate domain.Candidate) (string, error) {
	safe := candidate
	safe.DownloadRef = ""
	safe.Rating = 0
	safe.Popularity = 0
	safe.DownloadCount = 0
	if safe.Pack != nil {
		packInfo := *safe.Pack
		packInfo.DirectMembers = append([]domain.PackMemberRef(nil), safe.Pack.DirectMembers...)
		for index := range packInfo.DirectMembers {
			packInfo.DirectMembers[index].DownloadRef = ""
		}
		safe.Pack = &packInfo
	}
	payload, err := json.Marshal(safe)
	if err != nil {
		return "", fmt.Errorf("encode candidate signature: %w", err)
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

func (s *Service) downloadAndSelect(ctx context.Context, request Request, candidate domain.Candidate, workspace string, index int) (string, []Decision, error) {
	adapter := s.Providers[candidate.ProviderID]
	if adapter == nil {
		return "", nil, fmt.Errorf("provider %q is not available for download", candidate.ProviderID)
	}
	payloadPath := filepath.Join(workspace, fmt.Sprintf("download-%d", index))
	payload, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	limits := pack.DefaultLimits()
	bounded := &boundedDownloadWriter{writer: payload, remaining: limits.MaxCompressed, limit: limits.MaxCompressed}
	metadata, downloadErr := adapter.Download(ctx, candidate, bounded)
	syncErr := payload.Sync()
	closeErr := payload.Close()
	if bounded.exceeded {
		return "", nil, &pack.ContentError{Err: fmt.Errorf("provider download exceeds %d bytes", bounded.limit)}
	}
	if downloadErr != nil {
		return "", nil, downloadErr
	}
	if syncErr != nil || closeErr != nil {
		return "", nil, fmt.Errorf("flush provider download")
	}
	info, err := os.Stat(payloadPath)
	if err != nil {
		return "", nil, err
	}
	reader, err := os.Open(payloadPath)
	if err != nil {
		return "", nil, err
	}
	defer reader.Close()
	extractionCandidate := candidate
	if metadata.Filename != "" {
		extractionCandidate.DownloadRef = metadata.Filename
	}
	manifest, err := pack.Extract(ctx, extractionCandidate, reader, info.Size(), filepath.Join(workspace, fmt.Sprintf("extracted-%d", index)), pack.DefaultLimits())
	if err != nil {
		return "", nil, err
	}
	var member pack.Member
	if candidate.Pack == nil && len(manifest.Members) == 1 {
		member = manifest.Members[0]
	} else if request.Media.Ref.Kind == domain.MediaEpisode {
		member, err = pack.Select(manifest, extractionCandidate, request.Media, false)
		if err != nil {
			return "", nil, err
		}
	} else {
		return "", nil, fmt.Errorf("movie candidate archive contains multiple subtitle files")
	}
	var decisions []Decision
	if candidate.Pack != nil && s.PackCache != nil {
		if err := s.PackCache.Put(ctx, manifest, s.Clock.Now().Add(s.packTTL())); err != nil {
			decisions = append(decisions, Decision{Stage: "pack_cache", ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Reason: fmt.Sprintf("cache normalized pack: %v", err)})
		}
	}
	return member.NormalizedPath, decisions, nil
}

func (s *Service) analyzeCandidate(ctx context.Context, request Request, item downloadedCandidate, installed bool, existing store.Installation) (analyzedCandidate, error) {
	if !match.Eligible(item.score, s.minimumScore()) {
		return analyzedCandidate{}, fmt.Errorf("candidate score is below threshold or identity was rejected")
	}
	if installed {
		if sameInstalledCandidate(existing, request.Media, item.candidate) {
			return analyzedCandidate{}, fmt.Errorf("provider candidate is already installed")
		}
		allowed, err := ShouldUpgradeForMedia(existing, request.Media, item.score, item.candidate.ExactHash, s.MinimumUpgradeDelta)
		if err != nil || !allowed {
			if err != nil {
				return analyzedCandidate{}, err
			}
			return analyzedCandidate{}, fmt.Errorf("candidate does not satisfy upgrade policy")
		}
	}
	if item.candidate.ExactHash {
		return analyzedCandidate{downloadedCandidate: item, analysis: domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1}, bypass: true}, nil
	}
	if canBypassLapse(request.Media, item.candidate, item.score, installed, s.LapsePolicy.normalized()) {
		return analyzedCandidate{downloadedCandidate: item, analysis: domain.SyncResult{Verdict: "score_bypass", Mode: "bypass", Reference: "release_evidence"}, bypass: true}, nil
	}
	analysis, err := s.Synchronizer.AnalyzeCandidate(ctx, item.candidate, request.Media.Fingerprint.Path, item.path)
	if err != nil || analysis.Verdict != "solid" {
		if err != nil {
			return analyzedCandidate{}, err
		}
		return analyzedCandidate{}, fmt.Errorf("LAPSE analysis was not solid")
	}
	return analyzedCandidate{downloadedCandidate: item, analysis: analysis}, nil
}

func (s *Service) finalizeCandidate(ctx context.Context, request Request, item analyzedCandidate, workspace string, index int) (finalizedCandidate, error) {
	if item.bypass {
		return finalizedCandidate{candidate: item.candidate, score: item.score, priority: item.priority, sync: item.analysis, path: item.path}, nil
	}
	extension := strings.ToLower(filepath.Ext(item.path))
	output := filepath.Join(workspace, fmt.Sprintf("synchronized-%d%s", index, extension))
	synchronized, err := s.Synchronizer.SynchronizeCandidate(ctx, item.candidate, request.Media.Fingerprint.Path, item.path, output)
	if err != nil || synchronized.Verdict != "solid" {
		if err != nil {
			return finalizedCandidate{}, err
		}
		return finalizedCandidate{}, fmt.Errorf("LAPSE synchronization was not solid")
	}
	// Use analysis confidence for ranking because every candidate was compared at
	// the same stage; retain the synchronization metadata that produced the file.
	synchronized.Confidence = item.analysis.Confidence
	return finalizedCandidate{candidate: item.candidate, score: item.score, priority: item.priority, sync: synchronized, path: output}, nil
}

func sameInstalledCandidate(existing store.Installation, media domain.Media, candidate domain.Candidate) bool {
	return InstallationMatchesMedia(existing, media) &&
		existing.ProviderID == candidate.ProviderID &&
		existing.CandidateID == candidate.ResultID
}

func (s *Service) updateInstallationAssessment(ctx context.Context, existing store.Installation, candidate domain.Candidate, score domain.Score) (store.Installation, error) {
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
		return existing, fmt.Errorf("refresh installed candidate assessment: %w", err)
	}
	return existing, nil
}

func setReassessmentResult(result *Result, installation store.Installation, candidate domain.Candidate, score domain.Score, now time.Time) {
	result.Candidate = candidate
	result.Score = score
	result.Installation = installation
	result.NextUpgrade = NextUpgradeAt(now, score, candidate.ExactHash)
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
	return !policy.RequireEpisodeEvidence || media.Ref.Kind != domain.MediaEpisode || candidateHasEpisodeEvidence(media, candidate)
}

func candidateHasEpisodeEvidence(media domain.Media, candidate domain.Candidate) bool {
	return match.HasEpisodeEvidence(media, candidate)
}

func (s *Service) install(ctx context.Context, request Request, prepared finalizedCandidate, existing store.Installation, installed bool, result Result) (Result, error) {
	destination := subtitleDestination(request.Media.Fingerprint.Path, request.Language, prepared.path)
	if installed {
		if !strings.EqualFold(filepath.Ext(existing.Path), filepath.Ext(prepared.path)) {
			result.Outcome = OutcomeRejected
			result.Decisions = append(result.Decisions, Decision{Stage: "installation", ProviderID: prepared.candidate.ProviderID, ResultID: prepared.candidate.ResultID, Reason: "format-changing upgrades are not supported"})
			return result, nil
		}
		destination = existing.Path
	}
	installation, err := s.Installer.Install(ctx, InstallRequest{MediaID: request.MediaID, Media: request.Media, Language: request.Language, SourcePath: prepared.path, DestinationPath: destination, Candidate: prepared.candidate, Score: prepared.score, SyncResult: prepared.sync})
	if err != nil {
		return result, err
	}
	result.Outcome = OutcomeInstalled
	result.Candidate = prepared.candidate
	result.Score = prepared.score
	result.SyncResult = prepared.sync
	result.Installation = installation
	result.NextUpgrade = NextUpgradeAt(s.Clock.Now(), prepared.score, prepared.candidate.ExactHash)
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

type boundedDownloadWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
	exceeded  bool
}

func (w *boundedDownloadWriter) Write(payload []byte) (int, error) {
	if int64(len(payload)) > w.remaining {
		w.exceeded = true
		if w.remaining == 0 {
			return 0, fmt.Errorf("provider download exceeds %d bytes", w.limit)
		}
		payload = payload[:w.remaining]
	}
	written, err := w.writer.Write(payload)
	w.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if w.exceeded {
		return written, fmt.Errorf("provider download exceeds %d bytes", w.limit)
	}
	return written, nil
}

func candidateRecord(candidate domain.Candidate, score domain.Score, eligible bool) (store.CandidateRecord, error) {
	safe := candidate
	safe.DownloadRef = ""
	if safe.Pack != nil {
		packInfo := *safe.Pack
		packInfo.DirectMembers = append([]domain.PackMemberRef(nil), safe.Pack.DirectMembers...)
		for index := range packInfo.DirectMembers {
			packInfo.DirectMembers[index].DownloadRef = ""
		}
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
	return store.CandidateRecord{ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, MetadataJSON: metadata, ScoreJSON: scoreJSON, ValidationJSON: validation}, nil
}

func sortFinalized(candidates []finalizedCandidate) {
	sort.SliceStable(candidates, func(left, right int) bool {
		a, b := candidates[left], candidates[right]
		if a.score.Total != b.score.Total {
			return a.score.Total > b.score.Total
		}
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
