package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func searchQueueOrder(t *testing.T, repo *Repository, mediaID int64, language domain.Language) (next, order int64) {
	t.Helper()
	if err := repo.store.db.QueryRow(`SELECT next_attempt_at_ns, queue_order_ns FROM search_states WHERE media_id=? AND language=?`, mediaID, language.String()).Scan(&next, &order); err != nil {
		t.Fatal(err)
	}
	return next, order
}

// Outside throttling, queue position always equals the due time.
func requireQueueOrderFollowsDueTime(t *testing.T, repo *Repository) {
	t.Helper()
	var count int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states WHERE queue_order_ns <> next_attempt_at_ns`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d search states have queue_order_ns different from next_attempt_at_ns", count)
	}
}

func TestSearchWritesSetQueueOrderToDueTime(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)

	// Import/rename enqueue.
	first := insertTestMedia(t, repo, 1, now)
	requireSearchState(t, repo, first, "en", now.Add(-time.Hour), SearchPriorityMissing)
	requireQueueOrderFollowsDueTime(t, repo)

	// Webhook media event, including the rerun path during an active lease.
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-1", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en", "hr"}, At: now.Add(-30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	requireQueueOrderFollowsDueTime(t, repo)
	if _, err := repo.LeaseDueSearches(ctx, now, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "rename-1", Type: "rename", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en", "hr"}, At: now.Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	requireQueueOrderFollowsDueTime(t, repo)

	// Configured-language backfill.
	if _, err := repo.EnsureConfiguredLanguageSearches(ctx, []string{media.Ref.Instance}, []domain.Language{"de"}, now); err != nil {
		t.Fatal(err)
	}
	requireQueueOrderFollowsDueTime(t, repo)

	// Manual-search upgrade scheduling.
	if err := repo.EnsureUpgradeSearch(ctx, first, "pt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	requireQueueOrderFollowsDueTime(t, repo)
}

func TestCompleteSearchQueueOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	media := insertTestMedia(t, repo, 1, now)
	queued := now.Add(-2 * time.Hour)
	requireSearchState(t, repo, media, "en", queued, SearchPriorityMissing)
	requireSearchState(t, repo, media, "hr", queued, SearchPriorityMissing)
	leases, err := repo.LeaseDueSearches(ctx, now, 2, time.Minute)
	if err != nil || len(leases) != 2 {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	byLanguage := map[string]SearchLease{}
	for _, lease := range leases {
		byLanguage[lease.Language] = lease
	}

	retry := now.Add(6 * time.Hour)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: byLanguage["en"].JobID, Outcome: "no_result", NextAttemptAt: retry, AdvanceMissingAttempt: true, Priority: SearchPriorityMissing}); err != nil {
		t.Fatal(err)
	}
	if next, order := searchQueueOrder(t, repo, media, "en"); next != retry.UnixNano() || order != retry.UnixNano() {
		t.Fatalf("no-result completion next/order = %d/%d, want both %d", next, order, retry.UnixNano())
	}

	reset := now.Add(3 * time.Hour)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: byLanguage["hr"].JobID, Outcome: "throttled", NextAttemptAt: reset, PreserveQueueOrder: true}); err != nil {
		t.Fatal(err)
	}
	if next, order := searchQueueOrder(t, repo, media, "hr"); next != reset.UnixNano() || order != queued.UnixNano() {
		t.Fatalf("throttled completion next/order = %d/%d, want %d/%d", next, order, reset.UnixNano(), queued.UnixNano())
	}
}

func TestLeaseDueSearchesOrdersByQueuePositionNotDueTime(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	throttled := insertTestMedia(t, repo, 1, now)
	fresh := insertTestMedia(t, repo, 2, now)
	// The throttled search was queued first but became due again later (reset + jitter).
	requireSearchState(t, repo, throttled, "en", now.Add(-3*time.Hour), SearchPriorityMissing)
	leases, err := repo.LeaseDueSearches(ctx, now.Add(-3*time.Hour), 1, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: leases[0].JobID, Outcome: "throttled", NextAttemptAt: now.Add(-time.Minute), PreserveQueueOrder: true}); err != nil {
		t.Fatal(err)
	}
	requireSearchState(t, repo, fresh, "en", now.Add(-time.Hour), SearchPriorityMissing)

	leases, err = repo.LeaseDueSearches(ctx, now, 2, time.Minute)
	if err != nil || len(leases) != 2 {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	if leases[0].MediaID != throttled || leases[1].MediaID != fresh {
		t.Fatalf("lease order = %d, %d; want throttled %d before fresh %d", leases[0].MediaID, leases[1].MediaID, throttled, fresh)
	}
}

func TestLeaseDueSearchesRanksInstancesWithinClass(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	base := openTestRepository(t)
	insert := func(fileID int64, instance string) int64 {
		t.Helper()
		media := testMedia()
		media.EntityID = fileID
		media.Ref = domain.MediaRef{Instance: instance, Kind: domain.MediaEpisode, FileID: fileID}
		media.Fingerprint.FileID = fileID
		media.Fingerprint.Path = "/media/" + instance + ".mkv"
		id, _, err := base.UpsertMedia(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lowOld := insert(1, "sonarr-lq")
	highNew := insert(2, "sonarr-main")
	unranked := insert(3, "sonarr-other")
	lowImport := insert(4, "sonarr-lq")
	requireSearchState(t, base, lowOld, "en", now.Add(-3*time.Hour), SearchPriorityMissing)
	requireSearchState(t, base, highNew, "en", now.Add(-time.Hour), SearchPriorityMissing)
	requireSearchState(t, base, unranked, "en", now.Add(-4*time.Hour), SearchPriorityMissing)
	requireSearchState(t, base, lowImport, "en", now.Add(-time.Minute), SearchPriorityImport)

	repo := base.WithInstanceRanks(map[string]int{"sonarr-main": 20, "sonarr-lq": 10})
	leases, err := repo.LeaseDueSearches(ctx, now, 4, time.Minute)
	if err != nil || len(leases) != 4 {
		t.Fatalf("leases = %d, %v", len(leases), err)
	}
	got := []int64{leases[0].MediaID, leases[1].MediaID, leases[2].MediaID, leases[3].MediaID}
	// Class first (import beats every missing search), then rank, then queue position.
	want := []int64{lowImport, highNew, lowOld, unranked}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lease order = %v, want %v", got, want)
		}
	}
}

func TestLeaseDueSearchesExceptSkipsPausedRoutes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	insert := func(fileID int64, kind domain.MediaKind) int64 {
		t.Helper()
		media := testMedia()
		media.EntityID = fileID
		media.Ref = domain.MediaRef{Instance: "arr", Kind: kind, FileID: fileID}
		media.Fingerprint.FileID = fileID
		media.Fingerprint.Path = filepath.Join("/media", string(kind), fmt.Sprint(fileID)+".mkv")
		id, _, err := repo.UpsertMedia(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	movie := insert(1, domain.MediaMovie)
	episode := insert(2, domain.MediaEpisode)
	for _, id := range []int64{movie, episode} {
		requireSearchState(t, repo, id, "hr", now.Add(-time.Hour), SearchPriorityMissing)
		requireSearchState(t, repo, id, "en", now.Add(-time.Hour), SearchPriorityMissing)
	}

	leases, err := repo.LeaseDueSearchesExcept(ctx, now, 10, time.Minute, []RouteKey{{Language: "hr", Kind: domain.MediaMovie}, {Language: "en", Kind: domain.MediaEpisode}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, lease := range leases {
		got[fmt.Sprintf("%d/%s", lease.MediaID, lease.Language)] = true
	}
	want := map[string]bool{fmt.Sprintf("%d/en", movie): true, fmt.Sprintf("%d/hr", episode): true}
	if len(got) != len(want) || !got[fmt.Sprintf("%d/en", movie)] || !got[fmt.Sprintf("%d/hr", episode)] {
		t.Fatalf("leased routes = %v, want %v", got, want)
	}
}
