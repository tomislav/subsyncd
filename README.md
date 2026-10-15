# subsyncd

**Automatic subtitles for your Sonarr and Radarr library.**

subsyncd is a background service for people who run Sonarr or Radarr and want subtitles without managing them by hand. It finds, downloads, and synchronizes subtitles for every movie and episode in your library, in the languages you choose and from the providers you choose. Subtitles are saved as sidecar files beside your media. You configure it with one YAML file and control it from the command line; there is no web interface.

## Features

- **Movies and TV shows** from one or more Sonarr and Radarr instances.
- **Per-language provider order:** each language has its own list of providers, with optional fallbacks.
- **Checks what you already have:** embedded subtitle tracks and existing sidecar files count before anything is downloaded.
- **Matching and synchronization:** candidates are scored against the media's identity and release, and the bundled [LAPSE](https://github.com/Schwponaco-org/lapse) checks and corrects timing when needed.
- **Season packs:** takes the matching episode from ZIP and RAR downloads and skips ambiguous ones.
- **Keeps searching:** missing subtitles are searched again on a schedule until one turns up.
- **Safe upgrades:** replaces subtitles it installed with better matches, and never touches subtitles you added or edited.
- **Command line beside the running service:** see what is missing and why, search one file now, or check recent activity, by title or path; `--json` output for scripts.
- **Reject a wrong subtitle:** `reject` drops a subtitle it installed, never picks it again, and installs the next best one, or removes it when nothing else fits.
- **Survives restarts:** search history, provider limits, and rejected results are kept in its database.
- **Keeps Sonarr and Radarr current:** creates its own webhook in each instance and asks it to rescan a series or movie after a subtitle changes (`rescan_after_changes`).
- **Optional Silo refresh:** asks Silo to rescan the folder after a subtitle is installed.

## Supported services

| Service | Integration |
| --- | --- |
| [Sonarr](https://github.com/sonarr/sonarr) | TV library, imports, upgrades, renames, and file and series deletions; series rescans after subtitle changes |
| [Radarr](https://github.com/radarr/radarr) | Movie library, imports, upgrades, renames, and file and movie deletions; movie rescans after subtitle changes |
| [Silo](https://github.com/Silo-Server/silo-server) | Subtitle inventory refresh through its native scan API (v2); see the [compatibility notes](docs/references/silo.md) |

Sonarr or Radarr supplies the library; subsyncd does not scan standalone folders. A TV file that holds consecutive episodes of one season (for example `S06E01-E02`) gets one subtitle covering the whole file. Files whose episodes span seasons or have gaps are skipped.

## Subtitle providers

| Provider | Credentials | Coverage |
| --- | --- | --- |
| OpenSubtitles.com | Username and password (published images include the application key) | File-hash and title/episode searches, many languages |
| SubDL | API key | Movies, episodes, and season packs, many languages |
| Titlovi | Username and password of an API-enabled account | Bosnian, Croatian, English, Macedonian, Serbian (Latin and Cyrillic), and Slovenian |
| SubSource | API key | Movies, episodes, and season packs, more than 100 languages |
| Gestdown | None | TV episodes only, many languages |

Use one provider or several. Each language lists its providers in the order to try them:

```yaml
languages:
  en:
    providers: [opensubtitles-main, subdl-main]
  hr:
    providers: [titlovi-main]
```

subsyncd checks every language/provider pair at startup. Provider account limits still apply. Hearing-impaired (SDH/HI) subtitles are excluded unless you set `allow_hearing_impaired: true`. The [provider guide](docs/providers.md) covers enabling SubSource and Gestdown, fallback providers, and matching settings.

## Install with Docker Compose

You need Docker with the Compose plugin, a Sonarr or Radarr instance, and at least one subtitle provider. The image runs on Linux amd64 and arm64 and includes LAPSE and a minimal FFprobe 8.1; the general-purpose `ffmpeg` command is not included.

### 1. Get the configuration files

```bash
git clone https://github.com/tomislav/subsyncd.git
cd subsyncd
mkdir -p config data
cp config.example.yaml config/config.yaml
cp compose.example.yml compose.yaml
```

### 2. Set paths and credentials

Create a `.env` file next to `compose.yaml` with your host paths and container settings:

```dotenv
PUID=1000
PGID=1000
TZ=Etc/UTC
MOVIES_PATH=/srv/media/movies
TV_PATH=/srv/media/tv
```

Set `PUID` and `PGID` to a host user and group that can read your media and write into its folders, read `config/config.yaml`, and write to `data/`. `id -u` and `id -g` print your own IDs. The container does not change folder ownership; see [permissions](docs/running.md#permissions) for help.

Then edit `config/config.yaml`. Every setting is explained in the file.

- **`instances`:** set each Sonarr and Radarr `url` to an address reachable from the container, and enter its API key (found in that application's settings). Remove instances you do not use.
- **`path_mappings`:** set `remote` to the path Sonarr or Radarr reports, and `local` to where the same folder appears inside the container.
- **`providers`:** keep the providers you use and enter their credentials.
- **`languages`:** list the languages you want as tags such as `en`, `hr`, or `pt-BR`, and remove provider names you deleted.
- **`silo`:** optional; see [Silo refresh](docs/running.md#silo-refresh).

For example, if Sonarr reports `/data/tv/Show/episode.mkv` and your TV folder is mounted at `/media/tv`:

```yaml
path_mappings:
  - remote: /data/tv
    local: /media/tv
```

Keep the single quotes around credentials, and write an apostrophe inside a value as two (`it''s`). Keep `config/config.yaml` private: it holds all your credentials.

### 3. Connect the network and start

The supplied Compose file joins an existing Docker network called `media`. Create it if needed:

```bash
docker network inspect media >/dev/null 2>&1 || docker network create media
```

Attach your Sonarr and Radarr containers to the same network, so they reach subsyncd at `subsyncd:8097` and subsyncd reaches them by service name. If your stack already shares another network, put its name in `compose.yaml` instead of `media`.

If Sonarr or Radarr runs outside Docker, use its host address in `instances`, publish the port in `compose.yaml`, and set `server.webhook_base_url` to your Docker host's address and that port:

```yaml
    ports:
      - "8097:8097"
```

Start subsyncd and check its setup:

```bash
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
docker compose logs -f subsyncd
```

`doctor` checks the configuration, storage, and bundled tools. It does not test provider credentials or connections to Sonarr, Radarr, or Silo.

On first start, subsyncd reads each Sonarr and Radarr library once and queues a subtitle check for every movie and episode file. After that it follows changes through webhooks and a history check every six hours.

### 4. Sonarr and Radarr webhooks

subsyncd creates and maintains its own webhook connection in each instance under **Settings → Connect**. It points the connection at `server.webhook_base_url`, which the example configuration sets to `http://subsyncd:8097`, and names it after that address: `subsyncd (subsyncd:8097)`. Sonarr and Radarr must be able to reach that address, including through an HTTP proxy if they use one. To add webhooks by hand instead, see [Webhook connections](docs/running.md#webhook-connections).

## Everyday use

subsyncd runs on its own. To check on it or step in, run commands beside the running service:

```bash
docker compose exec subsyncd subsyncd coverage                     # subtitles per library and language
docker compose exec subsyncd subsyncd missing                      # what has no subtitle yet, and why
docker compose exec subsyncd subsyncd find --title "Show S01E02"   # one file's state per language
docker compose exec subsyncd subsyncd search --title "Movie (2019)"
docker compose exec subsyncd subsyncd reject --title "Movie (2019)" --language en --reason out_of_sync
docker compose exec subsyncd subsyncd recent                       # what happened in the last day
docker compose exec subsyncd subsyncd status
```

Name files as your media player shows them: `Movie (2019)`, `Show S01E02`. After changing `config/config.yaml`, restart with `docker compose restart subsyncd`. A newly added language is then checked for media already in the library. The [command guide](docs/commands.md) lists every command, including [what to do when a subtitle is wrong](docs/commands.md#a-subtitle-is-wrong) and [scripting](docs/commands.md#scripting-and-automation).

## Updates

The Compose file uses `ghcr.io/tomislav/subsyncd:latest`. Before updating, stop the service and back up `data/`: it holds search history, caches, and records of installed subtitles, and a newer build may upgrade the database so an older one cannot read it.

```bash
docker compose stop subsyncd
# Back up data/, config/, and .env.
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To stay on one build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or `sha-<commit>` tag. [Upgrade, back up and uninstall](docs/upgrade-and-backup.md) covers rollback and upgrading from builds that used `webhook_token`.

## Logs and troubleshooting

Read the logs with `docker compose logs subsyncd`. For more detail, set `logging.level: debug` and restart. The [logging guide](docs/logging.md) explains levels and how to follow one search; the [troubleshooting guide](docs/troubleshooting.md) covers common problems.

## Documentation

- [Documentation index](docs/README.md): every guide below, and how they write commands and paths.
- [Configuration example](config.example.yaml): every setting, with defaults; the [configuration reference](docs/configuration.md) lists allowed values.
- [Running subsyncd](docs/running.md): permissions, resources, security, webhooks, Arr rescans, and Silo.
- [Command guide](docs/commands.md): commands, manual searches, rejecting subtitles, and scripting.
- [Troubleshooting](docs/troubleshooting.md): why a file has no subtitle, and what a warning means.
- [Upgrade, back up and uninstall](docs/upgrade-and-backup.md): backups, upgrades, rollback, and removal.
- [Provider guide](docs/providers.md): provider setup, languages, matching, synchronization, and retry schedules.
- [Logging guide](docs/logging.md): log levels, following a search, warnings, and sharing diagnostics safely.
- [Architecture](docs/architecture.md): system boundaries and the processing flow.
- [Developer references](docs/development/README.md): implementation contracts for persistence, providers, notifications, and logging.

## Contributing

Tests use local fixtures and fake services; they never contact your library or subtitle providers. See [CONTRIBUTING.md](CONTRIBUTING.md) for building from source, running the tests, and commit conventions.

## License

subsyncd is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Tomislav Filipcic.

Third-party dependencies and bundled tools keep their own licenses.
