# Arrapi Stable-Identity Reconciliation Design

**Status:** Implemented and locally verified

The original full-library-discovery non-goal is superseded by [full-library discovery](2026-09-08-full-library-discovery-design.md). Startup assembly remains offline; background discovery is separate from history cursor reconciliation.

## Purpose

Replace subsyncd's hand-written Sonarr/Radarr history transport and inaccurate
history DTOs with `github.com/cplieger/arrapi/v2`, then repair reconciliation
around the identities the Arr APIs actually guarantee. Radarr history identifies
a movie with `movieId`; Sonarr history identifies an episode with `episodeId`.
Neither service guarantees a top-level historical file ID, and file-deletion
records do not retain the deleted file ID.

The change must stop the production reconciliation failure, recover imports, renames,
upgrades, and deletions without scanning the complete Arr library, preserve the
existing webhook and SQLite transaction guarantees, and keep unrelated media
outside an intentionally narrow canary mapping.

## Selected approach

Persist two distinct identities for every media row:

- `entity_id`: the stable Arr catalog entity (`movieId` or `episodeId`);
- `file_id`: the replaceable physical Arr movie-file or episode-file identity.

Use arrapi for authenticated Sonarr/Radarr history, entity lookup, retry,
response bounding, redirect policy, and log-safe status errors. Keep arrapi
types inside the catalog package and translate them to subsyncd domain values.

Arrapi intentionally carries only a curated file-metadata subset. It does not
currently expose every field subsyncd uses for release scoring and provenance,
including the complete quality, edition, runtime, and date-added structures.
Therefore the existing hardened detail request remains temporarily as a narrow
enrichment client for `/moviefile/{id}`, `/movie/{id}`, `/episodefile/{id}`,
`/episode?episodeFileId=...`, and `/series/{id}`. It no longer reads history or
decides reconciliation identity. Removing this enrichment client requires the
missing fields to land in a released arrapi version with equivalent tests.

Two alternatives are rejected:

- A separate entity-to-file mapping table duplicates the media lifecycle and
  makes atomic upgrades and deletions harder to reason about.
- Inferring a deleted file ID from adjacent history records fails whenever the
  matching import is older than the persisted cursor.

## Dependency and toolchain

Pin the reviewed `github.com/cplieger/arrapi/v2` release `v2.0.5` and record its
checksum in `go.sum`; do not track its branch. That release requires Go 1.27.1,
so update the module, Docker build argument, GitHub verification workflow, and
release documentation from Go 1.27.0 to Go 1.27.1 together. Do not rely on an
implicit toolchain download inside the container build.

The adapter supplies subsyncd's structured logger to arrapi and configures a
15-second request timeout, preserving the current catalog timeout. Retryable
429, 5xx, and transient transport failures use arrapi's bounded retry behavior.
No Arr API key, absolute path, URL, or upstream response body may enter normal
subsyncd logs. The adapter inspects arrapi's typed errors and synthesizes a
subsyncd-owned error containing only the instance, operation, HTTP status or
transport class, and retryability. It must not wrap or format arrapi's captured
response body or request URL into the surfaced error.

## Domain and storage model

Add `EntityID int64` to `domain.Media`. `MediaRef` remains instance, kind, and
file ID because workflows, installations, hashes, and webhook file events act
on one physical file.

Migration `010_media_entity_ids.sql` adds:

```sql
ALTER TABLE media ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX media_entity_identity_idx
    ON media(instance, kind, entity_id)
    WHERE entity_id > 0;
```

Zero is permitted only for rows created by older subsyncd versions. New or
hydrated live media must carry a positive entity ID. Reads and every direct or
transactional media upsert round-trip the field.

An upsert with a positive entity ID first locates the existing row by stable
entity identity and falls back to file identity only to adopt a legacy row. A
new Arr file ID for the same entity updates that row in place. Existing content
fingerprint rules then invalidate stale candidate and installation provenance,
schedule configured languages, and preserve active-lease/rerun semantics. This
prevents a movie or episode upgrade from creating a second logical media row.

The repository gains a stable-identity lookup used by reconciliation. It
returns an explicit not-found result rather than treating absence as failure.
Conflicting entity and file identities fail the complete transaction.

The existing unsupported multi-episode rule remains deterministic. Sonarr
detail hydration sorts every episode attached to a file and uses the earliest
episode ID as that row's canonical `entity_id`, while retaining the complete
attached-ID set only inside the catalog operation. A history event addressed to
another attached episode may hydrate that same unsupported file, but the
adapter must prove membership and canonicalize the present change to the
earliest ID before final reduction. When Sonarr emits deletion rows for every
attached episode, the canonical row deletes the stored media and the remaining
rows become harmless unknown audits. No combined-episode provider or LAPSE work
is enabled.

Legacy rows are upgraded lazily when a webhook, manual search, or reconciled
live state hydrates them. There is no automatic full-library backfill and no
startup network burst. Delete webhooks continue to identify legacy rows by file
ID. A historical deletion for a still-unadopted legacy row cannot be matched
from the Arr contract alone; it is audited as unknown and leaves the row
unchanged. This one-time compatibility limitation is documented. It disappears
for every row after its first successful hydration under the new version.

## Catalog contract

Change history reduction from physical file identity to stable entity identity.
A catalog history change contains:

- positive Arr history record ID;
- positive stable entity ID;
- normalized import, rename, or delete event;
- UTC event timestamp;
- authoritative current in-scope media when the entity presently has a file;
- otherwise an explicit absent or out-of-scope state.

The public catalog interface continues to expose domain-owned values; neither
arrapi concrete clients nor DTOs escape `internal/catalog`. Production
constructors create arrapi Sonarr/Radarr clients. Package-private constructors
accept narrow interfaces so tests use local fake HTTP servers or deterministic
fakes without contacting real Arr services.

Relevant arrapi history records are validated before reduction. A recognized
record with a missing history ID, entity ID, or timestamp fails closed and does
not advance the cursor. Unknown history event types are ignored but remain
available for bounded debug diagnostics through arrapi's raw-event field.

Records are ordered by timestamp then history ID and collapsed by entity ID.
For every changed entity, query current authoritative state once:

- Radarr: `MovieByID(movieId)` and its current `MovieFile`;
- Sonarr: `EpisodeByID(episodeId)` and its current `EpisodeFile`.

When a current file exists, hydrate the complete media through the narrow detail
client using that current file ID. The resulting change is an upsert even if
the last history row was a deletion, because current Arr state wins over an
intermediate upgrade tombstone. A current rename remains a rename for audit;
other present states normalize to import.

After current-state hydration, present Sonarr changes are collapsed once more
by canonical entity ID. This removes duplicate history work when several
episode-addressed records resolve to the same unsupported multi-episode file.

When no current file exists, return an absent entity change. The reconciler
emits an entity-addressed delete, and the repository resolves its previously
stored file inside the reconciliation transaction. An unknown entity produces
an idempotent audit without deleting another row.

## Scope and path mapping

Current-state resolution must distinguish deliberately unconfigured media from
unsafe or broken paths. Introduce a typed outside-scope result for:

- a remote path that matches no configured mapping;
- a safely mapped path that is outside every configured media root.

Traversal, malformed absolute paths, filesystem inspection failures, and
symlink-resolution failures remain hard errors. An outside-scope current entity
is treated like absence only with respect to subsyncd's managed set: delete a
previously indexed entity if one exists, otherwise skip it. This lets the production
two-movie canary consume production Radarr history without reading or indexing
the rest of the movie library.

Reconciliation reports bounded counts for changed, applied, outside-scope, and
unknown entities. It never logs their paths or titles at info level.

## Cursor and transaction behavior

Capture `pageEnd` before requesting history. Arrapi serializes the `since`
parameter at RFC3339 second precision, so each request intentionally overlaps
the preceding cursor's fractional second. Stable event IDs make replay harmless.
Subsyncd also ignores records strictly later than `pageEnd`; they are consumed
by the next page.

The reconciler converts the reduced entity changes into one batch:

- current in-scope media becomes an import/rename mutation carrying `EntityID`;
- absent or outside-scope known media becomes a delete mutation using the
  stored current file reference;
- absent or outside-scope unknown media becomes an audit-only mutation.

Media changes, search scheduling, candidate/provenance invalidation, entity-ID
adoption, audit rows, and cursor advancement commit in one SQLite transaction.
Any malformed record, lookup conflict, hydration failure, unsafe path, database
failure, or cursor failure rolls back the whole page. Existing event IDs remain
`reconcile:<instance>:<history-id>`.

An unresolved entity-addressed deletion stores instance, kind, stable entity
ID, history ID, and event type with a zero file ID. Repository validation
allows a zero file ID only for a delete carrying a positive entity ID;
webhook/import/rename mutations still require a positive file reference. If the
entity lookup succeeds, the transaction records the resolved current file ID
and links the audit to its media row before completing searches. Replays remain
idempotent.

## Webhooks and manual commands

Webhook payload normalization remains file-oriented. Imports and renames call
`GetMedia`, which now assigns the stable entity ID from authoritative Arr data;
their upserts adopt legacy rows or update an entity to its replacement file.
Delete webhooks continue to operate immediately by their supplied file ID and
do not need an additional Arr request.

Manual `search` also hydrates and persists the stable entity ID. `scan` remains
history reconciliation plus optional embedded-subtitle reprobe; it does not
become a complete Arr library walk.

Startup and readiness stay offline. Constructing arrapi clients must not perform
network requests.

## Failure scheduling

The worker currently records a reconciliation attempt time only after success,
causing a persistent Arr/schema failure to retry on every recovery poll. Track
consecutive failures in memory and retry after 5 minutes, 15 minutes, 1 hour,
then 6 hours for every later failure. A success resets the failure count and
restores the normal six-hour interval. Every failed attempt retains the old
durable cursor. Process restart deliberately permits one immediate attempt;
persisting reconciliation failure state is out of scope. Webhook delivery
continues to provide the low-latency path during a reconciliation backoff.

## Testing and acceptance

All production changes use focused red-green-refactor cycles. Contract fixtures
must mirror real sanitized Radarr 6.3 and Sonarr 4.0 payloads: top-level stable
entity IDs, event-specific `data.fileId` only where actually emitted, and no
fabricated top-level movie/episode file IDs.

Required coverage includes:

- arrapi construction performs no request and preserves base-path URLs;
- Radarr and Sonarr history decode real import, rename, delete, unknown, and
  malformed records;
- import plus delete plus replacement import collapses by entity and resolves
  the authoritative current file;
- an old-file deletion after cursor advancement removes the stored entity's
  current physical row without requiring the deleted file ID;
- a new file ID for one entity updates one media row, invalidates content
  provenance, and preserves active lease/rerun invariants;
- a legacy zero-entity row is adopted by matching file ID;
- deliberate outside-scope history is skipped without hydration, inventory,
  provider, LAPSE, or media-file access;
- traversal and filesystem/symlink failures still fail the page;
- second-resolution overlap replays safely and future records are deferred;
- mixed mutations and cursor advancement remain atomic;
- failed reconciliation does not hot-loop every recovery poll;
- arrapi 429/5xx/transport failures remain retryable, bounded, and secret-safe;
- existing webhook, manual search, multi-episode, path mapping, inventory,
  scoring, LAPSE, installation, and daemon tests remain green.

Final verification is:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
docker compose -f compose.example.yml config --quiet
git diff --check
```

No real Arr or subtitle-provider request belongs in ordinary or tagged end-to-end
tests. production validation, image publication, and deployment require separate
explicit approval after the implementation is committed and locally verified.

## Documentation

Update `AGENTS.md`, `README.md`, `docs/architecture.md`, `docs/operations.md`,
`docs/release-notes.md`, and `docs/implementation-status.md` with the final
dependency boundary, stable entity/file identity distinction, cursor overlap,
outside-scope behavior, legacy adoption limitation, and reconciliation retry
policy. Record every implementation commit and verification command in the
handoff ledger.

## Non-goals

- Full-library Sonarr/Radarr snapshot polling or startup backfill.
- Automatic Arr webhook installation or modification.
- Replacing provider, scoring, inventory, LAPSE, or Silo behavior.
- Exposing arrapi types outside the catalog adapter.
- Sending file contents or provider traffic while reconciling outside-scope
  entities.
- Publishing an image, pushing GitHub, or changing the production deployment as part
  of the local implementation.
