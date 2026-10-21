# Upgrade, back up and uninstall

- [What is in data/](#what-is-in-data)
- [Back up](#back-up)
- [Upgrade](#upgrade)
- [Roll back to an older build](#roll-back-to-an-older-build)
- [Restoring an older backup](#restoring-an-older-backup)
- [Configuration keys from older builds](#configuration-keys-from-older-builds)
- [Uninstall](#uninstall)

## What is in data/

| Path | Holds | Back it up? |
| --- | --- | --- |
| `subsyncd.db`, `subsyncd.db-wal`, `subsyncd.db-shm` | The SQLite database: indexed media, searches and their schedules, installed subtitles, rejections, provider limits, 30 days of history, and each instance's webhook token. | Yes. This is the state that matters. |
| `lapse-cache/` | LAPSE speech-timing profiles and embedded subtitle-track timings. | Optional. Without it, the next timing check of each file decodes its audio, or reads its embedded subtitle track, again. |
| `pack-cache/` | Season packs kept for later episodes (24 hours by default). | Optional. Missing entries are dropped and downloaded again when needed. |
| `subsyncd.lock` | The lock that lets only one process change the state. | No. Never delete it while subsyncd runs. |
| `subsyncd.sock` | The control socket the CLI uses. | No. |

The database holds the webhook tokens, so a backup is a credential: keep it as private as `config/config.yaml`. Subtitle files live beside your media; back them up with your media library.

## Back up

The database runs in WAL mode: recent changes sit in `subsyncd.db-wal` until SQLite folds them in. Copying the files while subsyncd writes can give an inconsistent copy. Either stop subsyncd first:

```bash
docker compose stop subsyncd
# Copy data/ (at least subsyncd.db and its -wal and -shm files), config/ and .env.
docker compose start subsyncd
```

Or, without stopping it, take a consistent copy with SQLite's backup command on the host (needs the `sqlite3` tool and read access to `data/`):

```bash
sqlite3 data/subsyncd.db ".backup 'subsyncd-backup.db'"
```

To restore such a copy, stop subsyncd, put it in place as `data/subsyncd.db`, delete `data/subsyncd.db-wal` and `data/subsyncd.db-shm`, and start subsyncd.

Take a backup right before every upgrade. It is the only way back to the older build.

## Upgrade

The Compose file uses `ghcr.io/tomislav/subsyncd:latest`.

```bash
docker compose stop subsyncd
# Back up data/, config/ and .env.
docker compose pull
docker compose up -d
docker compose exec subsyncd subsyncd doctor
```

- A newer build may upgrade the database on its first start. Migrations are forward-only: an older build cannot open the upgraded database.
- A build whose LAPSE update can change timing results checks candidates again that the old LAPSE rejected, including files whose synchronized result was too short or ran past the end. Your own rejections, and rejections for wrong language or a downloaded file that was already too short or invalid, stay. Expect more downloads and LAPSE runs for a while after such an upgrade. The upgrade to LAPSE 2.3.0 also re-checks, once, candidates rejected earlier for being too short or invalid. To see whether an upgrade does this, compare the `rejection baseline` on the `LAPSE:` line of `doctor` before and after: it changes only when the LAPSE update can change timing results.
- To stay on one build, set `SUBSYNCD_IMAGE_TAG` in `.env` to a published version or a `sha-COMMIT` tag.
- Let the container stop fully before starting another process against the same media. The Compose file allows 75 seconds, which covers both fixed 30-second shutdown windows; don't lower `stop_grace_period`.

## Roll back to an older build

An older build refuses a database that a newer build has upgraded, and its error names the unknown migration. Restore the backup taken before the upgrade; never start from an empty data directory, which loses every record of installed subtitles.

Builds from 13 October 2026 on say this in the error. Older builds, including every build you might roll back to before then, give the wrong advice: `serve` says to rebuild from an empty data directory, and `doctor` says to initialize with `serve` or `scan`. Ignore both and restore the backup.

1. `docker compose stop subsyncd`
2. Replace `data/` with the backup you took before the upgrade. Restore `subsyncd.db` together with its own `-wal` and `-shm` files, or delete the newer ones.
3. Set `SUBSYNCD_IMAGE_TAG` in `.env` to the build you ran when you took that backup.
4. Remove configuration keys the older build does not know (for example `install.strip_watermarks` or `rescan_after_changes`). Unknown keys stop startup; `doctor` names them.
5. `docker compose up -d`, then `docker compose exec subsyncd subsyncd doctor`.

## Restoring an older backup

Restoring any backup, for a rollback or after a disk failure, rewinds subsyncd's memory to that moment. The subtitle files on disk stay as they are.

- **Subtitles installed after the backup become yours.** They are on disk but not in the restored database, so subsyncd treats them as subtitles you added: it never upgrades, replaces, rejects or removes them. To hand one back, delete it and run `subsyncd search` for that file and language.
- **Rejections and history** made after the backup are gone. A subtitle you rejected since may come back.
- **Webhook tokens** are the ones in the backup. A managed connection is corrected at the next start; a manual one needs the URL from `webhook-url` again if you rotated the token since.

The closer the backup is to the moment you restore, the less of this applies.

## Configuration keys from older builds

`instances[].webhook_token` is refused at startup: subsyncd generates its webhook tokens itself. Remove the key from every instance. The first start creates a new token for each instance. With `server.webhook_base_url` set, subsyncd updates its connection in Sonarr and Radarr; otherwise enter the new URL from `subsyncd webhook-url --instance NAME --base URL` in each [manual webhook](running.md#manual-webhooks).

## Uninstall

1. **Settle interrupted work.** If subsyncd crashed or was killed recently, start it once and let it run for a minute, so it finishes or undoes any install it was in the middle of.
2. **Stop and remove the container:** `docker compose down`.
3. **Remove the webhook connections** named `subsyncd (...)` (or your manual ones) in each Sonarr and Radarr under **Settings → Connect**. subsyncd never deletes them.
4. **Decide about the subtitles.** Subtitles subsyncd installed are ordinary sidecar files and stay beside your media. There is no command that lists which ones it installed.
5. **Remove hidden leftovers.** With subsyncd stopped, look for its temporary files on the host:

   ```bash
   find /srv/media/movies /srv/media/tv -name '.subsyncd-*'
   ```

   Use your `MOVIES_PATH` and `TV_PATH`. `.subsyncd-stage-*` are unfinished downloads, `.subsyncd-rollback-*` older copies of replaced subtitles, and `.subsyncd-removing-*` subtitles being removed. Once subsyncd is gone for good, you can delete them. Never delete them while it runs.
6. **Delete its files:** the Compose folder's `config/`, `data/` and `.env`. They hold credentials, so delete backups you no longer need too.
7. **Remove the network** (`docker network rm media`) if nothing else uses it.
