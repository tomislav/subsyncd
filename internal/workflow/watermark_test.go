package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/watermark"
)

const watermarkedSRT = "1\n00:00:36,936 --> 00:00:40,936\nwww.titlovi.com\n\n2\n00:00:43,936 --> 00:00:47,064\nDovest ćete i Mickyja? <i>-Hoćemo.</i>\n\n3\n00:00:47,189 --> 00:00:51,093\nI on bi trebao biti ovdje.\n\n4\n01:52:08,334 --> 01:52:12,334\n<font color=\"#ffff00\">Titlovi.com</font>\n"

func TestInstallerStripsWatermarksWhenEnabled(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), watermarkedSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.hr.srt"))
	request.Media.Duration = 116 * time.Minute
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root, filepath.Dir(request.Media.Fingerprint.Path)}, StripWatermarks: true}
	installation, err := installer.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(installation.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(watermark.Strip(".srt", []byte(watermarkedSRT))) || installation.Checksum != checksumBytes(payload) {
		t.Fatalf("installed %q (checksum %s); want the stripped subtitle and its checksum", payload, installation.Checksum)
	}
}

func TestInstallerKeepsWatermarksByDefault(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), watermarkedSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.hr.srt"))
	request.Media.Duration = 116 * time.Minute
	installer := Installer{Repository: &installationRepository{}, MediaRoots: []string{root, filepath.Dir(request.Media.Fingerprint.Path)}}
	installation, err := installer.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(installation.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != watermarkedSRT || installation.Checksum != checksumBytes(payload) {
		t.Fatalf("installed %q; want the subtitle unchanged when stripping is off", payload)
	}
}

func TestInstallerStripsWatermarksBeforeTheOverrunCheck(t *testing.T) {
	// The advert cue at 01:52:08 is more than five minutes past a 100-minute
	// film; without it the subtitle fits.
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), watermarkedSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.hr.srt"))
	request.Media.Duration = 100 * time.Minute
	installer := Installer{Repository: &installationRepository{}, MediaRoots: []string{root, filepath.Dir(request.Media.Fingerprint.Path)}, StripWatermarks: true}
	installation, err := installer.Install(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(installation.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(watermark.Strip(".srt", []byte(watermarkedSRT))) {
		t.Fatalf("installed %q; want the stripped subtitle", payload)
	}
	// Without stripping, the advert still fails the check.
	if _, err := validatedSubtitle(source, request.Media); err == nil {
		t.Fatal("the unstripped subtitle passed the overrun check")
	}
}
