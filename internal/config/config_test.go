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

func TestLoadRejectsSecondYAMLDocumentBeforeEnvironmentExpansion(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n") + "\n---\nsecret: ${SECOND_DOCUMENT_SECRET}\n"
	_, err := loadTextWithLookup(t, text, func(name string) (string, bool) {
		if name == "TEST_SUBDL_KEY" {
			return "provider-secret", true
		}
		if name == "SECOND_DOCUMENT_SECRET" {
			return "must-not-appear", true
		}
		return "", false
	})
	assertErrorContains(t, err, "exactly one", "yaml", "document")
	if strings.Contains(strings.ToLower(err.Error()), "must-not-appear") {
		t.Fatalf("error leaked expanded trailing document: %v", err)
	}
}

func TestLoadAcceptsEmptyTrailingYAMLSeparator(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n") + "\n---\n"
	if _, err := loadText(t, text); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsExplicitTaggedNullTrailingYAMLDocuments(t *testing.T) {
	root := t.TempDir()
	base := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n")
	for _, document := range []string{"!!null \"\"", "!!null", "&empty !!null \"\""} {
		t.Run(document, func(t *testing.T) {
			_, err := loadText(t, base+"\n---\n"+document+"\n")
			assertErrorContains(t, err, "exactly one", "yaml", "document")
		})
	}
}

func TestLoadRejectsNonEmptyDocumentAfterMultipleEmptySeparators(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, "languages:\n  en: {providers: [subdl-main]}\n") + "\n---\n\n---\nsecret: ${LATE_DOCUMENT_SECRET}\n"
	_, err := loadTextWithLookup(t, text, func(name string) (string, bool) {
		if name == "LATE_DOCUMENT_SECRET" {
			return "must-not-appear", true
		}
		return "", false
	})
	assertErrorContains(t, err, "exactly one", "yaml", "document")
	if strings.Contains(strings.ToLower(err.Error()), "must-not-appear") {
		t.Fatalf("error leaked expanded trailing document: %v", err)
	}
}

func TestExampleConfigurationLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"), func(name string) (string, bool) {
		if name == "SUBSYNCD_LOG_LEVEL" {
			return "", false
		}
		return "example-secret", true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Instances) != 2 || len(cfg.Providers) != 3 || len(cfg.Languages) != 2 || cfg.Install.FileMode.Perm() != 0o644 {
		t.Fatalf("example config decoded incompletely: instances=%d providers=%d languages=%d mode=%o", len(cfg.Instances), len(cfg.Providers), len(cfg.Languages), cfg.Install.FileMode.Perm())
	}
}

func TestLoadDefaultsHearingImpairedToDisallowedAndHonorsExplicitTrue(t *testing.T) {
	root := t.TempDir()
	cfg, err := loadText(t, validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowHearingImpaired {
		t.Fatal("omitted allow_hearing_impaired should default to false")
	}

	text := validConfig(root, `
allow_hearing_impaired: true
languages:
  en: {providers: [subdl-main]}
`)
	cfg, err = loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowHearingImpaired {
		t.Fatal("explicit allow_hearing_impaired: true was ignored")
	}
}

func TestWorkerMaxConcurrentDefaultsAndAcceptsExplicitValue(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name  string
		block string
		want  int
	}{
		{name: "omitted", want: 1},
		{name: "explicit", block: "worker:\n  max_concurrent: 4\n", want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadText(t, validConfig(root, test.block+`languages:
  en: {providers: [subdl-main]}
`))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Worker.MaxConcurrent != test.want {
				t.Fatalf("worker max_concurrent = %d, want %d", cfg.Worker.MaxConcurrent, test.want)
			}
		})
	}
}

func TestWorkerMaxConcurrentRejectsExplicitOutOfRangeValues(t *testing.T) {
	root := t.TempDir()
	for _, value := range []int{0, -1, 9} {
		t.Run(fmt.Sprintf("value_%d", value), func(t *testing.T) {
			_, err := loadText(t, validConfig(root, fmt.Sprintf(`worker:
  max_concurrent: %d
languages:
  en: {providers: [subdl-main]}
`, value)))
			assertErrorContains(t, err, "worker", "max_concurrent", "1", "8")
		})
	}
}

func TestLoadLoggingLevelDefaultsNormalizesAndHonorsEnvironmentOverride(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name      string
		yamlLevel string
		envLevel  string
		want      string
	}{
		{name: "default", want: "info"},
		{name: "yaml debug", yamlLevel: "DeBuG", want: "debug"},
		{name: "yaml info", yamlLevel: "info", want: "info"},
		{name: "yaml warn", yamlLevel: "WARN", want: "warn"},
		{name: "yaml error", yamlLevel: "error", want: "error"},
		{name: "environment wins", yamlLevel: "warn", envLevel: "error", want: "error"},
		{name: "empty environment is ignored", yamlLevel: "warn", envLevel: " ", want: "warn"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			block := ""
			if test.yamlLevel != "" {
				block = fmt.Sprintf("logging:\n  level: %s\n", test.yamlLevel)
			}
			cfg, err := loadTextWithLookup(t, validConfig(root, block+`languages:
  en: {providers: [subdl-main]}
`), func(name string) (string, bool) {
				switch name {
				case "TEST_SUBDL_KEY":
					return "secret", true
				case "SUBSYNCD_LOG_LEVEL":
					if test.envLevel != "" {
						return test.envLevel, true
					}
				}
				return "", false
			})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Logging.Level != test.want {
				t.Fatalf("logging level = %q, want %q", cfg.Logging.Level, test.want)
			}
		})
	}
}

func TestLoadLoggingLevelRejectsUnknownValuesWithoutEchoingEnvironment(t *testing.T) {
	root := t.TempDir()
	_, err := loadTextWithLookup(t, validConfig(root, `logging:
  level: trace
languages:
  en: {providers: [subdl-main]}
`), func(name string) (string, bool) {
		if name == "TEST_SUBDL_KEY" {
			return "secret", true
		}
		return "", false
	})
	assertErrorContains(t, err, "log level", "debug", "info", "warn", "error")

	const invalidOverride = "verbose-SENSITIVE-SENTINEL"
	_, err = loadTextWithLookup(t, validConfig(root, `logging:
  level: info
languages:
  en: {providers: [subdl-main]}
`), func(name string) (string, bool) {
		switch name {
		case "TEST_SUBDL_KEY":
			return "secret", true
		case "SUBSYNCD_LOG_LEVEL":
			return invalidOverride, true
		default:
			return "", false
		}
	})
	assertErrorContains(t, err, "subsyncd_log_level", "invalid")
	if strings.Contains(err.Error(), invalidOverride) {
		t.Fatalf("error echoed invalid environment value: %v", err)
	}
}

func TestValidateRejectsManuallyConstructedUnknownLoggingLevel(t *testing.T) {
	root := t.TempDir()
	cfg, err := loadText(t, validConfig(root, `languages:
  en: {providers: [subdl-main]}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Logging.Level = "trace"
	assertErrorContains(t, cfg.Validate(), "log level")
}

func TestLoadParsesLapseConfidencePolicy(t *testing.T) {
	root := t.TempDir()
	text := strings.Replace(validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`), "sync: {lapse_path: /usr/local/bin/lapse, timeout: 30m}", `sync:
  lapse_path: /usr/local/bin/lapse
  timeout: 30m
  policy: confidence
  bypass_score: 80
  require_identity_anchor: false
  require_episode_evidence: false
  require_release_group: false
  lapse_for_packs: false
  lapse_for_upgrades: false`, 1)
	cfg, err := loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Policy != "confidence" || cfg.Sync.BypassScore != 80 || cfg.Sync.RequireIdentityAnchor || cfg.Sync.RequireEpisodeEvidence || cfg.Sync.RequireReleaseGroup || cfg.Sync.LapseForPacks || cfg.Sync.LapseForUpgrades {
		t.Fatalf("sync confidence policy = %#v", cfg.Sync)
	}
}

func TestLoadDefaultsToConservativeLapseConfidencePolicy(t *testing.T) {
	root := t.TempDir()
	cfg, err := loadText(t, validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Policy != "confidence" || cfg.Sync.BypassScore != 75 || !cfg.Sync.RequireIdentityAnchor || !cfg.Sync.RequireEpisodeEvidence || !cfg.Sync.RequireReleaseGroup || !cfg.Sync.LapseForPacks || !cfg.Sync.LapseForUpgrades {
		t.Fatalf("default sync confidence policy = %#v", cfg.Sync)
	}
}

func TestLoadRejectsInvalidLapseConfidencePolicy(t *testing.T) {
	root := t.TempDir()
	base := validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`)
	for _, replacement := range []string{
		"sync: {lapse_path: /usr/local/bin/lapse, timeout: 30m, policy: sometimes}",
		"sync: {lapse_path: /usr/local/bin/lapse, timeout: 30m, bypass_score: 101}",
	} {
		_, err := loadText(t, strings.Replace(base, "sync: {lapse_path: /usr/local/bin/lapse, timeout: 30m}", replacement, 1))
		assertErrorContains(t, err, "sync")
	}
}

func TestLoadParsesInstallModeAndOwnership(t *testing.T) {
	root := t.TempDir()
	text := validConfig(root, `
install:
  file_mode: "0640"
  uid: 1000
  gid: 1001
languages:
  en: {providers: [subdl-main]}
`)
	cfg, err := loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Install.FileMode.Perm() != 0o640 || cfg.Install.UID == nil || *cfg.Install.UID != 1000 || cfg.Install.GID == nil || *cfg.Install.GID != 1001 {
		t.Fatalf("install config = %#v", cfg.Install)
	}
}

func TestLoadDefaultsInstallModeAndRejectsExecutableMode(t *testing.T) {
	root := t.TempDir()
	cfg, err := loadText(t, validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Install.FileMode.Perm() != 0o644 {
		t.Fatalf("default install mode = %o", cfg.Install.FileMode.Perm())
	}
	_, err = loadText(t, validConfig(root, `
install:
  file_mode: "0755"
languages:
  en: {providers: [subdl-main]}
`))
	assertErrorContains(t, err, "file_mode", "execute")
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

func TestValidateRejectsInvalidListenAddress(t *testing.T) {
	root := t.TempDir()
	text := strings.Replace(validConfig(root, `
languages:
  en: {providers: [subdl-main]}
`), `server: {listen: "127.0.0.1:8097"}`, `server: {listen: "missing-port"}`, 1)
	_, err := loadText(t, text)
	assertErrorContains(t, err, "listen", "invalid")
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

func TestValidateRejectsUnsafeSiloURLAndRelativePathMapping(t *testing.T) {
	root := t.TempDir()
	for _, silo := range []string{
		`silo: {enabled: true, url: "http://user:pass@silo:8080", api_key: key}`,
		`silo: {enabled: true, url: "http://silo:8080", api_key: key, path_mappings: [{from: relative, to: /mnt/media}]}`,
	} {
		block := "\n" + silo + "\nlanguages:\n  en: {providers: [subdl-main]}\n"
		_, err := loadText(t, validConfig(root, block))
		assertErrorContains(t, err, "silo")
	}
}

func loadText(t *testing.T, text string) (Config, error) {
	t.Helper()
	t.Setenv("SUBSYNCD_LOG_LEVEL", "")
	if _, ok := os.LookupEnv("TEST_SUBDL_KEY"); !ok {
		t.Setenv("TEST_SUBDL_KEY", "secret")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, os.LookupEnv)
}

func loadTextWithLookup(t *testing.T, text string, lookup func(string) (string, bool)) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, lookup)
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
