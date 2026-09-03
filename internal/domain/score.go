package domain

type Contribution struct {
	Signal string `json:"signal"`
	Points int    `json:"points"`
	Reason string `json:"reason"`
}

type Score struct {
	Total           int            `json:"total"`
	Contributions   []Contribution `json:"contributions"`
	RejectedReasons []string       `json:"rejected_reasons,omitempty"`
}
