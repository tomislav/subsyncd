package pack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func extractZIP(t *testing.T, entries []zipEntry) (Manifest, string, error) {
	t.Helper()
	payload := zipPayload(t, entries)
	destination := filepath.Join(t.TempDir(), "published")
	manifest, err := Extract(context.Background(), domain.Candidate{ProviderID: "titlovi", ResultID: "pack", Language: "hr"}, bytes.NewReader(payload), int64(len(payload)), destination, testLimits())
	return manifest, destination, err
}

func memberNames(manifest Manifest) []string {
	names := make([]string, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		names = append(names, member.SafeName)
	}
	return names
}

func TestExtractIgnoresMacOSMetadataEntries(t *testing.T) {
	appleDouble := "\x00\x05\x16\x07\x00\x02\x00\x00Mac OS X        "
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "The.Thing.1982.srt", body: validSRT},
		{name: "__MACOSX/._The.Thing.1982.srt", body: appleDouble},
		{name: "._Other.srt", body: appleDouble},
	})
	if err != nil || strings.Join(memberNames(manifest), ",") != "The.Thing.1982.srt" {
		t.Fatalf("members = %v, err = %v; want only the real subtitle", memberNames(manifest), err)
	}
}

func TestExtractSkipsUnreadableMembersAndKeepsTheRest(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{
		{name: "Show.S01E01.srt", body: validSRT},
		{name: "Show.S01E02.srt", body: "\x00\x01binary"},
		{name: "Show.S01E03.srt", body: "not a subtitle at all"},
		{name: "Show.S01E04.srt", body: strings.ReplaceAll(validSRT, "Hello", "Four")},
	})
	if err != nil || strings.Join(memberNames(manifest), ",") != "Show.S01E01.srt,Show.S01E04.srt" {
		t.Fatalf("members = %v, err = %v; want the two readable episodes", memberNames(manifest), err)
	}
	if manifest.SkippedMembers != 2 {
		t.Fatalf("skipped = %d, want 2", manifest.SkippedMembers)
	}
}

func TestExtractStillRejectsAnArchiveWithNoReadableSubtitle(t *testing.T) {
	_, destination, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: "\x00\x01binary"}, {name: "Show.S01E02.srt", body: "garbage"}})
	var content *ContentError
	if !errors.As(err, &content) {
		t.Fatalf("error = %T %v; want a content rejection", err, err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination was published: %v", statErr)
	}
}

func TestExtractSortsSlightlyOutOfOrderCues(t *testing.T) {
	disordered := "1\n00:00:06,390 --> 00:00:08,000\nSecond\n\n2\n00:00:06,310 --> 00:00:07,000\nFirst\n\n3\n00:00:09,000 --> 00:00:10,000\nThird\n"
	manifest, destination, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: disordered}})
	if err != nil || len(manifest.Members) != 1 {
		t.Fatalf("members = %v, err = %v", memberNames(manifest), err)
	}
	content, err := os.ReadFile(filepath.Join(destination, manifest.Members[0].SafeName))
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:06,310 --> 00:00:07,000\nFirst\n\n2\n00:00:06,390 --> 00:00:08,000\nSecond\n\n3\n00:00:09,000 --> 00:00:10,000\nThird\n"
	if string(content) != want {
		t.Fatalf("normalized subtitle =\n%q\nwant\n%q", content, want)
	}
}

func TestExtractStillRejectsNegativeCueTimes(t *testing.T) {
	negative := "1\n-00:00:05,000 --> 00:00:04,000\nBefore the film\n"
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: negative}})
	var content *ContentError
	if !errors.As(err, &content) {
		t.Fatalf("error = %T %v; want a content rejection", err, err)
	}
}

func TestExtractRepairsACueThatEndsBeforeItStarts(t *testing.T) {
	// Malcolm in the Middle S03E19 on Titlovi: 00:04:21,290 --> 00:04:20,590.
	backwards := "1\n00:00:05,000 --> 00:00:04,000\nEnds before it starts\n\n2\n00:00:20,000 --> 00:00:21,000\nNext\n"
	manifest, destination, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: backwards}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, manifest.Members[0].SafeName))
	if err != nil {
		t.Fatal(err)
	}
	// One second plus 50 ms for each of the 21 characters.
	want := "1\n00:00:05,000 --> 00:00:07,050\nEnds before it starts\n\n2\n00:00:20,000 --> 00:00:21,000\nNext\n"
	if string(content) != want {
		t.Fatalf("normalized subtitle =\n%q\nwant\n%q", content, want)
	}
}

func TestExtractKeepsASubtitleWithAMisplacedCue(t *testing.T) {
	// A single cue far out of place is put at the time it states: from the
	// timestamps alone it cannot be told apart from a credit line written
	// out of order, and one misplaced line is better than no subtitle.
	misplaced := "1\n00:19:58,000 --> 00:19:59,000\nBefore\n\n2\n00:01:59,000 --> 00:02:00,000\nOut of place\n\n3\n00:20:01,000 --> 00:20:02,000\nAfter\n"
	manifest, destination, err := extractZIP(t, []zipEntry{{name: "Show.S01E08.srt", body: misplaced}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, manifest.Members[0].SafeName))
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:01:59,000 --> 00:02:00,000\nOut of place\n\n2\n00:19:58,000 --> 00:19:59,000\nBefore\n\n3\n00:20:01,000 --> 00:20:02,000\nAfter\n"
	if string(content) != want {
		t.Fatalf("normalized subtitle =\n%q\nwant\n%q", content, want)
	}
}

func TestSkippedMemberDoesNotPromoteAGenericSibling(t *testing.T) {
	manifest, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E05.srt", body: "\x00\x01binary"}, {name: "subs.srt", body: validSRT}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectSingleEpisode(manifest, domain.Candidate{}, domain.Media{Season: 1, Episode: 5}, false); err == nil {
		t.Fatal("a generic file was accepted after the archive's explicit S01E05 file was skipped")
	}
}
