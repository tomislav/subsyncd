package pack

import "subsyncd/internal/domain"

type Limits struct {
	MaxCompressed int64
	MaxExpanded   int64
	MaxFiles      int
	MaxDepth      int
}

func DefaultLimits() Limits {
	return Limits{MaxCompressed: 20 << 20, MaxExpanded: 100 << 20, MaxFiles: 100, MaxDepth: 1}
}

type Member struct {
	SafeName          string
	NormalizedPath    string
	Checksum          string
	ByteSize          int64
	Season            int
	EpisodeFrom       int
	EpisodeTo         int
	AbsoluteFrom      int
	AbsoluteTo        int
	NormalizedTitle   string
	Forced            bool
	SelectionRule     string
	SelectionEvidence string
}

type Manifest struct {
	ProviderID string
	ResultID   string
	Language   domain.Language
	Checksum   string
	Candidate  domain.Candidate
	Members    []Member
}
