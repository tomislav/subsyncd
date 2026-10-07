package store

import (
	"context"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestMediaEpisodeRangeRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	media := testMedia()
	media.Season, media.Episode, media.EpisodeEnd = 6, 1, 2
	media.AbsoluteEpisode, media.AbsoluteEpisodeEnd = 66, 67
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMedia(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.EpisodeEnd != 2 || got.AbsoluteEpisodeEnd != 67 || !got.IsEpisodeRange() {
		t.Fatalf("range = E%d-E%d abs %d-%d, IsEpisodeRange=%t", got.Episode, got.EpisodeEnd, got.AbsoluteEpisode, got.AbsoluteEpisodeEnd, got.IsEpisodeRange())
	}

	// Media events write the same columns.
	event := MediaEventMutation{EventID: "range-import", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, At: time.Now()}
	event.Media.EpisodeEnd = 3
	if _, err := repo.ApplyMediaEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if got, err = repo.GetMedia(ctx, id); err != nil || got.EpisodeEnd != 3 {
		t.Fatalf("event range = %d, %v; want 3", got.EpisodeEnd, err)
	}
}

func TestSingleEpisodeAndCheckedMarkerAreNotRanges(t *testing.T) {
	for _, end := range []int{0, 1, domain.CheckedUnsupportedEpisodeEnd} {
		media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Season: 1, Episode: 1, EpisodeEnd: end}
		if media.IsEpisodeRange() {
			t.Errorf("EpisodeEnd %d is reported as a range", end)
		}
	}
	movie := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Episode: 1, EpisodeEnd: 2}
	if movie.IsEpisodeRange() {
		t.Error("a movie is reported as an episode range")
	}
}

func TestListLegacyMultiEpisodeMedia(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	insert := func(fileID int64, instance string, end int, reason domain.UnsupportedReason) {
		t.Helper()
		media := testMedia()
		media.EntityID, media.Ref.FileID, media.Fingerprint.FileID = fileID, fileID, fileID
		media.Ref.Instance = instance
		media.Fingerprint.Path = "/media/" + instance + "-" + time.Unix(fileID, 0).UTC().Format("150405") + ".mkv"
		media.EpisodeEnd, media.UnsupportedReason = end, reason
		if _, _, err := repo.UpsertMedia(ctx, media); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, "sonarr-main", 0, domain.UnsupportedMultiEpisode)                                   // legacy: listed
	insert(2, "sonarr-main", domain.CheckedUnsupportedEpisodeEnd, domain.UnsupportedMultiEpisode) // checked: not listed
	insert(3, "sonarr-main", 2, "")                                                               // supported range
	insert(4, "sonarr-main", 0, "")                                                               // single episode
	insert(5, "sonarr-lq", 0, domain.UnsupportedMultiEpisode)                                     // other instance

	refs, err := repo.ListLegacyMultiEpisodeMedia(ctx, "sonarr-main")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].FileID != 1 || refs[0].Instance != "sonarr-main" || refs[0].Kind != domain.MediaEpisode {
		t.Fatalf("legacy refs = %+v, want only file 1", refs)
	}
}

func TestRecheckImportReopensLegacyMultiEpisodeSearches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	legacy := testMedia()
	legacy.Season, legacy.Episode, legacy.UnsupportedReason = 6, 1, domain.UnsupportedMultiEpisode
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "legacy-import", Type: "import", EntityID: legacy.EntityID, Ref: legacy.Ref, Media: legacy, Languages: []domain.Language{"en"}, At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	id, _, err := repo.FindMedia(ctx, legacy.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := repo.GetSearchStatus(ctx, id, "en"); err != nil || status.State != "complete" {
		t.Fatalf("legacy search = %+v, %v; want complete", status, err)
	}

	ranged := legacy
	ranged.EpisodeEnd, ranged.UnsupportedReason = 2, ""
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "multi-episode-recheck", Type: "import", EntityID: ranged.EntityID, Ref: ranged.Ref, Media: ranged, Languages: []domain.Language{"en"}, At: now, Priority: SearchPriorityMissing}); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, id, "en")
	if err != nil || status.State != "pending" {
		t.Fatalf("rechecked search = %+v, %v; want pending", status, err)
	}
	got, err := repo.GetMedia(ctx, id)
	if err != nil || got.EpisodeEnd != 2 || got.UnsupportedReason != "" {
		t.Fatalf("rechecked media = E%d-E%d reason %q, %v", got.Episode, got.EpisodeEnd, got.UnsupportedReason, err)
	}
}
