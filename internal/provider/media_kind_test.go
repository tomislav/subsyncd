package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"testing"
	"time"
)

type noAccessCache struct{ t *testing.T }

func (c noAccessCache) GetProviderCache(context.Context, string, time.Time) (store.ProviderCacheEntry, bool, error) {
	c.t.Error("unsupported kind reached cache read")
	return store.ProviderCacheEntry{}, false, nil
}
func (c noAccessCache) PutProviderCache(context.Context, store.ProviderCacheEntry) error {
	c.t.Error("unsupported kind reached cache write")
	return nil
}

func TestCoordinatorSkipsUnsupportedMediaBeforeCacheAndLogs(t *testing.T) {
	var caps Capabilities
	if err := json.Unmarshal([]byte(`{"ExactFileHash":true,"MediaKinds":["episode"]}`), &caps); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []SearchMode{SearchBroad, SearchExactHash} {
		t.Run(string(mode), func(t *testing.T) {
			p := &fakeProvider{id: "tv-only", capabilities: caps}
			var logs bytes.Buffer
			events, _ := observability.New(&logs, observability.Options{Level: "debug"})
			c := newTestCoordinator(Observe(p, events))
			c.Events = events
			c.Cache = noAccessCache{t}
			q := SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}, Language: "en", Mode: mode}
			got := c.Search(context.Background(), q)
			if len(p.calls) != 0 || logs.Len() != 0 || len(got.Candidates) != 0 || len(got.Errors) != 0 {
				t.Fatalf("unsupported kind was searched: calls=%v logs=%s result=%+v", p.calls, logs.String(), got)
			}
			c.Cache = nil
			q.Media.Ref.Kind = domain.MediaEpisode
			c.Search(context.Background(), q)
			if len(p.calls) != 1 {
				t.Fatal("supported episode was skipped")
			}
		})
	}
}
