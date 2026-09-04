package opensubtitles

import (
	"fmt"
	"io"
	"os"

	"github.com/opensubtitlescli/moviehash"
)

const minimumHashBytes = 128 * 1024

type FileHash struct {
	MovieHash string
	ByteSize  int64
}

type Hasher interface {
	Hash(path string) (FileHash, error)
}

type UnsupportedHashError struct {
	Reason string
	Err    error
}

func (e *UnsupportedHashError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("OpenSubtitles hash is unsupported: %s: %v", e.Reason, e.Err)
	}
	return "OpenSubtitles hash is unsupported: " + e.Reason
}

func (e *UnsupportedHashError) Unwrap() error { return e.Err }

type FileHasher struct{}

func (FileHasher) Hash(path string) (FileHash, error) {
	file, err := os.Open(path)
	if err != nil {
		return FileHash{}, fmt.Errorf("open media for OpenSubtitles hash: %w", err)
	}
	defer file.Close()
	return CalculateHash(file)
}

type randomReadFile interface {
	io.Reader
	io.ReaderAt
	Name() string
	Stat() (os.FileInfo, error)
}

func CalculateHash(source io.Reader) (FileHash, error) {
	file, ok := source.(randomReadFile)
	if !ok {
		return FileHash{}, &UnsupportedHashError{Reason: "source does not support random reads and file metadata"}
	}
	info, err := file.Stat()
	if err != nil {
		return FileHash{}, &UnsupportedHashError{Reason: "file metadata is unavailable", Err: err}
	}
	if info.Size() < minimumHashBytes {
		return FileHash{}, &UnsupportedHashError{Reason: fmt.Sprintf("file is shorter than %d bytes", minimumHashBytes)}
	}
	hash, err := moviehash.Sum(file)
	if err != nil {
		return FileHash{}, fmt.Errorf("calculate OpenSubtitles hash: %w", err)
	}
	return FileHash{MovieHash: hash, ByteSize: info.Size()}, nil
}
