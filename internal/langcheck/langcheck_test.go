package langcheck

import (
	"strings"
	"testing"
)

func mismatched(language, text string) bool {
	_, mismatch := Check(language, text)
	return mismatch
}

const serbian = "Gde si bio? Ovde je lepo vreme.\nUvek isto, deca su na mestu.\nKo je to? Šta hoćeš? Gde ćemo?\nNigde ne idem, lepo mi je ovde.\n"
const croatian = "Gdje si bio? Ovdje je lijepo vrijeme.\nUvijek isto, djeca su na mjestu.\nTko je to? Što hoćeš? Gdje ćemo?\nNigdje ne idem, lijepo mi je ovdje.\n"

func TestSerbianTextIsAMismatchForCroatianAndBosnian(t *testing.T) {
	for _, language := range []string{"hr", "bs", "hr-HR"} {
		if !mismatched(language, serbian) {
			t.Errorf("mismatched(%q, serbian) = false, want true", language)
		}
	}
}

func TestCroatianMixedAndShortTextPass(t *testing.T) {
	cases := map[string]string{
		"croatian":         croatian,
		"mixed like Meru":  strings.Repeat(croatian, 2) + strings.Repeat(serbian, 2),
		"too few ekavian":  "Gde si? Ovde.",
		"no reflex at all": "Da. Ne. Možda. Hvala.",
	}
	for name, text := range cases {
		if mismatched("hr", text) {
			t.Errorf("%s: Mismatch = true, want false", name)
		}
	}
}

func TestOtherLanguagesAreNeverChecked(t *testing.T) {
	for _, language := range []string{"sr", "en", "sl", ""} {
		if mismatched(language, serbian) {
			t.Errorf("mismatched(%q) = true, want false", language)
		}
	}
}

func TestCroatianInflectionsAndLookalikesAreNotEkavian(t *testing.T) {
	// "vremena" is the Croatian genitive of "vrijeme"; "lepeza" and
	// "lepršav" are Croatian words that merely start like "lep".
	text := strings.Repeat("Nemamo vremena. Bilo je to vremenom. Vremenski uvjeti. Lepeza je lepršava. ", 10)
	if mismatched("hr", text) {
		t.Fatal("Croatian inflections counted as Serbian")
	}
}

func TestWordsAreSplitOnUnicodeLetters(t *testing.T) {
	// Without Unicode-aware word splitting, "gde" inside "šgde" or "đovde"
	// would count.
	if mismatched("hr", strings.Repeat("šgde đovde čuvek ždete ", 5)) {
		t.Fatal("ekavian forms matched inside longer words")
	}
}

const portuguese = "Achei que era o fim de mim. Peguei meu rifle, e foi quando ela carregou. Não havia nada que eu pudesse fazer, eu tive que atirar nela. Era a imagem da perfeição. Uma criatura requintada. Onde você estava? "
const english = "I thought it was the end of me. I grabbed my rifle and that is when she charged. There was nothing I could do, I had to shoot her. "

func TestTextThatIsNotSouthSlavicIsAMismatch(t *testing.T) {
	for name, text := range map[string]string{"portuguese": strings.Repeat(portuguese, 12), "english": strings.Repeat(english, 12)} {
		reason, mismatch := Check("hr", text)
		if !mismatch || reason != ReasonNotSouthSlavic {
			t.Errorf("%s: Check = %q, %v; want %q, true", name, reason, mismatch, ReasonNotSouthSlavic)
		}
	}
	if reason, _ := Check("hr", serbian); reason != ReasonEkavian {
		t.Errorf("serbian: reason = %q, want %q", reason, ReasonEkavian)
	}
}

func TestShortOrSouthSlavicTextIsNotJudgedForeign(t *testing.T) {
	for name, text := range map[string]string{"short portuguese": portuguese, "croatian": strings.Repeat(croatian, 12)} {
		if mismatched("hr", text) {
			t.Errorf("%s: mismatch, want none", name)
		}
	}
}
