# Full-library discovery

Status: implemented and locally verified; publication authorized. User requested full existing-library discovery for both Sonarr and Radarr.

Each configured instance receives one background discovery pass after upgrading or first being added. Explicit `scan --instance NAME` repeats discovery. Constructors, readiness and diagnostics stay offline. Successful history reconciliation remains responsible for ongoing changes; discovery does not advance its cursor.

Discovery enumerates Radarr movies and Sonarr series/episode files through arrapi v2.0.5, checks scope before detail hydration, and reuses existing complete media hydration. Fileless media is skipped. IDs and returned attachment identities must agree. Multi-episode files retain their terminal unsupported state. Cancellation and invalid/incomplete transport responses abort the pass.

Persistence is additive: insert missing stable entity/file identities and their missing-priority language searches atomically, preserving all existing rows including retained deletions, leases, schedules, rejection records and installations. Never infer deletions from snapshot absence. Empty successful libraries count as completed. A migration persists completion per instance and configured discovery scope, so existing installations also receive a first pass; changed mappings/roots trigger a new pass.

Concurrent webhooks must not be overwritten or resurrect unknown deleted entities. A per-instance event revision, advanced transactionally on event insertion, fences the fetched snapshot: capture before enumeration, compare in the commit transaction, retry on change without marking completed. This conservatively requires a quiet enumeration window; it avoids stale publication. Existing history backoff handles discovery failures. No provider or media content work occurs until durable search work is available.

No production action, commit, push or deployment is part of implementation. Tests use local fakes and synthetic media. Acceptance: both histories empty yet libraries discovered; fileless/outside/multi handling; retries and restart completion; stable existing schedules and provenance; concurrency fence; explicit rescan; offline assembly; full race, vet and tagged E2E verification.
