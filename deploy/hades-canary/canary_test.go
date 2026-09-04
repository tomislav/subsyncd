package canary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
)

func TestDeploymentIsManualPinnedAndRestricted(t *testing.T) {
	compose := readFile(t, "compose.yml")
	for _, required := range []string{
		"ghcr.io/tomislav/subsyncd:sha-6f0683a",
		`profiles: ["manual"]`,
		`restart: "no"`,
		`command: ["doctor", "--config", "/config/config.yaml"]`,
		`/srv/media/movies/1917 (2019) [tmdbid-530915]`,
		`/srv/media/movies/Arrival (2016) [tmdbid-329865]`,
		`/srv/media/tv/1883 (2021) [tvdbid-396390]/Season 01`,
		`/srv/media/tv/3 Body Problem (2024) [tvdbid-411959]/Season 01`,
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("compose.yml is missing %q", required)
		}
	}
	for _, forbidden := range []string{"latest", "subdl", "SILO_API_KEY"} {
		if strings.Contains(strings.ToLower(compose), strings.ToLower(forbidden)) {
			t.Errorf("compose.yml contains forbidden automatic/broad setting %q", forbidden)
		}
	}

	cfg, err := config.Load(filepath.Join("config", "config.yaml"), func(string) (string, bool) {
		return "canary-test-secret", true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MediaRoots) != 5 {
		t.Fatalf("media roots = %d, want four selected library roots plus read-only /mnt", len(cfg.MediaRoots))
	}
	if len(cfg.Providers) != 2 || cfg.Providers["titlovi-main"].Type != "titlovi" || cfg.Providers["opensubtitles-main"].Type != "opensubtitles" {
		t.Fatalf("providers are not restricted to Titlovi and OpenSubtitles: %#v", cfg.Providers)
	}
	if got := cfg.Languages[domain.Language("hr")].Providers; len(got) != 1 || got[0] != "titlovi-main" {
		t.Fatalf("Croatian providers = %#v", got)
	}
	if got := cfg.Languages[domain.Language("en")].Providers; len(got) != 1 || got[0] != "opensubtitles-main" {
		t.Fatalf("English providers = %#v", got)
	}
	if cfg.MinimumReleaseScore != 35 || cfg.Sync.Policy != "always" || cfg.Silo.Enabled || cfg.AllowHearingImpaired {
		t.Fatalf("unsafe canary policy: minimum_score=%d sync=%q silo=%t hearing_impaired=%t", cfg.MinimumReleaseScore, cfg.Sync.Policy, cfg.Silo.Enabled, cfg.AllowHearingImpaired)
	}
	if len(cfg.Instances) != 2 || len(cfg.Instances[0].PathMappings)+len(cfg.Instances[1].PathMappings) != 4 {
		t.Fatalf("expected two instances and four exact path mappings: %#v", cfg.Instances)
	}
	for _, instance := range cfg.Instances {
		for _, mapping := range instance.PathMappings {
			if !strings.HasSuffix(mapping.Remote, ".mkv") || !strings.HasSuffix(mapping.Local, ".mkv") {
				t.Errorf("path mapping is broader than one media file: %#v", mapping)
			}
		}
	}

	runbook := readFile(t, "README.md")
	for _, required := range []string{
		"1440", "1168", "9864", "10146", "Do not run `scan`", "Silo remains disabled",
		"`config/`: `root:1000` and `0750`", "`config/config.yaml`: `root:1000` and `0640`", "`.env`: `root:root` and `0600`",
	} {
		if !strings.Contains(runbook, required) {
			t.Errorf("README.md is missing %q", required)
		}
	}
	if rootReadme := readFile(t, filepath.Join("..", "..", "README.md")); !strings.Contains(rootReadme, "deploy/hades-canary") {
		t.Error("root README does not link to the Hades canary runbook")
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
