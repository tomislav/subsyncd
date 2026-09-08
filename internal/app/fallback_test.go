package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type unsupportedFallbackProvider struct{ fakeProvider }

func (unsupportedFallbackProvider) SupportsLanguage(domain.Language) bool { return false }

func fallbackAppConfig(t *testing.T) config.Config {
	cfg := testConfig(t)
	cfg.Providers["backup"] = cfg.Providers["english"]
	cfg.Providers["third"] = cfg.Providers["english"]
	return cfg
}

func fallbackAppOptions() Options {
	return Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}, "backup": fakeProvider{id: "backup"}, "third": fakeProvider{id: "third"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
}

func TestFallbackCoordinatorsPreserveLanguageTiers(t *testing.T) {
	cfg := fallbackAppConfig(t)
	cfg.Languages["en"] = config.LanguageConfig{Providers: []string{"english"}, FallbackProviders: []string{"backup", "third"}}
	cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"third", "backup"}, FallbackProviders: []string{"english"}}
	cfg.Languages["de"] = config.LanguageConfig{Providers: []string{"english"}}
	a, err := New(t.Context(), cfg, fallbackAppOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for language, route := range cfg.Languages {
		service := a.Workflows[language]
		if !reflect.DeepEqual(service.ProviderOrder, route.Providers) || !reflect.DeepEqual(service.FallbackProviderOrder, route.FallbackProviders) {
			t.Fatalf("%s orders = %v/%v", language, service.ProviderOrder, service.FallbackProviderOrder)
		}
		primary := service.Searcher.(*provider.Coordinator)
		var ids []string
		for _, p := range primary.Providers {
			ids = append(ids, p.ID())
		}
		if !reflect.DeepEqual(ids, route.Providers) {
			t.Fatalf("%s primary = %v", language, ids)
		}
		if len(route.FallbackProviders) == 0 {
			if service.FallbackSearcher != nil {
				t.Fatal("unexpected fallback coordinator")
			}
			continue
		}
		fallback, ok := service.FallbackSearcher.(*provider.Coordinator)
		if !ok {
			t.Fatalf("%s fallback missing", language)
		}
		ids = nil
		for _, p := range fallback.Providers {
			ids = append(ids, p.ID())
		}
		if !reflect.DeepEqual(ids, route.FallbackProviders) || primary == fallback || fallback.Cache != primary.Cache || fallback.Clock != primary.Clock || fallback.Events != primary.Events {
			t.Fatalf("%s fallback wiring = %#v", language, fallback)
		}
	}
}

func TestUnsupportedFallbackLanguageFailsAssembly(t *testing.T) {
	cfg := fallbackAppConfig(t)
	cfg.Languages["en"] = config.LanguageConfig{Providers: []string{"english"}, FallbackProviders: []string{"backup"}}
	options := fallbackAppOptions()
	options.Providers["backup"] = unsupportedFallbackProvider{fakeProvider{id: "backup"}}
	a, err := New(t.Context(), cfg, options)
	if a != nil {
		a.Close()
	}
	if err == nil || !strings.Contains(err.Error(), `provider "backup" does not support language en`) {
		t.Fatalf("New error = %v", err)
	}
}

func TestExplainIncludesFallbackProvenanceAndProviderStates(t *testing.T) {
	cfg := fallbackAppConfig(t)
	cfg.Languages["en"] = config.LanguageConfig{Providers: []string{"english"}, FallbackProviders: []string{"backup"}}
	a, err := New(t.Context(), cfg, fallbackAppOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	now := a.Clock.Now()
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Movie.mkv"), FileID: 7, Size: 100, ModTime: now}, Title: "Movie"}
	id, _, err := a.Repository.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Repository.RecordInstallation(ctx, store.Installation{MediaID: id, Language: "en", Path: filepath.Join(cfg.MediaRoots[0], "Movie.en.srt"), Checksum: "sum", ProviderID: "backup", CandidateID: "one", Fallback: true, MediaPath: media.Fingerprint.Path, MediaFileID: 7, MediaSize: 100, MediaModTimeNS: now.UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"english", "backup"} {
		if err := a.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: id, Scope: "search", Reason: "rate_limit", ResetAt: now.Add(1)}); err != nil {
			t.Fatal(err)
		}
	}
	output, err := a.Explain(ctx, "tv", "movie", 7, "en")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fallback=true", "provider_state: provider=english", "provider_state: provider=backup"} {
		if !strings.Contains(output, want) {
			t.Fatalf("Explain missing %q: %s", want, output)
		}
	}
}

func TestManualFallbackSearchPersistsPromotionCheck(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			cfg := fallbackAppConfig(t)
			cfg.Languages["en"] = config.LanguageConfig{Providers: []string{"backup"}, FallbackProviders: []string{"english"}}
			mediaPath := filepath.Join(cfg.MediaRoots[0], "Movie.mkv")
			if err := os.WriteFile(mediaPath, []byte("media"), 0600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(mediaPath)
			if err != nil {
				t.Fatal(err)
			}
			media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Title: "Movie", Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}}
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			options := fallbackAppOptions()
			options.Clock = testutil.NewClock(now)
			options.ProbeRunner = notificationProbe{}
			options.Providers["english"] = notificationProvider{}
			options.Catalogs["tv"] = staticCatalog{media: media}
			a, err := New(t.Context(), cfg, options)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			id, _, err := a.Repository.UpsertMedia(t.Context(), media)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				if err := a.Repository.UpsertSearchStateWithPriority(t.Context(), id, "en", now, store.SearchPriorityUpgrade); err != nil {
					t.Fatal(err)
				}
				leases, err := a.Repository.LeaseDueSearches(t.Context(), now, 1, time.Minute)
				if err != nil || len(leases) != 1 {
					t.Fatalf("lease=%+v err=%v", leases, err)
				}
				if _, err := a.Repository.CompleteSearch(t.Context(), store.SearchCompletion{JobID: leases[0].JobID, Outcome: "satisfied"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.Search(t.Context(), "tv", "movie", 7, "en", false); err != nil {
				t.Fatal(err)
			}
			status, err := a.Repository.GetSearchStatus(t.Context(), id, "en")
			if err != nil || status.State != "pending" || status.Priority != store.SearchPriorityUpgrade || !status.NextAttemptAt.Equal(now.Add(7*24*time.Hour)) {
				t.Fatalf("status=%+v err=%v", status, err)
			}
		})
	}
}
