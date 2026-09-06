# Provider cache and download review — 2026-09-06

Scope: coordinator cache persistence/reuse, download-reference normalization, rejection propagation, and the reported astisub stderr message. No production changes were made.

## Confirmed and repaired

1. Sanitized links were replayed as usable downloads. Relative Titlovi query links became bare download paths; absolute links and pack-member references could also lose acquisition data. A versioned envelope now records a refresh flag when sanitization changes result identity or any download reference. Only unchanged references and empty results are reusable. Refresh errors return without incomplete fallback. Cache namespace v6 excludes earlier entries.
2. Malformed links could fetch a root page. Titlovi and SubDL now reject absent/root-equivalent, query-only, fragment-only, userinfo-bearing and network-path references before download requests. Titlovi also rejects its bare download endpoint without a query. Normalization uses the same checks. Valid resource paths and allowed absolute links remain supported. Download-reference failures are technical, not content rejections.
3. SSA parser callbacks printed raw subtitle content. Extraction, LAPSE-output validation and installation now use explicit parser options with no logging callbacks. Parsing and validation rules are unchanged. No process-global logger is replaced.

## Evidence

- Focused RED/GREEN tests reproduced lost query/absolute/member references, failed-refresh fallback, legacy array replay, malformed endpoint requests and raw SSA logs.
- A SQLite close/reopen test uses a local HTTP endpoint returning a single zero without its identifying query and a valid synthetic subtitle with it. The repaired coordinator refreshes the link and extraction succeeds.
- Credential-stripping tests remain active. Safe references and empty results retain six-hour caching.
- Tests use sanitized fixtures and local fake services. Final verification is recorded in implementation-status.md.

## Remaining review observations

- The dependency's WebVTT parser has hardcoded standard-log calls for repeated voice tags and malformed inline durations (go-astisub v0.42.0, webvtt.go). Unlike SSA, it has no per-call logging options. This separate pre-existing privacy/log-format issue requires a dependency-level or broader parser change; it is not fixed by the SSA callback repair.
- Nested pack-member IDs are not generically query-sanitized. Current production adapters do not populate DirectMembers, so this is dormant hardening rather than a demonstrated production path. Direct-member download references are stripped and now trigger refresh.

## Rollout

Existing rejection rows cannot prove whether content was bad or an old cached link was incomplete. They are not automatically deleted or globally invalidated. Deploy the repaired image, then use the explicit media/language-scoped retry override for confirmed affected work such as Braveheart Croatian. Keep the daemon stopped during the manual mutating search. Refresh may increase provider search traffic, particularly for Titlovi; normal limits and persisted cooldowns still apply.
