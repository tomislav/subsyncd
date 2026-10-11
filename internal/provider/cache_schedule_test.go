package provider

import (
	"testing"

	"subsyncd/internal/schedule"
)

// A missing-subtitle retry sooner than the search cache lifetime would reuse
// the previous empty answer and could not find a newly published subtitle.
func TestMissingRetriesNeverComeBeforeTheSearchCacheExpires(t *testing.T) {
	if first := schedule.MissingDelay(1, 0); first < searchCacheTTL {
		t.Fatalf("first missing retry after %s, sooner than the %s search cache", first, searchCacheTTL)
	}
}
