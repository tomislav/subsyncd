package opensubtitles

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFileHasherMatchesReferenceAndPreservesFileOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "movie.bin")
	payload := make([]byte, 192*1024)
	for index := range payload {
		payload[index] = byte(index * 31)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Seek(123, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	result, err := CalculateHash(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := referenceHash(payload); result.MovieHash != want || result.ByteSize != int64(len(payload)) {
		t.Fatalf("hash = %#v, want %s/%d", result, want, len(payload))
	}
	if offset, _ := file.Seek(0, io.SeekCurrent); offset != 123 {
		t.Fatalf("file offset = %d, want 123", offset)
	}
}

func TestFileHasherReturnsTypedUnsupportedForShortOrNonRandomSource(t *testing.T) {
	shortPath := filepath.Join(t.TempDir(), "short.bin")
	if err := os.WriteFile(shortPath, make([]byte, 128*1024-1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileHasher{}).Hash(shortPath); !errors.As(err, new(*UnsupportedHashError)) {
		t.Fatalf("short-file error = %v", err)
	}
	if _, err := CalculateHash(strings.NewReader("not random-access metadata")); !errors.As(err, new(*UnsupportedHashError)) {
		t.Fatalf("non-random source error = %v", err)
	}
}

func referenceHash(payload []byte) string {
	total := uint64(len(payload))
	for _, chunk := range [][]byte{payload[:64*1024], payload[len(payload)-64*1024:]} {
		for offset := 0; offset < len(chunk); offset += 8 {
			total += binary.LittleEndian.Uint64(chunk[offset : offset+8])
		}
	}
	return leftPad(strconv.FormatUint(total, 16), 16)
}

func leftPad(value string, length int) string {
	if len(value) >= length {
		return value
	}
	return strings.Repeat("0", length-len(value)) + value
}
