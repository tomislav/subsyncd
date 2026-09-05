package inventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type fakeRepository struct {
	record        store.InventoryRecord
	installations map[domain.Language]store.Installation
	replacements  int
}

func (f *fakeRepository) GetTrackInventory(context.Context, int64) (store.InventoryRecord, error) {
	return f.record, nil
}

func (f *fakeRepository) ReplaceTrackInventory(_ context.Context, _ int64, expected, fingerprint domain.MediaFingerprint, tracks []store.TrackRecord) error {
	f.replacements++
	f.record = store.InventoryRecord{CatalogFingerprint: fingerprint, ProbeFingerprint: &fingerprint, Tracks: tracks}
	return nil
}

func (f *fakeRepository) GetInstallation(_ context.Context, _ int64, language domain.Language) (store.Installation, bool, error) {
	installation, ok := f.installations[language]
	return installation, ok, nil
}

func TestRefreshReusesEmbeddedTracksButRescansSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Show.mkv")
	writeTestFile(t, path, "video")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := domain.MediaFingerprint{Path: path, FileID: 42, Size: info.Size(), ModTime: info.ModTime()}
	repository := &fakeRepository{record: store.InventoryRecord{CatalogFingerprint: fingerprint, ProbeFingerprint: &fingerprint, Tracks: []store.TrackRecord{{Index: 1, Language: "en", Codec: "subrip", Embedded: true}}}, installations: map[domain.Language]store.Installation{}}
	probePayload, err := os.ReadFile(filepath.Join("testdata", "ffprobe_streams.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{stdout: probePayload}
	service := Service{Repository: repository, Probe: Probe{Path: "ffprobe", Runner: runner}}
	media := domain.Media{Ref: domain.MediaRef{FileID: 42}, Fingerprint: fingerprint}

	first, err := service.Refresh(context.Background(), 1, media, false)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 || len(first.Tracks) != 1 {
		t.Fatalf("first refresh probe/tracks = %d/%#v", runner.calls, first.Tracks)
	}
	writeTestFile(t, filepath.Join(dir, "Show.hr.srt"), "subtitle")
	second, err := service.Refresh(context.Background(), 1, media, false)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 || len(second.Tracks) != 2 || repository.replacements != 2 {
		t.Fatalf("second refresh probe/tracks/replacements = %d/%#v/%d", runner.calls, second.Tracks, repository.replacements)
	}
}

func TestRefreshProbesWhenFingerprintChangesOrForced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Show.mkv")
	writeTestFile(t, path, "video")
	payload, _ := os.ReadFile(filepath.Join("testdata", "ffprobe_streams.json"))
	runner := &fakeRunner{stdout: payload}
	repository := &fakeRepository{record: store.InventoryRecord{CatalogFingerprint: domain.MediaFingerprint{Path: path, FileID: 42, Size: 1, ModTime: time.Unix(0, 1)}}, installations: map[domain.Language]store.Installation{}}
	service := Service{Repository: repository, Probe: Probe{Path: "ffprobe", Runner: runner}}
	media := domain.Media{Ref: domain.MediaRef{FileID: 42}, Fingerprint: domain.MediaFingerprint{Path: path, FileID: 42}}
	if _, err := service.Refresh(context.Background(), 1, media, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), 1, media, true); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatalf("probe calls = %d, want 2", runner.calls)
	}
}

func TestInventorySatisfiesRequiresFullKnownLanguageAndHIAllowance(t *testing.T) {
	inventory := Inventory{Tracks: []Track{{Language: "hr", Forced: true}, {Language: "en", Codec: "hdmv_pgs_subtitle"}, {Language: "hr", SDH: true}}}
	if !inventory.Satisfies("en", false) {
		t.Fatal("full-language PGS track should satisfy English")
	}
	if inventory.Satisfies("hr", false) {
		t.Fatal("forced and disallowed-SDH tracks should not satisfy Croatian")
	}
	if !inventory.Satisfies("hr", true) {
		t.Fatal("SDH track should satisfy Croatian when allowed")
	}
	if inventory.Satisfies("fr", true) {
		t.Fatal("unknown language must not satisfy French")
	}
}
