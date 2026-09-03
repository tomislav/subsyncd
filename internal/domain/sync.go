package domain

type SyncResult struct {
	Verdict    string  `json:"verdict"`
	Mode       string  `json:"mode"`
	Reference  string  `json:"reference"`
	OffsetMS   int64   `json:"offset_ms"`
	Ratio      float64 `json:"ratio"`
	Confidence float64 `json:"confidence"`
	Agreement  float64 `json:"agreement"`
	Coverage   float64 `json:"coverage"`
	Parts      int     `json:"parts"`
	Splits     int     `json:"splits"`
}
