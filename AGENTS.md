# subsyncd contributor guide

Keep this file short. It points contributors to the current documentation and records only repository-wide safety rules. Detailed subsystem behavior belongs in `docs/`, next to the audience it serves.

## Before you change code

Read the documents relevant to the change:

- `docs/architecture.md` for component boundaries, durable state, and the acquisition pipeline.
- `docs/development/providers.md` for providers, scoring, caches, cooldowns, fallback tiers, LAPSE, and upgrade scheduling.
- `docs/development/operations.md` for persistence, reconciliation, webhooks, filesystem publication, migrations, locking, and recovery.
- `docs/development/logging.md` before changing event fields, levels, redaction, or Loki guidance.
- `docs/references/silo.md` before changing Silo notifications.
- `README.md`, `docs/providers.md`, `docs/operations.md`, and `docs/logging.md` when changing user-visible behavior or configuration.

Before inspecting, debugging, deploying, or otherwise interacting with a production installation, read `.agents/production.local.md` completely when it exists. It is an ignored, host-specific operator file. Never commit it, require it in a clean clone, copy secrets from it, or guess production details when it is absent. `.agents/production.example.md` is only the tracked schema.

## Development workflow

- Use test-driven development: add one focused failing test, confirm the expected failure, implement the smallest production change, then run the affected package with `-race`.
- Ordinary tests must use sanitized fixtures and local fake servers. They must never contact Arr applications or subtitle providers.
- Keep user documentation and the appropriate development reference in sync with behavior changes. Do not add task ledgers, completed plans, review transcripts, or implementation-status files; Git history is the record of completed work.
- Preserve unrelated working-tree changes and never include secrets, production paths, provider responses, or real media metadata in fixtures, logs, commits, or documentation.

## Safety invariants

- SQLite is authoritative for media state, search scheduling, leases, reconciliation cursors, provider availability, rejection state, installation provenance, and the notification outbox. Related state transitions must remain atomic and owner-guarded.
- Canonical language identities are BCP 47 tags. Each language owns its configured provider order; preferred and fallback tiers must not be flattened into a global route.
- Media identity distinguishes the stable Arr entity from the replaceable physical file. Probe, hash, candidate, rejection, and installation evidence must remain bound to the exact file fingerprint and must be invalidated safely on replacement or deletion.
- Startup and readiness are local and offline. They must not require Arr, provider, or Silo access. Mutating commands hold the data-directory lock; diagnostic commands remain read-only and do not migrate or initialize storage.
- Provider-derived identities, cache entries, provenance, errors, and logs must be credential-free. HTTP concurrency permits cover the entire response-body lifetime, and every response body is closed on every path.
- Downloads are bounded and archive extraction fails closed on traversal, links, nested archives, decompression limits, invalid subtitle syntax, or ambiguous episode selection. Never choose an arbitrary archive member.
- LAPSE is the only synchronization engine. Invoke it with argument arrays and its strict JSON/output contract, preserve cancellation, and never expose raw subprocess output. Deterministic candidate failures may be retained; network, process, protocol, filesystem, and cancellation failures normally remain retryable technical failures.
- Every subtitle write stays inside a configured media root, is syntax-validated, staged beside its destination, fsynced, atomically renamed, checksum-recorded, and transactionally tied to provenance and notification intent. Protect user-owned or changed sidecars and restore managed replacements if the database commit fails.
- Scratch work belongs in a private system-temporary directory. Final installation staging stays media-local and pack-cache publication stays cache-local. Never follow cache symlinks or delete outside a validated cache layout.
- Webhooks remain authenticated and bounded, redact query tokens, and expose generic errors. Reconciliation and discovery must fence concurrent events and must not infer deletions from an incomplete or omitted catalog snapshot.
- Structured logs are synchronous NDJSON on stderr. Never log credentials, provider bodies, raw LAPSE output, command lines, absolute paths, or unsanitized user/media data.
- Silo notifications use only the documented versioned contract, reject redirects, map paths at component boundaries, and are delivered asynchronously from the durable outbox after installation commits.
- The supported database lineage starts at `001_baseline.sql`. Do not silently adopt or convert unsupported pre-release schemas.
- Keep the root and release Dockerfiles aligned across supported architectures. Preserve the rootless, read-only runtime, writable-mount boundaries, pinned toolchain/runtime inputs, and the full two-window shutdown budget.
