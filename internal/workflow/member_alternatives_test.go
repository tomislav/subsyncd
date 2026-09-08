package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"testing"
)

func TestPackVersionsPrepareOnceAndChooseConfidence(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{
		"Movie.S04E13.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "web-version"),
		"Movie.04x13.HDTV.srt": strings.ReplaceAll(installSRT, "Hello", "hdtv-version"),
		"Movie.S04E12.srt":     installSRT,
	})}}
	sync := &fakeSynchronizer{confidence: map[string]float64{"web-version": 0.95, "hdtv-version": 0.6}}
	installer := &versionInstaller{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, installer)
	service.Providers["provider"] = adapter
	var logs bytes.Buffer
	service.Events, _ = observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.SyncResult.Confidence != 0.95 || sync.synchronizeCalls != 2 || adapter.downloads != 1 {
		t.Fatalf("result=%+v err=%v calls=%d downloads=%d", result, err, sync.synchronizeCalls, adapter.downloads)
	}
	if len(installer.payloads) != 1 || !strings.Contains(installer.payloads[0], "web-version") {
		t.Fatalf("installed bytes = %v", installer.payloads)
	}
	selected := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.selected")
	if len(selected) != 1 || selected[0]["member_index"] == nil {
		t.Fatalf("winner missing member identity: %+v", selected)
	}
	starts := workflowEvents(workflowLogRecords(t, logs.String()), "lapse.sync_started")
	if len(starts) != 2 || starts[0]["member_index"] == nil || starts[1]["member_index"] == starts[0]["member_index"] {
		t.Fatalf("missing member correlation: %+v", starts)
	}
}

func TestPackVersionRejectionDoesNotHideCachedSibling(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.EntityID = 1
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{
		"Movie.S04E13.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "good-version"),
		"Movie.04x13.HDTV.srt": strings.ReplaceAll(installSRT, "Hello", "bad-version"),
	})}}
	sync := &fakeSynchronizer{rejectText: "bad-version"}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, sync, &fakeInstaller{})
	service.Providers["provider"] = adapter
	statePath, cachePath := filepath.Join(t.TempDir(), "state.db"), filepath.Join(t.TempDir(), "cache")
	db, err := store.Open(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	service.Repository = db.Repository()
	request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	service.PackCache, err = pack.NewCache(cachePath, db.Repository(), service.Clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{}
	for run := 0; run < 2; run++ {
		result, err := service.Run(ctx, request)
		if err != nil || result.Outcome != OutcomeInstalled {
			t.Fatalf("run %d: %+v %v", run, result, err)
		}
		if run == 0 {
			entries, err := db.Repository().ListPacksForEviction(ctx, service.Clock.Now())
			if err != nil || len(entries) != 1 {
				t.Fatalf("cache entries=%v/%v", entries, err)
			}
			payload, err := os.ReadFile(entries[0].ManifestPath)
			if err != nil {
				t.Fatal(err)
			}
			original[entries[0].ManifestPath] = payload
			var manifest pack.Manifest
			if err := json.Unmarshal(payload, &manifest); err != nil {
				t.Fatal(err)
			}
			for _, member := range manifest.Members {
				payload, err := os.ReadFile(member.NormalizedPath)
				if err != nil {
					t.Fatal(err)
				}
				original[member.NormalizedPath] = payload
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(ctx, statePath)
			if err != nil {
				t.Fatal(err)
			}
			service.Repository = db.Repository()
			service.PackCache, err = pack.NewCache(cachePath, db.Repository(), service.Clock, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for path, want := range original {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("cache source changed: %v", err)
		}
	}
	if adapter.downloads != 1 || sync.synchronizeCalls != 3 {
		t.Fatalf("downloads=%d sync=%d; expected one download, bad version evaluated once and good version twice", adapter.downloads, sync.synchronizeCalls)
	}
}

func TestPackVersionFailuresRemainScopedAndRetryable(t *testing.T) {
	for _, technical := range []bool{false, true} {
		t.Run(map[bool]string{false: "deterministic", true: "technical"}[technical], func(t *testing.T) {
			request := serviceRequest(t)
			request.Media.Ref.Kind = domain.MediaEpisode
			request.Media.Season, request.Media.Episode = 4, 13
			candidate := broadCandidate("versions")
			candidate.Kind = domain.MediaEpisode
			candidate.Season, candidate.Episode = 4, 13
			adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{"Movie.S04E13.A.srt": installSRT, "Movie.S04E13.B.srt": strings.ReplaceAll(installSRT, "Hello", "other")})}}
			sync := &fakeSynchronizer{rejectAll: !technical}
			if technical {
				sync.synchronizeErr = errors.New("process failed")
			}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, &fakeInstaller{})
			service.Providers["provider"] = adapter
			for run := 0; run < 2; run++ {
				result, err := service.Run(context.Background(), request)
				if technical && err == nil || !technical && (err != nil || result.Outcome != OutcomeRejected) {
					t.Fatalf("run %d = %+v %v", run, result, err)
				}
			}
			want := 2
			if technical {
				want = 4
			}
			if sync.synchronizeCalls != want {
				t.Fatalf("sync calls=%d want %d", sync.synchronizeCalls, want)
			}
			if technical && len(service.Repository.(*workflowRepository).rejections) != 0 {
				t.Fatal("technical failure persisted")
			}
			if !technical && adapter.downloads != 1 {
				t.Fatal("exhausted pack downloaded again")
			}
		})
	}
}

func TestPackVersionCancellationStopsBeforeNextMember(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	sync := &fakeSynchronizer{synchronizeHook: func(domain.Candidate) { cancel() }}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, &fakeInstaller{})
	service.Providers["provider"] = &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{"Movie.S04E13.A.srt": installSRT, "Movie.S04E13.B.srt": strings.ReplaceAll(installSRT, "Hello", "other")})}}
	_, err := service.Run(ctx, request)
	if !errors.Is(err, context.Canceled) || sync.synchronizeCalls != 1 || len(service.Repository.(*workflowRepository).rejections) != 0 {
		t.Fatalf("cancellation = %v calls=%d", err, sync.synchronizeCalls)
	}
}

func TestPackVersionsRememberExhaustedInstallationFallback(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{"Movie.S04E13.A.srt": installSRT, "Movie.S04E13.B.srt": strings.ReplaceAll(installSRT, "Hello", "other")})}}
	sync := &fakeSynchronizer{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, &fakeInstaller{err: &subtitleValidationError{reason: "invalid timestamps"}})
	service.Providers["provider"] = adapter
	for run := 0; run < 2; run++ {
		result, err := service.Run(context.Background(), request)
		if err != nil || result.Outcome != OutcomeRejected {
			t.Fatalf("run %d: %+v %v", run, result, err)
		}
	}
	if adapter.downloads != 1 || sync.synchronizeCalls != 2 {
		t.Fatalf("repeated exhausted versions: downloads=%d sync=%d", adapter.downloads, sync.synchronizeCalls)
	}
}

// Inspect the real prepared artifact at the installation boundary, including
// validation; failures model only the allowed pre-publication content rejection.
type versionInstaller struct {
	fakeInstaller
	payloads []string
	reject   string
}

func (i *versionInstaller) Install(ctx context.Context, r InstallRequest) (store.Installation, error) {
	if _, err := validatedSubtitle(r.SourcePath, r.Media.Duration); err != nil {
		return store.Installation{}, err
	}
	payload, err := os.ReadFile(r.SourcePath)
	if err != nil {
		return store.Installation{}, err
	}
	i.payloads = append(i.payloads, string(payload))
	if i.reject != "" && strings.Contains(string(payload), i.reject) {
		return store.Installation{}, &subtitleValidationError{reason: "invalid timestamps"}
	}
	return i.fakeInstaller.Install(ctx, r)
}

func TestPackVersionsRetainOutputForInstallationFallback(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{"Movie.S04E13.A.srt": strings.ReplaceAll(installSRT, "Hello", "best"), "Movie.S04E13.B.srt": strings.ReplaceAll(installSRT, "Hello", "backup")})}}
	sync := &fakeSynchronizer{confidence: map[string]float64{"best": 0.95, "backup": 0.6}}
	installer := &versionInstaller{reject: "best"}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, installer)
	service.Providers["provider"] = adapter
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || sync.synchronizeCalls != 2 || len(installer.payloads) != 2 || !strings.Contains(installer.payloads[1], "backup") {
		t.Fatalf("fallback: %+v/%v calls=%d payloads=%v", result, err, sync.synchronizeCalls, installer.payloads)
	}
	rejections := service.Repository.(*workflowRepository).rejections
	if len(rejections) != 1 || rejections[0].ArtifactChecksum == "" || !strings.Contains(rejections[0].CandidateSignature, "/member-") {
		t.Fatalf("wrong rejection scope: %+v", rejections)
	}
}

func TestInfoLogsExplainRetainedRejectionSkip(t *testing.T) {
	request := serviceRequest(t)
	candidate := broadCandidate("retained")
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, nil, nil)
	_, err := service.recordCandidateRejection(context.Background(), request, candidate, "", &pack.SelectionError{Rule: "none", Reason: "unknown episode"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	service.Events, _ = observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	_, err = service.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	skipped := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.skipped")
	if len(skipped) != 1 || skipped[0]["reason_code"] != "pack_selection" {
		t.Fatalf("missing retained rejection at info: %s", logs.String())
	}
}

type versionFailureSynchronizer struct {
	fakeSynchronizer
	failure error
}

func (s *versionFailureSynchronizer) SynchronizeCandidate(ctx context.Context, c domain.Candidate, media, input, output string) (domain.SyncResult, error) {
	payload, err := os.ReadFile(input)
	if err != nil {
		return domain.SyncResult{}, err
	}
	if strings.Contains(string(payload), "technical") {
		s.synchronizeCalls++
		return domain.SyncResult{}, s.failure
	}
	return s.fakeSynchronizer.SynchronizeCandidate(ctx, c, media, input, output)
}
func TestMixedVersionFailureCannotAggregateRejection(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{"Movie.S04E13.A.srt": strings.ReplaceAll(installSRT, "Hello", "bad"), "Movie.S04E13.B.srt": strings.ReplaceAll(installSRT, "Hello", "technical")})}}
	sync := &versionFailureSynchronizer{fakeSynchronizer: fakeSynchronizer{rejectText: "bad"}, failure: errors.New("process failure")}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, nil)
	service.Providers["provider"] = adapter
	for run := 0; run < 2; run++ {
		if _, err := service.Run(context.Background(), request); err == nil {
			t.Fatal("technical result lost")
		}
	}
	if sync.synchronizeCalls != 3 || adapter.downloads != 2 {
		t.Fatalf("mixed retry: sync=%d downloads=%d", sync.synchronizeCalls, adapter.downloads)
	}
	rejections := service.Repository.(*workflowRepository).rejections
	if len(rejections) != 1 || !strings.Contains(rejections[0].CandidateSignature, "/member-") {
		t.Fatalf("mixed result blacklisted: %+v", rejections)
	}
}
