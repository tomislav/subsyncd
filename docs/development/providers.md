# Provider and matching internals

Developer reference for scoring, search phases, caching, and scheduling. For configuration and everyday use, see the [provider guide](../providers.md).

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

Returned AKA titles are retained as alternate identity evidence without copying the query title. Allowed absolute download links retain their original origin during acquisition; relative links resolve against the configured base. Absolute and query-bearing references still require a fresh search after cache sanitization. Provider-specific cache versioning refreshes results normalized before these repairs.

Titlovi uses the supported Kodi API with an API-enabled account. It supports `bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl`, and `sl`. Search is broad-only and paginated (five pages by default). Token refresh preserves earlier result pages. The requested IMDb ID is a search filter only; it is never copied into candidate identity or awarded external-ID points. Episode-zero results become season-pack evidence only when the returned season matches; they are never treated as the requested episode automatically. Downloaded episode archives are checked independently of that search metadata: explicit wrong-episode filenames reject only that candidate, including a one-subtitle archive, while one generic filename remains usable for an exact episode result.

```yaml
titlovi-main:
  type: titlovi
  username: 'your-titlovi-username'
  password: 'your-titlovi-password'
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
  max_pages: 5
```

### OpenSubtitles.com

Broad searches with IMDb/TMDB IDs omit redundant title/year filters. Title-only episode searches also omit year because Sonarr's series premiere year is not an episode air year. Returned language codes are canonicalized before applying established provider aliases. Downloads request SRT conversion and retain normal signed-transfer credential isolation and bounded content validation. A stable `download_version` marks the changed payload contract so prior original-format rejections can be reconsidered once; existing installations and schedules are unchanged. Provider-specific cache versioning refreshes prior search results.

Every exact-hash and broad search page sends `ai_translated=exclude`, using the [official search API parameters](https://opensubtitles.stoplight.io/docs/opensubtitles-api/a172317bd5ccc-search-for-subtitles). Search queries must be canonical: OpenSubtitles answers any other query with a 301 to the canonical URL, and API requests never follow redirects (see below), so a non-canonical query fails the search. Canonical means sorted parameters with no parameter equal to its server default, so `page` is sent only from page 2 and `machine_translated=exclude` (the default) is never sent. Results are still filtered locally: any item whose `ai_translated` or `machine_translated` attribute is true is dropped before normalization, so the exclusion does not rely on server defaults. There is no configuration opt-in. Normalized search-cache version `candidate-v6` prevents reuse of older entries that lack download-reference refresh metadata, as well as results predating translation exclusions and corrected provider identity/episode normalization; existing installed subtitles and downloaded pack caches are unchanged.

OpenSubtitles supports exact file-hash search and broad metadata search as separate phases. The hash is computed on the first exact search and stored by algorithm plus path, Arr file ID, byte size, and nanosecond mtime. It reads only the first and last 64 KiB plus the file size, supports normal 64-bit media sizes without an artificial 9 GB ceiling, and is reused until any part of that fingerprint changes. Exact results score 100 and bypass LAPSE, but an unusable exact candidate advances to the next exact candidate and then broad fallback rather than ending the job. For movies, feature IMDb/TMDB IDs are comparable media identities. For episodes, OpenSubtitles feature IDs identify the episode while Sonarr supplies series IDs, so the adapter compares OpenSubtitles `parent_imdb_id`/`parent_tmdb_id` instead. Missing parent IDs remain neutral; episode feature IDs are never misrepresented as series IDs.

The OpenSubtitles rating is weighted by its vote count: the normalized average is multiplied by `votes / (votes + 5)`, so unrated results contribute nothing, one 10/10 vote earns 1 point, and the full 3 points need about 45 such votes. The weighted value is used for both the score signal and rating tie-breaks.

After authentication, official OpenSubtitles endpoints use the validated session API host returned by login (`api.opensubtitles.com` or `vip-api.opensubtitles.com`). Explicit private/test endpoints remain isolated from official routing. API requests (login, search, and link issuance) never follow redirects, because they carry the application key, bearer token, or account password; a 3xx response fails the request. Temporary subtitle-file requests carry no application key or bearer token and may follow at most five redirects, each to an HTTPS target without embedded credentials. A search reads at most ten result pages regardless of the reported `total_pages`. Issued links use a separate `download_transfer` operation, so an exhausted API download quota cannot prevent redemption of a link already issued; transfer concurrency and failure backoff still apply.

The published subsyncd container includes the application's OpenSubtitles API key. Configure only the account username and password. Source builds and private wrappers may set `api_key` explicitly to override the built-in value; a build without an embedded key requires that override.

```yaml
opensubtitles-main:
  type: opensubtitles
  username: 'your-opensubtitles-username'
  password: 'your-opensubtitles-password'
  user_agent: subsyncd
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
```

### SubDL

Searches request author comments and retain explicit forced/SDH/hearing-impaired annotations, including HI filename tokens. Negation is scoped to annotation markers, including filename separators; it cannot erase a positive structured HI flag. Selected direct members retain parent policy evidence. Movie searches retry an available TMDB ID once after a genuinely empty IMDb lookup; technical, quota, authentication and cancellation failures do not trigger that fallback. Provider-specific cache versioning refreshes older normalized results. Cached packs must carry the current `evidence_version`; older manifests are skipped before acquisition and later compatible packs remain reachable. Fresh normalization publishes a separately keyed immutable manifest, with the existing managed-cache cleanup removing its superseded directory. No migration, schedule reset or blanket cache deletion is needed.

SubDL is broad-only. Searches never send `hi=1`: SubDL treats it as a filter that returns hearing-impaired subtitles only, while every result carries its `hi` flag regardless, which subsyncd uses for hearing-impaired classification. For episodes it searches standard season/episode, an available absolute episode, and the season-pack form, merging normalized results by stable identity. Later duplicates fill missing identity fields, union release evidence, and retain the strongest rating/popularity signals before incomplete episode results are filtered. Missing response title/year remain unknown rather than being copied from the search request, so request defaults cannot earn identity points or qualify for LAPSE bypass. Absolute-numbered results retain explicit absolute episode/range fields for matching. Direct-member selection checks season as well as episode evidence, and release range parsing shares the conservative archive parser so resolution suffixes cannot create packs. It supports its published two-letter languages plus `pt-BR` and `zh-Hant`; configuration validation is the definitive capability check. API keys returned in download URL queries are discarded during normalization. The configured key is reconstructed only on the outbound download request and must never become a candidate ID, cache value, provenance record, or log field.

```yaml
subdl-main:
  type: subdl
  api_key: 'your-subdl-api-key'
  requests_per_second: 1
  burst: 1
  max_concurrent: 1
```

### SubSource

SubSource uses its documented REST API at `https://api.subsource.net/api/v1` with an `X-API-Key` header. The server also accepts the key as an `api_key` query parameter; the adapter never sends it there, and its tests assert that no request URL carries the key. Search is broad-only for movies and episodes. Startup makes no request.

A search first resolves SubSource's own `movieId`: an IMDb search for the media kind, or, without an IMDb ID, a text search accepting only a normalized exact title (and the year for movies). Series resolve to one entry per season; episodes select the target season and find nothing when it is missing. Conflicting IMDb IDs reject, and several exact title matches with different IMDb IDs are ambiguous and return nothing. The adapter then lists `/subtitles` for that ID and language slug, sorted by popularity, 100 rows per page up to `max_pages` (default 5, at most 20). A 404 from either step is an empty answer that is not cached, since a gateway fault can produce one.

Title searches and listings are kept in an in-memory cache for six hours (256 entries each, oldest evicted first) so the episodes of one season share one search and one listing, and a title missing from a successful search is remembered as an empty answer. Concurrent searches for the same key share one in-flight request; if it fails, a waiter retries rather than inheriting another caller's error or cancellation. Only complete, successful answers are stored; errors, cancellations, 404s and failed pages are not. The cache holds provider metadata only and is lost on restart.

Language slugs come from SubSource's own language list; an unknown slug returns an empty success rather than an error, so each mapping is exact and tested to parse as a canonical tag. The encoding and mixed-language entries `big_5_code`, `chinese_bg_code` and `chinese_bilingual` are not mapped. `tagalog`/`filipino` (`fil`), `pashto`/`pushto` (`ps`) and `sinhala`/`sinhalese` (`si`) share a tag; searches list every slug and merge.

Candidates take title, IMDb/TMDB IDs and season only from the resolved entry; episode candidates carry no year because SubSource's per-season year is not Sonarr's series year. Each trimmed `releaseInfo` entry is a release alternative; entries shaped like SubSource page slugs are dropped. Episode evidence comes from the shared release parser and range parser, plus narrow fallbacks for `S1 EP04`, `Season 1 Episode 4` and `Season01` names the shared parser leaves incomplete. All alternatives with evidence must agree. An exact target episode becomes a single-episode candidate, a containing range or double episode a range pack, and season-only or complete-season evidence for the target season a season pack. Rows naming another episode or season, or with no episode or season evidence at all, are dropped. A lone absolute number is not evidence; absolute ranges are, and must cover the file's whole absolute range. Listing metadata can name a different episode than the archive holds, and the shared strict member selector rejects such archives at installation.

`productionType: machine` rows are excluded. `productionType: forced` or `foreignParts` marks a candidate forced. SubSource reports `hearingImpaired: false` for imported subtitles regardless of content, so the structured flag is ignored and hearing-impaired classification comes only from the shared annotation parser over release names and commentary. Rating is the share of positive votes weighted by `total / (total + 5)`; popularity uses the shared download-count normalization. Search-cache version `subsource-v1` and evidence version `subsource-evidence-v1` scope later interpretation changes to this provider.

Candidate result IDs and download references are the decimal subtitle ID only. Downloads validate it, rebuild `/subtitles/{id}/download`, refuse redirects for every request, and limit declared and streamed ZIP bodies to 20 MiB (`max_download_bytes` may only lower it). The server's `Content-Disposition` name embeds media metadata and is ignored; the archive is named `subsource-<id>.zip` and handled by the normal fail-closed extraction pipeline.

SubSource reports only its 60-request minute window, as `X-RateLimit-*` headers with an RFC 3339 reset timestamp, which the shared parser accepts alongside epoch and delta seconds. The hourly (1,800) and daily (7,200) caps are not reported. A 429 that does not report an exhausted minute window, whether its headers show spare capacity or are absent, therefore means one of those caps (local pacing stays well under the minute limit), and the adapter persists a key-wide (`all` scope) quota cooldown for one hour; a 429 reporting an exhausted minute window uses the header reset. `Retry-After`, if present, keeps the transport's normal handling. HTTP 401 disables the provider like a rejected key; 403 stays an ordinary failure because it can come from a gateway challenge. The default pacing is 0.5 requests per second.

```yaml
subsource-main:
  type: subsource
  api_key: 'your-subsource-api-key'
  requests_per_second: 0.5
  burst: 1
  max_concurrent: 1
  max_pages: 5
```

### Gestdown

Gestdown uses the keyless public API at `https://api.gestdown.info`. The adapter is TV-only and broad-only: the coordinator skips it for movies before search logs, cache access or transport. Media-kind capabilities apply to both search phases; other adapters keep their existing movie/episode support. Unsupported routes do not count as available providers when classifying a tier's outages. Add a `type: gestdown` instance to either provider tier. No startup network request, credentials or migration is required.

Resolve the series by TVDB ID first; an empty/not-found lookup falls back to title search. Accept returned matching external IDs or a normalized exact title/alternate-title match when IDs are unavailable; conflicting IDs reject. Episode results must return the requested season/number and a consistent series title. Candidate title, series IDs, episode coordinates and language come only from responses. Year/IMDb/hash evidence is never invented. Only completed results with an explicit hearing-impaired flag are accepted. They retain their version/full release, hearing-impaired flag and normalized download popularity. Explicit quality alternatives supply resolution evidence; the broad HD flag does not invent a resolution or rating.

Downloads reconstruct the fixed API path from a validated subtitle UUID, `sp_{uuid}_ep_{N}` episode-extraction ID, or `sp_{packUuid}_entry_{entryUuid}` catalog-entry ID. Episode-number suffixes must agree with the returned episode number; catalog entries retain the same required response show/season/episode and HI checks. Entry IDs are supported by the current official controller even though the OpenAPI description omits them. Server-extracted single-episode artifacts use the same ordinary-episode classification as SubDL direct members; actual multi-member downloads still trigger runtime-pack validation and LAPSE. Whole-season pack IDs are excluded; the adapter does not enumerate the separate season-pack endpoint. Returned download URLs are ignored, redirects are rejected, and both declared and streamed downloads are limited to 20 MiB (optionally lowered by `max_download_bytes`). Existing content extraction, scoring, HI policy, LAPSE and fallback handling remain authoritative. Safe stable references support normal six-hour search caching. Adapter cache version `gestdown-evidence-v2` refreshes older Gestdown results once, including cached empty searches; other providers retain their cache keys.

Gestdown `version` values split at commas into independent release alternatives; the separate full `release` field remains intact. Simple group labels (`0tv`, `LOL`) and source-prefixed labels (`DVDRip ORPHEUS`, `BluRayREWARD`) supply explicit group evidence when the filename parser has none. Recognition uses whole bounded labels, excludes technical-only descriptors and never searches arbitrary substrings for the requested group. Existing group equivalences still apply. Supported values in `qualities` supply resolution alternatives; unknown values and `hd` alone stay neutral.

Candidate JSON stores optional `release_groups` and `resolutions` separately from filenames. They contribute only to the existing group/resolution signals (25/5 points, once each), never identity/episode/source/edition evidence. Duplicate results union these arrays without mutating the original candidates. Search caching and rejection signatures preserve them, with set ordering canonicalized so reordering/duplicates do not cause redownloads. Changed evidence can permit one reconsideration of an earlier Gestdown rejection; candidates without these optional fields keep their prior signatures. Existing identity, minimum score, HI, pack, upgrade and LAPSE policy gates still apply. No database migration is required.

Requests default to one per second, burst one, concurrency one, with the existing response-body lifetime permits. HTTP 404 searches are empty results. HTTP 423 refresh-in-progress and HTTP 429 responses persist operation cooldowns, honoring rate-limit headers/`Retry-After` or retrying in five minutes without headers. Network/5xx/body failures use the common persistent circuit; errors do not expose response bodies. An optional `base_url` must be an HTTPS origin.

Supported routes use an explicit English-name-to-BCP-47 mapping, including separate Portuguese/Brazilian Portuguese and French/Canadian French. Unsupported regional/script variants fail startup rather than silently broadening the language. The mapping follows the official [culture parser](https://github.com/Belphemur/AddictedProxy/blob/main/AddictedProxy.Culture/Service/CultureParser.cs); API fields and IDs follow the [live OpenAPI specification](https://api.gestdown.info/api/v1/swagger.json), version 5.2.0 at implementation.

Ordinary tests use synthetic local servers. The real search/download/extraction contract is separately gated by both `provider_contract` and `GESTDOWN_LIVE_TEST=1` because this provider has no credential gate:

```bash
GESTDOWN_LIVE_TEST=1 GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/providercontract -tags=provider_contract -run '^TestGestdownLiveSearchDownload$' -v -count=1
```

## Search order and caches

Languages may add `fallback_providers` after their required `providers` list. Provider tier precedes cache/exact/broad preference: run the sequence below within the preferred tier first, then the fallback tier only after acquisition is exhausted. Each tier has its own three-result broad shortlist; persisted candidate evidence covers both attempted tiers. Cache selection filters provider membership before accepting/touching a cached entry. Terminal inventory, repository, cancellation, media/publication and rollback failures do not enter another tier.

For one media/language job:

1. Reuse embedded inventory only when an actual completed probe has matching path, Arr file ID, size, and mtime; empty completed probes are valid, fresh catalog rows are not. Always rescan sibling sidecars. A stale or deleted catalog identity aborts inventory refresh technically before acquisition.
2. Stop for a full matching embedded track or a matching protected/user-owned sidecar. Forced-only and unknown-language tracks do not satisfy a normal request.
3. Try a reusable, checksummed season-pack member.
4. Run the explicit exact-hash phase against hash-capable providers sequentially in configured order. Exact candidates are uncapped and downloaded one at a time; a forced, rejected, malformed, or wrong-member candidate advances locally to the next exact candidate. Stop at the first committed installation.
5. After exact candidates are exhausted, run the explicit broad phase against every assigned provider and merge results in configured order. Duplicate `(provider_id, result_id)` rows collapse before caching, persistence, scoring, or shortlisting; missing identity evidence is filled from later duplicates while conflicting nonempty identity values retain the first stable value.
6. Persist score and rejection evidence for every result, but never a signed download URL or provider token.
7. Remove active deterministic rejections before shortlisting, allowing later-ranked candidates to advance.
8. Cap only the broad, non-hash shortlist at the best three eligible candidates and partition it into equal release-score tiers.
9. Lazily download/prepare only the highest remaining tier. For a first-install candidate scoring at least 75, bypass LAPSE only when identity, release group, and (for TV) episode evidence are all present. With `sync.policy: never`, every eligible candidate bypasses LAPSE and multi-version packs are rejected as ambiguous.
10. For equal-score LAPSE candidates, prepare the complete tier with one strict output-producing invocation per candidate source/version and rank solid results by the resulting confidence, provider priority, rating, provider/result identity. Install the winner's retained output directly; on candidate-local installation failure, try another prepared tie before a lower score tier.

Candidate downloads, extraction, and synchronization scratch use the system temporary directory. Raw archives and discarded candidates are released promptly; viable equal-score candidates remain available through installation fallback. Final subtitle staging remains beside the media, while pack-cache publication stages inside the persistent cache root.

Candidate-local installer validation failures also advance within the tournament, recording the original selected artifact's rejection evidence. Format-changing upgrades advance without quarantining the candidate. Media changes, publication failures, and rollback failures remain terminal technical errors; filesystem and transactional media-fingerprint checks prevent installation against stale media.

Search-result cache entries live for six hours when their acquisition references survive credential sanitization unchanged. Versioned cache records mark any changed result identity, top-level download reference, or direct-member download reference as requiring a fresh provider search before acquisition. A refresh failure returns a technical/provider error; it never falls back to incomplete cached links. Empty results and unchanged opaque references retain normal cache reuse. Titlovi query-bearing download links therefore require refresh even within six hours. Invalid empty, root-only, userinfo-bearing, or network-path references are rejected before download transport. Season packs default to 24 hours and a total 512 MiB LRU ceiling. Pack downloads are content-addressed and immutable; every cache hit reloads the manifest, verifies checksums, and reruns strict member selection for the current episode.

Range targets (multi-episode files) use `match.coversTarget`: an exact hash, a range pack spanning the target, or a parsed release whose standard range (or, when the target has absolute numbers, absolute range such as `66-67`, parsed only from names without a season, span at most 10, never year-like) spans it. Whole-season packs never cover a range: they cannot show a combined member, and downloading them would spend quota for a near-certain `pack_selection` rejection. The episode-conflict check accepts any candidate episode inside the range. Anything else is rejected at score time with "candidate episode range does not cover target". `pack.Select` for ranges accepts only a unique filename range covering the target; single-episode tokens, provider direct members, absolute numbers and titles are not used. A lone non-pack file is accepted when its filename does not contradict the range (`single_range`). Before LAPSE and again at installation, a range subtitle must end at or after 75% of the runtime, else `partial_coverage`. Providers declaring `Capabilities.SingleEpisodeOnly` (Gestdown) are excluded through `provider.SupportsMedia`. `RouteAvailability` also reports `RangesPaused` when only such providers can still download; the route gate then pauses just the multi-episode files of that route (`RouteKey.RangesOnly`, `media.episode_end > media.episode`). A filename range in a pack member or lone file must cover the target, and any other episode token in the name must fall inside it. The release parser reads the `SSxEE.EE` range form. Range fields join the candidate signature only for ranges, leaving single-episode signatures unchanged.

Runtime packs are episode payloads with multiple normalized subtitle members (including forced variants), or a single member carrying a valid multi-episode range, without provider-declared `Pack` metadata. Extraction persists `RuntimePack` in the manifest and preparation carries it privately; provider candidates and their original identities stay unchanged. Every nonexact runtime pack requires LAPSE, including cache hits. Genuine exact hashes retain their bypass, but runtime packs with exact-hash metadata cannot enter the cache, preventing cross-episode transfer of hash authority.

Runtime caching requires a provider series ID or nonempty provider title, plus one positive season established by provider metadata or consistent strictly parsed member tokens. Unknown, special-season zero, malformed, or conflicting season evidence disables publication. Absolute-only or generically named members require a provider season; no requested identity or filename-derived series guess fills the gaps. Cache hits revalidate season safety. A strictly selected cached member supplies the existing 20-point episode evidence and supersedes only the original episode restriction; all other identity checks, score weights, and HI policy remain active. Ordinary provider scoring is unchanged. Cache publication precedes LAPSE, so one rejected member does not discard useful siblings; writes remain best-effort and all stored sources remain immutable. Older provider-declared manifests remain readable without the new field.

Nonexact episode packs may supply up to three distinct-content versions identified by explicit, consistent target-episode tokens. `Select` remains unique; `SelectAlternatives` permits only that bounded ambiguity, with identical checksums collapsed before the limit. Provider-direct, range and title-only ambiguities remain rejected. Each version inherits its provider result's score and enters the same prepared tier; provider shortlist slots remain capped at three. Member rejection signatures include the original source checksum separately from the unchanged provider/result identity. Candidate-wide exhaustion is recorded for the current media/language only after all versions fail deterministically; technical failures prevent aggregation. Cache lookup verifies/filter versions individually before touching the entry. Episode-selection signature v2 allows legacy episode rejections to be reconsidered once; movie signatures are unchanged.

Episode tokens accept an optional dot between season and episode (`S01.E02`) and require complete token boundaries. Malformed episode-shaped names cannot use generic singleton or title-only fallback. Episode ranges are recognized only through complete hyphenated tokens (with the same optional dot): `S01E01-E03`, same-season `S01E01-S01E03`, or same-season `1x01-1x03`. Cross-season, reversed, chained, incomplete, and suffix-contaminated forms fail closed. Release suffixes such as `S01E01.1080p` remain single-episode evidence rather than becoming a range. Forced-only policy applies to every selected archive member, including a plain one-member movie payload.

Candidate rejections are not provider blacklists. They are scoped to one media/language/provider result and do not expire with time. Media fingerprint, stable release metadata, Sonarr season/episode/absolute numbering or episode title, selected member checksum, LAPSE compatibility version, or synchronization-policy changes invalidate the applicable match. Volatile provider rating, popularity, download counts, and temporary download URLs deliberately do not change the rejection identity. Set-like release names and direct-member evidence are sorted and deduplicated before hashing, without mutating provider results. A downloaded archive with no unique member for the requested episode is recorded as `pack_selection`; the workflow continues through its remaining shortlist without opening a provider cooldown.

## Score model

Known identity conflicts reject before points are considered. Conflicts include language, media kind, forced-only results for a full-language request, external IDs, movie year beyond ±1 without an exact external ID, season/episode outside pack scope, and a different known edition/cut. A verified exact file hash overrides only a conflicting textual edition label; all other identity gates remain active. Unknown evidence is normally neutral.

| Signal | Points |
| --- | ---: |
| Verified exact media-file hash | 100 (terminal within its provider tier) |
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

Popularity is `min(log10(downloads + 1) / 4, 1)`, so it saturates at about 10,000 downloads and earns its second point from about 1,000. When ranking the broad shortlist, equal scores are ordered by provider priority, rating, raw download count, provider ID, then result ID; the raw count keeps discriminating after popularity saturates.

Non-hash totals are capped at 100 and require `minimum_release_score` (35 by default). Exact episode coordinates, a parsed release range containing the episode, or an explicit containing pack earn episode evidence. The LAPSE bypass threshold is separate: `sync.bypass_score` defaults to 75. Reaching it is necessary but not sufficient; with the safe defaults, the candidate also needs an external-ID or title/year anchor, release-group evidence, matching season/episode evidence for TV, and an explicit edition match whenever Radarr identifies the target movie's edition. An edition-unknown candidate remains eligible but receives no edition points and must use LAPSE. Rating and popularity never substitute for these anchors.

Provider descriptors listing alternatives separated by a slash with whitespace on both sides (including tabs and Unicode separator spaces) are parsed as separate release names for scoring and LAPSE evidence checks. Alternative order therefore cannot hide a matching source, group, resolution, or edition. Each signal is awarded at most once. Compact slashes remain part of the descriptor. Source comparison also normalizes target aliases such as `webdl` and `WEB-DL`; Blu-ray remux remains distinct from Blu-ray encodes. Existing candidate identity conflicts still reject, and a known target edition still requires an explicit matching edition to bypass LAPSE.

Sonarr/Radarr hydration derives streaming-service evidence from explicit web-release scene-name metadata after a year or season/episode marker. Service names in movie/show titles do not supply this target evidence, and an absent identity boundary remains unknown. Existing media receives the enrichment when normally hydrated; startup does not backfill or contact Arr. Normalized search caches use a new version so pre-repair inferred identity fields are not reused; stored installation provenance and search schedules are not reset.

Edition comparison normalizes punctuation and recognizes Director's Cut, Extended, Remastered, Unrated, Theatrical, Final Cut, Special Edition, Ultimate Cut, Redux, and Anniversary Edition labels. Edition markers are read from the release descriptor after a movie year when present, so a title containing edition-like words is not mistaken for an edition. An explicit matching edition contributes 10 points; an explicit mismatch is rejected.

Release score is primary and lower score tiers cannot outrank a solid higher tier. Within one equal-score tier, LAPSE confidence is followed by configured provider priority, provider rating, provider ID, then result ID. Popularity already contributes up to two points to the primary score. A managed subtitle upgrades within its tier only for an exact hash or a score improvement of at least 10, and non-exact upgrades run LAPSE by default. Fallback-to-preferred promotion bypasses the delta and existing-exact terminal rule, but retains eligibility and LAPSE gates. Preferred-to-fallback downgrades are disallowed.

## Rate limits and cooldowns

Acquisition checks persisted download availability before exact/broad searches and normalized search-cache reads, then rechecks before each candidate download. All compiled adapters expose the read-only check through the observed wrapper. Provider-wide, download, and download-transfer cooldowns block fresh acquisition; disabled authentication also blocks it. Search quotas and timed authentication quotas retain their separate scopes. The latest applicable download reset wins within a provider, while ordinary workflow/fallback scheduling selects the applicable provider retry. Availability reads consume no permits and do not replace the transport's final checks. State-read failures remain terminal repository failures rather than permitting fallback.

Local pack-cache processing remains first and can install while remote downloads are blocked. If a quota/circuit opens during a shortlist, later candidates from that provider are skipped before adapter invocation; other shortlisted providers and fallback tiers remain eligible. Download unavailability is retained in provider-error summaries so successful fallback installs preserve an earlier preferred-provider reset even during a partial outage. Suppressed search/download attempts are debug events rather than repeated warning completions. A separate search preflight runs after usable normalized-cache lookup and before fresh exact/broad adapter calls. Expired, malformed, and refresh-required cache entries cannot bypass it. A cooldown or disabled state discovered at the final transport gate is marked locally suppressed and its provider completion is debug-only, including when state changes after preflight or while waiting for permits. Real remote failures retain their warning/error classification.

Preflight and the final gate select the latest applicable reset across scopes. Every cooldown-state read/write failure remains typed and terminal through adapters and fallback handling. Permanent disabled state survives late successful or failed requests; only explicit provider retry clears it. HTTP 429 always exhausts the selected window even when contradictory headers report remaining quota. SubDL preserves a valid server reset while classifying JSON quota errors. OpenSubtitles quota responses use one future reset for both persistence and the returned workflow error; absent, malformed, or expired resets use the existing six-hour fallback.

Each provider instance has its own token bucket and active-request semaphore. Accounts sharing an origin also use the configured `provider_http.shared_origin_max_concurrent` gate, while quota/auth state remains isolated per account. Both concurrency permits remain held until the response body reaches EOF or is closed early; transport/no-body failures release them immediately. Provider adapters must therefore consume or close every returned body. Availability is checked again after waiting for permits, so a queued request respects cooldowns or disabled authentication established while it waited.

`RateLimit` (draft-08 policy lists and the draft-07 `limit=…, remaining=…, reset=…` dictionary), `RateLimit-Policy`, separate `RateLimit-Limit`/`-Remaining`/`-Reset` headers, `X-RateLimit-*` (a reset value is read as delta seconds when it is too small to be a Unix epoch), numeric/date `Retry-After`, and provider JSON reset values are persisted. The gate clamps every persisted reset, whatever its source (headers, JSON bodies, provider fallbacks), to at most 48 hours ahead, and workflow cooldown and quota errors carry that clamped reset, so an oversized or overflowing value cannot disable a provider indefinitely. On a successful response, an exhausted window that resets within five seconds (per-second gateway windows) is not persisted; the per-instance token bucket paces those requests. `Retry-After` is honored only on non-success responses; some authentication endpoints include it on HTTP 2xx without indicating a throttle. Successful responses still retain standard `RateLimit` and `X-RateLimit-*` quota windows. The most restrictive applicable future reset wins: a response that was already in flight when a longer quota or rate-limit cooldown began cannot shorten or clear it, whether it reports remaining capacity or an earlier reset. A worker never sleeps through a remote cooldown: it releases the lease and schedules at or after reset with fresh random positive jitter of up to 10% of the remaining cooldown. That throttled completion keeps the search's `queue_order_ns`, so the jitter delays eligibility without changing its queue position. Provider cooldowns do not advance the missing-result or technical-failure counters.

Before each daemon lease, the worker's route gate checks every configured language for episodes and movies with `workflow.Service.RouteAvailability`: the same read-only download preflight, over the preferred then fallback providers that support the media kind. If every one reports a cooldown, quota or disabled state, that (language, kind) route is excluded from `LeaseDueSearchesExcept`; its reset is the earliest cooldown/quota reset (none when all are disabled, until `retry --provider`). Search-only scopes never pause a route, a route without a capable provider never pauses, and other routes keep leasing. Pausing is re-evaluated on every dispatch (at least every poll), so a route resumes within one poll after its reset. A state-read failure leases nothing for that dispatch and logs `queue.route_check_failed`. Manual `search` runs the workflow directly and is never paused. While paused, local-only satisfactions (embedded, sidecar, pack cache) on that route also wait.

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

Missing results reach absolute milestones from import or schedule reset: immediately, then at approximately 30 minutes, 2 hours, 8 hours, 24 hours, 3 days, 7 days, 14 days, and every 14 days thereafter. The scheduler draws fresh random ±10% jitter for the interval between adjacent milestones. Technical failures use 1 minute, 5 minutes, 15 minutes, 1 hour, 6 hours, then 24 hours without changing the missing counter; subsequent failures remain on a daily retry. A new import or replacement resets the failure counter and remains immediately due.

Every due workflow refreshes local sidecar inventory, but a retry does not necessarily contact a provider. Reusable normalized search results, including an empty result set, remain cached for six hours; results requiring refreshed download links are searched again. With the default milestones, provider traffic for a continuously missing subtitle with reusable cached results is therefore normally around import, 8 hours, 24 hours, 3 days, 7 days, and 14 days; the 30-minute and 2-hour checks usually reuse cached results while still detecting a sidecar added by another tool.

Managed nonexact subtitles are reconsidered after 7 days for scores 35–59, 30 days for 60–84, and 90 days for 85–100. Preferred exact-hash installations are terminal until the media fingerprint changes. Fallback installations, including exact hashes and score 100, start with a seven-day promotion interval. After a fallback install during a preferred cooldown, use the earlier future preferred reset or weekly check, including partial preferred outages. A clean unsuccessful search that retains a valid managed subtitle completes as satisfied and remains upgrade-priority work, including when no candidates return or every candidate fails eligibility. Unchanged checks advance from their base interval through the larger intervals in 14, 30, 60, and 90 days, capped at 90 days before jitter. All ordinary upgrade intervals have ±10% jitter, including the initial check; a preferred-reset recovery check after a fallback install is neither jittered nor delayed by this backoff. Successful scheduled installations and promotions restart the progression. Technical/throttle results retain existing backoff handling and do not advance the unchanged-check count.

The durable search `attempt` column counts missing retries for import/missing work and unchanged checks for upgrade work. Owner-guarded completion advances it for retained scheduled upgrades, resets it on installation or terminal satisfaction, and restarts it at one when an upgrade discovers a missing sidecar and completes its first unsuccessful acquisition. Media events and coalesced reruns retain their existing reset behavior. Manual searches continue to preserve earlier queued work and existing leases. No migration or proactive rescheduling is required: existing due times stay intact and the policy applies at the next completed search. Migration 006 stores a default-false installation `fallback` flag in the same sidecar/outbox commit; scores and filenames are unchanged. Current configured tier membership controls promotion, while the flag records installation-time provenance. Configuration changes do not proactively reopen terminal exact searches; manual search can reconsider them.

The persisted daemon queue has three strict classes: Arr imports and renames (`import`), missing/rejected subtitle searches and reconciliation discoveries (`missing`), and scheduled checks of retained managed subtitles (`upgrade`). Higher classes are leased first, then instances with a higher `queue_priority`, then older queue positions, then older due times. Technical failures and provider throttles retain the job's class. Webhook wakeups accelerate dispatch but do not alter schedules, provider ordering, cache validity, token buckets, or cooldown enforcement.

## Credential-gated contracts

Default tests are local-only. To deliberately exercise current public provider APIs:

```bash
go test ./test/providercontract -tags=provider_contract -v
```

Credentialed-provider tests skip unless their normal credential environment variables are present. They execute one bounded broad search for a fixed movie query and do not download a subtitle. The separate Gestdown contract requires `GESTDOWN_LIVE_TEST=1` and performs the episode search/download/validation described above.

### Permanent rejection compatibility

The legacy SQLite `expires_at_ns` column is retained to avoid rewriting the schema; new rows store zero, and all historical expiry values are ignored. Existing matching rejections remain effective even after their former deadline. Diagnostic output uses `expires=never`. No background download rechecks are performed. A silently replaced provider file under unchanged evidence stays skipped until an explicit override or another identity change.

Episode rejection signatures now include season, episode, absolute episode and episode title. Pre-change episode signatures cannot establish this evidence and may be evaluated once again after upgrading. Canonicalizing unordered provider evidence can likewise cause a one-time reconsideration of older noncanonical signatures. Subsequent reordering or duplicate evidence does not trigger another download. Cached lookup skips rejected members before touching the pack's access timestamp and continues to other usable cached packs; rejection lookup failures remain technical errors.

Invalid generated LAPSE subtitle size, encoding, syntax, or timestamps are remembered with reason `lapse_invalid_output`, retaining the original selected-source checksum and existing media/tool/policy identity. Only the direct typed content-validation failure qualifies. Missing/nonregular output, read failures, output-path or JSON protocol errors, process failures and mixed cleanup/cancellation errors remain technical and never create this rejection.

Migration `013_repeated_lapse_failures.sql` adds a separate, bounded durable strike record for the only process-failure exception. A strike is eligible only for the same selected artifact checksum and complete candidate/media/tool/policy identity when LAPSE returns a direct or unary `%w`-wrapped `ProcessExitError` with exit code 2 and both stdout and stderr empty. The first observation remains a technical failure. On a second identical observation from a later workflow attempt, SQLite atomically creates the ordinary permanent rejection `lapse_repeated_empty_exit` and removes the strike, so that workflow can advance to another version, candidate, or provider. Within one workflow attempt, the same immutable rejection/artifact identity is prepared and counted at most once across pack cache, exact, broad, preferred, and fallback acquisition. The record does not expose an operator-facing state and is scoped tightly enough that changed media bytes/path/file ID/mtime, candidate evidence or member scope, artifact bytes, LAPSE version, or policy start a fresh first strike.

Composite or multi-cause errors containing a process exit remain technical as a whole, even if another cause is a deterministic verdict, archive selection, subtitle content, no-speech result, cancellation, filesystem, or protocol error. They neither strike nor create an immediate candidate rejection. Diagnostic-bearing exits, other exit codes, timeouts, cancellation, runner/process-protocol/filesystem failures, and unavailable artifact checksums remain technical and retain normal failure backoff. `search --retry-rejected` and every existing candidate-rejection reset atomically clear both the confirmed rejections and unconfirmed strikes for the selected media/language; ordinary searches do not.
