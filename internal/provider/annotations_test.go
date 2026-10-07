package provider

import "testing"

func TestAnnotations(t *testing.T) {
	for _, tt := range []struct {
		name, comment   string
		structured      bool
		forced, hearing bool
	}{
		{"Example.S01E01.SDH.srt", "", false, false, true},
		{"Example.S01E01.HI.srt", "", false, false, true},
		{"Example.S01E01.srt", "NON-HI", false, false, false},
		{"Example.S01E01.srt", "Forced subtitles for foreign dialogue", false, true, false},
		{"Example.S01E01.srt", "Removed SDH subtitles", false, false, false},
		{"Example.S01E01.srt", "No SDH but forced subtitles", false, true, false},
		{"Example.S01E01.srt", "Removed SDH subtitles", true, false, true},
	} {
		forced, hearing := Annotations(tt.name, tt.comment, tt.structured)
		if forced != tt.forced || hearing != tt.hearing {
			t.Errorf("Annotations(%q, %q, %v) = forced %v, hearing %v; want %v, %v", tt.name, tt.comment, tt.structured, forced, hearing, tt.forced, tt.hearing)
		}
	}
}
