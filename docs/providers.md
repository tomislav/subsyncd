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

OpenSubtitles supports exact file-hash search followed by broad metadata search. The hash is computed on the first exact search and stored by algorithm plus path, Arr file ID, byte size, and nanosecond mtime. It is reused until any part of that fingerprint changes. Exact results score 100, stop all remaining provider searches, and bypass LAPSE.

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
7. Download/analyze no more than the best three eligible non-hash candidates.
8. Install only an exact hash or a LAPSE `solid` result.

Search-result cache entries live for six hours. Season packs default to 24 hours and a total 512 MiB LRU ceiling. Pack downloads are content-addressed and immutable; every cache hit reloads the manifest, verifies checksums, and reruns strict member selection for the current episode.

## Score model

Known identity conflicts reject before points are considered. Conflicts include language, media kind, external IDs, movie year beyond ±1 without an exact external ID, season/episode outside pack scope, and a different known edition/cut. Unknown evidence is neutral.

| Signal | Points |
| --- | ---: |
| Verified exact media-file hash | 100 (terminal) |
| Matching IMDb/TMDB/TVDB ID | 20 |
| Normalized title and year | 15 |
| Release group | 25 |
| Source | 15 |
| Edition/cut | 10 |
| Streaming service | 5 |
| Resolution | 5 |
| Provider rating | 0–3 |
| Popularity/downloads | 0–2 |

Non-hash totals are capped at 100 and require `minimum_release_score` (35 by default). Final ordering is release score, LAPSE analysis confidence, configured provider priority, provider rating, popularity, then stable provider/result identity. A managed subtitle upgrades only for an exact hash or a score improvement of at least 10.

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

Missing results run immediately, then at approximately 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, and every 14 days (±10% jitter). Technical failures use 1, 5, 15, then 60 minutes without changing the missing counter.

Managed nonexact subtitles are reconsidered after 7 days for scores 35–59, 30 days for 60–84, and 90 days for 85–99. Exact-hash installations are terminal until the media fingerprint changes.

## Credential-gated contracts

Default tests are local-only. To deliberately exercise current public provider APIs:

```bash
go test ./test/providercontract -tags=provider_contract -v
```

The tests skip each provider unless its normal credential environment variables are present. They execute one bounded broad search for *The Matrix (1999)* and do not download a subtitle.
