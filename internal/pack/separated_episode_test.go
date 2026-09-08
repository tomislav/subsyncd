package pack

import (
	"subsyncd/internal/domain"
	"testing"
)

func TestSeparatedEpisodeCoordinates(t *testing.T) {
	media := domain.Media{Season: 4, Episode: 13}
	for _, name := range []string{"Show.S04.E13.Face.Off.srt", "Show.S04.E12-E13.srt", "Show.S04.E12-S04.E13.srt"} {
		t.Run(name, func(t *testing.T) {
			_, err := Select(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, media, false)
			if err != nil {
				t.Fatalf("valid dotted coordinates rejected: %v", err)
			}
		})
	}
	for _, name := range []string{"Show.S04.E12.srt", "Show.S04.E13.extra.S04.E12.srt", "Show.S04.E13-E12.srt", "Show.S04.E13-S05.E14.srt", "Show.S04.E13-E.srt", "Show.S04.E13-E14-E15.srt"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Select(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, media, false); err == nil {
				t.Fatal("unsafe dotted coordinates accepted")
			}
		})
	}
}

func TestDottedEpisodeRequiresCompleteToken(t *testing.T) {
	for _, name := range []string{"Show.S04.E13junk.srt", "Show.prefixS04.E13.srt", "Show.S04.E13000.srt"} {
		if _, err := Select(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, domain.Media{Season: 4, Episode: 13}, false); err == nil {
			t.Errorf("incomplete token accepted: %s", name)
		}
	}
}

func TestMalformedEpisodeTokensCannotUseGenericSingletonFallback(t *testing.T) {
	for _, name := range []string{"Show.S04E12junk.srt", "Show.S04.E12junk.srt", "Show.S04.E13000.srt"} {
		if _, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, domain.Media{Season: 4, Episode: 13}, false); err == nil {
			t.Errorf("malformed singleton accepted: %s", name)
		}
	}
}
