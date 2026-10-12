package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func newMissingApp(t *testing.T) (*App, string) {
	t.Helper()
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application, cfg.MediaRoots[0]
}

func settle(t *testing.T, application *App, mediaID int64, outcome string) {
	t.Helper()
	// Settling needs a pending search, so queue it again first.
	if err := application.Repository.UpsertSearchStateWithPriority(t.Context(), mediaID, "en", application.Clock.Now(), store.SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.SettlePendingSearch(t.Context(), mediaID, "en", outcome, application.Clock.Now(), ""); err != nil {
		t.Fatal(err)
	}
}

func onlyMissing(t *testing.T, application *App, args control.MissingArgs) control.MissingItem {
	t.Helper()
	result, err := application.Missing(t.Context(), args)
	if err != nil || result.Total != 1 || len(result.Items) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	return result.Items[0]
}

func TestMissingReasonsAndActions(t *testing.T) {
	application, root := newMissingApp(t)
	file := filepath.Join(root, "a.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := storeEpisode(t, application, 7, "Show", 2020, file)
	base := "subsyncd search --instance tv --kind episode --file-id 7 --language en"
	for _, tc := range []struct {
		name, outcome, reason, action string
	}{
		{"never searched", "", "not_searched", base},
		{"no candidates", "no_result", "no_candidates", base},
		{"all rejected", "rejected", "all_rejected", base + " --retry-rejected"},
		{"technical failure", "workflow_error", "not_searched", base},
	} {
		if tc.outcome != "" {
			settle(t, application, id, tc.outcome)
		}
		item := onlyMissing(t, application, control.MissingArgs{})
		if item.Reason != tc.reason || item.Action != tc.action || item.Language != "en" || item.FileID != 7 || item.Title != "Show" {
			t.Fatalf("%s: item = %+v", tc.name, item)
		}
	}
}

func TestMissingProvidersCoolingDown(t *testing.T) {
	application, root := newMissingApp(t)
	id := storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "a.mkv"))
	settle(t, application, id, "throttled")
	reset := application.Clock.Now().Add(time.Hour)
	if err := application.Repository.PutProviderState(t.Context(), store.ProviderState{ProviderID: "english", Scope: "search", Reason: "rate_limit", ResetAt: reset}); err != nil {
		t.Fatal(err)
	}
	item := onlyMissing(t, application, control.MissingArgs{})
	if item.Reason != "providers_cooling_down" || len(item.Providers) != 1 || item.Providers[0] != "english" || item.Action != "subsyncd retry --provider english" {
		t.Fatalf("item = %+v", item)
	}
	// A cooled-down route is reported even when the last search found nothing.
	settle(t, application, id, "no_result")
	if item := onlyMissing(t, application, control.MissingArgs{}); item.Reason != "providers_cooling_down" {
		t.Fatalf("item = %+v", item)
	}
	// An expired cooldown no longer counts.
	if err := application.Repository.PutProviderState(t.Context(), store.ProviderState{ProviderID: "english", Scope: "search", Reason: "rate_limit", ResetAt: application.Clock.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if item := onlyMissing(t, application, control.MissingArgs{}); item.Reason != "no_candidates" || len(item.Providers) != 0 {
		t.Fatalf("item = %+v", item)
	}
}

func TestMissingMediaUnavailable(t *testing.T) {
	application, root := newMissingApp(t)
	id := storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "gone.mkv"))
	settle(t, application, id, "workflow_error")
	item := onlyMissing(t, application, control.MissingArgs{})
	if item.Reason != "media_unavailable" || item.Action == "" || strings.Contains(item.Action, root) {
		t.Fatalf("item = %+v", item)
	}
}

func TestMissingUnsupported(t *testing.T) {
	application, root := newMissingApp(t)
	media := domain.Media{EntityID: 9, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: 9},
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(root, "multi.mkv"), FileID: 9, Size: 1, ModTime: time.Unix(1, 0).UTC()},
		Title:       "Multi", Season: 1, Episode: 1, UnsupportedReason: domain.UnsupportedMultiEpisode}
	if _, _, err := application.Repository.UpsertMedia(t.Context(), media); err != nil {
		t.Fatal(err)
	}
	item := onlyMissing(t, application, control.MissingArgs{})
	if item.Reason != "unsupported" || item.Action == "" || strings.HasPrefix(item.Action, "subsyncd search") {
		t.Fatalf("item = %+v", item)
	}
}

func TestMissingFiltersAndLimit(t *testing.T) {
	application, root := newMissingApp(t)
	for i := int64(1); i <= 3; i++ {
		storeEpisode(t, application, i, "Show", 2020, filepath.Join(root, string(rune('a'+i))+".mkv"))
	}
	result, err := application.Missing(t.Context(), control.MissingArgs{Limit: 2})
	if err != nil || result.Total != 3 || len(result.Items) != 2 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if result, err = application.Missing(t.Context(), control.MissingArgs{Title: "show s01e01", Language: "en", Instance: "tv"}); err != nil || result.Total != 3 {
		t.Fatalf("filtered = %+v, %v", result, err)
	}
	for args, code := range map[control.MissingArgs]string{
		{Instance: "nope"}: control.CodeUnknownInstance,
		{Language: "fr"}:   control.CodeUnknownLanguage,
		{Limit: -1}:        control.CodeUsage,
		{Title: "  "}:      control.CodeUsage,
	} {
		_, err := application.Missing(t.Context(), args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != code {
			t.Fatalf("%+v: err = %v, want %s", args, err, code)
		}
	}
}

func TestMissingOpensReadOnly(t *testing.T) {
	if !readOnlyCommand("missing") {
		t.Fatal("missing must open read-only")
	}
	application, root := newMissingApp(t)
	storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "a.mkv"))
	cfg := application.Config
	// The daemon-style application above holds the process lock.
	diagnostic, err := New(t.Context(), cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	result, err := diagnostic.Missing(t.Context(), control.MissingArgs{})
	if err != nil || result.Total != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
}
