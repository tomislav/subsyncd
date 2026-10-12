# Subtitle providers

Choose the providers you want, add their credentials, and assign them to your languages. You don't need every provider. Every key is listed in [Configuration](configuration.md#providers).

- [Terms used here](#terms-used-here)
- [Supported providers](#supported-providers)
- [Add your providers](#add-your-providers)
- [Choose languages](#choose-languages)
- [Strip site adverts](#strip-site-adverts)
- [Matching](#matching)
- [Synchronization](#synchronization)
- [When subsyncd searches](#when-subsyncd-searches)
- [Provider limits](#provider-limits)
- [Rejected subtitles](#rejected-subtitles)

## Terms used here

| Term | Meaning |
| --- | --- |
| Tier | A language's preferred providers are tried first; its fallback providers only when those have nothing installable. |
| Exact match | A result found by the media file's hash, made for exactly this file. |
| Score | 0–100 points for how well a candidate's title, episode and release details match the file. |
| LAPSE | The bundled [timing checker](https://github.com/Schwponaco-org/lapse): it compares a subtitle with the audio and corrects or refuses it. |
| Cooldown | A pause on a provider after a rate limit, quota or outage. |
| Rejection | A candidate that failed a check, or that you rejected; it is not used again for that file and language. |

## Supported providers

| Provider | What you need | Searches |
| --- | --- | --- |
| OpenSubtitles.com | Account username and password, and a `user_agent`. Published images include the application key. | Exact file matches, movies and episodes, many languages |
| SubDL | API key from your SubDL account | Movies, episodes and season packs, many languages |
| Titlovi | Username and password of an API-enabled account | Movies, episodes and season packs in Bosnian, Croatian, English, Macedonian, Serbian (`sr`, `sr-Cyrl`) and Slovenian |
| SubSource | API key, created on your SubSource profile page | Movies, episodes and season packs, more than 100 languages |
| Gestdown | Nothing | TV episodes only, many languages |

- Account quotas apply. A SubSource key allows 7,200 requests a day; a search costs about two, shared by a season's episodes for six hours.
- OpenSubtitles and SubSource results marked as machine translated are skipped.
- Gestdown serves Addic7ed subtitles through its [public API](https://gestdown.readme.io/reference/getting-started-1). Movie searches skip it without a request, so keep a movie-capable provider in every language Radarr uses.

## Add your providers

These entries go under `providers:` in `config/config.yaml`. Keep the ones you use and replace the placeholders. To keep credentials out of the file, use [`'${NAME}'`](configuration.md#credentials-and-environment-variables).

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

Native builds and locally built images also need the OpenSubtitles application key as `api_key`. The request-rate settings in the [example configuration](../config.example.yaml) have safe defaults.

## Choose languages

Use language tags such as `en`, `hr` or `pt-BR`, and the provider names you configured:

```yaml
languages:
  en:
    providers: [opensubtitles-main, subdl-main]
  hr:
    providers: [titlovi-main]
    fallback_providers: [opensubtitles-main]
```

- **Order** sets preference within a tier, but a better-matching release from a later provider can still win.
- **The preferred tier comes first**, including its cached packs, exact matches and title searches. The fallback tier is tried only after it is exhausted or unavailable, with the same checks. A fallback exact match never beats an acceptable preferred match.
- **Fallback subtitles** keep their real score. `explain` shows `fallback=true`. subsyncd keeps checking for a preferred subtitle to replace it ([schedule](#when-subsyncd-searches)); the replacement may score lower but must pass every check.
- **Hearing-impaired** (SDH/HI) subtitles are skipped unless you set `allow_hearing_impaired: true` at the top level.
- **Changing languages** needs a restart; see [Changing the configuration](running.md#changing-the-configuration).

## Strip site adverts

Many subtitles carry advert cues such as `www.titlovi.com`, "Preuzeto sa www.titlovi.com", OpenSubtitles' "Advertise your product or brand here", or an Addic7ed signature. With `install.strip_watermarks: true` (off by default), subsyncd drops these cues from SRT subtitles when it installs them, and renumbers the rest.

A cue that also holds other text, such as a translator credit, keeps that text. Dialogue that mentions a website is kept, and subtitles already installed are not changed.

## Matching

subsyncd first checks whether an embedded track or sidecar already covers the language. Otherwise it looks for exact matches where the provider supports them, then searches by movie or episode details.

- **Identity first.** A known wrong language, episode or movie is rejected whatever its score. Croatian or Bosnian subtitles whose text is really Serbian or another language are rejected as `language_mismatch`.
- **Then the score.** A non-exact candidate needs at least `minimum_release_score` (35). At most three title-search results per tier are downloaded and checked.
- **Downloads are cleaned up.** Cues are sorted and repaired, empty ones dropped, and MicroDVD `.sub` files with a frame rate converted to SRT. Files that cannot be repaired safely are rejected.
- **Season packs** must clearly identify the requested episode. If two or three versions do, each is checked with LAPSE and the best one wins.
- **Multi-episode files** need a subtitle covering every episode, at least 75% of the file's runtime.
- **Upgrades.** subsyncd replaces only subtitles it installed and that are unedited. Within a tier, a replacement must be an exact match or score at least 10 points more. Subtitles you added or edited are never touched.

The scoring model, pack handling and cleanup rules are in the [developer matching reference](development/providers/matching.md).

## Synchronization

`sync.policy` sets when LAPSE runs:

| Policy | LAPSE runs for |
| --- | --- |
| `confidence` (default) | Every non-exact candidate, except strong first-time matches that pass the checks below. Exact matches skip it. |
| `always` | Every non-exact candidate. |
| `never` | Nothing. Subtitles install with their downloaded timing. |

Under `confidence`, a match skips LAPSE only when it scores at least `sync.bypass_score` (75) and every enabled requirement holds: `sync.require_identity_anchor`, `sync.require_episode_evidence` and `sync.require_release_group`. With `sync.lapse_for_packs` and `sync.lapse_for_upgrades`, season packs and upgrades always use LAPSE. All five default to `true`.

With `never`, the score is the only protection against an out-of-sync subtitle: raise `minimum_release_score` (60 is a reasonable start; 75 effectively requires a release-group match). Packs with several versions of the episode are skipped. Switching policy lets candidates that failed timing checks be reconsidered.

How long LAPSE may run is covered in [Resources](running.md#resources-and-lapse-run-time); the [developer LAPSE reference](development/providers/lapse.md) has the details.

## When subsyncd searches

The [developer scheduling reference](development/providers/scheduling.md) has the exact rules.

**Missing subtitles** are searched at once, then about 6 hours, 12 hours, 24 hours, 3 days, 7 days and 14 days after the first search, then every 14 days, indefinitely. Each wait may be up to 20% longer, never shorter, so no search reuses the previous one's six-hour result cache.

- Media released more than `aged_release_days` ago (30) skips the early steps: the next search is a week later, then every 14 days. The release date is the episode's air date, or a movie's digital release, else its physical release, else its cinema release plus 90 days.
- Media without a known release date gets the full schedule. `aged_release_days: 0` gives every file the full schedule.

**Upgrade checks** for a non-exact subtitle start 7, 30 or 90 days after install, for scores below 60, 60–84 and 85 or more. Each check that keeps the subtitle moves to the next step of 14, 30, 60 and 90 days, varied by ±10%. After `upgrade_checks` (4) such checks in a row, subsyncd stops looking; `0` never stops. A preferred exact match gets no upgrade check.

**Fallback subtitles** are checked for a preferred replacement and never stop:

- For media younger than `aged_release_days`, about 6 hours, 12 hours, 24 hours and 3 days after install, then a week later, then backing off to 14, 30, 60 and 90 days.
- Otherwise, and for media without a release date, first after about 7 days, then the same backoff.
- A known preferred-provider cooldown can bring a check forward to its reset.

**Technical failures** (network, LAPSE, storage) retry after 1 minute, 5 and 15 minutes, 1 hour, 6 hours and 24 hours, then every 24 hours. Provider cooldowns use their own reset times instead.

**What restarts a schedule:** a new or replaced media file, or a manual `search`. A subtitle you delete is searched again on the next check; once upgrade checks have stopped, only a replaced file or a manual `search` brings it back.

## Provider limits

subsyncd remembers provider limits across restarts. How they show in `subsyncd providers`:

| State | Cause | Ends |
| --- | --- | --- |
| `cooling_down` | A rate limit (even a daily request limit), a busy service, a blocked client, or repeated failures. | At `until`, by itself. |
| `quota_exhausted` | The provider's download quota is used up. | At `until`, by itself. |
| `auth_failed` | The provider rejected the credentials. | Only after you fix them, restart and run `subsyncd retry --provider NAME`. |

- **Other providers keep working**, including fallbacks and cached season packs.
- **Interrupted searches resume.** A first search that could not ask every provider is retried after the cooldown resets, asking only the providers it missed.
- **Whole languages pause.** When no provider for a language can download, subsyncd stops starting searches for that language and media kind until the earliest reset (`queue.route_paused`), and resumes within about 30 seconds of it. If the only usable providers have rejected credentials, there is no reset: searches stay paused until you fix the credentials and run `retry`.
- **Refusal pages.** A "download" that is really a limit or login page pauses that provider's downloads instead of rejecting a subtitle.

Cooldown lengths per provider are in the [developer limits reference](development/providers/limits.md).

## Rejected subtitles

A candidate that fails a check (invalid content, ambiguous pack, failed timing) is rejected for that file and language, and never downloaded again while the file and the rules stay the same. Searches keep looking for new candidates. A rejection is reconsidered when Sonarr corrects the episode, when the media file changes, or when subsyncd's rules for that check change.

- `subsyncd search --retry-rejected` reconsiders subsyncd's own rejections now.
- Network, provider and filesystem errors never reject a subtitle; they are retried.
- Subtitles you reject with [`reject`](commands.md#a-subtitle-is-wrong) stay rejected through LAPSE upgrades, rule changes and `--retry-rejected`, until the media file is replaced or you run `reject --undo`.

`explain` shows a file's rejections with their reasons. The [developer rejections reference](development/providers/rejections.md) lists every reason code; see [Troubleshooting](troubleshooting.md#why-is-there-no-subtitle-for-this-file) for what to do when nothing installs.
