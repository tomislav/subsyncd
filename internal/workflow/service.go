package workflow

import (
	"context"
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
	RecordCandidates(context.Context, int64, domain.Language, []store.CandidateRecord) error
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

type preparedCandidate struct {
	candidate domain.Candidate
	score     domain.Score
	sync      domain.SyncResult
	path      string
	priority  int
}

func (s *Service) Run(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := s.validate(request); err != nil {
		return Result{}, err
	}
	result := Result{ProviderErrors: map[string]error{}}
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
			score := s.evaluate(request.Media, cached.Candidate, request.Language)
			prepared, prepareErr := s.synchronize(ctx, request, cached.Candidate, score, cached.Path, workspace, 0, installed, existing)
			if prepareErr == nil {
				return s.install(ctx, request, prepared, existing, installed, result)
			}
			result.Decisions = append(result.Decisions, Decision{Stage: "pack_cache", ProviderID: cached.Candidate.ProviderID, ResultID: cached.Candidate.ResultID, Reason: prepareErr.Error()})
		}
	}

	search := s.Searcher.Search(ctx, provider.SearchQuery{Media: request.Media, Language: request.Language})
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.ProviderErrors = search.Errors
	if len(search.Candidates) == 0 {
		if retry, allUnavailable := allProvidersUnavailable(search.Errors, len(s.ProviderOrder)); allUnavailable {
			result.Outcome = OutcomeThrottled
			result.RetryAt = retry
			return result, nil
		}
		if len(s.ProviderOrder) > 0 && len(search.Errors) >= len(s.ProviderOrder) {
			return result, fmt.Errorf("all %d assigned subtitle providers failed", len(s.ProviderOrder))
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
	eligible := evaluated[:0]
	for _, item := range evaluated {
		if !match.Eligible(item.Score, s.minimumScore()) {
			continue
		}
		if installed {
			allowed, upgradeErr := ShouldUpgradeForMedia(existing, request.Media, item.Score, item.Candidate.ExactHash, s.MinimumUpgradeDelta)
			if upgradeErr != nil {
				return result, upgradeErr
			}
			if !allowed {
				continue
			}
		}
		eligible = append(eligible, item)
	}
	if len(eligible) == 0 {
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

	prepared := make([]preparedCandidate, 0, len(eligible))
	var candidateFailures []error
	for index, item := range eligible {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		path, decisions, prepareErr := s.downloadAndSelect(ctx, request, item.Candidate, workspace, index)
		result.Decisions = append(result.Decisions, decisions...)
		if prepareErr != nil {
			candidateFailures = append(candidateFailures, prepareErr)
			result.Decisions = append(result.Decisions, Decision{Stage: "candidate", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: prepareErr.Error()})
			continue
		}
		ready, syncErr := s.synchronize(ctx, request, item.Candidate, item.Score, path, workspace, index, installed, existing)
		if syncErr != nil {
			candidateFailures = append(candidateFailures, syncErr)
			result.Decisions = append(result.Decisions, Decision{Stage: "synchronization", ProviderID: item.Candidate.ProviderID, ResultID: item.Candidate.ResultID, Reason: syncErr.Error()})
			continue
		}
		ready.priority = item.ProviderPriority
		prepared = append(prepared, ready)
	}
	if len(prepared) == 0 {
		if retry, unavailable := unavailableErrors(candidateFailures); unavailable {
			result.Outcome = OutcomeThrottled
			result.RetryAt = retry
			return result, nil
		}
		result.Outcome = OutcomeRejected
		return result, nil
	}
	sortPrepared(prepared)
	return s.install(ctx, request, prepared[0], existing, installed, result)
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
		return "", nil, fmt.Errorf("provider download exceeds %d bytes", bounded.limit)
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

func (s *Service) synchronize(ctx context.Context, request Request, candidate domain.Candidate, score domain.Score, source, workspace string, index int, installed bool, existing store.Installation) (preparedCandidate, error) {
	if !match.Eligible(score, s.minimumScore()) {
		return preparedCandidate{}, fmt.Errorf("candidate score is below threshold or identity was rejected")
	}
	if installed {
		allowed, err := ShouldUpgradeForMedia(existing, request.Media, score, candidate.ExactHash, s.MinimumUpgradeDelta)
		if err != nil || !allowed {
			if err != nil {
				return preparedCandidate{}, err
			}
			return preparedCandidate{}, fmt.Errorf("candidate does not satisfy upgrade policy")
		}
	}
	if candidate.ExactHash {
		return preparedCandidate{candidate: candidate, score: score, sync: domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1}, path: source}, nil
	}
	if canBypassLapse(request.Media, candidate, score, installed, s.LapsePolicy.normalized()) {
		return preparedCandidate{candidate: candidate, score: score, sync: domain.SyncResult{Verdict: "score_bypass", Mode: "bypass", Reference: "release_evidence"}, path: source}, nil
	}
	analysis, err := s.Synchronizer.AnalyzeCandidate(ctx, candidate, request.Media.Fingerprint.Path, source)
	if err != nil || analysis.Verdict != "solid" {
		if err != nil {
			return preparedCandidate{}, err
		}
		return preparedCandidate{}, fmt.Errorf("LAPSE analysis was not solid")
	}
	extension := strings.ToLower(filepath.Ext(source))
	output := filepath.Join(workspace, fmt.Sprintf("synchronized-%d%s", index, extension))
	synchronized, err := s.Synchronizer.SynchronizeCandidate(ctx, candidate, request.Media.Fingerprint.Path, source, output)
	if err != nil || synchronized.Verdict != "solid" {
		if err != nil {
			return preparedCandidate{}, err
		}
		return preparedCandidate{}, fmt.Errorf("LAPSE synchronization was not solid")
	}
	// Use analysis confidence for ranking because every candidate was compared at
	// the same stage; retain the synchronization metadata that produced the file.
	synchronized.Confidence = analysis.Confidence
	return preparedCandidate{candidate: candidate, score: score, sync: synchronized, path: output}, nil
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
	if candidate.Season == media.Season && candidate.Episode == media.Episode && media.Season > 0 && media.Episode > 0 {
		return true
	}
	if media.AbsoluteEpisode > 0 && candidate.AbsoluteEpisode == media.AbsoluteEpisode {
		return true
	}
	for _, name := range candidate.ReleaseNames {
		release := match.ParseRelease(name)
		end := release.EpisodeEnd
		if end == 0 {
			end = release.Episode
		}
		if release.Season == media.Season && release.Episode > 0 && release.Episode <= media.Episode && media.Episode <= end {
			return true
		}
	}
	return false
}

func (s *Service) install(ctx context.Context, request Request, prepared preparedCandidate, existing store.Installation, installed bool, result Result) (Result, error) {
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

func sortPrepared(candidates []preparedCandidate) {
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
