package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"subsyncd/internal/config"
)

func TestMapPathOutsideScopeClassification(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(t.TempDir(), "elsewhere")
	for name, test := range map[string]struct {
		remote   string
		mappings []config.PathMapping
		roots    []string
	}{
		"unmatched mapping": {remote: "/other/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: root}}, roots: []string{root}},
		"outside root":      {remote: "/data/tv/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: out}}, roots: []string{root}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := MapPath(test.remote, test.mappings, test.roots)
			if !errors.Is(err, ErrOutsideScope) || !IsOutsideScope(err) {
				t.Fatalf("MapPath() error = %v, want ErrOutsideScope", err)
			}
		})
	}
}

func TestMapPathOutsideScopeDoesNotHideUnsafeFailures(t *testing.T) {
	root := t.TempDir()
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing-target"), dangling); err != nil {
		t.Fatal(err)
	}
	nonDirectoryParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(nonDirectoryParent, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingRoot := filepath.Join(t.TempDir(), "missing-root")
	for name, test := range map[string]struct {
		remote   string
		mappings []config.PathMapping
		roots    []string
	}{
		"traversal":           {remote: "/data/tv/../../etc/passwd", mappings: []config.PathMapping{{Remote: "/data/tv", Local: root}}, roots: []string{root}},
		"relative mapping":    {remote: "/data/tv/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: "relative"}}, roots: []string{root}},
		"inaccessible parent": {remote: "/data/tv/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: nonDirectoryParent}}, roots: []string{root}},
		"symlink resolution":  {remote: "/data/tv/dangling/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: root}}, roots: []string{root}},
		"root resolution":     {remote: "/data/tv/show.mkv", mappings: []config.PathMapping{{Remote: "/data/tv", Local: root}}, roots: []string{missingRoot}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := MapPath(test.remote, test.mappings, test.roots)
			if err == nil || errors.Is(err, ErrOutsideScope) || IsOutsideScope(err) {
				t.Fatalf("MapPath() error = %v, want hard failure", err)
			}
		})
	}
}

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

func TestMapPathRemoteRoot(t *testing.T) {
	root := t.TempDir()
	got, err := MapPath("/show/file.mkv", []config.PathMapping{{Remote: "/", Local: root}}, []string{root})
	if err != nil || got != filepath.Join(root, "show/file.mkv") {
		t.Fatalf("root mapping=%q %v", got, err)
	}
}
