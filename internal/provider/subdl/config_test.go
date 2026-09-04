package subdl

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestConfigIsStrictAndMapsLanguages(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail")
	}
	if _, err := DecodeConfig([]byte("type: subdl\napi_key: secret\nunexpected: true\n")); err == nil {
		t.Fatal("unknown config field should fail")
	}
	config, err := DecodeConfig([]byte("type: subdl\napi_key: secret\n"))
	if err != nil || config.MaxDownloadBytes <= 0 {
		t.Fatalf("config = %#v, %v", config, err)
	}
	for language, want := range map[domain.Language]string{"en": "EN", "hr": "HR", "pt-BR": "BR_PT", "zh-Hant": "ZH_BG", "sr": "SR"} {
		if got, ok := subDLLanguage(language); !ok || got != want {
			t.Errorf("language %s = %q/%v, want %q", language, got, ok, want)
		}
	}
}
