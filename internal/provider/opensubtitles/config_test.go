package opensubtitles

import (
	"strings"
	"testing"
)

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

func TestDecodeConfigUsesBuiltInAPIKeyUnlessOverridden(t *testing.T) {
	original := builtInAPIKey
	builtInAPIKey = "application-key"
	t.Cleanup(func() { builtInAPIKey = original })

	config, err := DecodeConfig([]byte("type: opensubtitles\nusername: user\npassword: pass\nuser_agent: subsyncd/1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.APIKey != "application-key" {
		t.Fatalf("default api key = %q", config.APIKey)
	}

	overridden, err := DecodeConfig([]byte("type: opensubtitles\napi_key: wrapper-key\nusername: user\npassword: pass\nuser_agent: subsyncd/1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if overridden.APIKey != "wrapper-key" {
		t.Fatalf("override api key = %q", overridden.APIKey)
	}
}

func TestDecodeConfigRequiresAPIKeyWhenBuildHasNoDefault(t *testing.T) {
	original := builtInAPIKey
	builtInAPIKey = ""
	t.Cleanup(func() { builtInAPIKey = original })

	_, err := DecodeConfig([]byte("type: opensubtitles\nusername: user\npassword: pass\nuser_agent: subsyncd/1\n"))
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("DecodeConfig() error = %v", err)
	}
}
