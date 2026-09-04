package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"subsyncd/internal/domain"
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
	key := providerCacheKey(provider.ID(), query)
	now := c.Clock.Now()
	if c.Cache != nil {
		entry, found, err := c.Cache.GetProviderCache(ctx, key, now)
		if err != nil {
			return nil, fmt.Errorf("read %s search cache: %w", provider.ID(), err)
		}
		if found {
			var candidates []domain.Candidate
			if err := json.Unmarshal(entry.ResultsJSON, &candidates); err == nil {
				return candidates, nil
			}
		}
	}
	candidates, err := provider.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	if c.Cache != nil {
		encoded, err := json.Marshal(cacheSafeCandidates(candidates))
		if err != nil {
			return nil, fmt.Errorf("encode %s search cache: %w", provider.ID(), err)
		}
		if err := c.Cache.PutProviderCache(ctx, store.ProviderCacheEntry{Key: key, ProviderID: provider.ID(), ResultsJSON: encoded, ExpiresAt: now.Add(searchCacheTTL)}); err != nil {
			return nil, fmt.Errorf("write %s search cache: %w", provider.ID(), err)
		}
	}
	return candidates, nil
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
