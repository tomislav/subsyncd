package pack

import "subsyncd/internal/domain"

// SubtitleRulesVersion and SelectionRulesVersion version the rules that
// reject a subtitle as invalid (extraction, decoding, repairs) and that select
// an archive's members. Candidate rejections for invalid_subtitle and
// pack_selection carry them, so bumping one after a rule change re-opens the
// rejections the old rules made. Rejections recorded before versioning count
// as version 1.
const (
	SubtitleRulesVersion  = 2
	SelectionRulesVersion = 3
)

type Limits struct {
	MaxCompressed int64
	MaxExpanded   int64
	// MaxFiles counts subtitle files only.
	MaxFiles int
	// MaxDepth is the number of folders a member may sit in.
	MaxDepth int
}

func DefaultLimits() Limits {
	return Limits{MaxCompressed: 20 << 20, MaxExpanded: 100 << 20, MaxFiles: 100, MaxDepth: 3}
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
	RuntimePack bool `json:",omitempty"`
	ProviderID  string
	ResultID    string
	Language    domain.Language
	Checksum    string
	ArchiveType string
	Candidate   domain.Candidate
	Members     []Member
	// SkippedMembers counts subtitle files the archive held that could not
	// be read; the readable ones are kept.
	SkippedMembers int `json:",omitempty"`
}
