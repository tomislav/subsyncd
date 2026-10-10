// Package langcheck catches subtitles whose text is in a different language
// from the one their provider tagged.
//
// Croatian and Bosnian are written ijekavian (gdje, uvijek, vrijeme, lijepo,
// djeca, mjesto), Serbian mostly ekavian (gde, uvek, vreme, lepo, deca,
// mesto). Uploads tagged Croatian on OpenSubtitles or SubDL are sometimes
// Serbian; LAPSE times them fine, so only the text tells them apart.
package langcheck

import (
	"strings"
	"unicode"
)

// Exact word forms only: prefixes would also catch standard Croatian, whose
// "vrijeme" inflects to "vremena", and words such as "lepeza".
var (
	ijekavian = wordSet("gdje ovdje ondje nigdje negdje svugdje uvijek vrijeme lijep lijepa lijepo lijepe lijepi lijepu lijepog lijepom dijete djeca djece djeci djecom mjesto mjesta mjestu mjestom")
	ekavian   = wordSet("gde ovde onde nigde negde svugde uvek vreme lep lepa lepo lepe lepi lepu lepog lepom dete deca dece deci decom mesto mesta mestu mestom")
)

const (
	// minimumEkavian forms before a verdict, so a short or neutral text never
	// counts as Serbian.
	minimumEkavian = 5
	// ekavianRatio: ekavian forms must outnumber ijekavian ones at least this
	// many times. Mixed texts (say, a Bosnian or carelessly edited one) pass.
	ekavianRatio = 10
)

// Reasons a subtitle's text does not match its language tag.
const (
	// ReasonEkavian: Serbian ekavian text tagged Croatian or Bosnian.
	ReasonEkavian = "serbian_ekavian"
	// ReasonNotSouthSlavic: text in another language altogether, such as a
	// Portuguese subtitle tagged Croatian.
	ReasonNotSouthSlavic = "not_south_slavic"
)

// functionWords are short South Slavic words common in ordinary dialogue.
// Words that are just as common in English ("i", "to", "a", "on", "me") are
// left out, so English text scores low too. On the library every Croatian
// subtitle scored at least 21% (median 33%) and a Portuguese one tagged
// Croatian 2.8%; see minimumFunctionShare.
var functionWords = wordSet("je da se ne u na sam si smo ste su nije nisam nisi bi bih će ću ćeš ćemo li ali ili što šta kako ovo ono tako sve ja ti ona mi vi oni te ga mu joj nas vas ih za od sa s iz po pa još samo kad kada gdje gde tu tamo bilo bio bila biti ima nema možda hvala dobro")

const (
	// minimumFunctionShare of words below which the text is not South Slavic.
	minimumFunctionShare = 0.12
	// minimumWords before the share is judged, so a short text never is.
	minimumWords = 200
)

// Check reports whether text tagged as language is in another language, and
// why. Only Croatian (hr) and Bosnian (bs) are checked. Words are split on
// Unicode letters, so a form never matches inside a longer word.
func Check(language, text string) (string, bool) {
	base, _, _ := strings.Cut(strings.ToLower(language), "-")
	if base != "hr" && base != "bs" {
		return "", false
	}
	ij, ek, function, total := 0, 0, 0, 0
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		total++
		if functionWords[word] {
			function++
		}
		switch {
		case ijekavian[word]:
			ij++
		case ekavian[word]:
			ek++
		}
	}
	if total >= minimumWords && float64(function) < minimumFunctionShare*float64(total) {
		return ReasonNotSouthSlavic, true
	}
	if ek >= minimumEkavian && ij*ekavianRatio <= ek {
		return ReasonEkavian, true
	}
	return "", false
}

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}
