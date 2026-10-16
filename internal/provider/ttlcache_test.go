package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestTTLCacheReturnsStoredValuesUntilExpiry(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	cache := NewTTLCache[[]int](clock, 6*time.Hour, 4)
	cache.Put("a", []int{1, 2})
	if got, ok := cache.Get("a"); !ok || len(got) != 2 {
		t.Fatalf("get = %v, %v", got, ok)
	}
	if got, ok := cache.Get("missing"); ok {
		t.Fatalf("missing key returned %v", got)
	}
	clock.now = clock.now.Add(6*time.Hour - time.Nanosecond)
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("entry expired early")
	}
	clock.now = clock.now.Add(time.Nanosecond)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("entry survived its TTL")
	}
}

func TestTTLCacheStoresEmptyAnswers(t *testing.T) {
	cache := NewTTLCache[[]int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	cache.Put("empty", nil)
	if got, ok := cache.Get("empty"); !ok || len(got) != 0 {
		t.Fatalf("get = %v, %v; want cached empty answer", got, ok)
	}
}

func TestTTLCacheEvictsOldestBeyondLimit(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	cache := NewTTLCache[int](clock, time.Hour, 3)
	for i := range 4 {
		clock.now = clock.now.Add(time.Second)
		cache.Put(fmt.Sprint(i), i)
	}
	if _, ok := cache.Get("0"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	for _, key := range []string{"1", "2", "3"} {
		if _, ok := cache.Get(key); !ok {
			t.Fatalf("entry %s was evicted", key)
		}
	}
	// Replacing an entry refreshes it rather than growing the cache.
	clock.now = clock.now.Add(time.Second)
	cache.Put("1", 10)
	cache.Put("4", 4)
	if _, ok := cache.Get("2"); ok {
		t.Fatal("refreshed entry should outlive the next oldest")
	}
	if got, ok := cache.Get("1"); !ok || got != 10 {
		t.Fatalf("refreshed entry = %v, %v", got, ok)
	}
}

func TestTTLCacheLoadCoalescesAndRetriesAfterFailure(t *testing.T) {
	cache := NewTTLCache[int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	if _, err := cache.Load(context.Background(), "k", func() (int, bool, error) { return 0, false, errors.New("boom") }); err == nil {
		t.Fatal("fetch error was not returned")
	}
	if _, ok := cache.Get("k"); ok {
		t.Fatal("failed fetch was cached")
	}
	value, err := cache.Load(context.Background(), "k", func() (int, bool, error) { return 0, false, nil })
	if err != nil || value != 0 {
		t.Fatalf("load = %v, %v", value, err)
	}
	if _, ok := cache.Get("k"); ok {
		t.Fatal("uncacheable answer was cached")
	}
	calls := 0
	for range 2 {
		value, err = cache.Load(context.Background(), "k", func() (int, bool, error) { calls++; return 7, true, nil })
	}
	if err != nil || value != 7 || calls != 1 {
		t.Fatalf("load = %v, %v after %d fetches", value, err, calls)
	}
}

func TestTTLCacheLoadWaiterHonorsCancellation(t *testing.T) {
	cache := NewTTLCache[int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	started, release := make(chan struct{}), make(chan struct{})
	go cache.Load(context.Background(), "k", func() (int, bool, error) { close(started); <-release; return 1, true, nil })
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Load(ctx, "k", func() (int, bool, error) { t.Error("waiter fetched"); return 0, false, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want cancellation", err)
	}
	close(release)
}
