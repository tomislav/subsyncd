package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneCacheExpiresOnlyOwnedFiles(t *testing.T) {
	l := newTestLapse(t, nil, t.TempDir())
	now := time.Now()
	ttl := 24 * time.Hour
	for _, test := range []struct {
		name   string
		age    time.Duration
		remove bool
	}{
		{"0123456789abcdef.spans", ttl, true},
		{"0123456789abcdee.spans.tmp", ttl + time.Hour, true},
		{"0123456789abcdea.spans", ttl - time.Second, false},
		{"0123456789abcdeb.spans", -time.Hour, false},
		{"other.txt", ttl + time.Hour, false},
		{"short.spans", ttl + time.Hour, false},
	} {
		path := filepath.Join(l.cacheDir, test.name)
		if err := os.WriteFile(path, []byte("profile"), 0600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-test.age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(l.cacheDir, "aaaaaaaaaaaaaaaa.spans")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(l.cacheDir, "bbbbbbbbbbbbbbbb.spans")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	count, err := l.PruneCache(t.Context(), ttl, now)
	if err != nil || count != 2 {
		t.Fatalf("prune=%d,%v", count, err)
	}
	for _, name := range []string{"0123456789abcdef.spans", "0123456789abcdee.spans.tmp"} {
		if _, err := os.Lstat(filepath.Join(l.cacheDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired file retained: %s %v", name, err)
		}
	}
	for _, path := range []string{outside, link, dir, filepath.Join(l.cacheDir, "other.txt"), filepath.Join(l.cacheDir, "short.spans"), filepath.Join(l.cacheDir, "0123456789abcdea.spans"), filepath.Join(l.cacheDir, "0123456789abcdeb.spans")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneCacheSkipsRunningLapse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	l := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) { close(started); <-release; return Execution{}, nil }), t.TempDir())
	now := time.Now()
	path := filepath.Join(l.cacheDir, "0123456789abcdef.spans")
	if err := os.WriteFile(path, []byte("profile"), 0600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	go func() { defer close(done); _, _ = l.execute(t.Context(), time.Hour, Command{}) }()
	<-started
	count, err := l.PruneCache(t.Context(), 24*time.Hour, now)
	_, statErr := os.Stat(path)
	close(release)
	<-done
	if err != nil || count != 0 || statErr != nil {
		t.Fatalf("active prune=%d,%v stat=%v", count, err, statErr)
	}
	count, err = l.PruneCache(t.Context(), 24*time.Hour, now)
	if err != nil || count != 1 {
		t.Fatalf("idle prune=%d,%v", count, err)
	}
}

func TestPruneCacheRejectsSymlinkRootAndCancellation(t *testing.T) {
	l := newTestLapse(t, nil, t.TempDir())
	moved := l.cacheDir + "-moved"
	if err := os.Rename(l.cacheDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, l.cacheDir); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PruneCache(t.Context(), time.Hour, time.Now()); err == nil {
		t.Fatal("symlink cache root accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.PruneCache(ctx, time.Hour, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}
