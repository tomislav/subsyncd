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
