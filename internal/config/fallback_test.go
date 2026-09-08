package config

import (
	"strings"
	"testing"
)

func TestFallbackProviderRoutes(t *testing.T) {
	for _, tt := range []struct{ name, route, want string }{
		{"valid", "providers: [subdl-main], fallback_providers: [backup]", ""},
		{"unknown", "providers: [subdl-main], fallback_providers: [unknown]", "unknown provider"},
		{"duplicate across tiers", "providers: [subdl-main], fallback_providers: [subdl-main]", "repeats provider"},
		{"duplicate fallback", "providers: [subdl-main], fallback_providers: [backup, backup]", "repeats provider"},
		{"primary required", "providers: [], fallback_providers: [backup]", "requires at least one provider"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := validConfig(t.TempDir(), "languages:\n  en: {"+tt.route+"}\n")
			input = strings.Replace(input, "providers:\n  subdl-main:", "providers:\n  backup: {type: subdl, api_key: backup}\n  subdl-main:", 1)
			cfg, err := loadText(t, input)
			if tt.want != "" {
				assertErrorContains(t, err, tt.want)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			route := cfg.Languages["en"]
			if strings.Join(route.AllProviders(), ",") != "subdl-main,backup" {
				t.Fatalf("route = %#v", route)
			}
			all := route.AllProviders()
			all[0] = "changed"
			all[1] = "changed"
			if route.Providers[0] != "subdl-main" || route.FallbackProviders[0] != "backup" {
				t.Fatal("AllProviders aliases source slices")
			}
		})
	}
}
