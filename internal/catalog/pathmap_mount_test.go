package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"subsyncd/internal/config"
)

func TestMapPathTreatsFailingMountAsUnavailable(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "Show")
	for _, errno := range []syscall.Errno{syscall.ENOTCONN, syscall.EIO, syscall.ESTALE, syscall.EHOSTDOWN} {
		t.Run(errno.Error(), func(t *testing.T) {
			lstat = func(path string) (os.FileInfo, error) {
				if path == show {
					return nil, &fs.PathError{Op: "lstat", Path: path, Err: errno}
				}
				return os.Lstat(path)
			}
			t.Cleanup(func() { lstat = os.Lstat })
			_, err := MapPath("/data/tv/Show/episode.mkv", []config.PathMapping{{Remote: "/data/tv", Local: root}}, []string{root})
			if !errors.Is(err, errMappedPathUnavailable) || IsOutsideScope(err) {
				t.Fatalf("MapPath() error = %v, want the media deferred as unavailable", err)
			}
		})
	}
	lstat = func(path string) (os.FileInfo, error) {
		if path == show {
			return nil, &fs.PathError{Op: "lstat", Path: path, Err: syscall.EACCES}
		}
		return os.Lstat(path)
	}
	t.Cleanup(func() { lstat = os.Lstat })
	if _, err := MapPath("/data/tv/Show/episode.mkv", []config.PathMapping{{Remote: "/data/tv", Local: root}}, []string{root}); err == nil || errors.Is(err, errMappedPathUnavailable) {
		t.Fatalf("permission failure = %v, want a hard error", err)
	}
}
