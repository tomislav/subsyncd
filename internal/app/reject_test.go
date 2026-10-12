package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
)

// rejectProvider offers an exact-hash subtitle ("best") and a lower-scoring
// release match ("lower"), each with its own text.
type rejectProvider struct {
	mu      sync.Mutex
	results []string
}

func (*rejectProvider) ID() string { return "english" }
func (*rejectProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ExactFileHash: true}
}
func (*rejectProvider) SupportsLanguage(domain.Language) bool { return true }
func (p *rejectProvider) Search(_ context.Context, query provider.SearchQuery) ([]domain.Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var candidates []domain.Candidate
	for _, id := range p.results {
		candidates = append(candidates, domain.Candidate{ProviderID: "english", ResultID: id, Language: query.Language, Kind: domain.MediaMovie, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}, ExactHash: id == "best", ReleaseNames: []string{"Movie." + id}})
	}
	return candidates, nil
}
func (*rejectProvider) Download(_ context.Context, candidate domain.Candidate, out io.Writer) (provider.DownloadMetadata, error) {
	_, err := io.WriteString(out, subtitleText(candidate.ResultID))
	return provider.DownloadMetadata{Filename: candidate.ResultID + ".srt"}, err
}

func subtitleText(id string) string {
	return "1\n00:00:01,000 --> 00:00:02,000\nSubtitle " + id + "\n"
}

type rejectHarness struct {
	app      *App
	cfg      config.Config
	provider *rejectProvider
	media    domain.Media
	mediaID  int64
	sidecar  string
	now      time.Time
	searches *int
}

// newRejectHarness indexes a movie and installs the exact-hash subtitle for
// each language, through the real workflow.
func newRejectHarness(t *testing.T, mutate func(*config.Config), languages ...domain.Language) *rejectHarness {
	t.Helper()
	cfg := testConfig(t)
	cfg.Sync.Policy = "never"
	for _, language := range languages {
		cfg.Languages[language] = config.LanguageConfig{Providers: []string{"english"}}
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mediaPath := filepath.Join(cfg.MediaRoots[0], "Movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}}
	h := &rejectHarness{cfg: cfg, provider: &rejectProvider{results: []string{"best", "lower"}}, media: media, sidecar: filepath.Join(cfg.MediaRoots[0], "Movie.en.srt"), now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC), searches: new(int)}
	h.open(t)
	ctx := t.Context()
	for _, language := range append([]domain.Language{"en"}, languages...) {
		searched, err := h.app.Search(ctx, searchArgs("tv", "movie", 7, language.String(), false))
		if err != nil || searched.Languages[0].Outcome != "installed" || searched.Languages[0].Candidate != "best" {
			t.Fatalf("initial search = %+v/%v", searched, err)
		}
		h.mediaID = searched.MediaID
	}
	return h
}

// open assembles the application, as a process start does.
func (h *rejectHarness) open(t *testing.T) {
	t.Helper()
	application, err := New(t.Context(), h.cfg, Options{Clock: testutil.NewClock(h.now), LapseRunner: capabilityRunner{}, ProbeRunner: notificationProbe{}, Providers: map[string]provider.Provider{"english": h.provider}, Catalogs: map[string]catalog.Catalog{"tv": staticCatalog{media: h.media}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	searches := h.searches
	application.runWorkflow = func(ctx context.Context, request workflow.Request) (workflow.Result, error) {
		*searches++
		return workflowRouter(application.Workflows).Run(ctx, request)
	}
	h.app = application
}

func rejectArgs(language, reason string) control.RejectArgs {
	return control.RejectArgs{Selection: control.Selection{Instance: "tv", Kind: "movie", FileID: 7}, Language: language, Reason: reason}
}

func (h *rejectHarness) sidecarText(t *testing.T) string {
	t.Helper()
	payload, err := os.ReadFile(h.sidecar)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var coded *control.Error
	if !errors.As(err, &coded) || coded.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestRejectRecordsUserRejectionAndReplaces(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	result, err := h.app.Reject(ctx, rejectArgs("en", "out_of_sync"))
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rejected
	if got.Outcome != "installed" || got.Candidate != "lower" || got.Subtitle != "Movie.en.srt" || result.Undone != 0 {
		t.Fatalf("Reject() = %+v", result)
	}
	if text := h.sidecarText(t); text != subtitleText("lower") {
		t.Fatalf("sidecar = %q", text)
	}
	rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now)
	if err != nil || len(rejections) != 1 || rejections[0].ResultID != "best" || rejections[0].ReasonCode != "user_out_of_sync" || rejections[0].CandidateSignature == "" || rejections[0].CandidateSignature == store.AnyCandidateSignature {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
	status, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en")
	if err != nil || status.ReplaceInstalled || status.State != "pending" || status.Priority != store.SearchPriorityUpgrade {
		t.Fatalf("status after the replacement = %+v/%v", status, err)
	}
	// The exact-hash subtitle scores higher, so an ordinary search would
	// upgrade to it; the owner's rejection keeps it out.
	again, err := h.app.Search(ctx, searchArgs("tv", "movie", 7, "en", false))
	if err != nil || again.Languages[0].Outcome != "satisfied" || h.sidecarText(t) != subtitleText("lower") {
		t.Fatalf("search after reject = %+v/%v, sidecar %q", again, err, h.sidecarText(t))
	}
	explained, err := h.app.Explain(ctx, explainArgs("tv", "movie", 7, "en"))
	if err != nil || len(explained.Rejections) != 1 || explained.Rejections[0].Reason != "user_out_of_sync" {
		t.Fatalf("explain rejections = %+v/%v", explained.Rejections, err)
	}
}

func TestRetryRejectedKeepsUserRejections(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	if _, err := h.app.Reject(ctx, rejectArgs("en", "wrong_release")); err != nil {
		t.Fatal(err)
	}
	again, err := h.app.Search(ctx, searchArgs("tv", "movie", 7, "en", true))
	if err != nil || again.Languages[0].Outcome != "satisfied" || h.sidecarText(t) != subtitleText("lower") {
		t.Fatalf("retry-rejected search = %+v/%v, sidecar %q", again, err, h.sidecarText(t))
	}
	rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now)
	if err != nil || len(rejections) != 1 || rejections[0].ReasonCode != "user_wrong_release" {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
}

func TestRejectRemovesWhenNoReplacement(t *testing.T) {
	h := newRejectHarness(t, func(cfg *config.Config) {
		cfg.Silo = config.SiloConfig{Enabled: true, URL: "http://silo.invalid", APIKey: "test-key"}
	})
	ctx := t.Context()
	if _, err := h.app.Reject(ctx, rejectArgs("en", "out_of_sync")); err != nil {
		t.Fatal(err)
	}
	installation, _, err := h.app.Repository.GetInstallation(ctx, h.mediaID, "en")
	if err != nil || installation.RollbackPath == "" {
		t.Fatalf("replacement has no rollback copy: %+v/%v", installation, err)
	}
	// The install notifications were delivered.
	inspection, err := sql.Open("sqlite", filepath.Join(h.cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	if _, err := inspection.Exec(`DELETE FROM notifications`); err != nil {
		t.Fatal(err)
	}
	// Now the replacement is wrong too, and nothing else is acceptable.
	result, err := h.app.Reject(ctx, rejectArgs("en", "poor_quality"))
	if err != nil || result.Rejected.Outcome != "removed" || result.Rejected.Reason != "rejected" {
		t.Fatalf("Reject() = %+v/%v", result, err)
	}
	if _, err := os.Lstat(h.sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected subtitle remains: %v", err)
	}
	if _, err := os.Lstat(installation.RollbackPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback copy remains: %v", err)
	}
	if _, found, err := h.app.Repository.GetInstallation(ctx, h.mediaID, "en"); err != nil || found {
		t.Fatalf("installation found/error = %v/%v", found, err)
	}
	leases, err := h.app.Repository.LeaseDueNotifications(ctx, h.now.Add(2*time.Hour), 10, time.Minute)
	if err != nil || len(leases) != 1 || leases[0].Notifier != "silo" {
		t.Fatalf("removal notifications = %+v/%v", leases, err)
	}
	found, err := h.app.Find(ctx, control.Selection{Instance: "tv", Kind: "movie", FileID: 7})
	if err != nil || len(found.Matches) != 1 || found.Matches[0].Languages[0].State != "missing" {
		t.Fatalf("find = %+v/%v", found, err)
	}
	status, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en")
	if err != nil || status.ReplaceInstalled || status.State != "pending" || status.Priority != store.SearchPriorityMissing || !status.NextAttemptAt.After(h.now) {
		t.Fatalf("status after the removal = %+v/%v", status, err)
	}
	entries, err := h.app.Repository.ListHistory(ctx, store.HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.Outcome == "removed" {
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("history = %+v, want one removed row", entries)
	}
}

func TestRejectRefusesEditedManagedSubtitle(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	edited := "1\n00:00:01,000 --> 00:00:02,000\nMy own fix\n"
	if err := os.WriteFile(h.sidecar, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en")
	if err != nil {
		t.Fatal(err)
	}
	searches := *h.searches
	_, err = h.app.Reject(ctx, rejectArgs("en", "out_of_sync"))
	requireCode(t, err, control.CodeNotManaged)
	if h.sidecarText(t) != edited || *h.searches != searches {
		t.Fatal("the edited subtitle was touched or searched")
	}
	if rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now); err != nil || len(rejections) != 0 {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
	if after, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en"); err != nil || after != before {
		t.Fatalf("status = %+v/%v, want %+v", after, err, before)
	}
	if _, found, err := h.app.Repository.GetInstallation(ctx, h.mediaID, "en"); err != nil || !found {
		t.Fatalf("installation found/error = %v/%v", found, err)
	}
}

func TestRejectRefusesUserSidecarAndEmbedded(t *testing.T) {
	h := newRejectHarness(t, nil, "hr")
	ctx := t.Context()
	// Croatian: the owner's own sidecar, never installed by subsyncd.
	if _, err := h.app.Repository.RemoveInstallation(ctx, store.InstallationRemoval{Installation: mustInstallation(t, h, "hr"), At: h.now}); err != nil {
		t.Fatal(err)
	}
	_, err := h.app.Reject(ctx, rejectArgs("hr", "wrong_language"))
	requireCode(t, err, control.CodeNotManaged)
	// A file with only an embedded track has no managed installation.
	if _, err := h.app.Repository.RemoveInstallation(ctx, store.InstallationRemoval{Installation: mustInstallation(t, h, "en"), At: h.now}); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Repository.ReplaceTrackInventory(ctx, h.mediaID, h.media.Fingerprint, h.media.Fingerprint, []store.TrackRecord{{Language: "en", Embedded: true}}); err != nil {
		t.Fatal(err)
	}
	_, err = h.app.Reject(ctx, rejectArgs("en", "poor_quality"))
	requireCode(t, err, control.CodeNotManaged)
	_, err = h.app.Reject(ctx, rejectArgs("", "poor_quality"))
	requireCode(t, err, control.CodeNotManaged)
	if rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now); err != nil || len(rejections) != 0 {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
}

func mustInstallation(t *testing.T, h *rejectHarness, language domain.Language) store.Installation {
	t.Helper()
	installation, found, err := h.app.Repository.GetInstallation(t.Context(), h.mediaID, language)
	if err != nil || !found {
		t.Fatalf("installation %s = %v/%v", language, found, err)
	}
	return installation
}

func TestRejectNeedsLanguageWhenSeveralManaged(t *testing.T) {
	h := newRejectHarness(t, nil, "hr")
	ctx := t.Context()
	_, err := h.app.Reject(ctx, rejectArgs("", "out_of_sync"))
	requireCode(t, err, control.CodeUsage)
	// With one managed language left, it is the default.
	if _, err := h.app.Repository.RemoveInstallation(ctx, store.InstallationRemoval{Installation: mustInstallation(t, h, "hr"), At: h.now}); err != nil {
		t.Fatal(err)
	}
	result, err := h.app.Reject(ctx, rejectArgs("", "out_of_sync"))
	if err != nil || result.Rejected.Language != "en" || result.Rejected.Outcome != "installed" {
		t.Fatalf("Reject() = %+v/%v", result, err)
	}
}

func TestRejectInvalidReason(t *testing.T) {
	h := newRejectHarness(t, nil)
	for _, args := range []control.RejectArgs{rejectArgs("en", ""), rejectArgs("en", "boring"), rejectArgs("en", "user_out_of_sync"), {Selection: rejectArgs("en", "").Selection, Language: "en", Reason: "other", Undo: true}} {
		_, err := h.app.Reject(t.Context(), args)
		requireCode(t, err, control.CodeUsage)
	}
	for _, args := range []control.RejectArgs{{Selection: rejectArgs("en", "").Selection, Language: "en", Reason: "other", Wait: time.Minute}, {Selection: rejectArgs("en", "").Selection, Language: "en", Reason: "other", NoWait: true}} {
		_, err := h.app.Reject(t.Context(), args)
		requireCode(t, err, control.CodeDaemonRequired)
	}
}

func TestRejectUndoClearsOnlyUserRejections(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	if _, err := h.app.Reject(ctx, rejectArgs("en", "out_of_sync")); err != nil {
		t.Fatal(err)
	}
	system := store.CandidateRejection{MediaID: h.mediaID, Language: "en", ProviderID: "english", ResultID: "system", CandidateSignature: "signature", ReasonCode: "lapse_unsure", ToolSignature: h.app.Workflows["en"].RejectionToolSignature(),
		MediaPath: h.media.Fingerprint.Path, MediaFileID: 7, MediaSize: h.media.Fingerprint.Size, MediaModTimeNS: h.media.Fingerprint.ModTime.UnixNano(), RejectedAt: h.now}
	if err := h.app.Repository.PutCandidateRejection(ctx, system); err != nil {
		t.Fatal(err)
	}
	searches := *h.searches
	result, err := h.app.Reject(ctx, control.RejectArgs{Selection: rejectArgs("en", "").Selection, Language: "en", Undo: true})
	if err != nil || result.Undone != 1 || result.Rejected.Outcome != "" || result.Rejected.Language != "en" {
		t.Fatalf("undo = %+v/%v", result, err)
	}
	if *h.searches != searches || h.sidecarText(t) != subtitleText("lower") {
		t.Fatal("undo searched or changed the subtitle")
	}
	rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now)
	if err != nil || len(rejections) != 1 || rejections[0].ResultID != "system" {
		t.Fatalf("rejections after undo = %+v/%v", rejections, err)
	}
	if text := result.Text(); !strings.Contains(text, "undone: 1") {
		t.Fatalf("text = %q", text)
	}
}

func TestRejectSurvivesDaemonRestart(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	target, err := h.app.prepareReject(ctx, rejectArgs("en", "out_of_sync"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.recordReject(ctx, target); err != nil {
		t.Fatal(err)
	}
	// The process stops here, before the search. The manual run's lease
	// expires, and the next start's worker takes the search.
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(manualLeaseDuration + time.Second)
	h.open(t)
	background, ok := h.app.Worker.(*worker.Worker)
	if !ok {
		t.Fatalf("worker = %T", h.app.Worker)
	}
	if err := background.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if text := h.sidecarText(t); text != subtitleText("lower") {
		t.Fatalf("sidecar after the restarted worker = %q", text)
	}
	status, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en")
	if err != nil || status.ReplaceInstalled {
		t.Fatalf("status = %+v/%v", status, err)
	}
}

func TestRejectThroughDaemon(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.app.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	client := dialDaemon(t, h.app)
	var result control.RejectResult
	args := control.RejectArgs{Selection: control.Selection{Title: "Movie (2024)"}, Reason: "out_of_sync"}
	if err := client.Call(context.Background(), "reject", args, &result); err != nil {
		t.Fatal(err)
	}
	if result.Rejected.Outcome != "installed" || result.Rejected.Candidate != "lower" {
		t.Fatalf("daemon reject = %+v", result)
	}
}

func (h *rejectHarness) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	inspection, err := sql.Open("sqlite", filepath.Join(h.cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	if _, err := inspection.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func TestRejectUndoDisarmsTheQueuedReplacement(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	// The rejection and its replacement are queued, but the search has not
	// finished (a throttle, or a stop) when the owner takes it back.
	target, err := h.app.prepareReject(ctx, rejectArgs("en", "out_of_sync"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.app.recordReject(ctx, target); err != nil {
		t.Fatal(err)
	}
	h.app.releaseManualLease(ctx, target.jobID)
	if result, err := h.app.Reject(ctx, control.RejectArgs{Selection: rejectArgs("en", "").Selection, Language: "en", Undo: true}); err != nil || result.Undone != 1 {
		t.Fatalf("undo = %+v/%v", result, err)
	}
	background := h.app.Worker.(*worker.Worker)
	if err := background.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if text := h.sidecarText(t); text != subtitleText("best") {
		t.Fatalf("the un-rejected subtitle was replaced: %q", text)
	}
	if status, err := h.app.Repository.GetSearchStatus(ctx, h.mediaID, "en"); err != nil || status.ReplaceInstalled {
		t.Fatalf("status = %+v/%v", status, err)
	}
}

func TestRejectOfAPre023InstallationSkipsThatProviderResult(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	// An installation recorded before candidate signatures existed.
	h.exec(t, `UPDATE installations SET candidate_signature='', artifact_checksum='' WHERE media_id=?`, h.mediaID)
	result, err := h.app.Reject(ctx, rejectArgs("en", "wrong_release"))
	if err != nil || result.Rejected.Outcome != "installed" || result.Rejected.Candidate != "lower" {
		t.Fatalf("Reject() = %+v/%v", result, err)
	}
	rejections, err := h.app.Repository.ListCandidateRejections(ctx, h.mediaID, "en", h.now)
	if err != nil || len(rejections) != 1 || rejections[0].CandidateSignature != store.AnyCandidateSignature || rejections[0].ResultID != "best" {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
	// The exact-hash result would win an ordinary search; it stays skipped,
	// while the provider's other result remains installable.
	again, err := h.app.Search(ctx, searchArgs("tv", "movie", 7, "en", true))
	if err != nil || again.Languages[0].Outcome != "satisfied" || h.sidecarText(t) != subtitleText("lower") {
		t.Fatalf("search = %+v/%v, sidecar %q", again, err, h.sidecarText(t))
	}
	// Removing the installed subtitle by hand: the search reinstalls the
	// other result, never the rejected one.
	if err := os.Remove(h.sidecar); err != nil {
		t.Fatal(err)
	}
	again, err = h.app.Search(ctx, searchArgs("tv", "movie", 7, "en", false))
	if err != nil || again.Languages[0].Outcome != "installed" || again.Languages[0].Candidate != "lower" {
		t.Fatalf("reacquisition = %+v/%v", again, err)
	}
}

func TestRejectRemovesWhenAnotherTrackCoversTheLanguage(t *testing.T) {
	h := newRejectHarness(t, nil)
	ctx := t.Context()
	// The owner added their own English subtitle beside it.
	own := filepath.Join(h.cfg.MediaRoots[0], "Movie.eng.srt")
	if err := os.WriteFile(own, []byte(subtitleText("mine")), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := h.app.Reject(ctx, rejectArgs("en", "out_of_sync"))
	if err != nil || result.Rejected.Outcome != "removed" || result.Rejected.Reason != "satisfied" {
		t.Fatalf("Reject() = %+v/%v", result, err)
	}
	if _, err := os.Lstat(h.sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected subtitle remains: %v", err)
	}
	if payload, err := os.ReadFile(own); err != nil || string(payload) != subtitleText("mine") {
		t.Fatalf("the owner's subtitle = %q/%v", payload, err)
	}
}
