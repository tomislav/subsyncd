package domain

import "time"

type MediaKind string

type UnsupportedReason string

const (
	MediaMovie   MediaKind = "movie"
	MediaEpisode MediaKind = "episode"

	UnsupportedMultiEpisode UnsupportedReason = "unsupported_multi_episode"
)

type ExternalIDs struct {
	IMDb string `json:"imdb,omitempty"`
	TMDB int64  `json:"tmdb,omitempty"`
	TVDB int64  `json:"tvdb,omitempty"`
}

type MediaRef struct {
	Instance string    `json:"instance"`
	Kind     MediaKind `json:"kind"`
	FileID   int64     `json:"file_id"`
}

type MediaFingerprint struct {
	Path    string    `json:"path"`
	FileID  int64     `json:"file_id"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type Media struct {
	SeriesID        int64            `json:"series_id,omitempty"`
	EntityID        int64            `json:"entity_id"`
	Ref             MediaRef         `json:"ref"`
	Fingerprint     MediaFingerprint `json:"fingerprint"`
	Title           string           `json:"title"`
	EpisodeTitle    string           `json:"episode_title,omitempty"`
	AlternateTitles []string         `json:"alternate_titles,omitempty"`
	Year            int              `json:"year,omitempty"`
	Season          int              `json:"season,omitempty"`
	Episode         int              `json:"episode,omitempty"`
	AbsoluteEpisode int              `json:"absolute_episode,omitempty"`
	// EpisodeEnd is the last episode of a file containing several consecutive
	// episodes of one season; 0 for a single episode. CheckedUnsupportedEpisodeEnd
	// marks a legacy multi-episode file that was re-checked and stays unsupported.
	EpisodeEnd         int               `json:"episode_end,omitempty"`
	AbsoluteEpisodeEnd int               `json:"absolute_episode_end,omitempty"`
	ExternalIDs        ExternalIDs       `json:"external_ids"`
	OriginalFilename   string            `json:"original_filename,omitempty"`
	ReleaseName        string            `json:"release_name,omitempty"`
	ReleaseGroup       string            `json:"release_group,omitempty"`
	Source             string            `json:"source,omitempty"`
	Resolution         string            `json:"resolution,omitempty"`
	StreamingService   string            `json:"streaming_service,omitempty"`
	Edition            string            `json:"edition,omitempty"`
	Quality            string            `json:"quality,omitempty"`
	Duration           time.Duration     `json:"duration,omitempty"`
	UnsupportedReason  UnsupportedReason `json:"unsupported_reason,omitempty"`
}

// CheckedUnsupportedEpisodeEnd records that an unsupported multi-episode file
// was re-read from Sonarr and still cannot be supported, so it is not re-read.
const CheckedUnsupportedEpisodeEnd = -1

// IsEpisodeRange reports whether this is one file covering several consecutive
// episodes, which needs a subtitle covering the whole range.
func (m Media) IsEpisodeRange() bool {
	return m.Ref.Kind == MediaEpisode && m.EpisodeEnd > m.Episode && m.Episode > 0
}
