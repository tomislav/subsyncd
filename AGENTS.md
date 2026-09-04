# subsyncd contributor guide

Read these documents before changing behavior:

1. `../docs/superpowers/specs/2026-09-03-focused-subtitle-service-design.md`
2. `../docs/superpowers/plans/2026-09-04-focused-subtitle-service.md`
3. `docs/implementation-status.md`
4. `docs/references/bazarr.md` for the task being implemented
5. `docs/references/silo.md` before changing Silo notification behavior

The design is authoritative. Bazarr commit `da73aeaf5e4d89ad86c8d559d3abd0e4129b24b2` is a GPL-3.0 behavioral reference only. Do not copy, vendor, execute, or translate its Python implementation or fixtures.

Use test-driven development: add one focused failing test, confirm the expected failure, implement the smallest production change, then run the affected package with `-race`. Ordinary tests must use sanitized fixtures and local fake servers; they must never contact Arr applications or subtitle providers.

Important invariants:

- Canonical language identities are BCP 47 tags.
- Hearing-impaired subtitles are allowed by default. When disabled, both existing SDH tracks and remote HI candidates are ignored/rejected with an explainable reason.
- Embedded subtitle inventory is fingerprint-cached; sidecars are scanned live before searches.
- Provider file hashes are calculated lazily, persisted by algorithm, and reusable only for an exact path/file-ID/size/mtime fingerprint.
- Providers are compiled-in adapters behind the common interface.
- Remote provider cooldowns are persisted and release worker leases; provider code must not sleep through them.
- All provider downloads are capped at 20 MiB by the workflow before reaching the common bounded extractor, even if an adapter mishandles writer errors. ZIP, RAR, and plain subtitle payloads are accepted; traversal, links, nested archives, decompression-limit violations, invalid subtitle syntax, and ambiguous season-pack members fail closed.
- A season-pack member must be uniquely identified by provider evidence, episode/range/absolute tokens, or the strict episode-title rule. Never select the first arbitrary archive member.
- Pack-cache directories are content-addressed and immutable. A cache hit reloads its manifest and reruns the normal strict selector for the current episode. Never persist candidate download references, follow cache symlinks, or delete paths outside the exact cache layout. Cache writes are best-effort and must not reject an otherwise valid installation.
- Sonarr episode titles are persisted because they are part of strict pack-member evidence; schema changes must update both normal and event-transaction media upserts.
- LAPSE is the only synchronization engine. The compatibility baseline is LAPSE v2.0.5. Exact hashes bypass it; the default confidence policy also permits a non-pack first install with score >=75, identity evidence, release-group evidence, and TV episode evidence to use the auditable `score_bypass` verdict. Uncertain candidates, packs, and upgrades require LAPSE `solid` by default. Rating/popularity points cannot replace required anchors.
- Invoke LAPSE through argument arrays with `--json --strict --no-sidecar`; analysis also uses `--dry-run` on a private copy, while synchronization uses an explicit new `--output` plus `--no-backup`. Cancellation must kill the subprocess group.
- LAPSE JSON is an intentionally strict protocol boundary. When upgrading LAPSE, update the documented mode/reference/field allowlists and sanitized fixtures together after checking the official source contract.
- The workflow persists every scored candidate without download references and prepares at most the best three eligible non-hash files. It installs only an exact hash, a policy-qualified `score_bypass`, or LAPSE `solid` result. `sync.policy: always` disables score bypass. Exact-hash installations are terminal; nonexact upgrades require the configured score delta (default 10).
- Deterministic `lapse_unsure`, `lapse_nothing`, invalid/oversized payload, and ambiguous pack-selection failures persist in `candidate_rejections` for 30 days and are filtered before the top-three shortlist. Rejection matching includes the exact media fingerprint, stable candidate evidence, optional artifact checksum, LAPSE compatibility version, and synchronization policy; never include volatile ratings/counts or download references. Cached pack rejection is member/media scoped; never delete or blacklist the whole pack/provider. `search --retry-rejected` is the explicit media/language-scoped override.
- LAPSE process/protocol, filesystem, provider, network, and cancellation failures are technical and must never poison candidate rejection state. No-speech is media-scoped and must not blacklist an individual candidate. A technical-only failed shortlist returns an error so workers advance failure backoff; deterministic-only rejection advances missing-result backoff.
- Only unchanged files owned by this service may be upgraded. A content fingerprint change invalidates old score/sync provenance; a path-only media rename rebases managed paths and retains it.
- Every media write is root-contained, syntax-validated, staged beside its destination, fsynced, atomically renamed, checksum-recorded, and auditable. Managed replacements retain a rollback copy and restore it if the post-rename database commit fails.
- Search workers lease at most 10 jobs for five minutes, renew once per minute (including queued leases), and execute at most two media/language workflows concurrently. Renewal must stop before compare-and-swap completion. Cancellation stops new polling, permits a bounded drain, then cancels active work so leases can recover.
- Missing/rejected results advance only the missing-attempt schedule; technical failures advance only the failure schedule; provider throttles advance neither and are scheduled at or after the provider reset. Successful/installed outcomes reset both counters.
- A committed installation enqueues a checksum-deduplicated notification before its search lease completes. Notification leases, attempts, and retry times are independent from subtitle acquisition; Silo failure never rolls back a valid subtitle.
- Silo integration is limited to its documented current pre-1.0 native `POST /api/v1/scan` contract on the main API listener (normally port 8090), authenticated with an admin API key in `Authorization: Bearer …`. Send only the mapped media-file path for a targeted scan, reject redirects, and never persist or surface the API key. Silo plans to tombstone `/api/v1` at 1.0; its v2 scan method/path/schema are not yet published, so do not guess or automatically replay a mutating notification across versions. Add a separately tested versioned adapter after the official contract lands; see `docs/references/silo.md`.
- The only HTTP routes are authenticated `POST /webhooks/{instance}`, `GET /healthz`, and `GET /readyz`. Webhook bodies are capped at 1 MiB, query tokens are compared as fixed-size SHA-256 values, response/log errors are generic, and structured logs use the path without its query string.
- Webhook IDs include the instance, event type, kind, file ID, size, path/previous-path, release evidence, and upgrade flag. Exact redeliveries are no-ops, but a later rename of the same Arr file ID must produce a new event ID.
- Startup and readiness are intentionally local/offline. Startup validates configuration, existing media-root directories, SQLite migrations, LAPSE usage capabilities, and `ffprobe -version`; it never requires a live Arr/provider/Silo response. Runtime readiness rechecks SQLite and media roots only.
- LAPSE v2.0.5 prints its usage to stderr and returns a nonzero status when invoked without arguments. It does not implement `--help`; that token is treated as an input filename. Capability detection must use an empty invocation and trust the presence of `--json`, `--strict`, `--output`, `--no-sidecar`, and `--no-cache`, while empty nonzero output or process failure is incompatible.
- The daemon owns a nonblocking advisory lock at `<data_dir>/subsyncd.lock`. `serve`, `scan`, `search`, and `retry` are mutating operations and must take it; `explain`, `doctor`, and `analyze-sync` are read-only. A second mutator must fail rather than race the daemon.
- CLI commands route through the assembled services, not HTTP management endpoints. The configuration path defaults to `/config/config.yaml`, can be set with `SUBSYNCD_CONFIG`, and every command accepts an explicit `--config`. Backend errors must pass through configuration-aware secret/media-root redaction before reaching stderr.
- Language routing is assembled as one workflow per canonical language, preserving that language's configured provider order. Do not use one global provider list for all worker jobs.
- Hearing-impaired/SDH tracks and candidates are disallowed by default. Only an explicit `allow_hearing_impaired: true` makes them satisfy inventory or remain eligible during candidate scoring.
- The production image is multi-stage, fixed to Go 1.27.0, Debian 13.2, and checksummed LAPSE v2.0.5 amd64/arm64 release archives. Debian 12 is incompatible with the upstream LAPSE binary's glibc requirement. It defaults to UID/GID 1000; Compose may override the rootless runtime identity through host-side `PUID`/`PGID` interpolation and must apply the same IDs to `/tmp` tmpfs. Do not add a root entrypoint or automatic ownership changes. The root filesystem is read-only; configuration is read-only and only data, temporary, and media mounts are writable.
- Container builds must work with BuildKit target arguments and legacy native Docker. When `TARGETARCH` is absent, derive the native architecture inside each build stage; never silently combine an amd64 LAPSE binary with an arm64 runtime or vice versa.
- Tagged `test/e2e` tests own the black-box embedded skip, exact-hash bypass, broad/LAPSE/install/Silo, and restart-dedup paths. Real provider contracts stay behind `provider_contract` and must skip without credentials.

Local verification uses writable caches:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
```

Update `docs/implementation-status.md` at every completed task boundary with the commit, adopted/tightened/rejected behavior, tests run, and next task.
