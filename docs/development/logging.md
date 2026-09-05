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
| `info` | Service, webhook, durable job, search, selected candidate, install/provenance, notification, reconciliation, and recovery lifecycle summaries. |
| `warn` | Expected degraded states such as throttling, persisted provider cooldown/circuit/auth transitions, non-solid LAPSE decisions, readiness loss, and bounded shutdown drain timeout. |
| `error` | Technical failures that require retry or operator attention. |
| `debug` | Candidate score components/rejections, release-name diagnostics, cache decisions, tournament/fallback decisions, and media paths only when safely root-relative. |

Successful `/healthz` and `/readyz` requests are intentionally silent. Readiness emits only a transition to unhealthy and a later recovery, avoiding probe noise. Webhook paths exclude query strings. Error text passes through configuration-aware redaction and is normalized to one line. Credentials, bearer/API keys, provider URLs and bodies, notification payloads, absolute media/data/temp paths, command arguments, and raw LAPSE stdout/stderr are never intentional log fields. If root containment cannot be proven, even the debug relative path is omitted.

When diagnosing one workflow, follow records with the same `job_id` and inspect `job.started`, provider search/download events, `candidate.selected`, any LAPSE phase, installation/notification, and `job.completed`. Acquisition LAPSE work emits one `lapse.sync_started` at `info` immediately before the subprocess call, followed by the matching completion or `lapse.failed` event. Selection and installation follow preparation of the complete current score tier; a sync completion alone does not mean that candidate was installed. Start events contain correlation, phase, provider, candidate, and compatibility-version fields, but no duration or result fields. Candidate details require a temporary `SUBSYNCD_LOG_LEVEL=debug` restart; return to `info` immediately afterward.

