package catalog

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"subsyncd/internal/config"
)

func TestMapPathUsesLongestPrefixAndNormalizesWindowsSeparators(t *testing.T) {
	root := t.TempDir()
	mappings := []config.PathMapping{
		{Remote: `D:\TV`, Local: root},
		{Remote: `D:\TV\Anime`, Local: filepath.Join(root, "Anime Library")},
	}
	want := filepath.Join(root, "Anime Library", "Show", "Episode.MKV")
	got, err := MapPath(`D:\TV\Anime\Show\Episode.MKV`, mappings, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("mapped path = %q, want %q", got, want)
	}
}

func TestMapPathRejectsPrefixLookalikeTraversalAndOutsideRoot(t *testing.T) {
	root := t.TempDir()
	mapping := []config.PathMapping{{Remote: "/data/tv", Local: root}}
	for _, input := range []string{"/data/tv-evil/file.mkv", "/data/tv/../../etc/passwd"} {
		if got, err := MapPath(input, mapping, []string{root}); err == nil {
			t.Fatalf("MapPath(%q) = %q, want error", input, got)
		}
	}
	outside := filepath.Join(filepath.Dir(root), "outside")
	if got, err := MapPath("/data/tv/file.mkv", []config.PathMapping{{Remote: "/data/tv", Local: outside}}, []string{root}); err == nil {
		t.Fatalf("outside mapping = %q, want error", got)
	}
}

func TestMapPathRejectsSymlinkedParentEscapingRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if got, err := MapPath("/data/tv/escape/file.mkv", []config.PathMapping{{Remote: "/data/tv", Local: root}}, []string{root}); err == nil {
		t.Fatalf("symlink escape = %q, want error", got)
	}
}

func TestMapPathPreservesPathCase(t *testing.T) {
	root := t.TempDir()
	got, err := MapPath("/data/tv/My Show/Episode.MKV", []config.PathMapping{{Remote: "/data/tv", Local: root}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "My Show", "Episode.MKV"); got != want {
		t.Fatalf("mapped path = %q, want %q", got, want)
	}
}
