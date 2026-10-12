# Developer documentation

These pages describe the behavior the implementation must preserve. They are living references: update the relevant page in the same change as the code. Completed plans, review notes and status files belong in Git history, not here.

Start with [CONTRIBUTING](../../CONTRIBUTING.md) for setup, tests, CI and commit rules, and [Architecture](../architecture.md) for the big picture. Operator documentation starts at [docs/README.md](../README.md).

## Pages

| Page | Covers |
| --- | --- |
| [Architecture](../architecture.md) | Components, durable state, the processing flow |
| [Persistence](persistence.md) | SQLite, the data directory, the process lock, tables, the migration index and how to add a migration |
| [Reconciliation](reconciliation.md) | Webhook intake and inbox, history reconciliation, deferral, the snapshot-deletion guard, replays, discovery, webhook tokens, the managed Arr connection |
| [Queue and control](queue-and-control.md) | Queue order, the worker, leases, shutdown, job limits, the media breaker, the control socket, routed and read-only commands, manual searches, scans, search history |
| [Installation safety](installation-safety.md) | Sidecar ownership, temporary files, publication, crash recovery, rollback copies, replaced and renamed media, reject and removal, watermarks |
| [Notifications](notifications.md) | The outbox, Silo, Arr rescans, retries and retention |
| [Providers and matching](providers/README.md) | Adapters, matching, limits, scheduling, rejections and LAPSE |
| [Logging](logging.md) | Record shape, privacy rules and the event catalogue |
| [Silo API reference](../references/silo.md) | The external Silo scan contract |

## Package map

| Package | Responsibility |
| --- | --- |
| `cmd/subsyncd` | Binary entry point: builds the CLI with `app.Open` as the local backend and `control.Dial` to reach a running daemon |
| `internal/app` | Composition root: validates configuration, opens the store, takes the lock, builds providers, workflow, worker, catalogs, notifiers and the HTTP and control servers, and implements every command |
| `internal/catalog` | Sonarr and Radarr clients, path mapping, history reconciliation and library discovery, the webhook inbox, the managed webhook connection, and the Arr rescan client |
| `internal/cli` | Flag parsing and command dispatch: routes `retry`, `search`, `reject` and `scan` to a daemon when one runs, otherwise opens the backend; renders text and `--json` |
| `internal/config` | YAML model, `${NAME}` expansion, defaults and validation |
| `internal/control` | The Unix-socket JSON protocol between CLI and daemon, error codes, and the shared argument and result types |
| `internal/domain` | Pure domain types: media and fingerprints, candidates, languages, scores, sync results, mount-error classification |
| `internal/httpapi` | HTTP server: `/healthz`, `/readyz` and `POST /webhooks/{instance}` |
| `internal/inventory` | FFprobe probing and sidecar scanning; decides whether a language is satisfied |
| `internal/langcheck` | Detects Croatian or Bosnian requests whose subtitle text is in another language |
| `internal/match` | Release parsing, identity gates, scoring, ranking and release affinity |
| `internal/notifier` | Silo scan and Arr rescan notifiers, coalescing, and delivery-error classification |
| `internal/observability` | The structured event emitter, error redaction hooks, and privacy-safe text, title and path helpers |
| `internal/pack` | Archive extraction, subtitle normalization, member selection, and the on-disk pack cache |
| `internal/provider` | Provider interface and registry, the search coordinator and cache, the rate-limit gate, HTTP transport, cooldown persistence and availability preflight |
| `internal/provider/gestdown` | Gestdown adapter (TV, single episodes) |
| `internal/provider/opensubtitles` | OpenSubtitles.com adapter with exact file-hash search |
| `internal/provider/subdl` | SubDL adapter with season packs and direct members |
| `internal/provider/subsource` | SubSource adapter with title resolution and season packs |
| `internal/provider/titlovi` | Titlovi Kodi API adapter |
| `internal/schedule` | Missing, failure and upgrade delays, turned into search completions |
| `internal/selection` | Parses and normalizes typed titles (`Show S01E02`, `Movie (2019)`) for media selection |
| `internal/store` | SQLite persistence and embedded migrations: media, searches, leases, installations, provider state, caches, webhooks, notifications, rejections, history |
| `internal/subtitleorder` | Sorts cues and applies timing repairs on raw records without reserializing |
| `internal/syncer` | Runs LAPSE with time limits, output limits and the speech-profile cache; capability checks for `doctor` |
| `internal/testutil` | Test helpers, such as a controllable clock |
| `internal/version` | The build version (set with `-ldflags`) |
| `internal/watermark` | Strips subtitle-site advert cues from SRT payloads |
| `internal/worker` | The background loop: reconciliation, search leasing and workflows, notification delivery, route gate, media breaker, shutdown |
| `internal/workflow` | The per-language acquisition pipeline: tiers, search, scoring, packs, language check, LAPSE, installation, upgrades, removal, rollback and notification intents |

Dependency direction: `app` wires everything. `worker` drives `workflow` and `catalog`; `workflow` uses `inventory`, `match`, `pack`, `provider`, `syncer`, `langcheck`, `watermark`, `notifier` and `store`. `domain`, `langcheck`, `selection`, `subtitleorder`, `testutil`, `version` and `watermark` import no other subsyncd package; `config`, `match` and `observability` import only `domain`.

## Writing these pages

- Describe current behavior in the present tense. Git history records how it got here.
- State each schedule or limit in one place and link to it; [Scheduling](providers/scheduling.md) owns retry schedules.
- Name the code (`package.Function`, file) behind a rule so readers can verify it.
- Use the [glossary](providers/README.md#glossary) terms.
