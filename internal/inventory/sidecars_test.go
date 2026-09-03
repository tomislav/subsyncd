package inventory

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"subsyncd/internal/domain"
)

func TestScanSidecarsRecognizesLanguagesVariantsAndOwnership(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "Show.mkv")
	writeTestFile(t, media, "video")
	files := map[string]string{
		"Show.en.srt":        "managed",
		"Show.pt-BR.ass":     "portuguese",
		"Show.hr.forced.srt": "forced",
		"Show.en.sdh.vtt":    "sdh",
		"Show.fr.txt":        "unsupported",
		"Other.en.srt":       "unrelated",
	}
	for name, body := range files {
		writeTestFile(t, filepath.Join(dir, name), body)
	}
	managedPath := filepath.Join(dir, "Show.en.srt")
	managedChecksum, err := checksumFile(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	ownership := map[domain.Language]OwnedSidecar{
		"en": {Path: managedPath, Checksum: managedChecksum},
	}

	tracks, err := ScanSidecars(media, ownership)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 4 {
		t.Fatalf("tracks = %#v, want 4", tracks)
	}
	byLanguage := make(map[domain.Language][]Track)
	for _, track := range tracks {
		byLanguage[track.Language] = append(byLanguage[track.Language], track)
	}
	var managedEnglish, protectedSDH bool
	for _, track := range byLanguage["en"] {
		if !track.SDH && !track.Protected {
			managedEnglish = true
		}
		if track.SDH && track.Protected {
			protectedSDH = true
		}
	}
	if !managedEnglish || !protectedSDH {
		t.Fatalf("English ownership/SDH = %#v", byLanguage["en"])
	}
	if !byLanguage["hr"][0].Forced || !byLanguage["hr"][0].Protected || byLanguage["pt-BR"][0].Language != "pt-BR" {
		t.Fatalf("variant parsing = %#v", byLanguage)
	}
}

func TestScanSidecarsProtectsChangedManagedFileAndSkipsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	media := filepath.Join(dir, "Show.mkv")
	changed := filepath.Join(dir, "Show.en.srt")
	writeTestFile(t, media, "video")
	writeTestFile(t, changed, "changed")
	if err := os.Symlink(changed, filepath.Join(dir, "Show.hr.srt")); err != nil {
		t.Fatal(err)
	}
	tracks, err := ScanSidecars(media, map[domain.Language]OwnedSidecar{"en": {Path: changed, Checksum: "old-checksum"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || !tracks[0].Protected {
		t.Fatalf("tracks = %#v, want one protected changed file", tracks)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
