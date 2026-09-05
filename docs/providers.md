# Subtitle providers

Choose the providers you want to use, add their credentials, and assign them to your languages. You do not need every supported provider.

## Supported providers

| Provider | What you need | Searches |
| --- | --- | --- |
| OpenSubtitles.com | Account username and password for published images | Exact file matches, movies, and episodes |
| SubDL | API key | Movies, episodes, and season packs |
| Titlovi | API-enabled account username and password | Movies, episodes, and season packs |

Provider account quotas still apply. OpenSubtitles results marked as AI or machine translated are excluded automatically.

## Add your providers

The following sections belong under `providers:` in `config/config.yaml`. Keep only the entries you use, and put their credential values in the Compose `.env` file. The supplied Compose example already forwards these variables into the container.

```yaml
providers:
  opensubtitles-main:
    type: opensubtitles
    username: ${OPENSUBTITLES_USERNAME}
    password: ${OPENSUBTITLES_PASSWORD}
    user_agent: subsyncd

  subdl-main:
    type: subdl
    api_key: ${SUBDL_API_KEY}

  titlovi-main:
    type: titlovi
    username: ${TITLOVI_USERNAME}
    password: ${TITLOVI_PASSWORD}
```

Published containers include the OpenSubtitles application key. Native builds and locally built images need an explicit `api_key` in that provider's configuration as well as the account credentials.

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

Order sets provider preference; it does not mean subsyncd always downloads the first provider's result. Release compatibility matters, and a better match can come from another provider.

OpenSubtitles and SubDL support multiple languages. Titlovi supports Bosnian (`bs`), Croatian (`hr`), English (`en`), Macedonian (`mk`), Serbian (`sr` and `sr-Cyrl`), and Slovenian (`sl`). subsyncd validates each language/provider combination at startup.

Restart after changing languages or provider settings. Adding a language schedules checks for media subsyncd already indexes. Removing a language stops its future searches without deleting installed subtitles. Recreate the container with `docker compose up -d` if you changed credentials in `.env`.

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

Season packs must contain a clearly identifiable subtitle for the requested episode. Ambiguous or wrong-episode archives are skipped.

subsyncd upgrades only subtitles it installed and that remain unedited. A replacement must be an exact match or improve the score by at least 10 points. Manually added and edited subtitles are protected.

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

Missing subtitles are checked again automatically: initially within minutes and hours, then after days, and eventually about every two weeks. Recent search results are cached for six hours, so every scheduled check does not necessarily make another provider request.

Provider rate limits and outages are handled automatically. subsyncd waits before retrying and remembers those delays across restarts. Restarting does not reset a provider's quota.

Candidates rejected for invalid content, ambiguous episode selection, or failed timing checks are normally skipped for 30 days. That applies to the particular candidate and media, not the entire provider. Network and service failures do not mark a subtitle as unsuitable.

Nonexact installed subtitles are normally reconsidered after 7, 30, or 90 days depending on their score. Exact matches need no scheduled upgrade check while the media stays unchanged.

If credentials are rejected, correct them and clear the affected provider's saved state using the [provider recovery instructions](operations.md#troubleshooting). Use [manual searches and explain](operations.md#inspect-or-search-one-file) to investigate a particular file.

The exact point values, schedules, API handling, and cache rules are in the [developer provider reference](development/providers.md).
