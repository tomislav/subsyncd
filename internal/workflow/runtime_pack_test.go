package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func TestRuntimePackReusesAcrossEpisodesAndRestart(t *testing.T) {
	for _, rejectFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "solid", true: "rejected_member"}[rejectFirst], func(t *testing.T) {
			ctx := context.Background()
			request := serviceRequest(t)
			request.Media.Ref.Kind = domain.MediaEpisode
			request.Media.EntityID = 1
			request.Media.Season, request.Media.Episode = 1, 1
			request.Media.ReleaseGroup, request.Media.Source = "GROUP", "WEB-DL"
			candidate := broadCandidate("runtime")
			candidate.Kind, candidate.Season, candidate.Episode = domain.MediaEpisode, 1, 1
			candidate.ReleaseNames = []string{"Movie.S01E01.2024.WEB-DL-GROUP"}
			original := candidate
			adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"runtime": workflowZIP(t, map[string]string{
				"Movie.S01E01.srt": strings.ReplaceAll(installSRT, "Hello", "first-member"),
				"Movie.S01E02.srt": strings.ReplaceAll(installSRT, "Hello", "second-member"),
			})}}
			searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
			synchronizer := &fakeSynchronizer{}
			if rejectFirst {
				synchronizer.rejectText = "first-member"
			}
			service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, nil)
			var logs bytes.Buffer
			service.Events, _ = observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
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
			first, err := service.Run(ctx, request)
			want := OutcomeInstalled
			if rejectFirst {
				want = OutcomeRejected
			}
			if err != nil || first.Outcome != want || synchronizer.synchronizeCalls != 1 {
				t.Fatalf("first run = %s/%v, sync calls %d; want %s and mandatory LAPSE", first.Outcome, err, synchronizer.synchronizeCalls, want)
			}
			entries, err := db.Repository().ListPacksForEviction(ctx, service.Clock.Now())
			if err != nil || len(entries) != 1 {
				t.Fatalf("cached packs = %v/%v", entries, err)
			}
			before, err := os.ReadFile(entries[0].ManifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(before), "/secret/") {
				t.Fatal("cache leaked download reference")
			}
			var manifest pack.Manifest
			if err := json.Unmarshal(before, &manifest); err != nil || !manifest.RuntimePack {
				t.Fatalf("runtime manifest: %v", err)
			}
			originalMembers := map[string]string{}
			for _, member := range manifest.Members {
				payload, err := os.ReadFile(member.NormalizedPath)
				if err != nil {
					t.Fatal(err)
				}
				originalMembers[member.NormalizedPath] = string(payload)
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
			if rejectFirst {
				again, err := service.Run(ctx, request)
				if err != nil || again.Outcome != OutcomeRejected || adapter.downloads != 1 || synchronizer.synchronizeCalls != 1 {
					t.Fatalf("rejected member retried: %+v/%v downloads=%d sync=%d", again, err, adapter.downloads, synchronizer.synchronizeCalls)
				}
			}
			request.Media.Episode, request.Media.EntityID = 2, 2
			request.Media.Ref.FileID, request.Media.Fingerprint.FileID = 8, 8
			request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			searchCalls := searcher.calls
			logs.Reset()
			second, err := service.Run(ctx, request)
			if err != nil || second.Outcome != OutcomeInstalled || adapter.downloads != 1 || searcher.calls != searchCalls || synchronizer.synchronizeCalls != 2 {
				t.Fatalf("second run = %+v/%v downloads=%d searches=%d/%d sync=%d", second, err, adapter.downloads, searcher.calls, searchCalls, synchronizer.synchronizeCalls)
			}
			if second.Candidate.Episode != 1 || second.Candidate.Pack != nil || !reflect.DeepEqual(candidate, original) {
				t.Fatal("provider identity was rewritten")
			}
			if !strings.Contains(logs.String(), `"cache_outcome":"hit"`) {
				t.Fatal("cache hit is not observable")
			}
			after, err := os.ReadFile(entries[0].ManifestPath)
			if err != nil || string(before) != string(after) {
				t.Fatal("cache manifest mutated")
			}
			for path, before := range originalMembers {
				after, err := os.ReadFile(path)
				if err != nil || string(after) != before {
					t.Fatal("cached source mutated")
				}
			}
		})
	}
}

func TestRuntimePackSafetyAndBestEffortCache(t *testing.T) {
	for _, scenario := range []string{"write_failure", "mixed_seasons", "ambiguous", "range", "single", "exact"} {
		t.Run(scenario, func(t *testing.T) {
			request := serviceRequest(t)
			request.Media.Ref.Kind = domain.MediaEpisode
			request.Media.Season, request.Media.Episode = 1, 1
			request.Media.ReleaseGroup, request.Media.Source = "GROUP", "WEB-DL"
			candidate := broadCandidate("runtime")
			candidate.Kind, candidate.Season, candidate.Episode = domain.MediaEpisode, 1, 1
			candidate.ReleaseNames = []string{"Movie.S01E01.2024.WEB-DL-GROUP"}
			members := map[string]string{"Movie.S01E01.srt": installSRT, "Movie.S01E02.srt": installSRT}
			cache := &fakePackCache{}
			wantSync, wantPuts, wantOutcome := 1, 1, OutcomeInstalled
			switch scenario {
			case "write_failure":
				cache.putErr = errors.New("unavailable")
			case "mixed_seasons":
				delete(members, "Movie.S01E02.srt")
				members["Movie.S02E01.srt"] = installSRT
				wantPuts = 0
			case "ambiguous":
				delete(members, "Movie.S01E02.srt")
				members["Other.S01E01.srt"] = installSRT
				wantPuts, wantSync, wantOutcome = 0, 0, OutcomeRejected
			case "range":
				members = map[string]string{"Movie.S01E01-E02.srt": installSRT}
			case "single":
				members = map[string]string{"subtitle.srt": installSRT}
				wantPuts, wantSync = 0, 0
			case "exact":
				candidate.ExactHash = true
				wantPuts, wantSync = 0, 0
			}
			synchronizer := &fakeSynchronizer{}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, cache, synchronizer, nil)
			service.Providers["provider"] = &fakeProvider{id: "provider", payloads: map[string][]byte{"runtime": workflowZIP(t, members)}}
			var logs bytes.Buffer
			var err error
			service.Events, err = observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Run(context.Background(), request)
			if err != nil || result.Outcome != wantOutcome || cache.puts != wantPuts || synchronizer.synchronizeCalls != wantSync {
				t.Fatalf("outcome=%s err=%v puts=%d sync=%d", result.Outcome, err, cache.puts, synchronizer.synchronizeCalls)
			}
			if !strings.Contains(logs.String(), `"event":"archive.classified"`) {
				t.Fatal("missing classification")
			}
			for _, line := range strings.Split(logs.String(), "\n") {
				if !strings.Contains(line, `"event":"archive.classified"`) && !strings.Contains(line, `"event":"pack_cache.publication"`) {
					continue
				}
				if strings.Contains(line, ".srt") || strings.Contains(line, "/secret/") || strings.Contains(line, request.Media.Fingerprint.Path) {
					t.Fatal("archive debug event exposed private detail")
				}
			}
		})
	}
}
