package selection

import "testing"

func TestParseTitleForms(t *testing.T) {
	tests := []struct {
		text string
		want TitleQuery
	}{
		{"Dune", TitleQuery{Name: "Dune"}},
		{"Movie (2019)", TitleQuery{Name: "Movie", Year: 2019}},
		{"Show S01E02", TitleQuery{Name: "Show", Season: 1, Episode: 2}},
		{"Show 1x02", TitleQuery{Name: "Show", Season: 1, Episode: 2}},
		{"Show - S01E02 - Episode Title", TitleQuery{Name: "Show", Season: 1, Episode: 2}},
		{"the office us s01e02", TitleQuery{Name: "the office us", Season: 1, Episode: 2}},
		{"Show (2010) S02E10", TitleQuery{Name: "Show", Year: 2010, Season: 2, Episode: 10}},
		{"  Dune  ", TitleQuery{Name: "Dune"}},
		{"Blade Runner 2049", TitleQuery{Name: "Blade Runner 2049"}},
	}
	for _, test := range tests {
		got, err := ParseTitle(test.text)
		if err != nil || got != test.want {
			t.Errorf("ParseTitle(%q) = %+v, %v; want %+v", test.text, got, err, test.want)
		}
	}
	for _, empty := range []string{"", "   ", "S01E02", "(2019)"} {
		if _, err := ParseTitle(empty); err == nil {
			t.Errorf("ParseTitle(%q) succeeded", empty)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := map[string]string{
		"Amélie":                          "amelie",
		"The Office (US)":                 "the office us",
		"Marvel's Agents of S.H.I.E.L.D.": "marvels agents of shield",
		"  Dune:   Part  Two ":            "dune part two",
		"Crème Brûlée Ł Æ ß":              "creme brulee l ae ss",
		"Spider-Man":                      "spider man",
	}
	for in, want := range tests {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
