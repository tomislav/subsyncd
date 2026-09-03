package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func TestLoadCanonicalizesLanguagesAndExpandsSecrets(t *testing.T) {
	t.Setenv("TEST_SUBDL_KEY", "secret")
	root := t.TempDir()
	cfg, err := loadText(t, validConfig(root, `
languages:
  hr: {providers: [subdl-main]}
  en: {providers: [subdl-main]}
  pt-br: {providers: [subdl-main]}
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	for _, want := range []domain.Language{"hr", "en", "pt-BR"} {
		if _, ok := cfg.Languages[want]; !ok {
			t.Errorf("canonical language %q missing from %#v", want, cfg.Languages)
		}
	}

	var subdl struct {
		APIKey string `yaml:"api_key"`
	}
	settings := cfg.Providers["subdl-main"].Settings
	if err := settings.Decode(&subdl); err != nil {
		t.Fatalf("decode provider settings: %v", err)
	}
	if subdl.APIKey != "secret" {
		t.Errorf("expanded api_key = %q, want secret", subdl.APIKey)
	}
}

func TestValidateRejectsLanguageWithoutProviders(t *testing.T) {
	root := t.TempDir()
	_, err := loadText(t, validConfig(root, `
languages:
  en: {providers: []}
`))
	assertErrorContains(t, err, "language en", "provider")
}

func TestValidateRejectsUnknownProviderReference(t *testing.T) {
	root := t.TempDir()
	_, err := loadText(t, validConfig(root, `
languages:
  en: {providers: [missing]}
`))
	assertErrorContains(t, err, "missing", "provider")
}

func TestValidateRejectsDuplicateInstanceName(t *testing.T) {
	root := t.TempDir()
	extra := fmt.Sprintf(`
languages:
  en: {providers: [subdl-main]}
instances:
  - {name: sonarr-main, type: sonarr, url: "http://sonarr:8989", api_key: key, webhook_token: token, path_mappings: [{remote: /tv, local: %q}]}
  - {name: sonarr-main, type: sonarr, url: "http://sonarr-2:8989", api_key: key, webhook_token: token, path_mappings: [{remote: /tv, local: %q}]}
`, root, root)
	_, err := loadText(t, validConfigWithoutInstances(root, extra))
	assertErrorContains(t, err, "duplicate", "sonarr-main")
}

func TestValidateRejectsPathMappingOutsideMediaRoots(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside")
	extra := fmt.Sprintf(`
languages:
  en: {providers: [subdl-main]}
instances:
  - {name: sonarr-main, type: sonarr, url: "http://sonarr:8989", api_key: key, webhook_token: token, path_mappings: [{remote: /tv, local: %q}]}
`, outside)
	_, err := loadText(t, validConfigWithoutInstances(root, extra))
	assertErrorContains(t, err, "path mapping", "media root")
}

func TestValidateRejectsInvalidPackCacheLimits(t *testing.T) {
	root := t.TempDir()
	text := strings.Replace(validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`), "ttl: 24h", "ttl: 0s", 1)
	_, err := loadText(t, text)
	assertErrorContains(t, err, "pack cache", "ttl")
}

func TestValidateRejectsInvalidProviderRateLimits(t *testing.T) {
	root := t.TempDir()
	text := strings.Replace(validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`), "requests_per_second: 1", "requests_per_second: 0", 1)
	_, err := loadText(t, text)
	assertErrorContains(t, err, "subdl-main", "requests_per_second")
}

func loadText(t *testing.T, text string) (Config, error) {
	t.Helper()
	if _, ok := os.LookupEnv("TEST_SUBDL_KEY"); !ok {
		t.Setenv("TEST_SUBDL_KEY", "secret")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, os.LookupEnv)
}

func validConfig(root, languageBlock string) string {
	return validConfigWithoutInstances(root, fmt.Sprintf(`%s
instances:
  - {name: sonarr-main, type: sonarr, url: "http://sonarr:8989", api_key: key, webhook_token: token, path_mappings: [{remote: /tv, local: %q}]}
`, languageBlock, root))
}

func validConfigWithoutInstances(root, suffix string) string {
	return fmt.Sprintf(`
data_dir: %q
media_roots: [%q]
server: {listen: "127.0.0.1:8097"}
providers:
  subdl-main:
    type: subdl
    api_key: ${TEST_SUBDL_KEY}
    requests_per_second: 1
    burst: 1
    max_concurrent: 1
provider_http: {shared_origin_max_concurrent: 1}
pack_cache:
  ttl: 24h
  max_bytes: 536870912
sync: {lapse_path: /usr/local/bin/lapse, timeout: 30m}
%s
`, filepath.Join(root, "data"), root, suffix)
}

func assertErrorContains(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range fragments {
		if !strings.Contains(message, strings.ToLower(fragment)) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}
