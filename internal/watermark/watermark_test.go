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

func cueStripped(text string) bool {
	srt := "1\n00:00:01,000 --> 00:00:02,000\n" + text + "\n\n2\n00:00:03,000 --> 00:00:04,000\nDialogue\n"
	return string(Strip(".srt", []byte(srt))) != srt
}

func TestStripRemovesSiteAdvertsSeenInTheLibrary(t *testing.T) {
	for _, text := range []string{
		"Preuzeto sa www.titlovi.com",
		"Preuzeto s www.titlovi.com",
		"Preuzeto sa\nwww.titlovi.com",
		"www. titlovi. com",
		"Preuzeto sa www. titlovi. com",
		"www.tilovi.com",
		"www.addic7ed.com",
		"@SUBSCENE",
		"www.prijevodi-online.org",
		"Advertise your product or brand here\ncontact www.OpenSubtitles.org today",
		"Support us and become VIP member\nto remove all ads from www.OpenSubtitles.org",
		"Подржите нас и постаните VIP члан да бисте уклонили све огласе са www.OpenSubtitles.org",
		"Please rate this subtitle at www.osdb.link/abc\nHelp other users to choose the best subtitles from OpenSubtitles.com",
	} {
		if !cueStripped(text) {
			t.Errorf("cue %q was kept; want it removed", text)
		}
	}
}

func TestStripKeepsCreditsAndDialogue(t *testing.T) {
	for _, text := range []string{
		"Subtitles by explosiveskull",
		"Synced & corrected by -robtor-",
		"WEB-DL resync by GoldenBeard",
		"lesaigneur@hotmail.com",
		"Monster.com.",
		"Travelocity.com?",
		"ovdje u Crypto.com areni.",
		"Preveo: Ivan za www.titlovi.com",
		"ko-fi.com/pearlfansub",
	} {
		if cueStripped(text) {
			t.Errorf("cue %q was removed; want it kept", text)
		}
	}
}

func TestStripRemovesAddic7edSignatureCues(t *testing.T) {
	for _, text := range []string{
		"- Synced and corrected by medvidecek007 -\n- www.addic7ed.com -",
		"- Synced and corrected by <font color=\"#009BCB\"><b>chamallow</b></font> -\n- www.addic7ed.com -",
		"- Synced and corrected by<font color=\"#00BFFF\"> Firefly</font> -\n- <font color=\"#00ffff\">www.addic7ed.com</font> -",
		"Sync & corrections by honeybunny\nwww.addic7ed.com",
		"Subtitles by explosiveskull\nwww.addic7ed.com",
		"- www.addic7ed.com -",
	} {
		if !cueStripped(text) {
			t.Errorf("cue %q was kept; want it removed", text)
		}
	}
}

func TestStripKeepsCreditsWithoutASiteAndDialogue(t *testing.T) {
	for _, text := range []string{
		"- Synced and corrected by medvidecek007 -",
		"Subtitles by explosiveskull",
		"corrected by now.",
		"- Who synced this?\n- www.addic7ed.com, I think.",
		"It was corrected by the lab\nwww.addic7ed.com",
	} {
		if cueStripped(text) {
			t.Errorf("cue %q was removed; want it kept", text)
		}
	}
}
