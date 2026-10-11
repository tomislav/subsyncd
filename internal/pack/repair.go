package pack

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"

	"subsyncd/internal/subtitleorder"
)

// typoDisorder is how far back a cue must start before an earlier one to be
// considered for a start-time typo repair.
const typoDisorder = 10 * time.Second

// mistypedEndDuration is how long a cue must last before its end, when it
// also runs more than typoDisorder past the next cue's start, is a typo.
const mistypedEndDuration = 15 * time.Second

// maximumMergedOverlap is the share of a subtitle's cue time that may be shown
// together with another cue. Real subtitles stay near 0% (1.5% at most in a
// sample of 89 Titlovi files); two episodes stitched into one file reach 50%.
const maximumMergedOverlap = 0.25

// planRepairs decides, for cues in file order, which to drop and which to
// retime before the subtitle is put in start order:
//   - cues with no text, and in SRT and WebVTT exact repeats of an earlier
//     cue, are dropped (ASS/SSA layers legitimately repeat a line);
//   - in SRT and WebVTT, a cue that starts well before the furthest start so
//     far while its end still continues from there has a mistyped start, which
//     moves to the end of the cue with that furthest start (00:33:04 ->
//     00:34:07 between 00:34:01 and 00:34:08); a cue whose end is also early
//     belongs to a misplaced block, and one the move would leave shorter than
//     its reading time is a long caption, so both are only sorted;
//   - a cue that ends before it starts ends after its reading time, or at the
//     next cue when that comes sooner;
//   - in SRT and WebVTT, a cue that ends more than 10 s after the next cue
//     starts and lasts over three times its reading time and over 15 s has a
//     mistyped end, which moves to the next cue's start (or its reading time,
//     when that is later). Song lyrics (♪, ♫ or #) may run that long and are
//     kept.
//
// Cues that are simply out of order are left to sorting. A subtitle whose cues
// overlap too much is two subtitles in one file and is rejected (SRT and
// WebVTT only).
func planRepairs(items []*astisub.Item, extension string) ([]subtitleorder.Cue, error) {
	// ASS/SSA run signs, typesetting and karaoke layers alongside dialogue, so
	// repeated and overlapping lines are normal there.
	ass := extension == ".ass" || extension == ".ssa"
	cues := make([]subtitleorder.Cue, len(items))
	texts := make([]string, len(items))
	seen := map[string]bool{}
	// latest is the furthest start so far and leader the cue that set it: the
	// main sequence a mistyped cue's end still continues.
	leader := -1
	var latest time.Duration
	for i, item := range items {
		cues[i] = subtitleorder.Cue{Start: item.StartAt, End: item.EndAt}
		texts[i] = strings.TrimSpace(item.String())
		key := fmt.Sprintf("%d|%d|%s", item.StartAt, item.EndAt, texts[i])
		if texts[i] == "" || !ass && seen[key] {
			cues[i].Drop = true
			continue
		}
		seen[key] = true
		if !ass && leader >= 0 && item.StartAt < latest-typoDisorder && item.EndAt >= latest && item.EndAt <= latest+typoDisorder && cues[leader].End < item.EndAt && item.EndAt-cues[leader].End >= readingTime(texts[i]) {
			cues[i].Start, cues[i].Retime = cues[leader].End, true
		}
		if leader < 0 || cues[i].Start >= latest {
			latest, leader = cues[i].Start, i
		}
	}
	starts := make([]time.Duration, 0, len(cues))
	for _, cue := range cues {
		if !cue.Drop {
			starts = append(starts, cue.Start)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	nextStart := func(start time.Duration) (time.Duration, bool) {
		next := sort.Search(len(starts), func(j int) bool { return starts[j] > start })
		if next == len(starts) {
			return 0, false
		}
		return starts[next], true
	}
	for i := range cues {
		if cues[i].Drop {
			continue
		}
		reading := readingTime(texts[i])
		if cues[i].End < cues[i].Start {
			end := cues[i].Start + reading
			if next, ok := nextStart(cues[i].Start); ok && next < end {
				end = max(next, cues[i].Start+500*time.Millisecond)
			}
			cues[i].End, cues[i].Retime = end, true
			continue
		}
		duration := cues[i].End - cues[i].Start
		if next, ok := nextStart(cues[i].Start); ok && !ass && cues[i].End > next+typoDisorder && duration > 3*reading && duration > mistypedEndDuration && !strings.ContainsAny(texts[i], "♪♫#") {
			cues[i].End, cues[i].Retime = max(next, cues[i].Start+reading), true
		}
	}
	if ass {
		return cues, nil
	}
	if overlap := overlapShare(cues); overlap > maximumMergedOverlap {
		return nil, fmt.Errorf("subtitle cues overlap for %.0f%% of their time, like two subtitles in one file", overlap*100)
	}
	return cues, nil
}

// readingTime is how long a cue's text needs on screen: about 20 characters
// a second after a one-second base, from one to seven seconds.
func readingTime(text string) time.Duration {
	reading := time.Second + time.Duration(utf8.RuneCountInString(strings.Join(strings.Fields(text), " ")))*50*time.Millisecond
	return min(reading, 7*time.Second)
}

// overlapShare is the share of the time covered by any cue that is covered by
// two or more cues at once.
func overlapShare(cues []subtitleorder.Cue) float64 {
	type event struct {
		at    time.Duration
		delta int
	}
	var events []event
	for _, cue := range cues {
		if !cue.Drop && cue.End > cue.Start {
			events = append(events, event{cue.Start, 1}, event{cue.End, -1})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at != events[j].at {
			return events[i].at < events[j].at
		}
		return events[i].delta < events[j].delta
	})
	var covered, overlapped time.Duration
	depth := 0
	for i, current := range events {
		if i > 0 && depth > 0 {
			span := current.at - events[i-1].at
			covered += span
			if depth > 1 {
				overlapped += span
			}
		}
		depth += current.delta
	}
	if covered == 0 {
		return 0
	}
	return float64(overlapped) / float64(covered)
}

// microDVDLine is one MicroDVD cue: {start frame}{end frame}text.
var microDVDLine = regexp.MustCompile(`^\{([0-9]+)\}\{([0-9]*)\}(.*)$`)

// microDVDStyle matches MicroDVD formatting codes such as {y:i} or {c:$0000ff}.
var microDVDStyle = regexp.MustCompile(`\{[A-Za-z]:[^}]*\}`)

// isMicroDVD reports whether text is a MicroDVD subtitle rather than, for
// example, a binary VobSub .sub: its first non-empty line is a frame cue.
func isMicroDVD(text []byte) bool {
	for _, line := range strings.Split(string(text), "\n") {
		if line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")); line != "" {
			return microDVDLine.MatchString(line)
		}
	}
	return false
}

// microDVDToSRT converts a MicroDVD subtitle that states its frame rate in a
// {1}{1}25.000 header (or {0}{0}, also with a decimal comma) to SRT. Without
// that header frames cannot be turned into times, so the file is rejected. A
// DOS end-of-file mark (Ctrl-Z) at the end is ignored.
func microDVDToSRT(text []byte) ([]byte, error) {
	var fps float64
	var output strings.Builder
	number := 0
	for _, raw := range strings.Split(strings.TrimRight(string(text), "\x1a \t\r\n"), "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
		if line == "" {
			continue
		}
		match := microDVDLine.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("MicroDVD line is malformed")
		}
		start, _ := strconv.Atoi(match[1])
		end, _ := strconv.Atoi(match[2])
		if fps == 0 && start <= 1 && end <= 1 {
			value, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(match[3]), ",", "."), 64)
			if err != nil || value < 10 || value > 120 {
				return nil, fmt.Errorf("MicroDVD frame rate header is invalid")
			}
			fps = value
			continue
		}
		if fps == 0 {
			return nil, fmt.Errorf("MicroDVD subtitle does not state its frame rate")
		}
		if match[2] == "" {
			end = start + int(fps*2)
		}
		number++
		fmt.Fprintf(&output, "%d\n%s --> %s\n%s\n\n", number, srtClock(start, fps), srtClock(end, fps), microDVDText(match[3]))
	}
	if number == 0 {
		return nil, fmt.Errorf("MicroDVD subtitle has no cues")
	}
	return []byte(strings.TrimRight(output.String(), "\n") + "\n"), nil
}

func srtClock(frame int, fps float64) string {
	milliseconds := int64(float64(frame) / fps * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", milliseconds/3_600_000, milliseconds/60_000%60, milliseconds/1000%60, milliseconds%1000)
}

// microDVDYStyle matches a MicroDVD font style code: lowercase {y:i} applies
// to its own line, uppercase {Y:i} to every line of the cue. Its value lists
// styles such as "i", "b" or "i,b".
var microDVDYStyle = regexp.MustCompile(`\{([yY]):([^}]*)\}`)

// microDVDText turns MicroDVD lines into SRT text: '|' separates lines, a
// leading '/' or {y:i} marks a line italic and {Y:i} the whole cue, and other
// formatting codes are dropped.
func microDVDText(text string) string {
	italic := func(line string, letter string) bool {
		for _, match := range microDVDYStyle.FindAllStringSubmatch(line, -1) {
			if match[1] == letter && strings.ContainsRune(strings.ToLower(match[2]), 'i') {
				return true
			}
		}
		return false
	}
	italicAll := italic(text, "Y")
	lines := strings.Split(text, "|")
	for i, line := range lines {
		italicLine := italicAll || italic(line, "y")
		line = strings.TrimSpace(microDVDStyle.ReplaceAllString(line, ""))
		if strings.HasPrefix(line, "/") || italicLine {
			line = "<i>" + strings.TrimSpace(strings.TrimPrefix(line, "/")) + "</i>"
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
