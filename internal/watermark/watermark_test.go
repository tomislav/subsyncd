package watermark

import "testing"

const watermarkedSRT = "1\n00:00:36,936 --> 00:00:40,936\nwww.titlovi.com\n\n2\n00:00:43,936 --> 00:00:47,064\nDovest ćete i Mickyja? <i>-Hoćemo.</i>\n\n3\n00:00:47,189 --> 00:00:51,093\nI on bi trebao biti ovdje.\n\n4\n01:52:08,334 --> 01:52:12,334\n<font color=\"#ffff00\">Titlovi.com</font>\n"

func TestStripSiteWatermarksRemovesTitloviCuesAndRenumbers(t *testing.T) {
	got := string(Strip(".srt", []byte(watermarkedSRT)))
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
		if got := string(Strip(".srt", []byte(srt))); got != srt {
			t.Errorf("cue %q was changed:\n%q", text, got)
		}
	}
}

func TestStripSiteWatermarksLeavesOtherFormatsAndAllWatermarkFilesAlone(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nwww.titlovi.com\n"
	if got := string(Strip(".vtt", []byte(vtt))); got != vtt {
		t.Fatalf("VTT changed: %q", got)
	}
	only := "1\n00:00:01,000 --> 00:00:02,000\nwww.titlovi.com\n"
	if got := string(Strip(".srt", []byte(only))); got != only {
		t.Fatalf("a subtitle that is nothing but a watermark was emptied: %q", got)
	}
}

func TestStripHandlesIrregularBlankLinesByteOrderMarkAndCRLF(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"extra blank line": {
			"1\n00:00:01,000 --> 00:00:02,000\nwww.titlovi.com\n\n\n2\n00:00:03,000 --> 00:00:04,000\nA\n\n3\n00:00:05,000 --> 00:00:06,000\nB\n",
			"1\n00:00:03,000 --> 00:00:04,000\nA\n\n2\n00:00:05,000 --> 00:00:06,000\nB\n",
		},
		"spaces on the blank line": {
			"1\n00:00:01,000 --> 00:00:02,000\nwww.titlovi.com\n \n2\n00:00:03,000 --> 00:00:04,000\nA\n",
			"1\n00:00:03,000 --> 00:00:04,000\nA\n",
		},
		"byte order mark on a removed first cue": {
			"\ufeff1\n00:00:01,000 --> 00:00:02,000\nwww.titlovi.com\n\n2\n00:00:03,000 --> 00:00:04,000\nA\n",
			"\ufeff1\n00:00:03,000 --> 00:00:04,000\nA\n",
		},
		"crlf": {
			"1\r\n00:00:01,000 --> 00:00:02,000\r\nwww.titlovi.com\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\nA\r\n",
			"1\r\n00:00:03,000 --> 00:00:04,000\r\nA\r\n",
		},
	}
	for name, test := range tests {
		if got := string(Strip(".srt", []byte(test.in))); got != test.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, test.want)
		}
	}
}
