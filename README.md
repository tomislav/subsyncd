# subsyncd

**Automatic subtitles for your Sonarr and Radarr library.**

subsyncd finds, downloads, and synchronizes subtitles for your movies and TV shows. Choose your languages and subtitle providers, connect your Sonarr or Radarr instances, and let it run in the background.

Subtitles are saved alongside your media files. Configuration lives in a YAML file, and a command line lets you look up, search, or reject subtitles for any file while it runs. There is no web interface.

## Features

- **Movies and TV shows:** connect one or more Sonarr and Radarr instances.
- **Your languages, your providers:** choose a different provider order for each language. Adding a language schedules searches for already-indexed media after a restart.
- **Checks what you already have:** detects embedded subtitles and separate subtitle files before searching.
- **Smart matching and synchronization:** scores candidates against your media’s identity and release details, then uses bundled [LAPSE](https://github.com/Schwponaco-org/lapse) to verify and adjust timing when needed.
- **Season-pack support:** extracts the matching episode from ZIP and RAR downloads, skipping ambiguous matches.
- **Ongoing searches:** keeps checking for missing subtitles when no suitable match is available.
- **Safe subtitle upgrades:** can replace subtitles it installed with better matches, while protecting manually added subtitles and any files you’ve edited.
- **Command line that works while it runs:** see what is missing and why, search one file now, or check recent activity, by title or path instead of IDs; `--json` output for scripts ([commands](docs/operations.md#commands)).
- **Tell it a subtitle is wrong:** `reject` drops a subtitle it installed, never picks it again, and installs the next best one, or removes it when nothing else fits ([how](docs/operations.md#a-subtitle-is-wrong)).
- **Remembers its progress:** saves search history and provider limits across restarts, and avoids repeatedly downloading rejected results.
- **Optional Silo refresh:** asks Silo to rescan the folder after installing subtitles.

## Supported services

| Service | Integration |
| --- | --- |
| [Sonarr](https://github.com/sonarr/sonarr) | TV library, imports, upgrades, renames, and file and series deletions |
| [Radarr](https://github.com/radarr/radarr) | Movie library, imports, upgrades, renames, and file and movie deletions |
| [Silo](https://github.com/Silo-Server/silo-server) | Refreshes subtitle inventory through its native scan API |

TV files containing several consecutive episodes of one season (for example `S06E01-E02`) receive one subtitle that covers the whole file; only subtitles explicitly covering every episode are used. Files whose episodes span seasons or have gaps are skipped.

Sonarr or Radarr supplies the library: subsyncd does not scan a standalone folder as a replacement for either service. See the [Silo compatibility notes](docs/references/silo.md) before enabling that integration.

## Subtitle providers

| Provider | Credentials | Support |
| --- | --- | --- |
| OpenSubtitles.com | Username and password (application key included in published images) | File-hash and title/episode searches across multiple languages |
| SubDL | API key | Movie, episode, and season-pack searches across multiple languages |
| Titlovi | API-enabled account username and password | Bosnian, Croatian, English, Macedonian, Serbian (Latin and Cyrillic), and Slovenian |
| SubSource | API key | Movie, episode, and season-pack searches across more than 100 languages |
| Gestdown | No account or API key | TV episodes across multiple languages; movies are skipped |

Use one provider or combine several. Available languages depend on the provider; subsyncd checks your language/provider settings at startup. Provider account limits still apply.

The example configuration uses Titlovi for Croatian and OpenSubtitles followed by SubDL for English. Change this to suit your library:

```yaml
languages:
  en:
    providers: [opensubtitles-main, subdl-main]
  hr:
    providers: [titlovi-main]
```

To use SubSource, uncomment `subsource-main` in the example configuration, add your API key, and add it to a language’s `providers` or `fallback_providers` list. To use Gestdown, uncomment `gestdown-main` in the example configuration and add it to a language’s `providers` or `fallback_providers` list. Keep a movie-capable provider in routes used by Radarr.

Hearing-impaired subtitles (SDH/HI) are excluded by default. Set `allow_hearing_impaired: true` to include them. See [provider configuration](docs/providers.md) for more options.

## Install with Docker Compose

You need Docker with the Compose plugin, a Sonarr or Radarr instance, and at least one configured subtitle provider. Gestdown needs no account or API key; other providers require credentials. The image supports **Linux amd64 and arm64** and includes LAPSE and a minimal FFprobe 8.1—no separate installation is needed. The general-purpose `ffmpeg` command is not included.

### 1. Get the configuration files

```bash
git clone https://github.com/tomislav/subsyncd.git
cd subsyncd
mkdir -p config data
cp config.example.yaml config/config.yaml
cp compose.example.yml compose.yaml
```

### 2. Set paths and credentials

Create a `.env` file next to `compose.yaml`. Use it for host paths and container settings:

```dotenv
PUID=1000
PGID=1000
TZ=Etc/UTC
MOVIES_PATH=/srv/media/movies
TV_PATH=/srv/media/tv
```

Find the Sonarr and Radarr API keys in each application's settings. Enter the API keys in the quoted placeholders under `instances` in `config/config.yaml`.

Edit `config/config.yaml` to:

- Set the Sonarr and Radarr URLs to addresses reachable from the container. Remove any instance you do not use.
- Keep only the providers you want, enter any required usernames, passwords, and API keys in the quoted placeholders, and remove unused provider names from `languages` too. Gestdown requires no credentials.
- If you use Silo, ensure it supports API v2, enable it, and replace its admin API-key placeholder in the same file.
- Choose your languages, using tags such as `en`, `hr`, or `pt-BR`.
- Match each `path_mappings.remote` to the path reported by Sonarr or Radarr. The `local` path is where that same folder appears inside subsyncd.
- Optionally set `queue_priority` on an instance (0–1000, default 0) to work through its library before instances with a lower value. New imports still go first.

Keep `config/config.yaml` private—it contains all your service credentials. Keep the single quotes around values; write an apostrophe inside a value as two apostrophes (`it''s`).

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

Set `server.webhook_base_url` in `config/config.yaml` to the address Sonarr and Radarr use to reach subsyncd, usually `http://subsyncd:8097`, then restart subsyncd. For each instance it creates a webhook connection named `subsyncd (subsyncd:8097)` in **Settings → Connect** with the right events, adopts a matching one that already exists, and keeps it correct. Its changes are limited to that one connection's URL, method and required events.

Sonarr and Radarr test the URL whenever they save a connection with events enabled, so subsyncd must be reachable from them. If Sonarr or Radarr use an HTTP proxy, it must let subsyncd's address through.

To add the webhooks by hand instead, leave `webhook_base_url` unset. In each application's **Settings → Connect**, add a webhook using the URL that subsyncd prints for the instance:

```bash
docker compose exec subsyncd subsyncd webhook-url --instance sonarr-main --base http://subsyncd:8097
```

The URL looks like this, where the token is one subsyncd generated for the instance and keeps in its database:

```text
http://subsyncd:8097/webhooks/sonarr-main?token=SONARR_WEBHOOK_TOKEN
```

Use the address Sonarr or Radarr reach subsyncd at as `--base`; for an instance outside Docker, that is your Docker host's address and the published port. Without `--base` (and with `server.webhook_base_url` unset) the command prints only the `/webhooks/...?token=...` path and query.

For a manual webhook, enable import/download, upgrade, rename, and file-delete events where available. Use the connection's **Test** action to check the webhook. Test events do not trigger subtitle searches, and other event types are accepted and ignored. subsyncd answers a webhook as soon as it has saved it and applies it right after, so Arr never waits on subsyncd's own lookups.

After the daemon starts, subsyncd discovers existing movie and episode files in each configured Sonarr/Radarr library once, including files absent from retained history, and queues subtitle checks. It remembers completion across restarts and then follows changes through webhooks and periodic history reconciliation.

## Everyday use

subsyncd works on its own once connected. When you want to look or step in, run commands beside the running service:

```bash
docker compose exec subsyncd subsyncd missing                      # what still has no subtitle, and why
docker compose exec subsyncd subsyncd find --title "Show S01E02"   # one file's state per language
docker compose exec subsyncd subsyncd search --title "Movie (2019)"
docker compose exec subsyncd subsyncd reject --title "Movie (2019)" --language en --reason out_of_sync
docker compose exec subsyncd subsyncd recent                       # what happened in the last day
docker compose exec subsyncd subsyncd status
```

Name files as you see them in your media player: `Movie (2019)`, `Show S01E02`. See the [operations guide](docs/operations.md#commands) for every command.

## Updates

The Compose file pulls `ghcr.io/tomislav/subsyncd:latest` by default. Back up `data/` with the service stopped before upgrading: it holds search history, caches, and records of installed subtitles, and a newer build may upgrade the database in a way an older one cannot read. Then update:

```bash
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

To stay on a particular build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or `sha-<commit>` tag. See [Back up and update](docs/operations.md#back-up-and-update) for rolling back.

Upgrading from a build that read `webhook_token` from the configuration: remove `webhook_token` from every instance first, because startup refuses the old key. The first start generates a new token for each instance. With `server.webhook_base_url` set, subsyncd updates the connection in Sonarr or Radarr itself. Otherwise run `subsyncd webhook-url --instance NAME --base URL` for each instance and put the new URL in its webhook.

## Logs and troubleshooting

Logs are available through `docker compose logs`. Set `logging.level: debug` in your configuration and restart for more detail while troubleshooting. See the [logging guide](docs/logging.md) for levels and how to follow one search, and the [operations guide](docs/operations.md#troubleshooting) for common problems.

## Documentation

- [Configuration example](config.example.yaml) — all settings in one place.
- [Operations guide](docs/operations.md) — commands, manual searches, rejecting subtitles, [scripting and automation](docs/operations.md#scripting-and-automation), Silo setup, backups, and troubleshooting.
- [Provider guide](docs/providers.md) — language support, matching, synchronization, and retry schedules.
- [Logging guide](docs/logging.md) — log levels, workflow tracing, warnings, and safe diagnostic sharing.
- [Architecture](docs/architecture.md) — system boundaries and durable processing flow.
- [Developer references](docs/development/) — current implementation contracts for operations, providers, and logging.

## Build from source

Install Go 1.27.1, FFprobe, and LAPSE v2.2.4. Set absolute paths in your configuration, including `sync.lapse_path`. Native builds and locally built Docker images also need an OpenSubtitles application key in the provider’s `api_key` setting; published images include it.

```bash
go build -trimpath -o subsyncd ./cmd/subsyncd
./subsyncd serve --config ./config/config.yaml
```

After the first startup initializes the database, run `./subsyncd doctor --config ./config/config.yaml` from another terminal. Diagnostics open the existing database read-only; they do not initialize or migrate it.

To build a local Docker image:

```bash
docker build --build-arg VERSION=dev -t subsyncd:local .
```

## Contributing

Read [AGENTS.md](AGENTS.md) and the [developer references](docs/development/) first, then run the test suite. Ordinary tests use local fixtures and fake services; they do not contact your library or subtitle providers.

```bash
go test -race ./...
go vet ./...
go test ./test/e2e -tags=e2e -race
```

Commit subjects follow Conventional Commits (`fix(control): bound socket drain on shutdown`); enable the checking hook once with `git config core.hooksPath .githooks`.

## License

subsyncd is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Tomislav Filipcic.

Third-party dependencies and bundled tools retain their respective licenses.
