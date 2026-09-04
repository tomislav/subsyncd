package pack

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

const validSRT = "1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n"

func TestExtractPublishesValidatedNormalizedZIP(t *testing.T) {
	payload := zipPayload(t, []zipEntry{{name: "Show.S01E02.srt", body: validSRT}, {name: "Show.S01E03.srt", body: strings.ReplaceAll(validSRT, "Hello", "Next")}})
	destination := filepath.Join(t.TempDir(), "published")
	manifest, err := Extract(context.Background(), domain.Candidate{ProviderID: "titlovi", ResultID: "pack", Language: "hr"}, bytes.NewReader(payload), int64(len(payload)), destination, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Members) != 2 || manifest.Checksum == "" || manifest.Members[0].Checksum == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
	content, err := os.ReadFile(filepath.Join(destination, manifest.Members[0].SafeName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "\r\n") {
		t.Fatalf("line endings were not normalized: %q", content)
	}
}

func TestExtractRecordsEpisodeTitleEvidence(t *testing.T) {
	payload := zipPayload(t, []zipEntry{{name: "Example.Show.A.Great.Adventure.srt", body: validSRT}})
	destination := filepath.Join(t.TempDir(), "published")
	manifest, err := Extract(context.Background(), domain.Candidate{ProviderID: "titlovi", ResultID: "pack", Language: "hr", Title: "Example Show"}, bytes.NewReader(payload), int64(len(payload)), destination, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Members) != 1 || manifest.Members[0].NormalizedTitle != "a great adventure" {
		t.Fatalf("manifest members = %#v", manifest.Members)
	}
}

func TestExtractAcceptsPlainSubtitle(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "plain")
	manifest, err := Extract(context.Background(), domain.Candidate{ProviderID: "subdl", ResultID: "single.srt", Language: "en", DownloadRef: "/single.srt"}, strings.NewReader(validSRT), int64(len(validSRT)), destination, testLimits())
	if err != nil || len(manifest.Members) != 1 {
		t.Fatalf("manifest = %#v, %v", manifest, err)
	}
}

func TestExtractRejectsHostileOrInvalidZIPsWithoutPublication(t *testing.T) {
	tests := []struct {
		name    string
		entries []zipEntry
		limits  Limits
	}{
		{"traversal", []zipEntry{{name: "../escape.srt", body: validSRT}}, testLimits()},
		{"absolute", []zipEntry{{name: "/escape.srt", body: validSRT}}, testLimits()},
		{"backslash traversal", []zipEntry{{name: `..\escape.srt`, body: validSRT}}, testLimits()},
		{"windows absolute", []zipEntry{{name: `C:\escape.srt`, body: validSRT}}, testLimits()},
		{"symlink", []zipEntry{{name: "link.srt", body: validSRT, mode: os.ModeSymlink | 0o777}}, testLimits()},
		{"nested archive", []zipEntry{{name: "nested.zip", body: "PK\x03\x04"}}, testLimits()},
		{"nested archive disguised", []zipEntry{{name: "nested.bin", body: "PK\x03\x04payload"}}, testLimits()},
		{"duplicate", []zipEntry{{name: "Episode.srt", body: validSRT}, {name: "episode.srt", body: validSRT}}, testLimits()},
		{"spoofed subtitle", []zipEntry{{name: "episode.srt", body: "\x00\x01binary"}}, testLimits()},
		{"too many", []zipEntry{{name: "one.srt", body: validSRT}, {name: "two.srt", body: validSRT}}, Limits{MaxCompressed: 1 << 20, MaxExpanded: 1 << 20, MaxFiles: 1, MaxDepth: 1}},
		{"too deep", []zipEntry{{name: "a/b/episode.srt", body: validSRT}}, Limits{MaxCompressed: 1 << 20, MaxExpanded: 1 << 20, MaxFiles: 10, MaxDepth: 1}},
		{"expanded", []zipEntry{{name: "episode.srt", body: validSRT}}, Limits{MaxCompressed: 1 << 20, MaxExpanded: 8, MaxFiles: 10, MaxDepth: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := zipPayload(t, test.entries)
			destination := filepath.Join(t.TempDir(), "published")
			_, err := Extract(context.Background(), domain.Candidate{ProviderID: "provider", ResultID: "pack", Language: "en"}, bytes.NewReader(payload), int64(len(payload)), destination, test.limits)
			if err == nil {
				t.Fatal("Extract() error = nil")
			}
			if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
				t.Fatalf("destination was published after rejection: %v", statErr)
			}
		})
	}
}

func TestExtractRejectsCompressedLimitBeforeReading(t *testing.T) {
	reader := &countingReader{reader: strings.NewReader(validSRT)}
	_, err := Extract(context.Background(), domain.Candidate{}, reader, 100, filepath.Join(t.TempDir(), "out"), Limits{MaxCompressed: 10, MaxExpanded: 100, MaxFiles: 1, MaxDepth: 1})
	if err == nil || reader.reads != 0 {
		t.Fatalf("error/reads = %v/%d", err, reader.reads)
	}
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func zipPayload(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type countingReader struct {
	reader io.Reader
	reads  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func testLimits() Limits {
	return Limits{MaxCompressed: 1 << 20, MaxExpanded: 1 << 20, MaxFiles: 10, MaxDepth: 1}
}
