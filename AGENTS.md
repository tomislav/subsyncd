# subsyncd agent guide

Keep this file short. It holds instructions for coding agents and the repository-wide safety invariants. Everything a human contributor needs (setup, building from source, tests, CI, releases, commit conventions and working rules) is in [CONTRIBUTING.md](CONTRIBUTING.md); follow it.

## Before you change code

Read the pages relevant to the change:

- [docs/architecture.md](docs/architecture.md) for component boundaries, durable state and the processing flow.
- [docs/development/README.md](docs/development/README.md) for the developer page index and the package map, then the page for the area you touch:
  - [persistence.md](docs/development/persistence.md) before changing the schema or adding a migration;
  - [reconciliation.md](docs/development/reconciliation.md), [queue-and-control.md](docs/development/queue-and-control.md), [installation-safety.md](docs/development/installation-safety.md) or [notifications.md](docs/development/notifications.md) for runtime behavior;
  - [providers/](docs/development/providers/README.md) for adapters, matching, limits, scheduling, rejections and LAPSE.
- [docs/development/logging.md](docs/development/logging.md) before adding or changing an event, its fields or level, or redaction.
- [docs/references/silo.md](docs/references/silo.md) before changing Silo notifications.
- The operator docs ([docs/README.md](docs/README.md): `docs/running.md`, `docs/commands.md`, `docs/configuration.md`, `docs/troubleshooting.md`, `docs/upgrade-and-backup.md`, `docs/providers.md`, `docs/logging.md`) and `README.md` when changing user-visible behavior or configuration.

## Agent rules

- Follow the working rules, [Changes and reviews](CONTRIBUTING.md#changes-and-reviews) and commit conventions in [CONTRIBUTING.md](CONTRIBUTING.md#working-rules); the commit hook enforces the subject format.
- When you delegate or hand back work: state behaviour as a timeline or example, include the user's request verbatim in review briefs, and treat any interpretation or deviation an implementer reports as a question to resolve (with the user if it changes requested behaviour) before reviewing or shipping.
- Before inspecting, debugging, deploying or otherwise touching a production installation, read `.agents/production.local.md` completely when it exists. It is an ignored, host-specific operator file. Never commit it, require it in a clean clone, copy secrets from it, or guess production details when it is absent. `.agents/production.example.md` is only the tracked schema.
- Never run the `provider_contract` tests or contact real Arr, provider or Silo services unless the user asks.

## Safety invariants

- SQLite is authoritative for media state, search scheduling, leases, reconciliation cursors, provider availability, rejection state, installation provenance and the notification outbox. Related state transitions must remain atomic and owner-guarded.
- Canonical language identities are BCP 47 tags. Each language owns its configured provider order; preferred and fallback tiers must not be flattened into a global route.
- Media identity distinguishes the stable Arr entity from the replaceable physical file. Probe, hash, candidate, rejection and installation evidence must remain bound to the exact file fingerprint and be invalidated safely on replacement or deletion.
- Startup and readiness are local and offline. They must not require Arr, provider or Silo access. Mutating commands hold the data-directory lock; diagnostic commands remain read-only and do not migrate or initialize storage.
- Migrations are forward-only and run no network request or startup backfill.
- Provider-derived identities, cache entries, provenance, errors and logs must be credential-free. HTTP concurrency permits cover the entire response-body lifetime, and every response body is closed on every path.
- Downloads are bounded and archive extraction fails closed on traversal, links, nested archives, decompression limits, invalid subtitle syntax or ambiguous episode selection. Never choose an arbitrary archive member.
- LAPSE is the only synchronization engine. Invoke it with argument arrays and its strict JSON/output contract, preserve cancellation, and never expose raw subprocess output. Deterministic candidate failures may be retained; network, process, protocol, filesystem and cancellation failures normally remain retryable technical failures.
- Every subtitle write stays inside a configured media root, is syntax-validated, staged beside its destination, fsynced, atomically published, checksum-recorded, and transactionally tied to provenance and notification intent. Protect user-owned or changed sidecars and restore managed replacements if the database commit fails.
- Scratch work belongs in a private system-temporary directory. Final installation staging stays media-local and pack-cache publication stays cache-local. Never follow cache symlinks or delete outside a validated cache layout.
- Webhooks remain authenticated and bounded, redact query tokens, and expose generic errors. Reconciliation and discovery must fence concurrent events and must not infer deletions from an incomplete or omitted catalog snapshot.
- Structured logs are synchronous NDJSON on stderr. Never log credentials, provider bodies, raw LAPSE output, command lines, absolute paths, or unsanitized user or media data.
- Silo notifications use only the documented versioned contract, reject redirects, map paths at component boundaries, and are delivered asynchronously from the durable outbox after installation commits.
- The supported database lineage starts at `001_baseline.sql`. Do not silently adopt or convert unsupported pre-release schemas.
- Keep the root and release Dockerfiles aligned across supported architectures. Preserve the rootless, read-only runtime, writable-mount boundaries, pinned toolchain and runtime inputs, and the full two-window shutdown budget.
