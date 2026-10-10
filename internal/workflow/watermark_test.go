package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const watermarkedSRT = "1\n00:00:36,936 --> 00:00:40,936\nwww.titlovi.com\n\n2\n00:00:43,936 --> 00:00:47,064\nDovest ćete i Mickyja? <i>-Hoćemo.</i>\n\n3\n00:00:47,189 --> 00:00:51,093\nI on bi trebao biti ovdje.\n\n4\n01:52:08,334 --> 01:52:12,334\n<font color=\"#ffff00\">Titlovi.com</font>\n"

func TestStripSiteWatermarksRemovesTitloviCuesAndRenumbers(t *testing.T) {
	got := string(stripSiteWatermarks(".srt", []byte(watermarkedSRT)))
	want := "1\n00:00:43,936 --> 00:00:47,064\nDovest ćete i Mickyja? <i>-Hoćemo.</i>\n\n2\n00:00:47,189 --> 00:00:51,093\nI on bi trebao biti ovdje.\n"
	if got != want {
		t.Fatalf("stripped subtitle =\n%q\nwant\n%q", got, want)
	}
}

func TestStripSiteWatermarksKeepsCuesThatOnlyMentionTheSite(t *testing.T) {
	for _, text := range []string{
		"Preveo: Ivan za www.titlovi.com",
		"Idemo na titlovi.com večeras",
		"www.example.com",
	} {
		srt := "1\n00:00:01,000 --> 00:00:02,000\n" + text + "\n\n2\n00:00:03,000 --> 00:00:04,000\nDialogue\n"
		if got := string(stripSiteWatermarks(".srt", []byte(srt))); got != srt {
			t.Errorf("cue %q was changed:\n%q", text, got)
		}
	}
}

func TestStripSiteWatermarksLeavesOtherFormatsAndAllWatermarkFilesAlone(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nwww.titlovi.com\n"
	if got := string(stripSiteWatermarks(".vtt", []byte(vtt))); got != vtt {
		t.Fatalf("VTT changed: %q", got)
	}
	only := "1\n00:00:01,000 --> 00:00:02,000\nwww.titlovi.com\n"
	if got := string(stripSiteWatermarks(".srt", []byte(only))); got != only {
		t.Fatalf("a subtitle that is nothing but a watermark was emptied: %q", got)
	}
}

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
	if string(payload) != string(stripSiteWatermarks(".srt", []byte(watermarkedSRT))) || installation.Checksum != checksumBytes(payload) {
		t.Fatalf("installed %q (checksum %s); want the stripped subtitle and its checksum", payload, installation.Checksum)
	}
}
