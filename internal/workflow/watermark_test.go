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

func TestInstallerWritesTheSubtitleWithoutWatermarks(t *testing.T) {
	root := t.TempDir()
	source := writeInstallFile(t, filepath.Join(t.TempDir(), "source.srt"), watermarkedSRT)
	request := installRequest(t, source, filepath.Join(root, "Movie.hr.srt"))
	request.Media.Duration = 116 * time.Minute
	repository := &installationRepository{}
	installer := Installer{Repository: repository, MediaRoots: []string{root, filepath.Dir(request.Media.Fingerprint.Path)}}
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
