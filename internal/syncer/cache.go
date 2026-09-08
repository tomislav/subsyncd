package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// PruneCache removes expired LAPSE v2.0.5 profiles by last-write time. A busy
// cache is skipped until the next sweep, so cleanup never races a subprocess.
// Unknown files, directories and symlinks are deliberately left untouched.
func (l *Lapse) PruneCache(ctx context.Context, ttl time.Duration, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("LAPSE cache TTL must be positive")
	}
	if !l.cacheMu.TryLock() {
		return 0, nil
	}
	defer l.cacheMu.Unlock()
	info, err := os.Lstat(l.cacheDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("LAPSE cache directory unavailable")
	}
	root, err := os.OpenRoot(l.cacheDir)
	if err != nil {
		return 0, fmt.Errorf("open LAPSE cache directory")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return 0, fmt.Errorf("LAPSE cache directory changed")
	}
	directory, err := root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("read LAPSE cache directory")
	}
	defer directory.Close()
	cutoff := now.Add(-ttl)
	removed := 0
	var failed bool
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return removed, fmt.Errorf("read LAPSE cache entries")
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if !speechCacheFilename(entry.Name()) {
				continue
			}
			info, err := root.Lstat(entry.Name())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				failed = true
				continue
			}
			if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
				continue
			}
			if err := root.Remove(entry.Name()); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					failed = true
				}
				continue
			}
			removed++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if failed {
		return removed, fmt.Errorf("some expired LAPSE cache entries could not be removed")
	}
	return removed, nil
}

func speechCacheFilename(name string) bool {
	name = strings.TrimSuffix(name, ".tmp")
	if !strings.HasSuffix(name, ".spans") {
		return false
	}
	key := strings.TrimSuffix(name, ".spans")
	if len(key) != 16 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
