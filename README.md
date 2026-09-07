# subsyncd

**Automatic subtitles for your Sonarr and Radarr library.**

subsyncd finds, downloads, and synchronizes subtitles for your movies and TV shows. Choose your languages and subtitle providers, connect your Sonarr or Radarr instances, and let it run in the background.

Subtitles are saved alongside your media files. Configuration lives in a YAML file, with command-line tools for diagnostics and manual searches. There is no web interface.

## Features

- **Movies and TV shows:** connect one or more Sonarr and Radarr instances.
- **Your languages, your providers:** choose a different provider order for each language. Adding a language schedules searches for already-indexed media after a restart.
- **Checks what you already have:** detects embedded subtitles and separate subtitle files before searching.
- **Smart matching and synchronization:** scores candidates against your media’s identity and release details, then uses bundled [LAPSE](https://github.com/Schwponaco-org/lapse) to verify and adjust timing when needed.
- **Season-pack support:** extracts the matching episode from ZIP and RAR downloads, skipping ambiguous matches.
- **Ongoing searches:** keeps checking for missing subtitles when no suitable match is available.
- **Safe subtitle upgrades:** can replace subtitles it installed with better matches, while protecting manually added subtitles and any files you’ve edited.
- **Remembers its progress:** saves search history and provider limits across restarts, and avoids repeatedly downloading rejected results.
- **Optional Silo refresh:** asks Silo to rescan a media file after installing subtitles.

## Supported services

| Service | Integration |
| --- | --- |
| [Sonarr](https://github.com/sonarr/sonarr) | TV library, imports, upgrades, renames, and file deletions |
| [Radarr](https://github.com/radarr/radarr) | Movie library, imports, upgrades, renames, and file deletions |
| [Silo](https://github.com/Silo-Server/silo-server) | Refreshes subtitle inventory through its native scan API |

TV files containing multiple episodes are currently unsupported and skipped during subtitle searches.

Sonarr or Radarr supplies the library: subsyncd does not scan a standalone folder as a replacement for either service. See the [Silo compatibility notes](docs/references/silo.md) before enabling that integration.

## Subtitle providers

| Provider | Credentials | Support |
| --- | --- | --- |
| OpenSubtitles.com | Username and password (application key included in published images) | File-hash and title/episode searches across multiple languages |
| SubDL | API key | Movie, episode, and season-pack searches across multiple languages |
| Titlovi | API-enabled account username and password | Bosnian, Croatian, English, Macedonian, Serbian (Latin and Cyrillic), and Slovenian |

Use one provider or combine several. Available languages depend on the provider; subsyncd checks your language/provider settings at startup. Provider account limits still apply.

The example configuration uses Titlovi for Croatian and OpenSubtitles followed by SubDL for English. Change this to suit your library:

```yaml
languages:
  en:
    providers: [opensubtitles-main, subdl-main]
  hr:
    providers: [titlovi-main]
```

Hearing-impaired subtitles (SDH/HI) are excluded by default. Set `allow_hearing_impaired: true` to include them. See [provider configuration](docs/providers.md) for more options.

## Install with Docker Compose

You need Docker with the Compose plugin, a Sonarr or Radarr instance, and credentials for at least one subtitle provider. The image supports **Linux amd64 and arm64** and includes LAPSE and FFmpeg—no separate installation is needed.

### 1. Get the configuration files

```bash
git clone https://github.com/tomislav/subsyncd.git
cd subsyncd
mkdir -p config data
cp config.example.yaml config/config.yaml
cp compose.example.yml compose.yaml
```

### 2. Set paths and credentials

Create a `.env` file next to `compose.yaml`. The following covers the services and providers in the example configuration; replace the placeholders with your own values:

```dotenv
PUID=1000
PGID=1000
TZ=Etc/UTC
MOVIES_PATH=/srv/media/movies
TV_PATH=/srv/media/tv

SONARR_MAIN_API_KEY=your-sonarr-api-key
SONARR_MAIN_WEBHOOK_TOKEN=your-own-random-sonarr-secret
RADARR_MAIN_API_KEY=your-radarr-api-key
RADARR_MAIN_WEBHOOK_TOKEN=your-own-random-radarr-secret

TITLOVI_USERNAME=your-username
TITLOVI_PASSWORD=your-password
OPENSUBTITLES_USERNAME=your-username
OPENSUBTITLES_PASSWORD=your-password
SUBDL_API_KEY=your-api-key
```

Find the Sonarr and Radarr API keys in each application's settings. Choose a separate random webhook secret for each instance; you will use it again in step 4. Keep `.env` private—it contains your credentials.

Edit `config/config.yaml` to:

- Set the Sonarr and Radarr URLs to addresses reachable from the container. Remove any instance you do not use.
- Keep only the providers you want, and remove unused provider names from `languages` too.
- Choose your languages, using tags such as `en`, `hr`, or `pt-BR`.
- Match each `path_mappings.remote` to the path reported by Sonarr or Radarr. The `local` path is where that same folder appears inside subsyncd.

For example, if Sonarr reports `/data/tv/Show/episode.mkv` and your TV folder is mounted at `/media/tv`, use:

```yaml
path_mappings:
  - remote: /data/tv
    local: /media/tv
```

Set `PUID` and `PGID` to a host user and group that can read your media and write subtitles into its folders. The same identity must be able to read `config/config.yaml` and write to `data/`. You can check your own IDs with `id -u` and `id -g`. The container does not change folder ownership for you; see [permissions](docs/operations.md#filesystem-and-container-permissions) if you need help.

### 3. Connect the network and start

The supplied Compose file uses an existing Docker network called `media`. Create it if needed:

```bash
docker network inspect media >/dev/null 2>&1 || docker network create media
```

Attach your Sonarr and Radarr containers to that same network so they can reach `subsyncd:8097` and subsyncd can reach them by service name. If your stack already uses another shared network, change `media` in `compose.yaml` to that network's name.

If Sonarr or Radarr runs outside Docker, use its reachable host address in the configuration and add a port mapping to the `subsyncd` service in `compose.yaml`:

```yaml
    ports:
      - "8097:8097"
```

Then start subsyncd and check its local setup:

```bash
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
docker compose logs -f subsyncd
```

`doctor` checks configuration, storage, and bundled tools. It does not test provider credentials or connections to Sonarr, Radarr, or Silo.

### 4. Add Sonarr and Radarr webhooks

In each application's **Settings → Connect**, add a webhook using the instance name from `config/config.yaml` and the secret you chose in `.env`:

```text
http://subsyncd:8097/webhooks/sonarr-main?token=YOUR_SONARR_SECRET
http://subsyncd:8097/webhooks/radarr-main?token=YOUR_RADARR_SECRET
```

For an instance outside Docker, replace `subsyncd` with your Docker host's address and use the published port.

Enable import/download, upgrade, rename, and file-delete events where available. Use the connection's **Test** action to check the webhook. Test events do not trigger subtitle searches.

subsyncd also checks for library changes on startup and periodically while running.

## Updates

The Compose file pulls `ghcr.io/tomislav/subsyncd:latest` by default. To update:

```bash
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To stay on a particular build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or `sha-<commit>` tag. Back up `data/` with the service stopped before upgrading; it holds search history, caches, and records of installed subtitles.

Logs are available through `docker compose logs`. Set `logging.level: debug` in your configuration and restart for more detail while troubleshooting. See [logging](docs/logging.md) for available levels and troubleshooting details.

## Documentation and development

- [Configuration example](config.example.yaml) — all settings in one place.
- [Operations guide](docs/operations.md) — manual searches, diagnostics, Silo setup, backups, and troubleshooting.
- [Provider guide](docs/providers.md) — language support, matching, synchronization, and retry schedules.

To build from source, install Go 1.27.1, FFprobe, and LAPSE v2.0.5. Set absolute paths in your configuration, including `sync.lapse_path`. Native builds and locally built Docker images also need an OpenSubtitles application key in the provider’s `api_key` setting; published images include it.

```bash
go build -trimpath -o subsyncd ./cmd/subsyncd
./subsyncd serve --config ./config/config.yaml
```

After the first startup initializes the database, run `./subsyncd doctor --config ./config/config.yaml` from another terminal. Diagnostics open the existing database read-only; they do not initialize or migrate it.

To build a local Docker image:

```bash
docker build --build-arg VERSION=dev -t subsyncd:local .
```

For contributions, read [AGENTS.md](AGENTS.md) and run the local test suite. Ordinary tests use local fixtures and fake services; they do not contact your library or subtitle providers.

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
```

## License

subsyncd is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Tomislav Filipcic.

Third-party dependencies and bundled tools retain their respective licenses.
