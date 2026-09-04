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
	Ref               MediaRef          `json:"ref"`
	Fingerprint       MediaFingerprint  `json:"fingerprint"`
	Title             string            `json:"title"`
	EpisodeTitle      string            `json:"episode_title,omitempty"`
	AlternateTitles   []string          `json:"alternate_titles,omitempty"`
	Year              int               `json:"year,omitempty"`
	Season            int               `json:"season,omitempty"`
	Episode           int               `json:"episode,omitempty"`
	AbsoluteEpisode   int               `json:"absolute_episode,omitempty"`
	ExternalIDs       ExternalIDs       `json:"external_ids"`
	OriginalFilename  string            `json:"original_filename,omitempty"`
	ReleaseName       string            `json:"release_name,omitempty"`
	ReleaseGroup      string            `json:"release_group,omitempty"`
	Source            string            `json:"source,omitempty"`
	Resolution        string            `json:"resolution,omitempty"`
	StreamingService  string            `json:"streaming_service,omitempty"`
	Edition           string            `json:"edition,omitempty"`
	Quality           string            `json:"quality,omitempty"`
	Duration          time.Duration     `json:"duration,omitempty"`
	UnsupportedReason UnsupportedReason `json:"unsupported_reason,omitempty"`
}
