# Provider adapters

Language routing, the built-in providers, and the checklist for adding one. Operator setup (accounts, keys, YAML) is in the [provider guide](../../providers.md) and `config.example.yaml`.

- [Language routing](#language-routing)
- [Common adapter rules](#common-adapter-rules)
- [Titlovi](#titlovi)
- [OpenSubtitles.com](#opensubtitlescom)
- [SubDL](#subdl)
- [SubSource](#subsource)
- [Gestdown](#gestdown)
- [Adding a provider](#adding-a-provider)

## Language routing

- Language keys are canonical BCP 47 tags. Each language owns its ordered `providers` (preferred tier) and optional `fallback_providers` (fallback tier); tiers are never flattened into a global order.
- Provider instances are independently credentialed and throttled.
- At startup `provider.ValidateLanguageRoutes` (`internal/provider/registry.go`) rejects a language without providers, an unknown provider ID, and a provider whose `SupportsLanguage` refuses the language. Unsupported regional or script variants fail startup rather than silently broadening the language.
- Hearing-impaired/SDH tracks and candidates are refused unless `allow_hearing_impaired: true` is set at the configuration root.
- Changing routes takes effect at the next start; see [Configured languages and instances](../reconciliation.md#configured-languages-and-instances).

## Common adapter rules

Every adapter is built by a `provider.Factory`, registered by type in `buildProviders` (`internal/app/app.go`), and wrapped by `provider.Observe`, which logs `provider.download_completed` and, at debug, `provider.download_details`. `provider.search_completed` comes from the search coordinator (`internal/provider/coordinator.go`).

- **Capabilities** (`provider.Capabilities`): `MediaKinds` (empty means all), `ExactFileHash`, `SeasonPacks`, `DirectPackMember`, `SingleEpisodeOnly`.
- **Credentials** never become a candidate ID, cache value, provenance record, error or log field. Keys go in headers where the API allows it.
- **Redirects**: API requests that carry credentials never follow redirects. Downloads follow at most five (`MaxDownloadRedirects`) and only to allowed hosts; a refused redirect (`RedirectError`) fails that candidate. Every provider request drops credential headers (`Authorization`, `Cookie`, `Api-Key`, `X-Api-Key`, `X-Auth-Token`) before following a redirect to another scheme, host or port.
- **Bodies**: downloads are size-bounded, and every response body is consumed or closed; the HTTP concurrency permits last until then (see [Limits](limits.md#pacing-and-concurrency)).
- **Overrides** such as `base_url`, `api_base_url`, `download_base_url`, `download_hosts` and `max_download_bytes` exist for tests and private endpoints. Base URLs must use HTTPS outside tests. SubSource and Gestdown cap `max_download_bytes` at their built-in limit; the other adapters accept any positive value.
- **Cache versions**: an adapter that changes how it interprets responses bumps its `SearchCacheVersion` so only its own cached results are refreshed.

| Type | Capabilities | Phases | Search cache version |
| --- | --- | --- | --- |
| `titlovi` | Season packs | Broad | `titlovi-evidence-v2` |
| `opensubtitles` | Exact file hash | Exact, broad | `opensubtitles-evidence-v2` |
| `subdl` | Season packs, direct pack members | Broad | `subdl-no-hi-filter-v3` |
| `subsource` | Season packs | Broad | `subsource-v1` |
| `gestdown` | Episodes only, single episode only | Broad | `gestdown-evidence-v2` |

## Titlovi

Titlovi uses the Kodi API with an API-enabled account. It supports `bs`, `en`, `hr`, `mk`, `sr`, `sr-Cyrl` and `sl`.

- Search is broad-only and paginated (`max_pages`, default 5). Token refresh keeps earlier result pages.
- The requested IMDb ID is a search filter only; it is never copied into candidate identity or awarded external-ID points.
- Returned AKA titles are kept as alternate identity evidence.
- Episode-zero results become season-pack evidence only when the returned season matches; they are never treated as the requested episode.
- Downloaded episode archives are checked independently of the search metadata: an explicit wrong-episode filename rejects that candidate, even in a one-subtitle archive, while one generic filename stays usable for an exact episode result.
- Absolute download links keep their origin; relative links resolve against the configured base. Query-bearing links require a fresh search after cache sanitization.
- Download links carry no token, so downloads neither log in nor refresh it. A download 401 is an ordinary failure of that candidate; a login 401 disables the instance (see [Limits](limits.md#disabled-authentication)).

## OpenSubtitles.com

OpenSubtitles supports exact file-hash search and broad metadata search as separate phases.

### Exact hash

- The hash reads the first and last 64 KiB plus the file size, and supports any 64-bit media size.
- It is computed at the first exact search and stored by algorithm plus path, Arr file ID, size and nanosecond mtime (`media_hashes`), and reused until any of them changes.
- Exact results score 100 and bypass LAPSE. An unusable exact candidate advances to the next exact candidate and then to the broad phase.

### Search

- Every exact and broad search page sends `ai_translated=exclude` ([search API](https://opensubtitles.stoplight.io/docs/opensubtitles-api/a172317bd5ccc-search-for-subtitles)). Results whose `ai_translated` or `machine_translated` attribute is true are also dropped locally, so the exclusion does not depend on server defaults.
- Queries must be canonical: sorted parameters and none equal to its server default (`page` only from page 2; `machine_translated=exclude` never sent). OpenSubtitles answers anything else with a 301, and API requests never follow redirects, so a non-canonical query fails.
- Broad searches with IMDb/TMDB IDs omit title and year filters. Title-only episode searches omit the year, because Sonarr's series premiere year is not an episode air year.
- A search reads at most ten result pages, whatever `total_pages` says.
- For movies, feature IMDb/TMDB IDs are compared as media identity. For episodes, the adapter compares `parent_imdb_id`/`parent_tmdb_id` with Sonarr's series IDs; missing parent IDs are neutral.
- Returned language codes are canonicalized before applying provider aliases.
- The rating is weighted by votes: the normalized average is multiplied by `votes / (votes + 5)`. Unrated results contribute nothing, one 10/10 vote earns 1 point, and a 10/10 rating reaches the full 3 points at about 25 votes. The weighted value is used for the score and for rating tie-breaks.

### Sessions and downloads

- After login, official endpoints use the session API host returned by login (`api.opensubtitles.com` or `vip-api.opensubtitles.com`). Explicit private or test endpoints stay isolated from that routing.
- Login, search and link issuance never follow redirects, because they carry the application key, bearer token or password. The temporary file request carries none and may follow up to five HTTPS redirects without embedded credentials.
- Downloads request SRT conversion. A stable `download_version` marks the payload contract in rejection identity.
- Issued links use a separate `download_transfer` scope, so an exhausted link quota cannot block redeeming a link already issued.
- `user_agent` is required. Published images embed the application API key at build time (`Dockerfile.release`); source builds set `api_key`. See [CONTRIBUTING](../../../CONTRIBUTING.md#dockerfiles).

## SubDL

SubDL is broad-only, with season packs and direct pack members. It supports its published two-letter languages plus `pt-BR` and `zh-Hant`; configuration validation is the definitive check.

### Search

- Searches never send `hi=1`: SubDL treats it as a filter returning only hearing-impaired subtitles, while every result carries its `hi` flag anyway.
- Searches request author comments and keep explicit forced/SDH/HI annotations, including HI filename tokens. Negation applies only to annotation markers and cannot erase a positive structured HI flag.
- Episodes are searched by standard season/episode, an available absolute episode, and the season-pack form; results are merged by stable identity. Later duplicates fill missing identity fields, union release evidence, and keep the strongest rating and popularity before incomplete episode results are filtered.
- Movie searches retry an available TMDB ID once after a genuinely empty IMDb lookup; technical, quota, authentication and cancellation failures do not trigger it.
- Missing response title or year stays unknown rather than being copied from the request, so request values cannot earn identity points or qualify for a LAPSE bypass. Title, year and IMDb/TMDB IDs are taken from the response's `results` only when they list exactly one distinct title.
- Absolute-numbered results keep their absolute episode and range fields. Direct-member selection checks season as well as episode evidence, and release ranges use the conservative archive parser.

### Answers

- HTTP 200 with `status: false` (or `success: false`) is read by its `error` message:
  - not-found messages (`can't find film`, `can't find movie or tv`, `film not found`, `no subtitles`, `no subtitles found`, `no subtitle found`; case, whitespace, curly apostrophes and trailing `.`/`!` ignored) are an empty result;
  - limit messages become a cooldown (see [Limits](limits.md#subdl));
  - messages about the key or authorization (`api key`, `api_key`, `apikey`, `unauthor…`, `forbidden`, `token`) fail the search as `SubDL search was rejected`;
  - anything else is also an empty result, because SubDL rewords its not-found message; it logs `provider.response_unrecognized` with a sanitized `excerpt`. Add new not-found wordings to `isNoResult`.
- API keys returned in download URL queries are discarded during normalization. The configured key is added only to the outbound download request.
- Cached packs must carry the current `evidence_version`; older manifests are skipped before acquisition.

## SubSource

SubSource uses its REST API at `https://api.subsource.net/api/v1` with an `X-API-Key` header; the adapter never sends the key as a query parameter, and its tests assert no request URL carries it. Search is broad-only for movies and episodes. Startup makes no request.

### Search

1. Resolve SubSource's own `movieId`: an IMDb search for the media kind, or without an IMDb ID a text search that accepts only a normalized exact title (and the year for movies). Series resolve to one entry per season; episodes pick the target season and find nothing when it is missing. Conflicting IMDb IDs reject; several exact title matches with different IMDb IDs are ambiguous and return nothing.
2. List `/subtitles` for that ID and language slug, sorted by popularity, 100 rows per page up to `max_pages` (default 5, at most 20).

- A 404 from either step is an empty answer that is not cached, since a gateway fault can produce one.
- Title searches and listings are cached in memory for six hours (256 entries each, oldest evicted). Concurrent searches for one key share one request; a waiter retries rather than inheriting another caller's error. Only complete, successful answers are stored. The cache is lost on restart.
- Language slugs come from SubSource's language list and each mapping is exact; an unknown slug returns an empty success, not an error. `big_5_code`, `chinese_bg_code` and `chinese_bilingual` are not mapped. `tagalog`/`filipino` (`fil`), `pashto`/`pushto` (`ps`) and `sinhala`/`sinhalese` (`si`) share a tag; searches list every slug and merge.

### Candidates

- Title, IMDb/TMDB IDs and season come only from the resolved entry. Episode candidates carry no year, because SubSource's per-season year is not Sonarr's series year.
- Each trimmed `releaseInfo` entry is a release alternative; entries shaped like page slugs are dropped.
- Episode evidence comes from the shared release and range parsers, plus fallbacks for `S1 EP04`, `Season 1 Episode 4` and `Season01`. All alternatives with evidence must agree.
  - an exact target episode is a single-episode candidate;
  - a containing range or double episode is a range pack;
  - season-only or complete-season evidence for the target season is a season pack;
  - rows naming another episode or season, or with no evidence, are dropped. A lone absolute number is not evidence; absolute ranges are, and must cover the file's whole absolute range.
- `productionType: machine` rows are excluded; `forced` or `foreignParts` marks a candidate forced.
- SubSource reports `hearingImpaired: false` for imported subtitles regardless of content, so hearing-impaired classification comes only from the annotation parser over release names and commentary.
- Rating is the share of positive votes weighted by `total / (total + 5)`; popularity uses the shared download-count normalization.

### Downloads

- Result IDs and download references are the decimal subtitle ID. Downloads validate it, rebuild `/subtitles/{id}/download`, refuse redirects, and limit declared and streamed ZIP bodies to 20 MiB.
- The server's `Content-Disposition` name embeds media metadata and is ignored; the archive is named `subsource-<id>.zip`.
- Default pacing is 0.5 requests per second. Rate limits and quotas are described in [Limits](limits.md#subsource).

## Gestdown

Gestdown uses the keyless public API at `https://api.gestdown.info`. It is TV-only and single-episode-only: the coordinator skips it for movies before search logs, cache access or transport, and range targets exclude it (`provider.SupportsMedia`). Unsupported routes do not count as available providers when a tier's outages are classified.

### Search

- Resolve the series by TVDB ID first; an empty lookup falls back to title search. Accept matching external IDs, or a normalized exact title or alternate title when IDs are unavailable; conflicting IDs reject.
- Episode results must return the requested season and number and a consistent series title. Title, series IDs, episode coordinates and language come only from responses; year, IMDb and hash evidence are never invented.
- Only completed results with an explicit hearing-impaired flag are accepted.
- `version` values split at commas into release alternatives; the full `release` field stays intact. Simple group labels (`0tv`, `LOL`) and source-prefixed labels (`DVDRip ORPHEUS`, `BluRayREWARD`) supply group evidence when the filename parser has none, using whole bounded labels only.
- Supported `qualities` values supply resolution alternatives; unknown values and `hd` alone stay neutral.
- Candidate JSON stores optional `release_groups` and `resolutions` separately from filenames. They feed only the group and resolution signals, are unioned across duplicates, and are canonicalized in rejection signatures so reordering does not cause redownloads.
- Language support uses an explicit English-name-to-BCP-47 mapping following the [culture parser](https://github.com/Belphemur/AddictedProxy/blob/main/AddictedProxy.Culture/Service/CultureParser.cs); fields follow the [OpenAPI specification](https://api.gestdown.info/api/v1/swagger.json).

### Downloads

- Downloads rebuild the fixed API path from a validated subtitle UUID, an `sp_{uuid}_ep_{N}` episode-extraction ID, or an `sp_{packUuid}_entry_{entryUuid}` catalog-entry ID. Episode suffixes must agree with the returned episode number.
- Server-extracted single-episode artifacts are classified like SubDL direct members; real multi-member downloads still go through runtime-pack validation and LAPSE. Whole-season pack IDs are excluded.
- Returned download URLs are ignored, redirects are refused, and downloads are limited to 20 MiB.
- HTTP 404 searches are empty. HTTP 423 (refresh in progress) is `gestdown.ErrNotReady`: a technical failure of that search or candidate, never a provider cooldown, so one show being refreshed does not pause Gestdown for every other item.
- Default pacing is one request per second, burst one, concurrency one. 429 handling is in [Limits](limits.md#fallback-resets).

## Adding a provider

1. **Implement `provider.Provider`** (`internal/provider/provider.go`): `ID`, `Capabilities`, `SupportsLanguage`, `Search(ctx, SearchQuery)` and `Download(ctx, candidate, writer)`. Set only the capabilities the API really has.
2. **Write a `Factory`** (`func(id string, node yaml.Node, deps Dependencies) (Provider, error)`) in `internal/provider/<type>/`. Decode the YAML node with known fields only, apply defaults (one request per second, burst one, concurrency one unless the API needs less), validate base URLs (require HTTPS outside tests), call `Gate.Configure`, and build a `provider.Client` with `ProviderType` set.
3. **Register it** by type in `buildProviders` (`internal/app/app.go`).
4. **Map languages** in `SupportsLanguage` with an exact, tested mapping to canonical tags, so `ValidateLanguageRoutes` accepts exactly what the provider can serve.
5. **Handle limits**: persist cooldowns through `Client.PersistCooldown` with the right `CooldownKind`, and add a `FallbackReset` case (`internal/provider/transport.go`) for any limit the provider does not date. Update the [fallback table](limits.md#fallback-resets).
6. **Implement the optional interfaces** that apply: `DownloadAvailability`, `SearchAvailability` and `DownloadRefusalRecorder` (`internal/provider/availability.go`) so preflight, the route gate and refusal handling work; `SearchCacheVersioner` and `CachedCandidateValidator` (`provider.go`) when interpretation can change. `provider.Observe` forwards all five.
7. **Keep credentials out** of URLs, candidate IDs, cache entries, errors and logs. Refuse redirects on requests that carry them, bound every download, and close every body.
8. **Fill evidence only from responses**: never copy request values (title, year, IDs) into candidates.
9. **Test** with local fake servers in the package. Add a live check to `test/providercontract` behind an environment variable (see [CONTRIBUTING](../../../CONTRIBUTING.md#provider-contract-tests)).
10. **Document** it here, in the operator [provider guide](../../providers.md), and in `config.example.yaml`.
