package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResultTextMatchesTheOriginalCommandOutput(t *testing.T) {
	retryAt := time.Date(2026, 10, 12, 8, 30, 0, 5, time.UTC)
	for _, test := range []struct {
		name   string
		result Result
		want   string
	}{
		{"scan", ScanResult{Instance: "tv", Media: 4, ForceProbe: true}, "scan complete: instance=tv media=4 force_probe=true"},
		{"scan deferred", ScanResult{Instance: "tv", HistoryDeferred: true, Deferred: 2, DeferredMedia: []string{"a", "b (c)"}}, "scan complete: instance=tv media=0 force_probe=false deferred=2 (a; b (c))"},
		{"search installed", SearchResult{MediaID: 7, Languages: []LanguageOutcome{{Language: "en", Outcome: "installed", Provider: "p", Candidate: "r1", Score: 90}}}, "media_id: 7\noutcome: installed\nprovider: p\ncandidate: r1\nscore: 90"},
		{"search throttled", SearchResult{MediaID: 7, Languages: []LanguageOutcome{{Language: "en", Outcome: "throttled", NextAttemptAt: retryAt}}}, "media_id: 7\noutcome: throttled\nretry_at: 2026-10-12T08:30:00.000000005Z"},
		{"retry", RetryResult{Provider: "titlovi", SearchesDue: 3}, "provider retry state reset: titlovi; throttled searches due again: 3"},
		{"doctor", DoctorResult{Configuration: "ok", SQLite: "ok", MediaRoots: 2, LAPSE: "compatible", FFprobe: "available", Webhooks: "managed (http://x)"}, "configuration: ok\nsqlite: ok\nmedia roots: ok (2)\nLAPSE: compatible\nffprobe: available\nwebhooks: managed (http://x)"},
		{"analyze", AnalyzeSyncResult{Analysis: json.RawMessage(`{"verdict":"solid","parts":1}`)}, "{\n  \"verdict\": \"solid\",\n  \"parts\": 1\n}"},
		{"forget preview", ForgetInstanceResult{Instance: "old", Media: 1}, "forget-instance preview: instance=old media=1 searches=0 installations=0 rollback_copies=0 candidates=0 rejections=0 notifications=0 events=0 replays=0\nsubtitle files installed for this instance stay on disk and will no longer be managed\nnothing was deleted; rerun with --yes to delete these records"},
		{"forget applied", ForgetInstanceResult{Instance: "old", Applied: true, RollbackCopies: 2}, "forget-instance complete: instance=old media=0 searches=0 installations=0 rollback_copies=2 candidates=0 rejections=0 notifications=0 events=0 replays=0\nsubtitle files installed for this instance stay on disk and will no longer be managed, as do 2 hidden .subsyncd-rollback-* copies beside them, which you can delete by hand"},
		{"webhook url", WebhookURLResult{Instance: "tv", URL: "http://h/webhooks/tv?token=t"}, "http://h/webhooks/tv?token=t"},
		{"rotate managed", RotateTokenResult{Instance: "tv", Managed: true}, "webhook token rotated for instance tv; start subsyncd now: the Arr connection is updated in Arr at that start."},
		{"rotate manual", RotateTokenResult{Instance: "tv"}, "webhook token rotated for instance tv; start subsyncd, then run subsyncd webhook-url --instance tv --base URL (the address Arr uses to reach subsyncd) and update the URL in Arr by hand."},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.Text(); got != test.want {
				t.Fatalf("Text() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExplainResultTextSurvivesJSON(t *testing.T) {
	mtime := time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("", 7200))
	original := ExplainResult{
		MediaID: 3, Instance: "tv", Kind: "episode", FileID: 9, Title: "Show", Episodes: "S01E01-E02",
		Probe: "completed", FingerprintSize: 100, FingerprintMTime: mtime,
		Tracks:     []ExplainTrack{{Language: "en", Embedded: true}},
		Search:     &ExplainSearch{State: "idle", Outcome: "installed", Priority: "missing", MissingAttempt: 1, NextAttemptAt: mtime},
		Candidates: []ExplainCandidate{{Provider: "p", ResultID: "r", Score: json.RawMessage(`{"total":9}`), Identity: json.RawMessage(`{}`)}},
		Rejections: []ExplainRejection{{Provider: "p", ResultID: "bad", Reason: "lapse_unsure", Artifact: "abc"}}, RejectionsSuperseded: 1,
		Installation:      &ExplainInstallation{Provider: "p", Candidate: "r", Checksum: "c", Score: json.RawMessage(`{}`), Lapse: json.RawMessage(`{}`), Rollback: true},
		PackCacheReusable: 2,
		ProviderStates:    []ExplainProviderState{{Provider: "p", Scope: "search", Reason: "rate", Remaining: 4, ResetAt: mtime}},
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ExplainResult
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Text() != original.Text() {
		t.Fatalf("text changed across JSON:\n%s\n---\n%s", decoded.Text(), original.Text())
	}
	want := "media: id=3 instance=tv kind=episode file_id=9 title=\"Show\"\nepisodes=S01E01-E02\n" +
		"inventory: tracks=1 probe=completed fingerprint_size=100 fingerprint_mtime=2026-10-01T09:00:00+02:00\n" +
		"  track: language=en embedded=true forced=false sdh=false protected=false\n" +
		"search: state=idle outcome=installed priority=missing rerun_pending=false missing_attempt=1 failure_attempt=0 next_attempt=2026-10-01T09:00:00+02:00\n" +
		"candidates: 1\n  candidate: provider=p result=r score={\"total\":9} identity={}\n" +
		"candidate_rejections: 1\ncandidate_rejections_superseded: 1\n  rejection: provider=p result=bad reason=lapse_unsure artifact=abc expires=never\n" +
		"installation: provider=p candidate=r checksum=c score={} lapse={} rollback=true fallback=false\n" +
		"pack_cache: reusable=2\nprovider_state: provider=p scope=search reason=rate remaining=4 reset=2026-10-01T09:00:00+02:00 disabled=false"
	if got := original.Text(); got != want {
		t.Fatalf("Text() =\n%s\nwant\n%s", got, want)
	}
}

func TestFindResultAndCandidatesText(t *testing.T) {
	movie := MediaMatch{Instance: "radarr-main", Kind: "movie", FileID: 42, Title: "Dune", Year: 2021}
	episode := MediaMatch{Instance: "sonarr-main", Kind: "episode", FileID: 1234, Title: "Shameless", Season: 1, Episode: 1}
	if got, want := CandidatesText([]MediaMatch{movie, episode}), "radarr-main movie 42 \"Dune\" (2021)\nsonarr-main episode 1234 \"Shameless\" S01E01"; got != want {
		t.Fatalf("CandidatesText = %q, want %q", got, want)
	}
	ranged := episode
	ranged.EpisodeEnd = 2
	if got := ranged.Text(); got != `sonarr-main episode 1234 "Shameless" S01E01-E02` {
		t.Fatalf("range = %q", got)
	}
	result := FindResult{Matches: []FindMatch{{MediaMatch: movie, Languages: []LanguageState{{"en", "installed"}, {"hr", "missing"}}}}}
	if got, want := result.Text(), "radarr-main movie 42 \"Dune\" (2021)\n  en: installed\n  hr: missing"; got != want {
		t.Fatalf("FindResult.Text = %q, want %q", got, want)
	}
	payload, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(payload), `"file_id":42,"title":"Dune","year":2021,"languages":[{"language":"en","state":"installed"}`) {
		t.Fatalf("payload = %s, %v", payload, err)
	}
	if (FindResult{}).Text() != "no matches" {
		t.Fatal("empty text")
	}
}
