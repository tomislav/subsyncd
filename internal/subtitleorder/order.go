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

// timingFields captures an SRT or WebVTT timing line: optional hours, minutes,
// seconds, the decimal separator and milliseconds of both times, and the
// separator and settings around them.
var timingFields = regexp.MustCompile(`^((?:[0-9]+:)?)([0-9]{2}):([0-9]{2})([,.])([0-9]{3})([\t ]*-->[\t ]*)((?:[0-9]+:)?)([0-9]{2}):([0-9]{2})[,.]([0-9]{3})(.*)$`)

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
// renumbered. Line endings are normalized to "\n", blank lines inside an SRT
// cue are removed, a missing blank line between cues is added, and a leading
// byte order mark is kept. The text is returned unchanged when it is already in order.
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
	if extension == ".ass" || extension == ".ssa" {
		if sorted && !edits {
			return text, nil
		}
		return repairDialogue(text, cues)
	}
	content, bom, trailing := splitContent(text)
	lines := strings.Split(content, "\n")
	blocks := cueBlocks(lines, extension == ".vtt")
	if sorted && !edits && (!closeItalics || !hasUnclosedItalics(lines, blocks)) {
		return text, nil
	}
	if len(blocks) != len(cues) {
		return nil, ErrUnsafe
	}
	order := keptInStartOrder(cues)
	var output []string
	output = append(output, lines[:blocks[0].start]...)
	for i, block := range blocks {
		if i >= len(order) {
			break
		}
		if i > 0 {
			// The lines between two cues stay where they are; a missing blank
			// line between them is put back.
			gap := lines[blocks[i-1].end:block.start]
			if len(gap) == 0 {
				gap = []string{""}
			}
			output = append(output, gap...)
		}
		source := blocks[order[i]]
		record := cueLines(lines, source)
		cue := cues[order[i]]
		if cue.Retime {
			var err error
			if record, err = retimeBlock(record, source.timing-source.start, cue.Start, cue.End); err != nil {
				return nil, err
			}
		}
		if closeItalics {
			record[len(record)-1] += missingItalics(record)
		}
		if extension == ".srt" && source.timing > source.start {
			record[0] = strconv.Itoa(i + 1)
		}
		output = append(output, record...)
	}
	// Dropped cues give up the last positions; the lines after the last
	// remaining cue stay.
	last := blocks[len(blocks)-1]
	if len(order) > 0 {
		output = append(output, lines[last.end:]...)
	}
	return []byte(bom + strings.Join(output, "\n") + strings.Repeat("\n", trailing)), nil
}

// splitContent normalizes line endings and separates a leading byte order mark
// and the final newlines from the records. The final newline belongs to the
// file, not to the last record: keep it out of the records so a moved last
// record does not carry it along.
func splitContent(text []byte) (content, bom string, trailing int) {
	content = strings.ReplaceAll(string(text), "\r\n", "\n")
	if strings.HasPrefix(content, "\ufeff") {
		bom = "\ufeff"
		content = strings.TrimPrefix(content, bom)
	}
	trailing = len(content) - len(strings.TrimRight(content, "\n"))
	return content[:len(content)-trailing], bom, trailing
}

// keptInStartOrder returns the indexes of the cues that are not dropped, in
// start order.
func keptInStartOrder(cues []Cue) []int {
	var order []int
	for i, cue := range cues {
		if !cue.Drop {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return cues[order[i]].Start < cues[order[j]].Start })
	return order
}

// cueBlock locates one SRT or WebVTT cue in the file's lines: its first line
// (the index or identifier, or the timing line when there is none), its timing
// line, and the line after its last line of text.
type cueBlock struct {
	start, timing, end int
}

// cueBlocks finds the cues the way astisub reads them, so the records match
// the parsed cues one to one: every line containing "-->" starts a cue, with
// or without a blank line before it or spaces around the arrow. An SRT cue
// runs up to the next cue's index or timing line, so a blank line inside it
// does not move its text to another cue; a WebVTT cue ends at a blank line.
// The line before a timing line is the cue's index when it is a number (SRT)
// or follows a blank line (WebVTT).
func cueBlocks(lines []string, vtt bool) []cueBlock {
	first := 0
	if vtt {
		// astisub skips everything up to the WEBVTT header.
		for i, line := range lines {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "WEBVTT" {
				first = i + 1
				break
			}
		}
	}
	var blocks []cueBlock
	for i := first; i < len(lines); i++ {
		if !isTimingLine(lines[i], vtt) {
			continue
		}
		block := cueBlock{start: i, timing: i}
		previous := first - 1
		if len(blocks) > 0 {
			previous = blocks[len(blocks)-1].timing
		}
		if i-1 > previous && strings.TrimSpace(lines[i-1]) != "" {
			_, err := strconv.Atoi(strings.TrimSpace(lines[i-1]))
			if (!vtt && err == nil) || (vtt && i-2 > previous && strings.TrimSpace(lines[i-2]) == "") {
				block.start = i - 1
			}
		}
		blocks = append(blocks, block)
	}
	for i := range blocks {
		limit := len(lines)
		if i+1 < len(blocks) {
			limit = blocks[i+1].start
		}
		end := blocks[i].timing + 1
		for line := end; line < limit; line++ {
			if strings.TrimSpace(lines[line]) == "" {
				if vtt {
					break
				}
				continue
			}
			end = line + 1
		}
		blocks[i].end = end
	}
	return blocks
}

func isTimingLine(line string, vtt bool) bool {
	line = strings.TrimSpace(line)
	if vtt && (strings.HasPrefix(line, "NOTE ") || strings.HasPrefix(line, "Region: ") || strings.HasPrefix(line, "STYLE")) {
		return false
	}
	return strings.Contains(line, "-->")
}

// cueLines copies a cue's lines without the blank lines inside it, which
// players that split cues at blank lines would read as the end of the cue.
func cueLines(lines []string, block cueBlock) []string {
	var record []string
	for _, line := range lines[block.start:block.end] {
		if strings.TrimSpace(line) != "" {
			record = append(record, line)
		}
	}
	return record
}

// repairDialogue sorts, retimes and drops the Dialogue lines of an ASS/SSA
// subtitle; every other line stays where it is.
func repairDialogue(text []byte, cues []Cue) ([]byte, error) {
	content, bom, trailing := splitContent(text)
	records := strings.Split(content, "\n")
	type entry struct {
		cue    Cue
		record string
		format []string
	}
	var entries []entry
	var positions []int
	var eventFormat, cueFormat string
	for index, record := range records {
		header, value, _ := strings.Cut(strings.TrimSpace(record), ":")
		if strings.TrimSpace(header) == "Format" {
			eventFormat = strings.TrimSpace(value)
		}
		if strings.TrimSpace(header) != "Dialogue" {
			continue
		}
		if len(entries) > 0 && eventFormat != cueFormat {
			return nil, ErrUnsafe
		}
		cueFormat = eventFormat
		if len(entries) >= len(cues) {
			return nil, ErrUnsafe
		}
		item := entry{cue: cues[len(entries)], record: record}
		for _, field := range strings.Split(cueFormat, ",") {
			item.format = append(item.format, strings.ToLower(strings.TrimSpace(field)))
		}
		entries = append(entries, item)
		positions = append(positions, index)
	}
	if len(entries) != len(cues) {
		return nil, ErrUnsafe
	}
	order := keptInStartOrder(cues)
	removed := map[int]bool{}
	for i, position := range positions {
		if i >= len(order) {
			removed[position] = true
			continue
		}
		item := entries[order[i]]
		record := item.record
		if item.cue.Retime {
			var err error
			if record, err = retimeDialogue(record, item.format, item.cue.Start, item.cue.End); err != nil {
				return nil, err
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
	return []byte(bom + strings.Join(records, "\n") + strings.Repeat("\n", trailing)), nil
}

// retimeBlock rewrites the timing line, at index timing of a cue's lines,
// keeping its decimal separator, hour style, the spacing around the arrow and
// any cue settings after the end time.
func retimeBlock(record []string, timing int, start, end time.Duration) ([]string, error) {
	match := timingFields.FindStringSubmatch(record[timing])
	if match == nil {
		return nil, ErrUnsafe
	}
	hours := match[1] != "" || match[7] != "" || start >= time.Hour || end >= time.Hour
	record[timing] = formatClock(start, match[4], hours) + match[6] + formatClock(end, match[4], hours) + match[11]
	return record, nil
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

// missingItalics is the </i> a cue's lines are missing, so an unclosed tag
// cannot italicize every later line in players that carry it over.
func missingItalics(record []string) string {
	lower := strings.ToLower(strings.Join(record, "\n"))
	missing := strings.Count(lower, "<i>") - strings.Count(lower, "</i>")
	if missing <= 0 {
		return ""
	}
	return strings.Repeat("</i>", missing)
}

func hasUnclosedItalics(lines []string, blocks []cueBlock) bool {
	for _, block := range blocks {
		if missingItalics(lines[block.start:block.end]) != "" {
			return true
		}
	}
	return false
}
