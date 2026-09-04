package opensubtitles

import "testing"

func TestConfigValidationRequiresCredentialsAndRejectsUnknownFields(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail")
	}
	if _, err := DecodeConfig([]byte("type: opensubtitles\napi_key: key\nusername: user\npassword: pass\nuser_agent: subsyncd/1\nunexpected: true\n")); err == nil {
		t.Fatal("unknown field should fail")
	}
	config, err := DecodeConfig([]byte("type: opensubtitles\napi_key: key\nusername: user\npassword: pass\nuser_agent: subsyncd/1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxDownloadBytes <= 0 || config.BaseURL == "" {
		t.Fatalf("defaults not applied: %#v", config)
	}
}
