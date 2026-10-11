package syncer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"subsyncd/internal/subtitleorder"
)

// Recut alignment can move cues across one another. Reorder complete raw records
// rather than reserializing through the subtitle library, which can lose styling,
// cue identifiers, positioning, or metadata. This only touches private LAPSE output.
func normalizeOutputOrder(path string) error {
	subtitles, payload, err := readOutput(path)
	if err != nil {
		return err
	}
	starts := make([]time.Duration, 0, len(subtitles.Items))
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt {
			return &InvalidOutputError{reason: "LAPSE output timestamps are invalid"}
		}
		starts = append(starts, item.StartAt)
	}
	reordered, err := subtitleorder.Reorder(payload, filepath.Ext(path), starts)
	if err != nil {
		return fmt.Errorf("LAPSE output cue boundaries cannot be normalized safely")
	}
	if bytes.Equal(reordered, payload) {
		return nil
	}
	normalized := reordered
	if len(normalized) > maximumSubtitleBytes {
		return &InvalidOutputError{reason: "LAPSE output size is invalid"}
	}
	if err := os.WriteFile(path, normalized, 0600); err != nil {
		return fmt.Errorf("write normalized LAPSE output: %w", err)
	}
	return nil
}
