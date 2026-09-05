package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"subsyncd/internal/domain"
)

func TestMediaTitleFormatsAndSanitizesHumanIdentity(t *testing.T) {
	tests := []struct {
		name  string
		media domain.Media
		want  string
	}{
		{name: "movie", media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Blade\nRunner", Year: 1982}, want: "Blade Runner (1982)"},
		{name: "episode", media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Show\tName", Season: 1, Episode: 2, EpisodeTitle: "Pilot\nPart"}, want: "Show Name - S01E02 - Pilot Part"},
		{name: "episode without title", media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Show", Season: 3, Episode: 7}, want: "Show - S03E07"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := MediaTitle(test.media); got != test.want {
				t.Fatalf("MediaTitle() = %q, want %q", got, test.want)
			}
		})
	}

	long := MediaTitle(domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: strings.Repeat("x", maximumTextRunes+100)})
	if len([]rune(long)) != maximumTextRunes {
		t.Fatalf("bounded title length = %d, want %d", len([]rune(long)), maximumTextRunes)
	}
}

func TestErrorAttrsRedactsBoundsAndFlattens(t *testing.T) {
	const secret = "credential-sentinel"
	root := t.TempDir()
	var output bytes.Buffer
	events, err := New(&output, Options{
		Level:      "info",
		Version:    "test",
		MediaRoots: []string{root},
		Redact: func(err error) string {
			return strings.ReplaceAll(strings.ReplaceAll(err.Error(), secret, "[redacted]"), root, "[media]")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("ž", 3000)
	attrs := events.ErrorAttrs("provider_search", errors.New(secret+"\n"+root+"/movie.mkv\r\n"+long))
	events.For("provider").Log(context.Background(), slog.LevelError, "provider.search_completed", "provider failed", attrs...)
	record := decodeJSONLines(t, output.String())[0]
	if got := record["error_kind"]; got != "provider_search" {
		t.Fatalf("error_kind = %#v", got)
	}
	message, ok := record["error"].(string)
	if !ok {
		t.Fatalf("error = %#v, want string", record["error"])
	}
	for _, forbidden := range []string{secret, root, "\n", "\r"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("error contains %q: %q", forbidden, message)
		}
	}
	if !utf8.ValidString(message) || utf8.RuneCountInString(message) > 2048 {
		t.Fatalf("error is invalid or unbounded: runes=%d", utf8.RuneCountInString(message))
	}
}

func TestRelativePathAllowsContainedExistingAndFutureFiles(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "Series", "Season 01")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(directory, "Episode.mkv")
	if err := os.WriteFile(media, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := New(&bytes.Buffer{}, Options{Level: "debug", MediaRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		media: "Series/Season 01/Episode.mkv",
		filepath.Join(directory, "Episode.hr.srt"): "Series/Season 01/Episode.hr.srt",
	}
	for path, want := range tests {
		got, ok := events.RelativePath(path)
		if !ok || got != want {
			t.Errorf("RelativePath(%q) = %q, %t; want %q, true", path, got, ok, want)
		}
	}
}

func TestRelativePathRejectsOutsideRootAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.mkv")
	if err := os.WriteFile(outsideFile, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escaped")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	events, err := New(&bytes.Buffer{}, Options{Level: "debug", MediaRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outsideFile, filepath.Join(link, "outside.mkv"), root, "relative/file.mkv"} {
		if got, ok := events.RelativePath(path); ok {
			t.Errorf("RelativePath(%q) = %q, true; want rejection", path, got)
		}
	}
}

func TestSafeTextIsSingleLineValidUTF8AndBounded(t *testing.T) {
	input := " first\nsecond\r\nthird\u2028" + string([]byte{0xff}) + strings.Repeat("x", 3000)
	got := SafeText(input)
	if strings.ContainsAny(got, "\r\n") || strings.ContainsRune(got, '\u2028') {
		t.Fatalf("SafeText retained line separator: %q", got)
	}
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 2048 {
		t.Fatalf("SafeText invalid or unbounded: runes=%d", utf8.RuneCountInString(got))
	}
}
