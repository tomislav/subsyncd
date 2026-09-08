package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

func tierService(t *testing.T, preferred, fallback *fakeSearcher) (*Service, *fakeInstaller) {
	t.Helper()
	installer := &fakeInstaller{}
	s := testService(t, inventory.Inventory{}, preferred, nil, &fakeSynchronizer{}, installer)
	s.FallbackSearcher = fallback
	s.FallbackProviderOrder = []string{"fallback"}
	s.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}, "fallback": &fakeProvider{id: "fallback"}}
	return s, installer
}

func fallbackCandidate(id string, exact bool) domain.Candidate {
	c := broadCandidate(id)
	c.ProviderID, c.ExactHash = "fallback", exact
	return c
}

func TestProviderTierPreferredBroadWinsBeforeFallbackExact(t *testing.T) {
	preferred := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{provider.SearchBroad: {Candidates: []domain.Candidate{broadCandidate("preferred")}}}}
	fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}}
	s, installer := tierService(t, preferred, fallback)
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Candidate.ResultID != "preferred" || fallback.calls != 0 || installer.calls != 1 {
		t.Fatalf("result=%+v err=%v fallback calls=%d installs=%d", result, err, fallback.calls, installer.calls)
	}
}

func TestProviderTierFallbackAfterEmptyRejectedOrUnavailablePreferred(t *testing.T) {
	for _, scenario := range []string{"empty", "rejected", "network", "cooldown", "download"} {
		t.Run(scenario, func(t *testing.T) {
			preferred := &fakeSearcher{}
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}}
			s, installer := tierService(t, preferred, fallback)
			switch scenario {
			case "rejected":
				preferred.result.Candidates = []domain.Candidate{broadCandidate("bad")}
				s.Synchronizer = &fakeSynchronizer{verdicts: map[string]string{"bad": "unsure"}}
			case "network":
				preferred.result.Errors = map[string]error{"provider": errors.New("offline")}
			case "cooldown":
				preferred.result.Errors = map[string]error{"provider": &provider.CooldownError{ProviderID: "provider", ResetAt: s.Clock.Now().Add(time.Hour)}}
			case "download":
				preferred.result.Candidates = []domain.Candidate{broadCandidate("bad")}
				s.Providers["provider"].(*fakeProvider).downloadErr = errors.New("download failed")
			}
			result, err := s.Run(t.Context(), serviceRequest(t))
			if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ProviderID != "fallback" || installer.calls != 1 || !installer.request.Fallback || result.NextUpgrade.IsZero() {
				t.Fatalf("result=%+v err=%v installs=%d request=%+v", result, err, installer.calls, installer.request)
			}
			want := s.Clock.Now().Add(7 * 24 * time.Hour)
			if scenario == "cooldown" {
				want = s.Clock.Now().Add(time.Hour)
			}
			if !result.NextUpgrade.Equal(want) {
				t.Fatalf("next=%v want=%v", result.NextUpgrade, want)
			}
			if scenario == "rejected" && len(s.Repository.(*workflowRepository).candidates) != 2 {
				t.Fatal("lost preferred scored evidence")
			}
			if s.Inventory.(*fakeInventory).calls != 1 {
				t.Fatal("inventory refreshed more than once")
			}
		})
	}
}

func TestProviderTierPromotesExactFallbackWithoutScoreDelta(t *testing.T) {
	request := serviceRequest(t)
	preferred := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("preferred")}}}
	fallback := &fakeSearcher{}
	s, installer := tierService(t, preferred, fallback)
	existing := matchingInstallation(request, fallbackCandidate("old", true), []byte(`{"total":100,"contributions":[{"signal":"exact_hash","points":100}]}`))
	existing.Fallback = true
	s.Repository = &workflowRepository{installation: existing, found: true}
	s.Inventory = &fakeInventory{current: managedSidecarInventory(existing)}
	result, err := s.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ProviderID != "provider" || installer.request.Fallback || s.Synchronizer.(*fakeSynchronizer).synchronizeCalls != 1 || fallback.calls != 0 {
		t.Fatalf("result=%+v err=%v request=%+v fallback calls=%d", result, err, installer.request, fallback.calls)
	}
}

func TestProviderTierExactFallbackKeepsPromotionScheduleWhenPreferredEmpty(t *testing.T) {
	request := serviceRequest(t)
	s, installer := tierService(t, &fakeSearcher{}, &fakeSearcher{})
	existing := matchingInstallation(request, fallbackCandidate("old", true), []byte(`{"total":100,"contributions":[{"signal":"exact_hash","points":100}]}`))
	existing.Fallback = true
	s.Repository = &workflowRepository{installation: existing, found: true}
	s.Inventory = &fakeInventory{current: managedSidecarInventory(existing)}
	result, err := s.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeSatisfied || !result.NextUpgrade.Equal(s.Clock.Now().Add(7*24*time.Hour)) || installer.calls != 0 {
		t.Fatalf("result=%+v err=%v installs=%d", result, err, installer.calls)
	}
}

func TestProviderTierPublicationFailureAndCancellationNeverStartFallback(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication", true: "cancel"}[cancel], func(t *testing.T) {
			preferred := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("preferred")}}}
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}}
			s, installer := tierService(t, preferred, fallback)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			if cancel {
				s.Synchronizer.(*fakeSynchronizer).synchronizeHook = func(domain.Candidate) { stop() }
			} else {
				installer.err = errors.New("publication failed")
			}
			_, err := s.Run(ctx, serviceRequest(t))
			if err == nil || fallback.calls != 0 {
				t.Fatalf("err=%v fallback=%d", err, fallback.calls)
			}
		})
	}
}

func TestProviderTierNeverDowngradesPreferredOrTouchesProtectedFallback(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(map[bool]string{false: "preferred", true: "protected"}[protected], func(t *testing.T) {
			request := serviceRequest(t)
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("better", true)}}}
			s, installer := tierService(t, &fakeSearcher{}, fallback)
			candidate := broadCandidate("old")
			if protected {
				candidate = fallbackCandidate("old", false)
			}
			existing := matchingInstallation(request, candidate, []byte(`{"total":35}`))
			current := managedSidecarInventory(existing)
			if protected {
				current.Tracks[0].Protected = true
				current.Tracks[0].Checksum = "edited"
			}
			s.Inventory = &fakeInventory{current: current}
			s.Repository = &workflowRepository{installation: existing, found: true}
			_, err := s.Run(t.Context(), request)
			if err != nil || fallback.calls != 0 || installer.calls != 0 {
				t.Fatalf("err=%v fallback=%d installs=%d", err, fallback.calls, installer.calls)
			}
		})
	}
}

func TestProviderTierFallbackCacheCannotPreemptPreferredSearch(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	cached := fallbackCandidate("cached", false)
	cached.Kind, cached.Season, cached.Episode = domain.MediaEpisode, 1, 2
	preferredCandidate := broadCandidate("preferred")
	preferredCandidate.Kind, preferredCandidate.Season, preferredCandidate.Episode = domain.MediaEpisode, 1, 2
	preferred := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{preferredCandidate}}}
	fallback := &fakeSearcher{}
	s, _ := tierService(t, preferred, fallback)
	s.PackCache = &fakePackCache{found: true, member: pack.CachedMember{Candidate: cached, Path: writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)}}
	result, err := s.Run(t.Context(), request)
	if err != nil || result.Candidate.ProviderID != "provider" || fallback.calls != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProviderTierTechnicalPreferredFailureSurvivesEmptyFallback(t *testing.T) {
	s, _ := tierService(t, &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"provider": errors.New("offline")}}}, &fakeSearcher{})
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err == nil || len(s.Repository.(*workflowRepository).rejections) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProviderTierSameFallbackUpgradeRequiresDelta(t *testing.T) {
	for _, score := range []int{35, 45} {
		t.Run(fmt.Sprint(score), func(t *testing.T) {
			request := serviceRequest(t)
			candidate := fallbackCandidate("new", false)
			if score == 45 {
				request.Media.Edition = "Extended"
				candidate.ReleaseNames = []string{"Movie.2024.Extended"}
			}
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
			s, installer := tierService(t, &fakeSearcher{}, fallback)
			existing := matchingInstallation(request, fallbackCandidate("old", false), []byte(`{"total":35}`))
			s.Repository = &workflowRepository{installation: existing, found: true}
			s.Inventory = &fakeInventory{current: managedSidecarInventory(existing)}
			result, err := s.Run(t.Context(), request)
			want := 0
			if score == 45 {
				want = 1
			}
			if err != nil || installer.calls != want || result.NextUpgrade.IsZero() {
				t.Fatalf("result=%+v err=%v installs=%d", result, err, installer.calls)
			}
		})
	}
}

func TestProviderTierInstallationProvenanceSurvivesRestartAndPromotion(t *testing.T) {
	request := serviceRequest(t)
	request.Media.EntityID = 1
	dbPath := filepath.Join(t.TempDir(), "state.db")
	database, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	request.MediaID, _, err = database.Repository().UpsertMedia(t.Context(), request.Media)
	if err != nil {
		t.Fatal(err)
	}
	preferred := &fakeSearcher{}
	fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("temporary", true)}}}
	s, _ := tierService(t, preferred, fallback)
	s.Repository = database.Repository()
	s.Installer = Installer{Repository: database.Repository(), MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}, NotifierNames: []string{"silo"}}
	result, err := s.Run(t.Context(), request)
	if err != nil || !result.Installation.Fallback || result.NextUpgrade.IsZero() {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	existing, found, err := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
	if err != nil || !found || !existing.Fallback {
		t.Fatalf("reopened=%+v found=%v err=%v", existing, found, err)
	}
	s.Repository = database.Repository()
	s.Installer = Installer{Repository: database.Repository(), MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}, NotifierNames: []string{"silo"}}
	s.Inventory = &fakeInventory{current: managedSidecarInventory(existing)}
	preferred.result.Candidates = []domain.Candidate{broadCandidate("preferred")}
	result, err = s.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Installation.Fallback || result.Candidate.ProviderID != "provider" {
		t.Fatalf("promotion=%+v err=%v", result, err)
	}
	promoted, found, err := database.Repository().GetInstallation(t.Context(), request.MediaID, request.Language)
	if err != nil || !found || promoted.Fallback || promoted.ProviderID != "provider" {
		t.Fatalf("promoted=%+v found=%v err=%v", promoted, found, err)
	}
	payload, err := os.ReadFile(promoted.Path)
	if err != nil || !strings.Contains(string(payload), "preferred") {
		t.Fatalf("published=%q err=%v", payload, err)
	}
}

func TestProviderTierFallbackRetriesPartiallyUnavailablePreferredAtReset(t *testing.T) {
	preferred := &fakeSearcher{}
	s, _ := tierService(t, preferred, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}})
	s.ProviderOrder = []string{"provider", "other"}
	reset := s.Clock.Now().Add(time.Hour)
	preferred.result.Errors = map[string]error{"provider": &provider.CooldownError{ProviderID: "provider", ResetAt: reset}}
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || !result.NextUpgrade.Equal(reset) {
		t.Fatalf("outcome=%s next=%v err=%v", result.Outcome, result.NextUpgrade, err)
	}
}

func TestProviderTierSearchPersistenceFailureIsTerminal(t *testing.T) {
	for _, phase := range []provider.SearchMode{provider.SearchExactHash, provider.SearchBroad} {
		t.Run(string(phase), func(t *testing.T) {
			sentinel := errors.New("database failure")
			preferred := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{phase: {Errors: map[string]error{"provider": &provider.SearchPersistenceError{Err: sentinel}}, Candidates: []domain.Candidate{exactCandidate("must-not-install")}}}}
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}}
			s, installer := tierService(t, preferred, fallback)
			_, err := s.Run(t.Context(), serviceRequest(t))
			if !errors.Is(err, sentinel) || fallback.calls != 0 || installer.calls != 0 {
				t.Fatalf("err=%v fallback=%d installs=%d", err, fallback.calls, installer.calls)
			}
		})
	}
}

func TestProviderTierRetainsRefreshedFallbackAssessment(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	candidate := fallbackCandidate("same", false)
	candidate.ReleaseNames = []string{"Movie.2024-GROUP"}
	s, _ := tierService(t, &fakeSearcher{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}})
	existing := matchingInstallation(request, candidate, []byte(`{"total":35}`))
	s.Repository = &workflowRepository{installation: existing, found: true}
	s.Inventory = &fakeInventory{current: managedSidecarInventory(existing)}
	result, err := s.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeSatisfied || result.Score.Total != 60 {
		t.Fatalf("score=%d outcome=%s err=%v", result.Score.Total, result.Outcome, err)
	}
}
