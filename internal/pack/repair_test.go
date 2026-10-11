package pack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/asticode/go-astisub"

	"subsyncd/internal/domain"
)

func normalizedMember(t *testing.T, name, body string) string {
	t.Helper()
	manifest, destination, err := extractZIP(t, []zipEntry{{name: name, body: body}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, manifest.Members[0].SafeName))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func srt(cues ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(cues); i += 2 {
		b.WriteString(strconv.Itoa(i/2+1) + "\n" + cues[i] + "\n" + cues[i+1] + "\n\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func TestRepairMovesAMistypedStartToItsPlace(t *testing.T) {
	// Mad Men S03E02 on Titlovi: cue 413 starts a minute early but ends in
	// sequence.
	body := srt(
		"00:33:58,000 --> 00:34:01,800", "William says",
		"00:34:01,900 --> 00:34:04,500", "or they move in",
		"00:33:04,600 --> 00:34:07,000", "Judy.",
		"00:34:08,900 --> 00:34:10,800", "He's probably right.",
	)
	got := normalizedMember(t, "Show.S03E02.srt", body)
	if !strings.Contains(got, "00:34:04,500 --> 00:34:07,000\nJudy.") || strings.Index(got, "or they move in") > strings.Index(got, "Judy.") {
		t.Fatalf("mistyped start not repaired in place:\n%s", got)
	}
}

func TestRepairSortsAMisplacedBlockWithoutRetiming(t *testing.T) {
	// The Martian (Extended) on Titlovi: a scene block stored after the next
	// scene. Its cues keep their own times and are sorted.
	body := srt(
		"01:20:57,728 --> 01:20:59,208", "Shaking a bit.",
		"01:21:00,230 --> 01:21:01,482", "Say again?",
		"01:17:34,692 --> 01:17:36,735", "Big precession.",
		"01:17:37,194 --> 01:17:38,153", "Is that OK?",
		"01:21:05,000 --> 01:21:07,000", "Next scene.",
	)
	got := normalizedMember(t, "Movie.srt", body)
	for _, timing := range []string{"01:17:34,692 --> 01:17:36,735", "01:17:37,194 --> 01:17:38,153"} {
		if !strings.Contains(got, timing) {
			t.Fatalf("block cue %s was retimed:\n%s", timing, got)
		}
	}
	if strings.Index(got, "Is that OK?") > strings.Index(got, "Shaking a bit.") {
		t.Fatalf("block not sorted:\n%s", got)
	}
}

func TestRepairEndsABackwardsCueBeforeTheNextOne(t *testing.T) {
	body := srt(
		"00:04:21,290 --> 00:04:20,590", "Don't analyse us.",
		"00:04:22,000 --> 00:04:24,000", "Next",
	)
	got := normalizedMember(t, "Show.S03E19.srt", body)
	if !strings.Contains(got, "00:04:21,290 --> 00:04:22,000") {
		t.Fatalf("backwards cue not ended at the next cue:\n%s", got)
	}
}

func TestRepairDropsEmptyAndRepeatedCues(t *testing.T) {
	body := srt(
		"00:00:01,000 --> 00:00:02,000", "Hello",
		"00:00:02,000 --> 00:00:03,000", "<i> </i>",
		"00:00:01,000 --> 00:00:02,000", "Hello",
		"00:00:04,000 --> 00:00:05,000", "World",
	)
	got := normalizedMember(t, "Show.S01E01.srt", body)
	want := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:04,000 --> 00:00:05,000\nWorld\n"
	if got != want {
		t.Fatalf("normalized =\n%q\nwant\n%q", got, want)
	}
}

func TestRepairClosesUnbalancedItalics(t *testing.T) {
	body := srt(
		"00:00:01,000 --> 00:00:02,000", "<i>Narrator",
		"00:00:03,000 --> 00:00:04,000", "Plain",
	)
	got := normalizedMember(t, "Show.S01E01.srt", body)
	if !strings.Contains(got, "<i>Narrator</i>\n") {
		t.Fatalf("italic tag not closed:\n%s", got)
	}
}

func TestRepairReadsCuesLikeTheParser(t *testing.T) {
	tests := map[string]struct{ body, want string }{
		"missing blank line between cues with an unclosed italic": {
			"1\n00:00:01,000 --> 00:00:02,000\n<i>Narrator\n2\n00:00:03,000 --> 00:00:04,000\nPlain\n",
			"1\n00:00:01,000 --> 00:00:02,000\n<i>Narrator</i>\n\n2\n00:00:03,000 --> 00:00:04,000\nPlain\n"},
		"arrow without spaces and an empty cue": {
			"1\n00:00:01,000-->00:00:02,000\nHello\n\n2\n00:00:02,000-->00:00:03,000\n\n3\n00:00:04,000-->00:00:05,000\nWorld\n",
			"1\n00:00:01,000-->00:00:02,000\nHello\n\n2\n00:00:04,000-->00:00:05,000\nWorld\n"},
		"blank line inside a misplaced cue": {
			"1\n00:00:05,000 --> 00:00:06,000\nLater\n\nstill later\n\n2\n00:00:01,000 --> 00:00:02,000\nFirst\n",
			"1\n00:00:01,000 --> 00:00:02,000\nFirst\n\n2\n00:00:05,000 --> 00:00:06,000\nLater\nstill later\n"},
	}
	for name, test := range tests {
		if got := normalizedMember(t, "Show.S01E01.srt", test.body); got != test.want {
			t.Errorf("%s: normalized =\n%q\nwant\n%q", name, got, test.want)
		}
	}
}

func TestUnsafeRepairKeepsAnOrderedSubtitleAsItIs(t *testing.T) {
	// "long ago" before the next timing line reads as that cue's index, so
	// closing the <i> cannot rewrite the records without losing it; the cues
	// already play in order, so the subtitle is kept as it is.
	body := "1\n00:00:01,000 --> 00:00:02,000\n<i>The year was\nlong ago\n00:00:05,000 --> 00:00:06,000\nLater\n"
	if got := normalizedMember(t, "Show.S01E01.srt", body); got != body {
		t.Fatalf("normalized =\n%q\nwant it unchanged", got)
	}
	// When the cues also need reordering, the subtitle is still rejected.
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: "1\n00:00:05,000 --> 00:00:06,000\nThe year was\nlong ago\n00:00:01,000 --> 00:00:02,000\nFirst\n"}})
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("error = %v; want the out-of-order rejection", err)
	}
}

func TestUnsafeRepairDoesNotKeepADroppedCueOutOfOrder(t *testing.T) {
	// Without indexes or blank lines, "Second" reads as the next cue's index,
	// so the first cue is empty and dropped; the cue left plays in order, but
	// the kept text would still start at 5s and then 1s.
	body := "00:00:05,000 --> 00:00:06,000\nSecond\n00:00:01,000 --> 00:00:02,000\nFirst\n"
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: body}})
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("error = %v; want the out-of-order rejection", err)
	}
}

func TestUnsafeRetimeIsRejected(t *testing.T) {
	// Two-digit milliseconds, colon milliseconds and no milliseconds parse,
	// but such a timing cannot be rewritten: a cue that ends before it starts
	// cannot be kept as it is, since install rejects it.
	for name, body := range map[string]string{
		"two-digit milliseconds": "1\n00:00:05,00 --> 00:00:04,00\nA\n\n2\n00:00:07,00 --> 00:00:08,00\nB\n",
		"colon milliseconds":     "1\n00:00:05:000 --> 00:00:04:000\n<i>A\n\n2\n00:00:07:000 --> 00:00:08:000\nB\n",
		"no milliseconds":        "1\n00:00:05 --> 00:00:04\nA\n\n2\n00:00:07 --> 00:00:08\nB\n",
	} {
		_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: body}})
		var content *ContentError
		if !errors.As(err, &content) {
			t.Errorf("%s: error = %v; want a content rejection", name, err)
		}
	}
}

func TestNonNumericIndexStaysOutOfTheText(t *testing.T) {
	body := "1.\n00:00:05,000 --> 00:00:06,000\nSecond\n\n2.\n00:00:01,000 --> 00:00:02,000\nFirst\n"
	want := "1\n00:00:01,000 --> 00:00:02,000\nFirst\n\n2\n00:00:05,000 --> 00:00:06,000\nSecond\n"
	if got := normalizedMember(t, "Show.S01E01.srt", body); got != want {
		t.Fatalf("normalized =\n%q\nwant\n%q", got, want)
	}
}

func TestCarriageReturnMicroDVDIsConverted(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{{name: "Movie.sub", body: "{1}{1}25.000\r{25}{50}Hello\r{60}{90}World\r"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Members) != 1 {
		t.Fatalf("members = %d; want the CR-only MicroDVD file converted", len(manifest.Members))
	}
}

func TestRepairRejectsTwoSubtitlesInOneFile(t *testing.T) {
	// The Big Bang Theory S01 pack on Titlovi: one file holds two episodes,
	// the second restarting at 00:01:59 after 00:19:43.
	var cues []string
	for second := 0; second < 600; second += 4 {
		cues = append(cues, clock(second)+" --> "+clock(second+3), "First episode")
	}
	for second := 2; second < 600; second += 4 {
		cues = append(cues, clock(second)+" --> "+clock(second+3), "Second episode")
	}
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E08.srt", body: srt(cues...)}})
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(err.Error(), "two subtitles") {
		t.Fatalf("error = %v; want the merged-file rejection", err)
	}
}

func clock(second int) string {
	return fmt.Sprintf("00:%02d:%02d,000", second/60, second%60)
}

func TestMicroDVDWithFrameRateIsConvertedToSRT(t *testing.T) {
	// Munich (2005) on Titlovi.
	body := "{1}{1}25.000\r\n{1964}{2012}Hej!\r\n{2026}{2096}/Šteta,|zatvoren je vrt.\r\n{2100}{2150}{y:i}Pjevaju\r\n"
	manifest, destination, err := extractZIP(t, []zipEntry{{name: "munich.sub", body: body}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Members[0].SafeName != "munich.sub.srt" {
		t.Fatalf("member name = %q, want munich.sub.srt", manifest.Members[0].SafeName)
	}
	got, _ := os.ReadFile(filepath.Join(destination, "munich.sub.srt"))
	want := "1\n00:01:18,560 --> 00:01:20,480\nHej!\n\n2\n00:01:21,040 --> 00:01:23,840\n<i>Šteta,</i>\nzatvoren je vrt.\n\n3\n00:01:24,000 --> 00:01:26,000\n<i>Pjevaju</i>\n"
	if string(got) != want {
		t.Fatalf("converted =\n%q\nwant\n%q", got, want)
	}
}

func TestMicroDVDWithoutFrameRateIsRejected(t *testing.T) {
	_, _, err := extractZIP(t, []zipEntry{{name: "Gods and Generals cd1 - hr.sub", body: "{575}{669}Ljudski život\n"}})
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(err.Error(), "frame rate") {
		t.Fatalf("error = %v; want a frame-rate rejection", err)
	}
}

func TestMicroDVDWithoutFrameRateBesideSRTIsIgnored(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Show.HDTV.XviD-LOL.srt", body: validSRT},
		{name: "Show.HDTV.XviD-LOL.sub", body: "{575}{669}Ljudski život\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Members) != 1 || manifest.SkippedMembers != 0 {
		t.Fatalf("members = %d skipped = %d; an unconvertible MicroDVD file is an extra file", len(manifest.Members), manifest.SkippedMembers)
	}
	selected, err := SelectSingleEpisode(manifest, domain.Candidate{}, domain.Media{Title: "Show", Season: 1, Episode: 2}, false)
	if err != nil || selected.SafeName != "Show.HDTV.XviD-LOL.srt" || selected.SelectionRule != "single_generic" {
		t.Fatalf("selected = %+v, %v; want the srt by single_generic", selected, err)
	}
}

func TestMicroDVDWithCommaFrameRateAndEndOfFileMarkIsConverted(t *testing.T) {
	out, extension, err := normalizeSubtitle([]byte("{1}{1}23,976\r\n{24}{48}Hej\r\n\x1a"), ".sub")
	if err != nil || extension != ".srt" || !strings.Contains(string(out), "00:00:01,001 --> 00:00:02,002\nHej") {
		t.Fatalf("normalizeSubtitle() = %q, %q, %v", out, extension, err)
	}
}

func TestBinaryVobSubIsIgnoredNotSkipped(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Movie.sub", body: "\x00\x00\x01\xbaVobSub binary"},
		{name: "Movie.idx", body: "# VobSub index file, v7"},
		{name: "Movie.srt", body: validSRT},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Members) != 1 || manifest.SkippedMembers != 0 {
		t.Fatalf("members = %d skipped = %d; a VobSub .sub is not a subtitle member", len(manifest.Members), manifest.SkippedMembers)
	}
}

func TestMicroDVDBesideSRTKeepsBoth(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Movie.sub", body: "{1}{1}25.000\n{25}{50}Hej\n"},
		{name: "Movie.srt", body: validSRT},
	})
	if err != nil || len(manifest.Members) != 2 {
		t.Fatalf("members = %+v, err = %v; want both subtitles", manifest.Members, err)
	}
}

func TestExtensionlessMicroDVDIsConverted(t *testing.T) {
	out, extension, err := normalizeSubtitle([]byte("{1}{1}25.000\n{25}{50}Hej\n"), "")
	if err != nil || extension != ".srt" || !strings.Contains(string(out), "00:00:01,000 --> 00:00:02,000\nHej") {
		t.Fatalf("normalizeSubtitle() = %q, %q, %v", out, extension, err)
	}
}

func TestLayeredASSIsNotTreatedAsMerged(t *testing.T) {
	// A sign shown for the whole scene beside the dialogue overlaps most of
	// the cue time; in ASS that is normal.
	body := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize\nStyle: Default,Arial,20\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"
	for second := 0; second < 60; second += 3 {
		body += fmt.Sprintf("Dialogue: 0,0:00:%02d.00,0:00:%02d.00,Default,,0,0,0,,Line %d\n", second, second+2, second)
	}
	body += "Dialogue: 1,0:00:00.00,0:01:00.00,Default,,0,0,0,,{\\an8}SIGN\n"
	if _, _, err := normalizeSubtitle([]byte(body), ".ass"); err != nil {
		t.Fatalf("layered ASS rejected: %v", err)
	}
	// Two SRT subtitles stitched into one file overlap all along.
	var cues []string
	for second := 0; second < 60; second += 3 {
		cues = append(cues, clock(second)+" --> "+clock(second+2), "Episode one")
	}
	for second := 1; second < 60; second += 3 {
		cues = append(cues, clock(second)+" --> "+clock(second+2), "Episode two")
	}
	if _, _, err := normalizeSubtitle([]byte(srt(cues...)), ".srt"); err == nil {
		t.Fatal("heavily overlapping SRT accepted")
	}
}

func TestSameSubtitleInTwoEncodingFoldersIsKeptOnce(t *testing.T) {
	// For All Mankind S02E03 on Titlovi ships "ANSI (CRLF_windows1250)/" and
	// "UTF/" copies of one subtitle.
	cp1250 := "1\r\n00:00:01,000 --> 00:00:02,000\r\n\x9eivjeli\r\n"
	utf8 := "1\n00:00:01,000 --> 00:00:02,000\nživjeli\n"
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "ANSI (CRLF_windows1250)/For All Mankind_s02e03.srt", body: cp1250},
		{name: "UTF/For All Mankind_s02e03.srt", body: utf8},
	})
	if err != nil || len(manifest.Members) != 1 || manifest.SkippedMembers != 0 {
		t.Fatalf("members = %+v, skipped = %d, err = %v; want one copy", manifest.Members, manifest.SkippedMembers, err)
	}
}

func TestDifferentSubtitlesWithOneNameKeepTheirFolders(t *testing.T) {
	web := zipEntry{name: "WEB/Show.S01E01.srt", body: validSRT}
	bluray := zipEntry{name: "BluRay/Show.S01E01.srt", body: strings.Replace(validSRT, "Hello", "Other", 1)}
	for _, entries := range [][]zipEntry{{web, bluray}, {bluray, web}} {
		manifest, _, err := extractZIP(t, entries)
		if err != nil || len(manifest.Members) != 2 {
			t.Fatalf("members = %+v, err = %v", manifest.Members, err)
		}
		names := memberNames(manifest)
		sort.Strings(names)
		if strings.Join(names, ",") != "Show.S01E01.srt,WEB - Show.S01E01.srt" {
			t.Fatalf("names = %v; want the same names in any member order", names)
		}
	}
}

func TestDifferentSubtitlesWithOneNameAndNoFolderAreNumbered(t *testing.T) {
	first := zipEntry{name: "Episode.srt", body: validSRT}
	second := zipEntry{name: "episode.srt", body: strings.Replace(validSRT, "Hello", "Other", 1)}
	taken := zipEntry{name: "Episode (2).srt", body: strings.Replace(validSRT, "Hello", "Third", 1)}
	copyOfFirst := zipEntry{name: "Copy/Episode.srt", body: validSRT}
	var want map[string]string
	for _, entries := range [][]zipEntry{{first, second, taken, copyOfFirst}, {copyOfFirst, taken, second, first}} {
		manifest, destination, err := extractZIP(t, entries)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, member := range manifest.Members {
			content, err := os.ReadFile(filepath.Join(destination, member.SafeName))
			if err != nil {
				t.Fatal(err)
			}
			got[member.SafeName] = string(content)
		}
		if len(got) != 3 {
			t.Fatalf("members = %v; want the identical copy kept once", memberNames(manifest))
		}
		if _, ok := got["episode (3).srt"]; !ok {
			t.Fatalf("members = %v; want the clash numbered past the taken name", memberNames(manifest))
		}
		if want == nil {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("names depend on member order: %v versus %v", got, want)
		}
	}
}

func TestPrefixedDuplicateNameTooLongIsNumbered(t *testing.T) {
	name := strings.Repeat("x", 240) + ".srt"
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Original encoding/" + name, body: validSRT},
		{name: "UTF-8 encoding/" + name, body: strings.Replace(validSRT, "Hello", "Other", 1)},
	})
	if err != nil || len(manifest.Members) != 2 {
		t.Fatalf("members = %v, err = %v", memberNames(manifest), err)
	}
	// With no room for even a number, the archive is a content rejection.
	name = strings.Repeat("x", 251) + ".srt"
	_, _, err = extractZIP(t, []zipEntry{
		{name: name, body: validSRT},
		{name: "Other/" + name, body: strings.Replace(validSRT, "Hello", "Other", 1)},
	})
	var content *ContentError
	if !errors.As(err, &content) {
		t.Fatalf("error = %T %v; want a content rejection", err, err)
	}
}

func TestUnreadableAndReadableMembersWithOneNameKeepTheReadableOne(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Movie.srt", body: "\x00\x01binary"},
		{name: "movie.srt", body: validSRT},
	})
	if err != nil || len(manifest.Members) != 1 || manifest.SkippedMembers != 1 {
		t.Fatalf("members = %+v skipped = %d err = %v", manifest.Members, manifest.SkippedMembers, err)
	}
}

func TestRepairSortsALongCaptionInsteadOfShorteningItsStart(t *testing.T) {
	// Starting 50 s early but ending in sequence: moving its start to the end
	// of the leader would leave one second for a long caption, so it is only
	// sorted.
	caption := "This is a long caption that needs several seconds of reading time on screen."
	body := srt(
		"00:10:02,000 --> 00:10:05,000", "First",
		"00:10:06,000 --> 00:10:09,000", "Second",
		"00:09:20,000 --> 00:10:10,000", caption,
		"00:10:12,000 --> 00:10:14,000", "Third",
	)
	got := normalizedMember(t, "Show.S01E01.srt", body)
	if !strings.Contains(got, "00:09:20,000 --> 00:10:10,000\n"+caption) || strings.Index(got, caption) > strings.Index(got, "First") {
		t.Fatalf("long caption retimed or not sorted:\n%s", got)
	}
}

func TestASSIsNeitherStartRetimedNorDeduplicated(t *testing.T) {
	body := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize\nStyle: Default,Arial,20\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:10:00.00,0:10:05.00,Default,,0,0,0,,First\n" +
		"Dialogue: 0,0:10:06.00,0:10:09.00,Default,,0,0,0,,Second\n" +
		"Dialogue: 0,0:09:50.00,0:10:12.00,Default,,0,0,0,,Sign\n" +
		"Dialogue: 1,0:10:12.00,0:10:14.00,Default,,0,0,0,,Glow\n" +
		"Dialogue: 0,0:10:12.00,0:10:14.00,Default,,0,0,0,,Glow\n"
	items, err := astisub.ReadFromSSAWithOptions(strings.NewReader(body), astisub.SSAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cues, err := planRepairs(items.Items, ".ass")
	if err != nil {
		t.Fatal(err)
	}
	for i, cue := range cues {
		if cue.Drop || cue.Retime {
			t.Fatalf("ASS cue %d dropped or retimed: %+v", i, cue)
		}
	}
}

func TestRepairEndsAMistypedLongCueAtTheNextOne(t *testing.T) {
	body := srt(
		"00:12:31,000 --> 00:22:33,000", "Short line.",
		"00:12:34,000 --> 00:12:36,000", "Next",
		"00:12:37,000 --> 00:12:39,000", "And on",
	)
	got := normalizedMember(t, "Show.S01E01.srt", body)
	if !strings.Contains(got, "00:12:31,000 --> 00:12:34,000\nShort line.") {
		t.Fatalf("mistyped end not repaired:\n%s", got)
	}
	// A song may hold one cue over the dialogue; it is kept as it is, and so
	// is a long cue that ends before the next one starts.
	body = srt(
		"00:12:31,000 --> 00:13:10,000", "♪ A song plays ♪",
		"00:12:34,000 --> 00:12:36,000", "Next",
		"00:12:37,000 --> 00:12:39,000", "And on",
		"00:13:20,000 --> 00:13:45,000", "Long quiet line.",
		"00:13:46,000 --> 00:13:48,000", "After",
	)
	got = normalizedMember(t, "Show.S01E01.srt", body)
	for _, timing := range []string{"00:12:31,000 --> 00:13:10,000", "00:13:20,000 --> 00:13:45,000"} {
		if !strings.Contains(got, timing) {
			t.Fatalf("cue %s was retimed:\n%s", timing, got)
		}
	}
}

func TestMicroDVDLowercaseItalicAppliesToItsLineOnly(t *testing.T) {
	cases := map[string]string{
		"{y:i}Sung line|Spoken line": "<i>Sung line</i>\nSpoken line",
		"Spoken line|{y:i}Sung line": "Spoken line\n<i>Sung line</i>",
		"{Y:i}Both|lines":            "<i>Both</i>\n<i>lines</i>",
		"{Y:b,i}Bold italic|too":     "<i>Bold italic</i>\n<i>too</i>",
		"{y:b}Bold only":             "Bold only",
	}
	for input, want := range cases {
		if got := microDVDText(input); got != want {
			t.Errorf("microDVDText(%q) = %q, want %q", input, got, want)
		}
	}
}
