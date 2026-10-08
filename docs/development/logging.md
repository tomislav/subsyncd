# Logging internals

Developer reference for event fields, levels, and privacy guarantees. For everyday use, see the [logging guide](../logging.md).

## Output and event contract

`subsyncd` writes one JSON event per line to stderr. View recent logs or follow new events with Docker Compose:

```bash
docker compose logs --tail=100 subsyncd
docker compose logs -f subsyncd
```

Configure the minimum level in YAML, or override it through the environment:

```yaml
logging:
  level: info
```

```dotenv
SUBSYNCD_LOG_LEVEL=debug
```

Valid levels are `debug`, `info`, `warn`, and `error`; omitted configuration defaults to `info`. The environment value takes precedence. Values are read only at startup, so a change requires a service restart. Use `info` continuously. Enable `debug` only for a bounded diagnostic run, then remove the override and restart at `info`.

Every record has `time`, `service`, `version`, `level`, `component`, `event`, and `msg`. Operational records add typed fields such as `job_id`, `media_id`, `media_title`, `media_kind`, `file_id`, `language`, `provider`, `candidate_id`, `outcome`, `reason`, `attempt`, `duration_ms`, `retry_at`, or `next_upgrade_at`. Once media is resolved, `media_title` follows the processing context through worker, workflow, provider, candidate, LAPSE, and completion events. Movies use `Movie (Year)` and episodes use `Show - S01E02 - Episode Title`; the value is normalized to one line and bounded to 2,048 Unicode code points. Follow one operation by parsing JSON and filtering on `job_id`.

The level behavior is:

| Level | Intended records |
| --- | --- |
| `info` | Service, webhook, durable job, search, archive-member selection, candidate rejection/retained skip, selected candidate, install/provenance, notification, reconciliation, and recovery lifecycle summaries. |
| `warn` | Expected degraded states such as throttling, persisted provider cooldown/circuit/auth transitions, non-solid LAPSE decisions, readiness loss, and bounded shutdown drain timeout. |
| `error` | Technical failures that require retry or operator attention. |
| `debug` | Candidate score components, release-name diagnostics, cache decisions, tournament/fallback decisions, and media paths only when safely root-relative. |

Successful `/healthz` and `/readyz` requests are intentionally silent. Readiness emits only a transition to unhealthy and a later recovery, avoiding probe noise. Webhook paths exclude query strings. Error text passes through configuration-aware redaction and is normalized to one line. Filesystem errors carry only their cause, never the path the operating system attaches, because redaction replaces configured roots but not the media path beneath them. The one deliberate exception for provider text is `provider.response_unrecognized` (warn, `component=provider`, with `provider` and `operation`): when SubDL answers with an unrecognised status-false message, its optional `excerpt` field carries at most 80 runes of that message, reduced to letters, digits and `.,:;'!?()-_%`, single-line, with the configured API key and credential-looking tokens replaced by `[redacted]` and a cut marked with `…`. Event fields bypass error redaction, so the SubDL client redacts the excerpt itself, and the text never enters an error message, whose wording `workflowErrorKind` uses to classify failures. Otherwise, credentials, bearer/API keys, provider URLs and bodies, notification payloads, absolute media/data/temp paths, command arguments, and raw LAPSE stdout/stderr are never intentional log fields. If root containment cannot be proven, even the debug relative path is omitted.

When diagnosing one workflow, follow records with the same `job_id` and inspect `job.started`, provider search/download events, `candidate.selected`, any LAPSE phase, installation/notification, and `job.completed`. Acquisition LAPSE work emits one `lapse.sync_started` at `info` immediately before the subprocess call, followed by the matching completion or `lapse.failed` event. Selection and installation follow preparation of the complete current score tier; a sync completion alone does not mean that candidate was installed. Start events contain correlation, phase, provider, candidate, and compatibility-version fields, but no duration or result fields. Candidate details require a temporary `SUBSYNCD_LOG_LEVEL=debug` restart; return to `info` immediately afterward.


`config.weak_webhook_token` (warn, `component=app`) is logged once per instance by `serve` only, after `service.starting`, when its `webhook_token` is shorter than 16 runes; it carries `instance` and `minimum_length` and never the token. Diagnostic commands do not emit it. Go's HTTP server reports its own failures as `http.server_error` (`component=http`) with `reason` `panic` (error level), `accept`, `tls_handshake`, or `other` (warn), plus `error_kind=http_server` and the redacted first line of the server message; panic stack traces are never logged. The worker logs route pauses once per transition: `queue.route_paused` at warn with `language`, `media_kind`, `provider_count`, and either `reset_at` or `reason=disabled`; `queue.route_resumed` at info with `language` and `media_kind`. A paused route is logged again when its reset time changes, including when only disabled providers keep it paused. A failed availability check logs `queue.route_check_failed` at error with `error_kind=route_check`, and that dispatch leases nothing; cancellation during shutdown is not logged. A pause that applies only to multi-episode files (single-episode-only providers can still serve the route) adds `episode_ranges_only=true`. These events carry no provider IDs. `reconcile.deferred` (warn, `component=worker`) reports a committed reconciliation page that is waiting for unreadable current media, with `instance`, `deferred_count`, `deferred_media` (at most ten log-safe `Sonarr episode <id> (<media title>)`, `Sonarr file <id> (<series> - season <n>)` or `Radarr movie <id> (<media title>)` labels, never paths; empty when a catalog reports the bare sentinel), `retry_at`, and `duration_ms`; it does not increment the failure backoff that `reconcile.failed` (error) reports. `reconcile.snapshot_deletions_withheld` (warn, `component=catalog`) reports a catalog identity snapshot that would retire all tracked identities, or more than half and at least 10, with `instance`, `media_kind`, `active_count`, `missing_count`, and `snapshot_count`; history from the page still commits. `reconcile.multi_episode_recheck_failed` (warn, `component=catalog`) reports a legacy multi-episode file that could not be re-read from Sonarr, with `instance`, `file_id` when known, and `error_kind`.

`archive.members_selected` is an info event with provider, candidate_id, selection_rule, matching_member_count and subtitle_member_count. Deterministic content/preparation failures emit `candidate.rejected` immediately at info with a bounded reason_code; selection failures add bounded archive type/rule/counts. `candidate.skipped` reports retained rejections at info. Multi-version preparation and selected/installed records carry member_index/member_count so repeated LAPSE events for the same provider candidate can be distinguished. Indices are local to the current preparation group; no filename or path is emitted. `candidate.rejection_details` and score/cache/tournament details remain debug-only.
