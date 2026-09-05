package workflow

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

type cleanupProvider struct {
	fakeProvider
	before func(domain.Candidate)
}

func (p *cleanupProvider) Download(ctx context.Context, c domain.Candidate, w io.Writer) (provider.DownloadMetadata, error) {
	if p.before != nil {
		p.before(c)
	}
	return p.fakeProvider.Download(ctx, c, w)
}
func scratchFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRejectedExactScratchReleasedBeforeNextDownload(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	adapter := &cleanupProvider{fakeProvider: fakeProvider{id: "provider", filenames: map[string]string{"first": "Movie.forced.srt"}}}
	adapter.before = func(c domain.Candidate) {
		if c.ResultID == "second" {
			for _, path := range scratchFiles(t, temp) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() > 0 {
					t.Errorf("previous rejected scratch survives: %s", path)
				}
			}
		}
	}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("first"), exactCandidate("second")}}}, nil, nil, nil)
	service.Providers = map[string]provider.Provider{"provider": adapter}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Candidate.ResultID != "second" {
		t.Fatalf("Run=%+v,%v", result, err)
	}
}

type cleanupPackCache struct {
	fakePackCache
	t       *testing.T
	members int
}

func (c *cleanupPackCache) Put(_ context.Context, m pack.Manifest, _ time.Time) error {
	c.members = len(m.Members)
	for _, member := range m.Members {
		if _, err := os.ReadFile(member.NormalizedPath); err != nil {
			c.t.Fatal(err)
		}
	}
	return nil
}
func TestDownloadScratchRetainsOnlySelectedAfterCaching(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season = 1
	request.Media.Episode = 2
	candidate := broadCandidate("pack")
	candidate.Kind = domain.MediaEpisode
	candidate.Pack = &domain.PackInfo{}
	cache := &cleanupPackCache{t: t}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, cache, nil, nil)
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider", payloads: map[string][]byte{"pack": workflowZIP(t, map[string]string{"Show.S01E02.srt": installSRT, "Show.S01E03.srt": installSRT})}, filenames: map[string]string{"pack": "season.zip"}}}
	workspace := t.TempDir()
	selected, _, err := service.downloadAndSelect(context.Background(), request, candidate, workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cache.members != 2 {
		t.Fatalf("cache saw %d members", cache.members)
	}
	files := scratchFiles(t, workspace)
	if len(files) != 1 || files[0] != selected {
		t.Fatalf("scratch after selection=%v, selected=%s", files, selected)
	}
}

type cleanupSynchronizer struct {
	fakeSynchronizer
	analyze func(domain.Candidate, string)
	sync    func(domain.Candidate, string, string) error
}

func (s *cleanupSynchronizer) AnalyzeCandidate(ctx context.Context, c domain.Candidate, media, path string) (domain.SyncResult, error) {
	if s.analyze != nil {
		s.analyze(c, path)
	}
	return s.fakeSynchronizer.AnalyzeCandidate(ctx, c, media, path)
}
func (s *cleanupSynchronizer) SynchronizeCandidate(ctx context.Context, c domain.Candidate, media, input, output string) (domain.SyncResult, error) {
	if s.sync != nil {
		if err := s.sync(c, input, output); err != nil {
			return domain.SyncResult{}, err
		}
	}
	return s.fakeSynchronizer.SynchronizeCandidate(ctx, c, media, input, output)
}
func TestScratchTiesSurviveUntilFallbackAndFailedOutputIsReleased(t *testing.T) {
	var first, failedSource, failedOutput string
	sync := &cleanupSynchronizer{fakeSynchronizer: fakeSynchronizer{confidence: map[string]float64{"first": 0.8, "second": 0.9}}}
	sync.analyze = func(c domain.Candidate, path string) {
		if c.ResultID == "first" {
			first = path
		} else {
			if _, err := os.ReadFile(first); err != nil {
				t.Fatalf("viable tie removed: %v", err)
			}
		}
	}
	sync.sync = func(c domain.Candidate, input, output string) error {
		if c.ResultID == "second" {
			failedSource = input
			failedOutput = output
			if err := os.WriteFile(output, []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			return errors.New("sync failed")
		}
		for _, path := range []string{failedSource, failedOutput} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("failed candidate scratch survives: %s: %v", path, err)
			}
		}
		return nil
	}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("first"), broadCandidate("second")}}}, nil, sync, nil)
	service.LapsePolicy.Mode = "always"
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Candidate.ResultID != "first" {
		t.Fatalf("Run=%+v,%v", result, err)
	}
}

type cleanupInstaller struct{ fakeInstaller }

func (i *cleanupInstaller) Install(ctx context.Context, request InstallRequest) (store.Installation, error) {
	if request.Candidate.ResultID == "first" {
		return store.Installation{}, &subtitleValidationError{reason: "invalid subtitle"}
	}
	return i.fakeInstaller.Install(ctx, request)
}

func TestDiscardedSelectedScratchReleasedBeforeNextDownload(t *testing.T) {
	for _, phase := range []string{"exact_install", "broad_analysis"} {
		t.Run(phase, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			first, second := broadCandidate("first"), broadCandidate("second")
			var installer CandidateInstaller = &fakeInstaller{}
			synchronizer := &fakeSynchronizer{rejectText: "first"}
			if phase == "exact_install" {
				first.ExactHash = true
				second.ExactHash = true
				installer = &cleanupInstaller{}
			}
			adapter := &cleanupProvider{fakeProvider: fakeProvider{id: "provider"}, before: func(c domain.Candidate) {
				if c.ResultID != "second" {
					return
				}
				files := scratchFiles(t, temp)
				if len(files) != 1 {
					t.Errorf("discarded candidate artifacts remain: %v", files)
				}
				for _, path := range files {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Size() != 0 {
						t.Errorf("discarded source remains: %s", path)
					}
				}
			}}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{first, second}}}, nil, synchronizer, installer)
			service.LapsePolicy.Mode = "always"
			service.Providers = map[string]provider.Provider{"provider": adapter}
			result, err := service.Run(context.Background(), serviceRequest(t))
			if err != nil || result.Candidate.ResultID != "second" {
				t.Fatalf("Run=%+v,%v", result, err)
			}
		})
	}
}

func TestScratchCleanupLeavesCachedSourcesUntouched(t *testing.T) {
	workspace := t.TempDir()
	cache := t.TempDir()
	path := writeInstallFile(t, filepath.Join(cache, "selected-0.srt"), installSRT)
	if err := removeWorkflowArtifact(workspace, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("cache source changed: %v", err)
	}
}
