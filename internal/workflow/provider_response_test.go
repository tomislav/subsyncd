package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func TestProviderMessagesAreNotSubtitleRejections(t *testing.T) {
	adapter := &fakeProvider{id: "provider",
		payloads: map[string][]byte{
			"html":        []byte("\ufeff  <!DOCTYPE html><html><body>Please log in</body></html>"),
			"typed-html":  []byte("<p>Daily download limit reached</p>"),
			"json":        []byte(`{"status":false,"error":"download limit"}`),
			"placeholder": workflowZIP(t, map[string]string{"titlovi_titl_plugin_message.srt": installSRT}),
		},
		contentTypes: map[string]string{"typed-html": "text/html; charset=utf-8", "json": "application/json"},
	}
	service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
	for _, id := range []string{"html", "typed-html", "json", "placeholder"} {
		_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate(id), t.TempDir(), 0)
		var message *ProviderResponseError
		if !errors.As(err, &message) || rejectionReasonCode(err) != "" {
			t.Errorf("%s: error = %T %v; want a provider error that is not a rejection", id, err, err)
		}
	}
}

func TestSubtitlesThatLookLikeMessagesAreStillRead(t *testing.T) {
	adapter := &fakeProvider{id: "provider",
		payloads: map[string][]byte{
			"microdvd": []byte("{1}{1}25.000\n{25}{50}Hello\n"),
			"italic":   []byte("<i>\n"),
			"pack":     workflowZIP(t, map[string]string{"Movie.srt": installSRT}),
		},
		filenames: map[string]string{"microdvd": "Movie.sub", "italic": "Movie.srt", "pack": "Movie.zip"},
	}
	service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
	for _, id := range []string{"microdvd", "pack"} {
		if _, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate(id), t.TempDir(), 0); err != nil {
			t.Errorf("%s: error = %v", id, err)
		}
	}
	// A subtitle starting with a tag is judged as a subtitle, not a page.
	_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate("italic"), t.TempDir(), 0)
	var message *ProviderResponseError
	if errors.As(err, &message) {
		t.Fatalf("error = %v; a tag in a subtitle is not an HTML page", err)
	}
}

type refusingProvider struct {
	*availabilityProvider
	refusals int
	reset    time.Time
}

func (p *refusingProvider) RecordDownloadRefusal(context.Context) error {
	p.refusals++
	p.unavailable = &provider.CooldownError{ProviderID: p.id, Scope: provider.OperationDownload, Reason: "download_quota", ResetAt: p.reset}
	return p.unavailable
}

func TestLimitPageThrottlesTheProviderInsteadOfFailingCandidates(t *testing.T) {
	limit := []byte("<!DOCTYPE html><html><body><h1>Daily download limit reached</h1></body></html>")
	placeholder := workflowZIP(t, map[string]string{"titlovi_titl_plugin_message.srt": installSRT})
	login := []byte(`<html><body><form><input type="password" name="pass"></form></body></html>`)
	for name, body := range map[string][]byte{"limit page": limit, "placeholder": placeholder, "login page": login} {
		t.Run(name, func(t *testing.T) {
			s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
			candidates := []domain.Candidate{broadCandidate("one"), broadCandidate("two"), broadCandidate("three")}
			payloads := map[string][]byte{}
			for _, candidate := range candidates {
				payloads[candidate.ResultID] = body
			}
			p := &refusingProvider{
				availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", payloads: payloads}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: candidates}},
				reset:                s.Clock.Now().Add(time.Hour),
			}
			useAvailabilityProviders(s, provider.Observe(p, observability.Discard()))
			result, err := s.Run(t.Context(), serviceRequest(t))
			if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(p.reset) || p.downloads != 1 || p.refusals != 1 {
				t.Fatalf("result=%+v err=%v downloads=%d refusals=%d", result, err, p.downloads, p.refusals)
			}
		})
	}
}

func TestOrdinaryErrorPageFailsOnlyTheCandidate(t *testing.T) {
	// A navigation "Login" link does not make an error page a login page.
	adapter := &refusingProvider{availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider",
		payloads: map[string][]byte{
			"html": []byte(`<!DOCTYPE html><html><body><a href="/login">Login</a><h1>500 Internal Server Error</h1></body></html>`),
			"json": []byte(`{"status":false,"error":"subtitle not found"}`),
		},
	}}}
	service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
	for _, id := range []string{"html", "json"} {
		_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate(id), t.TempDir(), 0)
		var message *ProviderResponseError
		if !errors.As(err, &message) || adapter.refusals != 0 {
			t.Errorf("%s: error = %T %v, refusals = %d; want a per-candidate provider error", id, err, err, adapter.refusals)
		}
	}
}
