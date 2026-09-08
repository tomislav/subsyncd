package provider

import (
	"context"
	"errors"
	"subsyncd/internal/store"
	"testing"
	"time"
)

type failingSearchCache struct{ readErr, writeErr error }

func (c failingSearchCache) GetProviderCache(context.Context, string, time.Time) (store.ProviderCacheEntry, bool, error) {
	return store.ProviderCacheEntry{}, false, c.readErr
}
func (c failingSearchCache) PutProviderCache(context.Context, store.ProviderCacheEntry) error {
	return c.writeErr
}

func TestCoordinatorMarksSearchPersistenceFailures(t *testing.T) {
	sentinel := errors.New("test persistence failure")
	for _, mode := range []SearchMode{SearchExactHash, SearchBroad} {
		for _, operation := range []string{"read", "write"} {
			t.Run(string(mode)+"/"+operation, func(t *testing.T) {
				item := &fakeProvider{id: "one", capabilities: Capabilities{ExactFileHash: true}}
				coordinator := newTestCoordinator(item)
				cache := failingSearchCache{}
				if operation == "read" {
					cache.readErr = sentinel
				} else {
					cache.writeErr = sentinel
				}
				coordinator.Cache = cache
				result := coordinator.Search(t.Context(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: mode})
				err := result.Errors["one"]
				var persistence *SearchPersistenceError
				if !errors.As(err, &persistence) || !errors.Is(err, sentinel) {
					t.Fatalf("search error = %T/%v, want persistence marker wrapping sentinel", err, err)
				}
			})
		}
	}
}
