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

// PruneCache removes expired LAPSE cache entries (speech profiles and
// embedded subtitle-track timings) by last-write time. It sweeps the current
// speech-cache generation, older generations (lapse-<version> directories,
// each removed once empty) and the unversioned entries LAPSE 2.3.2 and older
// wrote at the cache root. A busy cache is skipped until the next sweep, so
// cleanup never races a subprocess.
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
	cutoff := now.Add(-ttl)
	removed, generations, failed, err := pruneCacheEntries(ctx, root, cutoff)
	if err != nil {
		return removed, err
	}
	current := speechCacheGenerationPrefix + SpeechCacheGeneration
	for _, name := range generations {
		count, swept, generationFailed, err := pruneCacheGeneration(ctx, root, name, cutoff)
		removed += count
		failed = failed || generationFailed
		if err != nil {
			return removed, err
		}
		if swept && name != current {
			// Remove refuses a directory that still holds anything, so only an
			// emptied older generation goes.
			_ = root.Remove(name)
		}
	}
	if failed {
		return removed, fmt.Errorf("some expired LAPSE cache entries could not be removed")
	}
	return removed, nil
}

// pruneCacheGeneration sweeps one lapse-<version> directory. It must be a real
// directory, not a symlink, and is opened as its own root so nothing outside
// it can be reached. swept reports that it was such a directory and was read
// to the end.
func pruneCacheGeneration(ctx context.Context, root *os.Root, name string, cutoff time.Time) (removed int, swept, failed bool, err error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, true, nil
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, false, false, nil
	}
	generation, err := root.OpenRoot(name)
	if err != nil {
		return 0, false, true, nil
	}
	defer generation.Close()
	opened, err := generation.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return 0, false, true, nil
	}
	removed, _, failed, err = pruneCacheEntries(ctx, generation, cutoff)
	return removed, err == nil, failed, err
}

// pruneCacheEntries removes expired cache files directly inside dir and
// returns the names of the speech-cache generation directories it saw.
func pruneCacheEntries(ctx context.Context, dir *os.Root, cutoff time.Time) (removed int, generations []string, failed bool, err error) {
	directory, err := dir.Open(".")
	if err != nil {
		return 0, nil, false, fmt.Errorf("read LAPSE cache directory")
	}
	defer directory.Close()
	for {
		if err := ctx.Err(); err != nil {
			return removed, generations, failed, err
		}
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return removed, generations, failed, fmt.Errorf("read LAPSE cache entries")
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return removed, generations, failed, err
			}
			if speechCacheGenerationName(entry.Name()) {
				generations = append(generations, entry.Name())
				continue
			}
			if !speechCacheFilename(entry.Name()) {
				continue
			}
			info, err := dir.Lstat(entry.Name())
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
			if err := dir.Remove(entry.Name()); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					failed = true
				}
				continue
			}
			removed++
		}
		if errors.Is(readErr, io.EOF) {
			return removed, generations, failed, nil
		}
	}
}

// speechCacheGenerationName matches lapse-<major>.<minor>.<patch>.
func speechCacheGenerationName(name string) bool {
	version, ok := strings.CutPrefix(name, speechCacheGenerationPrefix)
	if !ok {
		return false
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 6 {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
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
