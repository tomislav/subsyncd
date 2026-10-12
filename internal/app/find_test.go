package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func newFindApp(t *testing.T, cfg config.Config) *App {
	t.Helper()
	cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application
}

// storeEpisode indexes an episode of a show with the given file ID and path
// and schedules every configured language, like an Arr import.
func storeEpisode(t *testing.T, application *App, fileID int64, show string, year int, path string) int64 {
	t.Helper()
	media := domain.Media{EntityID: fileID, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: fileID},
		Fingerprint: domain.MediaFingerprint{Path: path, FileID: fileID, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()},
		Title:       show, Year: year, Season: 1, Episode: 1}
	ctx := context.Background()
	id, _, err := application.Repository.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	for language := range application.Config.Languages {
		if err := application.Repository.UpsertSearchStateWithPriority(ctx, id, language, application.Clock.Now(), store.SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
	}
	if err := application.Repository.ReplaceTrackInventory(ctx, id, media.Fingerprint, media.Fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func titleSelection(title string) control.Selection { return control.Selection{Title: title} }

func TestResolveArrPath(t *testing.T) {
	cfg := testConfig(t)
	remote := "/tv"
	cfg.Instances[0].PathMappings = []config.PathMapping{{Remote: remote, Local: filepath.Join(cfg.MediaRoots[0], "tv")}}
	application := newFindApp(t, cfg)
	local := filepath.Join(cfg.MediaRoots[0], "tv", "Show", "S01E01.mkv")
	id := storeEpisode(t, application, 7, "Show", 0, local)
	for _, path := range []string{"/tv/Show/S01E01.mkv", local} {
		gotID, media, err := application.Resolve(context.Background(), control.Selection{Path: path})
		if err != nil || gotID != id || media.Ref.FileID != 7 {
			t.Fatalf("Resolve(%q) = %d, %+v, %v", path, gotID, media.Ref, err)
		}
	}
	// A path no instance maps and no media stores is not found.
	_, _, err := application.Resolve(context.Background(), control.Selection{Path: "/tv/Show/S01E02.mkv"})
	var coded *control.Error
	if !errors.As(err, &coded) || coded.Code != control.CodeMediaNotFound {
		t.Fatalf("err = %v", err)
	}
	// Narrowing to another instance finds nothing.
	_, _, err = application.Resolve(context.Background(), control.Selection{Instance: "other", Path: local})
	if !errors.As(err, &coded) || coded.Code != control.CodeUnknownInstance {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveAmbiguousTitleListsCandidates(t *testing.T) {
	cfg := testConfig(t)
	application := newFindApp(t, cfg)
	root := cfg.MediaRoots[0]
	storeEpisode(t, application, 1, "Shameless", 2004, filepath.Join(root, "a.mkv"))
	storeEpisode(t, application, 2, "Shameless", 2011, filepath.Join(root, "b.mkv"))
	_, _, err := application.Resolve(context.Background(), titleSelection("Shameless S01E01"))
	var coded *control.Error
	if !errors.As(err, &coded) || coded.Code != control.CodeAmbiguousMedia || len(coded.Candidates) != 2 {
		t.Fatalf("err = %#v", err)
	}
	if _, _, err := application.Resolve(context.Background(), titleSelection("Shameless (2011) S01E01")); err != nil {
		t.Fatalf("year disambiguation: %v", err)
	}
	_, _, err = application.Resolve(context.Background(), titleSelection("Nothing Like It"))
	if !errors.As(err, &coded) || coded.Code != control.CodeMediaNotFound {
		t.Fatalf("err = %#v", err)
	}
}

func TestResolveRejectsMixedOrEmptySelections(t *testing.T) {
	application := newFindApp(t, testConfig(t))
	for _, sel := range []control.Selection{
		{},
		{Title: "x", Path: "/y"},
		{Title: "x", FileID: 1, Instance: "tv", Kind: "movie"},
		{FileID: 1},
	} {
		_, _, err := application.Resolve(context.Background(), sel)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeUsage {
			t.Fatalf("Resolve(%+v) err = %#v", sel, err)
		}
	}
}

func TestFindReturnsMultipleMatchesAsSuccess(t *testing.T) {
	cfg := testConfig(t)
	application := newFindApp(t, cfg)
	root := cfg.MediaRoots[0]
	first := storeEpisode(t, application, 1, "Shameless", 2004, filepath.Join(root, "a.mkv"))
	storeEpisode(t, application, 2, "Shameless", 2011, filepath.Join(root, "b.mkv"))
	if err := application.Repository.RecordInstallation(context.Background(), store.Installation{MediaID: first, Language: "en", Path: filepath.Join(root, "a.en.srt"), Checksum: "c", ProviderID: "english", CandidateID: "x", ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: filepath.Join(root, "a.mkv"), MediaFileID: 1, MediaSize: 100, MediaModTimeNS: time.Unix(1700000000, 0).UnixNano()}); err != nil {
		t.Fatal(err)
	}
	result, err := application.Find(context.Background(), titleSelection("Shameless"))
	if err != nil || len(result.Matches) != 2 {
		t.Fatalf("Find = %+v, %v", result, err)
	}
	got := result.Matches[0]
	if got.FileID != 1 || got.Year != 2004 || len(got.Languages) != 2 || got.Languages[0] != (control.LanguageState{Language: "en", State: "installed"}) || got.Languages[1] != (control.LanguageState{Language: "hr", State: "missing"}) {
		t.Fatalf("first match = %+v", got)
	}
	if state := result.Matches[1].Languages[0].State; state != "missing" {
		t.Fatalf("second match en = %s", state)
	}
	// No match is an empty success, not an error.
	empty, err := application.Find(context.Background(), titleSelection("Nope"))
	if err != nil || len(empty.Matches) != 0 || empty.Matches == nil {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
	for _, text := range []string{result.Text()} {
		if strings.Contains(text, root) {
			t.Fatalf("find output leaks a path: %s", text)
		}
	}
}

func TestExplainWithoutLanguageCoversAllConfigured(t *testing.T) {
	cfg := testConfig(t)
	application := newFindApp(t, cfg)
	storeEpisode(t, application, 5, "Priority", 0, filepath.Join(cfg.MediaRoots[0], "p.mkv"))
	ctx := context.Background()
	all, err := application.Explain(ctx, control.ExplainArgs{Selection: titleSelection("priority")})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Languages) != 2 || all.Languages[0].Language != "en" || all.Languages[1].Language != "hr" || all.Title != "Priority" || all.FileID != 5 {
		t.Fatalf("explain = %+v", all)
	}
	for _, language := range all.Languages {
		if language.Search == nil || language.Title != "Priority" {
			t.Fatalf("language explain = %+v", language)
		}
	}
	// The single-language output is the same whether selected by title or file ID.
	byTitle, err := application.Explain(ctx, control.ExplainArgs{Selection: titleSelection("priority"), Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	byID, err := application.Explain(ctx, explainArgs("tv", "episode", 5, "en"))
	if err != nil || byTitle.Text() != byID.Text() || len(byTitle.Languages) != 0 {
		t.Fatalf("by title %q / by id %q / %v", byTitle.Text(), byID.Text(), err)
	}
	if !strings.Contains(all.Text(), "language: hr\nmedia: id=") {
		t.Fatalf("text = %s", all.Text())
	}
}

func TestSearchByTitleOnlyResolvesIndexedMedia(t *testing.T) {
	application := newFindApp(t, testConfig(t))
	_, err := application.Search(context.Background(), control.SearchArgs{Selection: titleSelection("Not Indexed"), Language: "en"})
	var coded *control.Error
	if !errors.As(err, &coded) || coded.Code != control.CodeMediaNotFound {
		t.Fatalf("err = %#v", err)
	}
}
