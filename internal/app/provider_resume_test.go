package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/worker"
)

type resumeTestProvider struct {
	fakeProvider
	clock *testutil.Clock
	reset time.Time
	err   error
	calls atomic.Int64
}

func (p *resumeTestProvider) Search(context.Context, provider.SearchQuery) ([]domain.Candidate, error) {
	p.calls.Add(1)
	if p.clock.Now().Before(p.reset) {
		return nil, &provider.CooldownError{ProviderID: p.id, Scope: provider.OperationSearch, ResetAt: p.reset}
	}
	return nil, p.err
}

type resumeTestApp struct {
	app       *App
	clock     *testutil.Clock
	media     domain.Media
	mediaID   int64
	providers []*resumeTestProvider
}

func newResumeTestApp(t *testing.T, count int, languages ...domain.Language) resumeTestApp {
	t.Helper()
	cfg := testConfig(t)
	clock := testutil.NewClock(time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC))
	mediaPath := filepath.Join(cfg.MediaRoots[0], "Movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	media := domain.Media{EntityID: 1, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 1}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 1, Size: info.Size(), ModTime: info.ModTime()}, Title: "Movie", Year: 2026}
	providerSpec := cfg.Providers["english"]
	cfg.Providers = map[string]config.ProviderSpec{}
	supplied := map[string]provider.Provider{}
	var providers []*resumeTestProvider
	var order []string
	for i := range count {
		id := fmt.Sprintf("p%d", i)
		p := &resumeTestProvider{fakeProvider: fakeProvider{id: id}, clock: clock}
		if i == 0 {
			p.reset = clock.Now().Add(8 * time.Hour)
		}
		cfg.Providers[id], supplied[id] = providerSpec, p
		providers, order = append(providers, p), append(order, id)
	}
	cfg.Languages = map[domain.Language]config.LanguageConfig{}
	for _, language := range languages {
		cfg.Languages[language] = config.LanguageConfig{Providers: slices.Clone(order)}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid test route rejected: %v", err)
	}
	application, err := New(t.Context(), cfg, Options{Clock: clock, SkipLapseCheck: true, SkipProbeCheck: true, ProbeRunner: notificationProbe{}, Providers: supplied, Catalogs: map[string]catalog.Catalog{"tv": staticCatalog{media: media}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	application.Worker.(*worker.Worker).RandomUnit = func() float64 { return 0 }
	application.Worker.(*worker.Worker).SearchBatch = 1
	mediaID, _, err := application.Repository.UpsertMedia(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range languages {
		if err := application.Repository.UpsertSearchStateWithPriority(t.Context(), mediaID, language, clock.Now(), store.SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
	}
	return resumeTestApp{app: application, clock: clock, media: media, mediaID: mediaID, providers: providers}
}

func (h resumeTestApp) run(t *testing.T) {
	t.Helper()
	if err := h.app.Worker.(*worker.Worker).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (h resumeTestApp) resume(t *testing.T, language string) ([]string, string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.app.Config.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var encoded, signature string
	if err := db.QueryRow(`SELECT resume_providers_json,resume_route_signature FROM search_states WHERE media_id=? AND language=?`, h.mediaID, language).Scan(&encoded, &signature); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
		t.Fatal(err)
	}
	return ids, signature
}

func TestProviderResumeAcceptsTenProviderRoute(t *testing.T) {
	h := newResumeTestApp(t, 10, "en")
	h.run(t)
	ids, signature := h.resume(t, "en")
	if !slices.Equal(ids, []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"}) || signature == "" {
		t.Fatalf("resume = %v/%q", ids, signature)
	}
	h.clock.Advance(8 * time.Hour)
	h.run(t)
	for i, p := range h.providers {
		want := int64(1)
		if i == 0 {
			want = 2
		}
		if got := p.calls.Load(); got != want {
			t.Errorf("%s calls = %d, want %d", p.id, got, want)
		}
	}
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.mediaID, "en")
	if err != nil || status.LastOutcome != "no_result" || status.Attempt != 1 || status.FailureAttempt != 0 {
		t.Fatalf("completion = %+v/%v", status, err)
	}
	if ids, signature := h.resume(t, "en"); len(ids) != 0 || signature != "" {
		t.Fatalf("completed resume = %v/%q", ids, signature)
	}
}

func TestProviderResumeInvalidatesLiveReplacementAcrossLanguages(t *testing.T) {
	h := newResumeTestApp(t, 3, "en", "hr")
	h.run(t)
	h.run(t)
	for _, language := range []string{"en", "hr"} {
		if ids, signature := h.resume(t, language); !slices.Equal(ids, []string{"p1", "p2"}) || signature == "" {
			t.Fatalf("initial %s resume = %v/%q", language, ids, signature)
		}
	}
	// No catalog event: inventory is the first component to discover replacement.
	if err := os.WriteFile(h.media.Fingerprint.Path, []byte("replacement media bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(8 * time.Hour) // Beyond the six-hour provider cache lifetime.
	h.run(t)
	for _, p := range h.providers {
		if got := p.calls.Load(); got != 3 {
			t.Errorf("replacement en %s calls = %d, want 3", p.id, got)
		}
	}
	if ids, signature := h.resume(t, "hr"); len(ids) != 0 || signature != "" {
		t.Errorf("other-language stale resume = %v/%q", ids, signature)
	}
	h.run(t)
	for _, p := range h.providers {
		if got := p.calls.Load(); got != 4 {
			t.Errorf("replacement hr %s calls = %d, want 4", p.id, got)
		}
	}
}

func TestProviderResumeProgressesTwoThrottledProviders(t *testing.T) {
	h := newResumeTestApp(t, 3, "en")
	h.providers[1].reset = h.clock.Now().Add(16 * time.Hour)
	for _, step := range []struct {
		advance     time.Duration
		wantIDs     []string
		wantCalls   []int64
		wantOutcome string
	}{
		{0, []string{"p2"}, []int64{1, 1, 1}, "throttled"},
		{8 * time.Hour, []string{"p0", "p2"}, []int64{2, 2, 1}, "throttled"},
		{8 * time.Hour, nil, []int64{2, 3, 1}, "no_result"},
	} {
		h.clock.Advance(step.advance)
		h.run(t)
		if ids, _ := h.resume(t, "en"); !slices.Equal(ids, step.wantIDs) {
			t.Fatalf("resume = %v, want %v", ids, step.wantIDs)
		}
		for i, p := range h.providers {
			if got := p.calls.Load(); got != step.wantCalls[i] {
				t.Fatalf("%s calls = %d, want %d", p.id, got, step.wantCalls[i])
			}
		}
		status, err := h.app.Repository.GetSearchStatus(t.Context(), h.mediaID, "en")
		if err != nil || status.LastOutcome != step.wantOutcome || status.FailureAttempt != 0 {
			t.Fatalf("status = %+v/%v", status, err)
		}
		if step.wantOutcome == "throttled" && (status.Attempt != 0 || !status.NextAttemptAt.Equal(h.providers[0].reset) && !status.NextAttemptAt.Equal(h.providers[1].reset)) {
			t.Fatalf("throttle schedule = %+v", status)
		}
	}
}

func TestProviderResumeRemainingTechnicalFailureKeepsProgress(t *testing.T) {
	h := newResumeTestApp(t, 3, "en")
	h.run(t)
	h.clock.Advance(8 * time.Hour)
	h.providers[0].err = errors.New("local fake provider failure")
	if err := h.app.Worker.(*worker.Worker).RunOnce(t.Context()); err == nil {
		t.Fatal("remaining provider technical failure returned success")
	}
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.mediaID, "en")
	if err != nil || status.FailureAttempt != 1 || status.Attempt != 0 || !status.NextAttemptAt.After(h.clock.Now()) {
		t.Fatalf("technical completion = %+v/%v", status, err)
	}
	if ids, _ := h.resume(t, "en"); !slices.Equal(ids, []string{"p1", "p2"}) {
		t.Fatalf("technical retry lost progress: %v", ids)
	}
	h.providers[0].err = nil
	h.clock.Set(status.NextAttemptAt)
	h.run(t)
	for i, p := range h.providers {
		want := int64(1)
		if i == 0 {
			want = 3
		}
		if got := p.calls.Load(); got != want {
			t.Fatalf("%s calls = %d, want %d", p.id, got, want)
		}
	}
	if ids, signature := h.resume(t, "en"); len(ids) != 0 || signature != "" {
		t.Fatalf("recovered cycle retained progress: %v/%q", ids, signature)
	}
}
