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

func TestExtractStillRejectsInvalidCueTimes(t *testing.T) {
	backwards := "1\n00:00:05,000 --> 00:00:04,000\nEnds before it starts\n"
	_, _, err := extractZIP(t, []zipEntry{{name: "Show.S01E01.srt", body: backwards}})
	var content *ContentError
	if !errors.As(err, &content) {
		t.Fatalf("error = %T %v; want a content rejection", err, err)
	}
}
