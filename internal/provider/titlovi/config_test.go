package titlovi

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestConfigAndLanguageMappings(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail")
	}
	if _, err := DecodeConfig([]byte("type: titlovi\nusername: user\npassword: pass\nunexpected: true\n")); err == nil {
		t.Fatal("unknown field should fail")
	}
	config, err := DecodeConfig([]byte("type: titlovi\nusername: user\npassword: pass\n"))
	if err != nil || config.MaxPages != 5 || config.MaxDownloadBytes <= 0 {
		t.Fatalf("config = %#v, %v", config, err)
	}
	tests := map[domain.Language]string{"hr": "Hrvatski", "en": "English", "bs": "Bosanski", "sr": "Srpski", "sr-Cyrl": "Cirilica", "sl": "Slovenski", "mk": "Makedonski"}
	for language, providerCode := range tests {
		if got, ok := titloviLanguage(language); !ok || got != providerCode {
			t.Errorf("to Titlovi %s = %q, %v", language, got, ok)
		}
		if got, err := fromTitloviLanguage(providerCode); err != nil || got != language {
			t.Errorf("from Titlovi %q = %s, %v", providerCode, got, err)
		}
	}
	for _, alias := range []string{"Hrvatski", "Croatian", "hr", "hrv"} {
		if got, err := fromTitloviLanguage(alias); err != nil || got != "hr" {
			t.Errorf("Croatian alias %q = %s, %v", alias, got, err)
		}
	}
}
