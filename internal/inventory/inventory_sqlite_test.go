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

type callbackRunner struct {
	calls    int
	callback func()
	payload  string
}

func (r *callbackRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	r.calls++
	if r.callback != nil {
		r.callback()
	}
	return []byte(r.payload), nil, nil
}

func sqliteInventory(t *testing.T) (*store.Repository, int64, domain.Media) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Example.mkv")
	writeTestFile(t, path, "video")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := db.Repository()
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "test", Kind: domain.MediaMovie, FileID: 42}, Fingerprint: domain.MediaFingerprint{Path: path, FileID: 42, Size: info.Size(), ModTime: info.ModTime()}}
	id, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	return repo, id, media
}

func TestSQLiteFreshInventoryRequiresCompletedProbeIncludingEmptyResult(t *testing.T) {
	repo, id, media := sqliteInventory(t)
	runner := &callbackRunner{payload: `{"streams":[]}`}
	service := Service{Repository: repo, Probe: Probe{Runner: runner}}
	for i := 0; i < 2; i++ {
		if _, err := service.Refresh(context.Background(), id, media, false); err != nil {
			t.Fatal(err)
		}
	}
	if runner.calls != 1 {
		t.Fatalf("fresh then cached empty inventory: probe calls=%d, want 1", runner.calls)
	}
}

func TestSQLiteReplacementCannotReuseOldEmbeddedTracks(t *testing.T) {
	repo, id, media := sqliteInventory(t)
	runner := &callbackRunner{payload: `{"streams":[{"index":1,"codec_type":"subtitle","tags":{"language":"en"}}]}`}
	service := Service{Repository: repo, Probe: Probe{Runner: runner}}
	if _, err := service.Refresh(context.Background(), id, media, true); err != nil {
		t.Fatal(err)
	}
	replacement := media
	replacement.Ref.FileID = 43
	replacement.Fingerprint.FileID = 43
	if _, _, err := repo.UpsertMedia(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	runner.payload = `{"streams":[]}`
	got, err := service.Refresh(context.Background(), id, replacement, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 0 || runner.calls != 2 {
		t.Fatalf("replacement inherited tracks: %#v; probes %d", got.Tracks, runner.calls)
	}
}

func TestSQLiteRefreshRejectsCatalogReplacementBeforeRead(t *testing.T) {
	repo, id, media := sqliteInventory(t)
	replacement := media
	replacement.Ref.FileID = 43
	replacement.Fingerprint.FileID = 43
	if _, _, err := repo.UpsertMedia(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	service := Service{Repository: repo, Probe: Probe{Runner: &callbackRunner{payload: `{"streams":[]}`}}}
	if _, err := service.Refresh(context.Background(), id, media, true); err == nil {
		t.Fatal("stale request overwrote catalog replacement")
	}
	got, err := repo.GetMedia(context.Background(), id)
	if err != nil || got.Ref.FileID != 43 {
		t.Fatalf("replacement lost: %#v %v", got, err)
	}
}

func TestSQLiteRefreshRejectsChangesDuringProbe(t *testing.T) {
	for _, change := range []string{"bytes", "replacement", "rename", "delete"} {
		t.Run(change, func(t *testing.T) {
			repo, id, media := sqliteInventory(t)
			runner := &callbackRunner{payload: `{"streams":[]}`, callback: func() {
				if change == "bytes" {
					writeTestFile(t, media.Fingerprint.Path, "changed video")
					return
				}
				if change == "delete" {
					if _, err := repo.ApplyMediaEvent(context.Background(), store.MediaEventMutation{EventID: "delete-during-probe", Type: "delete", Ref: media.Ref, At: time.Now()}); err != nil {
						t.Fatal(err)
					}
					return
				}
				replacement := media
				if change == "replacement" {
					replacement.Ref.FileID = 43
					replacement.Fingerprint.FileID = 43
				} else {
					replacement.Fingerprint.Path = filepath.Join(filepath.Dir(media.Fingerprint.Path), "Renamed.mkv")
				}
				if _, _, err := repo.UpsertMedia(context.Background(), replacement); err != nil {
					t.Fatal(err)
				}
			}}
			service := Service{Repository: repo, Probe: Probe{Runner: runner}}
			if _, err := service.Refresh(context.Background(), id, media, true); err == nil {
				t.Fatal("refresh accepted stale probe")
			}
			got, err := repo.GetMedia(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if change == "replacement" && got.Ref.FileID != 43 {
				t.Fatal("replacement reverted")
			}
			if change == "rename" && got.Fingerprint.Path == media.Fingerprint.Path {
				t.Fatal("rename reverted")
			}
		})
	}
}

func TestSQLiteRefreshRefinesCatalogDateAddedToLiveStat(t *testing.T) {
	repo, id, media := sqliteInventory(t)
	media.Fingerprint.ModTime = media.Fingerprint.ModTime.Add(-time.Hour)
	// A request can retain Arr DateAdded even when the row already has the live timestamp.
	service := Service{Repository: repo, Probe: Probe{Runner: &callbackRunner{payload: `{"streams":[]}`}}}
	got, err := service.Refresh(context.Background(), id, media, true)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(media.Fingerprint.Path)
	if !got.Fingerprint.ModTime.Equal(info.ModTime()) {
		t.Fatal("did not retain observed stat timestamp")
	}
}

func TestSQLiteCatalogChangeInvalidatesProbeEvenWhenLiveStatStillMatches(t *testing.T) {
	repo, id, media := sqliteInventory(t)
	runner := &callbackRunner{payload: `{"streams":[]}`}
	service := Service{Repository: repo, Probe: Probe{Runner: runner}}
	if _, err := service.Refresh(context.Background(), id, media, true); err != nil {
		t.Fatal(err)
	}
	changed := media
	changed.Fingerprint.Size++
	changed.Fingerprint.ModTime = changed.Fingerprint.ModTime.Add(-time.Hour)
	if _, _, err := repo.UpsertMedia(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), id, changed, false); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatal("catalog change reused old probe")
	}
}
