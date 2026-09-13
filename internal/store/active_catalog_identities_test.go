package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"subsyncd/internal/domain"
)

func TestListActiveCatalogIdentities(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()

	insertEpisode := func(t *testing.T, instance string, entityID, seriesID int64, deleted bool) {
		t.Helper()
		media := testMedia()
		media.Ref.Instance = instance
		media.EntityID = entityID
		media.Ref.FileID = entityID + 1000
		media.Fingerprint.FileID = media.Ref.FileID
		media.Fingerprint.Path = filepath.Join("/media", instance, "episode-"+strconv.FormatInt(entityID, 10)+".mkv")
		media.SeriesID = seriesID
		id, _, err := repo.UpsertMedia(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		value := 0
		if deleted {
			value = 1
		}
		if _, err := repo.store.db.ExecContext(ctx, `UPDATE media SET series_id=?, deleted=? WHERE id=?`, seriesID, value, id); err != nil {
			t.Fatal(err)
		}
	}

	insertMovie := func(t *testing.T, instance string, entityID int64, deleted bool) {
		t.Helper()
		media := testMedia()
		media.Ref.Instance = instance
		media.Ref.Kind = domain.MediaMovie
		media.EntityID = entityID
		media.Ref.FileID = entityID + 2000
		media.Fingerprint.FileID = media.Ref.FileID
		media.Fingerprint.Path = filepath.Join("/media", instance, "movie-"+strconv.FormatInt(entityID, 10)+".mkv")
		media.SeriesID = 0
		id, _, err := repo.UpsertMedia(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		value := 0
		if deleted {
			value = 1
		}
		if _, err := repo.store.db.ExecContext(ctx, `UPDATE media SET deleted=? WHERE id=?`, value, id); err != nil {
			t.Fatal(err)
		}
	}

	insertEpisode(t, "sonarr-main", 10, 300, false)
	insertEpisode(t, "sonarr-main", 11, 300, false)
	insertEpisode(t, "sonarr-main", 12, 100, false)
	insertEpisode(t, "sonarr-main", 13, 400, true)
	insertEpisode(t, "sonarr-main", 14, 0, false)
	insertEpisode(t, "sonarr-other", 15, 999, false)
	insertMovie(t, "sonarr-main", 16, false)
	insertMovie(t, "radarr-main", 40, false)
	insertMovie(t, "radarr-main", 5, false)
	insertMovie(t, "radarr-main", 50, true)

	tests := []struct {
		name     string
		instance string
		kind     domain.MediaKind
		want     []int64
	}{
		{name: "episodes use active positive series IDs", instance: "sonarr-main", kind: domain.MediaEpisode, want: []int64{100, 300}},
		{name: "movies use active positive entity IDs", instance: "radarr-main", kind: domain.MediaMovie, want: []int64{5, 40}},
		{name: "instance isolation", instance: "sonarr-other", kind: domain.MediaEpisode, want: []int64{999}},
		{name: "kind isolation", instance: "sonarr-main", kind: domain.MediaMovie, want: []int64{16}},
		{name: "empty active set", instance: "radarr-main", kind: domain.MediaEpisode, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.ListActiveCatalogIdentities(ctx, tt.instance, tt.kind)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identities = %#v, want %#v", got, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		name     string
		instance string
		kind     domain.MediaKind
	}{
		{name: "missing instance", instance: "", kind: domain.MediaMovie},
		{name: "invalid kind", instance: "sonarr-main", kind: domain.MediaKind("book")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := repo.ListActiveCatalogIdentities(ctx, tt.instance, tt.kind); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
