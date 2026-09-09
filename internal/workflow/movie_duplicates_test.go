package workflow

import (
	"bytes"
	"context"
	"strings"
	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"testing"
)

func TestMovieArchiveDuplicateSelectionAndDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     map[string]string
		installed bool
		rule      string
	}{
		{"identical", map[string]string{"Movie.srt": installSRT, "Movie_UTF8.srt": installSRT}, true, "single_movie"},
		{"distinct", map[string]string{"Movie.srt": installSRT, "Movie_other.srt": strings.ReplaceAll(installSRT, "00:00:01", "00:00:02")}, false, "single_movie"},
		{"forced excluded", map[string]string{"Movie.srt": installSRT, "Movie.forced.srt": installSRT}, true, "single_movie"},
		{"forced only", map[string]string{"Movie.forced.srt": installSRT, "Movie_other.forced.srt": installSRT}, false, "forced_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := serviceRequest(t)
			candidate := exactCandidate("duplicate")
			candidate.Kind = domain.MediaMovie
			adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"duplicate": workflowZIP(t, tc.files)}, filenames: map[string]string{"duplicate": "movie.zip"}}
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
			service.Providers = map[string]provider.Provider{"provider": adapter}
			service.Events = events
			result, err := service.Run(context.Background(), request)
			if err != nil || (result.Outcome == OutcomeInstalled) != tc.installed {
				t.Fatalf("outcome=%s err=%v decisions=%+v", result.Outcome, err, result.Decisions)
			}
			event := "candidate.rejected"
			if tc.installed {
				event = "archive.members_selected"
			}
			records := workflowEvents(workflowLogRecords(t, logs.String()), event)
			if len(records) != 1 || records[0]["selection_rule"] != tc.rule || records[0]["subtitle_member_count"] != float64(2) || records[0]["archive_type"] != "zip" {
				t.Fatalf("diagnostics=%v", records)
			}
			if strings.Contains(logs.String(), "Movie_UTF8.srt") || strings.Contains(logs.String(), request.Media.Fingerprint.Path) {
				t.Fatal("private archive detail logged")
			}
		})
	}
}
