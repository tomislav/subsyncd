package workflow

import (
	"sync"
	"time"
)

// MaxReplacedRemovalsPerHour is how many sidecars of replaced media files
// (reason media_replaced) one daemon deletes in any rolling hour. An event
// that replaces many files at once, such as an Arr database restore that
// gives every file a new ID, must not empty the library faster than the
// providers' quotas can fill it again. Past the limit the sidecar and its
// record stay (install.removal_deferred), the search ends throttled, and its
// retry, once a slot is free again, deletes the sidecar first.
const MaxReplacedRemovalsPerHour = 20

const replacedRemovalWindow = time.Hour

// removalRetryMargin is added to the time the next slot frees, so a deferred
// search does not come back a moment too early.
const removalRetryMargin = time.Minute

// RemovalBudget counts deletions in a rolling window. It lives in memory: a
// restart starts a new window. A nil budget never limits.
type RemovalBudget struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	taken  []time.Time
}

// NewReplacedRemovalBudget returns the budget for media_replaced deletions:
// MaxReplacedRemovalsPerHour per rolling hour.
func NewReplacedRemovalBudget() *RemovalBudget {
	return &RemovalBudget{limit: MaxReplacedRemovalsPerHour, window: replacedRemovalWindow}
}

// reserve takes one deletion at now, reporting false when the window is
// full. The returned release gives it back, for a deletion that did not
// happen.
func (b *RemovalBudget) reserve(now time.Time) (func(), bool) {
	if b == nil {
		return func() {}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.taken[:0]
	for _, at := range b.taken {
		if now.Sub(at) < b.window {
			kept = append(kept, at)
		}
	}
	b.taken = kept
	if len(b.taken) >= b.limit {
		return nil, false
	}
	b.taken = append(b.taken, now)
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for index, at := range b.taken {
				if at.Equal(now) {
					b.taken = append(b.taken[:index], b.taken[index+1:]...)
					return
				}
			}
		})
	}, true
}

// nextSlot reports when the window next has room after now: now when it has
// room already, otherwise when its oldest deletion leaves the window. A nil
// budget always has room.
func (b *RemovalBudget) nextSlot(now time.Time) time.Time {
	if b == nil {
		return now
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	live := 0
	var oldest time.Time
	for _, at := range b.taken {
		if now.Sub(at) >= b.window {
			continue
		}
		live++
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	if live < b.limit {
		return now
	}
	return oldest.Add(b.window)
}
