# Implementation status and handoff log

This file is the resumable implementation ledger. The approved design and plan remain authoritative; this records what the code actually implements and why.

## Current state

- Branch: `feat/subsyncd`
- Current task: Task 10, archive extraction and pack selection
- Next task: Task 11, LAPSE integration
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

- Webhook IDs are stable hashes of configured instance, normalized Arr event name, media kind, and Arr file ID. Duplicate deliveries are database no-ops.
- Imports and renames hydrate the authoritative file/episode-or-movie/series records from Arr before persistence. Deletes do not require a now-missing remote file resource.
- Content provenance invalidation compares Arr file ID, byte size, and nanosecond timestamp. A path-only rename retains candidate provenance while updating the path.
- Import/upgrade resets the missing schedule for every configured language. Delete events release/cancel pending jobs and remain in a bounded 10,000-row audit log.
- Reconciliation applies the full hydrated history page and cursor in one SQLite transaction, preventing a cursor gap after a partial failure.
- Path mapping tightens Bazarr behavior: longest boundary-aware remote prefix wins, both slash styles are accepted, case is preserved, traversal is rejected, and existing parent symlinks are resolved before media-root containment is accepted.
- Arr errors include instance/status and at most 4 KiB of response text; API keys are never included. Default client timeout is 15 seconds.
- Ordinary tests use sanitized fixture JSON and loopback fake servers only.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed. The full run exposed and retained a regression for SQLite partial unique indexes; targetless `ON CONFLICT DO NOTHING` is required for the partial `events(event_id)` index.

## Known follow-ups

- The application wiring will create/ensure configured Arr instance rows before reconciliation starts.
- HTTP webhook token validation belongs to the HTTP API task; catalog normalization deliberately accepts bytes only after transport authentication.
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

### Task 9 — candidate matching and scoring

- Commit: `dd09fe8 feat: add explainable subtitle matching`

- `github.com/chill-institute/torrentname` v1.4.1 is pinned behind the local release parser. The wrapper preserves raw evidence and normalizes title, season/episode/range, group, source, resolution, complete-season state, edition/cut, and common streaming-service tokens.
- Identity comparison is punctuation/case normalized but never fuzzy. Known language, media kind, external-ID, movie-year, season/episode, pack-season, and edition conflicts reject before scoring; unknown metadata remains neutral. Standard and absolute pack ranges are accepted only when they contain the target.
- Exact hash is terminal at 100 after identity gates. Non-hash contributions are emitted in stable order for external ID (20), title/year (15), release group (25), source (15), edition (10), service (5), resolution (5), rating (0–3), and popularity (0–2), capped at 100.
- `Eligible` applies the configured threshold independently from evaluation. `Rank` deterministically orders by score, provider priority, normalized rating, normalized popularity, then stable provider/result identity.
- Provider download counts now share a logarithmic `[0,1]` normalization saturating at four orders of magnitude. This prevents popularity from overpowering identity while making its two score points usable by OpenSubtitles, Titlovi, and SubDL.
- Adopted Bazarr's small known release-group equivalence sets as behavior, with independently authored tests. Tightened scoring retains explicit zero-point explanations and rejects wrong-season pack metadata even when candidate top-level season is absent.
- Verification: `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

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
