# Implementation status and handoff log

This file is the resumable implementation ledger. The approved design and plan remain authoritative; this records what the code actually implements and why.

## Current state

- Branch: `main`
- Current task: single-run LAPSE preparation complete; independent reviews and verification passed
- Next safe action: publish the reviewed commits to GitHub main; production rollout is a separate operator action
- Latest follow-up: single-run runtime `cfcb21c` and guidance `98a22cc` passed independent task and whole-branch review; publication follows this ledger commit
- Runtime module: `subsyncd` on Go 1.27.1
- Test caches: `GOCACHE=/tmp/subsyncd-gocache`, `GOMODCACHE=/tmp/subsyncd-gomodcache`

## Completed tasks

### Follow-up — README scoring feature, 2026-09-06

- Commit: this documentation update commit, based on `ede363e`.
- Replaced the release-aware matching feature bullet with the approved plain-language description of candidate scoring and conditional LAPSE verification. No runtime behavior changed.
- Combined matching and synchronization into one feature bullet. Clarified that upgrades replace service-installed subtitles with better matches while protecting manually added or edited files; missing-subtitle searches now have their own bullet.
- Removed the implementation-status link from the README at the user's request; the contributor ledger remains available through AGENTS.md.
- Removed collector-specific references from the README and the detailed pipeline/query examples from the operations guide. Retained JSON logging, log levels, privacy behavior, workflow diagnostics, and Docker Compose log commands; updated the logging anchor.
- Verification: checked README wording, logging links, absence of collector-specific content in the README/operations guide, and `git diff --check`; runtime tests are unnecessary for these documentation-only changes.
- Simplified the operations and provider guides around setup, everyday commands, matching, upgrades, and troubleshooting. Added a concise logging guide; moved detailed persistence, scoring, scheduling, and event contracts to `docs/development/` and corrected their relative links. No runtime behavior changed.
- Guide verification passed: all local Markdown links resolve, user-guide code fences balance, README logging links target the new guide, provider defaults and Compose CLI examples match configuration parsers and the image entrypoint, and `git diff --check` is clean. No runtime tests or live-service requests were needed.
- Publication: included in the user-requested push to GitHub `main`.
- Next task: retain the existing operational follow-ups.

### Remaining systems code review — 2026-09-05

- Reviewed `9d292c4`, the pushed temporary workspace/LAPSE repair. Commit: `docs: record remaining systems review findings` (this documentation commit, based on `9d292c4`).
- Recorded thirteen actionable findings in `docs/remaining-systems-review-2026-09-05.md`: Arr redirect credentials, stale inventory writes, missing completed-probe cache identity, writes before mutation locking, mixed webhook batches, root Arr mappings, detail-client error privacy, unbounded retained history, pre-dedup hydration, post-cancellation dispatch, obsolete language work, Compose shutdown budget, and duplicate unsanitized startup errors.
- Two findings are P1 (redirect credential forwarding and stale inventory identity overwrite); eleven are P2. No runtime behavior was adopted or changed by this review. Existing hash-cache ownership, upgrade guards, migration lineage validation, publication parity, and previously reviewed rollback/outbox handling had no additional confirmed findings in this bounded pass.
- Evidence uses sanitized temporary Go overlays, in-memory HTTP transports, and temporary SQLite/media fixtures. Eleven findings were exercised through focused reproductions; retained-history sizing and Compose shutdown timing were established statically. The repaired baseline passed race/vet/local tagged E2E; release Dockerfile parity and documentation diff checks also passed. No live Arr/provider/Silo requests or production operations occurred.
- Next action: implement findings with permanent focused RED/GREEN tests when requested; repair redirect handling and stale inventory identity first. No new review finding has been silently implemented.

### Temporary workspace and LAPSE downstream repairs — 2026-09-05

- Commit: `fix: isolate subtitle scratch and guard LAPSE installation` (this commit, based on `9a35743`), authorized by the user's request to fix related issues and push, extended by the LAPSE downstream review request.
- Workflow scratch now uses the system temporary directory (`TMPDIR`, `/tmp` in the container), including downloads, extraction, and synchronized outputs. Raw payloads and discarded artifacts are released promptly, while viable tied candidates and immutable cached pack members remain available. Final staging/rollback and pack-cache publication retain their same-filesystem atomic boundaries. Old production scratch directories are not swept.
- Related review tightened temporary-root error redaction and removed raw LAPSE stderr, diagnostic fields, decoder values, and runner errors from outward errors. Typed verdict/no-speech/cancellation semantics remain intact; LAPSE version, scoring weights, bypass policy, and shortlist size do not change.
- Downstream review found candidate-local installer rejection aborting fallback and missing current-media guards. Repairs advance on direct pre-publication content rejection or unsupported format-changing upgrade, retain original artifact rejection identity, and keep filesystem/database/rollback failures terminal. Current filesystem and transactional stored media fingerprints guard publication/provenance/outbox against stale processing.
- Focused RED/GREEN tests cover scratch location and cleanup, exact fallback retention, full-manifest cache publication, reordered tied candidates, partial synchronized output cleanup, error privacy, broad/cache installation fallback, and media changes before publication/commit. Independent final review reported no actionable findings. Verification passed: repository-wide race tests with an affected workflow/store rerun after correcting a fixture directory-count expectation, `go vet ./...`, local tagged E2E race tests, gofmt, and `git diff --check`. HTTP tests used local fakes only.
- Existing rollback restoration and asynchronous notification delivery needed no structural changes. No production lifecycle actions, live provider requests, media cleanup, configuration changes, or threshold adjustments were performed. Published as `9d292c4`; the subsequent remaining-system review is recorded above. Production rollout remains separate.

### Read-only verification of deployed provider repairs — 2026-09-05

- Observed production build `sha-9a35743` running healthy with zero restarts/OOM after startup at 18:56 UTC. Startup/readiness and Radarr reconciliation succeeded; all 168 bounded log entries examined were info, with no warnings/errors.
- OpenSubtitles and SubDL broad searches returned results; Titlovi searches and downloads succeeded. Two subtitle installations completed with LAPSE `solid`, and both Silo notifications were delivered. Nineteen jobs completed at the log snapshot: ten satisfied, two installed, four rejected, and three no-result. A subsequent read-only metadata check found one unexpired active lease and no persisted provider states.
- A Croatian movie search returned nine candidates but none reached configured minimum score 35. Best scores were 34 (title/year15 + source15 + rating2 + popularity2); removed request-derived IMDb evidence correctly contributes zero. This is an eligibility-policy consequence worth assessing with corrected scores, not a provider outage. No threshold was changed.
- Verification used bounded logs/status plus SQLite `mode=ro`/`query_only` and only an allowlisted numeric configuration value. No service lifecycle actions, database writes, provider searches, notification tests, or media mutations were performed. No tests needed for this diagnosis-only ledger entry; recorded with the temporary workspace repair commit, based on `9a35743`. Next action: assess minimum-score policy if requested; deployment itself is healthy in the observed window.

### Provider review repairs — 2026-09-05

- Commit: `fix: repair provider identity and transport edge cases` (this commit, based on `39f5e20`), authorized by the user's request to fix all findings and push to GitHub.
- Adopted all eleven repairs in `docs/provider-review-2026-09-05.md`: response-derived Titlovi identity, complete token-refresh pagination, strict SubDL range parsing, absolute episode/range evidence, season-aware unique direct members, queued availability rechecks, body-network failure circuits, persistent rejected-login state, validated OpenSubtitles session routing, separately gated issued-link redemption, and credential-free file requests.
- Tightened body completion: bounded JSON reads reach EOF before decoding; genuine network failures preserve typed cooldowns and safe error text. Caller cancellation, malformed payloads, local writer errors, and early Close do not create remote circuits. Rate headers preserve transient streaks until completion, and exhausted quota survives body failures. Underlying EOF releases permits even when recovery-state persistence fails. Typed cooldown/quota classification precedes timeout/cancellation classification for provider events.
- Normalized cache version advances to `candidate-v5`; old inferred identity and malformed episode evidence cannot be reused. No database migration, schedule reset, installation takeover, automatic scan, or scoring-weight/shortlist/LAPSE-policy change was added. Optional review observations remain follow-ups.
- Permanent focused RED/GREEN tests cover each finding, downstream scoring evidence, timeout versus caller cancellation, quota preservation, ordinary JSON recovery, trusted/private host boundaries, credential-free CDN transfers, stale caches, and EOF permit ownership. Existing tests were adjusted only where successful-body completion intentionally replaces header-time recovery.
- Final verification passed: `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, affected provider/pack/application race tests, `./scripts/verify-release-dockerfile.sh`, gofmt and `git diff --check`. HTTP tests used local fake servers or fake transports; no live providers or production access.
- Independent review approved all eleven repairs after one narrow follow-up wave for timeout event classification and EOF release on recovery persistence failure. Next action is the authorized GitHub push; no production rollout was performed.

### Subtitle provider code review — 2026-09-05

- Reviewed commit `39f5e20`; the historical review is recorded with the subsequent repair commit. The translation-exclusion repair was pushed as `39f5e20`.
- Recorded eleven actionable findings and follow-up improvements in `docs/provider-review-2026-09-05.md`: Titlovi identity/pagination, SubDL range/absolute/direct-member normalization, shared cooldown/body-failure handling, and OpenSubtitles authentication/host/download handling.
- No runtime behavior adopted or changed. Existing provider race tests and vet pass; temporary Go overlays and fake transports reproduce gaps without changing tracked tests or contacting live providers. Official OpenSubtitles documentation was checked for login host and rejected-credential contracts.
- Next task: fix the findings only when requested, promote reproductions into permanent tests, then verify affected packages with race detection. No production access, deployment, commit, or push performed for this review.

### OpenSubtitles translation exclusions — 2026-09-05

- Commit: `fix: exclude AI and machine translated OpenSubtitles results` (this commit, based on `5865d09`). The prior scoring repair was pushed to GitHub as `5865d09`.
- Every exact-hash and broad search page explicitly sends `ai_translated=exclude` and `machine_translated=exclude`. The provider applies these documented API filters; no translation opt-in configuration was added.
- Normalized search-cache version advances from `candidate-v3` to `candidate-v4`, preventing pre-policy search results from bypassing the exclusions. Existing installed subtitles and downloaded pack caches remain unchanged; no startup searches, schedule resets, or production mutations were added.
- RED confirmed missing flags on both pages in both search phases and reuse of a pre-policy cache entry. GREEN passed `go test ./internal/provider/... -race -count=1`, `go vet ./internal/provider/...`, and `git diff --check`. All HTTP tests used local fake servers; no live provider calls.
- Next task: publish this verified change as authorized; production rollout remains separately scoped.

### Scoring review repairs — 2026-09-05

- Commit: `fix: preserve scoring evidence and identity safeguards` (this commit, based on `0e19e88`), authorized by the user's request to fix all findings in `docs/scoring-review-2026-09-05.md`.
- Whitespace-delimited slash alternatives now use one shared parser for scoring, episode evidence, and edition bypass checks. Source aliases normalize on both sides; source/group/resolution evidence is order-independent and bonuses remain single-award. Compact slashes, remux distinctions, explicit identity conflicts, and existing edition/episode safeguards remain intact.
- SubDL no longer copies request title/year into candidate identity. Raw fallback responses reach normalized stable-identity merging before evidence is lost; incomplete episode rows remain available for missing-field merging but are filtered if still unresolved. Explicit wrong episode/season rows remain excluded, and existing direct-member/pack tests pass.
- Normalized search-cache version advances to `candidate-v3`, preventing reuse of old inferred identity fields. No migrations, schedule resets, startup network activity, or installation takeover were added.
- Sonarr/Radarr hydration now populates streaming-service evidence from explicit web-release scene metadata after an identity marker. Title-only `Max`, `Amazon`, and `Hulu` controls remain unknown. Existing media is enriched during normal hydration, without an automatic library scan.
- RED/GREEN reproduced source loss (39 instead of 54), target `webdl` mismatch (0 instead of 15), inferred identity (80 instead of 65), discarded duplicate evidence, old-cache reuse, alternative order dependence, and missing catalog service metadata. An actual SubDL-to-workflow regression confirms missing identity cannot bypass LAPSE even at a lowered threshold, while genuine returned identity can. A temporary Go overlay also confirmed this regression fails against the original SubDL source.
- Independent review found one additional duplicate-order edge case (unknown episode filtered before later completion), now covered and fixed; final review reported no material findings.
- Verification passed: affected packages with `-race`, `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, Compose `config --quiet`, and formatting/diff checks. Fake HTTP suites used approved loopback permission after the restricted sandbox refused test listeners. No live Arr/provider calls or production mutations were made during implementation.
- Publication: the user authorized pushing the completed repair to GitHub `main`; its existing CI verifies and publishes the container. No production rollout was authorized or performed. Scoring weights and shortlist policy remain unchanged; assess corrected evidence before tuning them. The earlier production diagnosis is retained below.

### Read-only production health and movie-score diagnosis — 2026-09-05

- Inspected the deployed `sha-0e19e88` service using operator-approved status/log commands and SQLite `mode=ro` with `query_only`; no application startup commands, provider searches, database writes, or media mutations were performed.
- Observed healthy readiness, zero restarts, successful reconciliation, an active unexpired worker lease, and successful installation notification delivery. Verified the diagnosed sidecar against its recorded checksum.
- Confirmed a Croatian movie installation scored 54: external identity 20, title/year 15, source 15, rating 2, popularity 2. Three candidates tied at 54; LAPSE selected the strongest confidence (0.798943, `solid`, embedded reference) and applied a -48688 ms shift. Missing release-group/resolution evidence explains the score; it is not a translation-quality percentage.
- Identified an existing scoring limitation for follow-up: slash-separated alternative release descriptions are parsed as one release, allowing WEB-DL evidence to suppress a listed Blu-ray source match. No scoring behavior changed.
- Verification: bounded production logs/status, read-only persisted candidate/installation/queue metadata, sidecar checksum, and deployed-source inspection. No tests run for this documentation-only diagnosis. Commit: uncommitted ledger entry based on `b9e5854`. Next task: consider a separately scoped release-evidence parsing repair; playback/translation quality remains unverified.

### Conventional repairs corrective round 2 — marked numeric endpoints

- The combining-mark exception now applies only when the matched continuation prefix ends in bare `E` or `e`. Marks after complete numeric endpoints remain explicit episode evidence, preventing ambiguous separated expressions from falling through to a single token or retaining an initial range.
- RED reproduced all 36 nonspacing/enclosing-mark combinations across tabs, nonbreaking spaces, em spaces, separated endpoints, and all three range forms. GREEN checks `episodeRange`, `Select`, and `SelectSingleEpisode`, while composed/decomposed `Édition`/`Épisode` and all earlier ambiguity controls remain green.
- Fresh verification passed focused pack/workflow controls, complete pack/workflow and repository race suites, vet, tagged E2E, credential-unset provider contracts (all three skipped), Compose, formatting/diff checks, and privacy/reference scans. Full/E2E used approved loopback fake servers; no external services, production hosts, image builds, push, or deployment were used.
- Commit: `fix: retain marked numeric episode continuation evidence` (corrective round 2 based on `2cbca93`). Next safe action: independent re-review.

### Conventional repairs corrective review — decomposed Unicode suffixes

- The continuation boundary treats Unicode combining marks as word continuation, preserving decomposed `Édition` and `Épisode` release/title suffixes after tabs, nonbreaking spaces, and em spaces. The change is confined to the continuation guard; supported range syntax and existing ambiguity rejection remain intact.
- RED reproduced rejection of 24 decomposed suffix/expression combinations through the selectors, with range parsing also rejecting valid ranges. Composed controls passed. GREEN covers those canonical forms through `episodeRange`, `Select`, and `SelectSingleEpisode`, together with every prior separated/incomplete whitespace regression.
- Fresh verification passed focused pack/workflow controls, complete pack/workflow race suites, `go test ./... -race -count=1`, vet, tagged E2E, credential-unset provider contracts (all three skipped), Compose, formatting/diff checks, and privacy/reference scans. Full/E2E used approved loopback fake servers; no live services, production hosts, image builds, push, or deployment were used.
- Commit: `fix: preserve decomposed Unicode release suffixes` (corrective review based on `bb2bd02`). Next safe action: independent re-review of this correction.

### Conventional repairs final follow-up — cancellation errors and Unicode whitespace

- Exact acquisition preserves the original installer error before considering cancellation-only handling. Cancellation plus a failed filesystem rollback therefore retains both causes; only direct pre-publication content-validation errors consult cancellation before candidate rejection/fallback.
- Episode continuation detection now trims Unicode whitespace with the existing dot, underscore, and hyphen separators, then applies the same episode-shape and Unicode alphanumeric boundary checks. Tabs, nonbreaking spaces, and em spaces in separated, incomplete, or extra episode expressions fail closed in range parsing and both selectors. Valid ranges, `S01E01.1080p`, and ordinary release suffixes remain accepted.
- The real-installer/SQLite regression observed RED when publication succeeded, cancellation prevented persistence, rollback removal failed, and `Service.Run` exposed only cancellation. GREEN proves cancellation and the rollback path error remain discoverable, no later exact/broad candidate runs, no rejection or installation is persisted, and the obstructed destination remains available for inspection. Direct cancellation and existing content-validation fallback controls also pass.
- Parser and selector regressions observed RED for all three whitespace classes before the shared continuation fix. Controls cover all supported range forms and whitespace-separated resolution/edition/release text without broad false positives.
- Verification passed focused RED/GREEN, `go test ./internal/pack ./internal/workflow -race -count=1`, `go test ./... -race -count=1`, `go vet ./...`, tagged E2E with `-race -count=1`, provider-contract with all six credential variables explicitly unset, Compose validation, and diff checks. Full/E2E required approved loopback access for fake local servers; all three provider contracts skipped. Privacy scanning found only existing placeholders/syntax; the tracked prohibited-reference scan returned no matches.
- Commit: `fix: preserve rollback errors and reject whitespace continuations` (focused follow-up based on `b7d8448`). No live services, production hosts, image builds, pushes, publication, or deployment were used. Next safe action: independent re-review of this follow-up.

### Conventional repairs final review — archive ambiguity and content-validation fallback

- Episode-shaped continuations are detected independently of complete range recognition. Separated forms such as `S01E01 E03` and `S01E01_E03`, incomplete endpoints such as `S01E01-E`, and extra endpoints after valid ranges fail closed in both archive selectors. Complete supported ranges and ordinary release suffixes retain their existing selection rules; punctuation alone is not range evidence.
- The installer returns a private typed error only for deterministic source-content validation before staging. The exact phase records these failures as `invalid_subtitle` with the artifact checksum and continues lazily through later exact candidates or broad fallback. Filesystem, SQLite installation/outbox/rejection persistence, cancellation, and wrapped/joined rollback errors remain terminal. No schema or configuration change was needed.
- The format-changing rejection regression now supplies matching live managed-sidecar inventory and verifies the first candidate's rejection decision, both downloads in order, and the later compatible candidate as the only installer invocation before its technical failure.
- RED/GREEN regressions covered both archive selectors and range parsing, real-installer/SQLite duration rejection with exact and broad fallback, deterministic exhaustion, source-content classification, and the previously misleading format-changing fixture. Additional controls verify terminal staging, installation, outbox, rejection-persistence, and joined rollback failures without quarantining the later valid candidate.
- Verification passed the focused regressions, `go test ./internal/pack ./internal/workflow -race -count=1`, `go test ./... -race -count=1`, `go vet ./...`, tagged E2E with `-race -count=1`, provider-contract with credentials explicitly unset, Compose validation, and `git diff --check`. Full/E2E tests required approved loopback permission for local fake servers. Credential scanning found only existing placeholders/syntax; the prohibited-reference scan had no matches.
- Commit: `fix: close final archive and exact fallback gaps` (single wave based on `032f526`). No live services, production hosts, image builds, pushes, publication, or deployments were used. Next safe action: independent re-review of this wave.

### Conventional repairs Task 9 — repaired contracts and local verification

- User, provider, operator, Silo, and contributor documentation now records the verified contracts: sequential uncapped exact candidates before the capped broad tournament; candidate-local fallback and first-install stop; conservative explicit episode ranges and universal forced-member policy; deleted managed-sidecar recovery; atomic installation/outbox persistence with independent asynchronous delivery; original-stream YAML cardinality; root-aware longest-prefix Silo mapping; and response-body-lifetime provider permits.
- Implementation commits covered by this verification are `234b0f3` and `358ade2` (explicit provider modes and workflow bridge), `26bd5c8` and `29c0b5e` (exact-first workflow plus outcome precedence), `92303d4`, `fae0406`, `3d93d21`, and `23ccc58` (archive/member selection and fail-closed range fixes), `e87570b` (deleted-sidecar recovery), `1f11656` (atomic installation/outbox), `52e3e0b` and `d9af618` (YAML cardinality and explicit-empty-document handling), `c1d71fd` (root Silo mappings), and `8d52824` (response-lifetime provider permits).
- Focused verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... ./internal/workflow ./internal/pack ./internal/store ./internal/worker ./internal/config ./internal/notifier ./internal/app -race -count=1` exited 0 for all eleven packages. Its first restricted-sandbox run failed only because local `httptest` listeners could not bind; the identical local-only command passed with loopback permission.
- Full verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1`, `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...`, `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1`, `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/providercontract -tags=provider_contract -count=1`, `docker compose -f compose.example.yml config --quiet`, and `git diff --check` each exited 0.
- The provider-contract suite was additionally run verbosely with all six provider credential variables explicitly unset; OpenSubtitles, SubDL, and Titlovi each skipped before constructing an adapter or making a request. All other tests used sanitized fixtures, temporary media, injected runners, and loopback fake servers. No live Arr, provider, Silo, LAPSE network service, production host, image build, publication, or deployment was used.
- The credential-pattern scan found only documented placeholder webhook tokens, the intentional disabled Silo sentinel, and historical syntax/reference prose; manual review found no literal secret. The repository-wide prohibited-comparison scan returned no matches. Repository state contained only the seven intended Task 9 documentation files before commit.
- Documentation commit: `docs: record correctness repair contracts`.
- Next safe action: independent whole-branch review; push, image publication, and deployment require a separate explicit request.

### Conventional repairs Task 8 — response-lifetime provider permits

- Provider HTTP responses now own both the configured-instance and shared-origin concurrency permits until a read reaches EOF or the caller closes the body. The private body wrapper releases once, including EOF followed by repeated/concurrent cleanup, while preserving body data and original read/close errors.
- Transport failures and invalid responses release immediately. Responses returned with authentication, cooldown, or state-persistence errors retain body ownership so cleanup cannot exceed configured concurrency. Adapters remain responsible for closing every returned body, including partial/error payloads; inspection confirmed all three adapters close before refresh, pagination, or subsequent download requests.
- RED/GREEN coverage reproduced and fixed early acquisition for both limit types, EOF and early Close, and HTTP 200/401/429/503. Tests also prove stale-body cleanup cannot release a later request's permit, EOF works without prior Close, read/close errors remain intact, and network/cancellation/invalid-body failures leak no permit.
- Verification passed focused transport race tests and `go test ./internal/provider/... ./internal/app -race -count=1`, plus `git diff --check`. Restricted adapter tests could not bind local fake servers; the same local-only suites passed with loopback permission. No live providers, production, image builds, or deployments were used.
- Commit: `fix: retain provider permits through response bodies`.
- Next task: final documentation and complete verification.

### Conventional repairs Task 7 — root-aware Silo path mappings

- Silo mapping endpoints are normalized once at construction with slash semantics, retain `/` as a valid endpoint, and reject relative or traversal-bearing values before any request can be built. Runtime target paths receive the same pre-cleaning traversal check.
- Rewrite selection remains deterministic: it chooses the longest matching source at a path-component boundary, so `/media` cannot capture `/media2`. Both `/media -> /` and `/ -> /mnt/media` preserve absolute namespace paths, including exact-root rewrites; non-matches return the clean original absolute path.
- The notifier now uses slash-native path operations for both remapping and parent-directory scan payloads, independent of the host filesystem separator. An unsafe mapping or target returns a bounded delivery error before an HTTP request.
- RED/GREEN coverage proves remote/local root mappings, exact-root behavior, longest-prefix selection, component boundaries, constructor rejection of relative/traversal endpoints, and no request for a traversal target. Verification: focused and complete `internal/notifier` race-enabled suites plus `git diff --check`. No external Silo, providers, production, image builds, or deployments were touched.
- Commit: `fix: preserve root Silo path mappings`.
- Next task: retain provider concurrency permits through response-body lifetime.

### Conventional repairs Task 6 — YAML document cardinality

- Configuration loading now decodes the original YAML byte stream as a document stream before environment expansion. Any non-empty trailing document is rejected with a secret-redacted cardinality error, while empty trailing separators remain harmless.
- Strict known-field validation still runs on the single expanded document, and the post-round-trip cardinality check was removed because it can no longer observe the original stream safely.
- RED/GREEN coverage proves a second document containing an environment placeholder is rejected before lookup/expansion can leak its value, while a normal configuration followed by an empty separator still loads unchanged.
- Verification: focused document-cardinality tests, complete `internal/config` race-enabled suite, and `git diff --check`. No providers, Silo, production, images, or deployments were touched.
- Commit: `fix: reject trailing configuration documents`.
- Next task: root-aware Silo path mappings.

### Conventional repairs Task 5 — atomic installation and notification outbox

- `RecordInstallationWithNotifications` validates all intents before mutation, then commits the current-media support check, installation audit, provenance upsert, and checksum-deduplicated outbox inserts in one SQLite transaction. It reports inserted/deduplicated results only after commit. `RecordInstallation` remains an installation-only compatibility wrapper, and standalone enqueue shares the same validation/SQL semantics.
- The installer constructs sorted notifier requests from the final checksum and the injected clock. Every manual, daemon, webhook, and retry path receives configured notifier names through the assembled language workflow. Failure to persist any intent invokes the existing file rollback, preserving prior managed provenance and content or removing a failed first install. Existing retained rollback-copy behavior is unchanged.
- Workers now only deliver durable intents; an installed search outcome never creates a notification. Remote Silo failure retries independently without changing the subtitle or installation row. The shared payload and existing notifier/media/language/checksum dedupe formula preserve durable-row compatibility across restart. `notification.queued` is emitted by the workflow installer only after a newly inserted intent commits, with the same bounded key prefix and no payload paths.
- RED/GREEN coverage observed missing repository/installer interfaces, the old worker enqueue, absent app intents in both English/Croatian manual and daemon paths, and tagged installation/intent counts of `1/0` before wiring. Real SQLite trigger tests fail the second outbox insert and prove audit/provenance/first-intent rollback; real installer/SQLite tests additionally prove first-file removal and managed-file restoration. Tagged acceptance covers both entry points, same-checksum reinstall dedupe, Silo 503 retry isolation, and successful delivery after restart without duplicate work.
- Verification passed `go test ./internal/store ./internal/workflow ./internal/worker ./internal/app -race -count=1`, `go test ./test/e2e -tags=e2e -race -count=1`, focused installer dedupe/SQLite checks, and `git diff --check`. The restricted app/E2E runs could not bind local test listeners; the same local-only commands passed with loopback permission. No external providers, Silo, production, image builds, or deployments were touched.
- Commit: `fix: commit subtitle notifications atomically`.
- Next task: reject trailing YAML documents before expansion.

### Conventional repairs Task 4 — deleted managed-sidecar recovery

- Workflow provenance is now active only when the refreshed live inventory contains a non-embedded sidecar at the recorded path with the recorded checksum. A deleted managed sidecar therefore cannot satisfy a request, short-circuit an exact candidate, trigger same-candidate reassessment, constrain upgrade analysis, or select replacement-install semantics.
- The historical installation row is retained until a successful first-install replacement overwrites it. A present sidecar with a different checksum remains protected and satisfies the request under the existing user-owned-sidecar policy.
- RED/GREEN coverage proves a missing exact-hash sidecar is reacquired and a modified present sidecar causes neither provider search nor installation. Existing reassessment/logging fixtures now explicitly model their live managed sidecars.
- Verification: focused workflow race test, complete `internal/workflow` race suite, tagged end-to-end race suite, and `git diff --check`. The restricted tagged run could not bind its local `httptest` listener; the same local-only command passed with loopback permission. No providers, Silo, production, images, or deployments were touched.
- Commit: `fix: reacquire deleted managed subtitles`.
- Next task: atomic installation and notification outbox.

### Conventional repairs Task 3 — conservative archive evidence and universal forced-member policy

- Episode ranges now require complete, hyphenated episode-token endpoints. The selector accepts `S01E01-E03`, same-season `S01E01-S01E03`, and `1x01-1x03`; it rejects separator-derived pseudo-ranges, release suffixes, non-token boundaries, cross-season/reversed ranges, and chained ambiguous forms. Ordinary `S01E01.1080p` evidence remains a single episode rather than becoming a range.
- Movie/plain single-member selection now passes through `SelectSingleMovie`, which applies the existing forced-subtitle policy and returns bounded typed `pack.SelectionError` diagnostics. A forced-only member is therefore a candidate-local `pack_selection` rejection, allowing the exact-candidate phase to continue; multi-member movies remain fail-closed.
- RED/GREEN coverage added for conservative range parsing, forced-only single-movie selection, and exact-candidate continuation after a forced plain payload. Focused and complete archive/workflow package suites run with `-race`; no providers, Silo, production, images, or deployments were touched.
- Commit: `fix: tighten subtitle member evidence`.
- Next task: deleted managed-sidecar recovery.

### Conventional repairs Task 2 — exact-first workflow fallback

- The workflow now performs one explicit exact-hash search before broad search, scores and persists every exact candidate, and tries eligible exact candidates lazily in provider order until one installs. Ineligible, previously rejected, malformed, and wrong-episode exact candidates no longer hide later exact candidates, and broad search starts only after exact candidates are exhausted.
- Exact and broad candidate records are merged by stable provider/result identity before the repository's replace-style broad write, so exact evidence survives fallback without duplicate persistence. The terminal candidate count uses the unique union.
- Broad fallback retains the existing top-three score-tier/LAPSE tournament and same-installed-candidate reassessment. Broad provider errors replace exact-phase errors, while exact candidate failures remain available for deterministic, technical, and throttled outcome classification; a fully unavailable broad phase retains its prior precedence.
- Review fix: a candidate-local format-changing upgrade rejection now advances to the next exact candidate instead of terminating the phase. Empty and all-ineligible broad results consult the same accumulated exact-candidate failure classifier, preserving download cooldown retry times and returning technical exact failures rather than masking them as deterministic rejection.
- Debug workflow logs now record bounded `search.phase_completed` events with `search_mode`, candidate count, and provider-error count. Info logs remain free of release names, download references, and absolute paths.
- Focused RED/GREEN command: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1 -run 'TestService(TriesExactCandidatesSequentiallyAndSkipsBroadAfterSuccess|FallsBackToBroadAfterExactCandidateFailure|Exact)'`.
- Affected verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow ./internal/app -race -count=1` and `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1`.
- Next task: conservative archive ranges and universal forced-only member selection.

### Conventional repairs Task 1 — explicit provider search modes

- `Coordinator.Search` now accepts only `SearchExactHash` or `SearchBroad` and dispatches exactly one requested mode. Unsupported modes return a coordinator error without invoking providers.
- Exact mode runs eligible providers sequentially in configured order, retains every candidate marked `ExactHash`, and records provider errors. Broad mode retains concurrent provider calls and configured provider ordering while forwarding the requested mode unchanged.
- Focused RED/GREEN command: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider -race -count=1 -run 'TestCoordinator(ExactModeReturnsEveryExactCandidateInProviderOrder|BroadModeNeverCallsExactSearch)'`.
- Full provider verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/provider/... -race -count=1`.
- Next task: exact-first workflow fallback after exact-mode exhaustion.

### Codebase correctness-repair design

- Commit `bdda491` records the approved architecture for the eight remaining findings from the 2026-09-05 conventional review. The two already-repaired findings—stable Arr history identity and OpenSubtitles parent-series identity—are explicitly outside its implementation scope.
- The design makes exact and broad provider searches explicit workflow phases, retains every exact candidate, filters and downloads them sequentially, falls back to broad search after exact exhaustion, and preserves distinct deterministic, technical, and throttled outcomes.
- It defines conservative hyphenated episode ranges, universal forced-only member policy, one atomic installation/outbox SQLite transaction with filesystem rollback, first-install semantics for a missing managed sidecar, pre-expansion multi-document YAML rejection, root-aware Silo mappings, and response-body-owned provider concurrency permits.
- No schema or configuration migration is planned. The existing notification outbox remains asynchronously delivered: inability to persist an intent fails and rolls back installation, while a remote Silo delivery failure after commit does not.
- Commit `a54bdfb` adds the nine-task TDD implementation plan: explicit provider modes, exact-first workflow fallback, archive/member safety, deleted-sidecar recovery, atomic installation/outbox persistence, YAML cardinality, root Silo mappings, response-lifetime permits, and final documentation/verification.
- Plan self-review mapped every design requirement to a task, checked the declared function/type names across task boundaries, and found no unfinished placeholders. It also clarified that filename/member selection supplies forced-only evidence while the existing candidate-level gate supplies hearing-impaired policy.
- `git diff --check` passed. No production code, image, provider, Silo, or production state changed.
- Next task: execute the plan with the selected execution workflow, beginning with Task 1's failing coordinator tests.

### Local production-access runbook convention

- Commit `7338264` adds the exact Git ignore rule for `.agents/production.local.md`, a sanitized tracked `.agents/production.example.md` schema, and an `AGENTS.md` instruction to read the local file before production debugging or deployment when present.
- Verification passed `git check-ignore -v .agents/production.local.md`, the local mode check, `git diff --check`, and focused credential-pattern scanning. No production state was changed and the ignored local file was not staged or committed.

### Human-readable media identity in structured logs

- Commit `5f03cf4` adds the sanitized `media_title` field after durable media resolution. Movie identities render as `Movie (Year)` and episode identities as `Show - S01E02 - Episode Title`; control characters collapse to spaces and output is bounded to 2,048 Unicode code points.
- Worker lifecycle records and the workflow context carry the title through provider, candidate, LAPSE, installation, and terminal events. A failed media lookup retains the existing ID-only records because no trusted title is available.
- The title remains ordinary JSON data and is explicitly excluded from the Alloy/Loki label set. Existing path privacy remains unchanged: absolute paths are still forbidden and root-relative paths remain debug-only.
- Focused tests first failed on the absent formatter and absent worker/workflow fields, then passed under the race detector. Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `go test ./test/providercontract -tags=provider_contract -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the credential-pattern scan.
- No image was built or deployed, and production remains without a running subsyncd container.

### Provider correctness and credential repair

- Commit `6f5dd3d` replaces the OpenSubtitles hash dependency with the canonical 64-bit first/last-64-KiB implementation. A sparse 14,836,065,099-byte regression proves hashing no longer fails at the former 9 GB limit and does not read the complete media file.
- Fresh and legacy-cached provider results now collapse duplicate `(provider_id, result_id)` rows before persistence, scoring, rejection filtering, and LAPSE shortlisting. The merge unions release names, fills only missing identity fields, retains the first nonempty identity on conflicts, and keeps the strongest exact-hash, rating, popularity, and download-count evidence. This prevents duplicate API rows from downloading or analyzing the same subtitle more than once.
- Successful HTTP responses discard `Retry-After` before common rate-limit parsing while retaining standard `RateLimit`/`X-RateLimit-*` windows. Error responses, including 429 and 5xx, keep existing `Retry-After`, fallback, and circuit behavior.
- SubDL normalization drops returned download-query credentials, reconstructs the configured key only on outbound requests, and emits credential-free result IDs. Cache, workflow provenance, pack-member copies, and candidate log correlation independently strip download references or query/fragment data.
- Migration `002_scrub_provider_credentials.sql` deletes only credential-bearing provider-derived cache/candidate/pack/installation/rejection rows. Regression fixtures prove that clean provider rows, media, active search leases, reconciliation cursors, and clean pack members survive, while members of deleted contaminated packs cascade. Existing subtitle files remain on disk if contaminated managed provenance is removed.
- TDD regressions failed against the prior size cap, duplicate retention, successful-response cooldown, SubDL query persistence, log fallback, cache pack-member reference, and migration boundary before the corresponding fixes. Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `go test ./test/providercontract -tags=provider_contract -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the credential-pattern scan.
- No subsyncd container is currently running on production. No image was built, published, deployed, or started. Rotate the SubDL key that was exposed by the legacy persisted candidate identity before using the corrected build.

### Comparative reference documentation cleanup

- Commit `863f5d7` removes the obsolete comparative reference document, its contributor-guide requirement, stale publication-plan checks, and every remaining mention from the current repository tree.
- Historical Git objects were deliberately left unchanged; rewriting published history would change commit identities and require a coordinated force-push.

### Newly configured language backfill

- Commit `2c64814` adds an offline startup reconciliation that inserts only absent search rows for every configured language and already-indexed media item belonging to a currently configured Arr instance. Supported media is immediately due at missing priority; unsupported multi-episode media receives its existing terminal outcome.
- The insert is transactional and conflict-ignoring. Existing attempts, technical-failure counters, due times, priorities, outcomes, leases, rerun state, installations, and other-language rows remain unchanged. Media retained for an instance no longer present in configuration is not backfilled.
- Adding a provider to an existing language changes its workflow route after restart without rewriting schedules. Adding a new Arr instance continues to use an empty reconciliation cursor plus retained Arr history; this change does not add a full-library Arr enumeration or startup network request.
- TDD evidence first failed on the absent repository method and then on the absent startup wiring. Focused repository and application tests passed under the race detector. Fresh full verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, Compose rendering, and `git diff --check`.

### Silo parent-directory notification correction

- Live production evidence showed that authenticated `POST /api/v1/scan` uses Silo's native container listener on port 8080. A media-file target returned 202 but completed without walking sibling files; the exact Season 01 directory target performed a subtree scan and processed all 10 media files.
- The notifier now applies the boundary-aware path mapping first and sends the mapped media file's parent directory. Unit and tagged end-to-end assertions require that exact payload. Container examples and production mappings use `http://silo:8080`.
- Commit `4af206c` passed the full race suite, vet, tagged end-to-end tests, Compose rendering, and diff hygiene. Its local arm64 image is `sha256:20bbdf3fd59dc07418609336a0d0c8ddaca76040e54565db4ae6ca09acf0ef31`, version `sha-4af206c`, user `1000:1000`.
- A single synthetic SQLite notification exercised the real durable worker. subsyncd delivered it once in 14 ms; Silo returned 202 in 8 ms and completed a 10-file subtree scan. Silo's database retained exactly one external subtitle matching the S01E06 Croatian sidecar.
- The synthetic notification was deleted after verification. The temporary API key was removed from `.env`, Silo was returned to `enabled: false`, and doctor plus readiness passed. production remains healthy on `sha-4af206c` with zero notification rows.

### Clean schema baseline Tasks 1–4

- Replaced the ten pre-release migrations with one complete `001_baseline.sql` while retaining the ordered embedded migration runner. Startup now rejects applied migration names outside the embedded lineage with an explicit rebuild-from-empty error.
- The baseline enforces positive `media.entity_id` in SQLite and retains separate replaceable `file_id`, current event-local zero identities, fingerprints, queues, rejections, notifications, and all final indexes/foreign keys.
- Removed zero-entity adoption, import/rename entity fallback, the unused default-priority search wrapper, and obsolete old-cache compatibility tests. Current file-replacement conflict checks, candidate serialization, provider cache safety, and entity-addressed deletions remain covered.
- Focused red/green store, catalog, worker, provider, and domain suites passed under the race detector. Full race, vet, tagged end-to-end, image, and live production verification completed before the Stage 6 cutover was accepted.

### Safe outside-scope webhooks

- Import/rename hydration now converts only wrapped `ErrOutsideScope` into `ErrIgnoredEvent`. The HTTP boundary returns 204, persists no event/media/search mutation, and emits no worker wake. Unsafe mapping/filesystem errors continue through the normal failure path; delete webhooks remain file-ID addressed.
- TDD RED evidence reproduced the existing wrapped scope error instead of the ignored sentinel. GREEN verification passed the focused test, affected catalog/http/app race suites, complete race suite, `go vet ./...`, tagged race-enabled end-to-end suite, and `git diff --check`.
- Code commit: `629a050`. Next: publish the documented boundary, deploy its immutable image to the isolated canary, and create the real Radarr webhook connection.

### Arrapi stable-identity reconciliation Task 7 — durable contract and release gate

- Durable README, architecture, operations, release, and contributor guidance now distinguishes stable movie/episode entity identity from replaceable physical file identity. It records arrapi v2.0.5 ownership of history/current-entity requests, the temporary detail-enrichment client, atomic entity lifecycle behavior, second-resolution cursor overlap, replay safety, narrow-canary outside-scope handling, lazy legacy adoption, and 5m/15m/1h/6h failure backoff.
- Startup remains offline and performs no full-library identity backfill. The compatibility limit remains explicit: a historical deletion for a legacy zero-entity row cannot be linked because Arr does not retain the deleted physical file ID; it is audited as unknown and the row remains until later live hydration or operator action.
- The first full race run exposed one pre-entity OpenSubtitles persistence fixture. Root-cause tracing showed production hydration already provided positive identity; the shared hydrated-media fixture was corrected at its source, its focused race suite passed, and the complete matrix was rerun afterward.
- Fresh verification passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `docker compose -f compose.example.yml config --quiet`, the obsolete-symbol/toolchain/version scans, and `git diff --check`. A native arm64 image `sha256:9138534acd0cd9e4196abf7a95693656f50bf92aa7427551c427dbe98d66d4fb` built with Go 1.27.1 and returned `subsyncd arrapi-local` from `--version` as UID/GID `1000:1000`.
- Implementation commits are `a2246e9` (bounded arrapi client), `8ef2561` (stable persistence), `4848059` (scoped detail identity), `485c188` (entity history/current state), `5ff7f54` (atomic entity deletion), and `43f398f` (failure backoff), followed by this documentation/verification boundary. Final scope review found no changes to provider routing, scoring, LAPSE, subtitle handling, Silo, automatic webhook management, publication behavior, or production deployment.
- Nothing was pushed, published, or deployed. The next action requires separate explicit authority.

### Arrapi stable-identity reconciliation Task 6 — failure backoff

- Worker reconciliation timing now records the last attempt and consecutive failure count independently per Arr instance. Failures retry after 5 minutes, 15 minutes, 1 hour, then 6 hours indefinitely; success resets that instance to the normal configured reconciliation interval.
- Attempt state is deliberately in memory. A process restart attempts reconciliation immediately, while the durable catalog cursor remains unchanged across every failure and still prevents a successful page gap.
- `reconcile.failed` now includes bounded `attempt` and `retry_at` fields. Error detail continues through the configured observability redactor; the regression test proves upstream body/path markers do not enter JSON logs.
- Clock-driven TDD evidence first reproduced retry on the 12:04:59 recovery poll. It now proves attempts exactly at 12:00, 12:05, 12:20, 13:20, and 19:20, then a six-hour post-success interval. A healthy second instance reconciles at its own six-hour boundary while the failing instance remains delayed.
- Verification passed with the focused backoff test, complete race-enabled worker and catalog suites, and `git diff --check`. One pre-entity SQLite worker fixture was updated with its stable ID.
- Next task: update README/architecture/operations/agent contracts, run the full unit/vet/e2e/container matrix, review the final diff against the approved design, and keep push/publication/production deployment out of scope.

### Arrapi stable-identity reconciliation Task 5 — atomic entity deletes

- The reconciler now validates history identity, state, hydrated-media consistency, and present event type before committing. Present state becomes a file-addressed import/rename; absent and outside-scope state become positive-entity, zero-file deletes. Unknown states fail before any store call or wake callback.
- Repository mutation validation distinguishes hydrated import/rename, file-addressed webhook delete, and entity-addressed reconciliation delete. A known entity delete resolves its current media/file inside the page transaction, completes searches as deleted, and enriches the audit with entity, file, and media linkage. An unknown entity remains an idempotent audit with entity ID, zero file ID, and no media link.
- Page mutations, audit linkage, search completion, replay deduplication, and cursor advancement remain one SQLite transaction. A later instance mismatch rolls back an earlier entity delete, its audit, and the cursor.
- The tagged Sonarr integration now uses live-shaped entity history with a file-less deletion, a normal import, a canonicalized combined file, and an outside-scope entity. It proves known deletion, unknown outside-scope audit, replay safety, support-state scheduling, and cursor advancement through the real arrapi-backed catalog and SQLite repository.
- Verification passed with focused RED/GREEN entity tests, race-enabled catalog/store suites, the tagged `TestSonarrReconciliation` integration, and `git diff --check`.
- Next task: replace success-only reconciliation timing with independent 5m/15m/1h/6h failure backoff per Arr instance.

### Arrapi stable-identity reconciliation Task 4 — entity history and current state

- The catalog contract now accepts both the durable `since` cursor and captured `through` page end. History changes carry stable entity/kind, typed current state, event type, and hydrated media only when present; no physical file ID is used as the history reduction key.
- Radarr and Sonarr use arrapi v2.0.5 for `/api/v3/history/since` and current movie/episode reads. Relevant history is reduced by `movieId`/`episodeId`, after-page records are filtered before collapse, and current state decides present, absent, or outside-scope behavior. Rename survives only as the latest present change; other present states normalize to import, while absent/outside normalize to delete.
- Sonarr verifies that each triggering episode belongs to the current file, caches detail hydration per file, and collapses attached multi-episode history to the deterministic canonical episode. The existing hardened client remains only for richer file/series/movie detail metadata.
- Sanitized fixtures mirror live API shapes: imports may contain `data.fileId`; deletes retain entity IDs and omit file IDs; unknown events are ignored. Tests cover RFC3339-second requests, page-end filtering, replacement resolution, no-file state, outside scope, malformed identity, attached-episode collapse, and membership failure.
- Verification passed with the complete race-enabled catalog and application suites, the focused history suites, `git diff --check`, and a negative scan proving the history fixtures contain no top-level `movieFileId` or `episodeFileId`.
- Next task: allow zero-file, positive-entity delete mutations, resolve known rows inside the reconciliation transaction, retain unknown audits, and advance the cursor only with the whole page.

### Arrapi stable-identity reconciliation Task 3 — detail identity and scope

- Radarr detail hydration now assigns the stable movie ID from the movie file and rejects a missing movie identity. Sonarr assigns the deterministic canonical episode ID, rejects attached episodes without IDs, and privately returns the complete sorted attached-episode set for later history membership checks.
- `ErrOutsideScope` identifies only deliberate scope exclusions: no boundary-aware mapping or a safely resolved mapped path outside configured media roots. Traversal, relative mappings, inaccessible/non-directory parents, dangling-symlink resolution, and media-root resolution remain hard failures.
- TDD evidence: focused tests first failed on missing entity assignments, Sonarr hydration evidence, and scope sentinel. Race-enabled `internal/catalog`, `internal/store`, and `internal/app` verification plus `git diff --check` passed using local fake Arr servers. One old history fixture was corrected to include the episode ID present in real hydrated responses.
- Next task: use arrapi v2.0.5 history and current-entity reads, reduce by stable entity, filter at the captured page end, and collapse combined-episode present state canonically.

### Arrapi stable-identity reconciliation Task 2 — stable entity persistence

- Migration `010_media_entity_ids.sql` adds zero-defaulted `entity_id` columns to media and audit events plus a positive-only unique `(instance, kind, entity_id)` index. Legacy rows survive migration with zero identity and adopt a positive entity ID lazily on successful hydration.
- `domain.Media` and media event mutations now carry stable entity identity separately from replaceable Arr file identity. Media reads round-trip it, events persist it, and `FindMediaByEntity` provides an explicit found/not-found lookup.
- Direct and transactional upserts resolve positive entity identity first, fall back only to a matching legacy zero-ID file row, and fail closed when entity and file identities point to different rows. A file replacement updates the original row and file lookup, invalidates candidate/installation provenance, and retains an active lease while scheduling one rerun.
- TDD evidence: focused tests first failed on the absent columns, fields, lookup, and file-only upsert. Race-enabled verification passed for `internal/store`, `internal/catalog`, and `internal/app`; the restricted run's local-server bind failures were environmental and the permitted local-only rerun passed. `git diff --check` passed.
- Next task: return entity identity from Radarr/Sonarr detail hydration and distinguish safe out-of-scope paths from hard mapping/filesystem failures.

### Arrapi stable-identity reconciliation Task 1 — bounded client boundary

- Pinned `github.com/cplieger/arrapi/v2` v2.0.5 and aligned the module, Docker builder, and GitHub verification job on Go 1.27.1.
- Added private Sonarr/Radarr interfaces and offline constructors in `internal/catalog`; arrapi concrete types do not escape into domain, store, workflow, provider, or application interfaces.
- Added a privacy boundary for arrapi retries and failures. Retry logging discards upstream messages, attributes, and groups and emits only bounded catalog events plus instance identity. Safe errors expose only instance, operation, failure class, status, and retryability.
- TDD evidence: the focused test first failed because arrapi and the private constructors were absent. Verification passed with the focused catalog race test, exact module-version inspection (`github.com/cplieger/arrapi/v2 v2.0.5`), and `git diff --check`.
- Next task: migration 010 and entity-first persistence, including upgrade-in-place, legacy adoption, conflict rejection, and lease/rerun preservation.

### Follow-up — arrapi stable-identity reconciliation design

- Live production evidence and the exact Radarr 6.3.0.10514 and Sonarr 4.0.19.2979 contracts show that history exposes stable `movieId`/`episodeId`, not the top-level `movieFileId`/`episodeFileId` assumed by subsyncd. Import records may carry `data.fileId`; deletion records do not. Nested current file IDs cannot serve as historical deletion identity.
- The approved design is `docs/superpowers/specs/2026-09-05-arrapi-stable-identity-reconciliation-design.md`. It pins `github.com/cplieger/arrapi/v2` behind the catalog adapter, persists stable entity identity separately from replaceable file identity, reduces history by entity, resolves authoritative current state, and commits entity adoption/deletion/audit/cursor changes atomically.
- Because arrapi's curated DTOs omit scoring/provenance fields subsyncd currently needs, the hardened custom detail requests remain temporarily as a narrow enrichment boundary; the custom history DTO and transport are removed.
- Deliberately unmapped history becomes a typed outside-scope result so the two-movie production canary can consume production Radarr history without indexing or reading the rest of the library. Unsafe paths and filesystem resolution failures still fail closed.
- Existing rows adopt `entity_id` lazily on their next successful hydration. No startup or full-library network backfill is introduced. Delete webhooks retain file-ID behavior; the one-time legacy limitation for a missed deletion before adoption is explicit.
- The design also prevents failed reconciliation from retrying every recovery poll while retaining the durable cursor for a later scheduled attempt. Publication, GitHub push, and production deployment remain separate actions requiring approval.
- Design-boundary verification: placeholder/ambiguity review and `git diff --check`. Next task: user review, then write the test-driven implementation plan.

### Follow-up — arrapi stable-identity reconciliation implementation plan

- The implementation plan is `docs/superpowers/plans/2026-09-05-arrapi-stable-identity-reconciliation.md`.
- Seven reviewable, test-driven boundaries cover the pinned arrapi/toolchain and safe error boundary, stable entity persistence and upgrade-in-place behavior, scope-aware detail hydration, real API-shaped entity history, atomic entity deletions/audits, reconciliation failure backoff, and final documentation/full verification.
- The plan keeps arrapi private to `internal/catalog`, retains only the narrow detail enrichment requests required for scoring/provenance fields absent from arrapi v2.0.5, and preserves offline startup plus file-oriented webhook/workflow behavior.
- Plan self-review covers every approved design section with exact interfaces, fixtures, failure expectations, commands, and commits. Push, publication, and production mutation remain excluded.
- Planning-boundary verification: spec coverage review, placeholder/type scan, and `git diff --check`. Next task: execute with the user's chosen workflow.

### Follow-up — wrong-episode archive hardening

- Episode archives now always pass episode-member validation. A non-pack archive containing one generically named subtitle retains compatibility, but a filename with explicit season/episode, range, or absolute evidence must match the requested episode before LAPSE or installation.
- The concrete Titlovi catalog issue #104 case (`306201`, requested Ozark S03E01, archive containing only S03E03 members) remains a candidate-local `pack_selection` rejection. It does not alter provider cooldown state and the workflow continues through the remaining shortlist.
- Debug-only `candidate.rejected` events now add bounded `reason_code`, `selection_rule`, `archive_type`, `subtitle_member_count`, and `matching_member_count` fields. Filenames, subtitle content, and absolute paths remain excluded.
- TDD red evidence: the singleton regression installed `306201`, and the issue reproduction lacked every required diagnostic field. Fresh verification: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/pack ./internal/workflow -race -count=1`, `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check` pass. Next task: none inside this hardening change.

### Conventional repairs Task 7 — integration coverage and durable documentation

- A tagged integration test now drives typed Sonarr history through the real catalog adapter and SQLite repository. One atomic page imports a searchable episode, tombstones an existing episode, persists an out-of-order combined file as `unsupported_multi_episode`, advances the cursor, and exposes only the supported import as due work.
- README, operations, architecture, and contributor guidance now preserve the reconciliation transaction, failed-page cursor, active-lease rerun, fail-closed combined-episode, two-window shutdown, and incomplete-rollback recovery contracts for future agents and operators.
- Verification: fresh parallel runs of `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1`, `go vet ./...`, and `go test ./test/e2e -tags=e2e -race -count=1` all exited 0; `git diff --check` also passes. Next task: none inside this repair plan.

### Conventional repairs Task 6 — visible installation rollback failures

- Installer rollback now joins the initiating failure with removal, restoration, directory-sync, or unused-backup cleanup failures instead of discarding them.
- A first-install sidecar that cannot be removed is reported explicitly and remains protected as untracked inventory. A failed managed replacement restoration retains its last known-good rollback copy rather than deleting it.
- Joined rollback failures remain ordinary technical workflow errors; they do not produce `rejected` outcomes or candidate quarantine entries.
- Verification: the focused first-install test reproduced the swallowed removal failure; all three restoration/classification tests and `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/workflow -race -count=1` plus `git diff --check` pass. Next task: integration and durable documentation.

### Conventional repairs Task 5 — bounded forced shutdown

- Daemon and deterministic drain paths now wait one graceful `ShutdownTimeout`, cancel once, then wait at most one additional `ShutdownTimeout` before returning.
- The first deadline emits `worker.drain_timed_out`; the second emits `worker.drain_abandoned` with bounded active-search and maintenance counts. Neither path clears durable leases.
- Completion channels remain buffered, and a race-safe regression harness proves a cancellation-insensitive workflow cannot hold daemon shutdown indefinitely or block on its late result.
- Verification: both focused tests reproduced the old unbounded wait; `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker -race -count=1` and `git diff --check` pass. Next task: installation rollback failure visibility.

### Conventional repairs Task 4 — fail-closed multi-episode indexing

- Sonarr sorts episode records by season, episode, absolute number, and ID. Zero episodes remains a catalog consistency failure; two or more use the earliest display identity and persist `unsupported_multi_episode`.
- Unleased unsupported searches are terminal at mutation time. A leased terminal rerun is intercepted immediately after current media load, completed without calling the workflow, and receives no provider, LAPSE, candidate, rejection, installation, or upgrade work.
- `explain` emits `unsupported_reason=unsupported_multi_episode`. The Task 1 installation transaction check remains the final guard against stale work already inside an external operation.
- Verification: the three focused tests failed on unsorted selection, workflow entry, and missing explanation; unrestricted `go test ./internal/catalog ./internal/store ./internal/worker ./internal/app ./internal/cli -race -count=1` and `git diff --check` pass. Next task: bounded canceled shutdown.

### Conventional repairs Task 3 — atomic typed reconciliation

- The reconciler converts typed catalog history into stable `reconcile:<instance>:<history-id>` mutations at missing priority and submits the full page once.
- Webhooks and reconciliation share one idempotent transaction helper for import, rename, delete, candidate/provenance invalidation, support-state scheduling, audit linking, and bounded event retention.
- A reconciled same-key change preserves lease owner/expiry and the greater existing priority, then coalesces one immediate rerun. Known and unknown deletes are audited; a later malformed/mismatched mutation rolls back all earlier page writes and the cursor.
- Verification: focused tests failed on the old media-only commit contract and missing mutation priority; unrestricted `go test ./internal/catalog ./internal/store -race -count=1` and `git diff --check` pass. Next task: multi-episode fail-closed dispatch and explanation.

### Conventional repairs Task 2 — typed Arr history decoding

- `Catalog.ListChangesSince` returns stable history identity, normalized import/rename/delete type, media reference, event time, and hydrated media only for live final states.
- Sonarr and Radarr sort history deterministically, ignore unrelated events, retain each file's newest relevant state before hydration, reject malformed relevant identity, and never hydrate explicit deletion tombstones.
- Sanitized contract tests cover rename/ignored-event reduction, import-then-delete, delete-then-import, missing history/file/date identity, stable ordering evidence, and request counts.
- Verification: the focused red build failed on the missing typed method; unrestricted loopback runs of `go test ./internal/catalog ./internal/app -race -count=1`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check` pass. The plan's untagged `test/e2e` command was corrected during execution because all files in that package require the `e2e` build tag. Next task: atomic typed reconciliation.

### Conventional repairs Task 1 — persisted unsupported media state

- Migration `009_media_unsupported_reason.sql` adds a backward-compatible empty-default support marker. `domain.Media` and every repository media read/write path round-trip the validated `unsupported_multi_episode` reason.
- Unsupported imports complete unleased language searches immediately. An event arriving during an active lease preserves ownership and coalesces one terminal rerun instead of starting concurrent work.
- `RecordInstallation` checks current persisted support status inside its transaction, preventing stale in-flight work from committing managed provenance after Sonarr marks a file unsupported.
- Verification: focused red tests failed on the missing domain/schema contract; `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -race -count=1` and `git diff --check` pass. Next task: typed Arr history decoding.

### Follow-up — conventional review repair design

- The approved design is `docs/superpowers/specs/2026-09-04-conventional-review-repairs-design.md`.
- Reconciliation will consume typed Arr history mutations, coalesce each file to its latest relevant state before hydration, and apply imports, renames, deletes, audit rows, and the cursor transactionally.
- Reconciled scheduling will preserve active lease ownership and coalesce one rerun. Sonarr multi-episode files will be indexed as `unsupported_multi_episode` and make no provider or LAPSE calls.
- Shutdown will use a graceful deadline followed by cancellation and one final bounded deadline. Installation rollback will return joined restoration failures instead of silently leaving an untracked sidecar.
- Full snapshots, combined-episode subtitle support, scoring/provider changes, and automatic takeover of rollback orphans remain out of scope.
- Verification for this design boundary: `git diff --check`. Next task: user review, then a test-driven implementation plan.

### Follow-up — conventional review repair implementation plan

- The implementation plan is `docs/superpowers/plans/2026-09-04-conventional-review-repairs.md`.
- Seven test-driven commit boundaries cover persisted unsupported status, typed Arr history decoding, atomic reconciliation and lease preservation, worker fail-closed dispatch, bounded shutdown, installation rollback errors, and final integration/documentation.
- The plan retains existing external APIs, provider/scoring/LAPSE behavior, SQLite durability, and structured redaction rules. Push, image publication, and production deployment remain explicitly separate.
- Plan self-review covers every approved design requirement with named interfaces, literal fixtures/outcomes, red-failure expectations, race verification, and resumable ledger updates.
- Verification for this planning boundary: placeholder scan and `git diff --check`. Next task: execute the plan using the user's selected workflow.

### Follow-up — structured Loki logging design

- Commit `5205f87` records the approved architecture for comprehensive newline-delimited JSON logs collected by Grafana Alloy and queried in Loki. The application owns stable event names, bounded common fields, severity policy, component ownership, central redaction, and correlation; it does not push to Loki directly.
- Normal successful health/readiness probes remain silent. Candidate-by-candidate scoring, release/file evidence, cache decisions, and proven root-relative paths are debug-only. Absolute paths, secrets, query strings, provider bodies/URLs, subtitle content, and raw LAPSE output are forbidden at every level.
- `logging.level` with a `SUBSYNCD_LOG_LEVEL` override defaults to `info` and requires restart. Only bounded `service`, `environment`, `level`, `component`, and `event` values are recommended Loki labels; dynamic identifiers remain JSON fields.
- Metrics, OpenTelemetry, direct Loki transport, runtime level reload, and behavior changes are explicitly out of scope. The implementation plan is `docs/superpowers/plans/2026-09-04-structured-loki-logging.md`.
- Verification for the design boundary: `git diff --check` passed. Next task: approve and execute the test-driven implementation plan.

### Follow-up — priority-aware continuous search dispatch

- Commits: `241b067 feat: prioritize persisted subtitle searches`, `6eca35b feat: classify search lifecycle priorities`, `f01ec7b feat: configure workflow concurrency`, `f7e85f7 feat: wake workers after catalog changes`, and `24f6a7c feat: continuously refill search workers`.
- Migration `008_search_priorities.sql` preserves existing rows as missing-priority work and adds durable priority plus same-key rerun state. Leasing is strict import (300), missing/rejected/reconciliation (200), then successful nonexact upgrade (100), followed by due time and stable identity. Technical failures and throttles retain priority.
- Import/rename arriving during an active lease preserves its owner and coalesces any number of arrivals into one immediately due import-priority rerun. The stale attempt's completion cannot overwrite that event schedule. A second completion without another event ends normally.
- `worker.max_concurrent` defaults to one, accepts 1–8, and controls media workflows. Daemon mode leases only free slots and refills on startup, a capacity-one nonblocking webhook/reconciliation wake, each completion, and the jittered recovery poll. `RunOnce` retains deterministic bounded-batch behavior. Reconciliation and notifications remain independently maintained.
- `explain` now exposes human-readable priority and whether a same-key rerun is pending. Unit and tagged end-to-end coverage proves migration compatibility, strict ordering, priority lifecycle, wake coalescing after partial webhook success, immediate free-slot fill, completion refill, lost-wake recovery, concurrency bounds, bounded shutdown, and persisted webhook-to-daemon dispatch.
- Verification through this boundary used race-enabled storage, schedule, worker, catalog, config, and application suites plus the focused tagged end-to-end webhook wake test. The final repository-wide gate is deferred until the LAPSE tournament is implemented in the same approved execution sequence.

### Follow-up — LAPSE score-tier tournament

- Commits: `cc7a24d feat: stop lapse after a winning score tier`, `ae78f05 feat: rank lapse score ties by confidence`, `87b4e66 feat: fall back across lapse score tiers`, and `ea3c07d test: protect lapse tournament invariants`.
- The top-three eligible cap is now lazy. Candidates are partitioned by descending release score, and the next tier is not downloaded until every viable candidate in the current tier fails. A unique successful leader therefore runs one download, one LAPSE analysis, and one synchronization before structurally skipping lower scores.
- Equal-score candidates are all analyzed before finalization. Solid analyses rank by confidence, configured provider priority, rating, provider ID, and result ID. Only the best is synchronized; a synchronization failure falls through already analyzed ties without repeating analysis, then opens the next score tier if necessary. Score and exact bypasses retain zero analysis confidence and invoke neither LAPSE method.
- Deterministic LAPSE/content/pack failures retain the existing exact candidate/media/tool quarantine. Process, protocol, filesystem, provider, network, and cancellation failures never create rejection records. Cancellation is checked before downloads, analysis, finalization, and tier transitions. Temporary workspaces are removed on every exit path.
- Race-enabled workflow/syncer tests cover unique-leader early stopping, confidence ties and deterministic ordering, within-tier and lower-tier fallback, transient error isolation, cancellation, lazy season-pack extraction, bypass/upgrade invariants, and workspace cleanup. Tagged end-to-end coverage asserts the broad single-candidate path performs exactly one analysis plus one synchronization.

### Task 1 — domain and configuration

- Commit: `679035f feat: bootstrap subsyncd domain and configuration`
- Implemented canonical BCP 47 languages with explicit legacy aliases, strict single-document YAML, whole-scalar environment expansion, provider/language routing validation, path/root checks, and the approved example configuration.
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
- Path mapping uses the longest boundary-aware remote prefix, accepts both slash styles, preserves case, rejects traversal, and resolves existing parent symlinks before accepting media-root containment.
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
- Titlovi handling covers the token contract, provider language names, pagination, episode-zero pack signals, typed 429 responses, explicit pack scope, wrong-season rejection, bounded pages/bytes, host-safe redirects, and extraction outside the provider adapter.
- Verification: `go test ./... -race`, `go vet ./internal/provider/titlovi`, and scoped `git diff --check` passed.

### Task 8 — SubDL adapter

- Commit: `cd2afdf feat: add SubDL provider`

- Strict configuration requires an API key, defaults to the official HTTPS search and download origins, and uses an explicit download-host allowlist. The provider is broad-only and advertises season-pack plus direct-member capabilities.
- Search sends the strongest available IMDb/TMDB ID, original filename or title fallback, media type/year/language, release/HI/unpack/full-season flags, 30-result page size, and `client=custom_integration`.
- Episode searches run standard season/episode, optional absolute episode, and season-only variants; a title-only query runs only when all filtered variants are empty. Stable URL/name identities deduplicate the merged response.
- Provider media-result identity is retained for hard gating and scoring. All release names survive normalization. Explicit and release-text episode ranges must contain the standard or absolute target; direct unpack members are preferred, explicit full seasons become `PackSeason`, and unproven episode-zero rows fail closed.
- Search and download understand daily quota, rate-limit, and service-busy payloads without sleeping. Cooldowns persist by provider instance and operation. Downloads attach the API key only to an allowlisted origin, reject unsafe redirects, and stream through the compressed-byte ceiling.
- Current official contract was checked at <https://subdl.com/api-doc>. We retain the official `full_season`, `unpack_files`, `client`, and optional authenticated-download behavior while rejecting arbitrary archive-member fallback.
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
- Scoring uses small known release-group equivalence sets with independently authored tests, retains explicit zero-point explanations, and rejects wrong-season pack metadata even when candidate top-level season is absent.
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
- Pack handling includes format filtering, numbered/ranged evidence, universal bounded extraction, no arbitrary single-file fallback, strict ambiguity rejection, content addressing, raw-link scrubbing, checksum validation, symlink-safe reuse, and rollback across filesystem/database publication.
- Verification: focused red/green regressions plus `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Task 11 — LAPSE synchronization

- Commit: `d37f7e1 feat: integrate LAPSE subtitle synchronization`

- The compatibility baseline is official LAPSE v2.0.5 at tag commit `8d57e43`; current upstream HEAD inspected during implementation was `a0e99bd919e80f9f836cb4c6f6cc391c1a666dd6`. The CLI/JSON contract was verified from <https://github.com/Schwponaco-org/lapse>.
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
- The optional Silo adapter follows the documented current pre-1.0 native contract: `POST /api/v1/scan`, `Authorization: Bearer …`, one mapped parent-directory `path`, and 2xx success (Silo documents 202). Selecting the directory after boundary-aware longest-prefix rewriting makes Silo perform a subtree scan and discover newly installed sibling subtitles. It rejects redirects and credential-bearing/invalid base URLs, uses a 15-second default timeout, treats timeout/408/429/5xx as retryable, and never includes the API key or response body in errors. Container-network examples target the live production native listener on port 8080. See `docs/references/silo.md`.
- Official Silo documentation is authoritative. The worker design independently tightens the nonblocking cooldown and durable-lease requirements from the approved service design.
- Verification: race-enabled tests cover renewal, completion ordering, two-worker exclusion, two-workflow concurrency, crash recovery, missing/failure/throttle accounting, poll/reset jitter, six-hour reconciliation, bounded shutdown, notification dedupe/retry isolation, disabled Silo, request contract, path mapping, authentication/status classification, timeout, redirect rejection, and secret redaction. Final `go test ./... -race`, `go vet ./...`, and `git diff --check` passed.

### Follow-up — native Silo scan API and focused README

- Commit: `4a394b1 feat: use Silo native scan API`
- Replaced the Jellyfin-compatibility notification with Silo's current native `POST /api/v1/scan` contract on the main listener: bearer admin API key, JSON parent-directory `path`, and accepted 2xx response. Durable retry isolation, path mapping, redirect rejection, and credential redaction are unchanged.
- Reworked `README.md` around a general feature list. Arr webhook connections remain deliberately manual; `subsyncd` neither creates nor modifies Sonarr/Radarr settings, while startup and six-hour reconciliation cover missed deliveries.
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

### Follow-up — absolute missing-subtitle milestones

- Commit: `acfc801 fix: use absolute missing-subtitle milestones`
- Missing-language searches now reach the intended elapsed milestones from import or schedule reset: immediately, 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, then every 14 days. The scheduler stores the adjacent intervals `30m`, `90m`, `6h`, `16h`, `48h`, `96h`, `168h`, then `336h`, preventing the former cumulative drift to 2h30m, 10h30m, 1d10h30m, and later dates.
- The existing ±10% interval jitter, technical-failure backoff, provider cooldowns, upgrade schedule, and six-hour normalized search cache are unchanged. Because cached empty results suppress provider access, the 30-minute and 2-hour jobs normally refresh only local sidecars; continuously missing subtitles normally generate provider searches around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days.
- A cumulative-elapsed-time regression test failed against the former delay table and passed after the interval correction. The scheduler test also proves that attempt two is scheduled 90 minutes after the preceding attempt.
- Verification on 2026-09-04: `go test ./internal/schedule -race -count=1 -v`, complete `go test ./... -race -count=1`, `go vet ./...`, tagged e2e tests with `-race -count=1`, and `git diff --check` passed. The complete and e2e suites required loopback permission solely for their local `httptest` servers.
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
- Verification: `go test ./... -race`, `go vet ./...`, tagged e2e tests, provider-contract compilation/no-credential skips, all Compose renderings, native legacy-Docker build, version smoke, and real in-container `doctor` passed. The final verified local arm64 image ID is `sha256:2051a4f528c586dda1046436f3a0072612632fc8158ecc62dbbaa35ac5418384`.

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

### Follow-up — standalone GitHub publishing

- Commits: `6fc34d5 chore: prepare standalone subsyncd tree`, `f2deb60 docs: deploy subsyncd from private GHCR`, and `b054f50 ci: publish verified multiarch images`.
- The self-contained source root was published with subsystem history to private `https://github.com/tomislav/subsyncd`; its canonical local clone is `/Users/tomislav/Development/subsyncd` on default branch `main`. The associated container package is private `ghcr.io/tomislav/subsyncd`.
- The standalone root now excludes environment files, runtime configuration, SQLite state, caches, media, logs, coverage, and the compiled binary. `AGENTS.md` resolves only in-tree documentation and retains the correct default-off hearing-impaired policy.
- `compose.example.yml` pulls `ghcr.io/tomislav/subsyncd:${SUBSYNCD_IMAGE_TAG:-latest}` without changing its rootless identity, read-only filesystem, tmpfs ownership, mounts, health check, resource limits, or network. The README documents private GHCR authentication, immutable tag selection, Compose pulls, and an explicit local build path. The parent `arr-stack/docker-compose.yml` is unchanged.
- `.github/workflows/container.yml` runs race-enabled unit/package tests, `go vet`, and tagged end-to-end tests before publishing. It builds exactly `linux/amd64` and `linux/arm64`, uses the Actions cache, and emits maximum provenance plus an SBOM.
- Action pins are `actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1`, `actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`, `docker/setup-qemu-action@1f40c72289eff860ee54a304f1438e3cff362e0a`, `docker/setup-buildx-action@37fe631027851001ddb9b187196cc803df7f5f0e`, `docker/login-action@dbcb813823bdd20940b903addbd779551569679f`, `docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302`, and `docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a`.
- Successful `main` pushes publish `latest` and `sha-<short-commit>`. Stable `vMAJOR.MINOR.PATCH` tags additionally publish `MAJOR.MINOR.PATCH`, `MAJOR.MINOR`, and `MAJOR`; prereleases publish only their full prerelease version and SHA. Manual runs always publish SHA and publish `latest` only when dispatched from `main`.
- Local verification on 2026-09-04 passed: credential-pattern scans of the subtree and relevant history; `go test ./... -race -count=1`; `go vet ./...`; `go test ./test/e2e -tags=e2e -race -count=1`; actionlint v1.7.12; `docker compose -f compose.example.yml config --quiet`; default, SHA-tag, and arbitrary PUID/PGID/TZ Compose renderings; a native `docker build --build-arg VERSION=prepublish -t subsyncd:prepublish .`; and `docker run --rm subsyncd:prepublish --version`, which returned `subsyncd prepublish`.
- GitHub CLI authorization was renewed through the interactive device flow without printing or committing a token. The first publication workflow, `https://github.com/tomislav/subsyncd/actions/runs/33857752027`, succeeded for head `607269e502c3c90d75403bcc82bbb497fc93b9d5`; both the `Verify` and `Publish multi-architecture image` jobs passed.
- `latest` and immutable `sha-607269e` resolve to manifest-list digest `sha256:d88884fab6e65fc4e657e3fbf0e2c5cc0c52b78d4d4f2f10c9d6438c60f4092f`. The index contains native `linux/amd64` (`sha256:e1e428b0fc1213bb993fad7281fbfa93d75ff7eedf7898702bed85dac737ff15`) and `linux/arm64` (`sha256:bd898356f1a86df98999c0b5230f8b897bf861a1fe8afe827bde5986e992712b`) images plus per-platform SBOM/provenance attestations. Runtime smoke output was `subsyncd sha-607269e`.
- GitHub reports package visibility `private` and repository association `tomislav/subsyncd`. The parent `/Users/tomislav/Development/arr-stack/docker-compose.yml` remained unchanged.

### Follow-up — live Titlovi expiration compatibility

- A credentialed production canary on 2026-09-04 found that Titlovi currently returns token expiry without an offset, for example `2026-09-11T13:16:35.55`. The adapter's RFC 3339-only parser rejected the otherwise valid login response before search.
- `subsyncd` keeps bounded Titlovi token-expiry caching: it accepts RFC 3339 first, then the observed fractional local timestamp in `Europe/Zagreb`; the existing one-time 401 refresh remains the fallback for server-side expiry or clock drift.
- The regression fixture uses the observed response shape. Its focused test failed with `Titlovi token expiration is invalid` before the fix and passed afterward; the complete Titlovi package passed with `-race`.

### Follow-up — authoritative inventory fingerprint retention

- The first repeat exposed a catalog/inventory timestamp mismatch. Sonarr and Radarr supply `dateAdded` as catalog metadata, while inventory records the actual filesystem modification time. Rehydrating an unchanged file compared those unlike values, falsely classified the media as changed, and cleared the installation's score, LAPSE result, and media fingerprint before upgrade evaluation.
- Both catalog paths now retain the stored authoritative inventory modification time whenever Arr file ID and byte size are unchanged. A real file replacement still invalidates provenance when file ID or size changes, and a fresh filesystem inventory explicitly invalidates provenance when it detects an actual modification-time change even with the same ID and size.
- Regression coverage exercises direct catalog refresh, transactional Arr event ingestion, and inventory-side invalidation. The retention tests first replace the catalog timestamp with an inventory timestamp, record a scored installation, repeat unchanged catalog metadata, and verify that the full inventory fingerprint, score, and LAPSE provenance survive. The invalidation test changes only the authoritative filesystem modification time and verifies that stale score, LAPSE, and media-fingerprint provenance are cleared atomically with inventory replacement.

### Follow-up — OpenSubtitles episode identity and evidence scoring

- OpenSubtitles episode `feature_details.imdb_id` and `tmdb_id` identify the episode, while Sonarr's stored IDs identify the series. The adapter now uses `parent_imdb_id` and `parent_tmdb_id` as the episode candidate's comparable series identity; missing parent IDs remain neutral and episode feature IDs are never misrepresented as series IDs. Movie feature IDs remain comparable and are retained. Language, title/year, season, episode, pack scope, and other conflict gates remain active.
- Exact episode coordinates, parsed release ranges containing the target episode, and explicit containing packs now contribute 20 points. A title/year plus episode-evidence candidate therefore reaches the default 35-point identity baseline, while release group, source, service, resolution, rating, and popularity continue to rank compatible encodes. Non-hash totals remain capped at 100 and low-confidence candidates still require LAPSE.
- OpenSubtitles `foreign_parts_only` is now retained as candidate `forced` evidence and is a hard rejection for a normal full-language request. This prevents a filename/API result such as candidate `8602118` from installing only foreign-dialogue captions even if LAPSE can align it. A forced-aware cache refresh on production confirmed candidate `8602118` is rejected solely as forced-only and `8733253` is rejected as both forced-only and hearing-impaired.
- Normalized provider-cache keys now carry schema version `candidate-v2`, so six-hour rows written before parent identity and forced-only evidence existed miss automatically rather than silently acquiring zero-value safety fields. Ordinary candidates omit `forced: false` from JSON, preserving existing deterministic rejection signatures, while forced-only candidates persist the flag.
- A live `candidate-v2` refresh then retained seven current results, hard-rejected forced-only candidates `8602118` and `8733253`, and rescored `8602117` to 57 by adding the parent-series IMDb match. That metadata-only increase exposed an unnecessary same-candidate upgrade: the unchanged provider/result ID was downloaded and passed through LAPSE before the installer rejected its identical checksum. An unchanged media fingerprint plus identical provider/result ID now refreshes score and exact-hash provenance without download, LAPSE, rewrite, install audit, or notification; the refreshed score becomes the upgrade baseline and non-exact candidates retain their future upgrade schedule. A changed media fingerprint still permits revalidation.

### Follow-up — Persisted provider outage circuits and SubDL authentication

- Commit `e620961` adds a shared, persisted transient circuit per configured provider instance and operation. Network failures and HTTP 5xx responses now return the existing typed cooldown outcome and back off for 1, 5, 15, then 60 minutes, capped at 60 minutes. A usable provider `Retry-After` value remains authoritative. Because the state is in `provider_states`, it applies to every media job and survives process restart without a schema migration.
- Any non-5xx HTTP response resets only the transient failure attempt. Rate-limit, quota, and disabled-authentication fields are preserved, and one operation's circuit does not suppress another operation. Provider/network failures remain operational and never create candidate rejections.
- SubDL HTTP 403 on either search or download now persists a disabled authentication scope. Later work fails locally without another provider request until the API key is corrected and `subsyncd retry --provider NAME` clears that provider's states.
- Regression coverage proves the restart boundary, HTTP-call suppression, complete 1/5/15/60-minute escalation and cap, success reset, `Retry-After` precedence for 503, operation isolation, quota-state preservation, and both SubDL 403 paths. Verification passed with `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race`, `go test ./test/providercontract -tags=provider_contract`, and `git diff --check`.

### Follow-up — Movie edition evidence

- Commit `92848eb` tightens non-hash movie-edition handling without changing the provider interface or configuration. Radarr's stored edition remains authoritative. A candidate with an explicit matching edition earns 10 points; an explicit mismatch is rejected; an edition-unknown candidate remains eligible but cannot use confidence-based score bypass and therefore requires LAPSE. A verified exact file hash overrides only a conflicting textual edition label, while language, media-kind, forced-only, external-ID, year, and episode/pack safety gates remain active.
- Edition normalization now explicitly recognizes Director's Cut, Extended, Remastered, Unrated, Theatrical, Final Cut, Special Edition, Ultimate Cut, Redux, and Anniversary Edition. When a four-digit movie year is present, fallback markers are read only from the following release descriptor, preventing titles such as titles containing edition-like words from becoming false edition evidence. Structural release parsing still uses `github.com/chill-institute/torrentname` v1.4.1; the custom layer supplies canonical policy evidence and conservative fallbacks.
- Regression tests cover the expanded aliases, title false positives, exact-hash precedence, explicit conflict, unknown-edition eligibility with zero points, and both blocked and allowed LAPSE bypass. Verification passed with `go test ./... -race`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race`, and `git diff --check`.

### In progress — Structured Loki-compatible logging (Tasks 1–6)

- Commits `91fc653`, `1ea794f`, `61b5d8e`, `54272da`, `debd8d5`, and `a58317b` add configurable synchronous NDJSON logging, centralized privacy-safe event emission, application lifecycle events, webhook/readiness transitions, durable worker/reconciliation/notification events, and provider activity/state transitions. `logging.level` or `SUBSYNCD_LOG_LEVEL` selects `debug`, `info`, `warn`, or `error`; the default is `info` and a change requires restart.
- Normal successful health/readiness probes remain silent. Candidate/cache detail is debug-only. Provider search completions report bounded outcomes and cache status without media paths, release names, cache keys, download references, or URLs. Provider downloads report provider/candidate correlation, duration, byte count, and bounded outcome; returned filenames appear only at debug.
- Provider cooldown, circuit, authentication-disable, and recovery records are emitted only after the corresponding state write succeeds. Identical persisted cooldown/auth states and locally suppressed requests do not repeat transition events. Adapter type, operation scope, safe reason class, reset time, and transient attempt are structured fields when applicable; origins, request URLs, and raw provider reasons remain absent.
- Verification for the provider boundary passed with `go test ./internal/provider/... ./internal/app -race -count=1` and `git diff --check`. Focused RED/GREEN coverage includes cache hit/miss, download byte accounting and outcome classification, info-level privacy, ordinary rate windows, deduplicated cooldown/auth transitions, transient escalation/recovery, and cancellation/deadline warning severity.
- Next task: instrument workflow summaries, candidate decisions, LAPSE analysis/synchronization, installation/provenance, and failure paths without duplicating the authoritative worker outcome.

### In progress — Structured Loki-compatible logging (Task 7)

- Commit `3a0fdee` adds the workflow observability boundary. Every workflow run emits one `search.started` and one terminal `search.completed`, including early embedded/sidecar satisfaction, provider throttling, rejection, cancellation, and technical failure paths. The terminal event uses bounded outcome/reason values and never repeats raw failure detail already owned by an inventory, provider, LAPSE, installation, or worker event.
- Inventory completion reports aggregate embedded/sidecar counts and satisfaction without paths. Candidate evaluation, score components, rejected reasons, release names, tournament tiers, fallbacks, rejection decisions, and early stops are debug-only; a root-relative media path is added only when containment can be proven. The selected candidate is safe at info with provider/result ID, score, exact-hash state, and selection mode.
- LAPSE analysis and synchronization are timed at the workflow boundary and expose only typed verdict/mode/offset/ratio/confidence/agreement/coverage/part/split/version fields. Exact-hash and score-bypass paths emit no LAPSE phase event. Installation and provenance events are emitted only after their writes commit; installation checksum correlation is capped to 12 characters.
- Verification passed with `go test ./internal/workflow ./internal/app -race -count=1`, `go test ./test/e2e -tags=e2e -race -count=1`, and `git diff --check`. Next task: publish the event/field/privacy contract, add Alloy/Loki guidance and queries, update the agent handoff, and run the full repository verification matrix before any production deployment.

### Follow-up — Structured Loki-compatible logging completed

- Implementation commits are `91fc653`, `1ea794f`, `61b5d8e`, `54272da`, `debd8d5`, `a58317b`, and `3a0fdee`; documentation checkpoints are `dd954d3` and `c7354a5`. Together they implement the approved synchronous NDJSON lifecycle, privacy, correlation, transition-only provider state, debug candidate detail, typed LAPSE result, committed install/provenance, and durable worker/notification contracts.
- The README documents `logging.level`, `SUBSYNCD_LOG_LEVEL` precedence, restart behavior, info/debug policy, and Alloy ownership. Operations documentation defines common and correlation fields, level behavior, successful-probe silence, privacy guarantees, a bounded-label Docker pipeline, and practical failed-job/provider/LAPSE/selection/job-trail LogQL queries. `AGENTS.md` now requires the logging design and plan before changing this contract.
- The deployment used Alloy `v1.19.2` and a managed Grafana Cloud Loki destination. The documented dedicated Docker source selects `/subsyncd` before JSON parsing and promotes only `service`, `environment`, `level`, `component`, and `event`; dynamic identifiers remain JSON fields. An isolated copy of the exact River pipeline passed `alloy validate` on production and was removed afterward. The live Alloy configuration was not changed.
- Final local verification on 2026-09-04 passed `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, `docker compose -f compose.example.yml config --quiet`, `git diff --check`, and the repository-wide focused logging/privacy/readiness test selection. Ordinary and tagged tests used local fixtures only.
- No image was published and no production service, canary, Alloy configuration, or Loki destination was mutated. Next step requires separate approval: publish the immutable multi-architecture image, pin the production service/canary to its SHA tag at `info`, add the validated Alloy route without duplicate ingestion, and inspect one bounded workflow for complete correlation and privacy before considering temporary debug.

### Follow-up — LAPSE phase start visibility

- LAPSE analysis and synchronization now emit `lapse.analysis_started` and `lapse.sync_started` at `info` immediately before invoking the synchronizer. Existing scoped context supplies job and sanitized media identity; the events add only phase, provider, candidate ID, and compatibility version.
- Start events intentionally omit duration and result fields because execution has not completed. Each is followed by the existing matching completion or `lapse.failed` event, making long network-media reads visible without paths, command arguments, or raw tool output.
- Regression coverage asserts start-before-completion and start-before-failure ordering, safe fields, absent premature result fields, and unchanged exact-hash/score-bypass behavior.

### Follow-up — OpenSubtitles application key packaging

- Published containers carry the subsyncd developer API key required by OpenSubtitles. The runtime provider configuration uses that linked value only when `api_key` is absent, so source builds and private wrappers may still supply their own application key explicitly. A build with neither value fails provider validation before use. Account username and password remain runtime secrets and are never embedded.
- `config.example.yaml`, `compose.example.yml`, and the provider example intentionally omit the application-key setting. The README distinguishes published images from native Go and legacy-Docker builds, whose private configuration must provide the override.
- GitHub Actions reads `OPENSUBTITLES_API_KEY` from its repository secret, rejects an empty release secret, and passes it to `Dockerfile.release` through a BuildKit secret mount. The value is not a build argument or persisted in an intermediate filesystem layer. Because the value is intentionally linked into the distributed application binary, it is recoverable by image recipients; BuildKit protects its build-time transport and history, not the released application credential.
- The root `Dockerfile` remains compatible with legacy native Docker and produces an unkeyed source build. `Dockerfile.release` is BuildKit-only; its runtime and LAPSE stages must remain synchronized with the root file. The Go stage bypasses cache reuse so rotating the secret cannot silently reuse a binary linked with the previous value.

### Follow-up — public README and MIT license, 2026-09-05

- Commit: this documentation commit, based on `1def056`. Adopted the user-requested public-facing README and MIT License with copyright attributed to Tomislav Filipcic (2026).
- README now presents features, supported services/providers, and Docker Compose installation with credentials, paths, permissions, networking, webhooks, diagnostics, and updates. Private-registry login instructions are removed for the planned public release. Preserved current application-key packaging, source-build requirements, language backfill, multi-episode limitation, logging links, and operational documentation links.
- Third-party tools retain their own licenses. No runtime behavior or repository/package visibility changed. Local, unrelated review notes were left out of this commit.
- Verification passed: documentation links, MIT attribution, Compose rendering, `git diff --check`, `go test ./... -race -count=1`, `go vet ./...`, and `go test ./test/e2e -tags=e2e -race -count=1`, using writable caches and local fake servers.
- Next task: complete public repository/package visibility changes separately and retain the existing operational follow-ups.

### Follow-up — generic public documentation, 2026-09-05

- Commit: this documentation cleanup commit, based on `4b51d42`.
- Removed the host-specific manual and daemon canary deployment bundles, webhook fixtures, runbooks, and the test dedicated to those retired examples. Removed their README links and host-specific deployment ledger/cutover instructions while retaining the implementation contracts.
- Replaced private host labels and real movie/TV titles throughout tracked documentation with generic descriptions, including historical plans, review notes, provider guidance, and logging examples. Local production instructions remain outside public documentation. Runtime acquisition behavior is unchanged.
- Verification passed: name/reference scan across all 27 remaining tracked Markdown files, local-link checks, `git diff --check`, and `go test ./... -race -count=1` with writable caches and local fake servers.
- Added the requested LAPSE GitHub link to the README services table.
- Publication: committed for the user-requested push to GitHub `main`; repository and package visibility are unchanged.
- Next task: retain the existing operational follow-ups; use the local production runbook for deployment-specific details.

### Follow-up — remaining system repairs, catalog input boundary (2026-09-05)

- Commit: this Task 1 commit, based on `708db31` (`fix: harden Arr transport and reconciliation input`). Addresses R1/R5/R6/R7/R8/R9 from the remaining-systems review.
- Arr detail requests clone the supplied client policy, reject same-origin and cross-origin redirects, and return bounded owned status/operation/error classes. Complete responses are limited to 16 MiB and exactly one JSON document; context cancellation/deadline identity and safe `net.Error` timeout semantics survive without retaining raw upstream errors.
- Remote `/` path mappings now match descendants while longest component-prefix mapping and containment checks remain in force. Mixed webhooks skip individual typed outside-scope entries and apply later relevant items. Retained, durably applied event identities are checked before hydration; transactional deduplication remains authoritative for races, and failed/ignored events are not recorded as applied.
- Reconciliation now requests `arrapi` v2.0.5 descending `History` pages of 100 records instead of the unbounded `HistorySince` endpoint. It overlaps the full persisted cursor second, filters records newer than the captured end, deduplicates boundary repeats from concurrent insertion, and preserves stable-entity reduction. Invalid metadata, shrinking totals, changed duplicate timestamps, missing identities, unordered records, incomplete pages, and non-progress fail closed without committing the cursor. No historical event cap is imposed.
- Verification passed with writable caches: full `go test ./... -race -count=1`, `go vet ./...`, tagged E2E race tests, and `git diff --check`; after tightening history metadata validation, catalog/store/HTTP/E2E race checks were rerun. Tests use sanitized data and loopback fake servers only.
- Retained limitations: webhook identity lookup follows the existing 10,000-row audit retention policy; paged Arr history supplies no snapshot token, so detectable concurrent pruning fails closed for retry. Next task: independent Task 1 review, then the planned workflow/daemon and configuration/CLI repairs before final integration verification. No production access, push, image publication, or deployment occurred.

### Follow-up — strict Arr history page completeness (2026-09-05)

- Commit: this Task 1 review-fix commit, following `bcdd2cc`.
- Tightened R8: before processing a page, require exactly `min(pageSize, max(0, totalRecords - offset))` records. A short nonfinal page can no longer skip an omitted boundary record and then advance the cursor after encountering older history. Valid short final pages and duplicate boundaries caused by concurrent insertion remain supported.
- Regression RED reproduced a 99-record first page claiming 101 total records, followed by a below-cursor record: the old implementation incorrectly committed 99 mutations and advanced the cursor. GREEN rejects the first page before fetching the next or committing anything. Paging fixtures now use truthful 100-record first pages.
- Verification passed: focused paging regression tests, `go test ./internal/catalog ./internal/store -race -count=1`, and `git diff --check`, with writable caches and local fake HTTP servers. Next task: independent re-review, then continue the remaining-system repair plan.

### Follow-up — completed inventory probes and stale identity guards (2026-09-05)

- Commit: this Task 2 commit, based on `9451f3c` (`fix: bind embedded inventory to completed probes`). Addresses R2/R3 from the remaining-systems review.
- Migration `003_inventory_probes.sql` persists a separate completed-probe fingerprint; old rows retain their tracks but have no completed marker, so their next normal inventory refresh probes locally. Successful empty results are cacheable. A SQLite trigger invalidates the marker on catalog path/file-ID/size/mtime changes or deletion. Path-only renames safely reprobe; no startup scan, network request, or probe backfill was added, and the embedded migration lineage remains strict.
- Inventory reads catalog identity, deletion state, optional probe identity, and tracks from one SQLite snapshot. Refresh first matches the requesting path/file ID against that snapshot, treats observed stat size/mtime separately from Arr DateAdded, rechecks the live file after probing/scanning, and writes only after a transactional comparison against the original full catalog fingerprint. Stale replacement, rename, deletion, or changed probe input returns a technical failure without rewriting identity, restoring stale installation eligibility, or rejecting candidates. Sidecars remain live scans.
- Actual Arr deletion retains media/history/provenance but now marks the row deleted; both catalog upsert paths clear that marker on reimport. The inventory and installation/outbox transactions reject deleted rows. Integration coverage confirms a deletion after publication restores the prior sidecar and leaves notification intents absent. Existing search completion and lease handling remain unchanged in this task.
- RED reproduced skipped first probes, old tracks inherited by replacement files, requests predating catalog replacement, file/catalog mutations during a probe, cache reuse after a catalog-only fingerprint change, and retained-row deletion accepting both inventory and notification-bearing installation commits. GREEN includes real SQLite migration, compare-and-swap, empty-cache, stat-refinement, deletion/reimport, installation rollback, and workflow technical-failure regressions.
- Verification passed with writable caches: `go test ./internal/inventory ./internal/store -race -count=1`; `go test ./internal/app ./internal/workflow ./internal/catalog -race -count=1`; focused deletion/technical-staleness workflow race tests; final inventory/store race checks and final `go test ./internal/workflow -race -count=1`; migration idempotence race check; `git diff --check`. Loopback fake-server suites required the approved sandbox escape after the restricted sandbox refused local listeners. No live services, production access, push, or deployment occurred. Full integration gates remain controller-owned.
- Next task: independent Task 2 review, then continue the remaining-system repair plan. The later queue filter should exclude durable `media.deleted` rows alongside obsolete languages.

### Follow-up — deletion preserves active search ownership (2026-09-05)

- Commit: this Task 2 review-fix commit, following `03c3cb9`.
- Independent review identified the retained deletion branch clearing active owners, allowing a same-key reimport to start a second workflow. Delete now marks searches terminal/deleted and clears their due time/rerun request while preserving every durable owner and expiry, including abandoned leases. Reimport retains that owner and coalesces one rerun using the existing scheduling contract.
- Owner-checked completion reads the current tombstone and rerun state in the same transaction. Still-deleted media remains terminal/deleted with no retry or counter advancement, regardless of the obsolete workflow outcome. Reimported media follows the ordinary coalesced-rerun branch; stale completion cannot erase its immediate rerun.
- RED reproduced missing original owner plus a competing lease after delete/reimport; a second RED after preserving ownership alone showed old completion reviving deleted work as a pending technical retry. GREEN covers real SQLite delete-active/reimport-twice/no-second-lease/owner-completion/exactly-one-rerun and delete-active/completion-stays-terminal paths.
- Verification: focused lease-transition race regression and affected store/worker/inventory/workflow race suites, plus `git diff --check`, using writable caches and approved loopback fake-server access. No live services or production actions. Next task: independent review of this follow-up, then controller-owned integration and remaining repairs.

### Follow-up — lock startup and isolate diagnostic assembly (2026-09-05)

- Commit: this Task 3 commit, based on `6265b03` (`fix: lock startup mutations and isolate diagnostics`). Addresses R4/R13 from the remaining-system review.
- Mutable `App.New` now owns the advisory process lock before capability checks, migrations, configured-instance/search writes, provider construction, and durable cache initialization. The lock remains held through command execution and `Close`, and failed assembly releases it. `Open` selects an explicit diagnostic lifecycle for `explain`, `doctor`, and `analyze-sync`; no exported skip-lock option exists.
- Diagnostics open SQLite with `mode=ro` and connection-level `query_only`, validate the complete embedded migration set without applying it, and read the daemon's live WAL without `immutable`. Missing/outdated/unknown-schema databases fail with a bounded initialization instruction. Diagnostic assembly does not initialize providers, workers, pack caches, speech caches, configured instances, or language searches; mutating methods are rejected. `analyze-sync` uses a private temporary speech cache removed on return, including technical failure.
- `explain` distinguishes unprobed, completed (including empty), and stale probe state using the separate persisted marker and a local file stat; it never probes or refreshes inventory.
- Once startup logs `service.start_failed`, `Open` returns a sanitized typed already-reported error. CLI recognizes that contract structurally (including wrapped errors) and does not print the raw failure again. Configuration-aware redaction now includes data directory and LAPSE executable paths, preserves media/temp/secret redaction, and applies bounded single-line UTF-8-safe text normalization.
- RED reproduced concurrent mutable assembly, diagnostics attempting the mutation lock, direct `Open` leaking a configured secret/media path, and `explain` omitting probe completion. GREEN covers early competing-startup rejection without configured-instance writes, failure/Close lock release, fresh mutable initialization, read-only WAL visibility/write rejection/current-schema validation, no diagnostic language backfill or cache initialization, private diagnostic cache cleanup, and actual CLI/Open single-owner startup sanitization.
- Verification: `go test ./internal/app ./internal/store ./internal/cli -race -count=1`, `go vet ./internal/app ./internal/store ./internal/cli`, and `git diff --check`, with writable caches and approved loopback listener access. No live services, production access, push, or deployment. Next task: independent Task 3 review, then the remaining-system repair plan; full integration gates remain controller-owned.

### Remaining systems repairs Task 4 — dispatch scope and shutdown — 2026-09-05

- Commit: `fix: scope worker dispatch and preserve shutdown drain` (this commit, based on `08d6985`); resolves remaining-system findings R10, R11, and R12.
- Tightened dispatch admission around parent cancellation, including canceled startup, claims returning after cancellation, each reconciliation, and notification admission. Only accepted work uses the detached graceful-drain context; abandoned durable leases are never falsely completed. The existing two bounded shutdown windows remain unchanged.
- Added a copied optional repository search scope. Mutable default worker assembly filters current instance/language routes and active media in SQLite before priority ordering and LIMIT. Generic repository users remain unscoped; removed jobs and retry history remain available for re-enable, lease-owner renewal/completion stay independent, and committed notification delivery is not search-route filtered.
- Compose now allows 75 seconds, covering the actual default two 30-second worker drain windows plus exit margin. The configuration contract parses YAML and the duration and compares it with prepared worker defaults.
- RED reproduced canceled search dispatch/completion, post-reconciliation and returned-notification dispatch after cancellation, missing claim scope, tombstone claims, obsolete-route worker assembly, and the insufficient Compose budget. GREEN: `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker ./internal/store ./internal/app -race -count=1`, affected-package `go vet`, gofmt, and `git diff --check`. App tests required local fake listener permission; no external services or production actions occurred.
- Next task: independent combined R1–R13 review and controller-owned repository-wide verification/publication. No push or production rollout performed in this task.

### Follow-up — remaining systems repair integration (2026-09-05)

- Commit: this documentation integration commit, based on `0a9b2ab`; code repairs are `bcdd2cc`, `9451f3c`, `03c3cb9`, `6265b03`, `08d6985`, and `0a9b2ab`. All R1–R13 in the remaining-systems review have permanent regression coverage. Every implementation group passed independent review; review follow-ups tightened history-page completeness and delete/reimport lease preservation.
- Adopted the repaired contracts in AGENTS, architecture, provider and operations guides, source-build instructions, and the original review's commit map. Migration 003 establishes completed-probe/deleted-media state without a startup scan; read-only diagnostics require an existing current-schema database. Disabled search routes retain work for re-enable, and Compose permits both default shutdown windows plus margin. Scoring thresholds and provider policy are unchanged.
- Final verification on `0a9b2ab`: `go test ./... -race -count=1`, `go vet ./...`, `go test ./test/e2e -tags=e2e -race -count=1`, release-Dockerfile parity, `docker compose -f compose.example.yml config --quiet`, `gofmt -l cmd internal test`, and `git diff --check` all passed. Tests used sanitized local fixtures and fake servers only.
- The original main checkout remains clean at `1a7a2ba`; fetched GitHub main matches it. No production inspection or deployment occurred. Next task: independent combined branch review, then the authorized fast-forward and GitHub push.

### 2026-09-05 — Final deletion integration repairs (F1/F2)

- Commit: this task's `fix: preserve deleted media across scans and upgrades` commit; exact SHA and RED/GREEN evidence are recorded in `.superpowers/sdd/2026-09-05-remaining-system-repairs/final-fix-report.md`.
- Tightened scans and configured-language backfill to enumerate active media only. Retained deletions no longer inflate scan counts, abort forced scans before later active media, or create new-language work. A concurrent deletion during probing still returns technical `ErrStaleInventory`; inventory and installation guards remain intact.
- Rejected the earlier no-historical-deletion-inference limitation for positive durable evidence in the supported 001/002 lineage. Migration 003 requires nonempty unanimously terminal/deleted searches, the latest linked applied lifecycle event by insertion ID to be a delete, and its timestamp to cover the stored media update. Later reimports/renames or direct updates prevent inference; missing/pruned audit, absent searches, and mixed outcomes stay ambiguous/active. No startup scan, probe, network request, identity backfill, or retained-row deletion was added.
- Migration regression controls cover both supported pre-003 migration states, equal-timestamp event ordering, a stale removed-language deleted row after reimport, a newer direct upsert, and ambiguous evidence. Known historical tombstones reject claims, inventory CAS, and installation commits after upgrade. The grouped audit query scans retained events once and uses existing primary/search indexes without adding a persistent index.
- Verification: focused RED reproduced both reported integration failures; focused GREEN and affected app/store/inventory/workflow/catalog race suites passed. Affected-package `go vet`, gofmt, and `git diff --check` passed. Local fake HTTP listener permission was needed for the broad suite; no live services or production were accessed.
- Next: controller independently reviews the final fix diff, refreshes the full integration gate, and integrates/publishes the authorized branch.

### Follow-up — final deletion integration verification (2026-09-05)

- Commit: this documentation verification commit, following `5407480`. The combined review's two confirmed findings are repaired: scans and language backfill exclude tombstones, and migration 003 preserves locally proven pre-upgrade deletion while leaving absent/conflicting evidence active. Contributor guidance, the plan, and review commit map now match that tightened contract.
- Fresh verification on `5407480` passed: complete `go test ./... -race -count=1`, `go vet ./...`, tagged E2E race (4.494s), release-Dockerfile parity, Compose configuration validation, gofmt, and diff checks. Local fixtures/fake servers only; no production operation.
- Next task: scoped independent re-review of the final fix wave, then the authorized fast-forward and GitHub push. The conservative migration cannot reconstruct pruned audit history; ambiguous rows remain active rather than risking false deletion after reimport.

### Follow-up — remaining repairs ready for publication (2026-09-05)

- Commit: this publication-ledger commit, following `2466415`; runtime code remains `5407480`. The final scoped re-review approved F1 and F2 with no new actionable issues. All thirteen original findings and both combined-review integration findings are closed under the documented contracts. Full verification evidence above remains current; subsequent commits changed documentation only.
- Migration decision: preserve only positively proven historical deletion; absent/pruned or conflicting chronology stays active to avoid suppressing legitimate reimports. Ambiguous older rows can still need reconciliation or operator correction. No scoring-policy change or production deployment is included.
- Publication: proceed with the user-authorized fast-forward of main and GitHub push. Next task: observe repository CI; production rollout remains a separate operator action.

### Verification — single-run LAPSE candidate evaluation (2026-09-05)

- Runtime commit: `3ad9aee`; this is an uncommitted documentation-only verification entry. User requested verification and a suggestion/plan, not implementation or production changes.
- Verified official LAPSE v2.0.5 tag at source commit `8d57e43ad72c2d05794d83591cf311499ae64c22`. `engine/main.cpp` computes the same verdict/ranking metrics before both save branches; `--dry-run` only skips the writer. Output mode exposes every field subsyncd consumes, including confidence, agreement, coverage, offset/ratio, parts/splits, verdict, written and output. Strict non-solid results refuse writes.
- Downloaded the official macOS arm64 archive to temporary storage and verified SHA-256 `779bc2438eb33ff9c5168679f79b815e0efce1b7d9a37fec9b2ac1272b3f935e` against GitHub release metadata. A seeded 100-cue subtitle-reference fixture shifted by 4200ms returned identical decision JSON for dry and output runs (solid, offset -4200ms, confidence1); only the output run created the file, whose cue timings matched the reference. Input checksum remained unchanged; no backup was created. A 40-cue periodic uncertain fixture returned exit2/unsure identically in both strict runs and created no output. No --force or relaxed confidence was used.
- Verified upstream quirk: dry-run solid reports written=true even though no file exists. Keep existing exit/protocol/verdict checks plus actual output-path, regular-file, size and subtitle-syntax validation; never rely on written alone. These are native synthetic contract tests, not a Linux/Hades benchmark or complete audio/split-format matrix.
- Proposed plan: use the existing validated output-producing SynchronizeCandidate once per LAPSE-required candidate, retaining source identity/checksum separately from its temporary output and SyncResult. Preserve exact/score bypass, lazy score tiers, all equal-score comparisons and existing tie-breaks; install the best already-produced output without another LAPSE process. Apply to broad and cached-pack paths, keep cached sources immutable, retain fallback artifacts until no longer needed, and keep fingerprint/atomic installation/outbox/rollback guards. Keep Analyze for read-only analyze-sync diagnostics.
- Verification plan before shipping: focused TDD process-count/tie/fallback/cleanup/rejection-provenance tests, strict malformed/missing/invalid output and cancellation cases, cached packs/upgrades/bypass regressions, real pinned Linux binary fixture comparisons, full race/vet/tagged E2E, and updated operational events/docs. A unique evaluated winner changes two LAPSE invocations to one; three tied viable candidates change four to three. Runtime savings require cold/warm-cache measurement and are not claimed as a fixed percentage.
- Next task: obtain agreement on this proposal, then write the implementation design/plan and implement in isolation if requested. No service restart, provider call, production media access, configuration change, code edit, commit, or push performed in this verification.

### Implementation — single-run LAPSE preparation (2026-09-06)

- Commit: `cfcb21c` (`perf: prepare LAPSE candidates in a single run`); focused RED/GREEN evidence is summarized below.
- Adopted one validated `SynchronizeCandidate` call per LAPSE-required acquisition candidate. Preparation retains the original selected source separately from its output and actual `SyncResult`; tied tiers rank that result's confidence and installation consumes the retained artifact without another process. Workflow no longer requires diagnostic analysis. Exact/score bypass, same-candidate reassessment, upgrades, lazy tiers/top-three cap, original-source rejection identity, strict syncer validation, atomic installation/outbox, and rollback guards remain intact.
- Cached members stay immutable. Cached, exact, and broad preparation use distinct output indexes, retained outputs survive tie reordering/fallback, and failed/discarded private artifacts are removed promptly. Tightened cancellation after cached preparation so it starts no provider search. Acquisition emits only the sync lifecycle plus `lapse_prepare` decisions; standalone diagnostic analysis remains unchanged.
- Focused RED reproduced extra unique-leader analysis, three analyses plus winner-only synchronization for three viable ties, and a provider search after cached cancellation. GREEN covers real installer/SQLite output bytes, checksum and full synchronization provenance; retained-output installation fallback; original-source rejection checksums; cached/provider output uniqueness; immutable cached members; failed-output cleanup; cancellation; technical failure classification; and existing bypass/diagnostic boundaries.
- Verification passed with `GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache`: `go test ./internal/workflow ./internal/syncer ./internal/app ./internal/worker -race -count=1`, tagged `go test ./test/e2e -tags=e2e -race -count=1`, focused regressions, gofmt, and `git diff --check`. Fake-server suites required loopback permission; no real Arr/provider/Silo or production operation occurred.
- Next task: controller task review, final repository-wide verification and independent branch review, then the user-authorized GitHub integration/push. No push or production deployment performed by this implementation task.
- Controller full-suite follow-up: repaired a pre-existing worker test fake exposed by concurrent completion/renewal. Its global completion flag incorrectly rejected renewals for unrelated jobs; tracking completing job IDs matches the repository contract while preserving the same-job guard. The existing concurrency test now deliberately overlaps completion with other jobs, reproducing RED before the fixture-only change. No worker runtime behavior changed. The full worker package passed `go test ./internal/worker -race -count=20` (13.521s) with the writable caches above.

### Single-run preparation verification and documentation — 2026-09-06

- Runtime commit `cfcb21c`; documentation commit follows it. Updated contributor invariants, provider/operations/architecture guidance and historical design status to describe one acquisition output-producing invocation per required candidate, retained-output ranking/fallback, original-source rejection identity and sync lifecycle ordering. Standalone diagnostic analysis remains dry-run.
- Verified the official Linux arm64 LAPSE v2.0.5 archive SHA-256 `23226fea64f7141687b764e5d080b6ed4f9e2fbed476938363e993bd3705ee17` in an isolated local Debian 13.2 container with networking disabled, no configuration/media/data mounts, and synthetic subtitle references only. Dry/output decision metrics matched; solid output timings matched the reference with unchanged source/no backup; strict unsure returned exit 2 without output. This supplements the native macOS/source verification above, and is not a Hades performance benchmark or complete audio/split-format matrix.
- Full `go test ./... -race -count=1` passed after the worker test-double correction documented above. `go vet ./...`, tagged E2E race, release Dockerfile parity, Compose configuration, gofmt and diff checks passed. All network tests used local fakes; no production action occurred.
- Next action: finish independent task and whole-branch review, then publish to GitHub main as authorized. No configuration/scoring thresholds or production deployment changed.

### Single-run LAPSE final review and publication handoff — 2026-09-06

- Runtime `cfcb21c`, documentation `98a22cc`, and design `9cffb55` reviewed against baseline `3ad9aee`. Independent task and whole-branch reviews approved with no actionable, deferred or unresolved findings. No acceptance thresholds or configuration values changed.
- Full repository race, affected package races, tagged E2E race, vet, Dockerfile parity, Compose, formatting and diff checks passed. The pinned Linux arm64 synthetic binary check confirmed identical dry/output decision metrics and strict refusal without output. Unique successful candidates now use one LAPSE process; three viable ties use three. Fixed timing/CPU savings are not claimed.
- GitHub main was refreshed and remains at baseline; the original workspace patch exactly matches the preserved verification note already included in this branch. Final publication will remove only that duplicate local patch, fast-forward main to this verified tree and push as explicitly requested. No production deployment, live provider search or media mutation occurred.
- Next task: operator rollout when requested; standalone `analyze-sync` remains read-only dry-run. This entry closes the implementation/review task and accompanies its final publication commit.
