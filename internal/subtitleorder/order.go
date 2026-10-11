// Package subtitleorder puts the cues of a subtitle in start-time order and
// applies small timing repairs without reserializing it.
package subtitleorder

import (
	"errors"
	"fmt"
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

// timingFields captures an SRT or WebVTT timing line: optional hours, minutes,
// seconds, the decimal separator and milliseconds of both times, and the
// separator and settings around them.
var timingFields = regexp.MustCompile(`^((?:[0-9]+:)?)([0-9]{2}):([0-9]{2})([,.])([0-9]{3})([\t ]+-->[\t ]+)((?:[0-9]+:)?)([0-9]{2}):([0-9]{2})[,.]([0-9]{3})(.*)$`)

// Cue describes one cue of a subtitle in file order, as parsed.
type Cue struct {
	Start, End time.Duration
	// Retime writes Start and End into the cue's record.
	Retime bool
	// Drop removes the cue's record.
	Drop bool
}

// Reorder sorts the cue records of text by their start times, which are given
// in file order (one per cue, as parsed). Whole raw records move, so styling,
// cue identifiers, positioning and metadata survive; SRT cue numbers are
// renumbered. Line endings are normalized to "\n" and a leading byte order
// mark is kept. The text is returned unchanged when it is already in order.
func Reorder(text []byte, extension string, starts []time.Duration) ([]byte, error) {
	cues := make([]Cue, len(starts))
	for i, start := range starts {
		cues[i].Start = start
	}
	return repair(text, extension, cues, false)
}

// Repair is Reorder with per-cue edits: cues marked Drop are removed, cues
// marked Retime get their Start and End written into their timing, and SRT and
// WebVTT cues with an unclosed <i> get the missing </i> at the end of their
// text. The text is returned unchanged when nothing needs to change.
func Repair(text []byte, extension string, cues []Cue) ([]byte, error) {
	return repair(text, extension, cues, true)
}

func repair(text []byte, extension string, cues []Cue, closeItalics bool) ([]byte, error) {
	edits := false
	for _, cue := range cues {
		edits = edits || cue.Retime || cue.Drop
	}
	sorted := sort.SliceIsSorted(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	extension = strings.ToLower(extension)
	ssa := extension == ".ass" || extension == ".ssa"
	if sorted && !edits && (!closeItalics || ssa || !hasUnclosedItalics(string(text))) {
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
	var records []string
	separator := "\n\n"
	if ssa {
		records = strings.Split(content, "\n")
		separator = "\n"
	} else {
		records = blockSeparator.Split(content, -1)
	}
	type entry struct {
		cue    Cue
		record string
		format []string
	}
	var entries []entry
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
				if len(entries) > 0 && eventFormat != cueFormat {
					return nil, ErrUnsafe
				}
				cueFormat = eventFormat
			}
		}
		if !isCue {
			continue
		}
		if len(entries) >= len(cues) {
			return nil, ErrUnsafe
		}
		item := entry{cue: cues[len(entries)], record: record}
		if ssa {
			for _, field := range strings.Split(cueFormat, ",") {
				item.format = append(item.format, strings.ToLower(strings.TrimSpace(field)))
			}
		}
		entries = append(entries, item)
		positions = append(positions, index)
	}
	if len(entries) != len(cues) {
		return nil, ErrUnsafe
	}
	kept := entries[:0:0]
	for _, item := range entries {
		if !item.cue.Drop {
			kept = append(kept, item)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].cue.Start < kept[j].cue.Start })
	removed := map[int]bool{}
	for i, position := range positions {
		if i >= len(kept) {
			removed[position] = true
			continue
		}
		item := kept[i]
		record := item.record
		if item.cue.Retime {
			var err error
			if ssa {
				record, err = retimeDialogue(record, item.format, item.cue.Start, item.cue.End)
			} else {
				record, err = retimeBlock(record, item.cue.Start, item.cue.End)
			}
			if err != nil {
				return nil, err
			}
		}
		if closeItalics && !ssa {
			record = closeItalicTags(record)
		}
		if extension == ".srt" {
			first, rest, found := strings.Cut(record, "\n")
			if _, err := strconv.Atoi(strings.TrimSpace(first)); found && err == nil {
				record = strconv.Itoa(i+1) + "\n" + rest
			}
		}
		records[position] = record
	}
	if len(removed) > 0 {
		remaining := records[:0:0]
		for index, record := range records {
			if !removed[index] {
				remaining = append(remaining, record)
			}
		}
		records = remaining
	}
	return []byte(bom + strings.Join(records, separator) + strings.Repeat("\n", trailing)), nil
}

// retimeBlock rewrites the timing line of an SRT or WebVTT record, keeping its
// decimal separator, hour style and any cue settings after the end time.
func retimeBlock(record string, start, end time.Duration) (string, error) {
	lines := strings.Split(record, "\n")
	for i, line := range lines {
		match := timingFields.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		hours := match[1] != "" || match[7] != "" || start >= time.Hour || end >= time.Hour
		lines[i] = formatClock(start, match[4], hours) + match[6] + formatClock(end, match[4], hours) + match[11]
		return strings.Join(lines, "\n"), nil
	}
	return "", ErrUnsafe
}

func formatClock(value time.Duration, separator string, hours bool) string {
	milliseconds := value.Milliseconds()
	h, m, s, ms := milliseconds/3_600_000, milliseconds/60_000%60, milliseconds/1000%60, milliseconds%1000
	if hours {
		return fmt.Sprintf("%02d:%02d:%02d%s%03d", h, m, s, separator, ms)
	}
	return fmt.Sprintf("%02d:%02d%s%03d", m, s, separator, ms)
}

// retimeDialogue rewrites the Start and End fields of an ASS/SSA Dialogue line
// in the event Format's field order.
func retimeDialogue(record string, format []string, start, end time.Duration) (string, error) {
	prefix, value, found := strings.Cut(record, ":")
	startIndex, endIndex := -1, -1
	for i, field := range format {
		switch field {
		case "start":
			startIndex = i
		case "end":
			endIndex = i
		}
	}
	if !found || startIndex < 0 || endIndex < 0 {
		return "", ErrUnsafe
	}
	leading := value[:len(value)-len(strings.TrimLeft(value, " "))]
	fields := strings.SplitN(strings.TrimLeft(value, " "), ",", len(format))
	if len(fields) != len(format) {
		return "", ErrUnsafe
	}
	fields[startIndex], fields[endIndex] = formatASS(start), formatASS(end)
	return prefix + ":" + leading + strings.Join(fields, ","), nil
}

func formatASS(value time.Duration) string {
	centiseconds := value.Milliseconds() / 10
	return fmt.Sprintf("%d:%02d:%02d.%02d", centiseconds/360_000, centiseconds/6000%60, centiseconds/100%60, centiseconds%100)
}

// closeItalicTags appends the </i> a record's text is missing, so an unclosed
// tag cannot italicize every later line in players that carry it over.
func closeItalicTags(record string) string {
	lower := strings.ToLower(record)
	missing := strings.Count(lower, "<i>") - strings.Count(lower, "</i>")
	if missing <= 0 {
		return record
	}
	return record + strings.Repeat("</i>", missing)
}

func hasUnclosedItalics(text string) bool {
	for _, record := range blockSeparator.Split(strings.ReplaceAll(text, "\r\n", "\n"), -1) {
		if closeItalicTags(record) != record {
			return true
		}
	}
	return false
}
