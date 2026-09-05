# Providers, matching, and scheduling

## Language routing

Language keys are canonical BCP 47 tags. A language can name one or several provider instances, in priority order:

```yaml
languages:
  hr:
    providers: [titlovi-main]
  en:
    providers: [opensubtitles-main, subdl-main]
  pt-BR:
    providers: [opensubtitles-main, subdl-main]
```

Provider instances are independently credentialed and throttled. Startup rejects unknown providers and language/provider combinations the adapter cannot represent. There is no hard-coded English/Croatian coupling in the workflow.

Language and provider routing is loaded at startup. When a newly configured language has no search row for an already-indexed media item belonging to a currently configured Arr instance, startup creates one immediately due missing-priority row using only SQLite. Existing rows—including attempts, future upgrade times, leases, and rerun state—are never reset. Unsupported multi-episode files receive the same terminal unsupported outcome used during import. Adding a provider to an existing language changes that language's future searches after restart without rewriting its schedule.

Hearing-impaired/SDH tracks and candidates are disallowed by default. Set `allow_hearing_impaired: true` at the configuration root to opt in; when enabled, an existing matching SDH track may satisfy the language and an HI provider result remains eligible.

## Built-in providers

### Titlovi

Titlovi uses the supported Kodi API with an API-enabled account. It supports `bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl`, and `sl`. Search is broad-only and paginated (five pages by default). Token refresh preserves earlier result pages. The requested IMDb ID is a search filter only; it is never copied into candidate identity or awarded external-ID points. Episode-zero results become season-pack evidence only when the returned season matches; they are never treated as the requested episode automatically. Downloaded episode archives are checked independently of that search metadata: explicit wrong-episode filenames reject only that candidate, including a one-subtitle archive, while one generic filename remains usable for an exact episode result.

```yaml
titlovi-main:
  type: titlovi
  username: ${TITLOVI_USERNAME}
  password: ${TITLOVI_PASSWORD}
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
  max_pages: 5
```

### OpenSubtitles.com

Every exact-hash and broad search page explicitly sends `ai_translated=exclude` and `machine_translated=exclude`, using the [official search API parameters](https://opensubtitles.stoplight.io/docs/opensubtitles-api/a172317bd5ccc-search-for-subtitles). This excludes results marked by OpenSubtitles as AI or machine translated without relying on server defaults. There is no configuration opt-in. Normalized search-cache version `candidate-v5` prevents reuse of results predating translation exclusions and corrected provider identity/episode normalization; existing installed subtitles and downloaded pack caches are unchanged.

OpenSubtitles supports exact file-hash search and broad metadata search as separate phases. The hash is computed on the first exact search and stored by algorithm plus path, Arr file ID, byte size, and nanosecond mtime. It reads only the first and last 64 KiB plus the file size, supports normal 64-bit media sizes without an artificial 9 GB ceiling, and is reused until any part of that fingerprint changes. Exact results score 100 and bypass LAPSE, but an unusable exact candidate advances to the next exact candidate and then broad fallback rather than ending the job. For movies, feature IMDb/TMDB IDs are comparable media identities. For episodes, OpenSubtitles feature IDs identify the episode while Sonarr supplies series IDs, so the adapter compares OpenSubtitles `parent_imdb_id`/`parent_tmdb_id` instead. Missing parent IDs remain neutral; episode feature IDs are never misrepresented as series IDs.

After authentication, official OpenSubtitles endpoints use the validated session API host returned by login (`api.opensubtitles.com` or `vip-api.opensubtitles.com`). Explicit private/test endpoints remain isolated from official routing. Temporary subtitle-file requests carry no application key or bearer token. Issued links use a separate `download_transfer` operation, so an exhausted API download quota cannot prevent redemption of a link already issued; transfer concurrency and failure backoff still apply.

The published subsyncd container includes the application's OpenSubtitles API key. Configure only the account username and password. Source builds and private wrappers may set `api_key` explicitly to override the built-in value; a build without an embedded key requires that override.

```yaml
opensubtitles-main:
  type: opensubtitles
  username: ${OPENSUBTITLES_USERNAME}
  password: ${OPENSUBTITLES_PASSWORD}
  user_agent: subsyncd
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
```

### SubDL

SubDL is broad-only. For episodes it searches standard season/episode, an available absolute episode, and the season-pack form, merging normalized results by stable identity. Later duplicates fill missing identity fields, union release evidence, and retain the strongest rating/popularity signals before incomplete episode results are filtered. Missing response title/year remain unknown rather than being copied from the search request, so request defaults cannot earn identity points or qualify for LAPSE bypass. Absolute-numbered results retain explicit absolute episode/range fields for matching. Direct-member selection checks season as well as episode evidence, and release range parsing shares the conservative archive parser so resolution suffixes cannot create packs. It supports its published two-letter languages plus `pt-BR` and `zh-Hant`; configuration validation is the definitive capability check. API keys returned in download URL queries are discarded during normalization. The configured key is reconstructed only on the outbound download request and must never become a candidate ID, cache value, provenance record, or log field.

```yaml
subdl-main:
  type: subdl
  api_key: ${SUBDL_API_KEY}
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
```

## Search order and caches

For one media/language job:

1. Reuse embedded inventory only when an actual completed probe has matching path, Arr file ID, size, and mtime; empty completed probes are valid, fresh catalog rows are not. Always rescan sibling sidecars. A stale or deleted catalog identity aborts inventory refresh technically before acquisition.
2. Stop for a full matching embedded track or a matching protected/user-owned sidecar. Forced-only and unknown-language tracks do not satisfy a normal request.
3. Try a reusable, checksummed season-pack member.
4. Run the explicit exact-hash phase against hash-capable providers sequentially in configured order. Exact candidates are uncapped and downloaded one at a time; a forced, rejected, malformed, or wrong-member candidate advances locally to the next exact candidate. Stop at the first committed installation.
5. After exact candidates are exhausted, run the explicit broad phase against every assigned provider and merge results in configured order. Duplicate `(provider_id, result_id)` rows collapse before caching, persistence, scoring, or shortlisting; missing identity evidence is filled from later duplicates while conflicting nonempty identity values retain the first stable value.
6. Persist score and rejection evidence for every result, but never a signed download URL or provider token.
7. Remove active deterministic rejections before shortlisting, allowing later-ranked candidates to advance.
8. Cap only the broad, non-hash shortlist at the best three eligible candidates and partition it into equal release-score tiers.
9. Lazily download/prepare only the highest remaining tier. For a first-install candidate scoring at least 75, bypass LAPSE only when identity, release group, and (for TV) episode evidence are all present.
10. For equal-score LAPSE candidates, prepare the complete tier with one strict output-producing invocation per candidate and rank solid results by the resulting confidence, provider priority, rating, provider/result identity. Install the winner's retained output directly; on candidate-local installation failure, try another prepared tie before a lower score tier.

Candidate downloads, extraction, and synchronization scratch use the system temporary directory. Raw archives and discarded candidates are released promptly; viable equal-score candidates remain available through installation fallback. Final subtitle staging remains beside the media, while pack-cache publication stages inside the persistent cache root.

Candidate-local installer validation failures also advance within the tournament, recording the original selected artifact's rejection evidence. Format-changing upgrades advance without quarantining the candidate. Media changes, publication failures, and rollback failures remain terminal technical errors; filesystem and transactional media-fingerprint checks prevent installation against stale media.

Search-result cache entries live for six hours. Season packs default to 24 hours and a total 512 MiB LRU ceiling. Pack downloads are content-addressed and immutable; every cache hit reloads the manifest, verifies checksums, and reruns strict member selection for the current episode.

Episode ranges are recognized only through complete hyphenated tokens: `S01E01-E03`, same-season `S01E01-S01E03`, or same-season `1x01-1x03`. Cross-season, reversed, chained, incomplete, and suffix-contaminated forms fail closed. Release suffixes such as `S01E01.1080p` remain single-episode evidence rather than becoming a range. Forced-only policy applies to every selected archive member, including a plain one-member movie payload.

Candidate rejections are not provider blacklists. They are scoped to one media/language/provider result and expire after 30 days. Media fingerprint, stable release metadata, selected member checksum, LAPSE compatibility version, or synchronization-policy changes invalidate the applicable match. Volatile provider rating, popularity, download counts, and temporary download URLs deliberately do not change the rejection identity. A downloaded archive with no unique member for the requested episode is recorded as `pack_selection`; the workflow continues through its remaining shortlist without opening a provider cooldown.

## Score model

Known identity conflicts reject before points are considered. Conflicts include language, media kind, forced-only results for a full-language request, external IDs, movie year beyond ±1 without an exact external ID, season/episode outside pack scope, and a different known edition/cut. A verified exact file hash overrides only a conflicting textual edition label; all other identity gates remain active. Unknown evidence is normally neutral.

| Signal | Points |
| --- | ---: |
| Verified exact media-file hash | 100 (terminal) |
| Matching IMDb/TMDB/TVDB ID | 20 |
| Normalized title and year | 15 |
| Matching episode or containing pack | 20 |
| Release group | 25 |
| Source | 15 |
| Edition/cut | 10 |
| Streaming service | 5 |
| Resolution | 5 |
| Provider rating | 0–3 |
| Popularity/downloads | 0–2 |

Non-hash totals are capped at 100 and require `minimum_release_score` (35 by default). Exact episode coordinates, a parsed release range containing the episode, or an explicit containing pack earn episode evidence. The LAPSE bypass threshold is separate: `sync.bypass_score` defaults to 75. Reaching it is necessary but not sufficient; with the safe defaults, the candidate also needs an external-ID or title/year anchor, release-group evidence, matching season/episode evidence for TV, and an explicit edition match whenever Radarr identifies the target movie's edition. An edition-unknown candidate remains eligible but receives no edition points and must use LAPSE. Rating and popularity never substitute for these anchors.

Provider descriptors listing alternatives separated by a slash with whitespace on both sides (including tabs and Unicode separator spaces) are parsed as separate release names for scoring and LAPSE evidence checks. Alternative order therefore cannot hide a matching source, group, resolution, or edition. Each signal is awarded at most once. Compact slashes remain part of the descriptor. Source comparison also normalizes target aliases such as `webdl` and `WEB-DL`; Blu-ray remux remains distinct from Blu-ray encodes. Existing candidate identity conflicts still reject, and a known target edition still requires an explicit matching edition to bypass LAPSE.

Sonarr/Radarr hydration derives streaming-service evidence from explicit web-release scene-name metadata after a year or season/episode marker. Service names in movie/show titles do not supply this target evidence, and an absent identity boundary remains unknown. Existing media receives the enrichment when normally hydrated; startup does not backfill or contact Arr. Normalized search caches use a new version so pre-repair inferred identity fields are not reused; stored installation provenance and search schedules are not reset.

Edition comparison normalizes punctuation and recognizes Director's Cut, Extended, Remastered, Unrated, Theatrical, Final Cut, Special Edition, Ultimate Cut, Redux, and Anniversary Edition labels. Edition markers are read from the release descriptor after a movie year when present, so a title containing edition-like words is not mistaken for an edition. An explicit matching edition contributes 10 points; an explicit mismatch is rejected.

Release score is primary and lower score tiers cannot outrank a solid higher tier. Within one equal-score tier, LAPSE confidence is followed by configured provider priority, provider rating, provider ID, then result ID. Popularity already contributes up to two points to the primary score. A managed subtitle upgrades only for an exact hash or a score improvement of at least 10, and non-exact upgrades run LAPSE by default.

## Rate limits and cooldowns

Each provider instance has its own token bucket and active-request semaphore. Accounts sharing an origin also use the configured `provider_http.shared_origin_max_concurrent` gate, while quota/auth state remains isolated per account. Both concurrency permits remain held until the response body reaches EOF or is closed early; transport/no-body failures release them immediately. Provider adapters must therefore consume or close every returned body. Availability is checked again after waiting for permits, so a queued request respects cooldowns or disabled authentication established while it waited.

`RateLimit`, `RateLimit-Policy`, `X-RateLimit-*`, numeric/date `Retry-After`, and provider JSON reset values are persisted. `Retry-After` is honored only on non-success responses; some authentication endpoints include it on HTTP 2xx without indicating a throttle. Successful responses still retain standard `RateLimit` and `X-RateLimit-*` quota windows. The most restrictive applicable future reset wins. A worker never sleeps through a remote cooldown: it releases the lease and schedules at or after reset with up to 10% positive jitter. Provider cooldowns do not advance the missing-result or technical-failure counters.

Network failures and HTTP 5xx responses open a persisted circuit for that provider instance and operation (`auth`, `search`, `download`, or OpenSubtitles `download_transfer`). The circuit is shared by every queued media item and survives restart, preventing a provider outage from producing one request per file. Consecutive failures retry after 1, 5, 15, then 60 minutes (capped at 60 minutes); an applicable provider `Retry-After` value takes precedence. Successful responses clear the transient failure streak only after their body is consumed; genuine connection failures while reading a body also open the circuit. Closing early, local writer errors, malformed content, and caller cancellation do not create a remote-outage circuit. Non-success, non-5xx responses retain their status-based recovery handling. It does not erase quota, rate-limit, or disabled-authentication state, and a search circuit does not block downloads.

A definitive login HTTP 401 from OpenSubtitles or Titlovi disables the configured instance instead of repeating the rejected credentials for each media job. Correct the credentials, then explicitly clear provider state with `subsyncd retry --provider NAME`.

A SubDL HTTP 403 on search or download is treated as a rejected API key and disables that configured provider instance persistently. This state intentionally has no automatic expiry: fix or replace the key, then run `subsyncd retry --provider NAME` to clear it. Transient and quota failures never blacklist subtitle candidates.

When the provider supplies no reset, compiled fallbacks are:

| Provider condition | Fallback |
| --- | ---: |
| Titlovi 429 | 5 minutes |
| OpenSubtitles 429 | 1 minute |
| OpenSubtitles download quota | 6 hours |
| SubDL rate limit | 15 minutes |
| SubDL daily downloads | next GMT midnight + 15 minutes |
| SubDL service busy | 1 hour |

These are policy constants, not YAML settings. `subsyncd retry --provider NAME` clears all persisted scopes for one configured provider.

## Search and upgrade schedules

Missing results reach absolute milestones from import or schedule reset: immediately, then at approximately 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, and every 14 days thereafter. The scheduler applies ±10% jitter to the interval between adjacent milestones. Technical failures use 1, 5, 15, then 60 minutes without changing the missing counter.

Every due workflow refreshes local sidecar inventory, but a retry does not necessarily contact a provider. Normalized search results, including an empty result set, remain cached for six hours. With the default milestones, provider traffic for a continuously missing subtitle is therefore normally around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days; the 30-minute and 2-hour checks usually reuse cached results while still detecting a sidecar added by another tool.

Managed nonexact subtitles are reconsidered after 7 days for scores 35–59, 30 days for 60–84, and 90 days for 85–99. Exact-hash installations are terminal until the media fingerprint changes.

The persisted daemon queue has three strict classes: Arr imports and renames (`import`), missing/rejected subtitle searches and reconciliation discoveries (`missing`), and successful nonexact reassessments (`upgrade`). Higher classes are leased first, then older due times. Technical failures and provider throttles retain the job's class. Webhook wakeups accelerate dispatch but do not alter schedules, provider ordering, cache validity, token buckets, or cooldown enforcement.

## Credential-gated contracts

Default tests are local-only. To deliberately exercise current public provider APIs:

```bash
go test ./test/providercontract -tags=provider_contract -v
```

The tests skip each provider unless its normal credential environment variables are present. They execute one bounded broad search for a fixed movie query defined in the contract test and do not download a subtitle.
