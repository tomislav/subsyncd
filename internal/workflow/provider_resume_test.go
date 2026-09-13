package workflow

import (
	"errors"
	"slices"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

func TestProviderRouteSignature(t *testing.T) {
	service := &Service{
		ProviderOrder:         []string{"open", "titlovi"},
		FallbackProviderOrder: []string{"subdl"},
	}
	got := service.RouteSignature("en")
	const wantSignature = "4edc91e8f52b59fbb7cbb21d478de6085a5ff29adeb631cdec98e3b3e1f8496a"
	if got != wantSignature {
		t.Fatalf("route signature = %q, want %q", got, wantSignature)
	}
	identical := &Service{
		ProviderOrder:         []string{"open", "titlovi"},
		FallbackProviderOrder: []string{"subdl"},
	}
	if want := identical.RouteSignature("en"); got != want {
		t.Fatalf("identical route signature = %q, want %q", want, got)
	}

	changes := []struct {
		name     string
		language string
		service  *Service
	}{
		{"language", "hr", identical},
		{"preferred membership", "en", &Service{ProviderOrder: []string{"open"}, FallbackProviderOrder: []string{"subdl"}}},
		{"preferred order", "en", &Service{ProviderOrder: []string{"titlovi", "open"}, FallbackProviderOrder: []string{"subdl"}}},
		{"tier", "en", &Service{ProviderOrder: []string{"open"}, FallbackProviderOrder: []string{"titlovi", "subdl"}}},
		{"fallback membership", "en", &Service{ProviderOrder: []string{"open", "titlovi"}, FallbackProviderOrder: []string{"other", "subdl"}}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			if changed := change.service.RouteSignature(domain.Language(change.language)); changed == got {
				t.Fatalf("changed route retained signature %q", changed)
			}
		})
	}
	service.FallbackProviderOrder = []string{"subdl", "other"}
	before := service.RouteSignature("en")
	service.FallbackProviderOrder = []string{"other", "subdl"}
	if after := service.RouteSignature("en"); after == before {
		t.Fatal("same-membership fallback reorder retained the route signature")
	}
}

func TestServiceRetainsProviderResumeAcrossInventoryFailure(t *testing.T) {
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.ProviderOrder = []string{"open", "titlovi"}
	sentinel := errors.New("inventory failed")
	service.Inventory.(*fakeInventory).err = sentinel
	request := serviceRequest(t)
	request.ResumeProviders = []string{"titlovi"}
	request.ResumeRouteSignature = service.ResumeSignature(request.Language, request.Media.Fingerprint)

	result, err := service.Run(t.Context(), request)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v", err, sentinel)
	}
	if !slices.Equal(result.ResumeProviders, []string{"titlovi"}) || result.ResumeRouteSignature != request.ResumeRouteSignature {
		t.Fatalf("resume = %v/%q", result.ResumeProviders, result.ResumeRouteSignature)
	}
}

func TestServiceInvalidatesResumeForEveryFingerprintField(t *testing.T) {
	service := &Service{ProviderOrder: []string{"p"}}
	original := domain.MediaFingerprint{Path: "/media/private/movie.mkv", FileID: 17, Size: 123, ModTime: time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)}
	for _, test := range []struct {
		name   string
		change func(*domain.MediaFingerprint)
	}{
		{"path", func(f *domain.MediaFingerprint) { f.Path = "/media/private/renamed.mkv" }},
		{"file ID", func(f *domain.MediaFingerprint) { f.FileID++ }},
		{"size", func(f *domain.MediaFingerprint) { f.Size++ }},
		{"mtime", func(f *domain.MediaFingerprint) { f.ModTime = f.ModTime.Add(time.Nanosecond) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Language: "en", Media: domain.Media{Fingerprint: original}, ResumeProviders: []string{"p"}, ResumeRouteSignature: service.ResumeSignature("en", original)}
			test.change(&request.Media.Fingerprint)
			validated, err := service.validatedResumeRequest(request)
			if err != nil || len(validated.ResumeProviders) != 0 || validated.ResumeRouteSignature != "" {
				t.Fatalf("changed fingerprint retained progress: %+v/%v", validated, err)
			}
		})
	}
}

func TestServiceValidatesProviderResume(t *testing.T) {
	t.Run("mismatched signature ignores exclusions", func(t *testing.T) {
		searcher := &fakeSearcher{}
		service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
		service.ProviderOrder = []string{"open"}
		request := serviceRequest(t)
		request.ResumeProviders = []string{"unknown"}
		request.ResumeRouteSignature = "stale-route"

		result, err := service.Run(t.Context(), request)
		if err != nil || result.Outcome != OutcomeNoResult {
			t.Fatalf("Run() = %+v, %v", result, err)
		}
		for _, query := range searcher.queries {
			if len(query.SkipProviders) != 0 {
				t.Fatalf("stale exclusions sent to provider: %v", query.SkipProviders)
			}
		}
	})

	for _, test := range []struct {
		name      string
		providers []string
	}{
		{"unknown", []string{"unknown"}},
		{"duplicate", []string{"open", "open"}},
		{"empty", []string{""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			searcher := &fakeSearcher{}
			service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
			service.ProviderOrder = []string{"open"}
			request := serviceRequest(t)
			request.ResumeProviders = slices.Clone(test.providers)
			request.ResumeRouteSignature = service.ResumeSignature(request.Language, request.Media.Fingerprint)

			if _, err := service.Run(t.Context(), request); err == nil {
				t.Fatal("Run() error = nil")
			}
			if calls := service.Inventory.(*fakeInventory).calls; calls != 0 {
				t.Fatalf("inventory calls = %d, want 0", calls)
			}
			if searcher.calls != 0 {
				t.Fatalf("provider calls = %d, want 0", searcher.calls)
			}
		})
	}
}

func TestServiceResumesOnlyUnfinishedProviders(t *testing.T) {
	for _, test := range []struct {
		name        string
		secondBroad provider.SearchResult
		wantOutcome Outcome
		wantResume  []string
		wantRetry   time.Time
	}{
		{
			name: "unfinished provider remains throttled",
			secondBroad: provider.SearchResult{
				ApplicableProviders: []string{"open"},
				Errors: map[string]error{"open": &provider.CooldownError{
					ProviderID: "open", Scope: provider.OperationSearch,
					ResetAt: time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC),
				}},
			},
			wantOutcome: OutcomeThrottled,
			wantResume:  []string{"titlovi", "subdl"},
			wantRetry:   time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC),
		},
		{
			name: "unfinished provider completes cleanly",
			secondBroad: provider.SearchResult{
				ApplicableProviders: []string{"open"},
				EmptyProviders:      []string{"open"},
				Errors:              map[string]error{},
			},
			wantOutcome: OutcomeNoResult,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstReset := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
			searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
				provider.SearchExactHash: {
					ApplicableProviders: []string{"open"},
					Errors: map[string]error{"open": &provider.CooldownError{
						ProviderID: "open", Scope: provider.OperationSearch, ResetAt: firstReset,
					}},
				},
				provider.SearchBroad: {
					ApplicableProviders: []string{"open", "titlovi", "subdl"},
					EmptyProviders:      []string{"titlovi", "subdl"},
					Errors: map[string]error{"open": &provider.CooldownError{
						ProviderID: "open", Scope: provider.OperationSearch, ResetAt: firstReset,
					}},
				},
			}}
			service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
			service.ProviderOrder = []string{"open", "titlovi", "subdl"}
			request := serviceRequest(t)

			first, err := service.Run(t.Context(), request)
			if err != nil || first.Outcome != OutcomeThrottled || !first.RetryAt.Equal(firstReset) {
				t.Fatalf("first Run() = %+v, %v", first, err)
			}
			if !slices.Equal(first.ResumeProviders, []string{"titlovi", "subdl"}) || first.ResumeRouteSignature != service.ResumeSignature(request.Language, request.Media.Fingerprint) {
				t.Fatalf("first resume = %v/%q", first.ResumeProviders, first.ResumeRouteSignature)
			}

			searcher.results[provider.SearchExactHash] = provider.SearchResult{
				ApplicableProviders: []string{"open"}, EmptyProviders: []string{"open"}, Errors: map[string]error{},
			}
			searcher.results[provider.SearchBroad] = test.secondBroad
			request.ResumeProviders = slices.Clone(first.ResumeProviders)
			request.ResumeRouteSignature = first.ResumeRouteSignature
			second, err := service.Run(t.Context(), request)
			if err != nil || second.Outcome != test.wantOutcome || !second.RetryAt.Equal(test.wantRetry) {
				t.Fatalf("second Run() = %+v, %v", second, err)
			}
			if !slices.Equal(second.ResumeProviders, test.wantResume) {
				t.Fatalf("second resume providers = %v, want %v", second.ResumeProviders, test.wantResume)
			}
			if test.wantResume == nil && second.ResumeRouteSignature != "" {
				t.Fatalf("cleared resume signature = %q", second.ResumeRouteSignature)
			}
			for _, query := range searcher.queries[2:] {
				if !slices.Equal(query.SkipProviders, []string{"titlovi", "subdl"}) {
					t.Fatalf("resume exclusions = %v", query.SkipProviders)
				}
			}
		})
	}
}
