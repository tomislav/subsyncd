# Subtitle providers

Choose the providers you want to use, add credentials where required, and assign them to your languages. You do not need every supported provider.

## Supported providers

| Provider | What you need | Searches |
| --- | --- | --- |
| OpenSubtitles.com | Account username and password for published images | Exact file matches, movies, and episodes |
| SubDL | API key | Movies, episodes, and season packs |
| Titlovi | API-enabled account username and password | Movies, episodes, and season packs |
| SubSource | API key | Movies, episodes, and season packs |
| Gestdown | No account or API key | TV episodes |

Provider account quotas still apply. OpenSubtitles and SubSource results marked as AI or machine translated are excluded automatically.

## Add your providers

The following sections belong under `providers:` in `config/config.yaml`. Keep only the entries you use and replace the quoted placeholders with your credentials directly in this file. Keep your configured copy private.

```yaml
providers:
  opensubtitles-main:
    type: opensubtitles
    username: 'your-opensubtitles-username'
    password: 'your-opensubtitles-password'
    user_agent: subsyncd

  subdl-main:
    type: subdl
    api_key: 'your-subdl-api-key'

  subsource-main:
    type: subsource
    api_key: 'your-subsource-api-key'

  gestdown-main:
    type: gestdown

  titlovi-main:
    type: titlovi
    username: 'your-titlovi-username'
    password: 'your-titlovi-password'
```

Published containers include the OpenSubtitles application key. Native builds and locally built images need an explicit `api_key` in that provider's configuration as well as the account credentials.

Gestdown is a keyless TV-only provider using its [public API](https://gestdown.readme.io/reference/getting-started-1). Add `gestdown-main` to a language’s `providers` or `fallback_providers` list to enable it. Movie searches skip it without an API request; keep a movie-capable provider in routes used by Radarr. It supports multiple languages, including `en`, `hr`, `pt`, and `pt-BR`; unsupported language tags fail startup validation.

SubSource searches by IMDb ID, or by exact title when the media has none, and supports more than 100 languages, including `en`, `hr`, `pt-BR`, and `es-419`; unsupported language tags fail startup validation. Create an API key on your SubSource profile page. A key allows 7,200 requests per day. One search costs about two requests, and episodes of the same season share them for six hours. Because SubSource's hearing-impaired flag is unreliable, subtitles count as hearing-impaired only when their release name or description says so. It works well as the last preferred provider for English, where it often has subtitles for specific releases, or as a fallback for other languages.

The [complete configuration example](../config.example.yaml) includes optional request-rate and concurrency settings. Start with its defaults.

## Choose languages

Use language tags such as `en`, `hr`, or `pt-BR`, and refer to the provider names you configured above:

```yaml
languages:
  en:
    providers: [opensubtitles-main, subdl-main]
  hr:
    providers: [titlovi-main]
```

Order sets preference within a provider tier; release compatibility can let a later provider win. To use OpenSubtitles only after Titlovi has no installable subtitle, configure a separate fallback tier:

```yaml
languages:
  hr:
    providers: [titlovi-main]
    fallback_providers: [opensubtitles-main]
```

The preferred tier is tried first, including cached packs, exact matches and broad results. Only after it is exhausted does subsyncd try the fallback tier, with the same matching and timing checks. Preferred-provider outages also allow fallback. Each tier has its own three-result broad shortlist. A fallback exact match cannot preempt an eligible preferred broad match.

Fallback installations retain their real score and normal filename. `explain` shows `fallback=true`, and installation logs include a boolean `fallback` field. Their first preferred-replacement check is after approximately seven days, including exact-hash fallback installations; repeated unchanged checks gradually slow to approximately 90 days. A preferred replacement may have a lower score, but must satisfy the minimum score, identity and timing requirements; nonexact promotions use LAPSE by default. Same-tier upgrades retain the normal score-delta rules, and a preferred installation is never downgraded to fallback. Edited and manually supplied subtitles stay protected.

If an eligible upgrade supplies exactly the same subtitle bytes, subsyncd refreshes its provider, score and timing provenance without rewriting the file. Same-media provenance refreshes do not queue another Silo scan; a changed media fingerprint still does.

After installing fallback during a preferred-provider cooldown, the next check uses the earlier future cooldown reset or weekly check. Failed searches keep ordinary technical-error/cooldown handling; successful searches without a replacement retain the fallback and its weekly check.

Provider tier membership is evaluated from current configuration. The stored flag records the tier at installation time. Moving a provider into fallback does not automatically wake previously terminal exact installations; use a manual search to reconsider one and persist any resulting promotion check. Restart after configuration changes. The preferred list must remain nonempty, and provider names cannot repeat within or across tiers.

OpenSubtitles and SubDL support multiple languages. Titlovi supports Bosnian (`bs`), Croatian (`hr`), English (`en`), Macedonian (`mk`), Serbian (`sr` and `sr-Cyrl`), and Slovenian (`sl`). subsyncd validates each language/provider combination at startup.

Many subtitles carry advert cues: Titlovi adds `www.titlovi.com` and `Preuzeto sa www.titlovi.com`, OpenSubtitles inserts lines such as "Advertise your product or brand here" or "become VIP member" naming its site, and re-uploads carry other sites' addresses (`www.addic7ed.com`, `@SUBSCENE`). subsyncd drops such cues from SRT subtitles when it installs them and renumbers the rest. Addic7ed subtitles, which Gestdown serves, carry a signature cue ("Synced and corrected by …" over "www.addic7ed.com"), and that goes too. A cue is removed only when it is nothing but a site address or "downloaded from" line, a credit paired with a site address, or names OpenSubtitles; a credit on its own and dialogue that mentions a website are kept. Subtitles installed before this change keep the cue until they are replaced.

Restart after changing languages or provider settings. Adding a language schedules checks for media subsyncd already indexes. Removing a language stops its future searches without deleting installed subtitles. Restart with `docker compose restart subsyncd` after changing credentials in the configuration file.

Hearing-impaired subtitles (SDH/HI) are excluded by default. To include them, add this at the configuration root:

```yaml
allow_hearing_impaired: true
```

## Matching and upgrades

subsyncd first checks whether a suitable subtitle already exists. It then looks for exact file matches where supported, followed by searches using movie or episode details. It skips unusable results and tries other candidates.

Candidates are scored against the media's identity and release details: title, episode, release group, source, edition, and resolution. Subtitles for one disc of an old multi-CD rip (CD1, CD2, 2cd) are skipped, since they cover only part of the film. A Croatian or Bosnian subtitle whose text is clearly Serbian (ekavian: gde, uvek, vreme, lepo… with hardly any ijekavian gdje, uvijek, vrijeme, lijepo) is rejected as `language_mismatch` before its timing is checked, and so is one that is not South Slavic at all (such as a Portuguese subtitle tagged Croatian); uploads tagged Croatian on OpenSubtitles and SubDL are sometimes neither. Ratings contribute only a small part of the score. A known wrong language, episode, or movie identity is rejected even if other details look promising.

When timing needs verification, [LAPSE](https://github.com/Schwponaco-org/lapse) checks and adjusts the subtitle before installation. By default:

- Exact file-hash matches skip LAPSE.
- Strong first-time matches may skip it when the required identity and release evidence agree.
- Uncertain matches, season packs, and upgrades use LAPSE and must pass its strict check.

Season packs must contain clearly identifiable subtitles for the requested episode. Both `S04E13` and `S04.E13` identify episode 13. If two or three distinct versions explicitly identify the same episode, subsyncd downloads the pack once, runs LAPSE once per version, and chooses the solid output with the highest confidence within its release-score tier. Identical content is evaluated once. More than three distinct versions, ambiguous episode identity, and wrong-episode members are skipped; exact-hash results still require a unique member. Explicit season/episode and absolute-number evidence must agree with the target when those coordinates are known; a matching token or provider member reference cannot override conflicting filename evidence. Other valid members in the same pack remain eligible. A movie archive containing identical copies of one subtitle counts as one choice; distinct versions in a movie archive are ambiguous and skipped.

For a file containing several consecutive episodes, providers are searched by its first episode, and a result is used only when it covers every episode in the file: a release name range such as `S06E01-E02`, `06x01.02` or (for absolute numbering) `66-67`, a range pack spanning it, or an exact file-hash match. Results for a single episode, and whole-season packs, are skipped before downloading. A downloaded subtitle must also run to at least 75% of the file's runtime; a shorter one is rejected as `partial_coverage`. Gestdown, which only has per-episode subtitles, is not searched for these files, and while only Gestdown can download for a language, these files wait (`queue.route_paused` with `episode_ranges_only`) instead of running searches that cannot succeed. Separate subtitles for each episode are never joined.

An episode result also receives pack handling when its downloaded payload contains multiple subtitle members or a valid multi-episode range, even if the provider labels it as one episode. Nonexact runtime packs always use LAPSE. When provider series identity and a consistent season are available, the original subtitles are cached for later episodes; each episode still undergoes strict selection and its own timing check. Missing or conflicting cache identity disables caching without relaxing selection or timing checks. A rejected version does not reject other versions or episodes. An entire provider result is remembered as exhausted only when every selected version failed deterministically. The broad shortlist still contains at most three provider results; each may supply up to three episode versions.

subsyncd upgrades only subtitles it installed and that remain unedited. Within a tier, a replacement must be an exact match or improve the score by at least 10 points. Promotion from fallback to preferred providers uses the separate rules above. Manually added and edited subtitles are protected.

## Adjusting matching settings

The defaults are intended for normal use:

```yaml
minimum_release_score: 35
sync:
  policy: confidence
  bypass_score: 75
```

`minimum_release_score` is the minimum score for considering a nonexact candidate. Raising it narrows the choices; lowering it admits weaker matches.

`bypass_score` is a separate threshold for skipping LAPSE. A high score alone is not enough: the required identity and release evidence must also agree.

Set `sync.policy: always` to run LAPSE for every nonexact candidate. Exact file matches still skip it. LAPSE can read much of the media file on its first run, so this may increase processing time and network-storage traffic.

LAPSE decodes the media file's whole audio track on its first run for that file, and saves the result so later checks of the same file take about a second. Each run is limited to `sync.timeout` (30 minutes by default), extended for large files to the time needed to read the whole file at `sync.min_read_rate` MiB per second (default 4; `0` turns the extension off), up to `sync.max_timeout` (default 4 hours). On network storage such as a FUSE mount, set `min_read_rate` below the read speed you actually get. A run that times out saves nothing, so for the rest of that search subsyncd doesn't run LAPSE again for the file: other candidates that need LAPSE are skipped, while exact file-hash matches and candidates that skip LAPSE are still tried, including from fallback providers. If nothing installs, the search fails and is retried with the normal failure delays.

Set `sync.policy: never` to download subtitles without synchronization. Every candidate that reaches `minimum_release_score` is installed with the timing it was downloaded with, including season packs and upgrades; `bypass_score` and the other `sync` switches are ignored. Timing is not verified, so the release score is the only protection against an out-of-sync subtitle: raise `minimum_release_score` (60 is a reasonable start; 75 effectively requires a release-group match). A pack containing several versions of the requested episode is skipped, because without LAPSE there is no safe way to choose between them. Switching policy lets candidates previously rejected for failed timing be reconsidered.

## Retries and provider limits

Missing subtitles are checked again automatically: initially within minutes and hours, then after days, and eventually about every two weeks. Reusable search results are cached for six hours, so every scheduled check does not necessarily make another provider request. Results whose download links cannot be safely stored, including Titlovi links with query parameters, need a fresh provider search before downloading.

Provider rate limits and outages are handled automatically. subsyncd waits before retrying and remembers those delays across restarts. Restarting does not reset a provider's quota. When a provider's downloads are unavailable, subsyncd skips its candidate searches and further download attempts until the cooldown resets. Usable local pack-cache subtitles and other providers, including fallbacks, remain available. A search-only cooldown skips fresh provider searches while still allowing usable cached search results to supply downloads. Locally suppressed requests are debug-only; the workflow retains its retry outcome.

When every provider for a language can no longer download (an exhausted download quota, a cooldown, or disabled credentials), subsyncd stops starting searches for that language and media kind until the earliest reset; other languages and media kinds continue. While a language is paused, waiting files are not checked at all, even when an embedded subtitle, a sidecar, or a cached season pack could satisfy them. Searches resume within about 30 seconds of the reset, in the same order as before. A search-only cooldown does not pause a language. A search that is throttled while running keeps its place in the queue: the retry delay only decides when it may run again. The log records `queue.route_paused` and `queue.route_resumed` when this changes.

Candidates rejected for invalid content, ambiguous archive selection, or failed timing checks are skipped without a time limit while their media, candidate evidence, and synchronization policy/version remain unchanged. Searches continue looking for new candidates; subsyncd does not periodically redownload rejected subtitles. This applies to one movie or episode and language, not an entire season pack or provider. Corrected Sonarr episode titles or numbering allow reconsideration. Use `search --retry-rejected` for an explicit override. Invalid subtitle content generated by LAPSE is remembered as `lapse_invalid_output`, using the original candidate identity. Network, process, protocol, and filesystem failures remain retryable and do not mark a subtitle as unsuitable.

Technical workflow failures retry after 1 minute, 5 minutes, 15 minutes, 1 hour, 6 hours, then 24 hours; subsequent failures retry daily. A new media import or replacement resets this failure schedule and runs immediately. Provider cooldowns use their separately persisted reset times instead.

If a first search could not try every provider because one was in a cooldown, the work stays unfinished instead of counting as "nothing found". It is retried after that provider's cooldown resets (with a little random delay), and only the providers that had not yet finished cleanly are asked again; preferred and fallback tiers still run in order. This progress survives restarts. Changing the language's provider route, replacing the media file, or running a manual `search` discards it, so the complete current route runs.

Nonexact installed subtitles receive their first upgrade check after 7, 30, or 90 days depending on their score; fallback matches start at 7 days. When a check retains the installed subtitle, the interval advances to the next larger value in 14, 30, 60, and 90 days, then stays at 90 days. Ordinary intervals vary by ±10% to spread the work. An automatic replacement restarts this progression. Preferred exact matches need no scheduled upgrade check while the media stays unchanged.

Finding no better candidate keeps the existing subtitle on the upgrade schedule. It does not start the faster missing-subtitle retries. If the managed sidecar has actually disappeared, normal missing-subtitle retries resume. Provider errors and cooldowns retain their technical retry handling; the prompt preferred-provider recovery check after a fallback install is preserved.

If credentials are rejected, correct them and clear the affected provider's saved state using the [provider recovery instructions](operations.md#troubleshooting). Use [manual searches and explain](operations.md#inspect-or-search-one-file) to investigate a particular file.

The exact point values, schedules, API handling, and cache rules are in the [developer provider reference](development/providers.md).
