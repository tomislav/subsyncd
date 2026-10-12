package store

import (
	"context"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestRenameRebasesOnlyTheRenamedFilesInstallations(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint.Path = "/media/Show.S01E01.WEB.mkv"
	media.Fingerprint.ModTime = now
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"en", "hr"}, At: now}); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	// en was installed for this file; hr for a file it replaced.
	for _, installation := range []Installation{
		{MediaID: mediaID, Language: "en", Path: "/media/Show.S01E01.WEB.en.srt", Checksum: "en", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()},
		{MediaID: mediaID, Language: "hr", Path: "/media/Show.S01E01.WEB.hr.srt", Checksum: "hr", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()},
	} {
		if err := repo.RecordInstallation(ctx, installation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.store.db.Exec(`UPDATE installations SET media_file_id=? WHERE language='hr'`, media.Fingerprint.FileID-1); err != nil {
		t.Fatal(err)
	}
	renamed := media
	renamed.Fingerprint.Path = "/media/Show.S01E01.Proper.mkv"
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "rename", Type: "rename", EntityID: media.EntityID, Media: renamed, Ref: renamed.Ref, Languages: []domain.Language{"en", "hr"}, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	en, _, err := repo.GetInstallation(ctx, mediaID, "en")
	if err != nil || en.Path != "/media/Show.S01E01.Proper.en.srt" || en.MediaPath != media.Fingerprint.Path {
		t.Fatalf("en = %#v/%v, want the sidecar renamed and the media path kept for the workflow", en, err)
	}
	hr, _, err := repo.GetInstallation(ctx, mediaID, "hr")
	if err != nil || hr.Path != "/media/Show.S01E01.WEB.hr.srt" {
		t.Fatalf("hr = %#v/%v, want the replaced file's sidecar left as recorded", hr, err)
	}
}

func TestDropAndRebaseInstallationAreGuarded(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ProviderID: "p", CandidateID: "c", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	stale := installation
	stale.Checksum = "other"
	if err := repo.RebaseInstallation(ctx, stale, "/media/y.en.srt", "/media/y.mkv"); err == nil {
		t.Fatal("RebaseInstallation() changed a record that is no longer the one read")
	}
	if err := repo.DropInstallation(ctx, stale, "sidecar_missing", time.Now()); err == nil {
		t.Fatal("DropInstallation() dropped a record that is no longer the one read")
	}
	if err := repo.RebaseInstallation(ctx, installation, "/media/y.en.srt", "/media/y.mkv"); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(ctx, mediaID, "en")
	if err != nil || !found || got.Path != "/media/y.en.srt" || got.MediaPath != "/media/y.mkv" || got.MediaFileID != installation.MediaFileID || got.Checksum != "sum" {
		t.Fatalf("rebased = %#v/%v/%v", got, found, err)
	}
	if err := repo.DropInstallation(ctx, got, "sidecar_missing", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.GetInstallation(ctx, mediaID, "en"); err != nil || found {
		t.Fatalf("dropped installation found/error = %v/%v", found, err)
	}
	var audits int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_type='subtitle_record_dropped' AND outcome='sidecar_missing' AND media_id=?`, mediaID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("drop audit events = %d/%v", audits, err)
	}
}
