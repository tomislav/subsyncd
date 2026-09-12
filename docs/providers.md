# Subtitle providers

Choose the providers you want to use, add credentials where required, and assign them to your languages. You do not need every supported provider.

## Supported providers

| Provider | What you need | Searches |
| --- | --- | --- |
| OpenSubtitles.com | Account username and password for published images | Exact file matches, movies, and episodes |
| SubDL | API key | Movies, episodes, and season packs |
| Titlovi | API-enabled account username and password | Movies, episodes, and season packs |
| Gestdown | No account or API key | TV episodes |

Provider account quotas still apply. OpenSubtitles results marked as AI or machine translated are excluded automatically.

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

  gestdown-main:
    type: gestdown

  titlovi-main:
    type: titlovi
    username: 'your-titlovi-username'
    password: 'your-titlovi-password'
```

Published containers include the OpenSubtitles application key. Native builds and locally built images need an explicit `api_key` in that provider's configuration as well as the account credentials.

Gestdown is a keyless TV-only provider using its [public API](https://gestdown.readme.io/reference/getting-started-1). Add `gestdown-main` to a language’s `providers` or `fallback_providers` list to enable it. Movie searches skip it without an API request; keep a movie-capable provider in routes used by Radarr. It supports multiple languages, including `en`, `hr`, `pt`, and `pt-BR`; unsupported language tags fail startup validation.

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

Fallback installations retain their real score and normal filename. `explain` shows `fallback=true`, and installation logs include a boolean `fallback` field. They are checked weekly for a preferred replacement, including exact-hash fallback installations. A preferred replacement may have a lower score, but must satisfy the minimum score, identity and timing requirements; nonexact promotions use LAPSE by default. Same-tier upgrades retain the normal score-delta rules, and a preferred installation is never downgraded to fallback. Edited and manually supplied subtitles stay protected.

If an eligible upgrade supplies exactly the same subtitle bytes, subsyncd refreshes its provider, score and timing provenance without rewriting the file. Same-media provenance refreshes do not queue another Silo scan; a changed media fingerprint still does.

After installing fallback during a preferred-provider cooldown, the next check uses the earlier future cooldown reset or weekly check. Failed searches keep ordinary technical-error/cooldown handling; successful searches without a replacement retain the fallback and its weekly check.

Provider tier membership is evaluated from current configuration. The stored flag records the tier at installation time. Moving a provider into fallback does not automatically wake previously terminal exact installations; use a manual search to reconsider one and persist any resulting promotion check. Restart after configuration changes. The preferred list must remain nonempty, and provider names cannot repeat within or across tiers.

OpenSubtitles and SubDL support multiple languages. Titlovi supports Bosnian (`bs`), Croatian (`hr`), English (`en`), Macedonian (`mk`), Serbian (`sr` and `sr-Cyrl`), and Slovenian (`sl`). subsyncd validates each language/provider combination at startup.

Restart after changing languages or provider settings. Adding a language schedules checks for media subsyncd already indexes. Removing a language stops its future searches without deleting installed subtitles. Restart with `docker compose restart subsyncd` after changing credentials in the configuration file.

Hearing-impaired subtitles (SDH/HI) are excluded by default. To include them, add this at the configuration root:

```yaml
allow_hearing_impaired: true
```

## Matching and upgrades

subsyncd first checks whether a suitable subtitle already exists. It then looks for exact file matches where supported, followed by searches using movie or episode details. It skips unusable results and tries other candidates.

Candidates are scored against the media's identity and release details: title, episode, release group, source, edition, and resolution. Ratings contribute only a small part of the score. A known wrong language, episode, or movie identity is rejected even if other details look promising.

When timing needs verification, [LAPSE](https://github.com/Schwponaco-org/lapse) checks and adjusts the subtitle before installation. By default:

- Exact file-hash matches skip LAPSE.
- Strong first-time matches may skip it when the required identity and release evidence agree.
- Uncertain matches, season packs, and upgrades use LAPSE and must pass its strict check.

Season packs must contain clearly identifiable subtitles for the requested episode. Both `S04E13` and `S04.E13` identify episode 13. If two or three distinct versions explicitly identify the same episode, subsyncd downloads the pack once, runs LAPSE once per version, and chooses the solid output with the highest confidence within its release-score tier. Identical content is evaluated once. More than three distinct versions, ambiguous episode identity, and wrong-episode members are skipped; exact-hash results still require a unique member. Explicit season/episode and absolute-number evidence must agree with the target when those coordinates are known; a matching token or provider member reference cannot override conflicting filename evidence. Other valid members in the same pack remain eligible.

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

## Retries and provider limits

Missing subtitles are checked again automatically: initially within minutes and hours, then after days, and eventually about every two weeks. Reusable search results are cached for six hours, so every scheduled check does not necessarily make another provider request. Results whose download links cannot be safely stored, including Titlovi links with query parameters, need a fresh provider search before downloading.

Provider rate limits and outages are handled automatically. subsyncd waits before retrying and remembers those delays across restarts. Restarting does not reset a provider's quota. When a provider's downloads are unavailable, subsyncd skips its candidate searches and further download attempts until the cooldown resets. Usable local pack-cache subtitles and other providers, including fallbacks, remain available. A search-only cooldown skips fresh provider searches while still allowing usable cached search results to supply downloads. Locally suppressed requests are debug-only; the workflow retains its retry outcome.

Candidates rejected for invalid content, ambiguous archive selection, or failed timing checks are skipped without a time limit while their media, candidate evidence, and synchronization policy/version remain unchanged. Searches continue looking for new candidates; subsyncd does not periodically redownload rejected subtitles. This applies to one movie or episode and language, not an entire season pack or provider. Corrected Sonarr episode titles or numbering allow reconsideration. Use `search --retry-rejected` for an explicit override. Invalid subtitle content generated by LAPSE is remembered as `lapse_invalid_output`, using the original candidate identity. Network, process, protocol, and filesystem failures remain retryable and do not mark a subtitle as unsuitable.

Technical workflow failures retry after 1 minute, 5 minutes, 15 minutes,
1 hour, 6 hours, then 24 hours; subsequent failures retry daily. A new media
import or replacement resets this failure schedule and runs immediately.
Provider cooldowns use their separately persisted reset times instead.

Nonexact installed subtitles are normally reconsidered after 7, 30, or 90 days depending on their score. Preferred exact matches need no scheduled upgrade check while the media stays unchanged; fallback matches are checked weekly.

If credentials are rejected, correct them and clear the affected provider's saved state using the [provider recovery instructions](operations.md#troubleshooting). Use [manual searches and explain](operations.md#inspect-or-search-one-file) to investigate a particular file.

The exact point values, schedules, API handling, and cache rules are in the [developer provider reference](development/providers.md).

LAPSE-generated cues with valid intervals but decreasing start times are stably reordered before final validation. Complete cue records retain their timestamps, text, styling and identifiers; equal-start cues retain their order. Negative or reversed intervals still reject. Migration `007_clear_lapse_invalid_output.sql` clears all existing `lapse_invalid_output` rejections once so these candidates can be reconsidered on normal search schedules. It preserves other rejection reasons and does not change schedules or bypass output validation; new invalid-output rejections remain retained.

Movie archives may contain duplicate copies of one subtitle. After forced-subtitle
filtering, identical normalized content counts as one choice; distinct eligible
versions remain ambiguous and are rejected. Archive selection logs include the
archive type, original subtitle count, selection rule, and selected or matching
count, without member filenames.

Migration 009 reconsiders existing movie `pack_selection` rejections once because
older records did not distinguish duplicate content from genuine ambiguity.
Episode rejections and other rejection reasons remain intact. Existing search and
upgrade schedules and installation ownership checks still apply; this does not
force an immediate replacement of an installed subtitle.
