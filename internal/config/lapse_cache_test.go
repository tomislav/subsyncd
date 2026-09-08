package config

import (
	"testing"
	"time"
)

func TestLapseCacheTTL(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		want        time.Duration
		invalid     bool
	}{
		{"default", "", 720 * time.Hour, false},
		{"configured", "lapse_cache:\n  ttl: 48h\n", 48 * time.Hour, false},
		{"zero", "lapse_cache:\n  ttl: 0s\n", 0, true},
		{"negative", "lapse_cache:\n  ttl: -1h\n", 0, true},
		{"invalid", "lapse_cache:\n  ttl: tomorrow\n", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadText(t, validConfig(t.TempDir(), "languages:\n  en: {providers: [subdl-main]}\n"+test.extra))
			if test.invalid {
				if err == nil {
					t.Fatal("expected invalid TTL")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LapseCache.TTL != test.want {
				t.Fatalf("TTL=%v want %v", cfg.LapseCache.TTL, test.want)
			}
		})
	}
}
