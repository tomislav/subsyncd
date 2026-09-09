package workflow

import (
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
)

func TestExplicitProviderEvidenceRetainsLapsePolicyGates(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example", Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 123}, ReleaseGroup: "REWARD", Source: "bluray", Resolution: "1080p"}
	candidate := domain.Candidate{Kind: domain.MediaEpisode, Language: "en", Title: "Example", Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 123}, ReleaseNames: []string{"BluRayREWARD"}, ReleaseGroups: []string{"reward", "reward"}, Resolutions: []string{"720p", "1080p", "1080p"}}
	policy := DefaultLapsePolicy()
	score := match.Evaluate(media, candidate, "en")
	if score.Total != 85 || !canBypassLapse(media, candidate, score, false, policy) {
		t.Fatalf("first-install evidence score=%+v", score)
	}
	if canBypassLapse(media, candidate, score, true, policy) {
		t.Fatal("upgrade bypassed LAPSE")
	}
	always := policy
	always.Mode = "always"
	if canBypassLapse(media, candidate, score, false, always) {
		t.Fatal("always policy bypassed LAPSE")
	}
	packed := candidate
	packed.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	if canBypassLapse(media, packed, score, false, policy) {
		t.Fatal("pack bypassed LAPSE")
	}
	unknown := candidate
	unknown.ExternalIDs = domain.ExternalIDs{}
	unknownScore := match.Evaluate(media, unknown, "en")
	lowThreshold := policy
	lowThreshold.BypassScore = 1
	if canBypassLapse(media, unknown, unknownScore, false, lowThreshold) {
		t.Fatal("group/resolution evidence invented identity anchor")
	}
	noEpisode := candidate
	noEpisode.Season = 0
	noEpisode.Episode = 0
	if canBypassLapse(media, noEpisode, match.Evaluate(media, noEpisode, "en"), false, lowThreshold) {
		t.Fatal("group/resolution evidence invented episode anchor")
	}
}
