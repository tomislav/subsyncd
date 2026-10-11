package subtitleorder

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReorderSortsRecordsForEachFormat(t *testing.T) {
	tests := map[string]struct {
		extension string
		in, want  string
		starts    []time.Duration
	}{
		"srt with byte order mark":      {".srt", "\ufeff1\n00:00:02,000 --> 00:00:03,000\nB\n\n2\n00:00:01,000 --> 00:00:02,000\nA\n", "\ufeff1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:02,000 --> 00:00:03,000\nB\n", []time.Duration{2 * time.Second, time.Second}},
		"vtt keeps its header":          {".vtt", "WEBVTT\n\n00:00:02.000 --> 00:00:03.000\nB\n\n00:00:01.000 --> 00:00:02.000\nA\n", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nA\n\n00:00:02.000 --> 00:00:03.000\nB\n", []time.Duration{2 * time.Second, time.Second}},
		"ass sorts dialogue lines only": {".ass", "[Events]\nFormat: Layer, Start, End, Text\nDialogue: 0,0:00:02.00,0:00:03.00,B\nDialogue: 0,0:00:01.00,0:00:02.00,A\n", "[Events]\nFormat: Layer, Start, End, Text\nDialogue: 0,0:00:01.00,0:00:02.00,A\nDialogue: 0,0:00:02.00,0:00:03.00,B\n", []time.Duration{2 * time.Second, time.Second}},
	}
	for name, test := range tests {
		got, err := Reorder([]byte(test.in), test.extension, test.starts)
		if err != nil || string(got) != test.want {
			t.Errorf("%s: Reorder() = %q, %v\nwant %q", name, got, err, test.want)
		}
	}
}

func TestReorderLeavesOrderedTextUntouchedAndRefusesMismatches(t *testing.T) {
	ordered := "1\r\n00:00:01,000 --> 00:00:02,000\r\nA\r\n"
	if got, err := Reorder([]byte(ordered), ".srt", []time.Duration{time.Second}); err != nil || string(got) != ordered {
		t.Fatalf("ordered text changed: %q, %v", got, err)
	}
	_, err := Reorder([]byte("1\n00:00:02,000 --> 00:00:03,000\nB\n"), ".srt", []time.Duration{2 * time.Second, time.Second})
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("error = %v; want ErrUnsafe when cues and records disagree", err)
	}
}

func TestRepairRetimesAndDropsInEachFormat(t *testing.T) {
	tests := map[string]struct {
		extension string
		in, want  string
		cues      []Cue
	}{
		"srt keeps cue settings and renumbers": {".srt",
			"1\n00:00:05,000 --> 00:00:04,000 X1:10\nA\n\n2\n00:00:06,000 --> 00:00:07,000\nB\n\n3\n00:00:08,000 --> 00:00:09,000\n\n",
			"1\n00:00:05,000 --> 00:00:05,900 X1:10\nA\n\n2\n00:00:06,000 --> 00:00:07,000\nB\n\n",
			[]Cue{{Start: 5 * time.Second, End: 5900 * time.Millisecond, Retime: true}, {Start: 6 * time.Second, End: 7 * time.Second}, {Start: 8 * time.Second, End: 9 * time.Second, Drop: true}}},
		"vtt keeps its short clock": {".vtt",
			"WEBVTT\n\n00:05.000 --> 00:04.000 line:90%\nA\n",
			"WEBVTT\n\n00:05.000 --> 00:06.000 line:90%\nA\n",
			[]Cue{{Start: 5 * time.Second, End: 6 * time.Second, Retime: true}}},
		"ass uses the event format's field order": {".ass",
			"[Events]\nFormat: Layer, Start, End, Style, Text\nDialogue: 0,0:00:05.00,0:00:04.00,Default,A, with comma\n",
			"[Events]\nFormat: Layer, Start, End, Style, Text\nDialogue: 0,0:00:05.00,0:00:06.50,Default,A, with comma\n",
			[]Cue{{Start: 5 * time.Second, End: 6500 * time.Millisecond, Retime: true}}},
	}
	for name, test := range tests {
		got, err := Repair([]byte(test.in), test.extension, test.cues)
		if err != nil || string(got) != test.want {
			t.Errorf("%s: Repair() = %q, %v\nwant %q", name, got, err, test.want)
		}
	}
}

func TestReorderDoesNotTouchItalics(t *testing.T) {
	// Reorder serves LAPSE output, which must stay byte for byte as written.
	in := "1\n00:00:02,000 --> 00:00:03,000\n<i>B\n\n2\n00:00:01,000 --> 00:00:02,000\nA\n"
	got, err := Reorder([]byte(in), ".srt", []time.Duration{2 * time.Second, time.Second})
	if err != nil || strings.Contains(string(got), "</i>") {
		t.Fatalf("Reorder() = %q, %v", got, err)
	}
}

func TestRepairFindsCuesTheWayTheParserDoes(t *testing.T) {
	tests := map[string]struct {
		in, want string
		cues     []Cue
	}{
		"missing blank line between cues with an unclosed italic": {
			"1\n00:00:01,000 --> 00:00:02,000\n<i>A\n2\n00:00:03,000 --> 00:00:04,000\nB\n",
			"1\n00:00:01,000 --> 00:00:02,000\n<i>A</i>\n\n2\n00:00:03,000 --> 00:00:04,000\nB\n",
			[]Cue{{Start: time.Second, End: 2 * time.Second}, {Start: 3 * time.Second, End: 4 * time.Second}}},
		"arrow without spaces and an empty cue": {
			"1\n00:00:01,000-->00:00:02,000\nA\n\n2\n00:00:03,000-->00:00:04,000\n\n3\n00:00:05,000-->00:00:06,000\nC\n",
			"1\n00:00:01,000-->00:00:02,000\nA\n\n2\n00:00:05,000-->00:00:06,000\nC\n",
			[]Cue{{Start: time.Second, End: 2 * time.Second}, {Start: 3 * time.Second, End: 4 * time.Second, Drop: true}, {Start: 5 * time.Second, End: 6 * time.Second}}},
		"blank line inside a reordered cue keeps its text": {
			"1\n00:00:05,000 --> 00:00:06,000\nB first\n\nB second\n\n2\n00:00:01,000 --> 00:00:02,000\nA\n",
			"1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:05,000 --> 00:00:06,000\nB first\nB second\n",
			[]Cue{{Start: 5 * time.Second, End: 6 * time.Second}, {Start: time.Second, End: 2 * time.Second}}},
		"retime without spaces around the arrow": {
			"1\n00:00:05,000-->00:00:04,000\nA\n",
			"1\n00:00:05,000-->00:00:06,000\nA\n",
			[]Cue{{Start: 5 * time.Second, End: 6 * time.Second, Retime: true}}},
	}
	for name, test := range tests {
		got, err := Repair([]byte(test.in), ".srt", test.cues)
		if err != nil || string(got) != test.want {
			t.Errorf("%s: Repair() = %q, %v\nwant %q", name, got, err, test.want)
		}
	}
}

func TestRepairWebVTTCueEndsAtBlankLine(t *testing.T) {
	in := "WEBVTT\n\nNOTE kept in place\n\nb\n00:00:05.000 --> 00:00:06.000\nB\n\n00:00:01.000 --> 00:00:02.000 line:90%\nA\n"
	want := "WEBVTT\n\nNOTE kept in place\n\n00:00:01.000 --> 00:00:02.000 line:90%\nA\n\nb\n00:00:05.000 --> 00:00:06.000\nB\n"
	got, err := Reorder([]byte(in), ".vtt", []time.Duration{5 * time.Second, time.Second})
	if err != nil || string(got) != want {
		t.Fatalf("Reorder() = %q, %v\nwant %q", got, err, want)
	}
}

func TestRepairTakesTheLineBeforeATimingLineAsTheIndex(t *testing.T) {
	// astisub drops the line before a timing line as the index whatever it
	// says, so "2." must not end up in the text of the cue before it.
	in := "1.\n00:00:05,000 --> 00:00:06,000\nSecond\n\n2.\n00:00:01,000 --> 00:00:02,000\nFirst\n"
	want := "1\n00:00:01,000 --> 00:00:02,000\nFirst\n\n2\n00:00:05,000 --> 00:00:06,000\nSecond\n"
	got, err := Reorder([]byte(in), ".srt", []time.Duration{5 * time.Second, time.Second})
	if err != nil || string(got) != want {
		t.Fatalf("Reorder() = %q, %v\nwant %q", got, err, want)
	}
	in = "1.\n00:00:01,000 --> 00:00:02,000\n<i>First\n\n2.\n00:00:05,000 --> 00:00:06,000\nSecond\n"
	want = "1\n00:00:01,000 --> 00:00:02,000\n<i>First</i>\n\n2\n00:00:05,000 --> 00:00:06,000\nSecond\n"
	got, err = Repair([]byte(in), ".srt", []Cue{{Start: time.Second, End: 2 * time.Second}, {Start: 5 * time.Second, End: 6 * time.Second}})
	if err != nil || string(got) != want {
		t.Fatalf("Repair() = %q, %v\nwant %q", got, err, want)
	}
}

func TestRepairRefusesToLoseANumericLastLineOfText(t *testing.T) {
	// "1984" directly before a timing line reads as the next cue's index, but
	// it is not the next cue number: rewriting would drop it.
	in := "1\n00:00:05,000 --> 00:00:06,000\nThe year was\n1984\n00:00:01,000 --> 00:00:02,000\nFirst\n"
	_, err := Reorder([]byte(in), ".srt", []time.Duration{5 * time.Second, time.Second})
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Reorder() error = %v; want ErrUnsafe", err)
	}
	ordered := "1\n00:00:01,000 --> 00:00:02,000\nThe year was\n1984\n00:00:05,000 --> 00:00:06,000\nLater\n"
	if got, err := Reorder([]byte(ordered), ".srt", []time.Duration{time.Second, 5 * time.Second}); err != nil || string(got) != ordered {
		t.Fatalf("ordered text changed: %q, %v", got, err)
	}
}
