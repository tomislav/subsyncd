package workflow

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
	"subsyncd/internal/testutil"
)

func TestPermanentRejectionSurvivesRestartAndAllowsNewCandidates(t *testing.T) {
	for _, kind := range []domain.MediaKind{domain.MediaMovie, domain.MediaEpisode} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			request := serviceRequest(t)
			request.Media.EntityID = 1
			request.Media.Ref.Kind = kind
			candidate := broadCandidate("old")
			candidate.Kind = kind
			if kind == domain.MediaEpisode {
				request.Media.Ref.Instance = "sonarr-main"
				request.Media.Season, request.Media.Episode = 1, 2
				candidate.Season, candidate.Episode = 1, 2
				candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
			}
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
			if err != nil {
				t.Fatal(err)
			}
			searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
			adapter := &fakeProvider{id: "provider"}
			synchronizer := &fakeSynchronizer{}
			service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
			service.Repository = db.Repository()
			service.Providers = map[string]provider.Provider{"provider": adapter}
			if ok, err := service.recordCandidateRejection(ctx, request, candidate, "checksum", &syncer.VerdictError{Verdict: "unsure"}); err != nil || !ok {
				t.Fatalf("record: %v %v", ok, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			service.Repository = db.Repository()
			service.Clock = fixedWorkflowClock{at: service.Clock.Now().AddDate(10, 0, 0)}
			result, err := service.Run(ctx, request)
			if err != nil || adapter.downloads != 0 || synchronizer.synchronizeCalls != 0 {
				t.Fatalf("unchanged rejection repeated: result=%+v err=%v downloads=%d sync=%d", result, err, adapter.downloads, synchronizer.synchronizeCalls)
			}
			fresh := candidate
			fresh.ResultID = "new"
			fresh.Pack = nil
			searcher.result.Candidates = append(searcher.result.Candidates, fresh)
			result, err = service.Run(ctx, request)
			if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "new" {
				t.Fatalf("new candidate blocked: %+v %v", result, err)
			}
			if err := db.Repository().ClearCandidateRejections(ctx, request.MediaID, request.Language); err != nil {
				t.Fatal(err)
			}
			if _, found, err := service.candidateRejection(ctx, request, candidate, ""); err != nil || found {
				t.Fatalf("manual clear: %v %v", found, err)
			}
		})
	}
}

func TestEpisodeRejectionChangesWithSelectionEvidence(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	candidate := broadCandidate("pack")
	candidate.Kind = domain.MediaEpisode
	candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	ctx := context.Background()
	if _, err := service.recordCandidateRejection(ctx, request, candidate, "", &pack.SelectionError{Reason: "ambiguous"}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name  string
		apply func(*Request)
	}{
		{"episode title", func(r *Request) { r.Media.EpisodeTitle = "The Long Journey" }},
		{"absolute episode", func(r *Request) { r.Media.AbsoluteEpisode = 12 }},
		{"episode", func(r *Request) { r.Media.Episode = 3 }},
		{"season", func(r *Request) { r.Media.Season = 2 }},
		{"other episode file", func(r *Request) { r.MediaID++ }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := request
			change.apply(&changed)
			if _, found, err := service.candidateRejection(ctx, changed, candidate, ""); err != nil || found {
				t.Fatalf("corrected/different episode blocked: %v %v", found, err)
			}
		})
	}
	if _, found, err := service.candidateRejection(ctx, request, candidate, ""); err != nil || !found {
		t.Fatalf("original episode rejection lost: %v %v", found, err)
	}
}

func TestAmbiguousMovieArchiveIsRemembered(t *testing.T) {
	request := serviceRequest(t)
	candidate := broadCandidate("ambiguous")
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"ambiguous": workflowZIP(t, map[string]string{"first.srt": installSRT, "second.srt": installSRT})}, filenames: map[string]string{"ambiguous": "subs.zip"}}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": adapter}
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeRejected {
		t.Fatalf("ambiguous archive classified technical: %+v %v", result, err)
	}
	service.Clock = fixedWorkflowClock{at: service.Clock.Now().Add(365 * 24 * time.Hour)}
	_, err = service.Run(context.Background(), request)
	if err != nil || adapter.downloads != 1 {
		t.Fatalf("ambiguous archive downloaded again: %d %v", adapter.downloads, err)
	}
}

func TestRejectionSignatureTreatsReleaseEvidenceAsASet(t *testing.T) {
	first := broadCandidate("stable")
	first.ReleaseNames = []string{"release-b", "release-a", "release-a"}
	first.Pack = &domain.PackInfo{Scope: domain.PackSeason, DirectMembers: []domain.PackMemberRef{{ID: "b", Filename: "b.srt"}, {ID: "a", Filename: "a.srt"}, {ID: "a", Filename: "a.srt"}}}
	second := first
	second.ReleaseNames = []string{"release-a", "release-b"}
	packCopy := *first.Pack
	second.Pack = &packCopy
	second.Pack.DirectMembers = []domain.PackMemberRef{{ID: "a", Filename: "a.srt"}, {ID: "b", Filename: "b.srt"}}
	a, _ := candidateSignature(first)
	b, _ := candidateSignature(second)
	if a != b {
		t.Fatal("same evidence reordered bypasses rejection")
	}
	if first.ReleaseNames[0] != "release-b" || first.Pack.DirectMembers[0].ID != "b" {
		t.Fatal("signature mutated candidate")
	}
}

func TestRejectedCachedPackDoesNotHideAnotherPack(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	request.Media.Ref.Instance = "sonarr-main"
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	clock := testutil.NewClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	cache, err := pack.NewCache(filepath.Join(t.TempDir(), "cache"), db.Repository(), clock, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{}
	sync := &fakeSynchronizer{}
	service := testService(t, inventory.Inventory{}, searcher, cache, sync, &fakeInstaller{})
	service.Repository = db.Repository()
	service.Clock = clock
	for _, id := range []string{"good", "bad"} {
		candidate := broadCandidate(id)
		candidate.Kind = domain.MediaEpisode
		candidate.Season = 1
		candidate.Episode = 2
		candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
		payload := workflowZIP(t, map[string]string{"Show.S01E02.srt": installSRT})
		manifest, err := pack.Extract(ctx, candidate, bytes.NewReader(payload), int64(len(payload)), filepath.Join(t.TempDir(), "extract"), pack.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
		if err := cache.Put(ctx, manifest, clock.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if id == "bad" {
			if _, err := service.recordCandidateRejection(ctx, request, candidate, manifest.Members[0].Checksum, &syncer.VerdictError{Verdict: "unsure"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, err := service.Run(ctx, request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "good" || searcher.calls != 0 || sync.synchronizeCalls != 1 {
		t.Fatalf("cached alternative hidden: %+v err=%v provider searches=%d sync=%d", result, err, searcher.calls, sync.synchronizeCalls)
	}
}
