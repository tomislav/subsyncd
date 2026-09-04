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

## Startup and health

Startup is deliberately offline with respect to Arr, subtitle providers, and Silo. It fails only for invalid configuration, unsafe/missing local roots, SQLite migration/open errors, missing `ffprobe`, or an incompatible LAPSE executable. Run diagnostics after every configuration/image change:

```bash
subsyncd doctor --config /config/config.yaml
```

`GET /healthz` means the HTTP process is alive. `GET /readyz` rechecks SQLite and media-root availability. Temporary provider or Arr failures do not make readiness fail; they are scheduled and logged instead.

## Sonarr and Radarr setup

Each instance needs a unique `name`, API key, webhook secret, and one or more remote-to-local path mappings. Longest boundary-aware mapping wins. Mapping destinations must sit inside a configured media root, and existing parent symlinks are resolved before acceptance.

Create an Arr webhook/connection pointing to:

```text
http://subsyncd:8097/webhooks/INSTANCE_NAME?token=INSTANCE_WEBHOOK_TOKEN
```

Enable download/import (including upgrades), rename, and file-delete events. `subsyncd` deliberately does not create or modify Arr connections; this keeps ownership explicit and avoids coupling startup to mutable, version-specific Arr configuration. Keep the token out of general reverse-proxy access logs. `subsyncd` itself logs only `/webhooks/INSTANCE_NAME`, never the query string. The request body limit is 1 MiB.

Every instance also reconciles immediately at process start and every six hours using an independent persisted cursor. A failed instance does not roll back another instance's cursor.

The daemon starts with one media workflow at a time. Increase this only when the host and media storage can sustain concurrent provider preparation and LAPSE reads:

```yaml
worker:
  max_concurrent: 1 # valid range: 1..8
```

Search work is durable and strictly ordered by class: a newly imported or renamed file runs before an ordinary missing-subtitle retry, which runs before a scheduled upgrade check. Within one class, the oldest due time wins. A webhook sends a nonblocking advisory wake after its database commit, so a free slot is filled without waiting for the next poll. Wakes may coalesce during bursts; startup and the jittered recovery poll read SQLite again, so correctness never depends on receiving every signal. A new event for a media/language already being processed requests exactly one immediate rerun without starting a concurrent duplicate. Priority changes dispatch order only and never bypass provider rate limits or persisted cooldowns.

For a missing language, search workflows run at absolute milestones from import or reset: immediately, about 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, and every 14 days thereafter, with interval jitter. Sidecars are refreshed on every run. Provider search results are cached for six hours, so the 30-minute and 2-hour workflows normally perform local checks without another provider request; under an unchanged empty result, external searches normally occur around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days. Provider cooldowns and technical-failure retries remain independent of this sequence.

## Embedded and external subtitle behavior

FFprobe indexes all embedded subtitle streams, including text and image codecs. The result is stored in SQLite and reused until path, Arr file ID, size, or nanosecond mtime changes. Sidecars are never trusted from that cache: `.srt`, `.ass`, `.ssa`, and `.vtt` files for the exact media stem are scanned and checksummed before every search.

A matching full embedded track prevents downloading. Forced-only or unknown-language (`und`) tracks do not. SDH/HI tracks are rejected by default and satisfy only when `allow_hearing_impaired: true` is explicitly configured. Existing sidecars are protected unless their path and checksum match `subsyncd` installation provenance, so user edits are never overwritten automatically.

## LAPSE policy

LAPSE v2.0.5 is the compatibility baseline. Exact OpenSubtitles hash matches always bypass it. With the default `sync.policy: confidence`, a non-pack first install also bypasses LAPSE when its release score is at least 75 and the score contains an identity anchor (external ID or normalized title/year), a release-group match, and explicit episode evidence for TV. Provider rating and popularity can add points but cannot replace those required anchors. The stored sync verdict is `score_bypass`, so `explain` distinguishes this decision from an exact hash and a LAPSE result.

Packs and managed-subtitle upgrades still require LAPSE by default, even if their score is high. Candidates below the bypass score or missing any required anchor also require it. Set `sync.policy: always` to restore LAPSE for every non-exact candidate. The individual `require_*` and `lapse_for_*` switches are configurable for unusual libraries, but relaxing them increases the chance of installing a wrong or unsynchronized subtitle.

When LAPSE is required, analysis runs against a private subtitle copy with `--dry-run --json --strict --no-sidecar`. Synchronization writes a new explicit output with `--output`, `--no-backup`, `--json`, `--strict`, and `--no-sidecar`. `unsure`, `nothing`, malformed JSON, invalid output, and missing speech are rejections; only `solid` can install.

The best three eligible non-hash candidates are a cap, not an eager batch. They are partitioned by release score and processed from highest score downward. A unique top scorer is downloaded and analyzed alone; if it is solid, it is synchronized once and lower tiers are never downloaded. Candidates tied at the same score are all downloaded and analyzed so LAPSE confidence can choose the best; only that winner is synchronized. A synchronization failure tries the next already analyzed tie, and only an exhausted tier opens the next lower score. Deterministic verdict/content failures retain their scoped quarantine behavior, while process, filesystem, provider, network, and cancellation errors remain retryable and never blacklist a candidate.

An `unsure` or `nothing` verdict quarantines that provider result for 30 days, scoped to the language, exact media fingerprint, stable candidate/release metadata, selected artifact checksum when known, LAPSE compatibility version, and synchronization policy. Invalid or oversized subtitle payloads and ambiguous pack selection are quarantined too. Rejected candidates are removed before the three-candidate shortlist, so later-ranked results advance on the next job. A rejected cached pack member is skipped by checksum; the pack remains available to other episodes.

Timeouts, crashes, malformed LAPSE protocol output, cancellation, filesystem failures, and provider/network failures do not quarantine the candidate. They surface as technical failures and use the separate failure backoff. No-speech is a media-validation rejection rather than evidence that one particular subtitle is bad. In every failure case, the job workspace is removed and an existing installed subtitle remains untouched.

LAPSE itself makes no internet request, but it reads the media to build a speech profile; on network storage, the first analysis can therefore read much or nearly all of the file. The profile cache is persisted under `/data/lapse-cache`, so later candidates for the unchanged media can reuse it. The confidence gate avoids that media read for strong first-install matches. A normal timeout is 30 minutes. Cancellation kills the entire LAPSE subprocess group.

Before this tournament, production canaries took 12m11s for Arrival Croatian and 24m55s for 1917 Croatian; the latter performed three analyses plus three synchronization passes and read about 22.1 GB. With distinct candidate scores and a successful leader, the expected path is one analysis plus one synchronization. Equal-score ties still require one analysis per tied candidate because confidence is meaningful only within that metadata tier.

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

Exit status is 0 for success, 1 for an operational failure, and 2 for invalid command usage. `serve`, `scan`, `search`, and `retry` take the nonblocking `/data/subsyncd.lock`; a second mutator fails immediately instead of racing the daemon. Stop the daemon before running a mutating one-shot command.

`explain` reports the indexed media identity, embedded/sidecar tracks, human-readable search priority, pending-rerun state, missing/failure attempts, last outcome and next attempt, candidates with score/identity evidence, active candidate rejections with reason and expiry, managed installation and LAPSE provenance, reusable pack count, and provider cooldowns. `search --retry-rejected` clears the rejection records for only the selected media/language before performing the manual search; any candidate that deterministically fails again is immediately re-quarantined.

## Silo notification

Enable Silo only after setting its native API URL and admin API key:

```yaml
silo:
  enabled: true
  url: http://silo:8090
  api_key: ${SILO_API_KEY}
  path_mappings:
    - from: /media
      to: /mnt/media
```

After a committed install, a durable notification sends `POST /api/v1/scan` with `Authorization: Bearer …` and the mapped media-file path. Silo resolves this to a targeted file scan, which refreshes its external-subtitle inventory. Notification failure never rolls back a subtitle. Timeout, 408, 429, and 5xx responses retry independently; other 4xx responses are terminal. See [the Silo protocol ledger](references/silo.md).

This adapter targets Silo's current pre-1.0 native API. Silo plans to retire `/api/v1` at 1.0, and the v2 scan route is not yet published. Check the protocol ledger and upgrade `subsyncd` before moving Silo past its dual-API bridge release; `subsyncd` deliberately does not guess or fall back between mutating API versions.

## Backup, restart, and recovery

For a consistent backup, stop the service and copy `/data` as one unit. It contains the SQLite database and both caches. Restoring only the database can leave pack manifests missing; those entries are detected and invalidated safely, but the cache benefit is lost.

Search and notification leases are recoverable after five minutes. A crash after atomic publication but before search completion does not trigger a second installation: the next inventory pass recognizes the checksum-recorded sidecar. Managed replacements retain one rollback file until a later successful replacement supersedes it; database-commit failure restores it immediately.

If a user wants to take ownership of a subtitle, edit or replace the sidecar. Its checksum then differs from provenance and `subsyncd` protects it. Use `explain` before removing any generated file; never delete arbitrary `.subsyncd-*` files while the daemon is running.

## Troubleshooting

- `another subsyncd mutation process is active`: stop the daemon or wait for the other one-shot mutator; do not delete the lock file to bypass a live lock.
- `media path is outside configured roots`: fix the Arr path mapping or mount path; do not broaden roots merely to silence the check.
- no provider call: check `explain` for an embedded/protected track, exact terminal installation, future schedule, pack cache, or provider cooldown.
- repeated missing result: this is expected backoff, not a worker sleep. Use `search` for a deliberate manual attempt or `retry` only to clear provider throttle/auth state.
- LAPSE rejection: inspect `candidate_rejections` with `explain`; `unsure`/`nothing` is intentionally not installable. Use `analyze-sync` for diagnosis or `search --retry-rejected` after changing the media, candidate source, LAPSE build, or policy.
- Silo does not refresh: confirm port 8090 reaches Silo's native API, the key is an admin API key, and the container-to-Silo path mapping is correct.
