package subsource

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
	cache := newTTLCache[[]int](clock, 6*time.Hour, 4)
	cache.put("a", []int{1, 2})
	if got, ok := cache.get("a"); !ok || len(got) != 2 {
		t.Fatalf("get = %v, %v", got, ok)
	}
	if got, ok := cache.get("missing"); ok {
		t.Fatalf("missing key returned %v", got)
	}
	clock.now = clock.now.Add(6*time.Hour - time.Nanosecond)
	if _, ok := cache.get("a"); !ok {
		t.Fatal("entry expired early")
	}
	clock.now = clock.now.Add(time.Nanosecond)
	if _, ok := cache.get("a"); ok {
		t.Fatal("entry survived its TTL")
	}
}

func TestTTLCacheStoresEmptyAnswers(t *testing.T) {
	cache := newTTLCache[[]int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	cache.put("empty", nil)
	if got, ok := cache.get("empty"); !ok || len(got) != 0 {
		t.Fatalf("get = %v, %v; want cached empty answer", got, ok)
	}
}

func TestTTLCacheEvictsOldestBeyondLimit(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	cache := newTTLCache[int](clock, time.Hour, 3)
	for i := range 4 {
		clock.now = clock.now.Add(time.Second)
		cache.put(fmt.Sprint(i), i)
	}
	if _, ok := cache.get("0"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	for _, key := range []string{"1", "2", "3"} {
		if _, ok := cache.get(key); !ok {
			t.Fatalf("entry %s was evicted", key)
		}
	}
	// Replacing an entry refreshes it rather than growing the cache.
	clock.now = clock.now.Add(time.Second)
	cache.put("1", 10)
	cache.put("4", 4)
	if _, ok := cache.get("2"); ok {
		t.Fatal("refreshed entry should outlive the next oldest")
	}
	if got, ok := cache.get("1"); !ok || got != 10 {
		t.Fatalf("refreshed entry = %v, %v", got, ok)
	}
}

func TestTTLCacheLoadCoalescesAndRetriesAfterFailure(t *testing.T) {
	cache := newTTLCache[int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	if _, err := cache.load(context.Background(), "k", func() (int, bool, error) { return 0, false, errors.New("boom") }); err == nil {
		t.Fatal("fetch error was not returned")
	}
	if _, ok := cache.get("k"); ok {
		t.Fatal("failed fetch was cached")
	}
	value, err := cache.load(context.Background(), "k", func() (int, bool, error) { return 0, false, nil })
	if err != nil || value != 0 {
		t.Fatalf("load = %v, %v", value, err)
	}
	if _, ok := cache.get("k"); ok {
		t.Fatal("uncacheable answer was cached")
	}
	calls := 0
	for range 2 {
		value, err = cache.load(context.Background(), "k", func() (int, bool, error) { calls++; return 7, true, nil })
	}
	if err != nil || value != 7 || calls != 1 {
		t.Fatalf("load = %v, %v after %d fetches", value, err, calls)
	}
}

func TestTTLCacheLoadWaiterHonorsCancellation(t *testing.T) {
	cache := newTTLCache[int](&fakeClock{now: time.Unix(0, 0)}, time.Hour, 4)
	started, release := make(chan struct{}), make(chan struct{})
	go cache.load(context.Background(), "k", func() (int, bool, error) { close(started); <-release; return 1, true, nil })
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.load(ctx, "k", func() (int, bool, error) { t.Error("waiter fetched"); return 0, false, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want cancellation", err)
	}
	close(release)
}
