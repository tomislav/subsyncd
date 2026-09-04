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
		parsed, err := url.Parse(safe[index].DownloadRef)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			safe[index].DownloadRef = ""
		}
	}
	return safe
}

func providerCacheKey(providerID string, query SearchQuery) string {
	fingerprint := query.Media.Fingerprint
	raw := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%d", normalizedCandidateCacheVersion, providerID, query.Mode, query.Language, query.Media.Ref.Instance, query.Media.Ref.FileID, fingerprint.Size, fingerprint.ModTime.UnixNano(), query.Media.ReleaseName, query.Media.Season, query.Media.Episode)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
