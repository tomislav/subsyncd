package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

const (
	searchCacheTTL                  = 6 * time.Hour
	normalizedCandidateCacheVersion = "candidate-v2"
)

type SearchCache interface {
	GetProviderCache(context.Context, string, time.Time) (store.ProviderCacheEntry, bool, error)
	PutProviderCache(context.Context, store.ProviderCacheEntry) error
}

type Coordinator struct {
	Providers []Provider
	Cache     SearchCache
	Clock     Clock
	Events    *observability.Emitter
}

type SearchResult struct {
	Candidates []domain.Candidate
	Errors     map[string]error
}

func (c *Coordinator) Search(ctx context.Context, query SearchQuery) SearchResult {
	result := SearchResult{Errors: make(map[string]error)}
	for _, provider := range c.Providers {
		if !provider.Capabilities().ExactFileHash || !provider.SupportsLanguage(query.Language) {
			continue
		}
		exactQuery := query
		exactQuery.Mode = SearchExactHash
		candidates, err := c.searchProvider(ctx, provider, exactQuery)
		if err != nil {
			result.Errors[provider.ID()] = err
			continue
		}
		for _, candidate := range candidates {
			if candidate.ExactHash {
				result.Candidates = []domain.Candidate{candidate}
				return result
			}
		}
	}

	type broadResult struct {
		index      int
		providerID string
		candidates []domain.Candidate
		err        error
	}
	channel := make(chan broadResult, len(c.Providers))
	active := 0
	for index, provider := range c.Providers {
		if !provider.SupportsLanguage(query.Language) {
			continue
		}
		active++
		go func(index int, provider Provider) {
			broadQuery := query
			broadQuery.Mode = SearchBroad
			candidates, err := c.searchProvider(ctx, provider, broadQuery)
			channel <- broadResult{index: index, providerID: provider.ID(), candidates: candidates, err: err}
		}(index, provider)
	}
	ordered := make([]broadResult, len(c.Providers))
	for range active {
		item := <-channel
		ordered[item.index] = item
	}
	for _, item := range ordered {
		if item.providerID == "" {
			continue
		}
		if item.err != nil {
			result.Errors[item.providerID] = item.err
			continue
		}
		delete(result.Errors, item.providerID)
		result.Candidates = append(result.Candidates, item.candidates...)
	}
	return result
}

func (c *Coordinator) searchProvider(ctx context.Context, provider Provider, query SearchQuery) ([]domain.Candidate, error) {
	events := c.Events
	if events == nil {
		events = observability.Discard()
	}
	events = events.For("provider")
	startedAt := time.Now()
	base := []slog.Attr{
		slog.String("provider", provider.ID()),
		slog.String("search_mode", string(query.Mode)),
		slog.String("language", string(query.Language)),
	}
	events.Log(ctx, slog.LevelInfo, "provider.search_started", "provider search started", base...)
	complete := func(candidates []domain.Candidate, cacheStatus string, err error) {
		attrs := append([]slog.Attr(nil), base...)
		attrs = append(attrs,
			slog.String("outcome", providerOutcome(err, len(candidates))),
			slog.String("cache_status", cacheStatus),
			slog.Int("candidate_count", len(candidates)),
			slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
		)
		if err != nil {
			attrs = append(attrs, events.ErrorAttrs(providerErrorKind(err), err)...)
		}
		events.Log(ctx, searchLogLevel(err), "provider.search_completed", "provider search completed", attrs...)
	}

	key := providerCacheKey(provider.ID(), query)
	now := c.Clock.Now()
	if c.Cache != nil {
		entry, found, err := c.Cache.GetProviderCache(ctx, key, now)
		if err != nil {
			wrapped := fmt.Errorf("read %s search cache: %w", provider.ID(), err)
			complete(nil, "error", wrapped)
			return nil, wrapped
		}
		if found {
			var candidates []domain.Candidate
			if err := json.Unmarshal(entry.ResultsJSON, &candidates); err == nil {
				candidates = deduplicateCandidates(candidates)
				events.Log(ctx, slog.LevelDebug, "provider.cache_hit", "provider search cache hit", base...)
				complete(candidates, "hit", nil)
				return candidates, nil
			}
		}
	}
	events.Log(ctx, slog.LevelDebug, "provider.cache_miss", "provider search cache miss", base...)
	candidates, err := provider.Search(ctx, query)
	if err != nil {
		complete(nil, "miss", err)
		return nil, err
	}
	candidates = deduplicateCandidates(candidates)
	if c.Cache != nil {
		encoded, err := json.Marshal(cacheSafeCandidates(candidates))
		if err != nil {
			wrapped := fmt.Errorf("encode %s search cache: %w", provider.ID(), err)
			complete(candidates, "miss", wrapped)
			return nil, wrapped
		}
		if err := c.Cache.PutProviderCache(ctx, store.ProviderCacheEntry{Key: key, ProviderID: provider.ID(), ResultsJSON: encoded, ExpiresAt: now.Add(searchCacheTTL)}); err != nil {
			wrapped := fmt.Errorf("write %s search cache: %w", provider.ID(), err)
			complete(candidates, "miss", wrapped)
			return nil, wrapped
		}
	}
	complete(candidates, "miss", nil)
	return candidates, nil
}

func deduplicateCandidates(candidates []domain.Candidate) []domain.Candidate {
	result := make([]domain.Candidate, 0, len(candidates))
	indices := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		if candidate.ProviderID == "" || candidate.ResultID == "" {
			result = append(result, candidate)
			continue
		}
		key := candidate.ProviderID + "\x00" + candidate.ResultID
		index, exists := indices[key]
		if !exists {
			indices[key] = len(result)
			candidate.ReleaseNames = append([]string(nil), candidate.ReleaseNames...)
			result = append(result, candidate)
			continue
		}
		merged := &result[index]
		if merged.Language == "" {
			merged.Language = candidate.Language
		}
		if merged.Kind == "" {
			merged.Kind = candidate.Kind
		}
		if merged.Title == "" {
			merged.Title = candidate.Title
		}
		if merged.Year == 0 {
			merged.Year = candidate.Year
		}
		if merged.Season == 0 {
			merged.Season = candidate.Season
		}
		if merged.Episode == 0 {
			merged.Episode = candidate.Episode
		}
		if merged.AbsoluteEpisode == 0 {
			merged.AbsoluteEpisode = candidate.AbsoluteEpisode
		}
		if merged.ExternalIDs.IMDb == "" {
			merged.ExternalIDs.IMDb = candidate.ExternalIDs.IMDb
		}
		if merged.ExternalIDs.TMDB == 0 {
			merged.ExternalIDs.TMDB = candidate.ExternalIDs.TMDB
		}
		if merged.ExternalIDs.TVDB == 0 {
			merged.ExternalIDs.TVDB = candidate.ExternalIDs.TVDB
		}
		merged.ReleaseNames = appendUniqueStrings(merged.ReleaseNames, candidate.ReleaseNames...)
		merged.ExactHash = merged.ExactHash || candidate.ExactHash
		merged.Forced = merged.Forced || candidate.Forced
		merged.HearingImpaired = merged.HearingImpaired || candidate.HearingImpaired
		merged.Rating = max(merged.Rating, candidate.Rating)
		merged.Popularity = max(merged.Popularity, candidate.Popularity)
		merged.DownloadCount = max(merged.DownloadCount, candidate.DownloadCount)
		if merged.DownloadRef == "" {
			merged.DownloadRef = candidate.DownloadRef
		}
		if merged.Pack == nil {
			merged.Pack = candidate.Pack
		}
	}
	return result
}

func appendUniqueStrings(existing []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		existing = append(existing, value)
	}
	return existing
}

func providerOutcome(err error, candidateCount int) string {
	if err == nil {
		if candidateCount == 0 {
			return "no_result"
		}
		return "success"
	}
	var cooldown *CooldownError
	var quota *QuotaError
	var disabled *DisabledError
	var authentication *AuthenticationError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.As(err, &cooldown), errors.As(err, &quota):
		return "throttled"
	case errors.As(err, &disabled):
		return "disabled"
	case errors.As(err, &authentication):
		return "authentication_failed"
	default:
		return "failed"
	}
}

func providerErrorKind(err error) string {
	return providerOutcome(err, 0)
}

func searchLogLevel(err error) slog.Level {
	if err == nil {
		return slog.LevelInfo
	}
	var cooldown *CooldownError
	var quota *QuotaError
	var disabled *DisabledError
	if errors.As(err, &cooldown) || errors.As(err, &quota) || errors.As(err, &disabled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return slog.LevelWarn
	}
	return slog.LevelError
}

func cacheSafeCandidates(candidates []domain.Candidate) []domain.Candidate {
	safe := make([]domain.Candidate, len(candidates))
	copy(safe, candidates)
	for index := range safe {
		safe[index].ResultID = stripCandidateQuery(safe[index].ResultID)
		parsed, err := url.Parse(safe[index].DownloadRef)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			safe[index].DownloadRef = ""
		} else {
			safe[index].DownloadRef = stripCandidateQuery(safe[index].DownloadRef)
		}
		if safe[index].Pack != nil {
			pack := *safe[index].Pack
			pack.DirectMembers = append([]domain.PackMemberRef(nil), pack.DirectMembers...)
			for member := range pack.DirectMembers {
				pack.DirectMembers[member].DownloadRef = ""
			}
			safe[index].Pack = &pack
		}
	}
	return safe
}

func stripCandidateQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		value, _, _ := strings.Cut(raw, "?")
		return value
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func providerCacheKey(providerID string, query SearchQuery) string {
	fingerprint := query.Media.Fingerprint
	raw := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%d", normalizedCandidateCacheVersion, providerID, query.Mode, query.Language, query.Media.Ref.Instance, query.Media.Ref.FileID, fingerprint.Size, fingerprint.ModTime.UnixNano(), query.Media.ReleaseName, query.Media.Season, query.Media.Episode)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
