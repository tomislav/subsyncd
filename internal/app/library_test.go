package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

type discoveryCatalog struct {
	fakeCatalog
	items []domain.Media
	calls int
	err   error
}

func (c *discoveryCatalog) ListLibrary(context.Context) ([]domain.Media, error) {
	c.calls++
	return c.items, c.err
}

func TestLibraryDiscoveryImportsExistingFilesOnceAndExplicitScanFindsMore(t *testing.T) {
	for _, kind := range []domain.MediaKind{domain.MediaEpisode, domain.MediaMovie} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			if kind == domain.MediaMovie {
				cfg.Instances[0].Type = "radarr"
			}
			media := domain.Media{EntityID: 10, SeriesID: 1, Ref: domain.MediaRef{Instance: "tv", Kind: kind, FileID: 100}, Fingerprint: domain.MediaFingerprint{FileID: 100, Path: filepath.Join(cfg.MediaRoots[0], "file.mkv"), Size: 100, ModTime: time.Now()}, Season: 1, Episode: 1}
			source := &discoveryCatalog{items: []domain.Media{media}}
			build := func() *App {
				a, err := New(ctx, cfg, Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": source}})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			first := build()
			defer first.Close()
			if source.calls != 0 {
				t.Fatal("assembly queried library")
			}
			if err := first.Reconcilers["tv"].Run(ctx); err != nil {
				t.Fatal(err)
			}
			id, _, err := first.Repository.FindMedia(ctx, media.Ref)
			if err != nil {
				t.Fatalf("empty history left existing library undiscovered: %v", err)
			}
			status, err := first.Repository.GetSearchStatus(ctx, id, "en")
			if err != nil || status.State != "pending" || status.Priority != store.SearchPriorityMissing {
				t.Fatalf("search = %#v, %v", status, err)
			}
			due := time.Now().Add(24 * time.Hour)
			if err := first.Repository.UpsertSearchStateWithPriority(ctx, id, "en", due, store.SearchPriorityUpgrade); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			second := build()
			defer second.Close()
			if err := second.Reconcilers["tv"].Run(ctx); err != nil {
				t.Fatal(err)
			}
			if source.calls != 1 {
				t.Fatalf("restart enumerated library again: %d", source.calls)
			}
			more := media
			more.EntityID = 11
			more.Ref.FileID = 101
			more.Fingerprint.FileID = 101
			more.Fingerprint.Path = filepath.Join(cfg.MediaRoots[0], "more.mkv")
			source.items = append(source.items, more)
			if _, err := second.Scan(ctx, "tv", false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := second.Repository.FindMedia(ctx, more.Ref); err != nil {
				t.Fatal(err)
			}
			status, err = second.Repository.GetSearchStatus(ctx, id, "en")
			if err != nil || status.Priority != store.SearchPriorityUpgrade || !status.NextAttemptAt.Equal(due) {
				t.Fatalf("rescan reset existing schedule: %#v, %v", status, err)
			}
			if source.calls != 2 {
				t.Fatalf("explicit scan did not enumerate once: %d", source.calls)
			}
		})
	}
}

func TestLibraryDiscoveryFailureRetriesAndScopeChangeReimports(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	source := &discoveryCatalog{err: errors.New("library unavailable")}
	build := func() *App {
		a, err := New(ctx, cfg, Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": source}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := build()
	defer a.Close()
	if err := a.Reconcilers["tv"].Run(ctx); err == nil {
		t.Fatal("missing library failure")
	}
	cursor, err := a.Repository.GetReconciliationCursor(ctx, "tv")
	if err != nil || !cursor.IsZero() {
		t.Fatalf("failed discovery advanced history: %v, %v", cursor, err)
	}
	source.err = nil
	if err := a.Reconcilers["tv"].Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Instances[0].PathMappings = []config.PathMapping{{Remote: "/tv", Local: cfg.MediaRoots[0]}}
	b := build()
	defer b.Close()
	if err := b.Reconcilers["tv"].Run(ctx); err != nil {
		t.Fatal(err)
	}
	if source.calls != 3 {
		t.Fatalf("scope change did not reimport: calls=%d", source.calls)
	}
}
