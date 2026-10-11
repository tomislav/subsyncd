package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/testutil"
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
	jsonLimit := []byte(`{"status":false,"error":"daily_limit"}`)
	// A page refusal needs a second candidate to confirm it; an explicit limit
	// in a JSON body and Titlovi's placeholder pause the provider at once.
	for name, test := range map[string]struct {
		body      []byte
		downloads int
	}{"limit page": {limit, 2}, "placeholder": {placeholder, 1}, "login page": {login, 2}, "JSON limit": {jsonLimit, 1}} {
		t.Run(name, func(t *testing.T) {
			s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
			candidates := []domain.Candidate{broadCandidate("one"), broadCandidate("two"), broadCandidate("three")}
			payloads := map[string][]byte{}
			for _, candidate := range candidates {
				payloads[candidate.ResultID] = test.body
			}
			p := &refusingProvider{
				availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", payloads: payloads}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: candidates}},
				reset:                s.Clock.Now().Add(time.Hour),
			}
			useAvailabilityProviders(s, provider.Observe(p, observability.Discard()))
			result, err := s.Run(t.Context(), serviceRequest(t))
			if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(p.reset) || p.downloads != test.downloads || p.refusals != 1 {
				t.Fatalf("result=%+v err=%v downloads=%d refusals=%d", result, err, p.downloads, p.refusals)
			}
			if rejections := s.Repository.(*workflowRepository).rejections; len(rejections) != 0 {
				t.Fatalf("rejections = %#v; a refusal never rejects a candidate", rejections)
			}
		})
	}
}

func TestOneRefusingCandidateDoesNotPauseTheProvider(t *testing.T) {
	// A deleted subtitle redirecting to a login page fails only itself; the
	// provider is not paused, so its other candidates still download.
	login := []byte(`<!DOCTYPE html><html><body><h1>Please log in to continue</h1><form><input type="password"></form></body></html>`)
	s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
	candidates := []domain.Candidate{broadCandidate("one")}
	p := &refusingProvider{
		availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", payloads: map[string][]byte{"one": login}}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: candidates}},
		reset:                s.Clock.Now().Add(time.Hour),
	}
	useAvailabilityProviders(s, provider.Observe(p, observability.Discard()))
	result, err := s.Run(t.Context(), serviceRequest(t))
	if result.Outcome == OutcomeThrottled || p.refusals != 0 || p.downloads != 1 {
		t.Fatalf("result=%+v err=%v downloads=%d refusals=%d; want one candidate failure", result, err, p.downloads, p.refusals)
	}
	var message *ProviderResponseError
	if !errors.As(err, &message) {
		t.Fatalf("error = %v; want the candidate's provider error", err)
	}
	if rejections := s.Repository.(*workflowRepository).rejections; len(rejections) != 0 {
		t.Fatalf("rejections = %#v; a refusal never rejects a candidate", rejections)
	}
}

func TestRefusalIsConfirmedOnlyByAnotherCandidate(t *testing.T) {
	recorder := &refusingProvider{availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}}, reset: time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)}
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	run := &refusalRun{ledger: &RefusalLedger{}, now: clock.Now}
	refusal := func() *ProviderResponseError { return &ProviderResponseError{Reason: "HTML page", Refusal: true} }
	first := refusal()
	if err := refusedDownload(t.Context(), recorder, broadCandidate("one"), run, first); err != first || recorder.refusals != 0 {
		t.Fatalf("first refusal = %v, refusals = %d; want only a candidate failure", err, recorder.refusals)
	}
	// The same candidate downloaded again confirms nothing.
	if err := refusedDownload(t.Context(), recorder, broadCandidate("one"), run, refusal()); recorder.refusals != 0 {
		t.Fatalf("repeated refusal = %v, refusals = %d; want no pause", err, recorder.refusals)
	}
	var cooldown *provider.CooldownError
	if err := refusedDownload(t.Context(), recorder, broadCandidate("two"), run, refusal()); !errors.As(err, &cooldown) || recorder.refusals != 1 {
		t.Fatalf("confirmed refusal = %v, refusals = %d; want a cooldown", err, recorder.refusals)
	}
	// The first failure now counts as the provider being unavailable too, so
	// the search ends as throttled rather than as failed.
	if !errors.As(error(first), &cooldown) {
		t.Fatalf("first refusal = %v; want it confirmed as a cooldown", first)
	}
	if _, unavailable := unavailableErrors([]error{first}); !unavailable {
		t.Fatal("a confirmed first refusal is still a candidate failure")
	}
}

func TestRefusalsConfirmAcrossRunsWithinTheWindow(t *testing.T) {
	recorder := &refusingProvider{availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}}, reset: time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)}
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	ledger := &RefusalLedger{}
	refusal := func() *ProviderResponseError { return &ProviderResponseError{Reason: "HTML page", Refusal: true} }

	// Refusals more than 30 minutes apart do not confirm each other, and the
	// stale one is pruned.
	if err := refusedDownload(t.Context(), recorder, broadCandidate("one"), &refusalRun{ledger: ledger, now: clock.Now}, refusal()); recorder.refusals != 0 {
		t.Fatalf("first refusal = %v; want a candidate failure", err)
	}
	clock.Advance(31 * time.Minute)
	firstErr := refusal()
	firstRun := &refusalRun{ledger: ledger, now: clock.Now}
	if err := refusedDownload(t.Context(), recorder, broadCandidate("two"), firstRun, firstErr); err != firstErr || recorder.refusals != 0 {
		t.Fatalf("refusal after the window = %v, refusals = %d; want a candidate failure", err, recorder.refusals)
	}
	ledger.mu.Lock()
	witness := ledger.witnesses["provider"]
	ledger.mu.Unlock()
	if witness.resultID != "two" {
		t.Fatalf("witness = %+v; want the stale refusal pruned", witness)
	}

	// Another search's candidate confirms it within the window. The earlier
	// run's failure is left as it was.
	clock.Advance(10 * time.Minute)
	var cooldown *provider.CooldownError
	if err := refusedDownload(t.Context(), recorder, broadCandidate("three"), &refusalRun{ledger: ledger, now: clock.Now}, refusal()); !errors.As(err, &cooldown) || recorder.refusals != 1 {
		t.Fatalf("refusal in another run = %v, refusals = %d; want a cooldown", err, recorder.refusals)
	}
	if firstErr.confirmed != nil {
		t.Fatal("a later run changed an earlier run's failure")
	}
	// Other providers keep their own witnesses.
	other := domain.Candidate{ProviderID: "other", ResultID: "four"}
	if err := refusedDownload(t.Context(), recorder, other, &refusalRun{ledger: ledger, now: clock.Now}, refusal()); recorder.refusals != 1 {
		t.Fatalf("refusal of another provider = %v; want no pause", err)
	}
}

func TestRefusalLedgerIsSafeForConcurrentRuns(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	ledger := &RefusalLedger{}
	var group sync.WaitGroup
	confirmations := make(chan struct{}, 64)
	for i := range 64 {
		group.Add(1)
		go func() {
			defer group.Done()
			run := &refusalRun{ledger: ledger, now: clock.Now}
			if _, confirmed := ledger.witness([]string{"a", "b"}[i%2], refusalWitness{resultID: fmt.Sprint(i), at: run.now(), run: run}); confirmed {
				confirmations <- struct{}{}
			}
		}()
	}
	group.Wait()
	close(confirmations)
	// Each provider's refusals pair up: 32 refusals confirm 16 times.
	if got := len(confirmations); got != 32 {
		t.Fatalf("confirmations = %d; want 32", got)
	}
}

func TestRunsShareTheServiceRefusalLedger(t *testing.T) {
	// One refusing candidate per search: the first search fails only its
	// candidate, and the second search's distinct candidate confirms it.
	login := []byte(`<!DOCTYPE html><html><body><h1>Please log in to continue</h1></body></html>`)
	s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
	s.Refusals = &RefusalLedger{}
	p := &refusingProvider{
		availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", payloads: map[string][]byte{"one": login, "two": login}}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: {broadCandidate("one")}}},
		reset:                s.Clock.Now().Add(time.Hour),
	}
	useAvailabilityProviders(s, provider.Observe(p, observability.Discard()))
	result, _ := s.Run(t.Context(), serviceRequest(t))
	if result.Outcome == OutcomeThrottled || p.refusals != 0 {
		t.Fatalf("first search = %+v, refusals = %d; want one candidate failure", result, p.refusals)
	}
	p.results[provider.SearchBroad] = []domain.Candidate{broadCandidate("two")}
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeThrottled || p.refusals != 1 {
		t.Fatalf("second search = %+v, %v, refusals = %d; want throttled", result, err, p.refusals)
	}
}

// failingRefusalRecorder cannot store a cooldown.
type failingRefusalRecorder struct{ *fakeProvider }

func (failingRefusalRecorder) RecordDownloadRefusal(context.Context) error {
	return errors.New("database is locked")
}

func TestRefusalCooldownStoreFailureEndsTheWorkflow(t *testing.T) {
	limit := &ProviderResponseError{Reason: "JSON body", Refusal: true, Limit: true}
	err := refusedDownload(t.Context(), failingRefusalRecorder{&fakeProvider{id: "provider"}}, broadCandidate("one"), nil, limit)
	var availability *provider.AvailabilityError
	if !errors.As(err, &availability) {
		t.Fatalf("error = %T %v; want a state-store failure", err, err)
	}
}

func TestOrdinaryPagesAreNotRefusals(t *testing.T) {
	siteText := strings.Repeat("<p>Browse the latest subtitles for films and series in every language.</p>", 40)
	for name, test := range map[string]struct{ body, contentType string }{
		"site page with a header login box": {`<!DOCTYPE html><html><body><nav><form action="/login"><input type="text" name="u"><input type="password" name="p"></form></nav><h1>Page not found</h1>` + siteText + `</body></html>`, "text/html"},
		"quotation":                         {`<!DOCTYPE html><html><body><h1>Error</h1><p>Request a quotation</p></body></html>`, ""},
		"quota in a script":                 {`<!doctype html><html><head><script>try{localStorage.setItem('x','y')}catch(e){/*QuotaExceededError quota*/}</script></head><body>Subtitle removed</body></html>`, ""},
		"password selector in a style":      {`<!doctype html><html><head><style>input[type=password]{border:0}</style></head><body>Subtitle removed</body></html>`, ""},
		"maintenance":                       {`<html><body>We are performing maintenance, too many changes at once. Back soon.</body></html>`, ""},
		"fragment served as text/html":      {"<SAMI><BODY><SYNC Start=1000><P>Too many requests, please log in to the ship!</SAMI>", "text/html"},
		"cloudflare 5xx":                    {`<!DOCTYPE html><html><head><title>502: Bad gateway</title></head><body>Please try again in a few minutes.</body></html>`, "text/html"},
		"comment prompt on a site page":     {`<!DOCTYPE html><html><body><h1>Subtitle removed</h1>` + siteText + `<p>Log in to post a comment</p></body></html>`, "text/html"},
		"copyright footer":                  {`<!DOCTYPE html><html><body><h1>Page not found</h1><footer>Unauthorized reproduction prohibited</footer></body></html>`, ""},
		"unshown elements":                  {`<!DOCTYPE html><html><body><noscript>Please log in to continue</noscript><textarea>Daily download limit reached</textarea><template><input type="password"></template><h1>Subtitle removed</h1></body></html>`, ""},
		"JSON field name":                   {`{"error":"not found","remaining_quota":5}`, "application/json"},
		"JSON other field value":            {`{"error":"not found","hint":"check your download limit"}`, "application/json"},
	} {
		path := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
			t.Fatal(err)
		}
		var message *ProviderResponseError
		if err := checkProviderResponse(path, test.contentType); !errors.As(err, &message) || message.Refusal {
			t.Errorf("%s: error = %#v; want a provider error that is not a refusal", name, err)
		}
	}
	for name, test := range map[string]struct {
		body  string
		limit bool
	}{
		"limit page":            {`<!DOCTYPE html><html><body><h1>Daily download   limit reached</h1></body></html>`, false},
		"login form":            {`<!DOCTYPE html><html><body><form><input name="u"><input type='password' name="p"></form></body></html>`, false},
		"JSON limit":            {`{"error":"limit_exceeded"}`, true},
		"JSON quota":            {`{"message":"Download quota used up"}`, true},
		"JSON sign in":          {`{"message":"Please sign in to download"}`, false},
		"limit escaped":         {`<!DOCTYPE html><html><body><p>You&#39;ve reached your daily&nbsp;limit</p></body></html>`, false},
		"JSON nested":           {`{"status":false,"error":{"code":7,"message":"Daily download limit reached"}}`, true},
		"JSON array":            {`[{"detail":"Please log in to download"}]`, false},
		"unauthorized download": {`<!DOCTYPE html><html><body><h1>Unauthorized access</h1></body></html>`, false},
	} {
		path := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
			t.Fatal(err)
		}
		var message *ProviderResponseError
		if err := checkProviderResponse(path, ""); !errors.As(err, &message) || !message.Refusal || message.Limit != test.limit {
			t.Errorf("%s: error = %#v; want a refusal with Limit %t", name, err, test.limit)
		}
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
