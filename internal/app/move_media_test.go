package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

// moveFixture is a movie with an English sidecar under radarr, moved on disk
// from <root>/movies to <root>/movies-1080p, which radarr-1080p reports as
// /movies-1080p.
type moveFixture struct {
	cfg      config.Config
	app      *App
	logs     *bytes.Buffer
	oldMedia string
	newMedia string
	plan     string
}

const moveSidecarContent = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"

func newMoveFixture(t *testing.T) *moveFixture {
	t.Helper()
	cfg := testConfig(t)
	root := cfg.MediaRoots[0]
	cfg.Instances = []config.InstanceConfig{
		{Name: "radarr", Type: "radarr", URL: "http://radarr.invalid", APIKey: "api"},
		{Name: "radarr-1080p", Type: "radarr", URL: "http://radarr-1080p.invalid", APIKey: "api", PathMappings: []config.PathMapping{{Remote: "/movies-1080p", Local: filepath.Join(root, "movies-1080p")}}},
	}
	f := &moveFixture{cfg: cfg, logs: &bytes.Buffer{}}
	events, err := observability.New(f.logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.app, err = New(t.Context(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"radarr": fakeCatalog{}, "radarr-1080p": fakeCatalog{}}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.Close() })

	oldFolder, newFolder := filepath.Join(root, "movies", "Dune (2021)"), filepath.Join(root, "movies-1080p", "Dune (2021)")
	for _, folder := range []string{oldFolder, newFolder} {
		if err := os.MkdirAll(folder, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	f.oldMedia, f.newMedia = filepath.Join(oldFolder, "Dune.mkv"), filepath.Join(newFolder, "Dune.mkv")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	media := domain.Media{EntityID: 3, Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: f.oldMedia, FileID: 7, Size: 5, ModTime: now}, Title: "Dune", Year: 2021, ExternalIDs: domain.ExternalIDs{TMDB: 438631}}
	mediaID, _, err := f.app.Repository.UpsertMedia(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(moveSidecarContent))
	if err := f.app.Repository.RecordInstallation(t.Context(), store.Installation{MediaID: mediaID, Language: "en", Path: filepath.Join(oldFolder, "Dune.en.srt"), Checksum: hex.EncodeToString(sum[:]), MediaPath: f.oldMedia, MediaFileID: 7, MediaSize: 5, MediaModTimeNS: now.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	// The files as moved by rename.
	writeMoveFile(t, f.newMedia, "media")
	writeMoveFile(t, filepath.Join(newFolder, "Dune.en.srt"), moveSidecarContent)
	f.plan = filepath.Join(t.TempDir(), "plan.json")
	writeMoveFile(t, f.plan, `{"entries":[{"kind":"movie","tmdb_id":438631,"to_entity_id":30,"to_file_id":70,"to_path":"/movies-1080p/Dune (2021)/Dune.mkv"}]}`)
	return f
}

func writeMoveFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMoveMediaChecksFilesThenMoves(t *testing.T) {
	f := newMoveFixture(t)
	preview, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, false)
	if err != nil || !preview.Ready || preview.Applied || preview.Media != 1 || preview.Installations != 1 {
		t.Fatalf("preview = %#v/%v", preview, err)
	}
	if !strings.Contains(preview.Text(), "rerun with --yes") {
		t.Fatalf("preview text = %q", preview.Text())
	}
	result, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, true)
	if err != nil || !result.Applied {
		t.Fatalf("move = %#v/%v", result, err)
	}
	if _, media, err := f.app.Repository.FindMedia(t.Context(), domain.MediaRef{Instance: "radarr-1080p", Kind: domain.MediaMovie, FileID: 70}); err != nil || media.Fingerprint.Path != f.newMedia {
		t.Fatalf("moved media = %#v/%v", media, err)
	}
	if !strings.Contains(f.logs.String(), `"event":"media.moved"`) || !strings.Contains(f.logs.String(), `"installation_count":1`) {
		t.Fatalf("logs = %s", f.logs.String())
	}
	if strings.Contains(f.logs.String(), f.cfg.MediaRoots[0]) {
		t.Fatalf("logs carry a media path: %s", f.logs.String())
	}
}

func TestMoveMediaRefusesWhenFilesAreNotInPlace(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, f *moveFixture)
		want    string
	}{
		{"media not moved", func(t *testing.T, f *moveFixture) { _ = os.Remove(f.newMedia) }, "the media file is not at to_path"},
		{"old media still there", func(t *testing.T, f *moveFixture) { writeMoveFile(t, f.oldMedia, "media") }, "still at its old path"},
		{"size differs", func(t *testing.T, f *moveFixture) { writeMoveFile(t, f.newMedia, "other media") }, "has 11 bytes, the recorded file 5"},
		{"sidecar missing", func(t *testing.T, f *moveFixture) {
			_ = os.Remove(filepath.Join(filepath.Dir(f.newMedia), "Dune.en.srt"))
		}, "the en sidecar Dune.en.srt: is not at the new path"},
		{"sidecar edited", func(t *testing.T, f *moveFixture) {
			writeMoveFile(t, filepath.Join(filepath.Dir(f.newMedia), "Dune.en.srt"), "edited")
		}, "has changed since subsyncd installed it"},
		{"outside media roots", func(t *testing.T, f *moveFixture) {
			writeMoveFile(t, f.plan, `{"entries":[{"kind":"movie","tmdb_id":438631,"to_entity_id":30,"to_file_id":70,"to_path":"/elsewhere/Dune.mkv"}]}`)
		}, "outside the configured media roots"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMoveFixture(t)
			test.prepare(t, f)
			preview, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, false)
			if err != nil || preview.Ready || preview.Problems == 0 || !strings.Contains(strings.Join(preview.Entries[0].Problems, "; "), test.want) {
				t.Fatalf("preview = %#v/%v, want problem %q", preview, err, test.want)
			}
			if !strings.Contains(preview.Text(), "not ready") {
				t.Fatalf("preview text = %q", preview.Text())
			}
			_, err = f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, true)
			var coded *control.Error
			if !errors.As(err, &coded) || coded.Code != control.CodeMoveRefused || !strings.Contains(coded.Message, test.want) {
				t.Fatalf("apply err = %#v, want move_refused", err)
			}
			if _, _, err := f.app.Repository.FindMedia(t.Context(), domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: 7}); err != nil {
				t.Fatalf("source row gone after a refused move: %v", err)
			}
		})
	}
}

func TestMoveMediaRejectsBadArguments(t *testing.T) {
	f := newMoveFixture(t)
	for _, test := range []struct {
		name, from, to, plan, code, want string
	}{
		{"same instance", "radarr", "radarr", f.plan, control.CodeUsage, "different instances"},
		{"unconfigured target", "radarr", "radarr-4k", f.plan, control.CodeUnknownInstance, "--to must be configured"},
		{"unknown source", "radarr-typo", "radarr-1080p", f.plan, control.CodeUnknownInstance, "no records"},
		{"missing plan", "radarr", "radarr-1080p", filepath.Join(t.TempDir(), "none.json"), control.CodeUsage, "open move plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.app.MoveMedia(t.Context(), test.from, test.to, test.plan, false)
			var coded *control.Error
			if !errors.As(err, &coded) || coded.Code != test.code || !strings.Contains(coded.Message, test.want) {
				t.Fatalf("err = %#v, want %s %q", err, test.code, test.want)
			}
		})
	}
	for _, plan := range []struct{ name, body, want string }{
		{"unknown field", `{"entries":[{"kind":"movie","tmdb":1}]}`, "unknown field"},
		{"no entries", `{"entries":[]}`, "no entries"},
		{"wrong kind", `{"entries":[{"kind":"episode","tvdb_id":1,"season":1,"episode":1,"to_series_id":1,"to_entity_id":1,"to_file_id":1,"to_path":"/x"}]}`, "does not match the radarr instance"},
		{"trailing data", `{"entries":[{"kind":"movie"}]} {}`, "unexpected data"},
	} {
		t.Run(plan.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan.json")
			writeMoveFile(t, path, plan.body)
			_, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", path, false)
			var coded *control.Error
			if !errors.As(err, &coded) || coded.Code != control.CodeUsage || !strings.Contains(coded.Message, plan.want) {
				t.Fatalf("err = %#v, want usage %q", err, plan.want)
			}
		})
	}
}

func TestMoveMediaPreviewRunsWhileDaemonHoldsLock(t *testing.T) {
	if !readOnlyCommand("move-media-preview") || readOnlyCommand("move-media") {
		t.Fatal("preview must open read-only and move-media must not")
	}
	f := newMoveFixture(t)
	diagnostic, err := New(t.Context(), f.cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"radarr": fakeCatalog{}, "radarr-1080p": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if preview, err := diagnostic.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, false); err != nil || !preview.Ready {
		t.Fatalf("preview = %#v/%v", preview, err)
	}
	if _, err := diagnostic.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, true); err == nil {
		t.Fatal("read-only application moved records")
	}
}

func TestMoveMediaExpectsSidecarsUnderARenamedMediaName(t *testing.T) {
	f := newMoveFixture(t)
	folder := filepath.Dir(f.newMedia)
	renamed := filepath.Join(folder, "Dune (2021) Bluray-1080p.mkv")
	if err := os.Rename(f.newMedia, renamed); err != nil {
		t.Fatal(err)
	}
	writeMoveFile(t, f.plan, `{"entries":[{"kind":"movie","tmdb_id":438631,"to_entity_id":30,"to_file_id":70,"to_path":"/movies-1080p/Dune (2021)/Dune (2021) Bluray-1080p.mkv"}]}`)
	// The sidecar kept its old name: inventory would no longer see it.
	preview, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, false)
	if err != nil || preview.Ready || !strings.Contains(strings.Join(preview.Entries[0].Problems, "; "), "Dune (2021) Bluray-1080p.en.srt: is not at the new path") {
		t.Fatalf("preview = %#v/%v, want the renamed sidecar missing", preview, err)
	}
	if err := os.Rename(filepath.Join(folder, "Dune.en.srt"), filepath.Join(folder, "Dune (2021) Bluray-1080p.en.srt")); err != nil {
		t.Fatal(err)
	}
	if result, err := f.app.MoveMedia(t.Context(), "radarr", "radarr-1080p", f.plan, true); err != nil || !result.Applied {
		t.Fatalf("move = %#v/%v", result, err)
	}
}
