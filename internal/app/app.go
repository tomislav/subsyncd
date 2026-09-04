package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/httpapi"
	"subsyncd/internal/inventory"
	"subsyncd/internal/notifier"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/opensubtitles"
	"subsyncd/internal/provider/subdl"
	"subsyncd/internal/provider/titlovi"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
)

type Runner interface {
	Run(context.Context) error
}

type Options struct {
	Logger         *slog.Logger
	Clock          provider.Clock
	HTTPClient     *http.Client
	LapseRunner    syncer.Runner
	ProbeRunner    inventory.CommandRunner
	Providers      map[string]provider.Provider
	Catalogs       map[string]catalog.Catalog
	Worker         Runner
	Listener       net.Listener
	SkipLapseCheck bool
	SkipProbeCheck bool
}

type App struct {
	Config      config.Config
	Store       *store.Store
	Repository  *store.Repository
	Catalogs    map[string]catalog.Catalog
	Providers   map[string]provider.Provider
	Reconcilers map[string]catalog.Reconciler
	Workflows   map[domain.Language]*workflow.Service
	Inventory   inventory.Service
	Lapse       *syncer.Lapse
	LapseRunner syncer.Runner
	ProbeRunner inventory.CommandRunner
	Worker      Runner
	Handler     http.Handler
	Listener    net.Listener
	Logger      *slog.Logger
	Clock       provider.Clock

	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, configPath string, logger *slog.Logger) (*App, error) {
	cfg, err := config.Load(configPath, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return New(ctx, cfg, Options{Logger: logger})
}

func New(ctx context.Context, cfg config.Config, options Options) (_ *App, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}
	if err := validateMediaRoots(cfg.MediaRoots); err != nil {
		return nil, err
	}
	if !options.SkipLapseCheck {
		if err := syncer.CheckCapabilities(ctx, cfg.Sync.LapsePath, options.LapseRunner); err != nil {
			return nil, err
		}
	}
	probeRunner := options.ProbeRunner
	if probeRunner == nil {
		probeRunner = inventory.OSCommandRunner{}
	}
	if !options.SkipProbeCheck {
		if _, _, err := probeRunner.Run(ctx, "ffprobe", "-version"); err != nil {
			return nil, fmt.Errorf("run ffprobe capability check: %w", err)
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	database, err := store.Open(ctx, filepath.Join(cfg.DataDir, "subsyncd.db"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = database.Close()
		}
	}()
	repository := database.Repository()
	clock := options.Clock
	if clock == nil {
		clock = provider.SystemClock{}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	catalogs, err := buildCatalogs(cfg, options.Catalogs)
	if err != nil {
		return nil, err
	}
	for _, instance := range cfg.Instances {
		if err := repository.EnsureInstance(ctx, instance.Name, instance.Type, instance.URL, clock.Now()); err != nil {
			return nil, err
		}
	}

	providers, err := buildProviders(cfg, database, repository, clock, httpClient, options.Providers)
	if err != nil {
		return nil, err
	}
	routes := languageRoutes(cfg)
	if err := provider.ValidateLanguageRoutes(routes, providers); err != nil {
		return nil, err
	}

	packCache, err := pack.NewCache(filepath.Join(cfg.DataDir, "pack-cache"), repository, clock, cfg.PackCache.MaxBytes)
	if err != nil {
		return nil, err
	}
	lapse, err := syncer.New(syncer.Options{Path: cfg.Sync.LapsePath, CacheDir: filepath.Join(cfg.DataDir, "lapse-cache"), AnalyzeTimeout: cfg.Sync.Timeout, SynchronizeTimeout: cfg.Sync.Timeout, MediaRoots: cfg.MediaRoots, Runner: options.LapseRunner})
	if err != nil {
		return nil, err
	}
	inventoryService := inventory.Service{Repository: repository, Probe: inventory.Probe{Path: "ffprobe", Runner: probeRunner}}
	installer := workflow.Installer{Repository: repository, MediaRoots: cfg.MediaRoots, Mode: cfg.Install.FileMode, UID: cfg.Install.UID, GID: cfg.Install.GID}
	workflows := make(map[domain.Language]*workflow.Service, len(routes))
	for language, providerIDs := range routes {
		ordered := make([]provider.Provider, 0, len(providerIDs))
		for _, id := range providerIDs {
			ordered = append(ordered, providers[id])
		}
		workflows[language] = &workflow.Service{
			Inventory: inventoryService, Searcher: &provider.Coordinator{Providers: ordered, Cache: repository, Clock: clock}, PackCache: packCache,
			Synchronizer: lapse, Installer: installer, Repository: repository, Providers: providers, ProviderOrder: append([]string(nil), providerIDs...),
			MinimumScore: cfg.MinimumReleaseScore, MinimumUpgradeDelta: workflow.DefaultMinimumUpgradeDelta, PackTTL: cfg.PackCache.TTL,
			LapsePolicy:          workflow.LapsePolicy{Mode: cfg.Sync.Policy, BypassScore: cfg.Sync.BypassScore, RequireIdentityAnchor: cfg.Sync.RequireIdentityAnchor, RequireEpisodeEvidence: cfg.Sync.RequireEpisodeEvidence, RequireReleaseGroup: cfg.Sync.RequireReleaseGroup, LapseForPacks: cfg.Sync.LapseForPacks, LapseForUpgrades: cfg.Sync.LapseForUpgrades},
			AllowHearingImpaired: cfg.AllowHearingImpaired, Clock: clock,
		}
	}

	languages := sortedLanguages(cfg)
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	reconcilers := make(map[string]catalog.Reconciler, len(cfg.Instances))
	webhookInstances := make(map[string]httpapi.Instance, len(cfg.Instances))
	for _, instance := range cfg.Instances {
		arrCatalog := catalogs[instance.Name]
		reconciler := catalog.Reconciler{Instance: instance.Name, Catalog: arrCatalog, Store: repository, Languages: languages, Now: clock.Now, OnCommitted: notify}
		reconcilers[instance.Name] = reconciler
		webhookInstances[instance.Name] = httpapi.Instance{Token: instance.WebhookToken, Handler: catalog.WebhookHandler{Instance: instance.Name, InstanceType: instance.Type, Catalog: arrCatalog, Store: repository, Languages: languages, Now: clock.Now, OnApplied: notify}}
	}

	notifiers := map[string]notifier.Notifier{}
	if cfg.Silo.Enabled {
		mappings := make([]notifier.PathMapping, 0, len(cfg.Silo.PathMappings))
		for _, mapping := range cfg.Silo.PathMappings {
			mappings = append(mappings, notifier.PathMapping{From: mapping.From, To: mapping.To})
		}
		silo, err := notifier.NewSilo(notifier.SiloConfig{Enabled: true, BaseURL: cfg.Silo.URL, APIKey: cfg.Silo.APIKey, PathMappings: mappings})
		if err != nil {
			return nil, err
		}
		notifiers["silo"] = silo
	}
	reconcilerInterfaces := make(map[string]worker.Reconciler, len(reconcilers))
	for name, reconciler := range reconcilers {
		reconcilerInterfaces[name] = reconciler
	}
	workerRunner := options.Worker
	if workerRunner == nil {
		workerRunner = &worker.Worker{Repository: repository, Workflow: workflowRouter(workflows), Clock: clock, Notifiers: notifiers, Reconcilers: reconcilerInterfaces, MaxWorkflows: cfg.Worker.MaxConcurrent, Wake: wake, OnError: func(err error) { logger.Error("background cycle failed", "error", redactedError(err, cfg)) }}
	}

	application := &App{Config: cfg, Store: database, Repository: repository, Catalogs: catalogs, Providers: providers, Reconcilers: reconcilers, Workflows: workflows, Inventory: inventoryService, Lapse: lapse, LapseRunner: options.LapseRunner, ProbeRunner: probeRunner, Worker: workerRunner, Listener: options.Listener, Logger: logger, Clock: clock}
	application.Handler = httpapi.Server{Instances: webhookInstances, Ready: application.Ready, Logger: logger}.Handler()
	return application, nil
}

func buildCatalogs(cfg config.Config, supplied map[string]catalog.Catalog) (map[string]catalog.Catalog, error) {
	if supplied != nil {
		result := make(map[string]catalog.Catalog, len(supplied))
		for name, item := range supplied {
			result[name] = item
		}
		for _, instance := range cfg.Instances {
			if result[instance.Name] == nil {
				return nil, fmt.Errorf("Arr instance %q has no catalog", instance.Name)
			}
		}
		return result, nil
	}
	result := make(map[string]catalog.Catalog, len(cfg.Instances))
	for _, instance := range cfg.Instances {
		var item catalog.Catalog
		var err error
		switch instance.Type {
		case "sonarr":
			item, err = catalog.NewSonarr(instance.Name, instance.URL, instance.APIKey, instance.PathMappings, cfg.MediaRoots)
		case "radarr":
			item, err = catalog.NewRadarr(instance.Name, instance.URL, instance.APIKey, instance.PathMappings, cfg.MediaRoots)
		default:
			err = fmt.Errorf("unknown Arr instance type %q", instance.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("configure Arr instance %s: %w", instance.Name, err)
		}
		result[instance.Name] = item
	}
	return result, nil
}

func buildProviders(cfg config.Config, database *store.Store, repository *store.Repository, clock provider.Clock, client *http.Client, supplied map[string]provider.Provider) (map[string]provider.Provider, error) {
	if supplied != nil {
		result := make(map[string]provider.Provider, len(supplied))
		for id, item := range supplied {
			result[id] = item
		}
		return result, nil
	}
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	specs := make([]provider.InstanceSpec, 0, len(ids))
	for _, id := range ids {
		spec := cfg.Providers[id]
		specs = append(specs, provider.InstanceSpec{ID: id, Type: spec.Type, Settings: spec.Settings})
	}
	registry := provider.NewRegistry(map[string]provider.Factory{"titlovi": titlovi.Factory, "opensubtitles": opensubtitles.Factory, "subdl": subdl.Factory})
	gate := provider.NewGate(repository, clock, cfg.ProviderHTTP.SharedOriginMaxConcurrent)
	return registry.Build(specs, provider.Dependencies{HTTPClient: client, Store: database, Clock: clock, Gate: gate})
}

func languageRoutes(cfg config.Config) map[domain.Language][]string {
	routes := make(map[domain.Language][]string, len(cfg.Languages))
	for language, route := range cfg.Languages {
		routes[language] = append([]string(nil), route.Providers...)
	}
	return routes
}

func sortedLanguages(cfg config.Config) []domain.Language {
	languages := make([]domain.Language, 0, len(cfg.Languages))
	for language := range cfg.Languages {
		languages = append(languages, language)
	}
	sort.Slice(languages, func(i, j int) bool { return languages[i] < languages[j] })
	return languages
}

type workflowRouter map[domain.Language]*workflow.Service

func (r workflowRouter) Run(ctx context.Context, request workflow.Request) (workflow.Result, error) {
	service := r[request.Language]
	if service == nil {
		return workflow.Result{}, fmt.Errorf("language %s is not configured", request.Language)
	}
	return service.Run(ctx, request)
}

func (a *App) Ready(ctx context.Context) error {
	if a.Store == nil {
		return fmt.Errorf("SQLite is not open")
	}
	if err := a.Store.Ping(ctx); err != nil {
		return err
	}
	return validateMediaRoots(a.Config.MediaRoots)
}

func validateMediaRoots(roots []string) error {
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("inspect media root: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("media root is not a directory")
		}
		if _, err := filepath.EvalSymlinks(root); err != nil {
			return fmt.Errorf("resolve media root: %w", err)
		}
	}
	return nil
}

func (a *App) Serve(ctx context.Context) error {
	release, err := a.acquireMutationLock()
	if err != nil {
		return err
	}
	defer release()
	listener := a.Listener
	if listener == nil {
		listener, err = net.Listen("tcp", a.Config.Server.Listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", a.Config.Server.Listen, err)
		}
	}
	server := &http.Server{Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverDone := make(chan error, 1)
	workerDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	go func() { workerDone <- a.Worker.Run(runCtx) }()

	var cause error
	select {
	case <-ctx.Done():
	case err := <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			cause = fmt.Errorf("HTTP server: %w", err)
		}
		serverDone = nil
	case err := <-workerDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			cause = fmt.Errorf("worker: %w", err)
		}
		workerDone = nil
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	cancel()
	if serverDone != nil {
		if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	if workerDone != nil {
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	return errors.Join(cause, shutdownErr)
}

func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if a.Store != nil {
			a.closeErr = a.Store.Close()
		}
	})
	return a.closeErr
}

func (a *App) Redact(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(redactedError(err, a.Config))
}

func (a *App) Scan(ctx context.Context, instance string, forceProbe bool) (string, error) {
	release, err := a.acquireMutationLock()
	if err != nil {
		return "", err
	}
	defer release()
	reconciler, ok := a.Reconcilers[instance]
	if !ok {
		return "", fmt.Errorf("unknown instance %q", instance)
	}
	if err := reconciler.Run(ctx); err != nil {
		return "", err
	}
	items, err := a.Repository.ListMediaByInstance(ctx, instance)
	if err != nil {
		return "", err
	}
	if forceProbe {
		for _, item := range items {
			if _, err := a.Inventory.Refresh(ctx, item.ID, item.Media, true); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("scan complete: instance=%s media=%d force_probe=%t", instance, len(items), forceProbe), nil
}

func (a *App) Search(ctx context.Context, instance, kind string, fileID int64, languageTag string, retryRejected bool) (string, error) {
	release, err := a.acquireMutationLock()
	if err != nil {
		return "", err
	}
	defer release()
	language, service, err := a.workflowFor(languageTag)
	if err != nil {
		return "", err
	}
	arrCatalog := a.Catalogs[instance]
	if arrCatalog == nil {
		return "", fmt.Errorf("unknown instance %q", instance)
	}
	ref := domain.MediaRef{Instance: instance, Kind: domain.MediaKind(kind), FileID: fileID}
	media, err := arrCatalog.GetMedia(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("load media: %w", err)
	}
	mediaID, _, err := a.Repository.UpsertMedia(ctx, media)
	if err != nil {
		return "", err
	}
	if retryRejected {
		if err := a.Repository.ClearCandidateRejections(ctx, mediaID, language); err != nil {
			return "", err
		}
	}
	result, err := service.Run(ctx, workflow.Request{MediaID: mediaID, Media: media, Language: language, Manual: true})
	if err != nil {
		return "", err
	}
	return formatWorkflowResult(mediaID, result), nil
}

func (a *App) Retry(ctx context.Context, providerID string) (string, error) {
	if a.Providers[providerID] == nil {
		return "", fmt.Errorf("unknown provider %q", providerID)
	}
	release, err := a.acquireMutationLock()
	if err != nil {
		return "", err
	}
	defer release()
	if err := a.Repository.ClearProviderState(ctx, providerID); err != nil {
		return "", err
	}
	return fmt.Sprintf("provider retry state reset: %s", providerID), nil
}

func (a *App) Explain(ctx context.Context, instance, kind string, fileID int64, languageTag string) (string, error) {
	language, _, err := a.workflowFor(languageTag)
	if err != nil {
		return "", err
	}
	if a.Catalogs[instance] == nil {
		return "", fmt.Errorf("unknown instance %q", instance)
	}
	mediaID, media, err := a.Repository.FindMedia(ctx, domain.MediaRef{Instance: instance, Kind: domain.MediaKind(kind), FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("media is not indexed: %w", err)
	}
	var output strings.Builder
	fmt.Fprintf(&output, "media: id=%d instance=%s kind=%s file_id=%d title=%q\n", mediaID, instance, kind, fileID, media.Title)
	inventoryRecord, err := a.Repository.GetTrackInventory(ctx, mediaID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&output, "inventory: tracks=%d fingerprint_size=%d fingerprint_mtime=%s\n", len(inventoryRecord.Tracks), inventoryRecord.Fingerprint.Size, inventoryRecord.Fingerprint.ModTime.Format(time.RFC3339Nano))
	for _, track := range inventoryRecord.Tracks {
		fmt.Fprintf(&output, "  track: language=%s embedded=%t forced=%t sdh=%t protected=%t\n", track.Language, track.Embedded, track.Forced, track.SDH, track.Protected)
	}
	if status, statusErr := a.Repository.GetSearchStatus(ctx, mediaID, language); statusErr == nil {
		fmt.Fprintf(&output, "search: state=%s outcome=%s priority=%s rerun_pending=%t missing_attempt=%d failure_attempt=%d next_attempt=%s\n", status.State, status.LastOutcome, status.Priority, status.RerunPending, status.Attempt, status.FailureAttempt, status.NextAttemptAt.Format(time.RFC3339Nano))
	} else if errors.Is(statusErr, sql.ErrNoRows) {
		fmt.Fprintf(&output, "search: not scheduled\n")
	} else {
		return "", statusErr
	}
	candidates, err := a.Repository.ListCandidates(ctx, mediaID, language)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&output, "candidates: %d\n", len(candidates))
	for _, candidate := range candidates {
		fmt.Fprintf(&output, "  candidate: provider=%s result=%s score=%s identity=%s\n", candidate.ProviderID, candidate.ResultID, compactJSON(candidate.ScoreJSON), compactJSON(candidate.ValidationJSON))
	}
	rejections, err := a.Repository.ListCandidateRejections(ctx, mediaID, language, a.Clock.Now())
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&output, "candidate_rejections: %d\n", len(rejections))
	for _, rejection := range rejections {
		fmt.Fprintf(&output, "  rejection: provider=%s result=%s reason=%s artifact=%s expires=%s\n", rejection.ProviderID, rejection.ResultID, rejection.ReasonCode, rejection.ArtifactChecksum, rejection.ExpiresAt.Format(time.RFC3339Nano))
	}
	installation, installed, err := a.Repository.GetInstallation(ctx, mediaID, language)
	if err != nil {
		return "", err
	}
	if installed {
		fmt.Fprintf(&output, "installation: provider=%s candidate=%s checksum=%s score=%s lapse=%s rollback=%t\n", installation.ProviderID, installation.CandidateID, installation.Checksum, compactJSON(installation.ScoreJSON), compactJSON(installation.SyncResultJSON), installation.RollbackPath != "")
	} else {
		fmt.Fprintln(&output, "installation: none")
	}
	packs, err := a.Repository.ListReusablePacks(ctx, store.PackLookup{SeriesIDs: media.ExternalIDs, SeriesTitle: media.Title, SeriesYear: media.Year, Season: media.Season, Episode: media.Episode, AbsoluteEpisode: media.AbsoluteEpisode, Language: language.String()}, a.Clock.Now())
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&output, "pack_cache: reusable=%d\n", len(packs))
	for _, providerID := range a.Config.Languages[language].Providers {
		states, err := a.Repository.ListProviderStates(ctx, providerID)
		if err != nil {
			return "", err
		}
		for _, state := range states {
			fmt.Fprintf(&output, "provider_state: provider=%s scope=%s reason=%s remaining=%d reset=%s disabled=%t\n", providerID, state.Scope, state.Reason, state.Remaining, state.ResetAt.Format(time.RFC3339Nano), state.Disabled)
		}
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func (a *App) Doctor(ctx context.Context) (string, error) {
	if err := a.Ready(ctx); err != nil {
		return "", err
	}
	if err := syncer.CheckCapabilities(ctx, a.Config.Sync.LapsePath, a.LapseRunner); err != nil {
		return "", err
	}
	if _, _, err := a.ProbeRunner.Run(ctx, "ffprobe", "-version"); err != nil {
		return "", fmt.Errorf("run ffprobe capability check: %w", err)
	}
	return fmt.Sprintf("configuration: ok\nsqlite: ok\nmedia roots: ok (%d)\nLAPSE: compatible\nffprobe: available", len(a.Config.MediaRoots)), nil
}

func (a *App) AnalyzeSync(ctx context.Context, mediaPath, subtitlePath string) (string, error) {
	if !withinRoots(mediaPath, a.Config.MediaRoots) {
		return "", fmt.Errorf("media path is outside configured roots")
	}
	result, err := a.Lapse.Analyze(ctx, mediaPath, subtitlePath)
	if err != nil {
		return "", err
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (a *App) workflowFor(tag string) (domain.Language, *workflow.Service, error) {
	language, err := domain.ParseLanguage(tag)
	if err != nil {
		return "", nil, err
	}
	service := a.Workflows[language]
	if service == nil {
		return "", nil, fmt.Errorf("language %s is not configured", language)
	}
	return language, service, nil
}

func (a *App) acquireMutationLock() (func(), error) {
	lockPath := filepath.Join(a.Config.DataDir, "subsyncd.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another subsyncd mutation process is active")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

func withinRoots(path string, roots []string) bool {
	clean := filepath.Clean(path)
	for _, root := range roots {
		relative, err := filepath.Rel(filepath.Clean(root), clean)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func formatWorkflowResult(mediaID int64, result workflow.Result) string {
	output := fmt.Sprintf("media_id: %d\noutcome: %s", mediaID, result.Outcome)
	if result.Candidate.ProviderID != "" {
		output += fmt.Sprintf("\nprovider: %s\ncandidate: %s\nscore: %d", result.Candidate.ProviderID, result.Candidate.ResultID, result.Score.Total)
	}
	if !result.RetryAt.IsZero() {
		output += "\nretry_at: " + result.RetryAt.Format(time.RFC3339Nano)
	}
	return output
}

func compactJSON(payload []byte) string {
	if len(payload) == 0 {
		return "{}"
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, payload); err != nil {
		return "{}"
	}
	return buffer.String()
}

func redactedError(err error, cfg config.Config) string {
	value := err.Error()
	for _, instance := range cfg.Instances {
		for _, secret := range []string{instance.APIKey, instance.WebhookToken} {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[redacted]")
			}
		}
	}
	if cfg.Silo.APIKey != "" {
		value = strings.ReplaceAll(value, cfg.Silo.APIKey, "[redacted]")
	}
	for _, spec := range cfg.Providers {
		for _, secret := range providerSecrets([]*yaml.Node{&spec.Settings}) {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	for _, root := range cfg.MediaRoots {
		value = strings.ReplaceAll(value, root, "[media]")
	}
	return value
}

func providerSecrets(nodes []*yaml.Node) []string {
	var secrets []string
	for _, node := range nodes {
		if node.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(node.Content); index += 2 {
				key, item := strings.ToLower(node.Content[index].Value), node.Content[index+1]
				if item.Kind == yaml.ScalarNode && (strings.Contains(key, "password") || strings.Contains(key, "api_key") || strings.Contains(key, "token") || strings.Contains(key, "username")) && len(item.Value) >= 4 {
					secrets = append(secrets, item.Value)
				}
				secrets = append(secrets, providerSecrets(item.Content)...)
			}
		} else {
			secrets = append(secrets, providerSecrets(node.Content)...)
		}
	}
	return secrets
}
