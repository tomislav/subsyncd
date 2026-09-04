# Implementation status and handoff log

This file is the resumable implementation ledger. The approved design and plan remain authoritative; this records what the code actually implements and why.

## Current state

- Branch: `feat/subsyncd`
- Current task: complete through Task 15
- Next task: none; the approved implementation plan ends after Task 15
- Latest follow-up: hearing-impaired subtitles default off in `5a42290`
- Runtime module: `subsyncd` on Go 1.27
- Test caches: `GOCACHE=/tmp/subsyncd-gocache`, `GOMODCACHE=/tmp/subsyncd-gomodcache`

## Completed tasks

### Task 1 — domain and configuration

- Commit: `679035f feat: bootstrap subsyncd domain and configuration`
- Implemented canonical BCP 47 languages with explicit legacy aliases, strict single-document YAML, whole-scalar environment expansion, provider/language routing validation, path/root checks, and the approved example configuration.
- Added the pinned Bazarr behavior/decision index. Bazarr remains reference-only GPL-3.0 material.
- Verified with configuration/domain tests and `go vet ./...`.

### Task 2 — SQLite and schedules

- Commit: `6924360 feat: add persistent scheduling and provenance store`
- Implemented transactional embedded migrations, WAL/foreign-key/busy-timeout SQLite setup, typed media/inventory/search/provider/pack/installation persistence, recoverable leases, deterministic missing/failure/upgrade schedules, and an injected test clock.
- Important behavior: provider state is keyed by provider instance plus operation scope; pack eviction returns expired entries before live LRU entries; installation audit and provenance commit atomically.
- Verified with race-enabled `internal/store`, `internal/schedule`, and `internal/testutil` tests plus `go vet ./...`.

### Task 3 — Arr catalogs, webhooks, and path mapping

- Commit: `b047df2 feat: integrate Sonarr and Radarr catalogs`

- Webhook IDs are stable hashes of configured instance, normalized Arr event name, media kind, Arr file ID, size, path/previous-path, release evidence, and upgrade flag. Exact duplicate deliveries are database no-ops while later renames of the same file ID remain distinct.
- Imports and renames hydrate the authoritative file/episode-or-movie/series records from Arr before persistence. Deletes do not require a now-missing remote file resource.
- Content provenance invalidation compares Arr file ID, byte size, and nanosecond timestamp. A path-only rename retains candidate provenance while updating the path.
- Import/upgrade resets the missing schedule for every configured language. Delete events release/cancel pending jobs and remain in a bounded 10,000-row audit log.
- Reconciliation applies the full hydrated history page and cursor in one SQLite transaction, preventing a cursor gap after a partial failure.
- Path mapping tightens Bazarr behavior: longest boundary-aware remote prefix wins, both slash styles are accepted, case is preserved, traversal is rejected, and existing parent symlinks are resolved before media-root containment is accepted.
- Arr errors include only instance and status; untrusted upstream response bodies and API keys are never surfaced. Default client timeout is 15 seconds.
- Ordinary tests use sanitized fixture JSON and loopback fake servers only.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed. The full run exposed and retained a regression for SQLite partial unique indexes; targetless `ON CONFLICT DO NOTHING` is required for the partial `events(event_id)` index.

## Known follow-ups

- Reconciliation currently hydrates history rows that still identify a live file. Deletions are handled by delete webhooks; if an Arr history endpoint exposes reliable deletion tombstones, add them behind a contract fixture before changing this rule.

### Task 7 — Titlovi adapter

- Commit: `b803647 feat: add Titlovi provider`

- Strict provider configuration requires credentials, defaults to the HTTPS Kodi API and Titlovi download origin, and permits alternate origins only through an explicit host allowlist. Local HTTP is available solely through an unexported YAML test flag.
- Tokens and user IDs are cached until one minute before expiry. Search/download permit one 401-triggered refresh; common transport keeps credential-bearing URLs out of errors and persists no-header 429 responses as a five-minute provider-instance cooldown.
- The broad-only adapter maps every supported Titlovi language independently (`bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl`, `sl`), paginates to a configured ceiling, and retains title, release, rating, download count, and opaque result/download identity.
- Episode-zero results become season-pack evidence only when the returned season matches the target. They retain episode zero and never masquerade as a direct episode; wrong-season and wrong-episode rows are rejected.
- Downloads accept only configured HTTPS origins and redirects, stream through a compressed-byte ceiling, and leave ZIP/RAR member selection to the later common fail-closed extractor.
- Bazarr behavior adopted: token contract, provider language names, pagination, episode-zero pack signal, and typed 429 handling. Tightened: explicit pack scope, wrong-season rejection, bounded pages/bytes, host-safe redirects, and no archive extraction inside the provider.
- Verification: `go test ./... -race`, `go vet ./internal/provider/titlovi`, and scoped `git diff --check` passed.

### Task 8 — SubDL adapter

- Commit: `cd2afdf feat: add SubDL provider`

- Strict configuration requires an API key, defaults to the official HTTPS search and download origins, and uses an explicit download-host allowlist. The provider is broad-only and advertises season-pack plus direct-member capabilities.
- Search sends the strongest available IMDb/TMDB ID, original filename or title fallback, media type/year/language, release/HI/unpack/full-season flags, 30-result page size, and `client=custom_integration`.
- Episode searches run standard season/episode, optional absolute episode, and season-only variants; a title-only query runs only when all filtered variants are empty. Stable URL/name identities deduplicate the merged response.
- Provider media-result identity is retained for hard gating and scoring. All release names survive normalization. Explicit and release-text episode ranges must contain the standard or absolute target; direct unpack members are preferred, explicit full seasons become `PackSeason`, and unproven episode-zero rows fail closed.
- Search and download understand daily quota, rate-limit, and service-busy payloads without sleeping. Cooldowns persist by provider instance and operation. Downloads attach the API key only to an allowlisted origin, reject unsafe redirects, and stream through the compressed-byte ceiling.
- Current official contract was checked at <https://subdl.com/api-doc>; Bazarr was used only to compare multi-query/range behavior. We retain the official `full_season`, `unpack_files`, `client`, and optional authenticated-download behavior while rejecting Bazarr's arbitrary archive-member fallback.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — score/evidence-gated LAPSE

- Commit: `a8887ef feat: gate LAPSE by release confidence`
- The default `sync.policy: confidence` avoids a LAPSE media read for a non-pack first install only when the internally computed release score is at least 75 and contains an identity anchor (`external_id` or `title_year`) plus `release_group`; TV also requires direct, absolute, or parsed release-name episode evidence. Provider rating/popularity points cannot replace the anchors.
- Exact hashes retain the existing `exact_hash` bypass. Confidence-qualified installs persist `SyncResult{Verdict: "score_bypass", Mode: "bypass", Reference: "release_evidence"}` with zero LAPSE metrics so `explain` remains honest about what was and was not measured.
- Candidates below threshold or missing required evidence still run LAPSE. Packs and managed-subtitle upgrades run LAPSE by default regardless of score. `sync.policy: always` restores the previous every-nonexact behavior; the threshold and evidence/pack/upgrade guards are configurable, default true, and strict YAML validated.
- LAPSE remains installed and capability-checked because any uncertain candidate can need it. Its persistent speech profile cache remains under `<data_dir>/lapse-cache`; on network media storage, the first required analysis may read much or nearly all of the media file, while a score bypass performs no LAPSE media read.
- The existing broad-search black-box test explicitly selects `policy: always`, preserving coverage of real LAPSE analysis/synchronization. Workflow tests cover the score bypass, adjustable threshold, `always` mode, missing anchors, TV ambiguity, packs, and upgrades. Configuration tests cover defaults, explicit switches, invalid policy, and invalid thresholds.
- Verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race`, `go vet ./...`, tagged e2e tests, and `git diff --check` passed on 2026-09-04.

### Follow-up — persistent candidate rejection quarantine

- Commit: `42943d3 feat: quarantine rejected subtitle candidates`
- Migration `007_candidate_rejections.sql` adds a cascade-owned rejection ledger keyed by media/language/provider result, stable candidate signature, optional artifact checksum, and tool/policy signature. Each row retains the exact media fingerprint, reason code, rejection time, and 30-day expiry.
- Candidate signatures deliberately omit download references, ratings, popularity, and download counts so volatile provider data cannot trigger another download. Release/identity/pack evidence changes do invalidate the signature. Media fingerprint, known member checksum, LAPSE compatibility version, synchronization policy, expiry, or a manual clear also invalidate the applicable rejection match.
- LAPSE `unsure` and `nothing`, invalid or oversized subtitle payloads, and ambiguous pack selection are deterministic rejections. They are persisted immediately, removed before the top-three shortlist, and allow later-ranked results to advance on later jobs. Cached pack members are checked with their checksum; rejecting one episode does not remove the pack or affect another episode/media row.
- Process timeouts/crashes, malformed LAPSE protocol, filesystem errors, provider/network failures, and cancellation never create rejection records. A shortlist containing only such failures returns an error so the worker uses technical-failure backoff. No-speech remains a media-validation rejection and does not poison an individual candidate.
- `explain` lists active rejection reason, artifact checksum, and expiry. `search --retry-rejected` clears only the selected media/language ledger before the manual workflow; deterministic failures during that run are reinserted.
- Red/green tests cover SQLite match/expiry/clear semantics, media/tool/artifact invalidation, top-three promotion, stable candidate signatures, invalid payload quarantine, cached-pack suppression, transient LAPSE classification, CLI forwarding, manual clearing, explain output, and the exported LAPSE compatibility version. Verification passed with the complete race suite, `go vet ./...`, tagged e2e tests, and `git diff --check` on 2026-09-04.

### Task 9 — candidate matching and scoring

- Commit: `dd09fe8 feat: add explainable subtitle matching`

- `github.com/chill-institute/torrentname` v1.4.1 is pinned behind the local release parser. The wrapper preserves raw evidence and normalizes title, season/episode/range, group, source, resolution, complete-season state, edition/cut, and common streaming-service tokens.
- Identity comparison is punctuation/case normalized but never fuzzy. Known language, media kind, external-ID, movie-year, season/episode, pack-season, and edition conflicts reject before scoring; unknown metadata remains neutral. Standard and absolute pack ranges are accepted only when they contain the target.
- Exact hash is terminal at 100 after identity gates. Non-hash contributions are emitted in stable order for external ID (20), title/year (15), release group (25), source (15), edition (10), service (5), resolution (5), rating (0–3), and popularity (0–2), capped at 100.
- `Eligible` applies the configured threshold independently from evaluation. `Rank` deterministically orders by score, provider priority, normalized rating, normalized popularity, then stable provider/result identity.
- Provider download counts now share a logarithmic `[0,1]` normalization saturating at four orders of magnitude. This prevents popularity from overpowering identity while making its two score points usable by OpenSubtitles, Titlovi, and SubDL.
- Adopted Bazarr's small known release-group equivalence sets as behavior, with independently authored tests. Tightened scoring retains explicit zero-point explanations and rejects wrong-season pack metadata even when candidate top-level season is absent.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 10 — safe extraction and reusable season packs

- Commit: `8ff4405 feat: add safe reusable season-pack handling`

- One common extractor handles ZIP, RAR, and plain `.srt`, `.ass`, `.ssa`, or `.vtt` payloads. Defaults cap input at 20 MiB compressed, 100 MiB expanded, 100 files, and one directory level. Every archive member counts toward limits, including ignored extensions.
- Extraction rejects Unix and Windows absolute paths, traversal, symlinks and other special files, duplicate case-insensitive names, nested archives by extension or ZIP/RAR magic, excessive expansion, NUL/binary payloads, unsupported syntax, more than 100,000 cues, and negative or non-monotonic timestamps. Text is normalized to UTF-8/LF, accepting UTF-8 and Windows-1250 input, and is published from a same-parent staging directory.
- Subtitle syntax validation uses `github.com/asticode/go-astisub` v0.42.0. RAR streaming uses `github.com/nwaples/rardecode/v2` v2.4.1. ZIP, plain, and hostile archive branches have independent fixtures; the RAR decoder integration is compiled and bounded but still needs a provenance-safe valid RAR fixture in the later black-box suite.
- Member selection is fail-closed and ordered: provider direct member, unique `SxxEyy`/`xxXyy`, containing episode range, absolute `EP`/`ABS` token, then unique normalized episode-title similarity of at least 0.98 for titles longer than four characters. Range starts are not misclassified as single episodes, repeated direct evidence is deduplicated, and forced members are excluded from normal requests.
- Sonarr episode titles now hydrate into `domain.Media` and migration `004_episode_titles.sql` persists them. Both direct and webhook-transaction media upserts carry the field.
- The cache key hashes provider, result, language, and download checksum. Normalized members live below an exact 64-hex content directory with checksums and an SQLite manifest. Raw candidate and direct-member download references are scrubbed before persistence.
- Cache publication is immutable and rolls back if SQLite rejects the entry. Existing destinations must be real directories containing real regular files; cache reads reject symlinks and verify checksums. Expired entries are removed before live LRU entries, eviction renames to a tombstone before database deletion, and orphan cleanup is root-contained. The cache assumes one service process owns its configured cache root.
- Reuse matches any known TVDB, TMDB, IMDb, or title/year identity so a provider result keyed by one external ID can serve media carrying stronger additional IDs. Cached members remain subject to later matching/scoring and LAPSE validation.
- Bazarr behavior adopted: format filtering and numbered/ranged pack evidence. Tightened: universal bounded extraction, no arbitrary single-file fallback, strict ambiguity rejection, content addressing, raw-link scrubbing, checksum validation, symlink-safe reuse, and rollback across filesystem/database publication.
- Verification: focused red/green regressions plus `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 11 — LAPSE synchronization

- Commit: `d37f7e1 feat: integrate LAPSE subtitle synchronization`

- The compatibility baseline is official LAPSE v2.0.5 at tag commit `8d57e43`; current upstream HEAD inspected during implementation was `a0e99bd919e80f9f836cb4c6f6cc391c1a666dd6`. The CLI/JSON contract was verified from <https://github.com/Schwponaco-org/lapse> rather than inferred from Bazarr.
- Analysis runs LAPSE with the media path plus a private copy of the candidate and `--dry-run --json --strict --no-sidecar`. The original candidate is never supplied as LAPSE's writable input. LAPSE dry-run legitimately reports `written:true` as “would write,” so trust comes from the `solid` verdict and validated protocol, not file existence during analysis.
- Synchronization runs only to a caller-provided absent output using `--output PATH --no-backup --json --strict --no-sidecar`. It accepts only exit-0 `solid`, `written:true`, the exact reported output path, a real regular nonempty file, a supported text extension, valid UTF-8, parseable cues, and nonnegative monotonic timestamps.
- JSON decoding requires the complete v2.0.5 field set, rejects unknown/trailing fields, validates the documented modes and `vad`/`embedded`/`subtitle` references, requires finite bounded metrics, and checks cue/part/split consistency. A future LAPSE protocol addition intentionally fails closed until fixtures and the allowlist are reviewed.
- Strict `unsure` and `nothing` reports with exits 2/3 become typed `VerdictError` rejections. Early no-speech/no-audio failures without JSON become typed `NoSpeechError`. Nonzero failures, malformed/truncated JSON, missing output, invalid output, timeout, and protocol contradictions are rejected.
- Exact-hash candidate entry points return an `exact_hash` bypass result without invoking either LAPSE command. Task 12 must call these candidate-aware entry points rather than calling raw analysis/synchronization for exact results.
- The subprocess runner never uses a shell, caps stdout at 1 MiB and stderr at 64 KiB, replaces inherited `LAPSE_CACHE` with the configured persistent speech-cache directory, applies separate analysis/synchronization contexts, redacts media and temporary paths from surfaced errors, and kills the entire process group on cancellation.
- Verification: sanitized v2.0.5 JSON fixtures and fake runners cover solid/weak/nothing, unsafe metrics, malformed/trailing/oversized output, no speech, nonzero exits, timeouts, path redaction, output validation, exact-hash bypass, environment replacement, output caps, exit preservation, and descendant-process cancellation. `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 12 — workflow orchestration and atomic installation

- Commit: `fce31ab feat: orchestrate subtitle acquisition and upgrades`

- The workflow refreshes embedded/sidecar inventory before every attempt, stops for acceptable embedded or protected/user-managed subtitles, checks a reusable season pack before remote providers, and otherwise consumes the provider coordinator's exact-first/broad-fan-out result. A stale exact-phase error is cleared when that provider's broad phase succeeds.
- Every search result is identity-gated, explainably scored, and transactionally persisted without raw candidate or direct-member download references. Disallowed hearing-impaired candidates carry a persisted rejection reason. Task 12 originally defaulted `allow_hearing_impaired` to `true`; follow-up `5a42290` supersedes that policy, so the current default is `false` and only explicit `true` opts in.
- Exact hashes bypass LAPSE and reduce the download shortlist to that terminal result. Nonexact acquisition analyzes and synchronizes at most the top three eligible candidates. Final ordering is release score, LAPSE analysis confidence, configured provider priority, rating, then stable provider/result ID.
- Provider output is capped at 20 MiB at the workflow writer boundary even if an adapter ignores a short-write error, then sent through the common ZIP/RAR/plain extractor and strict episode selector. Ambiguous packs fail closed. Normalized season packs are cached only after successful selection; cache persistence failure is recorded but does not discard a valid candidate.
- Cache reuse now reloads the immutable manifest and reruns strict selection for the requested episode instead of trusting the first indexed member. Missing, malformed, symlinked, or checksum-mismatched entries are invalidated through root-contained cleanup. Ambiguity remains a typed rejection rather than a cache miss.
- Existing exact-hash installations are terminal. Managed nonexact files require a default 10-point improvement and schedule another upgrade check after 7, 30, or 90 days according to score. Media-content changes invalidate installed score, LAPSE result, and fingerprint provenance while retaining the old sidecar; path-only renames rebase managed sidecar/rollback paths and retain valid provenance.
- Installation validates a regular UTF-8 SRT/ASS/SSA/VTT source, cue order, and duration bounds; stages it beside a root-contained destination; fsyncs content; applies configured mode/ownership; rechecks ownership immediately before rename; atomically publishes and fsyncs the directory; and commits checksum, provider, candidate, score, sync result, and media fingerprint. A managed replacement keeps a rollback copy and restores it after any post-rename failure. User-modified, unmanaged, special, and symlink destinations are protected.
- Remote cooldown/quota/disabled results become a nonblocking throttled outcome with the earliest known reset. A partial provider outage can still yield a normal no-result or successful installation; a generic failure from every assigned provider returns a technical workflow error. Manual requests still honor provider cooldown and protected-file rules.
- Verification: focused red/green tests cover inventory ownership/HI policy, cached-pack fallback, exact bypass, three-candidate limit, confidence tie-breaks, outages, throttles, manual search, cancellation, ambiguity, LAPSE rejection, score upgrades, secret-free persistence, download limits, cache-write degradation, installation fault restoration, fingerprint invalidation, and rename rebasing. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 13 — durable workers and Silo notifications

- Commit: `f50b76c feat: add durable worker and Silo notifications`

- Search leasing now carries independent missing-result and technical-failure attempt indexes. SQLite compare-and-swap renewal/completion APIs prevent an old job owner from renewing or completing a lease reclaimed by another worker. Missing/rejected work advances only the jittered missing schedule; technical errors advance only the 1m/5m/15m/60m failure schedule; successful work resets both; provider throttles release immediately and reschedule at the reported reset plus up to 10% positive jitter without advancing either counter.
- Each cycle leases no more than 10 searches for five minutes, starts renewal for every claimed job (including jobs queued behind the semaphore), and runs no more than two media/language workflows concurrently. Renewal stops and joins before completion, closing the renewal-versus-completion race. Polling uses ±10% jitter. Graceful shutdown stops polling, allows a configurable drain, then cancels active work so unfinished leases remain recoverable.
- Multiple workers sharing SQLite cannot process the same lease. A crash/failure after a committed installation but before search completion leaves the lease recoverable; the next inventory pass sees the installed sidecar and does not perform a second installation. Repository media hydration now supplies the complete persisted `domain.Media` to workflow jobs.
- Per-instance reconcilers run immediately at startup and then every six hours. Each `catalog.Reconciler` continues to read and atomically advance its own persisted SQLite cursor, so a failed instance is retried independently while successful instances retain their progress.
- Migration `006_notification_leases.sql` adds partial unique notification deduplication plus a due-work index. Notification enqueue, lease, renewal, retry, and terminal completion are persistent. A committed installation enqueues one checksum-derived job per configured notifier before the search lease completes; deliveries run independently with bounded concurrency. Retryable failures use the technical backoff and never change acquisition state or revert a subtitle.
- The optional Silo adapter follows the documented current pre-1.0 native contract: `POST /api/v1/scan`, `Authorization: Bearer …`, one mapped media-file `path`, and 2xx success (Silo documents 202). It supports boundary-aware longest-prefix mount rewrites, rejects redirects and credential-bearing/invalid base URLs, uses a 15-second default timeout, treats timeout/408/429/5xx as retryable, and never includes the API key or response body in errors. The example targets Silo's main API listener on port 8090. See `docs/references/silo.md`.
- Bazarr was not used for Silo behavior; official Silo documentation is authoritative. The worker design independently tightens the nonblocking cooldown and durable-lease requirements from the approved service design.
- Verification: race-enabled tests cover renewal, completion ordering, two-worker exclusion, two-workflow concurrency, crash recovery, missing/failure/throttle accounting, poll/reset jitter, six-hour reconciliation, bounded shutdown, notification dedupe/retry isolation, disabled Silo, request contract, path mapping, authentication/status classification, timeout, redirect rejection, and secret redaction. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — native Silo scan API and focused README

- Commit: `4a394b1 feat: use Silo native scan API`
- Replaced the Jellyfin-compatibility notification with Silo's current native `POST /api/v1/scan` contract on the main listener: bearer admin API key, JSON media-file `path`, and accepted 2xx response. Durable retry isolation, path mapping, redirect rejection, and credential redaction are unchanged.
- Reworked `README.md` around a general feature list and removed its Bazarr references. Internal provenance/reference notes remain where they are useful to maintainers. Arr webhook connections remain deliberately manual; `subsyncd` neither creates nor modifies Sonarr/Radarr settings, while startup and six-hour reconciliation cover missed deliveries.
- Silo's API-v2 program plans a dual-API bridge followed by a Silo 1.0 `/api/v1` tombstone. The current migration ledger says the scan operation will be ported but leaves its v2 method, path, and operation ID unset. Do not guess or automatically replay the mutating request across versions; add an explicit versioned adapter with contract tests once Silo publishes the route and schema.
- Verification on 2026-09-04: `go test ./internal/notifier -race -v`, `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -v`, and `git diff --check` passed. The end-to-end flow asserts the native route, bearer header, mapped media path, accepted response, durable deduplication, and restart behavior.
- Next task: monitor Silo API-v2 issue #135, cutover issue #886, and migration-ledger PR #902; implement v2 only after the scan contract is assigned and published.

### Follow-up — configurable rootless container identity

- Commit: `ce023d5 feat: make container identity configurable`
- The image's baked-in unprivileged account and owned runtime directories now default to UID/GID `1000:1000`. Both Compose definitions interpolate host-side `PUID` and `PGID` into `user:` and `/tmp` tmpfs ownership, also defaulting to `1000:1000`.
- This deliberately is not a LinuxServer-style root entrypoint: the service does not read `PUID`/`PGID` from its container environment, create users, retain `CHOWN`/`SETUID` capabilities, or mutate bind-mount ownership. Operators set the values in `.env` or the invoking shell and must prepare `/data` and media permissions for that numeric identity.
- Compose passes `TZ`, defaulting to `Europe/Zagreb`; the runtime image explicitly installs timezone data. Arbitrary numeric identities do not require passwd/home lookup because `subsyncd` uses explicit configured paths and LAPSE cache locations.
- Red/green Compose checks proved that standalone and root-stack definitions previously ignored `PUID=1234 PGID=2345 TZ=UTC`, then rendered `user: 1234:2345`, matching tmpfs ownership, and `TZ=UTC` after the change. Default renders were separately checked as `1000:1000` and `Europe/Zagreb`.
- Verification on 2026-09-04: native arm64 image build; image metadata and runtime UID/GID checks; Zagreb timezone offset; default and arbitrary-identity `subsyncd --version` smoke runs; `go test ./... -race`; `go vet ./...`; tagged e2e tests; and `git diff --check`. The local image ID is recorded in `docs/release-notes.md`; it is not a registry digest.
- Next task: none. Before publication, retain the existing native-amd64 build gate from the initial release notes.

### Follow-up — hearing-impaired subtitles default off

- Commit: `5a42290 fix: default hearing-impaired subtitles off`
- Omitted `allow_hearing_impaired` now resolves to `false`; the shipped example also sets it to `false`. Explicit `allow_hearing_impaired: true` remains the opt-in for users who want SDH/HI tracks and candidates.
- With the default policy, an embedded SDH track does not satisfy an ordinary language request and a provider result marked hearing-impaired is rejected during candidate evaluation. Explicit opt-in restores both behaviors. Forced-only and unknown-language behavior is unchanged.
- Red/green coverage changed the configuration contract test before production code: it failed because omission still yielded `true`, then passed after the default changed while also proving explicit `true` is honored. Existing inventory and workflow policy tests passed unchanged.
- Verification on 2026-09-04: focused configuration/workflow/inventory race tests, complete `go test ./... -race -count=1`, `go vet ./...`, tagged e2e tests with `-count=1`, and `git diff --check` all passed.
- Next task: none.

### Task 14 — daemon, webhook API, and operational CLI

- Commit: `9d0d06a feat: expose subtitle daemon and CLI`

- `internal/app` now assembles strict configuration, media-root checks, SQLite/migrations, Arr catalogs and persisted instance rows, compiled-in providers, per-language ordered coordinators/workflows, embedded inventory, immutable pack cache, LAPSE, atomic installer, optional Silo notification, reconcilers, durable worker, and the HTTP handler. Assembly performs no Arr/provider/Silo network request, so those dependencies can be unavailable without blocking startup/readiness.
- Startup fails before serving when configuration/listen addresses are invalid, media roots do not exist as directories, SQLite cannot open/migrate, `ffprobe -version` fails, or LAPSE does not advertise `--json`, `--strict`, `--output`, `--no-sidecar`, and cache support (`--no-cache`). The pinned v2.0.5 executable prints usage to stderr and returns nonzero when invoked without arguments; it does not implement `--help` and treats that token as a media filename. A complete advertised capability set is therefore accepted regardless of the no-argument exit status; empty nonzero output fails. See <https://github.com/Schwponaco-org/lapse/tree/v2.0.5>.
- `serve` runs the worker and a time-bounded `net/http` server together. SIGINT/SIGTERM cancel the shared context; HTTP drains first with a 30-second ceiling and force-closes on timeout, then active worker work receives cancellation and recoverable leases remain in SQLite. Mutating processes are excluded by a nonblocking advisory lock at `<data_dir>/subsyncd.lock`.
- The HTTP surface is limited to `POST /webhooks/{instance}?token=...`, `GET /healthz`, and `GET /readyz`. Tokens are SHA-256-normalized before constant-time comparison; exactly one token is required. Bodies are capped at 1 MiB and must contain one complete JSON object. Unknown instances, invalid tokens, unsupported/missing event identity, test events, oversized input, and dependency failure have explicit generic status responses. Every response receives a bounded request ID, and structured logs contain the path without query strings or error bodies.
- Readiness rechecks only SQLite and local media roots. It never probes Arr, subtitle providers, or Silo. Arr non-2xx errors now omit untrusted response bodies from logs/CLI output. Configuration-aware CLI error redaction removes Arr/webhook/Silo/provider credentials and configured media-root prefixes.
- Webhook deduplication was tightened: exact redeliveries remain transactionally harmless, but stable IDs now include file/path/release/upgrade evidence so a later rename of the same Arr file ID is not suppressed. Catalog normalization exposes a typed invalid-webhook error for correct HTTP classification.
- The stable CLI surface is `serve`, `scan`, `search`, `retry`, `explain`, `doctor`, and `analyze-sync`, with exit codes 0/1/2 for success/operation failure/usage. Configuration defaults to `/config/config.yaml`, honors `SUBSYNCD_CONFIG`, and can be overridden by `--config` on every command. Flags unrelated to a command are rejected.
- `scan` reconciles one configured instance and can force-refresh embedded tracks for every indexed file. `search` hydrates current Arr metadata and calls the same protected-file/scoring/LAPSE/install workflow used by workers. `retry` clears every persisted throttle/auth scope for one configured provider. `explain` reports indexed identity, embedded/sidecar inventory, scheduling attempts/outcome, candidate score and identity evidence, installation/LAPSE provenance, reusable packs, and provider cooldowns. `analyze-sync` accepts only media inside configured roots, calls LAPSE analysis mode, and never installs output.
- The example configuration no longer advertises nonexistent `fallback_cooldowns` YAML; the documented provider-specific fallback values remain compiled policy in `provider.FallbackReset`.
- Verification: focused red/green tests cover HTTP authentication/body/JSON/status/readiness/request-ID/query-redaction behavior; CLI golden output, strict flags, exit codes, and backend redaction; real compiled provider assembly without network calls; startup diagnostics; local readiness; advisory-lock exclusion; and HTTP/worker drain. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 15 — packaging, end-to-end verification, and operator documentation

- Commit: `5619812 docs: package and document subsyncd`
- The multi-stage production image builds a static Go 1.27.0 service and installs checksum-pinned LAPSE v2.0.5 assets for Linux amd64/arm64. Debian 13.2 is intentional: the upstream LAPSE executable needs glibc 2.38+, which the initially tested Debian 12 runtime did not provide. The final runtime includes FFmpeg/FFprobe, runs as fixed UID/GID 10001, and declares a readiness health check.
- LAPSE capability discovery was corrected against the packaged executable: v2.0.5 exposes usage only on an empty invocation and interprets `--help` as a media filename. Startup now checks the real no-argument usage contract. The v2.0.5 release archive's executable reports internal version `2.0.0`; both values are recorded rather than conflated.
- Compose examples use a read-only root filesystem, read-only configuration, writable data/media mounts, bounded tmpfs, dropped capabilities, no-new-privileges, init, a 45-second stop grace period, resource limits, health checks, external media networking, and credential environment variables with no embedded secrets. The root stack keeps the service behind the opt-in `subsyncd` profile.
- Installation mode defaults to `0644` and accepts a nonzero octal mode without execute bits; optional nonnegative UID/GID ownership is applied by the atomic installer. The shipped example config has its own load test.
- Tagged black-box tests cover embedded-language provider avoidance, OpenSubtitles exact-hash terminal installation without broad search/LAPSE, broad search plus LAPSE plus atomic sidecar plus Silo, and restart/webhook deduplication without redownload or renotification. Credential-gated OpenSubtitles/SubDL/Titlovi contracts are excluded from ordinary CI and skip independently without credentials.
- The workflow now always replaces catalog fingerprint evidence with the freshly statted inventory fingerprint before hash-capable provider search. Hash calculation and persistence therefore use the authoritative current path, Arr file ID, size, and nanosecond mtime rather than a potentially stale catalog timestamp.
- `README.md`, `docs/providers.md`, and `docs/operations.md` document language routing, provider constraints, hash/embedded caches, live sidecars, exact/broad search, scoring, packs, throttling, backoffs, upgrades, LAPSE, Silo, webhooks, CLI, permissions, backup, restart, and recovery. `docs/release-notes.md` records resolved dependencies, checksums, image identity, and the remaining native-amd64 publication gate.
- Verification: `go test ./... -race`, `go vet ./...`, tagged e2e tests, provider-contract compilation/no-credential skips, all Compose renderings, native legacy-Docker build, version smoke, and real in-container `doctor` passed. `go list -m all` contains no Bazarr dependency. The final verified local arm64 image ID is `sha256:2051a4f528c586dda1046436f3a0072612632fc8158ecc62dbbaa35ac5418384`.

### Task 4 — embedded and sidecar inventory

- Commit: `0f69f8d feat: cache embedded subtitle inventory`

- FFprobe is invoked through an injectable runner with the exact approved arguments. JSON stdout is bounded to 8 MiB; failure text does not expose captured stderr.
- Every subtitle stream is inventoried, including image codecs such as PGS. Unknown/missing languages remain visible with an empty canonical language but never satisfy a configured language.
- `Inventory.Satisfies` requires an equivalent known language and a full subtitle. Forced-only tracks never satisfy the normal-language request; SDH satisfies only when hearing-impaired subtitles are allowed.
- The cached embedded inventory is reused only when path, Arr file ID, byte size, and nanosecond modification time match. `forceProbe` always bypasses the cache.
- Sidecars are rescanned on every refresh, only for the exact media stem and `.srt`, `.ass`, `.ssa`, or `.vtt`; directory recursion and symlink following are forbidden.
- An external file is considered managed only when both normalized path and SHA-256 checksum match installation provenance. All other external tracks are protected.
- Fingerprint update and complete track replacement occur in one SQLite transaction so a crash cannot pair a new fingerprint with stale embedded rows.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 6 — OpenSubtitles adapter

- Commit: `53930be feat: add OpenSubtitles provider`
- Follow-up: `884c0fe feat: cache OpenSubtitles file hashes`

- The OpenSubtitles file hash is delegated to `github.com/opensubtitlescli/moviehash` v0.1.1 behind a local `Hasher`; independent little-endian reference tests verify the first/last 64-KiB plus size algorithm and unchanged file offset. Files below 128 KiB and non-random-access sources return a typed unsupported error.
- Strict provider YAML requires API key, username, password, and user agent. The configured base URL is injectable only for contract tests; the production default is `https://api.opensubtitles.com/api/v1`.
- API key and user agent accompany all calls. Login tokens cache until one minute before expiry. Search and download each allow exactly one immediate 401-triggered refresh; a second rejection disables only the named provider instance via persistent `auth` state.
- Exact search sends movie hash plus byte size. The hash is calculated lazily on the first exact search and persisted in SQLite with its path, Arr file ID, size, and nanosecond modification time. Later exact searches reuse it; a changed fingerprint is a cache miss, and a concurrent fingerprint change rejects the stale hash write. Broad search sends the strongest IMDb/TMDB identity, title/year, and episode coordinates. Results preserve release/file names, language, HI, rating, count, IDs, and explicit hash-match evidence; inferred season packs are forbidden.
- OpenSubtitles custom codes round-trip as `pt ↔ pt-PT`, `zh ↔ zh-CN`, and `es-MX ↔ ea`.
- A 406 response persists download-scope quota reset and returns `QuotaError`; 429 and rate headers use the common transport. Temporary links are request-local, safety-checked, and streamed through the configured size ceiling.
- Common transport errors deliberately omit upstream URLs and wrapped network text so signed queries, API keys, and tokens cannot leak through errors.
- Verification: the original task passed `go test ./... -race`, `go vet ./...`, and `git diff --check`; the hash-cache follow-up passed race-enabled tests and vet for `internal/store` and `internal/provider/opensubtitles` plus scoped `git diff --check`.

### Task 5 — provider registry, coordinator, and throttling

- Commit: `6cf5c97 feat: add provider registry and persistent throttling`

- Providers are compiled-in factories producing independently credentialed named instances. The registry validates duplicate IDs, unknown types, provider-specific strict YAML, and language capabilities before workers start.
- Exact-hash-capable providers run sequentially in configured order; the first exact result stops all further searches. If none succeeds, all providers assigned to that language receive one concurrent broad query. Results merge in configured order and failures remain provider-local.
- Normalized search results cache for six hours by provider instance, search mode, media/file fingerprint, language, episode identity, and release name. HTTP(S) download URLs are stripped before persistence; opaque provider IDs may be retained.
- Each provider instance owns a token bucket and active-request semaphore. A separate host-origin semaphore coordinates multiple accounts/types using the same upstream origin.
- Cooldowns and quotas persist by provider instance plus `all`, `search`, `download`, or `auth` scope. An exhausted future window prevents the HTTP call and returns a typed reset immediately; workers never sleep through remote cooldowns.
- Parsed response evidence includes named `RateLimit`/`RateLimit-Policy` windows, `X-RateLimit-*`, seconds/date `Retry-After`, and provider JSON resets. Past/skewed resets are ignored and the most restrictive future reset wins.
- No-header fallbacks are Titlovi rate limit 5 minutes; OpenSubtitles rate limit 1 minute and download quota 6 hours; SubDL rate limit 15 minutes, daily quota until next GMT midnight plus 15 minutes, and service busy 1 hour.
- Added `golang.org/x/time/rate` v0.15.0 for local token-bucket pacing.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.
