# Operations

## Filesystem and container permissions

The image defaults to UID/GID `1000:1000`. The Compose examples select the runtime identity from host-side interpolation variables:

```dotenv
PUID=1000
PGID=1000
TZ=Europe/Zagreb
```

Compose resolves `PUID` and `PGID` before the container starts and applies them to both `user:` and `/tmp` tmpfs ownership. Put them in the project's `.env` file or export them in the invoking shell. Adding them only to the service's `environment:` mapping would not change the process identity. `TZ` is passed into the container, whose image includes timezone data.

The selected identity expects:

- `/config/config.yaml`: readable configuration, normally a read-only mount.
- `/data`: writable SQLite database, LAPSE speech cache, pack cache, and process lock.
- Every configured media root: readable for probing/hashing and writable for atomic sidecar installation.
- `/tmp`: writable ephemeral space; the Compose examples provide a bounded tmpfs.

Create host directories before starting and grant the selected UID/GID access. The container remains rootless and does not create users or change ownership at startup. The mounted paths and optional `install.uid`/`install.gid` must agree. Chown is omitted unless those optional settings are configured; unprivileged containers normally leave them unset.

`install.file_mode` defaults to `0644`, accepts an octal non-executable mode, and is applied before atomic publication:

```yaml
install:
  file_mode: "0640"
  # uid: 1000
  # gid: 1000
```

## Temporary processing files

Downloads, extracted subtitles, and LAPSE synchronization output use a private `.subsyncd-work-*` directory in the system temporary directory (`TMPDIR` when set, otherwise `/tmp` in the Linux container). LAPSE analysis uses its own private temporary copy there too. Normal workflow completion, rejection, errors, and cancellation remove the workflow directory. Rejected-candidate artifacts are released as processing advances so an uncapped exact search does not retain every attempted download.

The final `.subsyncd-stage-*` file and managed-replacement `.subsyncd-rollback-*` copy stay beside the media for atomic publication and recovery. Pack-cache publication stages within the persistent pack-cache root; LAPSE speech profiles also remain in the persistent data directory. Moving scratch to a separate filesystem does not change either atomic publication path.

Application errors redact the effective temporary root. LAPSE failures expose bounded verdict, exit-code, and protocol/operation categories; raw stderr, JSON diagnostic values, and command errors are not included. No-speech detection still classifies the failure internally without logging the diagnostic output.

The Compose example supplies a 512 MiB `/tmp` tmpfs shared by active workflows. Increasing workflow concurrency may require increasing that temporary-storage allowance. Do not point `TMPDIR` at a media-library directory if temporary subtitles must remain invisible to library scans. Abrupt process/container termination can bypass deferred cleanup; container tmpfs is discarded when its mount is recreated. This change does not scan or remove legacy `.subsyncd-work-*` directories from media roots.

## Startup and health

Startup is deliberately offline with respect to Arr, subtitle providers, and Silo. It fails only for invalid configuration, unsafe/missing local roots, SQLite migration/open errors, missing `ffprobe`, or an incompatible LAPSE executable. Configuration must contain exactly one non-empty YAML document: cardinality is checked on the original bytes before environment expansion, so a trailing document is rejected without expanding or exposing its values. Empty trailing separators are harmless. Run diagnostics after every configuration/image change:

```bash
subsyncd doctor --config /config/config.yaml
```

`GET /healthz` means the HTTP process is alive. `GET /readyz` rechecks SQLite and media-root availability. Temporary provider or Arr failures do not make readiness fail; they are scheduled and logged instead.

Provider network errors and HTTP 5xx responses are circuit-broken per provider instance and operation with persisted 1, 5, 15, and 60 minute retries. A provider-supplied `Retry-After` overrides that delay. Interrupted response bodies also participate in provider failure backoff; caller cancellation and local output failures do not. Definitive login HTTP 401 from OpenSubtitles/Titlovi and SubDL HTTP 403 disable the affected instance until its credentials are corrected and the operator clears provider state (substitute that instance name below):

```bash
subsyncd retry --config /config/config.yaml --provider subdl-main
```

The retry command clears all cooldown, quota, transient-failure, and disabled-authentication scopes for that one configured provider; it does not alter candidate rejections or search schedules.

## Structured logging and Grafana Loki

`subsyncd` writes synchronous newline-delimited JSON to stderr. Each physical line is one event, suitable for Docker's log driver and Grafana Alloy. Configure the minimum level in YAML, or override it through the environment:

```yaml
logging:
  level: info
```

```dotenv
SUBSYNCD_LOG_LEVEL=debug
```

Valid levels are `debug`, `info`, `warn`, and `error`; omitted configuration defaults to `info`. The environment value takes precedence. Values are read only at startup, so a change requires a service restart. Use `info` continuously. Enable `debug` only for a bounded diagnostic run, then remove the override and restart at `info`.

Every record has `time`, `service`, `version`, `level`, `component`, `event`, and `msg`. Operational records add typed fields such as `job_id`, `media_id`, `media_title`, `media_kind`, `file_id`, `language`, `provider`, `candidate_id`, `outcome`, `reason`, `attempt`, `duration_ms`, `retry_at`, or `next_upgrade_at`. Once media is resolved, `media_title` follows the processing context through worker, workflow, provider, candidate, LAPSE, and completion events. Movies use `Movie (Year)` and episodes use `Show - S01E02 - Episode Title`; the value is normalized to one line and bounded to 2,048 Unicode code points. Follow one operation by parsing JSON and filtering on `job_id`; titles and IDs remain fields in the JSON body rather than Loki labels.

The level behavior is:

| Level | Intended records |
| --- | --- |
| `info` | Service, webhook, durable job, search, selected candidate, install/provenance, notification, reconciliation, and recovery lifecycle summaries. |
| `warn` | Expected degraded states such as throttling, persisted provider cooldown/circuit/auth transitions, non-solid LAPSE decisions, readiness loss, and bounded shutdown drain timeout. |
| `error` | Technical failures that require retry or operator attention. |
| `debug` | Candidate score components/rejections, release-name diagnostics, cache decisions, tournament/fallback decisions, and media paths only when safely root-relative. |

Successful `/healthz` and `/readyz` requests are intentionally silent. Readiness emits only a transition to unhealthy and a later recovery, avoiding probe noise. Webhook paths exclude query strings. Error text passes through configuration-aware redaction and is normalized to one line. Credentials, bearer/API keys, provider URLs and bodies, notification payloads, absolute media/data/temp paths, command arguments, and raw LAPSE stdout/stderr are never intentional log fields. If root containment cannot be proven, even the debug relative path is omitted.

### Example Alloy pipeline

This example was verified with Alloy `v1.19.2` and a managed Grafana Cloud Loki destination. It collects both stdout and stderr from Docker. To avoid duplicate ingestion, add a `/subsyncd` drop rule to the existing general Docker target pipeline, then use this dedicated keep-before-parse pipeline. It promotes only the bounded application labels `service`, `environment`, `level`, `component`, and `event`; dynamic IDs remain in the original JSON line.

```alloy
discovery.relabel "subsyncd_logs" {
  targets = discovery.docker.logs_integrations_docker.targets

  rule {
    source_labels = ["__meta_docker_container_name"]
    regex         = "/subsyncd"
    action        = "keep"
  }

  rule {
    target_label = "service"
    replacement  = "subsyncd"
  }

  rule {
    target_label = "environment"
    replacement  = "production"
  }
}

loki.source.docker "subsyncd" {
  host             = "unix:///var/run/docker.sock"
  targets          = discovery.relabel.subsyncd_logs.output
  forward_to       = [loki.process.subsyncd.receiver]
  refresh_interval = "60s"
}

loki.process "subsyncd" {
  stage.json {
    expressions = {
      level     = "level",
      component = "component",
      event     = "event",
    }
  }

  stage.labels {
    values = {
      level     = "",
      component = "",
      event     = "",
    }
  }

  forward_to = [loki.write.grafana_cloud_loki.receiver]
}

loki.write "grafana_cloud_loki" {
  endpoint {
    url = sys.env("LOKI_URL")

    basic_auth {
      username = sys.env("LOKI_USERNAME")
      password = sys.env("LOKI_PASSWORD")
    }
  }
}
```

The referenced `loki.write` name can instead be your existing component. Loki credentials belong only in Alloy and its service environment, never in `subsyncd` YAML or Compose environment. Do not add `job_id`, `media_id`, `media_title`, `candidate_id`, `file_id`, `language`, or `provider` to `stage.labels`; their cardinality is unbounded.

Grafana Cloud manages the Loki version when you use its hosted service. These LogQL queries use its current JSON parser, duration numeric filter, and escaped literal-dot regex syntax:

```logql
{service="subsyncd", environment="production", event="job.completed"} | json | outcome="failed"
{service="subsyncd", environment="production", event=~"provider\\.(cooldown_started|circuit_opened|auth_disabled)"} | json
{service="subsyncd", environment="production", event=~"lapse\\.(analysis_completed|sync_completed)"} | json | duration_ms > 600000
{service="subsyncd", environment="production", event="candidate.selected"} | json
{service="subsyncd", environment="production"} | json | job_id="JOB_ID"
```

When diagnosing one workflow, start with the final query, then inspect `job.started`, provider search/download events, `candidate.selected`, any LAPSE phase, installation/notification, and `job.completed`. Long-running LAPSE work emits `lapse.analysis_started` or `lapse.sync_started` at `info` immediately before the subprocess call, followed by the matching completion or `lapse.failed` event. Start events contain correlation, phase, provider, candidate, and compatibility-version fields, but no duration or result fields. Candidate details require a temporary `SUBSYNCD_LOG_LEVEL=debug` restart; return to `info` immediately afterward.

## Sonarr and Radarr setup

Each instance needs a unique `name`, API key, webhook secret, and one or more remote-to-local path mappings. Longest boundary-aware mapping wins. Mapping destinations must sit inside a configured media root, and existing parent symlinks are resolved before acceptance.

Create an Arr webhook/connection pointing to:

```text
http://subsyncd:8097/webhooks/INSTANCE_NAME?token=INSTANCE_WEBHOOK_TOKEN
```

Enable download/import (including upgrades), rename, and file-delete events. `subsyncd` deliberately does not create or modify Arr connections; this keeps ownership explicit and avoids coupling startup to mutable, version-specific Arr configuration. Keep the token out of general reverse-proxy access logs. `subsyncd` itself logs only `/webhooks/INSTANCE_NAME`, never the query string. The request body limit is 1 MiB.

An import or rename whose hydrated media is deliberately outside the configured path mappings returns HTTP 204 as ignored. It creates no audit mutation, media/search row, or worker wake. This permits a normal Arr connection to target a narrow canary without causing retries for the remainder of the library. Only the typed scope condition is ignored: traversal, unsafe filesystem resolution, Arr transport/authentication, and SQLite failures still return an error. Delete webhooks remain file-ID addressed; reconciliation provides stable-entity deletion recovery.

Startup assembly remains offline: it creates no Arr client traffic and performs no full-library scan or identity backfill. After the daemon starts, every instance attempts reconciliation immediately and normally repeats every six hours using an independent persisted cursor. A failed attempt retains that cursor and retries after 5 minutes, 15 minutes, 1 hour, then every 6 hours until success. This attempt state is in memory, so a process restart tries immediately; one failed instance neither advances nor delays another.

Reconciliation uses `github.com/cplieger/arrapi/v2` v2.0.5 for bounded, retried history and current movie/episode requests. History is fetched in descending 100-record pages, with strict page-count, ordering, and progress checks; malformed or inconsistent pagination does not advance the cursor. History is grouped by stable Arr entity (`movieId` or `episodeId`), not replaceable physical file ID, and current state decides whether that entity is present, absent, or outside the configured scope. The detail client rejects redirects even within the same origin and exposes only bounded error categories, preserving cancellation and timeout identity. It is retained only to enrich live files with release, quality, edition, runtime, and provenance fields not exposed by arrapi v2.0.5.

Imports, renames, upgrades, deletion tombstones, audit rows, media/search changes, and the new cursor commit in one SQLite transaction. Arr history requests have second resolution, so subsyncd rounds the durable cursor down and intentionally overlaps its fractional second; stable history event IDs make the replay transactionally idempotent. A malformed or otherwise failed page advances nothing. Root remote mappings (`/`) remain valid. Webhook batches skip outside-scope items individually and continue later relevant items; retained committed event IDs are checked before hydration so exact redeliveries do not depend on the old file remaining available. Deliberately unmapped or safely out-of-root media is consumed as an outside-scope delete/audit, which keeps a narrow canary from reading or indexing the rest of the library; unsafe mappings and filesystem errors fail the page closed.

subsyncd supports only the current lineage beginning with `001_baseline.sql`, followed by its ordered embedded migrations. A database containing any migration name from the pre-release `001_initial.sql`–`010_media_entity_ids.sql` lineage fails startup with an explicit unsupported-lineage error. Stop the service, preserve or move the complete old data directory for rollback, create an empty data directory with the configured container ownership, and let a mutating command initialize the current schema. `serve` performs that initialization locally before starting runtime work; after initialization, `doctor` can inspect the database while the daemon runs. Do not copy rows between lineages.

Migration `002_scrub_provider_credentials.sql` removes provider-derived cache, candidate, pack, installation-provenance, and rejection rows whose persisted fields contain a legacy `api_key=` query. It preserves media inventory, search schedules and active leases, reconciliation cursors, and unrelated clean provider rows; deleting a contaminated pack cascades only its cached members. Subtitle files already installed on disk remain in place when contaminated installation provenance is removed, but they become protected untracked sidecars and cannot be upgraded as service-owned files. Rotate any SubDL key used by an affected build before starting the corrected image.

Migration `003_inventory_probes.sql` adds completed-probe fingerprints and retained media deletion state. Existing track rows do not become completed probes automatically: their next normal refresh probes the file once, then caches the result. The migration performs no startup scan and does not infer deletion state from historical events. Subsequent catalog deletions block inventory and installation commits until the media is reimported.

Every persisted media row has a positive stable Arr entity ID. Physical file ID remains separate and replaceable for hashes, inventory, installations, and webhook processing. Startup remains offline and performs no Arr request, identity backfill, or full-library scan. The migration runner remains active for schema changes added after `001_baseline.sql`.

The daemon starts with one media workflow at a time. Increase this only when the host and media storage can sustain concurrent provider preparation and LAPSE reads:

```yaml
worker:
  max_concurrent: 1 # valid range: 1..8
```

Search work is durable and strictly ordered by class: a newly imported or renamed file runs before an ordinary missing-subtitle retry, which runs before a scheduled upgrade check. Within one class, the oldest due time wins. A webhook sends a nonblocking advisory wake after its database commit, so a free slot is filled without waiting for the next poll. Wakes may coalesce during bursts; startup and the jittered recovery poll read SQLite again, so correctness never depends on receiving every signal. A new event for a media/language already being processed requests exactly one immediate rerun without starting a concurrent duplicate. Priority changes dispatch order only and never bypass provider rate limits or persisted cooldowns.

To add an Arr instance or language, edit the configuration and restart subsyncd. A new instance receives an empty reconciliation cursor and its retained Arr history is reconciled into the normal configured-language schedules; configure its webhook as well for immediate future changes. This is not a full Arr library enumeration, so media absent from retained history requires a later webhook, rename, or manual search. A newly added language is backfilled locally for media subsyncd already indexes under currently configured instances. Those checks are immediately due at missing priority, but embedded or protected subtitles can satisfy them without a provider request. Existing search schedules, leases, attempts, installations, and other-language state are preserved.

The same lease rule applies to reconciliation. If history reports a change for media already being processed, the current owner and expiry remain intact and one immediate rerun is coalesced. An old completion cannot erase that rerun.

For a missing language, search workflows run at absolute milestones from import or reset: immediately, about 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, and every 14 days thereafter, with interval jitter. Sidecars are refreshed on every run. Provider search results are cached for six hours, so the 30-minute and 2-hour workflows normally perform local checks without another provider request; under an unchanged empty result, external searches normally occur around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days. Provider cooldowns and technical-failure retries remain independent of this sequence.

## Embedded and external subtitle behavior

FFprobe indexes all embedded subtitle streams, including text and image codecs. A separate completed-probe fingerprint records successful probes, including empty results; catalog metadata alone never establishes cache validity. The result is reused until path, Arr file ID, size, or nanosecond mtime changes. Inventory commits compare the original catalog snapshot transactionally and recheck live media after probing, so stale work cannot restore a replaced or renamed file identity. Sidecars are never trusted from that cache: `.srt`, `.ass`, `.ssa`, and `.vtt` files for the exact media stem are scanned and checksummed before every search.

A matching full embedded track prevents downloading. Forced-only or unknown-language (`und`) tracks do not. SDH/HI tracks are rejected by default and satisfy only when `allow_hearing_impaired: true` is explicitly configured. Existing sidecars are protected unless their path and checksum match `subsyncd` installation provenance, so user edits are never overwritten automatically.

Stored provenance is historical evidence, not proof that a sidecar still exists. Each workflow refreshes live sidecars and treats a managed installation as active only when its recorded path and checksum are present. If that managed file was deleted, normal acquisition runs with first-install semantics and can reacquire the same exact candidate. If a sidecar is present but its checksum differs, it remains protected as user-owned content and is not replaced.

Sonarr can associate more than one episode with a single media file. `subsyncd` records such a file using the earliest episode as its canonical stable entity and persists `unsupported_multi_episode`. Every configured language is terminally marked with that outcome; provider search, download, candidate processing, LAPSE, installation, and upgrade work are skipped. `subsyncd explain` shows the reason. A later single-episode import or rename clears the marker and schedules normal work.

## LAPSE policy

LAPSE v2.0.5 is the compatibility baseline. Exact OpenSubtitles hash matches always bypass it. With the default `sync.policy: confidence`, a non-pack first install also bypasses LAPSE when its release score is at least 75 and the score contains an identity anchor (external ID or normalized title/year), a release-group match, and explicit episode evidence for TV. Provider rating and popularity can add points but cannot replace those required anchors. The stored sync verdict is `score_bypass`, so `explain` distinguishes this decision from an exact hash and a LAPSE result.

Packs and managed-subtitle upgrades still require LAPSE by default, even if their score is high. Candidates below the bypass score or missing any required anchor also require it. If Radarr identifies a movie edition, a non-hash candidate must explicitly match that edition to bypass LAPSE; an unknown edition remains eligible but is analyzed, while a known mismatch is rejected. Set `sync.policy: always` to restore LAPSE for every non-exact candidate. The individual `require_*` and `lapse_for_*` switches are configurable for unusual libraries, but relaxing them increases the chance of installing a wrong or unsynchronized subtitle.

When LAPSE is required, analysis runs against a private subtitle copy with `--dry-run --json --strict --no-sidecar`. Synchronization writes a new explicit output with `--output`, `--no-backup`, `--json`, `--strict`, and `--no-sidecar`. `unsure`, `nothing`, malformed JSON, invalid output, and missing speech are rejections; only `solid` can install.

The best three eligible non-hash candidates are a cap, not an eager batch. They are partitioned by release score and processed from highest score downward. A unique top scorer is downloaded and analyzed alone; if it is solid, it is synchronized once and lower tiers are never downloaded. Candidates tied at the same score are all downloaded and analyzed so LAPSE confidence can choose the best; only that winner is synchronized. A synchronization failure tries the next already analyzed tie, and only an exhausted tier opens the next lower score. Deterministic verdict/content failures retain their scoped quarantine behavior, while process, filesystem, provider, network, and cancellation errors remain retryable and never blacklist a candidate.

An `unsure` or `nothing` verdict quarantines that provider result for 30 days, scoped to the language, exact media fingerprint, stable candidate/release metadata, selected artifact checksum when known, LAPSE compatibility version, and synchronization policy. Invalid or oversized subtitle payloads and ambiguous, missing, or explicitly conflicting episode-member selection are quarantined too. This is candidate-local and does not throttle the provider. Rejected candidates are removed before the three-candidate shortlist, so later-ranked results advance on the next job. A rejected cached pack member is skipped by checksum; the pack remains available to other episodes.

A candidate-local subtitle validation rejection during installation advances to the next analyzed tie or score tier, with rejection evidence based on the original selected subtitle. A format-changing upgrade also advances without replacing the managed sidecar. Filesystem, stale-media, database, and rollback failures stop installation. The installer checks the media file immediately before publication, and the installation/outbox transaction checks the stored path, Arr file ID, size, and modification time; a media replacement during LAPSE cannot commit obsolete provenance or notification intents.

At debug level, `candidate.rejected` reports bounded archive-selection fields: `reason_code`, `selection_rule`, `archive_type`, `subtitle_member_count`, and `matching_member_count`, together with provider and candidate ID. Archive filenames and absolute paths are deliberately omitted.

Timeouts, crashes, malformed LAPSE protocol output, cancellation, filesystem failures, and provider/network failures do not quarantine the candidate. They surface as technical failures and use the separate failure backoff. No-speech is a media-validation rejection rather than evidence that one particular subtitle is bad. In every failure case, the job workspace is removed and an existing installed subtitle remains untouched.

LAPSE itself makes no internet request, but it reads the media to build a speech profile; on network storage, the first analysis can therefore read much or nearly all of the file. The profile cache is persisted under `/data/lapse-cache`, so later candidates for the unchanged media can reuse it. The confidence gate avoids that media read for strong first-install matches. A normal timeout is 30 minutes. Cancellation kills the entire LAPSE subprocess group.

With distinct candidate scores and a successful leader, the expected path is one analysis plus one synchronization. Equal-score ties still require one analysis per tied candidate because confidence is meaningful only within that metadata tier.

Diagnostic analysis never installs a sidecar:

```bash
subsyncd analyze-sync --config /config/config.yaml --media /media/movies/Movie.mkv --subtitle /tmp/candidate.srt
```

## CLI reference

The default configuration path is `/config/config.yaml`; set `SUBSYNCD_CONFIG` or pass `--config` to override it.

```bash
subsyncd serve --config /config/config.yaml
subsyncd scan --config /config/config.yaml --instance sonarr-main
subsyncd scan --config /config/config.yaml --instance sonarr-main --force-probe
subsyncd search --config /config/config.yaml --instance radarr-main --kind movie --file-id 42 --language en
subsyncd search --config /config/config.yaml --instance radarr-main --kind movie --file-id 42 --language en --retry-rejected
subsyncd retry --config /config/config.yaml --provider titlovi-main
subsyncd explain --config /config/config.yaml --instance sonarr-main --kind episode --file-id 1001 --language hr
subsyncd doctor --config /config/config.yaml
subsyncd analyze-sync --config /config/config.yaml --media /media/tv/Show/S01E01.mkv --subtitle /tmp/test.srt
subsyncd --version
```

Exit status is 0 for success, 1 for an operational failure, and 2 for invalid command usage. `serve`, `scan`, `search`, and `retry` take the nonblocking `/data/subsyncd.lock` before migrations or durable initialization and retain it until application close; a second mutator fails before changing startup state. Stop the daemon before running a mutating one-shot command.

`explain`, `doctor`, and `analyze-sync` open an existing current-schema database read-only and can run while the daemon holds its lock. They do not apply migrations, add configured-language searches, or create persistent caches. A missing or outdated database requires initialization by a mutating command first. Diagnostic LAPSE analysis uses temporary cache storage. Startup failures already emitted as structured events are not printed again as raw CLI errors.

`explain` reports the indexed media identity, embedded/sidecar tracks, human-readable search priority, pending-rerun state, missing/failure attempts, last outcome and next attempt, candidates with score/identity evidence, active candidate rejections with reason and expiry, managed installation and LAPSE provenance, reusable pack count, and provider cooldowns. `search --retry-rejected` clears the rejection records for only the selected media/language before performing the manual search; any candidate that deterministically fails again is immediately re-quarantined.

## Silo notification

Enable Silo only after setting its native API URL and admin API key:

```yaml
silo:
  enabled: true
  url: http://silo:8080
  api_key: ${SILO_API_KEY}
  path_mappings:
    - from: /media
      to: /mnt/media
```

The installation provenance and one checksum-deduplicated intent per configured notifier are written in the same SQLite transaction as the filesystem publication's logical commit. If that durable database operation fails, the new sidecar is removed or the prior managed copy is restored. Only after it commits does asynchronous delivery send `POST /api/v1/scan` with `Authorization: Bearer …` and the mapped parent directory of the media file. A later Silo failure never rolls back the subtitle or installation; timeout, 408, 429, and 5xx responses retry independently, while other 4xx responses are terminal. See [the Silo protocol ledger](references/silo.md).

This adapter targets Silo's current pre-1.0 native API. Silo plans to retire `/api/v1` at 1.0, and the v2 scan route is not yet published. Check the protocol ledger and upgrade `subsyncd` before moving Silo past its dual-API bridge release; `subsyncd` deliberately does not guess or fall back between mutating API versions.

## Backup, restart, and recovery

For a consistent backup, stop the service and copy `/data` as one unit. It contains the SQLite database and both caches. Restoring only the database can leave pack manifests missing; those entries are detected and invalidated safely, but the cache benefit is lost.

Search and notification leases are recoverable after five minutes. On shutdown, workers receive one graceful drain window, then cancellation, then one final equally bounded window. The Compose example allows 75 seconds before forced termination, covering both default 30-second windows with exit margin. Increase that grace period if increasing `worker.shutdown_timeout`. A cancellation-insensitive external operation may outlive the daemon return, but its lease is deliberately not cleared and becomes recoverable after expiry; operators should avoid starting a replacement process against the same media mounts until the old process/container has actually stopped.

Search claims include only configured instance/language routes and active media. Removing a route preserves its stored work without retrying it; re-enabling the route makes eligible work claimable again. Deleting media retains any active lease until its owner completes. Reimport during that lease requests one rerun instead of overlapping the existing workflow.

A crash after atomic publication but before search completion does not trigger a second installation: the next inventory pass recognizes the checksum-recorded sidecar. Managed replacements retain one rollback file until a later successful replacement supersedes it; database-commit failure restores it immediately. If removal of a newly published sidecar or restoration/fsync of a replacement also fails, the combined technical error is logged and returned. The destination or rollback copy is retained rather than silently discarded. Such a sidecar may be valid but untracked and is therefore protected from automatic takeover; inspect it and `explain` before resolving it manually.

If a user wants to take ownership of a subtitle, edit or replace the sidecar. Its checksum then differs from provenance and `subsyncd` protects it. Use `explain` before removing any generated file; never delete arbitrary `.subsyncd-*` files while the daemon is running.

## Troubleshooting

- `another subsyncd mutation process is active`: stop the daemon or wait for the other one-shot mutator; do not delete the lock file to bypass a live lock.
- `media path is outside configured roots`: fix the Arr path mapping or mount path; do not broaden roots merely to silence the check.
- no provider call: check `explain` for an embedded/protected track, exact terminal installation, future schedule, pack cache, or provider cooldown.
- repeated missing result: this is expected backoff, not a worker sleep. Use `search` for a deliberate manual attempt or `retry` only to clear provider throttle/auth state.
- LAPSE rejection: inspect `candidate_rejections` with `explain`; `unsure`/`nothing` is intentionally not installable. Use `analyze-sync` for diagnosis or `search --retry-rejected` after changing the media, candidate source, LAPSE build, or policy.
- Silo does not refresh: confirm port 8090 reaches Silo's native API, the key is an admin API key, and the container-to-Silo path mapping is correct.
