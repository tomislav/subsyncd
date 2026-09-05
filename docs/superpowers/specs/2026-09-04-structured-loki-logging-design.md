# Structured Loki Logging Design

**Status:** Approved for implementation

## Purpose

Make `subsyncd` operationally observable through comprehensive, stable, machine-queryable logs without adding a direct Loki dependency. The service emits one JSON object per line and Grafana Alloy collects the container streams, adds deployment labels, and forwards them to Loki.

The design favors useful workflow summaries at `info`, detailed diagnostic evidence at `debug`, and stable event names and field types that dashboards and alerts can safely query. It must not expose credentials, upstream response bodies, absolute host paths, or subtitle content.

This phase adds logs only. A Prometheus endpoint, distributed tracing, and direct log shipping are out of scope.

## Alternatives considered

The selected approach adds a small internal event layer on top of Go's `log/slog`. The layer owns event names, common fields, severity policy, normalization, and privacy rules. Components receive a scoped logger/event emitter and remain responsible for recording events at their own lifecycle boundaries.

Adding uncoordinated `slog` calls directly to every package would be quicker initially, but fields, levels, and redaction would drift. Loki queries would then depend on human message text instead of a stable event contract.

OpenTelemetry logging and direct Loki push were also considered. Neither is justified for a single local service whose container output is already collected by Alloy. They would add exporters, buffering, shutdown, retry, and configuration behavior that duplicates the collector.

## Output contract

All runtime application logs are newline-delimited JSON written to standard error. Each physical line is one complete JSON object. Messages, errors, and field values must not contain unescaped line breaks that create additional records.

Every event contains:

```text
time        RFC 3339 timestamp emitted by slog
level       debug, info, warn, or error
msg         short human-readable summary
event       stable machine-readable event name
service     always "subsyncd"
version     build version, or "dev" when unavailable
component   bounded subsystem name
```

The `event` field is the query contract. `msg` may improve over time without being treated as an API. Field names use lower snake case. Durations use integer milliseconds with a `_ms` suffix, byte counts use a `_bytes` suffix, timestamps use RFC 3339 strings with an `_at` suffix, and counts are JSON integers.

CLI usage diagnostics may retain the existing concise human-readable stderr contract because they run before application logging is initialized. Once configuration and logging initialize, operational events and backend failures use the JSON event contract. The daemon must never interleave plain-text application logs with its JSON stream.

## Configuration

Add an optional top-level configuration section:

```yaml
logging:
  level: info
```

`logging.level` accepts `debug`, `info`, `warn`, or `error`, case-insensitively, and defaults to `info`. An empty value is equivalent to omission. Any other value fails strict configuration validation.

`SUBSYNCD_LOG_LEVEL`, when non-empty, overrides the YAML value. The same four values are accepted. The level is resolved at process startup and changing either source requires a restart. JSON format and stderr destination are fixed rather than configurable.

Alloy supplies deployment-specific labels such as environment or host. `subsyncd` does not need deployment knowledge in its application configuration.

## Loki label policy

The application emits fields; Alloy decides which become Loki labels. The documented Alloy example should promote only bounded values:

```text
service environment level component event
```

`environment` is an Alloy static label rather than an application field. Stable event names form a finite set and are safe to label. Dynamic values—including request, job, media, file, candidate, provider-instance, and language identifiers—remain parsed JSON fields. This avoids high-cardinality streams while preserving precise LogQL filtering.

## Correlation and common fields

Components attach only fields they know authoritatively. Common correlation fields are:

```text
request_id provider instance job_id media_id media_kind file_id language
candidate_id attempt priority queue_trigger outcome reason error_kind
duration_ms retry_at next_upgrade_at candidate_count score exact_hash
sync_result sync_mode confidence installation_id notification_id
```

The worker's persisted job ID is the primary correlation value for background acquisition. It is propagated through the workflow, provider coordinator, LAPSE, installer, and notification-enqueue events. HTTP request IDs cover webhook processing. A webhook that durably schedules jobs records the affected media IDs or a bounded count; it does not invent job IDs before they exist.

Fields must retain one JSON type across events. Optional evidence is omitted rather than represented by inconsistent empty strings, zeroes, or stringified objects. Lists included at `info` must be bounded; per-candidate or per-file detail belongs in individual `debug` events.

## Privacy and redaction

Secrets are never structured fields. This includes Arr API keys, webhook tokens, Silo API keys, provider credentials and bearer tokens, signed download URLs, cookies, request/response authorization headers, and configuration environment values.

Absolute media, subtitle, data, cache, and temporary paths are never logged at any level. At `debug`, a component may emit a root-relative media or sidecar path only after containment is proven against a configured media root. The normalized field is `relative_path`; its associated configured mapping may be identified by a non-secret bounded root name or instance. If containment or safe relativization fails, omit the path.

Provider URLs, query strings, response bodies, subtitle content, archive member listings, and raw LAPSE stdout/stderr are never logged. Candidate release names and subtitle filenames are diagnostic data and may appear only at `debug`; they must be normalized to single-line bounded strings.

Errors pass through one central sanitizer before emission. It applies existing configuration-aware credential and media-root redaction, bounds the message, removes line breaks, and records a stable `error_kind` separately from the sanitized human `error`. Code should prefer typed reason/error categories over parsing error strings.

The event layer rejects or safely stringifies unsupported structured values. It must never reflect arbitrary configuration structs, HTTP requests, provider payloads, or domain objects into logs.

## Severity policy

`debug` records evidence needed for deep diagnosis but too verbose for continuous retention:

- provider request attempts and cache hit/miss decisions;
- every candidate's score total, individual score components, gates, and rejection reasons;
- candidate release/file evidence and safe root-relative paths;
- shortlist tier construction, tie-breaking, score-bypass eligibility, and early stopping;
- detailed LAPSE analysis/finalization decisions without raw subprocess output;
- lease renewal and coalesced wake details.

`info` records normal state changes and one summary per meaningful unit of work:

- service startup, ready state, shutdown request, and completed shutdown;
- accepted webhooks and their durable scheduling result;
- reconciliation, inventory, and sidecar scan summaries;
- job lease/start/completion and final outcome;
- provider search completion summaries and the selected candidate;
- LAPSE completion outcome and timings;
- subtitle installation or same-candidate provenance refresh;
- upgrade scheduling and notification delivery;
- provider recovery and readiness recovery transitions.

`warn` records expected but actionable degraded operation:

- readiness becoming unhealthy;
- provider quota, rate limit, transient circuit, or authentication-disable transitions;
- provider search/download failure when other providers may still succeed;
- unusable downloads, deterministic candidate rejection, or LAPSE non-solid result;
- technical retry scheduling and retryable notification failure;
- lease loss, stale completion, or bounded shutdown cancellation.

`error` is reserved for unexpected failure of an operation or invariant where useful work did not complete, including unrecoverable startup, database, dispatch, workflow, installation, or shutdown errors. Expected missing-subtitle results, provider cooldowns, candidate mismatches, and LAPSE uncertainty are not errors.

The same failure is logged once at the owning boundary. Callers add correlation and final outcome rather than repeating an identical stack of error messages at every layer.

## Event ownership and taxonomy

Stable event names use dotted lowercase namespaces. The initial contract includes the following families; implementation may add a more specific member only when it follows the same ownership and field rules.

### Application and health

```text
service.starting
service.ready
service.shutdown_requested
service.stopped
readiness.unhealthy
readiness.recovered
```

Successful `/healthz` and `/readyz` requests are silent. The first successful readiness check establishes healthy state without logging. The first failed check emits `readiness.unhealthy`; repeated identical failures remain silent at `info` and may emit bounded `debug` diagnostics. A later success emits `readiness.recovered`. HTTP access logging is not added for probe endpoints.

### HTTP and catalog

```text
webhook.rejected
webhook.accepted
webhook.applied
reconcile.started
reconcile.completed
reconcile.failed
inventory.refresh_completed
```

Authentication failures remain generic and never identify which token check failed. Webhook events include `request_id`, method, query-free path, instance when safely resolved, result, affected count, and duration. Successful general-purpose HTTP request access logs are unnecessary because the service exposes only probes and webhooks; the webhook lifecycle events are authoritative.

### Queue and workflow

```text
job.leased
job.started
job.rerun_requested
job.completed
job.retry_scheduled
job.lease_lost
search.started
search.completed
candidate.evaluated
candidate.rejected
candidate.selected
upgrade.scheduled
```

`job.completed` is the single authoritative outcome event and includes duration plus `outcome`, `reason`, attempts, and next schedule when applicable. `search.completed` summarizes provider participation, candidate counts, cache use, and duration without expanding all candidates. Candidate events are `debug`, except the selected candidate may have an `info` event containing stable identity, score, provider, hash status, and decision—never its filename or release text.

### Providers

```text
provider.search_started
provider.search_completed
provider.download_completed
provider.cooldown_started
provider.circuit_opened
provider.recovered
provider.auth_disabled
```

Provider state-transition events are emitted only when persisted state actually changes, not on every locally suppressed request. A suppressed request is visible through the workflow/search summary and a `debug` decision. Events distinguish configured provider instance from adapter type when both are useful. Quota/cooldown reset timestamps and transient attempt are safe structured fields.

### LAPSE and installation

```text
lapse.analysis_started
lapse.analysis_completed
lapse.sync_started
lapse.sync_completed
lapse.failed
subtitle.installed
subtitle.provenance_refreshed
subtitle.install_failed
```

LAPSE start events are emitted at `info` immediately before analysis or synchronization and include only correlation, phase, provider, candidate, and compatibility version. Completion and failure events add duration and the bounded result fields that exist after execution: verdict, mode, confidence, offset, ratio, agreement, coverage, and part/split counts. They never include command lines or raw output. Installation logs include media/language, managed replacement state, checksum prefix if useful for correlation, duration, and safe outcome—not absolute destination or rollback paths.

### Notifications

```text
notification.queued
notification.delivered
notification.retry_scheduled
notification.failed
```

Notification events use durable notification/job identities, notifier type/instance, attempt, duration, outcome, and retry time. They never log the Silo endpoint, bearer key, or mapped media path.

## Instrumentation architecture

Add an internal observability package that constructs the configured JSON `slog.Logger`, supplies fixed service/version/component attributes, sanitizes errors and optional relative paths, and exposes stable event constants or narrow component helpers. It must remain a lightweight local abstraction, not a second asynchronous logging system. `slog` retains responsibility for serialization and synchronized writes.

Pass scoped loggers or event emitters through constructors. Context-aware operations attach correlation attributes once and derive child loggers for deeper layers. Do not use a mutable global logger, and do not add logging parameters to pure scoring/parser functions solely to produce events. Those functions should continue returning structured decisions; the workflow boundary logs them.

Ownership boundaries are:

- app: configuration-resolved lifecycle, readiness transitions, and reconciliation summaries;
- HTTP server: webhook rejection, acceptance, application, request correlation, and duration;
- worker: leasing, job lifecycle, retries, priority, reruns, and shutdown behavior;
- provider coordinator/transport: search/download summaries and persisted cooldown/circuit/auth transitions;
- workflow: inventory outcome, candidate scoring summaries/details, selection, LAPSE decisions, upgrade decisions, and final acquisition result;
- syncer/installer: subprocess and filesystem operation outcomes returned to the workflow without duplicate logging;
- notification worker: durable enqueue/delivery/retry lifecycle.

Library constructors used by tests may accept a nil logger and replace it with a discard handler. Production assembly always supplies the configured logger. Tests must not depend on wall-clock timestamps or JSON object field order.

## Startup, shutdown, and failures

Logging initializes early enough to report configuration loading failures when a valid log level can be resolved. An invalid YAML or environment log level still produces a bounded diagnostic and the existing operation-failure exit code; it must not print configuration contents.

`service.starting` is emitted after configuration is valid and includes version, command, configured instance/language/provider counts, worker concurrency, and non-secret feature flags. `service.ready` follows successful offline startup validation. Shutdown records the trigger category (`signal`, `context`, or component failure), drain outcome, active work count, and duration without exposing signal-handler internals.

Panics are not converted into success. If a narrow recovery boundary already exists, it logs one sanitized `error` event and preserves the existing failure semantics. This design does not introduce broad panic recovery.

## Documentation and Alloy example

The README receives a concise feature/configuration note. `docs/operations.md` documents levels, privacy behavior, correlation fields, event querying, and an Alloy-to-Loki example appropriate for Docker logs. The example must:

- collect both stdout and stderr container streams;
- parse JSON without discarding malformed lines from unrelated containers;
- select the `subsyncd` container before promoting fields;
- add a static deployment `environment` label;
- promote only the bounded label set;
- leave IDs and other dynamic fields in the JSON payload;
- avoid requiring a direct Loki URL or credentials in `subsyncd` configuration.

Example LogQL should cover failed jobs, provider cooldown transitions, LAPSE duration, selected candidates, and one media/job correlation trail.

## Testing and acceptance

Tests must demonstrate:

- omitted logging configuration resolves to `info`;
- YAML accepts all four levels and rejects unknown values;
- `SUBSYNCD_LOG_LEVEL` overrides YAML and rejects unknown values without exposing environment contents;
- each emitted physical line is valid JSON with the required common fields;
- filtering suppresses lower-severity events;
- stable fields retain their intended JSON types;
- candidate scoring components and root-relative paths appear only at `debug`;
- absolute paths, media roots, credentials, query strings, provider URLs, upstream bodies, raw LAPSE output, and multiline errors never appear at any level;
- unsafe/non-contained paths are omitted rather than relativized;
- successful health and readiness probes are silent;
- readiness logs only unhealthy and recovered transitions at normal levels;
- webhook, job, provider, search, selected-candidate, LAPSE, install, upgrade, reconciliation, and notification lifecycles emit the documented correlation and outcome fields;
- provider cooldown/circuit/auth events occur on persisted state transitions rather than every suppressed request;
- one underlying failure does not produce duplicate error events at adjacent ownership boundaries;
- daemon shutdown flushes synchronous log writes and preserves current bounded-drain behavior;
- the documented Alloy pipeline parses representative records and promotes only approved labels.

Use table-driven unit tests with an in-memory handler or buffer and sanitized fixtures. Workflow and worker lifecycle tests should exercise existing fakes; ordinary tests must not contact providers, Arr, Silo, Alloy, or Loki. Golden records should normalize or remove timestamps and durations rather than weakening the runtime schema.

Before completion, run the affected packages with `-race`, the complete `go test ./... -race`, `go vet ./...`, tagged end-to-end tests, the relevant configuration/Compose documentation checks, and `git diff --check`.

## Rollout

The default `info` output is intended to be safe for Hades without a temporary debug deployment. First deploy at `info` and verify JSON parsing, bounded labels, lifecycle coverage, and absence of secrets or absolute paths in Alloy/Loki. Enable `debug` only for a time-bounded diagnostic window, then restart at `info`.

Existing provider, scoring, queue, synchronization, installation, and notification behavior must remain unchanged. Observability failures must never block a subtitle workflow; the synchronous standard-library handler is treated as best-effort process output and does not add retries or persistence.

## Non-goals

- Prometheus metrics or a `/metrics` endpoint.
- OpenTelemetry traces, spans, or log exporters.
- Direct Loki clients, credentials, buffering, retry, or backpressure.
- Per-request access logs for successful health/readiness probes.
- Runtime log-level reload or an administrative logging endpoint.
- Text/console log formatting or configurable output destinations.
- Logging subtitle contents, raw provider payloads, raw LAPSE output, or full filesystem paths.
- Changing provider selection, scoring, backoff, queueing, LAPSE, installation, or Silo behavior.
