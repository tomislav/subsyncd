package app

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func recordRecent(t *testing.T, application *App, id int64, language, outcome string, detail store.SearchHistoryDetail, ago time.Duration) {
	t.Helper()
	if err := application.Repository.RecordSearchHistory(t.Context(), id, domain.Language(language), outcome, detail, application.Clock.Now().Add(-ago)); err != nil {
		t.Fatal(err)
	}
}

func TestRecentDefaultsTo24Hours(t *testing.T) {
	application, root := newMissingApp(t)
	id := storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "a.mkv"))
	recordRecent(t, application, id, "en", "installed", store.SearchHistoryDetail{MediaTitle: "Show S01E01", ProviderID: "english", ReleaseName: "Show.S01E01.WEB"}, time.Hour)
	recordRecent(t, application, id, "en", "no_result", store.SearchHistoryDetail{MediaTitle: "Show S01E01"}, 25*time.Hour)
	result, err := application.Recent(t.Context(), control.RecentArgs{})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	item := result.Items[0]
	if item.Outcome != "installed" || item.Provider != "english" || item.Release != "Show.S01E01.WEB" || item.Language != "en" || item.Instance != "tv" || item.Kind != "episode" || item.FileID != 7 || item.Title != "Show S01E01" || item.Manual || item.FinishedAt.IsZero() {
		t.Fatalf("item = %+v", item)
	}
	if drift := time.Since(result.Since.Add(24 * time.Hour)); drift < 0 || drift > time.Minute {
		t.Fatalf("since = %v, want about 24 hours ago", result.Since)
	}
	wide, err := application.Recent(t.Context(), control.RecentArgs{Since: 48 * time.Hour})
	if err != nil || len(wide.Items) != 2 {
		t.Fatalf("wide = %+v, %v", wide, err)
	}
}

func TestRecentFilters(t *testing.T) {
	application, root := newMissingApp(t)
	first := storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "a.mkv"))
	second := storeEpisode(t, application, 8, "Other", 2020, filepath.Join(root, "b.mkv"))
	recordRecent(t, application, first, "en", "installed", store.SearchHistoryDetail{MediaTitle: "Show"}, 3*time.Hour)
	recordRecent(t, application, second, "en", "no_result", store.SearchHistoryDetail{MediaTitle: "Other"}, 2*time.Hour)
	recordRecent(t, application, second, "hr", "no_result", store.SearchHistoryDetail{MediaTitle: "Other"}, time.Hour)
	if result, err := application.Recent(t.Context(), control.RecentArgs{Limit: 1}); err != nil || len(result.Items) != 1 || result.Items[0].Language != "hr" {
		t.Fatalf("limit = %+v, %v", result, err)
	}
	if result, err := application.Recent(t.Context(), control.RecentArgs{Instance: "tv", Language: "en"}); err != nil || len(result.Items) != 2 {
		t.Fatalf("filter = %+v, %v", result, err)
	}
	for _, tc := range []struct {
		args control.RecentArgs
		code string
	}{
		{control.RecentArgs{Instance: "nope"}, control.CodeUnknownInstance},
		{control.RecentArgs{Language: "xx-bogus-"}, control.CodeUnknownLanguage},
		{control.RecentArgs{Language: "fr"}, control.CodeUnknownLanguage},
	} {
		_, err := application.Recent(t.Context(), tc.args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != tc.code {
			t.Errorf("%+v: err = %v, want %s", tc.args, err, tc.code)
		}
	}
}

func TestRecentInvalidSinceIsUsage(t *testing.T) {
	application, _ := newMissingApp(t)
	for _, args := range []control.RecentArgs{{Since: -time.Hour}, {Limit: -1}} {
		_, err := application.Recent(t.Context(), args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeUsage {
			t.Errorf("%+v: err = %v", args, err)
		}
	}
}

// Last night: 26 installs, then about 950 throttled searches that filled
// --limit and hid every install. --outcome installed filters before the
// limit, so the installs come back.
func TestRecentOutcomeFilterAppliesBeforeTheLimit(t *testing.T) {
	application, root := newMissingApp(t)
	id := storeEpisode(t, application, 7, "Show", 2020, filepath.Join(root, "a.mkv"))
	recordRecent(t, application, id, "hr", "installed", store.SearchHistoryDetail{MediaTitle: "Show S01E01", ProviderID: "opensubtitles"}, 10*time.Hour)
	recordRecent(t, application, id, "hr", "installed", store.SearchHistoryDetail{MediaTitle: "Show S01E01", ProviderID: "titlovi"}, 9*time.Hour)
	for minutes := 1; minutes <= 5; minutes++ {
		recordRecent(t, application, id, "hr", "throttled", store.SearchHistoryDetail{MediaTitle: "Show S01E01"}, time.Duration(minutes)*time.Minute)
	}
	if result, err := application.Recent(t.Context(), control.RecentArgs{Limit: 3}); err != nil || len(result.Items) != 3 || result.Items[0].Outcome != "throttled" {
		t.Fatalf("unfiltered = %+v, %v", result, err)
	}
	result, err := application.Recent(t.Context(), control.RecentArgs{Limit: 3, Outcomes: []string{"installed"}})
	if err != nil || len(result.Items) != 2 || result.Items[0].Provider != "titlovi" || result.Items[1].Provider != "opensubtitles" {
		t.Fatalf("installed = %+v, %v", result, err)
	}
	both, err := application.Recent(t.Context(), control.RecentArgs{Outcomes: []string{"installed", "throttled"}})
	if err != nil || len(both.Items) != 7 {
		t.Fatalf("installed,throttled = %+v, %v", both, err)
	}
}

func TestRecentUnknownOutcomeIsUsage(t *testing.T) {
	application, _ := newMissingApp(t)
	for _, outcomes := range [][]string{{"instaled"}, {"installed", ""}} {
		_, err := application.Recent(t.Context(), control.RecentArgs{Outcomes: outcomes})
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeUsage {
			t.Errorf("%q: err = %v", outcomes, err)
		}
	}
}
