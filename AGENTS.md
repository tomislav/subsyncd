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
- LAPSE is the only synchronization engine. The compatibility baseline is LAPSE v2.0.5; non-exact candidates require its documented `solid` JSON verdict. Its strict weak verdicts return exit 2, and dry-run reports `written:true` to mean “would write.”
- Invoke LAPSE through argument arrays with `--json --strict --no-sidecar`; analysis also uses `--dry-run` on a private copy, while synchronization uses an explicit new `--output` plus `--no-backup`. Cancellation must kill the subprocess group.
- LAPSE JSON is an intentionally strict protocol boundary. When upgrading LAPSE, update the documented mode/reference/field allowlists and sanitized fixtures together after checking the official source contract.
- The workflow persists every scored candidate without download references, analyzes at most the best three eligible non-hash files, and installs only an exact hash or LAPSE `solid` result. Exact-hash installations are terminal; nonexact upgrades require the configured score delta (default 10).
- Only unchanged files owned by this service may be upgraded. A content fingerprint change invalidates old score/sync provenance; a path-only media rename rebases managed paths and retains it.
- Every media write is root-contained, syntax-validated, staged beside its destination, fsynced, atomically renamed, checksum-recorded, and auditable. Managed replacements retain a rollback copy and restore it if the post-rename database commit fails.
- Search workers lease at most 10 jobs for five minutes, renew once per minute (including queued leases), and execute at most two media/language workflows concurrently. Renewal must stop before compare-and-swap completion. Cancellation stops new polling, permits a bounded drain, then cancels active work so leases can recover.
- Missing/rejected results advance only the missing-attempt schedule; technical failures advance only the failure schedule; provider throttles advance neither and are scheduled at or after the provider reset. Successful/installed outcomes reset both counters.
- A committed installation enqueues a checksum-deduplicated notification before its search lease completes. Notification leases, attempts, and retry times are independent from subtitle acquisition; Silo failure never rolls back a valid subtitle.
- Silo integration is limited to its documented Jellyfin-compatible `POST /Library/Media/Updated` contract on the compatibility listener (normally port 8096), authenticated with `X-Emby-Token`. Send the mapped media-file path as `Modified`, reject redirects, and never persist or surface the API key.
- The only HTTP routes are authenticated `POST /webhooks/{instance}`, `GET /healthz`, and `GET /readyz`. Webhook bodies are capped at 1 MiB, query tokens are compared as fixed-size SHA-256 values, response/log errors are generic, and structured logs use the path without its query string.
- Webhook IDs include the instance, event type, kind, file ID, size, path/previous-path, release evidence, and upgrade flag. Exact redeliveries are no-ops, but a later rename of the same Arr file ID must produce a new event ID.
- Startup and readiness are intentionally local/offline. Startup validates configuration, existing media-root directories, SQLite migrations, LAPSE help capabilities, and `ffprobe -version`; it never requires a live Arr/provider/Silo response. Runtime readiness rechecks SQLite and media roots only.
- LAPSE v2.0.5 prints its usage to stderr and may return a nonzero status for `--help`. Capability detection therefore trusts the presence of `--json`, `--strict`, `--output`, `--no-sidecar`, and `--no-cache`, while an empty nonzero response or failed process is incompatible.
- The daemon owns a nonblocking advisory lock at `<data_dir>/subsyncd.lock`. `serve`, `scan`, `search`, and `retry` are mutating operations and must take it; `explain`, `doctor`, and `analyze-sync` are read-only. A second mutator must fail rather than race the daemon.
- CLI commands route through the assembled services, not HTTP management endpoints. The configuration path defaults to `/config/config.yaml`, can be set with `SUBSYNCD_CONFIG`, and every command accepts an explicit `--config`. Backend errors must pass through configuration-aware secret/media-root redaction before reaching stderr.
- Language routing is assembled as one workflow per canonical language, preserving that language's configured provider order. Do not use one global provider list for all worker jobs.

Local verification uses writable caches:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
```

Update `docs/implementation-status.md` at every completed task boundary with the commit, adopted/tightened/rejected behavior, tests run, and next task.
