# Logging internals

The structured event contract: record shape, correlation and privacy rules, and a catalogue of every event the code emits. For reading logs and choosing a level as an operator, see the [logging guide](../logging.md).

- [Record shape](#record-shape)
- [Correlation fields](#correlation-fields)
- [Levels](#levels)
- [Privacy](#privacy)
- [Conventions for new events](#conventions-for-new-events)
- [Event catalogue](#event-catalogue)

## Record shape

subsyncd writes one JSON object per line to stderr, synchronously (`internal/observability`). Every record has:

| Field | Value |
| --- | --- |
| `time` | RFC 3339 timestamp |
| `service` | `subsyncd` |
| `version` | Build version (`internal/version`) |
| `level` | `debug`, `info`, `warn` or `error` |
| `component` | The emitter's component (`Emitter.For`): `app`, `control`, `http`, `catalog`, `worker`, `workflow`, `provider` or `store` |
| `event` | A stable dotted name, catalogued below |
| `msg` | A short human sentence; never parse it |

Events are emitted with `Emitter.Log(ctx, level, event, message, attrs...)`. A record with an empty event or component is dropped. Explicit attributes override context attributes with the same key; the reserved keys above cannot be overridden.

Failures add `error_kind` (a short category) and `error` through `Emitter.ErrorAttrs`, which runs the error text through the configuration-aware redactor (`redactedError` in `internal/app/app.go`) and `SafeText`.

## Correlation fields

`observability.WithAttrs` puts these in the context, so every record of one operation carries them:

| Operation | Fields |
| --- | --- |
| Worker search job | `job_id`, `media_id`, `language`, `priority`, `attempt`, `failure_attempt`; after the media loads `instance`, `media_kind`, `file_id`, `media_title` |
| Manual search | `job_id` (`manual-<hex>`), `media_id`, `language`, `priority=manual`, `instance`, `media_kind`, `file_id`, `media_title` |
| Pack member preparation | `member_index`, `member_count` (local to the current preparation group) |
| Notification delivery | `notification_id`, `notifier`, `attempt`; `arr_rescan` adds `instance`, `media_kind` and `series_id` (Sonarr) or `movie_id` (Radarr) |
| HTTP requests | `request_id`, `method`, `path` (added by `httpapi.Server.log`) |

Follow one search by filtering on `job_id`. Common operational fields are `outcome`, `reason`, `duration_ms`, `retry_at`, `next_upgrade_at`, `provider` and `candidate_id`.

### Media titles

`media_title` comes from `observability.MediaTitle`:

- movies: `Movie (Year)`, or the title alone without a year;
- episodes: `Show - S01E02 - Episode Title`; a multi-episode range adds `-E03`: `Show - S01E02-E03 - Episode Title`. The episode title is omitted when unknown.

The value passes through `SafeText`: invalid UTF-8 becomes U+FFFD, control characters and line separators become spaces, whitespace collapses to single spaces, and the text is bounded to 2,048 Unicode code points. The same helper fills `search_history.media_title`.

## Levels

| Level | Intended records |
| --- | --- |
| `info` | Service, webhook, durable job, search, archive selection, candidate rejection and retained skip, selected candidate, install and provenance, notification, reconciliation and recovery lifecycle summaries |
| `warn` | Expected degraded states: throttling, provider cooldown, circuit and authentication transitions, non-solid LAPSE decisions, readiness loss, bounded drain timeouts, deferred work |
| `error` | Technical failures that need a retry or operator attention |
| `debug` | Score components, release-name diagnostics, cache decisions, tournament and fallback decisions, and media paths only when safely root-relative |

`logging.level` (default `info`) and `SUBSYNCD_LOG_LEVEL` (which wins) are read at startup only.

Successful `/healthz` and `/readyz` requests are silent; readiness logs only the transition to unhealthy and the recovery. A cooldown found at the final transport gate logs its provider completion at debug, not warn (see [Limits](providers/limits.md#preflight-and-the-final-gate)).

## Privacy

Credentials, bearer and API keys, webhook tokens, provider URLs and response bodies, notification payloads, absolute media/data/temp paths, command arguments and command lines, socket paths, and raw LAPSE stdout/stderr are never intentional log fields.

- Error text passes through the redactor, which replaces instance API keys, webhook tokens, the Silo key and provider settings whose key contains `password`, `api_key`, `token` or `username` with `[redacted]`; `data_dir` and the LAPSE path with `[path]`; media roots with `[media]`; and the temporary directory with `[temp]`. Text is normalized to one line.
- Filesystem errors carry only their cause (`observability.PathErrorCause`), never the path the operating system attaches, because redaction replaces configured roots but not the media path below them.
- Webhook paths are logged without query strings.
- Debug paths are logged only relative to a media root; when containment cannot be proven, even the relative path is omitted.
- `http.server_error` keeps only the first line of Go's message, so panic stack traces are never logged.
- The one deliberate exception for provider text is `provider.response_unrecognized`: its optional `excerpt` carries at most 80 runes of an unrecognized SubDL message, reduced to letters, digits and `.,:;'!?()-_%`, single-line, with the configured API key and credential-looking tokens replaced by `[redacted]` and a cut marked with `…`. Event fields bypass error redaction, so the SubDL client redacts the excerpt itself, and the text never enters an error message (whose wording `workflowErrorKind` uses to classify failures).
- Archive member filenames, subtitle paths and payloads are never logged; candidate IDs pass through `SafeText`, and checksums appear only as 12-character `checksum_prefix` values.

## Conventions for new events

- Name events `area.past_tense_or_state` (`install.removed`, `queue.route_paused`), lowercase with underscores. Reuse an existing area.
- Pick the level by the table above; a recorded candidate rejection is info or warn, never error.
- Add context once with `WithAttrs` rather than repeating fields; use the field names already in this catalogue.
- Keep fields low-cardinality and bounded: counts, codes, IDs, `media_title`. Never log lists of provider IDs, route signatures or free text from providers.
- Add a row to the catalogue below in the same change. `TestLoggingDocumentationContract` (`internal/app/app_test.go`) checks that key events stay documented.

## Event catalogue

The tables list each event's level, component, the fields it adds beyond the record and correlation fields, and where it is emitted (paths relative to `internal/`).

### service and command

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `service.starting` | info | app | `command`, `instance_count`, `provider_count`, `language_count`, `max_concurrent`, `silo_enabled` | `app/app.go` |
| `service.start_failed` | error | app | `error_kind=startup` | `app/app.go` |
| `service.ready` | info | app | `command` | `app/app.go` |
| `service.shutdown_requested` | info | app | `trigger` (such as a signal or `component_failure`) | `app/app.go` |
| `service.stopped` | info | app | `outcome`, `duration_ms` | `app/app.go` |
| `command.failed` | error | app | `command`, `error_kind=command` | `app/app.go` (`ReportFailure`) |

### control

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `control.request_completed` | info | control | `command`, `outcome=success`, `duration_ms`; `search` and `reject` add `media_title`, `reject` also `language` | `app/control.go` |
| `control.request_failed` | warn | control | `command` (a known name or `unknown`), `code`, `error_kind=control` | `app/control.go` |

### http and readiness

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `http.server_error` | warn; error for `reason=panic` | http | `reason` (`panic`, `accept`, `tls_handshake`, `other`), `error_kind=http_server` | `observability/emitter.go` (`ServerErrorLog`) |
| `readiness.unhealthy` | warn | http | `status=503`, `error_kind=readiness` | `httpapi/server.go` |
| `readiness.recovered` | info | http | `status=200` | `httpapi/server.go` |

### webhook

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `webhook.accepted` | info | http | `instance`, `body_bytes`, `duration_ms` | `httpapi/server.go` |
| `webhook.queued` | info | http | `instance`, `event_count`, `status=202`, `duration_ms` | `httpapi/server.go` |
| `webhook.rejected` | warn | http | `instance`, `reason` (`unknown_instance`, `authentication`, `body_read`, `body_too_large`, `invalid_json`, `invalid_event`), `status`, `duration_ms` | `httpapi/server.go` |
| `webhook.applied` | info | http | `instance`, `outcome` (`applied`, `duplicate`, `ignored`), `event_count`, `applied_count`; HTTP adds `status=204`, the inbox adds `attempt`; `duration_ms` | `httpapi/server.go`, `catalog/webhook_inbox.go` |
| `webhook.failed` | warn for `retry_scheduled`; error for `abandoned`, `inbox_unavailable` and HTTP failures | http | `instance`, `outcome`, `retry_at`, `attempt`, `event_count`, `applied_count`, `duration_ms`, `error_kind` (`webhook_processing`, `webhook_inbox`) | `httpapi/server.go`, `catalog/webhook_inbox.go` |
| `webhook.inbox_orphaned` | warn | app | `instance`, `webhook_count` | `app/app.go` |
| `webhook.token_rotated` | info | app | `instance` (never a token) | `app/webhook_token.go` |
| `webhook.connection_created` | info | catalog | `instance`, `connection_id` | `catalog/webhook_connection.go` |
| `webhook.connection_adopted` | info | catalog | `instance`, `connection_id` | `catalog/webhook_connection.go` |
| `webhook.connection_repaired` | info | catalog | `instance`, `connection_id`, `changed` (subset of `url,method,events`) | `catalog/webhook_connection.go` |
| `webhook.connection_ok` | debug | catalog | `instance`, `connection_id` | `catalog/webhook_connection.go` |
| `webhook.connection_foreign` | info | catalog | `instance`, `count` | `catalog/webhook_connection.go` |
| `webhook.connection_ambiguous` | warn | catalog | `instance`, `count` | `catalog/webhook_connection.go` |
| `webhook.connection_sync_failed` | warn | catalog | `instance`, `reason` (`arr_unreachable`, `arr_status`, `arr_response_invalid`, `arr_test_rejected`, `arr_test_unreachable`, `name_unavailable`, `schema_missing_webhook`, `internal`), `status` (Arr's HTTP status or 0); `internal` adds `error_kind=webhook_connection_sync` | `catalog/webhook_connection_maintainer.go` |

Webhook URLs, tokens, Arr response bodies, and the fields of any connection other than the owned one's numeric ID are never logged.

### arr

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `arr.request_retry` | debug | catalog | `instance` | `catalog/arrapi_client.go` |
| `arr.request_retries_exhausted` | warn | catalog | `instance` | `catalog/arrapi_client.go` |

These come from the arrapi library's logger (`arrRetryHandler`); the name is chosen by its record level and its own attributes and message are discarded.

### reconcile, scan and instance

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `reconcile.started` | info | worker | `instance` | `worker/worker.go` |
| `reconcile.completed` | info | worker | `instance`, `outcome=success`, `duration_ms` | `worker/worker.go` |
| `reconcile.deferred` | warn | worker | `instance`, `deferred_count`, `deferred_media` (at most ten labels such as `Sonarr episode <id> (<media title>)`, never paths), `retry_at`, `duration_ms` | `worker/worker.go` |
| `reconcile.failed` | error | worker | `instance`, `attempt`, `retry_at`, `duration_ms`, `error_kind=reconciliation` | `worker/worker.go` |
| `reconcile.snapshot_deletions_withheld` | warn | catalog | `instance`, `media_kind`, `active_count`, `missing_count`, `snapshot_count` | `catalog/reconcile.go` |
| `reconcile.multi_episode_recheck_failed` | warn | catalog | `instance`, `file_id` when known, `error_kind` (`list`, `hydration`) | `catalog/reconcile.go` |
| `scan.completed` | info | app | `instance`, `media`, `deferred`, `probe_skipped`, `force_probe`, `duration_ms` | `app/scan.go` |
| `scan.failed` | warn (`outcome=failed`, `error_kind=scan`); info (`outcome=canceled` by shutdown) | app | `instance`, `force_probe`, `duration_ms`, `outcome` | `app/scan.go` |
| `instance.forgotten` | info | app | `instance`, `media_count`, `search_count`, `installation_count`, `rollback_copy_count`, `candidate_count`, `rejection_count`, `notification_count`, `event_count`, `replay_count`, `history_count` | `app/forget_instance.go` |

### queue, job and worker

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `queue.route_paused` | warn | worker | `language`, `media_kind`, `provider_count`, `reset_at` or `reason=disabled`, `episode_ranges_only` when true; no provider IDs | `worker/route_gate.go` |
| `queue.route_resumed` | info | worker | `language`, `media_kind`, `episode_ranges_only` when true | `worker/route_gate.go` |
| `queue.route_check_failed` | error | worker | `error_kind=route_check` | `worker/route_gate.go` |
| `queue.media_paused` | warn | worker | `failure_count`, `pause_ms`, `resume_at` | `worker/media_breaker.go` |
| `queue.media_file_unavailable` | info | worker | job context only | `worker/media_breaker.go` |
| `job.leased` | info | worker | `job_id`, `media_id`, `language`, `priority`, `attempt`, `failure_attempt`, `resume_provider_count` | `worker/worker.go` |
| `job.started` | info | worker; app for manual searches | job context | `worker/worker.go`, `app/manual_search.go` |
| `job.completed` | info; error on failure | worker; app for manual searches | `outcome`, `reason`, `duration_ms`, `resume_provider_count`, `next_attempt_at`, `release_age_days` and `schedule=aged` when they apply; failures add `error_kind` (`manual_search` for manual runs) | `worker/worker.go`, `app/manual_search.go` |
| `job.retry_scheduled` | warn | worker | `reason=workflow_error`, `retry_at`, `next_failure_attempt` | `worker/worker.go` |
| `job.rerun_requested` | info | worker | `priority` | `worker/worker.go` |
| `job.timed_out` | warn | worker; app for manual searches | `limit` (worker), `priority=manual` (app) | `worker/worker.go`, `app/manual_search.go` |
| `job.lease_lost` | warn | worker | `error_kind=lease_renewal` | `worker/worker.go` |
| `worker.drain_timed_out` | warn | worker | `active_searches`, `maintenance_active` | `worker/worker.go` |
| `worker.drain_abandoned` | warn | worker | `active_searches`, `maintenance_active` | `worker/worker.go` |

### search, inventory and upgrade

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `search.started` | info | workflow | `media_id`, `media_kind`, `file_id`, `language`, `manual` | `workflow/service.go` |
| `search.phase_completed` | debug | workflow | `search_mode`, `candidate_count`, `provider_error_count` | `workflow/service.go` |
| `search.release_numbered` | info | workflow | `release_season`, `release_episode`, `candidate_count`, `provider_error_count`: the broad search repeated in the release's own numbering | `workflow/release_numbering.go` |
| `search.completed` | info; warn for canceled, `removal_deferred` and `provider_unavailable`; error for failures such as `lapse_timeout` | workflow | `outcome`, `reason`, `candidate_count`, `provider_error_count`, `score`, `duration_ms`, `retry_at`, `next_upgrade_at` | `workflow/service.go` |
| `search.resume_reset` | warn | store | `media_id`, `language` | `store/repository.go` |
| `inventory.refresh_completed` | info | workflow | `track_count`, `embedded_count`, `sidecar_count`, `satisfied` | `workflow/service.go` |
| `inventory.refresh_failed` | error | workflow | `error_kind=inventory` | `workflow/service.go` |
| `upgrade.scheduled` | info | workflow | `next_upgrade_at`, `score`, `exact_hash` | `workflow/service.go` |

### provider

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `provider.search_started` | info | provider | `provider`, `search_mode`, `language` | `provider/coordinator.go` |
| `provider.search_skipped` | debug | provider | `provider`, `search_mode`, `language`, `outcome`, `reason` (`download_unavailable`, `search_unavailable`) | `provider/coordinator.go` |
| `provider.cache_hit` | debug | provider | `provider`, `search_mode`, `language` | `provider/coordinator.go` |
| `provider.cache_miss` | debug | provider | `provider`, `search_mode`, `language` | `provider/coordinator.go` |
| `provider.search_completed` | info; debug when locally suppressed; warn for cooldown, quota, disabled and canceled; error otherwise | provider | `provider`, `search_mode`, `language`, `outcome`, `cache_status`, `candidate_count`, `duration_ms`, `error_kind` on failure | `provider/coordinator.go` |
| `provider.download_completed` | as `provider.search_completed` | provider | `provider`, `candidate_id`, `outcome`, `bytes`, `duration_ms`, `error_kind` on failure | `provider/observed.go` |
| `provider.download_details` | debug | provider | `provider`, `candidate_id`, `filename` | `provider/observed.go` |
| `provider.download_skipped` | debug | workflow | `provider`, `reason=download_unavailable` | `workflow/service.go` |
| `provider.auth_disabled` | warn | provider | `provider`, `scope`, `provider_type`, `reason=authentication_failed` | `provider/limiter.go` |
| `provider.cooldown_started` | warn | provider | `provider`, `scope`, `provider_type`, `reason`, `reset_at` | `provider/limiter.go` |
| `provider.circuit_opened` | warn | provider | `provider`, `scope`, `provider_type`, `reason`, `attempt`, `reset_at` | `provider/limiter.go` |
| `provider.recovered` | info | provider | `provider`, `scope`, `provider_type`, `reason=transient_cleared`, `previous_attempt` | `provider/limiter.go` |
| `provider.cooldown_save_failed` | warn | provider | `provider`, `scope`, `provider_type`, `reason`, `error_kind=provider_state` | `provider/limiter.go` |
| `provider.response_unrecognized` | warn | provider | `provider`, `operation=search`, `excerpt` (see [Privacy](#privacy)) | `provider/subdl/client.go` |

### candidate, archive and pack cache

`candidate.decision`, `.skipped`, `.rejection_details`, `.tier_started`, `.fallback`, `.early_accepted` and `.early_stopped` come from `logWorkflowDecision` (`workflow/service.go`), which picks the name from the decision stage. They carry `stage`, `provider` and `candidate_id` when set, and, with a reason code, `reason_code`, `selection_rule`, `archive_type`, `subtitle_member_count` and `matching_member_count`.

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `candidate.evaluated` | debug | workflow | `provider`, `candidate_id`, `score`, `eligible`, `score_components`, `rejected_reasons`, `release_names` (at most 8), `relative_path` when under a media root | `workflow/service.go` |
| `candidate.decision` | debug | workflow | `stage` (any stage not listed below) | `workflow/service.go` |
| `candidate.skipped` | info | workflow | `stage=candidate_rejection`, `reason_code` (a stored code or `retained_rejection`) | `workflow/service.go` |
| `candidate.rejection_details` | debug | workflow | `stage=candidate` | `workflow/service.go` |
| `candidate.tier_started` | debug | workflow | `stage=tournament_tier` | `workflow/service.go` |
| `candidate.fallback` | debug | workflow | `stage=fallback` | `workflow/service.go` |
| `candidate.early_accepted` | info | workflow | `provider`, `candidate_id`, `confidence`, `skipped_count` | `workflow/service.go` |
| `candidate.early_stopped` | debug | workflow | `provider`, `candidate_id` | `workflow/service.go` |
| `candidate.selected` | info | workflow | `provider`, `candidate_id`, `score`, `exact_hash`, `selection_mode` (`exact_hash`, `score_bypass`, `sync_disabled`, `lapse`) | `workflow/service.go` |
| `candidate.rejected` | info | workflow; app for `reject` | `provider`, `candidate_id`, `reason_code` (`user_<reason>` from `reject`), selection fields when a rule applies; `reject` adds `media_id`, `language` | `workflow/member_preparation.go`, `app/reject.go` |
| `archive.classified` | debug | workflow | `archive_type`, `subtitle_member_count`, `classification` (`single`, `provider_pack`, `runtime_pack`) | `workflow/service.go` |
| `archive.members_selected` | info | workflow | `provider`, `candidate_id`, `selection_rule`, `archive_type`, `matching_member_count`, `subtitle_member_count` | `workflow/service.go` |
| `pack_cache.lookup` | debug | workflow | `cache_outcome` (`hit`, `miss`, `error`), `runtime_pack` | `workflow/service.go` |
| `pack_cache.publication` | debug | workflow | `cache_outcome` (`published`, `disabled`, `unsafe_identity`, `write_failed`, …) | `workflow/service.go` |

### lapse

`lapse.sync_started` and `lapse.sync_completed` are built as `"lapse." + phase + "_started"` / `"_completed"`; `sync` is the only phase. Completion and verdict `lapse.failed` records carry LAPSE's `verdict`, `mode`, `reference` (`embedded`, `vad`, …), `offset_ms`, `ratio`, `confidence`, `sigma`, `margin`, `agreement`, `coverage`, `parts` and `splits`, plus `phase`, `provider`, `candidate_id`, `duration_ms` and `compatibility_version`. `sigma` is on the scale of LAPSE's own `solid` bar (`--confidence`, default 8). Technical failures leave the metrics zero.

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `lapse.sync_started` | info | workflow | `phase=sync`, `provider`, `candidate_id`, `compatibility_version`; no result fields | `workflow/service.go` |
| `lapse.sync_completed` | info for `solid`; warn otherwise | workflow | verdict metrics | `workflow/service.go` |
| `lapse.failed` | error; warn for a verdict without a process exit | workflow | verdict metrics, `error_kind=lapse_sync` | `workflow/service.go` |
| `lapse.cache_cleaned` | debug | app | `removed_count` | `app/lapse_cache.go` |
| `lapse.cache_cleanup_failed` | warn | app | `removed_count` | `app/lapse_cache.go` |

A sync completion alone does not mean the candidate was installed: selection follows preparation of the whole score tier.

### subtitle and install

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `subtitle.installed` | info | workflow | `provider`, `candidate_id`, `score`, `replaced`, `fallback`, `duration_ms`, `checksum_prefix`; over a managed subtitle (an upgrade or a `reject` replacement) `replaced=true` and also `previous_provider`, `previous_candidate_id` and, when its stored score is readable, `previous_score` | `workflow/service.go` |
| `subtitle.install_failed` | error; warn for a validation rejection | workflow | `provider`, `candidate_id`, `duration_ms`, `error_kind=installation` | `workflow/service.go` |
| `subtitle.provenance_refreshed` | info | workflow | `provider`, `candidate_id`, `score`, `exact_hash`, `duration_ms` | `workflow/service.go` |
| `subtitle.provenance_refresh_failed` | error | workflow | `provider`, `candidate_id`, `score`, `duration_ms`, `error_kind=persistence` | `workflow/service.go` |
| `install.removed` | info | workflow | `provider`, `candidate_id`, `checksum_prefix`, `rollback_deleted`, `reason` (`media_replaced`, `media_renamed`) for a media change | `workflow/remove.go` |
| `install.record_dropped` | info | workflow | `reason` (`sidecar_missing`, `sidecar_changed`, `media_replaced`, `sidecar_unmovable`), `provider`, `candidate_id`, `checksum_prefix`, `rollback_deleted` | `workflow/installation_reconcile.go` |
| `install.renamed` | info | workflow | `moved`, `provider`, `candidate_id`, `checksum_prefix` | `workflow/installation_reconcile.go` |
| `install.reattached` | info | workflow | `provider`, `candidate_id`, `checksum_prefix` | `workflow/installation_reconcile.go` |
| `install.removal_deferred` | warn | workflow | `reason=cap`, `limit_per_hour`, `retry_at`, `provider`, `candidate_id`, `checksum_prefix`; the search then completes `throttled` with `reason=removal_deferred` | `workflow/installation_reconcile.go` |
| `install.rename_cleanup_failed` | warn | workflow | `error_kind=rename_cleanup` | `workflow/installation_reconcile.go` |
| `install.rollback_skipped` | warn | workflow | none | `workflow/install.go` |
| `install.rollback_cleanup_failed` | warn | workflow | `error_kind=rollback_cleanup` | `workflow/rollback.go` |
| `install.pending_clear_failed` | warn | workflow | `error_kind=pending_install` | `workflow/pending_install.go` |
| `install.recovered` | info | workflow | `media_id`, `language`, `outcome` (`recorded`, `undone`, `cleaned`, `move_finished`, `move_cleaned`) | `workflow/pending_install.go` |
| `install.recovery_failed` | warn | workflow | `media_id`, `language`, `error_kind=pending_install` | `workflow/pending_install.go` |
| `install.recovery_incomplete` | warn | app | `error_kind=pending_install` | `app/app.go` |
| `install.recovery_timed_out` | warn | app | `timeout_ms` | `app/app.go` |
| `install.rollback_sweep_completed` | info | app | `removed_count`, `skipped_count` | `app/app.go` |
| `install.rollback_sweep_failed` | warn | app | `error_kind=rollback_cleanup` | `app/app.go` |
| `install.rollback_sweep_timed_out` | warn | app | `timeout_ms` | `app/app.go` |

None of these carries a path. A reject's removed search completes with `outcome=removed` and `reason=rejected_subtitle_removed`.

### notification

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `notification.queued` | info | workflow | `notifier` (`silo`, `arr_rescan`), `notification_key` (12 characters); none when a change joins a queued Arr rescan | `workflow/install.go` |
| `notification.delivered` | info | worker | `outcome=success`, `duration_ms` | `worker/worker.go` |
| `notification.retry_scheduled` | warn | worker | `outcome=retry_scheduled`, `retry_at`, `duration_ms`, `error_kind=notification_delivery` | `worker/worker.go` |
| `notification.abandoned` | warn | worker | `outcome=abandoned` (an Arr 4xx such as 404 for a deleted series), `duration_ms`, `error_kind=notification_delivery` | `worker/worker.go` |
| `notification.failed` | error | worker | `outcome=failed` (any other terminal failure, including Silo 404, 410 and 422, an invalid Arr rescan request or media kind mismatch, or a completion that could not be stored), `duration_ms`, `error_kind` (`notification_delivery`, `notification_completion`) | `worker/worker.go` |
| `notification.skipped` | info | worker; workflow | `outcome=skipped`, `reason` (`rescan_disabled`, `unknown_arr_id`); the workflow variant adds `notifier=arr_rescan`, `instance`, `media_kind` | `worker/worker.go`, `workflow/notification.go` |
| `notification.pruned` | debug | worker | `removed_count`, `retention_days` | `worker/worker.go` |
| `notification.prune_failed` | warn | worker | `error_kind=notification_prune` | `worker/worker.go` |

Delivery records carry the notification correlation fields, including `series_id` or `movie_id` for `arr_rescan`. Errors name the HTTP status or `transport unavailable`, never a URL, key or body.

### history

| Event | Level | Component | Key fields | Emitter |
| --- | --- | --- | --- | --- |
| `history.record_failed` | warn | app | `error_kind=search_history` | `app/manual_search.go` |
| `history.prune_failed` | warn | app | `error_kind=search_history` | `app/lapse_cache.go` |
