# Codebase Correctness Repairs Design

**Status:** Implemented and locally verified

## Purpose

Repair the remaining correctness gaps identified by the 2026-09-05 conventional
code review while preserving subsyncd's provider economy, fail-closed media
handling, transactional installation guarantees, and asynchronous notification
delivery.

The current tree already resolves two findings from the review: Arr history is
reconciled by stable entity identity through arrapi, and OpenSubtitles episode
identity uses parent-series identifiers. OpenSubtitles also preserves its
`foreign_parts_only` evidence. This design covers only the remaining behavior:

- prevent an unusable exact-hash result from hiding later exact or broad
  candidates;
- parse season-pack ranges conservatively so release suffixes such as `1080p`
  cannot become episode endpoints;
- atomically record a committed installation and its notification intents;
- apply forced-only policy to every archive shape, including a single movie
  subtitle;
- reacquire a managed subtitle that has been deleted from disk;
- reject trailing YAML documents;
- support Silo path mappings whose local or remote endpoint is `/`;
- retain provider concurrency permits until response bodies are consumed or
  closed.

No production deployment, provider request, or production mutation is part of this
work.

## Selected approach

Keep the existing subsystem boundaries, but make workflow orchestration own the
two acquisition phases and make the repository own the installation/outbox
transaction. The provider coordinator will return candidates rather than make
workflow policy decisions, archive selection will use explicit evidence only,
and the HTTP transport will transfer permit ownership to the response body.

The changes are deliberately narrow. They do not introduce a new queue, scoring
model, provider API, database migration, or notification delivery protocol.

## Exact-first acquisition without candidate starvation

### Problem

The coordinator currently stops after the first exact-hash candidate. Policy
checks, deterministic rejection records, archive selection, and downloads occur
later in the workflow. If that one candidate is forced-only, previously
rejected, malformed, or contains the wrong archive member, valid later exact
results and the broad-search fallback are never considered correctly.

### Search phases

Make `provider.SearchQuery.Mode` authoritative:

- `SearchExactHash` asks only providers capable of exact hash lookup and returns
  every exact candidate in configured provider order;
- `SearchBroad` performs the existing broad provider searches and returns their
  merged candidates in configured provider order;
- the coordinator no longer invokes both modes inside one call and no longer
  collapses an exact phase to its first result.

Duplicate candidates continue to collapse by `(provider_id, result_id)` using
the existing evidence-merging rules. Cache behavior remains phase-aware. An
exact-mode result that does not actually claim the queried file hash is not
promoted to exact and cannot enter the exact list.

The workflow explicitly runs the phases:

1. refresh live subtitle inventory and resolve the current installation state;
2. search exact-capable providers;
3. apply language, media identity, forced/hearing-impaired, rejection, and
   archive-evidence gates before downloading where the available metadata makes
   that possible;
4. try eligible exact candidates sequentially in provider/result order and stop
   after the first committed installation;
5. if every exact candidate is absent or unusable, run the broad search and the
   existing score-tier/LAPSE tournament;
6. stop after the first successful installation.

Exact candidates are not limited by the broad-search top-three shortlist. They
are cheap to validate when metadata is sufficient and are downloaded lazily,
one at a time, so later alternatives remain reachable without spending all
provider download quota up front.

### Failure and provider-state semantics

A deterministic candidate-local failure, including forced-only content,
invalid payload, or wrong archive member, is recorded under the existing
rejection policy and advances to the next exact candidate. It does not open a
provider cooldown. A technical failure remains technical and is not persisted
as a candidate rejection.

Broad search still runs after deterministic exact-candidate exhaustion. It also
runs when the exact phase has only provider-local technical failures and at
least one provider remains able to perform broad search. A successful later
phase must not be poisoned by an earlier error from the same provider. If no
candidate installs, the final outcome uses the existing precedence:

- provider-wide throttling returns the earliest safe retry time;
- technical-only exhaustion returns an error and advances failure backoff;
- deterministic-only or empty exhaustion returns rejected/missing and advances
  missing-result backoff.

Logs identify `exact` or `broad` as the search phase using bounded fields. They
must not include download references, provider URLs, release names at info, or
absolute media paths.

## Conservative archive-member selection

### Explicit episode ranges

Accept a range only when both its start and endpoint are explicit episode
tokens separated by a hyphen:

- `S01E01-E03`;
- `S01E01-S01E03`, when both seasons match;
- `1x01-1x03`, when both seasons match.

Reject reversed and cross-season ranges. A dot, underscore, whitespace, or
ordinary release separator is never enough to create a range. In particular,
`S01E01.1080p`, `S01E01.2024`, and similar suffixes represent only episode 1.
Ambiguous forms such as `S01E01 E03` fail closed rather than expanding the
range.

Existing unambiguous single-episode, absolute-number, provider-supplied member,
and strict title evidence remains valid.

### Forced-only members

Every selected subtitle member passes the same forced-only policy, regardless
of archive shape. Candidate-level hearing-impaired policy remains applied before
download. Remove the movie/plain
single-member shortcut that directly takes `manifest.Members[0]`; use one
selection path that inspects provider metadata and bounded filename evidence
before returning a member.

A single forced-only movie subtitle cannot satisfy a normal full-language
request. Candidate-local rejection continues to the next exact candidate or
the next broad tournament option. This does not reject an entire season pack
when only one member is unsuitable.

## Atomic installation and notification outbox

### Transaction boundary

The filesystem publish and SQLite record remain one logical installation:

1. stage, validate, fsync, and atomically publish the sidecar while retaining
   the existing rollback copy when replacing a managed file;
2. begin one SQLite transaction;
3. upsert the installation/provenance record;
4. insert one checksum-deduplicated notification intent for every configured
   notifier;
5. commit the transaction;
6. remove the rollback copy only after commit.

If either the installation upsert or an outbox insert fails, roll back SQLite
and use the existing filesystem rollback path. Rollback errors remain joined
with the initiating error and the retained copy stays available after an
incomplete restoration.

The existing `notifications` table already models the outbox, leases, attempts,
and retry time, so no schema migration is required. Add a repository operation
that records the installation and its notification intents in one transaction.
The installer receives the configured notifier names and creates the durable
intents through that operation.

Remote delivery remains asynchronous. Once the transaction commits, a later
Silo outage, authentication failure, or retry exhaustion never removes or rolls
back the installed subtitle. This distinguishes a local inability to durably
schedule notification—which fails the installation—from a remote delivery
failure after durable scheduling—which does not.

### All installation entry points

Remove the worker's post-install notification enqueue. Daemon work, webhook
work, retries, and manual `search` all use the same assembled installer and
therefore obtain identical outbox behavior. Notification checksum deduplication
prevents replay from creating duplicate delivery work.

Search-lease completion still occurs only after the installation/outbox
transaction commits. Notification workers keep their independent leases and
retry schedule.

## Reacquiring a deleted managed subtitle

Live sidecar inventory is authoritative for file presence. Stored installation
provenance is active only when its sidecar exists and its checksum still
matches. When inventory finds no satisfying sidecar:

- a stale installation row must not produce `satisfied`;
- its prior exact-hash or score must not block the same candidate as a
  non-upgrade;
- candidate selection treats the run as a first installation;
- a successful replacement atomically overwrites the stale provenance and
  schedules notification normally.

The stale row may remain in SQLite until replacement; it is historical evidence,
not proof of a present sidecar. This avoids a destructive pre-search database
mutation and preserves auditability when providers are unavailable.

An existing sidecar whose checksum differs from service-owned provenance remains
protected as user-owned/untracked content under the current inventory rules. It
must not be overwritten merely because the old installation row is stale.

## Configuration document validation

Validate YAML document cardinality on the original byte stream before
environment expansion or any marshal/unmarshal round trip:

1. decode the first document into a YAML node;
2. attempt to decode a second document;
3. reject any non-empty second document and any later document;
4. only then expand allowed environment placeholders and perform the existing
   strict known-fields decode.

This prevents a parser round trip from silently discarding trailing documents.
Empty trailing separators may be accepted only if the YAML decoder represents
them as empty documents; they must not change the loaded configuration.

Configuration errors remain secret-redacted and identify the problem as
multiple YAML documents without echoing document contents.

## Root-aware Silo path mapping

Normalize path-mapping endpoints without converting `/` to an empty string.
Root is a valid mapping endpoint on either side:

- local `/media` to Silo `/` maps `/media/Movies/A/file.mkv` to
  `/Movies/A/file.mkv`;
- local `/` to Silo `/mnt/media` maps `/Movies/A/file.mkv` to
  `/mnt/media/Movies/A/file.mkv`.

Continue to select the longest matching local prefix and require a path-component
boundary, so `/media` does not match `/media2`. Join the suffix with the remote
root using path semantics that preserve a leading slash. The notification still
sends the mapped media file's parent directory to Silo's native scan endpoint.

Traversal, a non-absolute endpoint, an escaped result, or an otherwise unsafe
mapping fails closed without an HTTP request.

## Response-lifetime concurrency permits

Provider concurrency limits cover the complete response-body lifetime, not just
the wait for response headers. On a successful `Do` call, wrap `response.Body`
with an owner that releases both the provider-instance permit and the shared
origin permit exactly once when either:

- a read reaches EOF; or
- the caller closes the body early.

Use `sync.Once` so EOF followed by `Close`, repeated `Close`, and error cleanup
cannot double-release. Release immediately when the transport returns an error
or an unusable response without a body. Preserve the original body's read and
close errors.

All provider adapters remain responsible for closing bodies. A caller that
abandons a body without reading or closing it deliberately keeps the permit,
matching `net/http` ownership semantics and exposing the caller bug instead of
silently exceeding configured concurrency.

## Data flow

For one media/language job, the resulting flow is:

```text
live inventory
    |
    +-- satisfying embedded/sidecar track --> satisfied
    |
    +-- exact provider phase
    |       |
    |       +-- policy/rejection filters
    |       +-- lazy download + strict member selection
    |       +-- first valid result --> publish + installation/outbox transaction
    |
    +-- broad provider phase (only after exact exhaustion)
            |
            +-- score/rejection filters + top-three shortlist
            +-- LAPSE tier tournament
            +-- publish + installation/outbox transaction
```

Every provider request holds its concurrency permits until the response body is
consumed or closed. Every successful publish has either a committed installation
plus outbox intents or a completed/attempted filesystem rollback.

## Compatibility and migration

No database migration or configuration key is added. Existing installation and
notification rows remain valid. The repository API changes are internal, and
the worker-to-installer notification ownership change occurs in one release so
there is no mixed-version process contract.

Provider ordering, language routing, release scoring, the broad-search
top-three limit, LAPSE policy, notification retry policy, and the current Silo
API version remain unchanged. Existing exact-hash installations remain terminal
while their sidecar exists and matches provenance.

## Testing and acceptance

Implement each behavior with a focused red-green-refactor cycle and run the
affected package with the race detector before proceeding. Required regression
coverage includes:

- a forced, rejected, malformed, and wrong-member first exact result each allow
  a later exact candidate to install;
- exact exhaustion invokes broad search, while the first exact success prevents
  broad search and any later download;
- exact candidates are downloaded sequentially and duplicate provider results
  remain collapsed;
- technical and deterministic exact failures retain their distinct scheduling
  outcomes and do not create incorrect provider cooldowns;
- `S01E01.1080p` and year-like suffixes select only episode 1;
- all accepted explicit range forms select their bounded members, while
  reversed, cross-season, and ambiguous ranges fail closed;
- a forced-only single-member movie/plain payload is rejected and a later
  candidate can install;
- an injected outbox insert failure leaves neither installation nor notification
  rows and restores/removes the published sidecar;
- daemon and manual installations both create checksum-deduplicated outbox rows,
  and remote notification failure leaves the installation intact;
- deleting a managed sidecar causes the same exact candidate to be reacquired,
  while a modified present sidecar remains protected;
- a second non-empty YAML document is rejected before expansion, and a normal
  one-document configuration is unchanged;
- `/media -> /`, `/ -> /mnt/media`, exact-root, longest-prefix, and component
  boundary Silo mappings behave correctly;
- provider-instance and shared-origin permits block a second request until the
  first response reaches EOF or is closed, and release exactly once on every
  error/close path;
- existing provider, workflow, notification, configuration, Silo, daemon,
  command, and end-to-end tests remain green.

Final verification is:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/providercontract -tags=provider_contract -count=1
docker compose -f compose.example.yml config --quiet
git diff --check
```

Ordinary and tagged tests use only sanitized fixtures, fake local servers, and
temporary media. They must not contact real Arr instances, subtitle providers,
Silo, or production.

## Documentation and rollout

After implementation, update the feature/operations/provider documentation and
the contributor invariants to describe:

- exact-first sequential fallback;
- explicit archive ranges and universal forced-only filtering;
- installation/outbox atomicity and remote delivery independence;
- deleted-sidecar recovery;
- root Silo mappings;
- response-lifetime provider concurrency.

Update `docs/implementation-status.md` at each completed task boundary with the
commit, behavior, focused tests, and next safe task. Build or publication is not
part of correctness implementation. A later production rollout requires fresh
approval, a clean immutable image, offline `doctor`, and a bounded canary before
normal daemon work resumes.

## Non-goals

- changing release-score weights, LAPSE thresholds, or upgrade cadence;
- downloading several candidates in parallel;
- expanding the broad shortlist beyond three;
- adding new subtitle providers or notification targets;
- changing the Silo API version or guessing its future contract;
- converting or pruning historical installation rows before a replacement;
- adding a new UI, queue, or external service;
- deploying, starting, or modifying production infrastructure.
