package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// newCoverageApp configures en and hr for the tv instance and a second,
// empty, movies instance.
func newCoverageApp(t *testing.T) *App {
	t.Helper()
	cfg := testConfig(t)
	cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
	cfg.Instances = append(cfg.Instances, config.InstanceConfig{Name: "movies", Type: "radarr", URL: "http://radarr.invalid", APIKey: "api"})
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}, "movies": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application
}

func TestCoverageAddsUpAndMatchesMissing(t *testing.T) {
	application := newCoverageApp(t)
	ctx := t.Context()
	root := application.Config.MediaRoots[0]
	episode := func(fileID int64) (int64, domain.MediaFingerprint) {
		path := filepath.Join(root, fmt.Sprintf("e%d.mkv", fileID))
		return storeEpisode(t, application, fileID, "Show", 2020, path), domain.MediaFingerprint{Path: path, FileID: fileID, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()}
	}
	// en: 1 installed, 2 covered by an embedded track, 3 satisfied by its
	// last search, 4 rejected, 5 never searched. hr: only 1 is installed.
	installed, installedPrint := episode(1)
	for _, language := range []string{"en", "hr"} {
		if err := application.Repository.RecordInstallation(ctx, store.Installation{MediaID: installed, Language: language, Path: filepath.Join(root, "e1."+language+".srt"), Checksum: "c", ProviderID: "english", CandidateID: "x", ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: installedPrint.Path, MediaFileID: 1, MediaSize: 100, MediaModTimeNS: installedPrint.ModTime.UnixNano()}); err != nil {
			t.Fatal(err)
		}
	}
	tracked, trackedPrint := episode(2)
	if err := application.Repository.ReplaceTrackInventory(ctx, tracked, trackedPrint, trackedPrint, []store.TrackRecord{{MediaID: tracked, Language: "en", Embedded: true}}); err != nil {
		t.Fatal(err)
	}
	satisfied, _ := episode(3)
	if err := application.Repository.SettlePendingSearch(ctx, satisfied, "en", "satisfied", application.Clock.Now(), ""); err != nil {
		t.Fatal(err)
	}
	rejected, _ := episode(4)
	if err := application.Repository.SettlePendingSearch(ctx, rejected, "en", "rejected", application.Clock.Now(), ""); err != nil {
		t.Fatal(err)
	}
	episode(5)
	// An instance removed from the configuration but not forgotten still
	// counts for missing, so coverage shows it too.
	stale := domain.Media{EntityID: 9, Ref: domain.MediaRef{Instance: "old", Kind: domain.MediaMovie, FileID: 9},
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(root, "old.mkv"), FileID: 9, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()},
		Title:       "Old", Year: 2020}
	if _, _, err := application.Repository.UpsertMedia(ctx, stale); err != nil {
		t.Fatal(err)
	}

	result, err := application.Coverage(ctx, control.CoverageArgs{})
	if err != nil {
		t.Fatal(err)
	}
	reasons := func(pairs ...any) []control.ReasonCount {
		counts := []control.ReasonCount{}
		for i := 0; i < len(pairs); i += 2 {
			counts = append(counts, control.ReasonCount{Reason: pairs[i].(string), Count: pairs[i+1].(int)})
		}
		return counts
	}
	want := []control.CoverageRow{
		{Instance: "tv", Configured: true, Language: "en", Media: 5, Covered: 3, Installed: 1, Existing: 2, Missing: 2, MissingByReason: reasons("all_rejected", 1, "not_searched", 1)},
		{Instance: "tv", Configured: true, Language: "hr", Media: 5, Covered: 1, Installed: 1, Missing: 4, MissingByReason: reasons("not_searched", 4)},
		{Instance: "movies", Configured: true, Language: "en", MissingByReason: reasons()},
		{Instance: "movies", Configured: true, Language: "hr", MissingByReason: reasons()},
		{Instance: "old", Language: "en", Media: 1, Missing: 1, MissingByReason: reasons("not_searched", 1)},
		{Instance: "old", Language: "hr", Media: 1, Missing: 1, MissingByReason: reasons("not_searched", 1)},
	}
	if got, expected := fmt.Sprint(result.Rows), fmt.Sprint(want); got != expected {
		t.Fatalf("rows = %s\nwant   %s", got, expected)
	}
	// Coverage's missing is missing's total, and the summary splits it the
	// same way.
	sum := 0
	for _, row := range result.Rows {
		sum += row.Missing
		if !row.Configured {
			continue
		}
		listed, err := application.Missing(ctx, control.MissingArgs{Instance: row.Instance, Language: row.Language})
		if err != nil || listed.Total != row.Missing {
			t.Fatalf("%s %s: missing = %+v, %v; coverage says %d", row.Instance, row.Language, listed, err, row.Missing)
		}
	}
	summary, err := application.Missing(ctx, control.MissingArgs{Summary: true})
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := []control.MissingGroup{
		{Instance: "old", Language: "en", Reason: "not_searched", Count: 1},
		{Instance: "old", Language: "hr", Reason: "not_searched", Count: 1},
		{Instance: "tv", Language: "en", Reason: "all_rejected", Count: 1},
		{Instance: "tv", Language: "en", Reason: "not_searched", Count: 1},
		{Instance: "tv", Language: "hr", Reason: "not_searched", Count: 4},
	}
	if summary.Total != 8 || summary.Total != sum || len(summary.Items) != 0 || fmt.Sprint(summary.Summary) != fmt.Sprint(wantSummary) {
		t.Fatalf("summary = %+v, coverage missing = %d", summary, sum)
	}
	if text := summary.Text(); text != "8 missing\nold en not_searched: 1\nold hr not_searched: 1\ntv en all_rejected: 1\ntv en not_searched: 1\ntv hr not_searched: 4" {
		t.Fatalf("summary text = %q", text)
	}

	// Filters narrow the rows.
	filtered, err := application.Coverage(ctx, control.CoverageArgs{Instance: "tv", Language: "hr"})
	if err != nil || len(filtered.Rows) != 1 || fmt.Sprint(filtered.Rows[0]) != fmt.Sprint(want[1]) {
		t.Fatalf("filtered = %+v, %v", filtered, err)
	}
	if text := filtered.Text(); text != "tv hr: 5 files, 1 covered (1 installed, 0 existing), 4 missing\n  missing: not_searched 4" {
		t.Fatalf("text = %q", text)
	}
	if text := result.Text(); !strings.Contains(text, "old en: 1 files, 0 covered (0 installed, 0 existing), 1 missing (not configured; forget-instance removes it)\n") {
		t.Fatalf("text = %q", text)
	}
}

func TestMissingSummaryIsAnEmptyListWhenNothingIsMissing(t *testing.T) {
	application := newCoverageApp(t)
	summary, err := application.Missing(t.Context(), control.MissingArgs{Summary: true})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"total":0,"items":[],"summary":[]}` {
		t.Fatalf("json = %s", payload)
	}
	// Without --summary the field stays out.
	listed, err := application.Missing(t.Context(), control.MissingArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if payload, _ := json.Marshal(listed); string(payload) != `{"total":0,"items":[]}` {
		t.Fatalf("json = %s", payload)
	}
}

func TestCoverageErrors(t *testing.T) {
	application := newCoverageApp(t)
	for args, code := range map[control.CoverageArgs]string{
		{Instance: "nope"}: control.CodeUnknownInstance,
		{Language: "fr"}:   control.CodeUnknownLanguage,
		{Language: "!!"}:   control.CodeUnknownLanguage,
	} {
		_, err := application.Coverage(t.Context(), args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != code {
			t.Fatalf("%+v: err = %v, want %s", args, err, code)
		}
	}
	// --summary counts every row, so a limit makes no sense with it.
	_, err := application.Missing(t.Context(), control.MissingArgs{Summary: true, Limit: 5})
	var coded *control.Error
	if !errors.As(err, &coded) || coded.Code != control.CodeUsage {
		t.Fatalf("summary with limit: err = %v", err)
	}
}

func TestCoverageOpensReadOnly(t *testing.T) {
	if !readOnlyCommand("coverage") {
		t.Fatal("coverage must open read-only")
	}
}
