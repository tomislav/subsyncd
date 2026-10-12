# subsyncd documentation

Start with the [README](../README.md): what subsyncd does and how to install it with Docker Compose.

## Using subsyncd

| Page | Read it to |
| --- | --- |
| [Running subsyncd](running.md) | Check the service, set permissions and resources, understand how it follows Sonarr and Radarr, set up webhooks, Arr rescans and Silo, and see what to keep private. |
| [Commands](commands.md) | Look up a file, search now, reject a wrong subtitle, see what is missing or happened recently, and script it all with `--json`. |
| [Troubleshooting](troubleshooting.md) | Find out why a file has no subtitle, what subsyncd did to an episode, and what a warning means. |
| [Configuration](configuration.md) | Look up any key in `config/config.yaml`: default, allowed values, meaning. |
| [Subtitle providers](providers.md) | Choose providers and languages, tune matching and synchronization, and learn the search schedules and provider limits. |
| [Logs](logging.md) | Read the logs, pick a log level, and see which events matter. |
| [Upgrade, back up and uninstall](upgrade-and-backup.md) | Back up `data/`, upgrade or roll back, restore a backup, and remove subsyncd cleanly. |

The commented [configuration example](../config.example.yaml) and [Compose example](../compose.example.yml) are the starting files.

## How these pages write things

- Commands are written as `subsyncd COMMAND`. With Docker Compose, run them as `docker compose exec subsyncd subsyncd COMMAND`.
- The configuration file is `config/config.yaml` on the host and `/config/config.yaml` in the container. Paths in the configuration and in commands are container paths, such as `/media/tv`.
- Words in capitals, such as `NAME` or `PATH`, are placeholders.

## Under the hood

- [Architecture](architecture.md): the components and how a subtitle gets from a provider to your library.
- [Silo API reference](references/silo.md): the contract subsyncd uses with Silo.
- [Developer documentation](development/README.md) and [contributing](../CONTRIBUTING.md): building, testing and changing subsyncd.
