package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFallbackInstallationPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repo := db.Repository()
	media := testMedia()
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: id, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ProviderID: "backup", CandidateID: "one", Fallback: true, ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = db.Repository()
	check := func(want bool) {
		t.Helper()
		got, found, err := repo.GetInstallation(ctx, id, "en")
		if err != nil || !found || got.Fallback != want {
			t.Fatalf("installation = %#v, %v, %v; want fallback %v", got, found, err, want)
		}
	}
	check(true)
	installation.Fallback = false
	if err := repo.UpdateInstallationAssessment(ctx, installation); err != nil {
		t.Fatal(err)
	}
	check(false)
	installation.Fallback = true
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	check(true)
	media.Fingerprint.Size++
	if _, _, err := repo.UpsertMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := db.db.Exec(`UPDATE installations SET fallback=2`); err == nil {
		t.Fatal("nonboolean fallback accepted")
	}
}

func TestFallbackMigrationDefaultsExistingInstallationsToPrimary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repo := db.Repository()
	media := testMedia()
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: id, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	// Reproduce schema 005 with a retained installation before opening the new version.
	if _, err := db.db.Exec(`ALTER TABLE installations DROP COLUMN fallback`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DELETE FROM schema_migrations WHERE version='006_fallback_installations.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, found, err := db.Repository().GetInstallation(ctx, id, "en")
	if err != nil || !found || got.Fallback || got.Checksum != "sum" {
		t.Fatalf("migrated installation = %#v/%v/%v", got, found, err)
	}
}
