package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

type scratchSynchronizer struct {
	fakeSynchronizer
	check  func(string)
	cancel context.CancelFunc
}

func (s *scratchSynchronizer) AnalyzeCandidate(ctx context.Context, candidate domain.Candidate, media, subtitle string) (domain.SyncResult, error) {
	s.check(subtitle)
	if s.cancel != nil {
		s.cancel()
	}
	return s.fakeSynchronizer.AnalyzeCandidate(ctx, candidate, media, subtitle)
}

func (s *scratchSynchronizer) SynchronizeCandidate(ctx context.Context, candidate domain.Candidate, media, input, output string) (domain.SyncResult, error) {
	s.check(input)
	s.check(output)
	return s.fakeSynchronizer.SynchronizeCandidate(ctx, candidate, media, input, output)
}

func TestWorkflowScratchUsesSystemTempAndCleansUp(t *testing.T) {
	for _, outcome := range []string{"success", "rejection", "error", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			request := serviceRequest(t)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checks := 0
			synchronizer := &scratchSynchronizer{check: func(path string) {
				checks++
				relative, err := filepath.Rel(temp, path)
				if err != nil || !strings.HasPrefix(relative, ".subsyncd-work-") || strings.HasPrefix(relative, "..") {
					t.Errorf("scratch path %q is outside system temp %q", path, temp)
				}
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(request.Media.Fingerprint.Path), ".subsyncd-work-*"))
				if err != nil || len(matches) != 0 {
					t.Errorf("scratch appeared in media directory: %v, %v", matches, err)
				}
			}}
			switch outcome {
			case "rejection":
				synchronizer.rejectAll = true
			case "error":
				synchronizer.analyzeErr = errors.New("analysis failed")
			case "cancellation":
				synchronizer.cancel = cancel
			}
			installer := &fakeInstaller{}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("candidate")}}}, nil, synchronizer, installer)
			service.LapsePolicy.Mode = "always"
			service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
			result, err := service.Run(ctx, request)
			if checks == 0 {
				t.Fatal("no candidate reached analysis")
			}
			if outcome == "success" && (err != nil || result.Outcome != OutcomeInstalled || synchronizer.synchronizeCalls != 1) {
				t.Fatalf("Run()=%+v,%v", result, err)
			}
			if outcome == "rejection" && (err != nil || result.Outcome != OutcomeRejected) {
				t.Fatalf("rejection Run()=%+v,%v", result, err)
			}
			if outcome == "error" && err == nil {
				t.Fatal("analysis failure did not propagate")
			}
			if outcome == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error=%v", err)
			}
			if outcome == "success" && filepath.Dir(installer.request.DestinationPath) != filepath.Dir(request.Media.Fingerprint.Path) {
				t.Fatal("final destination left media directory")
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("scratch not cleaned: %v,%v", entries, err)
			}
		})
	}
}
