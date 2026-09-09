package workflow

import (
	"context"
	"errors"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/gestdown"
	"testing"
	"time"
)

func TestMovieProviderFailuresExcludeTVOnlyRoutes(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name    string
		failure error
		outcome Outcome
		wantErr bool
	}{
		{"cooldown", &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationSearch, ResetAt: reset}, OutcomeThrottled, false},
		{"technical", errors.New("provider failed"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{provider.SearchBroad: {Errors: map[string]error{"provider": tc.failure}}}}
			s := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
			s.ProviderOrder = []string{"gestdown", "provider"}
			s.Providers["gestdown"] = &gestdown.Client{}
			result, err := s.Run(context.Background(), serviceRequest(t))
			if (err != nil) != tc.wantErr || result.Outcome != tc.outcome {
				t.Fatalf("unsupported route masked failure: outcome=%s err=%v", result.Outcome, err)
			}
			if !tc.wantErr && !result.RetryAt.Equal(reset) {
				t.Fatal("lost provider reset")
			}
		})
	}
}
