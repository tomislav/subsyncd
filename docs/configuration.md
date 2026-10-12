# Configuration reference

subsyncd reads one YAML file: `config/config.yaml` on the host, mounted as `/config/config.yaml` in the container. The [commented example](../config.example.yaml) is a working starting point; this page lists every key it accepts.

- [How the file is read](#how-the-file-is-read)
- [Credentials and environment variables](#credentials-and-environment-variables)
- [Top level and server](#top-level-and-server)
- [Sonarr and Radarr instances](#sonarr-and-radarr-instances)
- [Providers](#providers)
- [Languages](#languages)
- [Matching and schedules](#matching-and-schedules)
- [Caches](#caches)
- [Synchronization (LAPSE)](#synchronization-lapse)
- [Installing subtitles](#installing-subtitles)
- [Silo](#silo)

## How the file is read

- **Restart after any change.** There is no reload: run `docker compose restart subsyncd`.
- **Unknown keys stop startup.** A misspelled key is an error, not a silent default. So is the old `instances[].webhook_token` key: subsyncd creates webhook tokens itself (see [Upgrade and backup](upgrade-and-backup.md#configuration-keys-from-older-builds)).
- **Leave out what you don't change.** Every key without "required" has a default, so you can delete it.
- **Paths are container paths.** `data_dir`, `media_roots`, `path_mappings[].local` and `sync.lapse_path` must be absolute paths inside the container.
- **One document only.** The file must hold exactly one YAML document.
- **Choosing the file.** Every command takes `--config PATH`. Without it, subsyncd uses the `SUBSYNCD_CONFIG` environment variable, else `/config/config.yaml`.

`subsyncd doctor` loads the file and reports the first problem it finds.

## Credentials and environment variables

`config/config.yaml` holds every credential subsyncd uses: the Sonarr and Radarr API keys, provider keys and passwords, and the Silo admin key. Keep it readable only by you and the container user. [Security](running.md#security) lists everything else that needs protecting.

Write credentials in single quotes. To put an apostrophe inside a quoted value, type it twice (`'it''s'`).

To keep a value out of the file, write the whole value as `'${NAME}'`. subsyncd then reads it from the environment variable `NAME`, and startup fails when `NAME` is not set. This works for any string value, such as a password, API key or URL. Only a value that is exactly `${NAME}` is expanded; `prefix-${NAME}` is used as written. Pass the variable to the container under `environment:` in `compose.yaml` (the [Compose example](../compose.example.yml) shows where).

```yaml
providers:
  subdl-main:
    type: subdl
    api_key: '${SUBDL_API_KEY}'
```

## Top level and server

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `data_dir` | required | absolute path | Database, caches, lock and control socket. The example uses `/data`. |
| `media_roots` | required | absolute paths, at least one | Folders subsyncd may write subtitles into. |
| `server.listen` | `0.0.0.0:8097` | `host:port` | Address for webhooks and the `/healthz` and `/readyz` checks. |
| `server.webhook_base_url` | unset | `http` or `https` URL, no query | subsyncd's address as Sonarr and Radarr reach it. Set: subsyncd manages its Arr webhooks. Unset: you add them by hand. A path prefix is allowed. |
| `logging.level` | `info` | `debug`, `info`, `warn`, `error` | Log level. The `SUBSYNCD_LOG_LEVEL` environment variable overrides it. |
| `worker.max_concurrent` | `1` | 1–8 | Subtitle searches that run at the same time. More needs more `/tmp`; see [Resources](running.md#resources-and-lapse-run-time). |
| `provider_http.shared_origin_max_concurrent` | `1` | 1 or more | Requests in flight to one provider website across all accounts you configured for it. |

## Sonarr and Radarr instances

`instances` is a list. Each entry:

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `name` | required | unique | Your name for the instance. It is part of the webhook URL and of every command's `--instance`. |
| `type` | required | `sonarr`, `radarr` | Which application it is. |
| `url` | required | `http` or `https` URL, no query | Address reachable from the container. |
| `api_key` | required | string | The application's API key (Settings → General). |
| `path_mappings` | none, so no file of the instance is processed | list of `remote`/`local` | Translates the path Arr reports (`remote`) to the same folder in the container (`local`). Each `local` must sit inside a media root. Media whose path matches no mapping is skipped. |
| `queue_priority` | `0` | 0–1000 | Higher runs first within each queue stage. See [Queue order](running.md#queue-order). |
| `rescan_after_changes` | `true` | `true`, `false` | Ask Arr to rescan the series or movie after a subtitle change. See [Arr rescans](running.md#sonarr-and-radarr-rescans). |

## Providers

`providers` maps a name you choose (such as `opensubtitles-main`) to one provider account. List that name under `languages` to use it. [Providers](providers.md) explains how to choose them.

Every provider accepts:

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `type` | required | `opensubtitles`, `subdl`, `titlovi`, `subsource`, `gestdown` | Which provider this is. |
| `requests_per_second` | `1` (SubSource `0.5`) | above 0 | Average request rate. |
| `burst` | `1` | 1 or more | Requests allowed back to back before the rate applies. |
| `max_concurrent` | `1` | 1 or more | Requests in flight at the same time. |

Lower these if a provider complains; raising them rarely helps.

Provider-specific keys:

| Type | Key | Default | Meaning |
| --- | --- | --- | --- |
| `opensubtitles` | `username`, `password` | required | Your OpenSubtitles.com account. |
| | `user_agent` | required | Sent as the HTTP User-Agent; any short identifier such as `subsyncd`. |
| | `api_key` | built into published images | Needed only for native builds and locally built images. |
| `subdl` | `api_key` | required | Your SubDL API key. |
| `titlovi` | `username`, `password` | required | An API-enabled Titlovi account. |
| | `max_pages` | `5` | Result pages read per search, 1 or more. |
| `subsource` | `api_key` | required | Your SubSource API key. |
| | `max_pages` | `5` | 100-result pages read per title and language, 1–20. |
| `gestdown` | none | | No account needed. |

Each provider also accepts endpoint and size overrides, meant for tests and private endpoints. Every base URL must use HTTPS. SubSource and Gestdown accept a `max_download_bytes` only up to their built-in limit; the others accept any positive value. Leave them unset:

| Type | Keys and defaults |
| --- | --- |
| `opensubtitles` | `base_url` (`https://api.opensubtitles.com/api/v1`), `max_download_bytes` (32 MiB) |
| `subdl` | `base_url`, `download_base_url`, `download_hosts` (`dl.subdl.com`), `max_download_bytes` (64 MiB) |
| `titlovi` | `api_base_url`, `download_base_url`, `download_hosts`, `max_download_bytes` (64 MiB) |
| `subsource` | `base_url`, `max_download_bytes` (20 MiB, also the maximum) |
| `gestdown` | `base_url` (an origin with no path), `max_download_bytes` (20 MiB, also the maximum) |

## Languages

`languages` maps a language tag (BCP 47, such as `en`, `hr`, `pt-BR`) to its providers. At least one language is required.

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `providers` | required | at least one configured provider | The preferred tier, tried in order. |
| `fallback_providers` | none | configured providers | Tried only when the preferred tier has no installable subtitle or is unavailable. |

A provider may appear only once per language, across both tiers. Startup checks that every provider supports each language it is listed for. Two tags that mean the same language (such as `pt-br` and `pt-BR`) are refused.

## Matching and schedules

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `allow_hearing_impaired` | `false` | `true`, `false` | Accept hearing-impaired (SDH/HI) subtitles and tracks. |
| `minimum_release_score` | `35` | 1–100; `0` means the default | Lowest score a non-exact match needs to be considered. |
| `upgrade_checks` | `4` | 0–100 | Unchanged upgrade checks in a row before subsyncd stops looking for a better subtitle. `0` never stops. Fallback subtitles never stop. |
| `aged_release_days` | `30` | 0–3650 | Media released longer ago skips the early missing-subtitle retries; younger fallback subtitles get early preferred-provider rechecks. `0` turns off both the aged shortcut and the early fallback rechecks, so fallback subtitles are first rechecked after about a week. |

[When subsyncd searches](providers.md#when-subsyncd-searches) explains the schedules these keys change.

## Caches

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `pack_cache.ttl` | `24h` | positive duration | How long a downloaded season pack is kept for later episodes. |
| `pack_cache.max_bytes` | `536870912` (512 MiB) | 1 or more | Total size of the pack cache. |
| `lapse_cache.ttl` | `720h` (30 days) | positive duration | How long LAPSE speech-timing profiles are kept, counted from when they were written. Reading a profile does not extend it. |

Expired LAPSE profiles are removed at startup and hourly while subsyncd runs, and are rebuilt when needed. Expiry never removes subtitles, resets schedules or clears rejections.

## Synchronization (LAPSE)

[LAPSE](https://github.com/Schwponaco-org/lapse) checks and corrects subtitle timing against the media's audio.

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `sync.lapse_path` | required | absolute path | The LAPSE binary. Published images have it at `/opt/lapse/lapse`. |
| `sync.timeout` | `30m` | positive duration | Time limit for one LAPSE run. |
| `sync.min_read_rate` | `4` | 0 or more, MiB per second | For a file too large to read at this rate within `timeout`, the limit becomes size ÷ rate. `0` turns the extension off. |
| `sync.max_timeout` | `4h`, or `timeout` when longer | at least `timeout` | Upper limit for that extension. A whole search is stopped after twice this. |
| `sync.policy` | `confidence` | `confidence`, `always`, `never` | When to run LAPSE. See [Synchronization](providers.md#synchronization). |
| `sync.bypass_score` | `75` | 1–100; `0` means the default | Lowest score for a match to skip LAPSE under `confidence`. |
| `sync.require_identity_anchor` | `true` | `true`, `false` | Skipping LAPSE needs a matching external ID, or title and year. |
| `sync.require_episode_evidence` | `true` | `true`, `false` | For TV, skipping LAPSE needs the subtitle to name the right episode. |
| `sync.require_release_group` | `true` | `true`, `false` | Skipping LAPSE needs a matching release group. |
| `sync.lapse_for_packs` | `true` | `true`, `false` | Always check subtitles taken from season packs. |
| `sync.lapse_for_upgrades` | `true` | `true`, `false` | Always check upgrades of a subtitle subsyncd installed. |

The last six keys apply only under `policy: confidence`; `never` ignores them.

## Installing subtitles

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `install.file_mode` | `"0644"` | quoted octal, no execute bits | Permissions of new subtitle files. |
| `install.uid`, `install.gid` | unset | 0 or more | Owner and group of new subtitles. Unset keeps the container user. The supplied Compose file drops all capabilities, so changing the owner fails and `gid` works only for a group the container user belongs to. |
| `install.strip_watermarks` | `false` | `true`, `false` | Drop subtitle-site advert cues from SRT subtitles when installing them. See [Strip site adverts](providers.md#strip-site-adverts). |

## Silo

| Key | Default | Allowed | Meaning |
| --- | --- | --- | --- |
| `silo.enabled` | `false` | `true`, `false` | Ask Silo to rescan a folder after subsyncd installs, replaces or removes a subtitle in it. |
| `silo.url` | required when enabled | URL without `/api/v2` | Silo's native API v2 server base URL. |
| `silo.api_key` | required when enabled | string | A Silo admin API key. |
| `silo.path_mappings` | none | list of `from`/`to`, both absolute | Translates subsyncd's media paths (`from`) to Silo's paths (`to`). |

See [Silo refresh](running.md#silo-refresh).
