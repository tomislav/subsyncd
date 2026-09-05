# Structured Loki Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Emit comprehensive, privacy-safe newline-delimited JSON events that Grafana Alloy can collect and query in Loki without changing subtitle behavior.

**Architecture:** Add strict startup logging configuration and a small synchronous event emitter over `log/slog`. Inject component-scoped emitters into the app, HTTP server, worker, provider coordinator/gate, and workflow; propagate job correlation through context and log typed results at their owning boundaries. Keep Loki transport and deployment labels in Alloy.

**Tech Stack:** Go 1.27, `log/slog`, existing YAML configuration, existing fake-driven Go tests, Grafana Alloy/Loki documentation.

**Spec:** `docs/superpowers/specs/2026-09-04-structured-loki-logging-design.md`

## Global Constraints

- Runtime application logs are one JSON object per physical line on standard error.
- Every event contains `time`, `level`, `msg`, `event`, `service`, `version`, and `component`.
- `logging.level` and `SUBSYNCD_LOG_LEVEL` accept exactly `debug`, `info`, `warn`, or `error`; the environment override wins and the default is `info`.
- Successful `/healthz` and `/readyz` probes are silent; readiness logs only unhealthy and recovered transitions at normal levels.
- Candidate-by-candidate scoring, filenames/release names, cache details, and safe root-relative paths are `debug` only.
- Absolute paths, credentials, query strings, provider URLs/bodies, subtitle content, archive listings, and raw LAPSE output are never logged.
- Dynamic IDs and language/provider values remain JSON fields, not recommended Loki labels.
- Expected absence, mismatch, throttling, and non-solid LAPSE results are not `error` events.
- A failure is logged once at its owning boundary; adjacent callers emit outcome summaries without repeating the same error.
- Logging must not change provider selection, scores, schedules, leases, LAPSE behavior, installation, or notification semantics.
- Do not add asynchronous log queues, direct Loki clients, OpenTelemetry, metrics, runtime level reload, or new HTTP routes.
- Follow repository TDD: focused red test, confirm the intended failure, minimal implementation, affected package with `-race`, then commit.

## File structure and responsibilities

- `internal/config/config.go`: normalized logging configuration and environment precedence.
- `internal/config/config_test.go`: strict level/default/override contract.
- `internal/observability/emitter.go`: JSON logger construction, component scoping, context correlation, event emission, and level parsing.
- `internal/observability/privacy.go`: bounded single-line error/text sanitization and proven root-relative paths.
- `internal/observability/emitter_test.go`, `privacy_test.go`: event schema, filtering, typing, redaction, and path safety.
- `cmd/subsyncd/main.go`: pass command, version, and stderr into application logging bootstrap.
- `internal/cli/cli.go`: preserve plain usage errors before bootstrap while delegating initialized backend failures to structured reporting.
- `internal/app/app.go`: construct/inject emitters, lifecycle events, command correlation, and configuration-aware redaction.
- `internal/httpapi/server.go`: webhook lifecycle and readiness transition events.
- `internal/catalog/webhook.go`: return bounded webhook application counts needed by the HTTP summary.
- `internal/worker/worker.go`: queue, job, reconciliation, notification, retry, lease, and shutdown events.
- `internal/store/repository.go`: report whether completion scheduled a coalesced rerun and whether notification enqueue inserted a durable row.
- `internal/provider/coordinator.go`: exact/broad search, cache, and download/search summary events.
- `internal/provider/observed.go`: provider download timing and outcome wrapper without adapter duplication.
- `internal/provider/limiter.go`, `transport.go`: persisted cooldown, transient circuit, authentication-disable, and recovery transitions.
- `internal/workflow/service.go`: inventory/search/scoring/selection/LAPSE/install/upgrade events using existing typed decisions.
- `config.example.yaml`, `README.md`, `docs/operations.md`: operator configuration, event contract, Alloy pipeline, labels, and LogQL examples.
- Existing tests beside every modified package: focused lifecycle assertions without network access.

---

### Task 1: Strict startup log-level configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`

**Interfaces:**
- Produces: `config.LoggingConfig{Level string}` at `Config.Logging`.
- Produces: `config.Load` precedence `SUBSYNCD_LOG_LEVEL` > YAML `logging.level` > `info`.
- Consumes: the existing injected `lookupEnv func(string) (string, bool)`; tests must not mutate process-global environment.

- [ ] **Step 1: Write failing configuration tests**

Add table-driven cases that load otherwise-minimal valid YAML and assert the normalized level. Include omission, mixed-case YAML values, each valid level, environment override, empty environment value, unknown YAML, and unknown environment values:

```go
func TestLoadLoggingLevel(t *testing.T) {
	tests := []struct {
		name, yamlLevel, envLevel, want string
		wantErr bool
	}{
		{name: "default", want: "info"},
		{name: "yaml debug", yamlLevel: "DeBuG", want: "debug"},
		{name: "environment wins", yamlLevel: "warn", envLevel: "error", want: "error"},
		{name: "empty environment ignored", yamlLevel: "warn", envLevel: " ", want: "warn"},
		{name: "invalid yaml", yamlLevel: "trace", wantErr: true},
		{name: "invalid environment", yamlLevel: "info", envLevel: "verbose", wantErr: true},
	}
	// Write the fixture, inject a lookup returning envLevel only for
	// SUBSYNCD_LOG_LEVEL, then assert cfg.Logging.Level or a bounded error.
}
```

Assert invalid-environment errors name `SUBSYNCD_LOG_LEVEL` but do not contain an arbitrary sentinel environment value longer than the accepted vocabulary.

- [ ] **Step 2: Run the focused tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/config -race -run 'TestLoadLoggingLevel' -count=1 -v
```

Expected: FAIL because `Config.Logging` and strict logging normalization do not exist.

- [ ] **Step 3: Implement logging normalization and validation**

Add:

```go
const defaultLogLevel = "info"

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type Config struct {
	// existing fields...
	Logging LoggingConfig
}

type rawConfig struct {
	// existing fields...
	Logging LoggingConfig `yaml:"logging"`
}
```

In `Load`, after YAML normalization and before `Validate`, apply the injected environment override only when `strings.TrimSpace(value) != ""`. Normalize through one helper:

```go
func normalizeLogLevel(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return defaultLogLevel, nil
	}
	switch value {
	case "debug", "info", "warn", "error":
		return value, nil
	default:
		return "", fmt.Errorf("log level must be debug, info, warn, or error")
	}
}
```

Wrap an invalid override as `SUBSYNCD_LOG_LEVEL is invalid` without echoing its value. Ensure `Config.Validate` also rejects a manually constructed invalid `Config.Logging.Level`, so `app.New` retains strict validation.

Add the documented default to `config.example.yaml`:

```yaml
logging:
  level: info
```

- [ ] **Step 4: Run focused and complete configuration tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/config -race -count=1
```

Expected: PASS, including strict unknown-field coverage.

- [ ] **Step 5: Commit the configuration contract**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat: configure structured log levels"
```

### Task 2: Safe structured event emitter

**Files:**
- Create: `internal/observability/emitter.go`
- Create: `internal/observability/emitter_test.go`
- Create: `internal/observability/privacy.go`
- Create: `internal/observability/privacy_test.go`

**Interfaces:**
- Consumes: normalized level strings from Task 1.
- Produces:

```go
type Options struct {
	Level      string
	Version    string
	MediaRoots []string
	Redact     func(error) string
}

func New(out io.Writer, options Options) (*Emitter, error)
func Discard() *Emitter
func (e *Emitter) For(component string) *Emitter
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context
func (e *Emitter) Log(ctx context.Context, level slog.Level, event, message string, attrs ...slog.Attr)
func (e *Emitter) ErrorAttrs(kind string, err error) []slog.Attr
func (e *Emitter) RelativePath(path string) (string, bool)
func SafeText(value string) string
```

- [ ] **Step 1: Write failing emitter schema and filtering tests**

Create buffer-backed tests that decode every output line into `map[string]any` and assert common fields, JSON types, context correlation, component scoping, and severity filtering:

```go
events, err := New(&logs, Options{Level: "info", Version: "sha-test"})
require.NoError(t, err)
ctx := WithAttrs(context.Background(), slog.String("job_id", "job-1"), slog.Int64("media_id", 42))
events.For("worker").Log(ctx, slog.LevelInfo, "job.started", "subtitle job started", slog.Int("attempt", 2))
events.For("worker").Log(ctx, slog.LevelDebug, "candidate.evaluated", "candidate evaluated")

record := decodeSingleJSONLine(t, logs.String())
assert.Equal(t, "job.started", record["event"])
assert.Equal(t, "subsyncd", record["service"])
assert.Equal(t, "sha-test", record["version"])
assert.Equal(t, "worker", record["component"])
assert.Equal(t, float64(42), record["media_id"])
assert.NotContains(t, logs.String(), "candidate.evaluated")
```

Also assert invalid levels fail, empty versions become `dev`, empty event/component values cannot emit, and a newline in `msg` remains inside one valid physical JSON line.

- [ ] **Step 2: Write failing privacy tests**

Cover:

```go
func TestErrorAttrsRedactsBoundsAndFlattens(t *testing.T) { /* secret/root/newline sentinels */ }
func TestRelativePathAllowsContainedExistingFile(t *testing.T) { /* returns season/file.mkv */ }
func TestRelativePathAllowsContainedFutureSidecar(t *testing.T) { /* resolved parent */ }
func TestRelativePathRejectsOutsideAndSymlinkEscape(t *testing.T) { /* ok == false */ }
func TestSafeTextBoundsAndRemovesLineBreaks(t *testing.T) { /* one-line UTF-8 */ }
```

Use a temporary real root and symlink escape fixture. Assert no absolute temporary prefix survives.

- [ ] **Step 3: Run the observability tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/observability -race -count=1 -v
```

Expected: FAIL because the package does not exist.

- [ ] **Step 4: Implement the synchronous emitter**

Use `slog.NewJSONHandler` with a parsed `slog.Level`, attach `service=subsyncd` and the normalized version once, and store the component on each derived emitter. `Log` must use `LogAttrs` and prepend copied context attributes:

```go
type Emitter struct {
	logger    *slog.Logger
	component string
	redact    func(error) string
	roots     []resolvedRoot
}

func (e *Emitter) Log(ctx context.Context, level slog.Level, event, message string, attrs ...slog.Attr) {
	if e == nil || strings.TrimSpace(event) == "" || strings.TrimSpace(e.component) == "" {
		return
	}
	all := []slog.Attr{slog.String("event", event), slog.String("component", e.component)}
	all = append(all, attrsFromContext(ctx)...)
	all = append(all, attrs...)
	e.logger.LogAttrs(ctx, level, SafeText(message), all...)
}
```

Copy attributes when storing them in context so caller slice mutation cannot alter a later event. Keep writes synchronous through the standard handler; add no goroutine, buffer, retry, or global logger.

- [ ] **Step 5: Implement centralized privacy helpers**

`SafeText` replaces CR/LF and other control line separators with spaces, trims, preserves valid UTF-8, and caps output at 2,048 runes. `ErrorAttrs` calls the configured redactor, then `SafeText`, and emits exactly `error_kind` plus `error`.

Resolve media roots at construction. For an existing target, evaluate the target symlinks; for a future sidecar, evaluate its existing parent and append only its base name. Use `filepath.Rel` and reject `..`, absolute results, empty base names, or any unresolvable containment. Return slash-normalized relative paths and never return the root itself.

- [ ] **Step 6: Run observability tests with the race detector**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/observability -race -count=1
```

Expected: PASS with exactly one JSON line in schema tests and no path/secret sentinels in privacy tests.

- [ ] **Step 7: Commit the event foundation**

```bash
git add internal/observability
git commit -m "feat: add privacy-safe event logging"
```

### Task 3: Bootstrap logging and application lifecycle

**Files:**
- Modify: `cmd/subsyncd/main.go`
- Modify: `cmd/subsyncd/main_test.go`
- Modify: `internal/cli/cli.go`
- Modify: `internal/cli/cli_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`

**Interfaces:**
- Consumes: `observability.New`, `Emitter.For`, and `config.Config.Logging`.
- Replaces:

```go
type OpenFunc func(context.Context, string, string) (Backend, error) // ctx, config path, command
type FailureReporter interface {
	ReportFailure(context.Context, string, error) // command, sanitized failure
}

type OpenOptions struct {
	LogWriter io.Writer
	Version   string
	Command   string
}

func Open(context.Context, string, OpenOptions) (*App, error)
```

- Extends `app.Options` with `Events *observability.Emitter`; replace production `Logger *slog.Logger` wiring rather than retaining two logging systems.

- [ ] **Step 1: Write failing CLI/bootstrap tests**

Update fake `OpenFunc` signatures and add tests proving:

```go
func TestInitializedBackendFailureUsesReporterInsteadOfPlainText(t *testing.T) {
	backend := &fakeBackend{serveErr: errors.New("backend exploded")}
	command := Command{Open: func(context.Context, string, string) (Backend, error) { return backend, nil }, Stderr: &stderr}
	code := command.Run(context.Background(), []string{"serve"})
	assert.Equal(t, ExitFailure, code)
	assert.Equal(t, 1, backend.reportCalls)
	assert.NotContains(t, stderr.String(), "backend exploded")
}
```

Retain tests proving flag/unknown-command errors before backend creation remain `subsyncd: ...` text. In `cmd/subsyncd/main_test.go`, use a valid fixture with `logging.level: debug` and assert the opened application receives a debug-enabled JSON emitter without relying on JSON field order.

- [ ] **Step 2: Write failing lifecycle/redaction tests**

Construct an application with a buffer emitter and injected fakes. Assert `service.starting`, `service.ready`, `service.shutdown_requested`, and `service.stopped` contain command/version/count/concurrency/duration fields. Make startup and serve failures contain stable `error_kind` but not configured secrets, media roots, or embedded newlines.

- [ ] **Step 3: Run focused tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./cmd/subsyncd ./internal/cli ./internal/app -race -run 'Logging|Lifecycle|InitializedBackendFailure' -count=1 -v
```

Expected: FAIL because bootstrap still constructs an unconfigured raw `slog.Logger`, `OpenFunc` lacks command context, and lifecycle events do not exist.

- [ ] **Step 4: Move emitter construction behind configuration loading**

`cmd/subsyncd/main.go` should no longer import `log/slog`. Pass stderr, build version, and command into `app.Open`:

```go
Open: func(ctx context.Context, path, command string) (cli.Backend, error) {
	return app.Open(ctx, path, app.OpenOptions{LogWriter: stderr, Version: version.Value, Command: command})
},
```

`app.Open` loads configuration first, creates the emitter with `cfg.Logging.Level`, the build version, media roots, and `redactedError` closure, emits `service.starting`, and calls `New` with `Options.Events`. A nil test emitter becomes `observability.Discard()`, not a process-global/default stderr logger.

- [ ] **Step 5: Implement lifecycle and structured backend failure ownership**

Add `Events *observability.Emitter` and `Command string` to `App`. Emit bounded non-secret startup counts and feature flags:

```go
events.For("app").Log(ctx, slog.LevelInfo, "service.starting", "subsyncd starting",
	slog.String("command", command),
	slog.Int("instance_count", len(cfg.Instances)),
	slog.Int("provider_count", len(cfg.Providers)),
	slog.Int("language_count", len(cfg.Languages)),
	slog.Int("max_concurrent", cfg.Worker.MaxConcurrent),
)
```

Emit `service.ready` only after `New` succeeds. In `Serve`, time the run, classify shutdown trigger as `signal`, `context`, or `component_failure`, and emit one request/stopped pair. `ReportFailure` emits `command.failed` through the app emitter with sanitized error attributes. `cli.Command` calls it instead of writing the backend error as plain text; when no reporter exists, preserve the current fallback.

Do not log successful read-only/one-shot command output or configuration contents.

- [ ] **Step 6: Run affected packages**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./cmd/subsyncd ./internal/cli ./internal/app -race -count=1
```

Expected: PASS and daemon application records are valid JSON only after initialization.

- [ ] **Step 7: Commit bootstrap and lifecycle logging**

```bash
git add cmd/subsyncd internal/cli internal/app
git commit -m "feat: log application lifecycle events"
```

### Task 4: Webhook summaries and readiness transitions

**Files:**
- Modify: `internal/catalog/webhook.go`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/server_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**
- Produces:

```go
type WebhookResult struct {
	EventCount   int
	AppliedCount int
}

type WebhookHandler interface {
	Handle(context.Context, []byte) (catalog.WebhookResult, error)
}
```

- Consumes: `httpapi.Server.Events *observability.Emitter` and application-scoped redaction.

- [ ] **Step 1: Write failing webhook result tests**

Update catalog webhook tests to assert exact duplicate delivery returns `EventCount: 1, AppliedCount: 0`, a new delivery returns `1, 1`, multi-file partial progress retains the successfully applied count alongside the error, and ignored/invalid payloads return zero counts.

- [ ] **Step 2: Write failing HTTP logging tests**

Replace substring-only slog tests with decoded JSON events. Assert:

```go
func TestSuccessfulProbesAreSilent(t *testing.T) { /* healthz + healthy readyz => zero records */ }
func TestReadinessLogsTransitionsOnly(t *testing.T) { /* fail, fail, success, success => unhealthy + recovered */ }
func TestWebhookLifecycleIsCorrelated(t *testing.T) { /* accepted + applied share request_id */ }
func TestWebhookLogsNeverContainQueryOrToken(t *testing.T) { /* path is /webhooks/main only */ }
```

Also cover unknown instance (404), authentication rejection, invalid JSON, semantic rejection, ignored test event, partial application failure, and service failure. Assert status, duration, event/applied counts, and stable `reason` values without raw error bodies.

- [ ] **Step 3: Run focused tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog ./internal/httpapi -race -run 'Webhook|Readiness|Probe' -count=1 -v
```

Expected: FAIL because handlers return only errors, successful webhooks have no summary events, and readiness warns on every failure.

- [ ] **Step 4: Return bounded catalog application counts**

Change `catalog.WebhookHandler.Handle` to accumulate and return `WebhookResult`. Preserve the current deferred `OnApplied` behavior after any committed mutation, including a later error. Do not return event IDs, file paths, or media objects to HTTP.

- [ ] **Step 5: Implement HTTP event ownership and readiness state**

Replace `Server.Logger` with `Server.Events`. Have `Handler` allocate a private mutex-protected readiness tracker shared by its closures. State rules:

```text
unknown + success -> healthy, silent
unknown/healthy + failure -> unhealthy, warn readiness.unhealthy
unhealthy + failure -> silent at info (optional bounded debug diagnostic)
unhealthy + success -> healthy, info readiness.recovered
healthy + success -> silent
```

Emit `webhook.accepted` only after authentication, body-size, and JSON-object validation. Emit `webhook.applied` on success or ignored event with `outcome`, `event_count`, `applied_count`, `status`, and `duration_ms`. Emit `webhook.rejected` with a stable reason for every client rejection and a single `webhook.failed` error event for dependency failure. Every event includes request ID, method, and query-free `request.URL.Path`.

- [ ] **Step 6: Run affected package suites**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog ./internal/httpapi ./internal/app -race -count=1
```

Expected: PASS with exactly two normal events for an applied webhook and no successful probe noise.

- [ ] **Step 7: Commit HTTP observability**

```bash
git add internal/catalog/webhook.go internal/catalog/webhook_test.go internal/httpapi/server.go internal/httpapi/server_test.go internal/app/app.go
git commit -m "feat: log webhook and readiness transitions"
```

### Task 5: Durable worker, reconciliation, and notification events

**Files:**
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**
- Adds: `worker.Worker.Events *observability.Emitter`.
- Changes worker repository results without changing persistence semantics:

```go
type SearchCompletionResult struct { RerunScheduled bool }
func (*Repository) CompleteSearch(context.Context, SearchCompletion) (SearchCompletionResult, error)
func (*Repository) EnqueueNotification(context.Context, NotificationRequest) (inserted bool, err error)
```

- Consumes: `observability.WithAttrs` to propagate `job_id`, `media_id`, `media_kind`, `file_id`, `language`, `priority`, and `attempt` through workflow context.
- Preserves: `OnError func(error)` only as an optional non-logging/test hook; production application wiring no longer uses it as a generic logger.

- [ ] **Step 1: Write failing search-job lifecycle tests**

Extend the existing fake repository/workflow tests to decode events and assert:

- `job.leased` and `job.started` include the same correlation fields;
- `job.completed` is emitted only after successful compare-and-swap completion and contains outcome, duration, and next schedule;
- technical failure emits one sanitized failed outcome plus `job.retry_scheduled` with failure attempt;
- missing/rejected, throttled, satisfied, installed, and same-key rerun schedules keep their existing counters/priorities;
- lease renewal loss emits `job.lease_lost` once and does not emit a successful completion;
- cancellation is not reported as an unexpected error.

Add repository tests proving `CompleteSearch` returns `RerunScheduled: true` only when the compare-and-swap completion consumed a pending same-key rerun, and `EnqueueNotification` returns true for the inserted row but false for a deduplicated conflict. Existing row state, counters, priority, and lease assertions remain unchanged.

Use assertions such as:

```go
events := recordsFor(t, logs.String(), "job.completed")
require.Len(t, events, 1)
assert.Equal(t, "installed", events[0]["outcome"])
assert.Equal(t, "job-1", events[0]["job_id"])
assert.IsType(t, float64(0), events[0]["duration_ms"])
```

- [ ] **Step 2: Write failing reconciliation, notification, and shutdown tests**

Assert `reconcile.started/completed/failed`, `notification.queued/delivered/retry_scheduled/failed`, and worker drain events. Notification logs must include notifier, notification job ID, attempt, outcome, retry time, and duration but never decoded payload paths. A timed-out drain emits one warning with active counts; normal drain emits only the completion summary.

- [ ] **Step 3: Run focused worker tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker -race -run 'Event|Lifecycle|Notification|Reconcile|Drain' -count=1 -v
```

Expected: FAIL because `Worker` exposes only `OnError` and no structured lifecycle events.

- [ ] **Step 4: Add correlation and single-owner job outcomes**

Scope the injected emitter to `worker`. On lease dispatch, log `job.leased`; after media hydration attach context fields:

```go
jobCtx = observability.WithAttrs(jobCtx,
	slog.String("job_id", lease.JobID),
	slog.Int64("media_id", lease.MediaID),
	slog.String("language", lease.Language),
	slog.String("priority", lease.Priority.String()),
	slog.Int("attempt", lease.Attempt),
)
```

After `GetMedia`, add instance/kind/file ID for downstream workflow logs. Refactor completion construction into a value returned before persistence so `job.completed` can log the exact committed outcome and schedule. Make repository completion transactionally read the leased row's `rerun_requested` value before applying the existing update and return it after commit. When true, emit `job.rerun_requested` with the current job/media/language correlation and the retained import priority. If completion persistence fails, emit only a failed job event with `error_kind=search_completion`; do not also emit success.

Classify stable reasons (`workflow_error`, `missing_backoff`, `provider_throttle`, `upgrade_wait`, `lease_lost`, `canceled`) without parsing error text.

- [ ] **Step 5: Instrument maintenance and notification ownership**

Time each reconciler independently. Log retryable and permanent notification completion after the repository completion succeeds. Change notification enqueue to return `RowsAffected()==1` and emit `notification.queued` only for a newly inserted durable row; use the dedupe prefix or durable job identity, never payload JSON. Log renewal failures at their lease owner and leave `OnError` for external observation only.

At daemon shutdown, stop new lease events immediately, record whether the drain timed out, cancel active work only at the existing deadline, and preserve current recoverable-lease behavior.

- [ ] **Step 6: Run worker and application suites**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker ./internal/app -race -count=1
```

Expected: PASS without scheduling, retry, concurrency, or shutdown behavior changes.

- [ ] **Step 7: Commit durable worker events**

```bash
git add internal/store/repository.go internal/store/repository_test.go internal/worker internal/app/app.go
git commit -m "feat: log durable worker lifecycle"
```

### Task 6: Provider search and persisted state-transition events

**Files:**
- Modify: `internal/provider/coordinator.go`
- Modify: `internal/provider/coordinator_test.go`
- Modify: `internal/provider/limiter.go`
- Modify: `internal/provider/limiter_test.go`
- Modify: `internal/provider/transport.go`
- Modify: `internal/provider/transport_test.go`
- Modify: `internal/provider/registry.go`
- Modify: `internal/provider/registry_test.go`
- Create: `internal/provider/observed.go`
- Create: `internal/provider/observed_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**
- Adds: `Coordinator.Events *observability.Emitter` and `Gate.Events *observability.Emitter`.
- Extends: `provider.Dependencies` with `Events *observability.Emitter`, allowing registry-built clients to inherit the provider emitter.
- Produces: an internal `observedProvider` wrapper that delegates identity/capabilities/language/search unchanged and owns `provider.download_completed` timing around `Download`.
- Preserves: provider public search/download APIs and typed cooldown/auth errors.

- [ ] **Step 1: Write failing coordinator event tests**

Cover exact search early success, exact miss followed by concurrent broad search, cache hit/miss, partial provider failure, all-provider failure, and canceled context. Assert:

- `provider.search_started/completed` include provider, mode, candidate count, duration, cache status, and bounded outcome;
- `search.completed` remains owned by workflow, so coordinator does not emit it;
- cache keys, download references, URLs, release names, media paths, and provider error text are absent at `info`;
- `debug` emits `provider.cache_hit` or `provider.cache_miss` without its hashed key.

Add wrapper tests proving download success, cooldown, disabled authentication, cancellation, and technical failure each emit exactly one `provider.download_completed` with provider, candidate ID, outcome, byte count when known, and duration. Its records must omit metadata filenames at `info`, response/download references, URLs, and wrapped network text.

- [ ] **Step 2: Write failing gate/transport transition tests**

Using the existing fake store and clock, assert:

```text
first persisted 429/quota window -> provider.cooldown_started
locally suppressed request -> no repeated transition event
each persisted 1/5/15/60m transient escalation -> provider.circuit_opened
successful non-5xx after transient state -> provider.recovered
first disabled authentication state -> provider.auth_disabled
later locally rejected disabled request -> no repeated auth event
ordinary nonzero rate-window update -> no warning transition
```

Events contain provider, scope, reason, reset time, failure attempt, and provider type when known; they contain neither origin nor request URL.

- [ ] **Step 3: Run provider tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -race -run 'Event|Transition|Cache' -count=1 -v
```

Expected: FAIL because coordinators and gates have no emitter and state writes do not expose/log transitions.

- [ ] **Step 4: Instrument exact/broad provider searches and cache decisions**

Time each `searchProvider` call and emit its completion exactly once, including cached successes. Use explicit `outcome` values `success`, `no_result`, `throttled`, `disabled`, `failed`, or `canceled`; classify typed provider errors with `errors.As` rather than their strings. The outer `Coordinator.Search` may emit one provider-component aggregate with exact/broad provider counts, but the workflow remains owner of the overall `search.completed` event.

- [ ] **Step 5: Emit persisted provider transitions without repeats**

Keep state mutation under `Gate.stateMu`. Before `PutProviderState`, read the previous row and compute an explicit transition:

```go
type stateTransition struct {
	becameUnavailable bool
	recovered         bool
	previous          store.ProviderState
	current           store.ProviderState
}
```

Log only after persistence succeeds. A cooldown transition requires a newly unavailable future-zero window or a changed later reset/reason; a circuit event accompanies each actual transient-attempt increment; auth disable requires `Disabled` changing false to true; recovery requires a nonzero transient attempt returning to zero. `Acquire` performs no transition logging because it is read-only suppression.

Wire the emitter through registry dependencies and `app.buildProviders`. Do not alter fallback durations, Retry-After precedence, or operation scoping.

Wrap each successfully constructed provider once in `Registry.Build` when events are enabled. The wrapper delegates `Search` without logging because the coordinator owns cached and uncached search events; it times `Download`, counts bytes through a narrow forwarding writer, classifies the existing typed errors, and logs a single completion. At `debug` it may add the `SafeText`-normalized returned filename. Supplied test providers should be wrapped by the same helper in `buildProviders` so application assembly has one consistent download boundary.

- [ ] **Step 6: Run all provider adapters and application assembly tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... ./internal/app -race -count=1
```

Expected: PASS; adapter request contracts and authentication refresh behavior remain unchanged.

- [ ] **Step 7: Commit provider observability**

```bash
git add internal/provider internal/app/app.go
git commit -m "feat: log provider searches and outages"
```

### Task 7: Workflow scoring, LAPSE, installation, and upgrade events

**Files:**
- Modify: `internal/workflow/service.go`
- Modify: `internal/workflow/service_test.go`
- Modify: `internal/workflow/install_test.go`
- Modify: `internal/app/app.go`

**Interfaces:**
- Adds: `workflow.Service.Events *observability.Emitter`.
- Consumes: job/media context attributes from Task 5, typed `domain.Score`, `domain.SyncResult`, `workflow.Result`, and `observability.SafeText/RelativePath`.
- Preserves: pure match/scoring/parser and LAPSE packages; they return typed evidence and do not acquire loggers.

- [ ] **Step 1: Write failing info-level workflow summary tests**

Reuse existing workflow fakes to cover embedded/protected sidecar satisfaction, no result, all-provider throttle, deterministic rejection, exact-hash bypass, score bypass, LAPSE solid install, same-candidate provenance refresh, upgrade wait, and technical failure. Assert:

- one `search.started` and one terminal `search.completed` per `Run`;
- `inventory.refresh_completed` includes track counts and satisfied reason without paths;
- `candidate.selected` includes provider/result ID, score, exact hash, selection mode, and no release filename;
- `lapse.analysis_completed` and `lapse.sync_completed` include phase, duration, verdict, mode, offset, ratio, confidence, agreement, coverage, parts, splits, and compatibility version;
- bypass paths emit no LAPSE subprocess event and identify `exact_hash` or `score_bypass` selection;
- `subtitle.installed`, `subtitle.provenance_refreshed`, and `upgrade.scheduled` reflect committed typed results;
- technical errors emit one owning failure event, while `search.completed` supplies a nonduplicating outcome/reason summary.

- [ ] **Step 2: Write failing debug scoring/privacy tests**

At `info`, assert candidate release names, filenames, rejected reasons, score contributions, cache detail, and every absolute path sentinel are absent. At `debug`, assert one `candidate.evaluated` record per result with:

```json
{
  "provider": "opensubtitles",
  "candidate_id": "8602117",
  "score": 57,
  "eligible": true,
  "score_components": [
    {"signal":"external_id","points":20,"reason":"..."}
  ],
  "rejected_reasons": [],
  "relative_path": "Series/Season 01/Episode.mkv"
}
```

Normalize every reason/release/file value with `SafeText`. Add a symlink-escape input and prove `relative_path` is omitted. Test shortlist tier, tie-break, candidate rejection, fallback, and early-stop debug events using the existing tournament fixtures.

- [ ] **Step 3: Run focused workflow tests and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -run 'Event|Logging|Tournament|Bypass|Upgrade' -count=1 -v
```

Expected: FAIL because workflow currently returns decisions but emits no lifecycle or scoring events.

- [ ] **Step 4: Add one terminal workflow summary path**

At the start of `Run`, derive a workflow emitter and record start time. Use a named return/deferred summary only if it can reliably see the final typed result and error; otherwise funnel all exits through a small `finish(result, err)` helper. Do not change business branches merely to log.

The terminal summary vocabulary is exactly the existing outcomes plus `failed` and `canceled`. Include candidate count, provider error count, score, retry/upgrade timestamps when present, and duration. Errors use explicit kinds (`inventory`, `provider_search`, `candidate_download`, `lapse_analysis`, `lapse_sync`, `installation`, `persistence`, `canceled`) selected at the branch that owns them.

- [ ] **Step 5: Log scoring and tournament decisions at debug**

Immediately after each `s.evaluate`, emit a trusted, explicitly built score representation rather than reflecting the entire candidate:

```go
components := make([]map[string]any, 0, len(score.Contributions))
for _, item := range score.Contributions {
	components = append(components, map[string]any{
		"signal": item.Signal,
		"points": item.Points,
		"reason": observability.SafeText(item.Reason),
	})
}
events.Log(ctx, slog.LevelDebug, "candidate.evaluated", "subtitle candidate evaluated",
	slog.String("provider", candidate.ProviderID),
	slog.String("candidate_id", candidate.ResultID),
	slog.Int("score", score.Total),
	slog.Bool("eligible", match.Eligible(score, s.minimumScore())),
	slog.Any("score_components", components),
	slog.Any("rejected_reasons", safeStrings(score.RejectedReasons)),
	slog.Any("release_names", safeStrings(candidate.ReleaseNames)),
)
```

Implement `safeStrings` in `internal/workflow/service.go` as a local helper that applies `observability.SafeText`, omits empty results, and returns at most eight entries. It is used only for the two explicitly approved diagnostic fields above; it must never receive URLs, download references, paths, or arbitrary provider payloads.

Emit structured debug events directly at tier, rejection, fallback, and early-stop branches instead of logging free-form `Decision.Reason` at info. Keep the existing `Result.Decisions` unchanged for CLI explanation and tests.

- [ ] **Step 6: Time typed LAPSE and installation boundaries**

Time `AnalyzeCandidate`, `SynchronizeCandidate`, `UpdateInstallationAssessment`, and `Installer.Install` calls in the workflow. On success, derive fields only from `domain.SyncResult` or `store.Installation`. On deterministic non-solid results emit `warn` with stable reason/verdict; on technical errors emit a sanitized owning error event. Never emit command arguments, output paths, raw subprocess output, or full checksums; if checksum correlation is retained, cap it to a documented 12-character prefix.

Wire one workflow-scoped emitter into every language service in `app.New`. Do not add logging to pure `internal/match`, low-level `syncer`, or installer implementations; the workflow boundary prevents duplicate events.

- [ ] **Step 7: Run workflow and end-to-end behavior tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow ./internal/app -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: PASS with exact-hash, LAPSE tournament, install rollback, and Silo behavior unchanged.

- [ ] **Step 8: Commit workflow observability**

```bash
git add internal/workflow internal/app/app.go
git commit -m "feat: log subtitle workflow decisions"
```

### Task 8: Operator documentation, Alloy contract, and final verification

**Files:**
- Modify: `AGENTS.md`
- Modify: `README.md`
- Modify: `docs/operations.md`
- Modify: `docs/implementation-status.md`
- Test: existing documentation/configuration/Compose tests discovered with `go test ./...`

**Interfaces:**
- Consumes: final event names and fields from Tasks 2–7.
- Produces: copyable Alloy Docker discovery/relabel/process/write example and bounded-label LogQL queries.

- [ ] **Step 1: Add a failing documentation contract test**

Add `TestLoggingDocumentationContract` to `internal/app/app_test.go`. It must read `../../README.md` and `../../docs/operations.md` and require:

```go
required := []string{
	"SUBSYNCD_LOG_LEVEL", "logging:", "level: info",
	"service", "environment", "level", "component", "event",
	"job_id", "media_id", "duration_ms",
	"loki.process", "loki.write",
}
```

Also reject documentation that promotes `job_id`, `media_id`, `candidate_id`, `file_id`, `language`, or `provider` as Loki labels, or suggests putting Loki credentials into `subsyncd` configuration.

- [ ] **Step 2: Run the documentation contract and confirm red**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/app -race -run 'TestLoggingDocumentationContract' -count=1 -v
```

Expected: FAIL because no logging configuration or Alloy guidance exists.

- [ ] **Step 3: Document runtime behavior and configuration**

Add a concise README feature/config note: JSON stderr, default `info`, YAML setting, environment precedence, restart requirement, debug privacy behavior, and Alloy ownership. Keep the description focused on subsyncd.

Add the approved logging spec and plan to `AGENTS.md`'s required reading list before future agents change logging behavior.

In `docs/operations.md`, document the level matrix, required/common event fields, correlation workflow, path/secret guarantees, readiness silence, and temporary debug procedure. Include a Docker-oriented Alloy example using the deployment's actual Alloy syntax, selecting the `subsyncd` container before JSON parsing and setting these labels only:

```text
service="subsyncd"
environment="hades"
level=<parsed level>
component=<parsed component>
event=<parsed event>
```

The pipeline must collect both Docker streams and leave the original JSON line queryable. Keep Loki endpoint credentials in Alloy's `loki.write` block, never in `subsyncd`.

- [ ] **Step 4: Add practical LogQL examples**

Include working examples for:

```logql
{service="subsyncd", environment="hades", event="job.completed"} | json | outcome="failed"
{service="subsyncd", environment="hades", event=~"provider.(cooldown_started|circuit_opened|auth_disabled)"} | json
{service="subsyncd", environment="hades", event=~"lapse.(analysis_completed|sync_completed)"} | json | duration_ms > 600000
{service="subsyncd", environment="hades", event="candidate.selected"} | json
{service="subsyncd", environment="hades"} | json | job_id="JOB_ID"
```

Validate the regex escaping and numeric filters against the documented Loki version used on Hades; if the installed version requires a different valid syntax, document that verified syntax exactly.

- [ ] **Step 5: Run documentation and focused privacy tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -run 'LoggingDocumentation|Redact|Privacy|RelativePath|Probe|Readiness' -count=1
```

Expected: PASS with no secret/path fixture values in captured logs.

- [ ] **Step 6: Run the complete verification gate**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
git diff --check
docker compose -f compose.example.yml config --quiet
```

Expected: every command exits zero. Ordinary and end-to-end tests use only local fakes/fixtures and do not contact Arr, providers, Silo, Alloy, or Loki.

- [ ] **Step 7: Audit a representative log corpus**

Run buffer-backed unit fixtures at both `info` and `debug`, then search the captured test artifact for fixture secrets, absolute media/data/temp roots, `token=`, provider base URLs, raw LAPSE stderr, and newline-split records. The test must fail on any match. Decode every remaining line as exactly one JSON object and assert the required common fields.

- [ ] **Step 8: Update the implementation ledger and commit documentation**

Record every implementation commit, adopted event/schema behavior, tests run, and the next Hades canary step in `docs/implementation-status.md`.

```bash
git add AGENTS.md README.md docs/operations.md docs/implementation-status.md internal/app/app_test.go
git commit -m "docs: operate structured Loki logging"
```

- [ ] **Step 9: Perform the controlled Hades log rollout after publication approval**

This step is operational and requires the user's separate deployment approval. Publish the verified immutable image through the existing GitHub workflow, pin Hades to its SHA tag, keep `logging.level: info`, and restart only the `subsyncd` canary/service. Verify:

```text
doctor/startup remains healthy
Alloy parses one JSON event per line
labels are only service/environment/level/component/event plus existing infrastructure labels
successful health/readiness probes produce no records
one bounded manual or scheduled workflow has a complete job_id trail
no absolute paths, tokens, credentials, URLs, provider bodies, or raw LAPSE output appear
```

Do not enable debug permanently. If diagnostic detail is needed, set `SUBSYNCD_LOG_LEVEL=debug`, restart for a bounded test, inspect the root-relative/candidate events, then remove the override and restart at `info`.
