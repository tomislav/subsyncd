package match

import "subsyncd/internal/domain"

// ReleaseAffinity scores how closely a subtitle file name's release matches the
// media file, using the release-group, source, edition, streaming-service and
// resolution weights of Evaluate. It only orders versions of a subtitle already
// known to be for the target; it says nothing about which episode a file is.
func ReleaseAffinity(media domain.Media, name string) int {
	releases := []Release{ParseRelease(name)}
	return boolPoints(releaseGroupMatches(releases, media.ReleaseGroup), 25) +
		boolPoints(releaseSourceMatches(releases, media.Source), 15) +
		boolPoints(editionMatches(releases, media.Edition), 10) +
		boolPoints(releaseFieldMatches(releases, media.StreamingService, func(r Release) string { return r.Service }), 5) +
		boolPoints(releaseFieldMatches(releases, media.Resolution, func(r Release) string { return r.Resolution }), 5)
}
