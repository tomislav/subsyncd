package pack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	// Two-digit milliseconds parse, but the timing cannot be rewritten; the
	// backwards cue plays as it is rather than losing the subtitle.
	body := "1\n00:00:05,00 --> 00:00:04,000\nA\n\n2\n00:00:07,000 --> 00:00:08,000\nB\n"
	if got := normalizedMember(t, "Show.S01E01.srt", body); got != body {
		t.Fatalf("normalized =\n%q\nwant it unchanged", got)
	}
	// When the cues also need reordering, the subtitle is still rejected.
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: "1\n00:00:05,00 --> 00:00:04,000\nA\n\n2\n00:00:01,000 --> 00:00:02,000\nB\n"}})
	var content *ContentError
	if !errors.As(err, &content) || !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("error = %v; want the out-of-order rejection", err)
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
	// The same overlap in SRT is two subtitles in one file.
	cues := []string{clock(0) + " --> " + clock(60), "Whole scene"}
	for second := 0; second < 60; second += 3 {
		cues = append(cues, clock(second)+" --> "+clock(second+2), "Line")
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
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "WEB/Show.S01E01.srt", body: validSRT},
		{name: "BluRay/Show.S01E01.srt", body: strings.Replace(validSRT, "Hello", "Other", 1)},
	})
	if err != nil || len(manifest.Members) != 2 {
		t.Fatalf("members = %+v, err = %v", manifest.Members, err)
	}
	if manifest.Members[1].SafeName != "BluRay - Show.S01E01.srt" {
		t.Fatalf("second member name = %q", manifest.Members[1].SafeName)
	}
}

func TestPrefixedDuplicateNameTooLongIsAContentRejection(t *testing.T) {
	name := strings.Repeat("x", 240) + ".srt"
	_, _, err := extractZIP(t, []zipEntry{
		{name: "Original encoding/" + name, body: validSRT},
		{name: "UTF-8 encoding/" + name, body: strings.Replace(validSRT, "Hello", "Other", 1)},
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
