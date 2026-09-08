package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var outputBlockSeparator = regexp.MustCompile(`\n[\t ]*\n`)
var outputTimingLine = regexp.MustCompile(`(?m)^(?:[0-9]+:)?[0-9]{2}:[0-9]{2}[,.][0-9]{3}[\t ]+-->`)

// Recut alignment can move cues across one another. Reorder complete raw records
// rather than reserializing through the subtitle library, which can lose styling,
// cue identifiers, positioning, or metadata. This only touches private LAPSE output.
func normalizeOutputOrder(path string) error {
	subtitles, payload, err := readOutput(path)
	if err != nil {
		return err
	}
	ordered := true
	previous := time.Duration(0)
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt {
			return &InvalidOutputError{reason: "LAPSE output timestamps are invalid"}
		}
		if item.StartAt < previous {
			ordered = false
		}
		previous = item.StartAt
	}
	if ordered {
		return nil
	}
	text := strings.ReplaceAll(string(payload), "\r\n", "\n")
	bom := ""
	if strings.HasPrefix(text, "\ufeff") {
		bom = "\ufeff"
		text = strings.TrimPrefix(text, bom)
	}
	extension := strings.ToLower(filepath.Ext(path))
	var records []string
	separator := "\n\n"
	if extension == ".ass" || extension == ".ssa" {
		records = strings.Split(text, "\n")
		separator = "\n"
	} else {
		records = outputBlockSeparator.Split(text, -1)
	}
	type cue struct {
		start  time.Duration
		record string
	}
	var cues []cue
	var positions []int
	var eventFormat, cueFormat string
	for index, record := range records {
		isCue := outputTimingLine.MatchString(record)
		if extension == ".ass" || extension == ".ssa" {
			trimmed := strings.TrimSpace(record)
			header, value, _ := strings.Cut(trimmed, ":")
			if strings.TrimSpace(header) == "Format" {
				eventFormat = strings.TrimSpace(value)
			}
			isCue = strings.TrimSpace(header) == "Dialogue"
			if isCue {
				if len(cues) > 0 && eventFormat != cueFormat {
					return fmt.Errorf("LAPSE output event formats cannot be normalized safely")
				}
				cueFormat = eventFormat
			}
		}
		if !isCue {
			continue
		}
		if len(cues) >= len(subtitles.Items) {
			return fmt.Errorf("LAPSE output cue boundaries cannot be normalized safely")
		}
		cues = append(cues, cue{start: subtitles.Items[len(cues)].StartAt, record: record})
		positions = append(positions, index)
	}
	if len(cues) != len(subtitles.Items) {
		return fmt.Errorf("LAPSE output cue boundaries cannot be normalized safely")
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
	for i, item := range cues {
		record := item.record
		if extension == ".srt" {
			first, rest, found := strings.Cut(record, "\n")
			if _, err := strconv.Atoi(strings.TrimSpace(first)); found && err == nil {
				record = strconv.Itoa(i+1) + "\n" + rest
			}
		}
		records[positions[i]] = record
	}
	normalized := []byte(bom + strings.Join(records, separator))
	if len(normalized) > maximumSubtitleBytes {
		return &InvalidOutputError{reason: "LAPSE output size is invalid"}
	}
	if err := os.WriteFile(path, normalized, 0600); err != nil {
		return fmt.Errorf("write normalized LAPSE output: %w", err)
	}
	return nil
}
