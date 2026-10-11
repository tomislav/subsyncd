package domain

type SyncResult struct {
	Verdict    string  `json:"verdict"`
	Mode       string  `json:"mode"`
	Reference  string  `json:"reference"`
	OffsetMS   int64   `json:"offset_ms"`
	Ratio      float64 `json:"ratio"`
	Confidence float64 `json:"confidence"`
	// Sigma is how far LAPSE's answer stands out; its --confidence bar for
	// a solid verdict (default 8) is on this scale. Margin is its lead over
	// the next best answer.
	Sigma     float64 `json:"sigma,omitempty"`
	Margin    float64 `json:"margin,omitempty"`
	Agreement float64 `json:"agreement"`
	Coverage  float64 `json:"coverage"`
	Parts     int     `json:"parts"`
	Splits    int     `json:"splits"`
}
