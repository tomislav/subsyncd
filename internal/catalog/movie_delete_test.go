package catalog

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func TestMovieDeleteRetiresStoredEntityWithoutHydration(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := db.Repository()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ids := map[string]int64{}
	for _, instance := range []string{"main", "other"} {
		if err := repo.EnsureInstance(ctx, instance, "radarr", "http://radarr.invalid", now); err != nil {
			t.Fatal(err)
		}
		for _, entity := range []int64{42, 43} {
			media := domain.Media{EntityID: entity, Ref: domain.MediaRef{Instance: instance, Kind: domain.MediaMovie, FileID: entity + 1000}, Title: "Example", Fingerprint: domain.MediaFingerprint{Path: "/movies/example.mkv", FileID: entity + 1000, Size: 100, ModTime: now}}
			key := fmt.Sprintf("%s-%d", instance, entity)
			if _, err := repo.ApplyMediaEvent(ctx, store.MediaEventMutation{EventID: key, Type: "import", EntityID: entity, Ref: media.Ref, Media: media, Languages: []domain.Language{"en", "hr"}, At: now}); err != nil {
				t.Fatal(err)
			}
			id, _, err := repo.FindMedia(ctx, media.Ref)
			if err != nil {
				t.Fatal(err)
			}
			ids[key] = id
		}
	}
	catalog := &fakeEventCatalog{}
	wakes := 0
	h := WebhookHandler{Instance: "main", InstanceType: "radarr", Catalog: catalog, Store: repo, Now: func() time.Time { return now }, OnApplied: func() { wakes++ }}
	body := []byte(`{"eventType":"MovieDelete","movie":{"id":42,"title":"Example","folderPath":"/movies/example"},"deletedFiles":false}`)
	result, err := h.Handle(ctx, body)
	if err != nil || result.EventCount != 1 || result.AppliedCount != 1 {
		t.Fatalf("MovieDelete = %#v, %v", result, err)
	}
	for key, id := range ids {
		for _, lang := range []domain.Language{"en", "hr"} {
			status, err := repo.GetSearchStatus(ctx, id, lang)
			if err != nil {
				t.Fatal(err)
			}
			if key == "main-42" {
				if status.State != "complete" || status.LastOutcome != "deleted" {
					t.Fatalf("deleted status = %#v", status)
				}
			} else if status.State != "pending" {
				t.Fatalf("unrelated %s changed: %#v", key, status)
			}
		}
	}
	active, err := repo.ListMediaByInstance(ctx, "main")
	if err != nil || len(active) != 1 {
		t.Fatalf("active media = %#v, %v", active, err)
	}
	result, err = h.Handle(ctx, body)
	if err != nil || result.AppliedCount != 0 || wakes != 1 || catalog.calls != 0 {
		t.Fatalf("replay = %#v, %v; wakes=%d hydration=%d", result, err, wakes, catalog.calls)
	}
	// Unknown movie IDs are audited idempotently, without touching a file whose ID matches.
	result, err = h.Handle(ctx, []byte(`{"eventType":"MovieDelete","movie":{"id":1043},"deletedFiles":true}`))
	if err != nil || result.AppliedCount != 1 {
		t.Fatalf("unknown delete = %#v, %v", result, err)
	}
	status, err := repo.GetSearchStatus(ctx, ids["main-43"], "en")
	if err != nil || status.State != "pending" {
		t.Fatalf("unknown entity changed unrelated file: %#v, %v", status, err)
	}
}

func TestSeriesDeleteRetiresEveryEpisodeWithoutHydration(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := db.Repository()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "main", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 4)
	for i := range ids {
		series := int64(42)
		if i == 2 {
			series = 43
		}
		if i == 3 {
			series = 0
		}
		media := domain.Media{EntityID: int64(i + 100), SeriesID: series, Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: int64(i + 1000)}, Title: "Example", Fingerprint: domain.MediaFingerprint{Path: "/tv/example.mkv", FileID: int64(i + 1000), Size: 100, ModTime: now}}
		id, _, err := repo.UpsertMedia(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		// Exercise both the ordinary and event-transaction persistence paths.
		if _, err := repo.ApplyMediaEvent(ctx, store.MediaEventMutation{EventID: fmt.Sprint("seed", i), Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en", "hr"}, At: now}); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetMedia(ctx, id)
		if err != nil || got.SeriesID != series {
			t.Fatalf("series round trip = %#v, %v", got, err)
		}
	}
	leases, err := repo.LeaseDueSearches(ctx, now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &fakeEventCatalog{}
	h := WebhookHandler{Instance: "main", InstanceType: "sonarr", Catalog: catalog, Store: repo, Now: func() time.Time { return now }}
	body := []byte(`{"eventType":"SeriesDelete","series":{"id":42,"title":"Example","tvdbId":123,"path":"/tv/example"},"deletedFiles":false}`)
	result, err := h.Handle(ctx, body)
	if err != nil || result.AppliedCount != 1 {
		t.Fatalf("SeriesDelete = %#v, %v", result, err)
	}
	for _, lease := range leases {
		if _, err := repo.CompleteSearch(ctx, store.SearchCompletion{JobID: lease.JobID, Outcome: "no_result", NextAttemptAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range ids {
		for _, lang := range []domain.Language{"en", "hr"} {
			status, err := repo.GetSearchStatus(ctx, id, lang)
			if err != nil {
				t.Fatal(err)
			}
			if i < 2 {
				if status.State != "complete" || status.LastOutcome != "deleted" || status.RerunPending {
					t.Fatalf("deleted status=%#v", status)
				}
			} else if status.State != "pending" {
				t.Fatalf("unrelated status=%#v", status)
			}
		}
	}
	result, err = h.Handle(ctx, body)
	if err != nil || result.AppliedCount != 0 || catalog.calls != 0 {
		t.Fatalf("replay=%#v, %v; calls=%d", result, err, catalog.calls)
	}
}

func TestWholeDeletionRejectsMissingOrWrongIdentity(t *testing.T) {
	for _, test := range []struct{ kind, body string }{
		{"radarr", `{"eventType":"MovieDelete","movieFile":{"id":42}}`},
		{"radarr", `{"eventType":"MovieDelete","movie":{"id":0}}`},
		{"radarr", `{"eventType":"MovieDelete","movie":{"id":-1}}`},
		{"sonarr", `{"eventType":"SeriesDelete","episodeFile":{"id":42}}`},
		{"sonarr", `{"eventType":"SeriesDelete","series":{"id":0}}`},
		{"sonarr", `{"eventType":"SeriesDelete","series":{"id":-1}}`},
		{"sonarr", `{"eventType":"MovieDelete","movie":{"id":42}}`},
		{"radarr", `{"eventType":"SeriesDelete","series":{"id":42}}`},
	} {
		if _, err := NormalizeWebhook("main", test.kind, []byte(test.body)); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatalf("%s: error=%v", test.body, err)
		}
	}
	for _, kind := range []string{"radarr", "sonarr"} {
		body := `{"eventType":"MovieDelete","movie":{"id":42}}`
		other := `{"eventType":"MovieDelete","movie":{"id":43}}`
		if kind == "sonarr" {
			body = `{"eventType":"SeriesDelete","series":{"id":42}}`
			other = `{"eventType":"SeriesDelete","series":{"id":43}}`
		}
		first, err := NormalizeWebhook("main", kind, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		second, err := NormalizeWebhook("other", kind, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		third, err := NormalizeWebhook("main", kind, []byte(other))
		if err != nil {
			t.Fatal(err)
		}
		if first[0].EventID == second[0].EventID || first[0].EventID == third[0].EventID {
			t.Fatal("delete IDs collide across instances or entities")
		}
	}
}
