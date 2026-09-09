package gestdown

import "testing"

func TestConfigRequiresNoKeyAndRejectsUnsafeSettings(t *testing.T) {
	c, err := DecodeConfig([]byte("type: gestdown\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://api.gestdown.info" || c.MaxDownloadBytes != 20<<20 {
		t.Fatalf("defaults %+v", c)
	}
	for _, raw := range []string{"api_key: secret", "base_url: http://example.test", "base_url: https://user:pass@example.test", "base_url: https://example.test/?key=secret", "base_url: https://example.test/path", "max_download_bytes: 20971521", "max_concurrent: -1", "unknown: true"} {
		if _, err := DecodeConfig([]byte("type: gestdown\n" + raw + "\n")); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestLanguageVariantsStayDistinct(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"English", "en"}, {"Croatian", "hr"}, {"Portuguese", "pt"}, {"Portuguese (Brazilian)", "pt-BR"}, {"Portuguese (Brazil)", "pt-BR"}, {"French (Canadian)", "fr-CA"}, {"French", "fr"}, {"unknown", ""},
	} {
		got, ok := returnedLanguage(tc.raw)
		if string(got) != tc.want || ok != (tc.want != "") {
			t.Fatalf("%q => %q %v", tc.raw, got, ok)
		}
	}
	c := &Client{}
	if c.SupportsLanguage("en-US") || c.SupportsLanguage("sr-Cyrl") {
		t.Fatal("unmapped variant accepted")
	}
}
