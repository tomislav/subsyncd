package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

type artifactSynchronizer struct {
	fakeSynchronizer
	t         *testing.T
	results   map[string]domain.SyncResult
	invalid   string
	failure   string
	paths     map[string]string
	sources   map[string]string
	checksums map[string]string
}

func (s *artifactSynchronizer) SynchronizeCandidate(ctx context.Context, c domain.Candidate, media, input, output string) (domain.SyncResult, error) {
	s.t.Helper()
	for _, previous := range s.paths {
		if previous == output {
			s.t.Fatalf("reused synchronized output %q", output)
		}
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		s.t.Fatalf("output is not new: %v", err)
	}
	s.paths[c.ResultID], s.sources[c.ResultID] = output, input
	s.checksums[c.ResultID], _ = fileChecksum(input)
	if _, err := s.fakeSynchronizer.SynchronizeCandidate(ctx, c, media, input, output); err != nil {
		return domain.SyncResult{}, err
	}
	payload := "1\n00:00:03,000 --> 00:00:04,000\nPrepared " + c.ResultID + "\n"
	if c.ResultID == s.invalid {
		payload = "1\n00:00:01,000 --> 01:36:00,000\nInvalid duration\n"
	}
	if err := os.WriteFile(output, []byte(payload), 0600); err != nil {
		return domain.SyncResult{}, err
	}
	if c.ResultID == s.failure {
		return domain.SyncResult{}, &syncer.VerdictError{Verdict: "unsure"}
	}
	return s.results[c.ResultID], nil
}

func TestSingleRunInstallsPreparedBytesAndTheirProvenance(t *testing.T) {
	for _, scenario := range []string{"leader", "ties", "retained_fallback", "cached_failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			request := serviceRequest(t)
			request.Media.EntityID = 1
			candidates := []domain.Candidate{broadCandidate("a"), broadCandidate("b"), broadCandidate("c")}
			results := map[string]domain.SyncResult{}
			for id, confidence := range map[string]float64{"a": 0.6, "b": 0.95, "c": 0.8, "cached": 0.99} {
				results[id] = domain.SyncResult{Verdict: "solid", Mode: "auto/shifted", Reference: "vad", OffsetMS: 2000, Ratio: 1, Confidence: confidence, Agreement: 0.88, Coverage: 0.77, Parts: 2, Splits: 1}
			}
			synchronizer := &artifactSynchronizer{t: t, results: results, paths: map[string]string{}, sources: map[string]string{}, checksums: map[string]string{}}
			// A separate dry-run result would incorrectly favor a. Only the result
			// that produced the prepared artifact may rank or become provenance.
			synchronizer.confidence = map[string]float64{"a": 0.99, "b": 0.1, "c": 0.2}
			want, calls := "b", []string{"a", "b", "c"}
			var cache PackCache
			var cachePath string
			if scenario == "leader" {
				request.Media.ReleaseGroup = "GROUP"
				candidates[0].ReleaseNames = []string{"Movie.2024-GROUP"}
				want, calls = "a", []string{"a"}
			}
			if scenario == "retained_fallback" {
				synchronizer.invalid, want = "b", "c"
			}
			if scenario == "cached_failure" {
				request.Media.Ref.Kind = domain.MediaEpisode
				request.Media.Season, request.Media.Episode = 1, 2
				for i := range candidates {
					candidates[i].Kind = domain.MediaEpisode
					candidates[i].Season, candidates[i].Episode = 1, 2
				}
				cached := candidates[0]
				cached.ResultID = "cached"
				cached.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
				cachePath = writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
				checksum, err := fileChecksum(cachePath)
				if err != nil {
					t.Fatal(err)
				}
				cache = &fakePackCache{found: true, member: pack.CachedMember{Candidate: cached, Path: cachePath, Checksum: checksum}}
				synchronizer.failure = "cached"
				calls = []string{"cached", "a", "b", "c"}
			}
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			installer := &Installer{Repository: database.Repository(), MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}, NotifierNames: []string{"silo"}}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: candidates}}, cache, synchronizer, installer)
			service.Repository = database.Repository()
			service.LapsePolicy.Mode = "always"
			service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
			var logs bytes.Buffer
			service.Events, err = observability.New(&logs, observability.Options{Level: "info", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Run(ctx, request)
			if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != want {
				t.Fatalf("Run=%+v, %v", result, err)
			}
			if len(synchronizer.analyzed) != 0 || !slices.Equal(synchronizer.synchronized, calls) {
				t.Fatalf("analysis/sync=%v/%v", synchronizer.analyzed, synchronizer.synchronized)
			}
			payload, err := os.ReadFile(result.Installation.Path)
			expected := "1\n00:00:03,000 --> 00:00:04,000\nPrepared " + want + "\n"
			if err != nil || string(payload) != expected {
				t.Fatalf("installed bytes=%q, %v", payload, err)
			}
			persisted, found, err := database.Repository().GetInstallation(ctx, request.MediaID, request.Language)
			if err != nil || !found {
				t.Fatalf("installation=%+v %v %v", persisted, found, err)
			}
			var provenance domain.SyncResult
			if err := json.Unmarshal(persisted.SyncResultJSON, &provenance); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(provenance, results[want]) || !reflect.DeepEqual(result.SyncResult, results[want]) {
				t.Fatalf("provenance=%+v result=%+v want=%+v", provenance, result.SyncResult, results[want])
			}
			checksum, err := fileChecksum(persisted.Path)
			if err != nil || checksum != persisted.Checksum || checksum == synchronizer.checksums[want] {
				t.Fatalf("installed checksum=%s stored=%s original=%s (%v)", checksum, persisted.Checksum, synchronizer.checksums[want], err)
			}
			for _, path := range synchronizer.paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("prepared scratch survives: %v", err)
				}
			}
			if cachePath != "" {
				if payload, err := os.ReadFile(cachePath); err != nil || string(payload) != installSRT {
					t.Fatalf("cache source changed: %q %v", payload, err)
				}
			}
			if scenario == "retained_fallback" || scenario == "cached_failure" {
				rejected := "b"
				if scenario == "cached_failure" {
					rejected = "cached"
				}
				rejectedCandidate := candidates[1]
				if rejected == "cached" {
					rejectedCandidate = cache.(*fakePackCache).member.Candidate
				}
				rejection, found, err := service.candidateRejection(ctx, request, rejectedCandidate, synchronizer.checksums[rejected])
				if err != nil || !found || rejection.ArtifactChecksum != synchronizer.checksums[rejected] {
					t.Fatalf("original rejection=%+v found=%v err=%v", rejection, found, err)
				}
			}
			records := workflowLogRecords(t, logs.String())
			if strings.Contains(logs.String(), "lapse.analysis_") || len(workflowEvents(records, "lapse.sync_started")) != len(calls) {
				t.Fatalf("acquisition lifecycle=%s", logs.String())
			}
			completed := len(calls)
			if scenario == "cached_failure" {
				completed--
			}
			if len(workflowEvents(records, "lapse.sync_completed")) != completed {
				t.Fatalf("sync completions=%s", logs.String())
			}
			lastCompleted := -1
			for i, record := range records {
				if record["event"] == "lapse.sync_completed" {
					lastCompleted = i
				}
			}
			if lastCompleted >= workflowEventIndex(records, "candidate.selected") {
				t.Fatalf("selected before all preparation completed: %s", logs.String())
			}
		})
	}
}

func TestCachedPreparationCancellationStartsNoProviderSearch(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	candidate := broadCandidate("cached")
	candidate.Kind, candidate.Season, candidate.Episode = domain.MediaEpisode, 1, 2
	candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	path := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	checksum, err := fileChecksum(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := &fakePackCache{found: true, member: pack.CachedMember{Candidate: candidate, Path: path, Checksum: checksum}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	synchronizer := &fakeSynchronizer{synchronizeHook: func(domain.Candidate) { cancel() }}
	searcher := &fakeSearcher{}
	service := testService(t, inventory.Inventory{}, searcher, cache, synchronizer, &fakeInstaller{})
	_, err = service.Run(ctx, request)
	if !errors.Is(err, context.Canceled) || searcher.calls != 0 || len(synchronizer.analyzed) != 0 || !slices.Equal(synchronizer.synchronized, []string{"cached"}) {
		t.Fatalf("err=%v search=%d analysis/sync=%v/%v", err, searcher.calls, synchronizer.analyzed, synchronizer.synchronized)
	}
}
