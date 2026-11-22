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

// LAPSE keys a cached profile by the film alone, so profiles from an older
// LAPSE would be reused after an upgrade. Each speech-cache generation gets
// its own directory instead.
func TestLapseUsesAVersionedSpeechCache(t *testing.T) {
	media, subtitle := testFiles(t)
	var env []string
	l := newTestLapse(t, runnerFunc(func(_ context.Context, command Command) (Execution, error) {
		env = command.Env
		return Execution{Stdout: fixture(t, "solid-v2.3.json"), ExitCode: 0}, nil
	}), filepath.Dir(media))
	want := filepath.Join(l.cacheDir, "lapse-"+SpeechCacheGeneration)
	if info, err := os.Lstat(want); err != nil || !info.IsDir() {
		t.Fatalf("generation directory = %v, %v", info, err)
	}
	if _, err := l.Analyze(t.Context(), media, subtitle); err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != "LAPSE_CACHE="+want {
		t.Fatalf("env = %q, want LAPSE_CACHE=%s", env, want)
	}
}

func TestNewRefusesASymlinkedSpeechCacheGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "speech-cache")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "lapse-"+SpeechCacheGeneration)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Path: "/usr/local/bin/lapse", CacheDir: root, MediaRoots: []string{t.TempDir()}}); err == nil {
		t.Fatal("New() accepted a symlinked speech-cache generation")
	}
}

// Every LAPSE bump must decide whether cached profiles stay valid.
func TestSpeechCacheGenerationWasReviewedForThisLAPSE(t *testing.T) {
	if SpeechCacheGenerationReviewedFor != CompatibilityVersion {
		t.Fatalf("SpeechCacheGenerationReviewedFor = %q, CompatibilityVersion = %q: decide whether LAPSE %s changes cached profiles (see docs/development/providers/lapse.md#upgrading-lapse), then set SpeechCacheGenerationReviewedFor to %q", SpeechCacheGenerationReviewedFor, CompatibilityVersion, CompatibilityVersion, CompatibilityVersion)
	}
}

func TestPruneCacheExpiresOlderGenerations(t *testing.T) {
	l := newTestLapse(t, nil, t.TempDir())
	now := time.Now()
	ttl := 24 * time.Hour
	current := "lapse-" + SpeechCacheGeneration
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(l.cacheDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("profile"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := ttl + time.Hour
	gone := []string{
		write("0123456789abcdef.spans", old), // unversioned, from LAPSE 2.3.2 and earlier
		write(filepath.Join(current, "0123456789abcdef.spans"), old),
		write(filepath.Join("lapse-2.3.2", "0123456789abcdef.spans"), old),
		write(filepath.Join("lapse-2.4.0", "0123456789abcdef.spans.tmp"), old),
	}
	kept := []string{
		write(filepath.Join(current, "0123456789abcdea.spans"), time.Hour),
		write(filepath.Join("lapse-2.4.0", "0123456789abcdea.spans"), time.Hour),
		write(filepath.Join("lapse-2.4.0", "other.txt"), old),
		write(filepath.Join("lapse-next", "0123456789abcdef.spans"), old),
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "0123456789abcdef.spans")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := now.Add(-old)
	if err := os.Chtimes(victim, at, at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.cacheDir, "lapse-2.2.4")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(l.cacheDir, "lapse-2.1.0"), 0o750); err != nil {
		t.Fatal(err)
	}

	count, err := l.PruneCache(t.Context(), ttl, now)
	if err != nil || count != len(gone) {
		t.Fatalf("prune=%d,%v want %d", count, err, len(gone))
	}
	for _, path := range gone {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired file retained: %s %v", path, err)
		}
	}
	for _, path := range append(kept, victim, filepath.Join(l.cacheDir, "lapse-2.2.4"), filepath.Join(l.cacheDir, current)) {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("kept path removed: %s %v", path, err)
		}
	}
	// An older generation is removed once it is empty; the current one stays.
	for _, name := range []string{"lapse-2.3.2", "lapse-2.1.0"} {
		if _, err := os.Lstat(filepath.Join(l.cacheDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty older generation retained: %s %v", name, err)
		}
	}
}
