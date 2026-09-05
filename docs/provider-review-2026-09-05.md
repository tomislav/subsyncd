# Subtitle provider review — 2026-09-05

Reviewed commit: `39f5e20`. Scope: OpenSubtitles, SubDL, Titlovi, shared coordination, transport, cooldowns, and downstream scoring/selection. The original review made no production-code changes and used no production access or live provider calls. The user subsequently authorized repairs and GitHub publication; see the repair status below.

## Findings

1. **P1 — Titlovi fabricates candidate IMDb evidence** (`internal/provider/titlovi/client.go:240`). The adapter copies the query IMDb into every result although the response model has no IMDb field. A local response for Different Movie (1999), queried for Target Movie (2024), acquires the target ID, avoids the movie-year rejection, and scores 80 with matching release metadata. That satisfies default score-bypass requirements. Treat the query as a filter, not returned identity; preserve unknown candidate IDs. This proves the client-side gap, not that Titlovi currently returns such a response in production. The API's strict filter exclusivity was not established.

2. **P1 — SubDL resolution suffixes create false episode packs** (`internal/provider/subdl/client.go:317`). `S01E02.1080p.WEB-DL-GROUP` parses as episodes 2–1080, replacing explicit episode 2 with pack metadata. Ordinary singleton subtitles then require LAPSE and strict pack selection; generic filenames can be rejected. Use the strict shared episode-range contract and test ordinary suffixes, cross-season endpoints, and malformed ranges. Existing identity tests contain this trigger but do not assert pack shape.

3. **P2 — Titlovi token refresh loses earlier pages** (`internal/provider/titlovi/client.go:178`). A page-two 401 recursively restarts with mutated parameters still containing `pg=2`, discarding accumulated candidates. The local reproduction requests pages `[1,2,2,2]` and returns `[102,102]`, losing page-one result 101. Retry the interrupted page while preserving results or restart with clean pagination. Existing refresh coverage fails only on page one.

4. **P2 — SubDL absolute numbering loses episode points** (`internal/provider/subdl/client.go:254`, `:277`). For target S01E02/absolute102, singleton102 is stored in `Episode` with no `AbsoluteEpisode`; range101–110 is stored in ordinary range fields. Both are accepted by normalization but fail `HasEpisodeEvidence`, losing 20 points and required bypass evidence. Preserve absolute numbering in the domain fields consumed by matching. Existing absolute-search tests verify the request but return ordinary episode coordinates.

5. **P2 — SubDL direct-member selection ignores season** (`internal/provider/subdl/client.go:309`). For S01E02, members S02E02 followed by S01E02 select the former. Scoring rejects it and the correct member/parent alternative is lost. Match known season as well as episode and distinguish absolute numbering; test conflicting members and ambiguous matches.

6. **P2 — Queued requests bypass newly persisted cooldowns** (`internal/provider/limiter.go:87`). State is checked before waiting for rate and concurrency permits, never after. A second request queued behind an active one proceeds even if that active request establishes a cooldown before releasing its permit. A deterministic local test reproduces this. Recheck state after acquiring permits and release them on rejection; cover disabled authentication too. This matters with concurrent workflows/provider calls.

7. **P2 — Response-body network failures do not open the provider circuit** (`internal/provider/transport.go:37`, `:63`). Only errors from `HTTP.Do` update transient state; a 200 response whose body fails with unexpected EOF has already been marked recovered. A local failing body leaves no circuit state. Record genuine transport-read failures without treating malformed payloads, writer failures, or cancellation as provider outages, and defer recovery until appropriate completion.

8. **P2 — Login 401 is retried for every job** (`internal/provider/opensubtitles/client.go:218`, `internal/provider/titlovi/client.go:95`). Login returns authentication errors before the existing post-refresh disabling paths. Repeated searches with rejected credentials therefore repeat login instead of persistently disabling the instance. A local OpenSubtitles fixture observes two login requests for two searches. OpenSubtitles explicitly instructs clients to stop retrying identical rejected credentials. Persist auth rejection and retain the existing explicit retry mechanism.

9. **P2 — OpenSubtitles ignores the returned API host** (`internal/provider/opensubtitles/client.go:201`, `:282`). Login discards `base_url`; all API requests continue against the configured host. The official contract requires subsequent requests to use the returned host, including VIP routing. A fake login returning the VIP host confirms it is ignored. Validate and adopt the documented host without allowing arbitrary credential-bearing destinations or breaking explicit test/private endpoints.

10. **P2 — Successful download-link requests can block their own file transfer** (`internal/provider/opensubtitles/client.go:398`, shared transport state persistence). If `/download` returns a valid link plus an exhausted rate-limit window, that state is persisted under `OperationDownload`. The immediately following CDN GET uses the same scope and is blocked before transfer. A local fixture with `RateLimit: "default";r=0;t=3600` returns a cooldown and performs zero CDN requests. Separate API link-quota gating from redemption of an already issued link while retaining concurrency and actual CDN failure handling. This is a reproduced supported-header case; its frequency on the live service is unknown.

11. **P2 — OpenSubtitles sends the application key to any returned HTTPS download host** (`internal/provider/opensubtitles/client.go:467`). Download URLs accept arbitrary HTTPS origins, and `newAbsoluteRequest` always adds `Api-Key`. A fake link to another origin receives the configured key. Remove API credentials from ordinary signed/CDN downloads, or restrict them to verified API destinations if documented as necessary. Official embedded keys are already distributed, but private overrides should not be forwarded to unrelated hosts.

OpenSubtitles login contract: [official documentation](https://ai.opensubtitles.com/docs#login), checked 2026-09-05. It documents both rejected-credential handling and returned-host routing.

## Validation and limits

- Existing `go test ./internal/provider/... -race -count=1` and `go vet ./internal/provider/...` pass. HTTP baseline tests use local fake servers.
- Temporary Go overlays reproduce findings without editing tracked tests: `/tmp/provider-review-overlay.json`, `/tmp/subsyncd-subdl-review/overlay.json`, `/tmp/subsyncd-titlovi-review/`, and `/tmp/subsyncd-review-os-overlay.json`. These are ephemeral evidence; turn them into permanent regression tests when fixing the findings.
- Shared transport and SubDL overlays intentionally fail assertions for expected correct behavior. Titlovi and OpenSubtitles evidence tests characterize the observed defects. No real credentials or provider responses were used.
- This was a source/fixture review, not a live provider contract test or a fresh production diagnosis. Findings do not establish that an existing installed subtitle is incorrect.

## Improvements after correctness repairs

- Add a bounded pagination policy to OpenSubtitles, with tests for empty/repeated pages and unreasonable `total_pages`; preserve exact-candidate completeness under a clearly documented policy.
- Consider defense-in-depth decoding/filtering of OpenSubtitles translation flags in addition to the newly explicit server query filters. Unknown/unmarked translation quality still cannot be inferred reliably.
- Keep release-score weights unchanged until these evidence errors are repaired. Several findings alter scores or bypass eligibility without requiring weight changes.

## Repair status

All eleven findings are implemented under `docs/superpowers/plans/2026-09-05-provider-review-repairs.md`; permanent regressions replace the temporary evidence. Titlovi no longer invents IMDb identity and retries interrupted pages without dropping results. SubDL shares strict pack-range parsing, preserves absolute coordinates, and requires consistent unique direct-member evidence. Shared HTTP handling rechecks queued availability and records genuine body failures while preserving quota/auth state. OpenSubtitles validates session routing, disables rejected logins, redeems issued links under a separate transfer scope, and omits credentials from file requests. Both login adapters preserve definitive auth rejection. Normalized cache version is `candidate-v5`.

Repository race tests, vet, and tagged local E2E tests passed. Independent review approved all eleven contracts and the final follow-ups: body timeouts report throttling rather than cancellation, and underlying EOF releases permits even if recovery-state persistence fails. Final repository race tests, vet, and tagged E2E tests passed after those changes. Optional pagination/translation-filter improvements remain follow-up observations; scoring weights are retained. No live provider calls or deployment were performed.
