package pack

import (
	"context"
	"os"
	"subsyncd/internal/testutil"
	"testing"
	"time"
)

func TestCachePublishesChangedEvidenceWithoutReusingLegacyManifest(t *testing.T) {
	ctx := context.Background()
	cache, err := NewCache(t.TempDir(), openRepository(t), testutil.NewClock(time.Now()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manifest := extractedManifest(t, cacheCandidate("same-pack"))
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	manifest.Candidate.EvidenceVersion = "annotations-v1"
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	member, found, err := cache.Find(ctx, cacheMedia(), "en")
	if err != nil || !found || member.Candidate.EvidenceVersion != "annotations-v1" {
		t.Fatalf("fresh metadata hidden by immutable legacy manifest: found=%v version=%q err=%v", found, member.Candidate.EvidenceVersion, err)
	}
	entries, err := os.ReadDir(cache.root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("superseded manifest directory leaked: entries=%d err=%v", len(entries), err)
	}
}
