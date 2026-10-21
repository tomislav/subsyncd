package pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

func TestCachePutFindExpiryAndTamperLifecycle(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	root := filepath.Join(t.TempDir(), "pack-cache")
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(root, repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	media := cacheMedia()
	manifest := extractedManifest(t, cacheCandidate("pack-one"))
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		payload, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(payload), "secret-not-persisted") {
			t.Errorf("cached file %s contains a raw download reference", path)
		}
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	member, found, err := cache.Find(ctx, media, "en")
	if err != nil || !found || member.Checksum == "" || member.Candidate.ResultID != "pack-one" {
		t.Fatalf("Find() = %#v/%v/%v", member, found, err)
	}
	entries, err := repository.ListPacksForEviction(ctx, clock.Now())
	if err != nil || len(entries) != 1 || !entries[0].LastAccessAt.Equal(clock.Now()) {
		t.Fatalf("last access was not touched: %#v/%v", entries, err)
	}
	if _, found, err := cache.Find(ctx, media, "hr"); err != nil || found {
		t.Fatalf("wrong-language Find() = %v/%v", found, err)
	}
	if err := os.WriteFile(member.Path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := cache.Find(ctx, media, "en"); err != nil || found {
		t.Fatalf("tampered Find() = %v/%v, want clean miss", found, err)
	}

	manifest = extractedManifest(t, cacheCandidate("pack-two"))
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	if _, found, err := cache.Find(ctx, media, "en"); err != nil || found {
		t.Fatalf("expired Find() = %v/%v", found, err)
	}
}

func TestCacheEvictsExpiredThenLRUAndCleansOrphans(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	root := filepath.Join(t.TempDir(), "pack-cache")
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(root, repository, clock, int64(len(validSRT)+1))
	if err != nil {
		t.Fatal(err)
	}
	first := extractedManifest(t, cacheCandidate("old"))
	if err := cache.Put(ctx, first, clock.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	second := extractedManifest(t, cacheCandidate("new"))
	if err := cache.Put(ctx, second, clock.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cache.Evict(ctx, clock.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := repository.ListPacksForEviction(ctx, clock.Now())
	if err != nil || len(entries) != 1 || entries[0].ResultID != "new" {
		t.Fatalf("entries = %#v, %v", entries, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists: %v", err)
	}
}

func TestCacheRejectsDatabasePathOutsideRoot(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	root := filepath.Join(t.TempDir(), "pack-cache")
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(root, repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cacheCandidate("outside")
	encoded, _ := json.Marshal(candidate)
	entry := store.PackCacheEntry{ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, SeriesKey: store.StrongestSeriesKey(candidate.ExternalIDs, candidate.Title, candidate.Year), Season: 1, Language: "en", ContentChecksum: "content", ManifestPath: "/tmp/outside/manifest.json", CandidateJSON: encoded, ByteSize: 1, ExpiresAt: clock.Now().Add(time.Hour), LastAccessAt: clock.Now()}
	member := store.PackMemberRecord{SafeName: "Show.S01E02.srt", CachePath: "/tmp/outside/Show.S01E02.srt", Checksum: "bad", Season: 1, EpisodeFrom: 2, EpisodeTo: 2}
	if err := repository.PutPack(ctx, entry, []store.PackMemberRecord{member}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Find(ctx, cacheMedia(), "en"); err == nil {
		t.Fatal("outside cache path should be rejected")
	}
}

func TestCacheSupportsConcurrentReaders(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(filepath.Join(t.TempDir(), "pack-cache"), repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, extractedManifest(t, cacheCandidate("concurrent")), time.Time{}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, 16)
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, found, err := cache.Find(ctx, cacheMedia(), "en")
			if err != nil {
				errors <- err
			} else if !found {
				errors <- fmt.Errorf("cache miss")
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestCacheFindEligibleSkipsRejectedPackWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(filepath.Join(t.TempDir(), "pack-cache"), repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"viable", "rejected"} {
		if err := cache.Put(ctx, extractedManifest(t, cacheCandidate(id)), time.Time{}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	rejectedAccess := clock.Now().Add(-time.Minute)
	var visited []string
	member, found, err := cache.FindEligible(ctx, cacheMedia(), "en", Eligibility{Accept: func(member CachedMember) (bool, error) {
		visited = append(visited, member.Candidate.ResultID)
		return member.Candidate.ResultID != "rejected", nil
	}})
	if err != nil || !found || member.Candidate.ResultID != "viable" || strings.Join(visited, ",") != "rejected,viable" {
		t.Fatalf("FindEligible() = %#v/%v/%v, visited %v", member, found, err, visited)
	}
	entries, err := repository.ListPacksForEviction(ctx, clock.Now())
	if err != nil || len(entries) != 2 {
		t.Fatalf("packs = %#v/%v", entries, err)
	}
	for _, entry := range entries {
		want := clock.Now()
		if entry.ResultID == "rejected" {
			want = rejectedAccess
		}
		if !entry.LastAccessAt.Equal(want) {
			t.Errorf("%s last access = %v, want %v", entry.ResultID, entry.LastAccessAt, want)
		}
	}
	clock.Advance(time.Minute)
	lookupErr := errors.New("rejection lookup failed")
	_, found, err = cache.FindEligible(ctx, cacheMedia(), "en", Eligibility{Accept: func(CachedMember) (bool, error) { return false, lookupErr }})
	if found || !errors.Is(err, lookupErr) {
		t.Fatalf("callback failure = %v/%v", found, err)
	}
	_, found, err = cache.FindEligible(ctx, cacheMedia(), "en", Eligibility{Accept: func(CachedMember) (bool, error) { return false, nil }})
	if found || err != nil {
		t.Fatalf("all rejected = %v/%v, want clean miss", found, err)
	}
	after, err := repository.ListPacksForEviction(ctx, clock.Now())
	if err != nil || len(after) != len(entries) {
		t.Fatalf("packs after failure = %#v/%v", after, err)
	}
	for i := range entries {
		if after[i].ID != entries[i].ID || !after[i].LastAccessAt.Equal(entries[i].LastAccessAt) {
			t.Errorf("callback failure touched cache: before %#v, after %#v", entries[i], after[i])
		}
	}
}

func TestCacheFindRerunsSelectionAndDeduplicatesIdenticalVersions(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(filepath.Join(t.TempDir(), "pack-cache"), repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cacheCandidate("ambiguous")
	payload := zipPayload(t, []zipEntry{{name: "one.S01E02.srt", body: validSRT}, {name: "two.S01E02.srt", body: validSRT}})
	manifest, err := Extract(ctx, candidate, strings.NewReader(string(payload)), int64(len(payload)), filepath.Join(t.TempDir(), "extracted"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if member, found, err := cache.Find(ctx, cacheMedia(), "en"); err != nil || !found || len(member.Alternatives) != 0 {
		t.Fatalf("deduplicated versions = %+v/%v/%v", member, found, err)
	}
}

func TestCacheFindEligibleSkipsRejectedVersionsBeforeKeepingThree(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(filepath.Join(t.TempDir(), "pack-cache"), repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cacheCandidate("versions")
	var entries []zipEntry
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		entries = append(entries, zipEntry{name: name + ".S01E02.srt", body: strings.ReplaceAll(validSRT, "Hello", "Hello "+name)})
	}
	payload := zipPayload(t, entries)
	manifest, err := Extract(ctx, candidate, strings.NewReader(string(payload)), int64(len(payload)), filepath.Join(t.TempDir(), "extracted"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	checksums := map[string]string{}
	for _, member := range manifest.Members {
		checksums[member.Checksum] = strings.TrimSuffix(member.SafeName, ".S01E02.srt")
	}
	lookup := func(rejected string) (CachedMember, bool, []string, error) {
		var exhausted []string
		member, found, err := cache.FindEligible(ctx, cacheMedia(), "en", Eligibility{
			Rejected: func(member CachedMember) (bool, error) {
				return strings.Contains(rejected, checksums[member.Checksum]), nil
			},
			Exhausted: func(member CachedMember) error {
				exhausted = append(exhausted, member.Candidate.ResultID)
				return nil
			},
		})
		return member, found, exhausted, err
	}
	names := func(member CachedMember) string {
		got := []string{checksums[member.Checksum]}
		for _, alternative := range member.Alternatives {
			got = append(got, checksums[alternative.Checksum])
		}
		return strings.Join(got, ",")
	}
	// Without rejections the first three versions are tried and two are left.
	if member, found, exhausted, err := lookup(""); err != nil || !found || names(member) != "a,b,c" || !member.MoreVersions || len(exhausted) != 0 {
		t.Fatalf("no rejections = %s more=%v found=%v exhausted=%v err=%v", names(member), member.MoreVersions, found, exhausted, err)
	}
	// Rejected versions give their places to untried ones.
	if member, found, exhausted, err := lookup("abc"); err != nil || !found || names(member) != "d,e" || member.MoreVersions || len(exhausted) != 0 {
		t.Fatalf("three rejected = %s more=%v found=%v exhausted=%v err=%v", names(member), member.MoreVersions, found, exhausted, err)
	}
	// With every version rejected the pack is reported exhausted.
	if member, found, exhausted, err := lookup("abcde"); err != nil || found || len(exhausted) != 1 || exhausted[0] != "versions" {
		t.Fatalf("all rejected = %+v found=%v exhausted=%v err=%v", member, found, exhausted, err)
	}
	lookupErr := errors.New("rejection lookup failed")
	if _, found, err := cache.FindEligible(ctx, cacheMedia(), "en", Eligibility{Rejected: func(CachedMember) (bool, error) { return false, lookupErr }}); found || !errors.Is(err, lookupErr) {
		t.Fatalf("rejection lookup failure = %v/%v", found, err)
	}
}

func TestCachePutRejectsExistingSymlinkDestination(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	root := filepath.Join(t.TempDir(), "pack-cache")
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(root, repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manifest := extractedManifest(t, cacheCandidate("symlink"))
	outside := t.TempDir()
	for _, member := range manifest.Members {
		payload, readErr := os.ReadFile(member.NormalizedPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(outside, member.SafeName), payload, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, cacheContentKey(manifest))); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, manifest, time.Time{}); err == nil {
		t.Fatal("Put() error = nil, want symlink destination rejection")
	}
	entries, err := repository.ListPacksForEviction(ctx, clock.Now())
	if err != nil || len(entries) != 0 {
		t.Fatalf("database entries = %#v, %v", entries, err)
	}
}

func TestCachePutRollsBackPublicationWhenDatabaseWriteFails(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "pack-cache")
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(root, database.Repository(), clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := extractedManifest(t, cacheCandidate("database-failure"))
	if err := cache.Put(ctx, manifest, time.Time{}); err == nil {
		t.Fatal("Put() error = nil after database close")
	}
	children, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("cache root still contains %d entries after rollback", len(children))
	}
}

func extractedManifest(t *testing.T, candidate domain.Candidate) Manifest {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "extracted")
	manifest, err := Extract(context.Background(), candidate, strings.NewReader(validSRT), int64(len(validSRT)), destination, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func cacheCandidate(resultID string) domain.Candidate {
	return domain.Candidate{ProviderID: "titlovi", ResultID: resultID, Language: "en", Kind: domain.MediaEpisode, Title: "Show", Year: 2024, Season: 1, ExternalIDs: domain.ExternalIDs{TVDB: 123}, Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 1, DirectMembers: []domain.PackMemberRef{{Filename: "Show.S01E02.srt", DownloadRef: "/secret-not-persisted"}}}, DownloadRef: "/Show.S01E02.srt?secret=not-persisted"}
}

func cacheMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 123}}
}

func openRepository(t *testing.T) *store.Repository {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database.Repository()
}

// A cached pack is served without a fresh download, so a member it could only
// give through the release-name fallback is not served from the cache.
func TestCacheFindDoesNotServeReleaseNameMatches(t *testing.T) {
	ctx := context.Background()
	repository := openRepository(t)
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	cache, err := NewCache(filepath.Join(t.TempDir(), "pack-cache"), repository, clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cacheCandidate("release-numbered")
	candidate.Pack.DirectMembers = nil
	payload := zipPayload(t, []zipEntry{{name: "Show.S02E01.srt", body: validSRT}, {name: "Show.S02E02.srt", body: validSRT}})
	manifest, err := Extract(ctx, candidate, strings.NewReader(string(payload)), int64(len(payload)), filepath.Join(t.TempDir(), "extracted"), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, manifest, time.Time{}); err != nil {
		t.Fatal(err)
	}
	media := cacheMedia()
	media.Episode = 10
	media.ReleaseName = "Show.S02E02.WEB-GROUP"
	member, found, err := cache.Find(ctx, media, "en")
	var selection *SelectionError
	if found || !errors.As(err, &selection) {
		t.Fatalf("Find() = %+v/%v/%v, want no cached member", member, found, err)
	}
}
