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

Hearing-impaired/SDH tracks and candidates are disallowed by default. Set `allow_hearing_impaired: true` at the configuration root to opt in; when enabled, an existing matching SDH track may satisfy the language and an HI provider result remains eligible.

## Built-in providers

### Titlovi

Titlovi uses the supported Kodi API with an API-enabled account. It supports `bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl`, and `sl`. Search is broad-only and paginated (five pages by default). Episode-zero results become season-pack evidence only when the returned season matches; they are never treated as the requested episode automatically.

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

OpenSubtitles supports exact file-hash search followed by broad metadata search. The hash is computed on the first exact search and stored by algorithm plus path, Arr file ID, byte size, and nanosecond mtime. It is reused until any part of that fingerprint changes. Exact results score 100, stop all remaining provider searches, and bypass LAPSE. For movies, feature IMDb/TMDB IDs are comparable media identities. For episodes, OpenSubtitles feature IDs identify the episode while Sonarr supplies series IDs, so the adapter compares OpenSubtitles `parent_imdb_id`/`parent_tmdb_id` instead. Missing parent IDs remain neutral; episode feature IDs are never misrepresented as series IDs.

```yaml
opensubtitles-main:
  type: opensubtitles
  api_key: ${OPENSUBTITLES_API_KEY}
  username: ${OPENSUBTITLES_USERNAME}
  password: ${OPENSUBTITLES_PASSWORD}
  user_agent: subsyncd
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
```

### SubDL

SubDL is broad-only. For episodes it searches standard season/episode, an available absolute episode, and the season-pack form, merging results by stable identity. It supports its published two-letter languages plus `pt-BR` and `zh-Hant`; configuration validation is the definitive capability check.

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

1. Reuse embedded inventory only when path, Arr file ID, size, and mtime match; always rescan sibling sidecars.
2. Stop for a full matching embedded track or a matching protected/user-owned sidecar. Forced-only and unknown-language tracks do not satisfy a normal request.
3. Try a reusable, checksummed season-pack member.
4. Ask hash-capable providers sequentially in configured order. The first verified exact match is terminal.
5. If no exact match exists, query every assigned provider broadly and merge results in configured order.
6. Persist score and rejection evidence for every result, but never a signed download URL or provider token.
7. Remove active deterministic rejections before shortlisting, allowing later-ranked candidates to advance.
8. Download/prepare no more than the best three eligible non-hash candidates.
9. For a first-install candidate scoring at least 75, bypass LAPSE only when identity, release group, and (for TV) episode evidence are all present.
10. Otherwise require a LAPSE `solid` result; packs and upgrades always take this path by default.

Search-result cache entries live for six hours. Season packs default to 24 hours and a total 512 MiB LRU ceiling. Pack downloads are content-addressed and immutable; every cache hit reloads the manifest, verifies checksums, and reruns strict member selection for the current episode.

Candidate rejections are not provider blacklists. They are scoped to one media/language/provider result and expire after 30 days. Media fingerprint, stable release metadata, selected member checksum, LAPSE compatibility version, or synchronization-policy changes invalidate the applicable match. Volatile provider rating, popularity, download counts, and temporary download URLs deliberately do not change the rejection identity.

## Score model

Known identity conflicts reject before points are considered. Conflicts include language, media kind, forced-only results for a full-language request, external IDs, movie year beyond ±1 without an exact external ID, season/episode outside pack scope, and a different known edition/cut. Unknown evidence is neutral.

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

Non-hash totals are capped at 100 and require `minimum_release_score` (35 by default). Exact episode coordinates, a parsed release range containing the episode, or an explicit containing pack earn episode evidence. The LAPSE bypass threshold is separate: `sync.bypass_score` defaults to 75. Reaching it is necessary but not sufficient; with the safe defaults, the candidate also needs an external-ID or title/year anchor, release-group evidence, and matching season/episode evidence for TV. Rating and popularity never substitute for these anchors.

Final ordering is release score, synchronization confidence/provenance, configured provider priority, provider rating, popularity, then stable provider/result identity. A managed subtitle upgrades only for an exact hash or a score improvement of at least 10, and non-exact upgrades run LAPSE by default.

## Rate limits and cooldowns

Each provider instance has its own token bucket and active-request semaphore. Accounts sharing an origin also use the configured `provider_http.shared_origin_max_concurrent` gate, while quota/auth state remains isolated per account.

`RateLimit`, `RateLimit-Policy`, `X-RateLimit-*`, numeric/date `Retry-After`, and provider JSON reset values are persisted. The most restrictive future reset wins. A worker never sleeps through a remote cooldown: it releases the lease and schedules at or after reset with up to 10% positive jitter. Provider cooldowns do not advance the missing-result or technical-failure counters.

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

The tests skip each provider unless its normal credential environment variables are present. They execute one bounded broad search for *The Matrix (1999)* and do not download a subtitle.
