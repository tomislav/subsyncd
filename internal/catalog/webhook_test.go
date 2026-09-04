package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func TestNormalizeWebhookFixtures(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		file     string
		wantType EventType
		wantKind domain.MediaKind
		wantID   int64
	}{
		{"sonarr download", "sonarr", "sonarr_download.json", EventImport, domain.MediaEpisode, 1001},
		{"sonarr rename", "sonarr", "sonarr_rename.json", EventRename, domain.MediaEpisode, 1001},
		{"sonarr delete", "sonarr", "sonarr_delete.json", EventDelete, domain.MediaEpisode, 1001},
		{"radarr download", "radarr", "radarr_download.json", EventImport, domain.MediaMovie, 2001},
		{"radarr rename", "radarr", "radarr_rename.json", EventRename, domain.MediaMovie, 2001},
		{"radarr delete", "radarr", "radarr_delete.json", EventDelete, domain.MediaMovie, 2001},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatal(err)
			}
			first, err := NormalizeWebhook("main", test.kind, body)
			if err != nil {
				t.Fatal(err)
			}
			second, err := NormalizeWebhook("main", test.kind, body)
			if err != nil {
				t.Fatal(err)
			}
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("events = %d/%d, want 1/1", len(first), len(second))
			}
			event := first[0]
			if event.Type != test.wantType || event.Ref.Kind != test.wantKind || event.Ref.FileID != test.wantID {
				t.Fatalf("unexpected event: %#v", event)
			}
			if event.EventID == "" || event.EventID != second[0].EventID {
				t.Fatalf("event ID is not stable: %q / %q", event.EventID, second[0].EventID)
			}
		})
	}
}

func TestNormalizeWebhookIgnoresArrTestEvent(t *testing.T) {
	_, err := NormalizeWebhook("main", "sonarr", []byte(`{"eventType":"Test"}`))
	if !errors.Is(err, ErrIgnoredEvent) {
		t.Fatalf("error = %v, want ErrIgnoredEvent", err)
	}
}

func TestNormalizeWebhookPreservesUpgradeReleaseMetadata(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "radarr_download.json"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := NormalizeWebhook("radarr-main", "radarr", body)
	if err != nil {
		t.Fatal(err)
	}
	event := events[0]
	if !event.IsUpgrade || event.OriginalFilename != "Example.Movie.2024.2160p.WEB-DL-GROUP" || event.Quality != "WEBDL-2160p" || event.ReleaseGroup != "GROUP" {
		t.Fatalf("release metadata was lost: %#v", event)
	}
}

func TestNormalizeWebhookDistinguishesLaterRenameOfSameFile(t *testing.T) {
	first, err := NormalizeWebhook("main", "radarr", []byte(`{"eventType":"Rename","movieFile":{"id":2001,"path":"/movies/one.mkv"}}`))
	if err != nil {
		t.Fatal(err)
	}
	redelivery, err := NormalizeWebhook("main", "radarr", []byte(`{"eventType":"Rename","movieFile":{"id":2001,"path":"/movies/one.mkv"}}`))
	if err != nil {
		t.Fatal(err)
	}
	later, err := NormalizeWebhook("main", "radarr", []byte(`{"eventType":"Rename","movieFile":{"id":2001,"path":"/movies/two.mkv"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if first[0].EventID != redelivery[0].EventID || first[0].EventID == later[0].EventID {
		t.Fatalf("event IDs do not distinguish redelivery from later rename: %q %q %q", first[0].EventID, redelivery[0].EventID, later[0].EventID)
	}
}

type fakeEventCatalog struct {
	media domain.Media
	calls int
}

func (f *fakeEventCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	f.calls++
	return f.media, nil
}

func (f *fakeEventCatalog) ListMediaChangedSince(context.Context, time.Time) ([]domain.Media, error) {
	return nil, nil
}

type fakeEventStore struct {
	mutations []store.MediaEventMutation
	results   []bool
	errors    []error
}

func (f *fakeEventStore) ApplyMediaEvent(_ context.Context, mutation store.MediaEventMutation) (bool, error) {
	f.mutations = append(f.mutations, mutation)
	index := len(f.mutations) - 1
	if index < len(f.errors) && f.errors[index] != nil {
		return false, f.errors[index]
	}
	if index < len(f.results) {
		return f.results[index], nil
	}
	return true, nil
}

func TestWebhookHandlerHydratesImportsAndAppliesDeletesWithoutHydration(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	catalog := &fakeEventCatalog{media: domain.Media{Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	store := &fakeEventStore{}
	handler := WebhookHandler{Instance: "main", InstanceType: "sonarr", Catalog: catalog, Store: store, Languages: []domain.Language{"hr", "en"}, Now: func() time.Time { return now }}

	download, err := os.ReadFile(filepath.Join("testdata", "sonarr_download.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(context.Background(), download)
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != 1 || result.AppliedCount != 1 {
		t.Fatalf("download result = %#v", result)
	}
	deleted, err := os.ReadFile(filepath.Join("testdata", "sonarr_delete.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err = handler.Handle(context.Background(), deleted)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.calls != 1 {
		t.Fatalf("catalog calls = %d, want 1", catalog.calls)
	}
	if len(store.mutations) != 2 || store.mutations[0].Media.Title != "Show" || store.mutations[1].Type != "delete" {
		t.Fatalf("mutations = %#v", store.mutations)
	}
	if len(store.mutations[0].Languages) != 2 || !store.mutations[0].At.Equal(now) {
		t.Fatalf("import scheduling metadata = %#v", store.mutations[0])
	}
}

func TestWebhookHandlerOnAppliedTracksCommittedWork(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	catalog := &fakeEventCatalog{media: domain.Media{Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	eventStore := &fakeEventStore{results: []bool{true, false}}
	wakes := 0
	handler := WebhookHandler{Instance: "main", InstanceType: "sonarr", Catalog: catalog, Store: eventStore, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now }, OnApplied: func() { wakes++ }}
	body, err := os.ReadFile(filepath.Join("testdata", "sonarr_download.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != 1 || result.AppliedCount != 1 {
		t.Fatalf("first webhook result = %#v", result)
	}
	result, err = handler.Handle(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != 1 || result.AppliedCount != 0 {
		t.Fatalf("duplicate webhook result = %#v", result)
	}
	if wakes != 1 {
		t.Fatalf("wake callbacks = %d, want 1", wakes)
	}
	result, err = handler.Handle(context.Background(), []byte(`{"eventType":"Test"}`))
	if !errors.Is(err, ErrIgnoredEvent) {
		t.Fatalf("test event error = %v", err)
	}
	if result.EventCount != 0 || result.AppliedCount != 0 {
		t.Fatalf("ignored webhook result = %#v", result)
	}
	if wakes != 1 {
		t.Fatalf("ignored event changed wake callbacks to %d", wakes)
	}
}

func TestWebhookHandlerOnAppliedSurvivesLaterFileFailure(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	catalog := &fakeEventCatalog{media: domain.Media{Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	eventStore := &fakeEventStore{results: []bool{true}, errors: []error{nil, errors.New("disk full")}}
	wakes := 0
	handler := WebhookHandler{Instance: "main", InstanceType: "sonarr", Catalog: catalog, Store: eventStore, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now }, OnApplied: func() { wakes++ }}
	body := []byte(`{"eventType":"Download","episodeFiles":[{"id":1001,"path":"/tv/one.mkv"},{"id":1002,"path":"/tv/two.mkv"}]}`)
	result, err := handler.Handle(context.Background(), body)
	if err == nil {
		t.Fatal("Handle() error = nil")
	}
	if result.EventCount != 2 || result.AppliedCount != 1 {
		t.Fatalf("partial webhook result = %#v", result)
	}
	if wakes != 1 {
		t.Fatalf("wake callbacks = %d, want 1 after partial commit", wakes)
	}
}
