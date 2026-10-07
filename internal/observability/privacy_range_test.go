package observability

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestMediaTitleShowsEpisodeRanges(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Mad Men", EpisodeTitle: "The Doorway", Season: 6, Episode: 1, EpisodeEnd: 2}
	if got := MediaTitle(media); got != "Mad Men - S06E01-E02 - The Doorway" {
		t.Fatalf("MediaTitle = %q", got)
	}
	media.EpisodeEnd = domain.CheckedUnsupportedEpisodeEnd
	if got := MediaTitle(media); got != "Mad Men - S06E01 - The Doorway" {
		t.Fatalf("MediaTitle for a non-range = %q", got)
	}
}
