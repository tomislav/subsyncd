// Package subtitleorder puts the cues of a subtitle in start-time order
// without reserializing it.
package subtitleorder

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUnsafe means the cues could not be matched to their records, so the text
// is left as it was rather than risking a broken reorder.
var ErrUnsafe = errors.New("subtitle cue boundaries cannot be reordered safely")

var blockSeparator = regexp.MustCompile(`\n[\t ]*\n`)
var timingLine = regexp.MustCompile(`(?m)^(?:[0-9]+:)?[0-9]{2}:[0-9]{2}[,.][0-9]{3}[\t ]+-->`)

// Reorder sorts the cue records of text by their start times, which are given
// in file order (one per cue, as parsed). Whole raw records move, so styling,
// cue identifiers, positioning and metadata survive; SRT cue numbers are
// renumbered. Line endings are normalized to "\n" and a leading byte order
// mark is kept. The text is returned unchanged when it is already in order.
func Reorder(text []byte, extension string, starts []time.Duration) ([]byte, error) {
	if sort.SliceIsSorted(starts, func(i, j int) bool { return starts[i] < starts[j] }) {
		return text, nil
	}
	content := strings.ReplaceAll(string(text), "\r\n", "\n")
	bom := ""
	if strings.HasPrefix(content, "\ufeff") {
		bom = "\ufeff"
		content = strings.TrimPrefix(content, bom)
	}
	// The final newline belongs to the file, not to the last record: keep it
	// out of the records so a moved last record does not carry it along.
	trailing := len(content) - len(strings.TrimRight(content, "\n"))
	content = content[:len(content)-trailing]
	extension = strings.ToLower(extension)
	ssa := extension == ".ass" || extension == ".ssa"
	var records []string
	separator := "\n\n"
	if ssa {
		records = strings.Split(content, "\n")
		separator = "\n"
	} else {
		records = blockSeparator.Split(content, -1)
	}
	type cue struct {
		start  time.Duration
		record string
	}
	var cues []cue
	var positions []int
	var eventFormat, cueFormat string
	for index, record := range records {
		isCue := timingLine.MatchString(record)
		if ssa {
			header, value, _ := strings.Cut(strings.TrimSpace(record), ":")
			if strings.TrimSpace(header) == "Format" {
				eventFormat = strings.TrimSpace(value)
			}
			isCue = strings.TrimSpace(header) == "Dialogue"
			if isCue {
				if len(cues) > 0 && eventFormat != cueFormat {
					return nil, ErrUnsafe
				}
				cueFormat = eventFormat
			}
		}
		if !isCue {
			continue
		}
		if len(cues) >= len(starts) {
			return nil, ErrUnsafe
		}
		cues = append(cues, cue{start: starts[len(cues)], record: record})
		positions = append(positions, index)
	}
	if len(cues) != len(starts) {
		return nil, ErrUnsafe
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
	return []byte(bom + strings.Join(records, separator) + strings.Repeat("\n", trailing)), nil
}
