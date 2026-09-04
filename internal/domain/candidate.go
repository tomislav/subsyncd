package domain

type PackScope string

const (
	PackSeason PackScope = "season"
	PackRange  PackScope = "range"
)

type PackMemberRef struct {
	ID              string `json:"id"`
	Filename        string `json:"filename"`
	DownloadRef     string `json:"download_ref"`
	Season          int    `json:"season,omitempty"`
	Episode         int    `json:"episode,omitempty"`
	AbsoluteEpisode int    `json:"absolute_episode,omitempty"`
}

type PackInfo struct {
	Scope               PackScope       `json:"scope"`
	Season              int             `json:"season,omitempty"`
	EpisodeFrom         int             `json:"episode_from,omitempty"`
	EpisodeTo           int             `json:"episode_to,omitempty"`
	AbsoluteEpisodeFrom int             `json:"absolute_episode_from,omitempty"`
	AbsoluteEpisodeTo   int             `json:"absolute_episode_to,omitempty"`
	DirectMembers       []PackMemberRef `json:"direct_members,omitempty"`
}

type Candidate struct {
	ProviderID      string      `json:"provider_id"`
	ResultID        string      `json:"result_id"`
	Language        Language    `json:"language"`
	Kind            MediaKind   `json:"kind"`
	Title           string      `json:"title"`
	Year            int         `json:"year,omitempty"`
	Season          int         `json:"season,omitempty"`
	Episode         int         `json:"episode,omitempty"`
	AbsoluteEpisode int         `json:"absolute_episode,omitempty"`
	ExternalIDs     ExternalIDs `json:"external_ids"`
	ReleaseNames    []string    `json:"release_names,omitempty"`
	ExactHash       bool        `json:"exact_hash"`
	Forced          bool        `json:"forced,omitempty"`
	HearingImpaired bool        `json:"hearing_impaired"`
	Rating          float64     `json:"rating"`
	Popularity      float64     `json:"popularity"`
	DownloadCount   int64       `json:"download_count"`
	DownloadRef     string      `json:"download_ref"`
	Pack            *PackInfo   `json:"pack,omitempty"`
}
