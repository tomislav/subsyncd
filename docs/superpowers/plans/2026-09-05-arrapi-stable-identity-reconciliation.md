# Arrapi Stable-Identity Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace custom Arr history decoding with `arrapi/v2` and make reconciliation reliably track movies and episodes across file replacement and deletion.

**Architecture:** Keep `arrapi` private to `internal/catalog`: it supplies bounded/retried history and current-entity reads, while the existing hardened detail client temporarily enriches fields arrapi does not carry. Persist stable Arr `entity_id` separately from replaceable `file_id`; collapse history by entity, resolve current state once, and let the repository resolve entity-addressed deletes inside the same transaction that advances the cursor.

**Tech Stack:** Go 1.27.1, `github.com/cplieger/arrapi/v2` v2.0.5, SQLite migrations, `net/http/httptest`, structured `slog`, Docker BuildKit/GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-05-arrapi-stable-identity-reconciliation-design.md`

## Global Constraints

- Pin `github.com/cplieger/arrapi/v2` exactly at `v2.0.5`; do not depend on a branch or implicit Go toolchain download.
- Keep arrapi concrete types inside `internal/catalog`; provider, workflow, store, app, and CLI packages consume only subsyncd-owned domain types.
- `entity_id` is positive for newly hydrated media; zero exists only for legacy rows. `file_id` remains the physical-file identity used by hashes, inventory, workflows, installations, and webhooks.
- Startup/readiness remain offline. No constructor, migration, or readiness check may contact an Arr service.
- Arr errors and logs must omit API keys, URLs, absolute paths, and upstream response bodies.
- Deliberately unmapped media is outside scope; traversal and filesystem/symlink resolution failures remain hard errors.
- History requests overlap fractional-second cursors, filter records after the captured page end, and rely on stable reconciliation event IDs for replay safety.
- Reconciliation mutations, stable-ID adoption, deletes, audit rows, schedules, and cursor advancement remain one SQLite transaction.
- Ordinary and tagged end-to-end tests use sanitized fixtures and local fake servers only.
- Do not publish, push, or deploy to Hades in this plan.

---

### Task 1: Pin arrapi and establish the private client boundary

**Files:**
- Create: `internal/catalog/arrapi_client.go`
- Create: `internal/catalog/arrapi_client_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `Dockerfile`
- Modify: `.github/workflows/container.yml`
- Modify: `docs/release-notes.md`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces: `arrHistoryClient`, `sonarrEntityClient`, and `radarrEntityClient`, implemented by arrapi clients.
- Produces: `newSonarrEntityClient(instance, baseURL, apiKey string, events *observability.Emitter) (sonarrEntityClient, error)`, `newRadarrEntityClient(instance, baseURL, apiKey string, events *observability.Emitter) (radarrEntityClient, error)`, `arrRetryHandler`, and `safeArrAPIError(...)` for later catalog tasks.
- Consumes: existing instance name/base URL/API key and subsyncd logging privacy rules.

- [ ] **Step 1: Write failing private-boundary and constructor tests**

Add compile-time interfaces and tests that construct both clients against a local server without observing a request:

```go
type arrHistoryClient interface {
    HistorySince(context.Context, time.Time, ...arrapi.EventType) ([]arrapi.HistoryRecord, error)
}

type sonarrEntityClient interface {
    arrHistoryClient
    EpisodeByID(context.Context, int) (arrapi.Episode, error)
}

type radarrEntityClient interface {
    arrHistoryClient
    MovieByID(context.Context, int) (arrapi.Movie, error)
}

func TestArrapiConstructionIsOfflineAndAcceptsBasePath(t *testing.T) {
    requests := 0
    server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
    defer server.Close()
    if _, err := newSonarrEntityClient("sonarr-main", server.URL+"/sonarr", "secret", nil); err != nil {
        t.Fatal(err)
    }
    if _, err := newRadarrEntityClient("radarr-main", server.URL+"/radarr", "secret", nil); err != nil {
        t.Fatal(err)
    }
    if requests != 0 {
        t.Fatalf("constructor requests = %d, want 0", requests)
    }
}
```

Construct `&arrapi.StatusError{Code: 502, Path: "/api/v3/history/since", Body: "secret /media/private marker"}`. Assert `safeArrAPIError("radarr-main", "history", err).Error()` contains instance, operation, status, and retryability but none of the body strings or arrapi path. Invoke `arrRetryHandler.Handle` with those strings in the slog message and attributes; assert the emitted JSON contains only the mapped event and configured instance.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -run 'TestArrapiConstruction|TestSafeArrAPIError' -count=1
```

Expected: compilation fails because the arrapi dependency and private constructors do not exist.

- [ ] **Step 3: Add the dependency, toolchain patch, and minimal wrapper**

Set `go 1.27.1`, require `github.com/cplieger/arrapi/v2 v2.0.5`, run `go mod tidy`, update `ARG GO_VERSION=1.27.1`, and make the GitHub verification job use `go-version: "1.27.1"`.

Implement the constructors with `arrapi.WithTimeout(15*time.Second)` and a
`slog.New(arrRetryHandler{...})`. The handler discards every arrapi attribute
and message, then maps only record level to a subsyncd-owned event:

```go
// Debug records become arr.request_retry; Warn-or-higher records become
// arr.request_retries_exhausted. Both carry only the configured instance.
type arrRetryHandler struct {
    Events   *observability.Emitter
    Instance string
}
```

Implement all four `slog.Handler` methods. `WithAttrs` and `WithGroup` return
the same safe handler without retaining library fields. A nil emitter uses
`observability.Discard()`. This keeps retry diagnostics in structured logging
without allowing arrapi error bodies, paths, URLs, or messages around the
redaction boundary.

Implement a subsyncd-owned error:

```go
type arrAPIError struct {
    Instance   string
    Operation  string
    Kind       string
    StatusCode int
    Retryable  bool
}

func safeArrAPIError(instance, operation string, err error) error
```

Use `errors.As` for `*arrapi.StatusError` and `*arrapi.ResponseTooLargeError`; classify context cancellation/deadline and all other failures with bounded constants. Never include `err.Error()`, `StatusError.Body`, `StatusError.Path`, or the base URL in the returned text.

- [ ] **Step 4: Verify the focused package and module graph**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -run 'TestArrapiConstruction|TestSafeArrAPIError' -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go list -m github.com/cplieger/arrapi/v2
git diff --check
```

Expected: tests pass and the module output is exactly `github.com/cplieger/arrapi/v2 v2.0.5`.

- [ ] **Step 5: Record and commit the dependency boundary**

Update release notes and the implementation ledger with the exact
dependency/toolchain versions, the safe retry-log adapter, tests run, and next
task.

```bash
git add go.mod go.sum Dockerfile .github/workflows/container.yml internal/catalog/arrapi_client.go internal/catalog/arrapi_client_test.go docs/release-notes.md docs/implementation-status.md
git commit -m "chore: adopt bounded Arr API client"
```

---

### Task 2: Persist stable entity identity and update one row across upgrades

**Files:**
- Create: `internal/store/migrations/010_media_entity_ids.sql`
- Modify: `internal/domain/media.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/catalog/webhook.go`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `test/e2e/e2e_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces: `domain.Media.EntityID int64`.
- Produces: `Repository.FindMediaByEntity(ctx, instance, kind, entityID) (mediaID int64, media domain.Media, found bool, err error)`.
- Changes: `MediaEventMutation` gains `EntityID int64`; media upserts locate positive entity identity before file identity.
- Consumes: Task 1 only indirectly; this task contains no Arr request.

- [ ] **Step 1: Write failing migration and round-trip tests**

Add tests that open a database through migration 009, insert a legacy media/event row, reopen it, and assert both new columns are zero and migration count is 10. Add a domain round-trip test:

```go
func TestMediaEntityIDRoundTrips(t *testing.T) {
    repo := openTestRepository(t)
    media := testMedia()
    media.EntityID = 101
    id, _, err := repo.UpsertMedia(context.Background(), media)
    if err != nil { t.Fatal(err) }
    got, err := repo.GetMedia(context.Background(), id)
    if err != nil { t.Fatal(err) }
    if got.EntityID != 101 { t.Fatalf("entity ID = %d, want 101", got.EntityID) }
    foundID, _, found, err := repo.FindMediaByEntity(context.Background(), media.Ref.Instance, media.Ref.Kind, 101)
    if err != nil || !found || foundID != id {
        t.Fatalf("entity lookup = %d/%v/%v, want %d/true/nil", foundID, found, err, id)
    }
}
```

Add a test that imports entity 101 as file 1001, records candidate/installation provenance and leases a search, then imports entity 101 as file 1002. Assert:

- one row has `entity_id=101`;
- its `file_id` is 1002;
- content provenance is invalidated;
- the lease owner/expiry survives and `rerun_requested=1`;
- `FindMedia` for file 1001 fails and file 1002 returns the original media row ID.

Add a legacy-adoption test: insert a row with entity ID zero and file 1001 through raw SQL (the only supported source of a zero-ID row is an older schema), then hydrate entity 101 with the same file through `UpsertMedia`; assert the same media row is updated rather than duplicated.

- [ ] **Step 2: Run the storage tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store -run 'TestMediaEntityID|TestEntityUpgrade|TestLegacyEntity' -count=1
```

Expected: failures show the missing migration/field and current file-ID-only upsert behavior.

- [ ] **Step 3: Add schema and domain identity**

Create the exact migration:

```sql
ALTER TABLE media ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX media_entity_identity_idx
    ON media(instance, kind, entity_id)
    WHERE entity_id > 0;
```

Add `EntityID int64` to `domain.Media`. Extend all media
SELECT/INSERT/UPDATE paths and event INSERT/UPDATE paths to carry it. Extend
`MediaEventMutation` with `EntityID int64`; webhook hydration copies
`media.EntityID` into the mutation, and the transitional reconciler copies it
from present hydrated media until Task 4 makes it explicit on every history
change.

- [ ] **Step 4: Implement entity-first upsert and lookup**

Factor the shared direct/transactional lookup into a transaction-compatible helper:

```go
func findMediaIdentityTx(ctx context.Context, tx *sql.Tx, media domain.Media) (
    id int64, existingPath string, existingFileID, existingSize, existingModTime int64, found bool, err error,
)
```

Rules:

1. Reject newly upserted media with `EntityID <= 0`.
2. Query both `(instance, kind, entity_id)` and `(instance, kind, file_id)`.
3. If both exist and identify different rows, fail closed with `conflicting media identities`.
4. Prefer the entity row; otherwise adopt the file row only when its stored `entity_id` is zero.
5. Insert only when neither lookup exists.
6. Update `file_id` and `entity_id` together before applying existing fingerprint/provenance/search rules.

Implement `FindMediaByEntity` with explicit `(found=false, err=nil)` on `sql.ErrNoRows` and reject nonpositive inputs.

- [ ] **Step 5: Update fixtures and verify storage behavior**

Give every newly hydrated test media a positive entity ID; keep zero only in the named migration/adoption tests.

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/store ./internal/catalog ./internal/app -race -count=1
git diff --check
```

Expected: all tests pass; upgrade/adoption tests demonstrate one stable row.

- [ ] **Step 6: Record and commit stable persistence**

```bash
git add internal/domain/media.go internal/store/migrations/010_media_entity_ids.sql internal/store/repository.go internal/store/repository_test.go internal/catalog/webhook.go internal/catalog/webhook_test.go internal/catalog/reconcile.go internal/catalog/reconcile_test.go internal/app/app_test.go test/e2e/e2e_test.go docs/implementation-status.md
git commit -m "feat: persist stable Arr entity identity"
```

---

### Task 3: Make detailed hydration assign entity identity and classify scope

**Files:**
- Modify: `internal/catalog/pathmap.go`
- Modify: `internal/catalog/pathmap_test.go`
- Modify: `internal/catalog/radarr.go`
- Modify: `internal/catalog/radarr_test.go`
- Modify: `internal/catalog/sonarr.go`
- Modify: `internal/catalog/sonarr_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Produces: exported sentinel `ErrOutsideScope` and `IsOutsideScope(error) bool` inside `internal/catalog`.
- Changes: `Radarr.GetMedia` sets `Media.EntityID=file.MovieID`; `Sonarr.GetMedia` sets `Media.EntityID=selectedEpisode.ID` and its private hydration helper also returns every attached episode ID.
- Consumes: Task 2's positive `domain.Media.EntityID` invariant.

- [ ] **Step 1: Write failing hydration and scope tests**

Extend existing local-server tests to assert:

```go
if media.EntityID != 20 { // Radarr movieId
    t.Fatalf("entity ID = %d, want 20", media.EntityID)
}
if episodeMedia.EntityID != 101 { // Sonarr episodeId
    t.Fatalf("entity ID = %d, want 101", episodeMedia.EntityID)
}
```

Extend the multi-episode test so out-of-order episode IDs produce canonical
`EntityID=101`. Test the private Sonarr hydration result contains exactly
`[]int64{101, 102}` in sorted order; this set is catalog-operation evidence and
is not added to `domain.Media` or persisted.

Add table tests proving `errors.Is(err, ErrOutsideScope)` for a safe unmatched mapping and a safely mapped path outside roots. Assert traversal, relative output, inaccessible parent, and symlink-resolution failures do not match `ErrOutsideScope`.

- [ ] **Step 2: Run catalog tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -run 'TestRadarrGetMedia|TestSonarrGetMedia|TestMapPathOutsideScope' -count=1
```

Expected: entity assertions fail and no typed scope error exists.

- [ ] **Step 3: Implement the minimal identity and error classification**

Define:

```go
var ErrOutsideScope = errors.New("media is outside configured scope")

func IsOutsideScope(err error) bool { return errors.Is(err, ErrOutsideScope) }
```

Wrap only the two safe scope outcomes with `%w`: no boundary-aware mapping and a resolved mapped path outside all configured roots. Preserve existing hard errors for traversal, nonabsolute mappings, `Lstat`, `EvalSymlinks`, and root-resolution failure.

Set the stable entity ID in both hydrated media results and reject zero Arr entity IDs before returning them. Refactor Sonarr's existing detail path behind:

```go
func (s *Sonarr) hydrateMedia(ctx context.Context, ref domain.MediaRef) (domain.Media, []int64, error)
```

`GetMedia` returns only the first and third values. The entity slice contains
all attached episodes in the same deterministic order already used for display
selection; ordinary files therefore return one ID.

- [ ] **Step 4: Verify catalog and dependent packages**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog ./internal/store ./internal/app -race -count=1
git diff --check
```

- [ ] **Step 5: Record and commit hydration identity**

```bash
git add internal/catalog/pathmap.go internal/catalog/pathmap_test.go internal/catalog/radarr.go internal/catalog/radarr_test.go internal/catalog/sonarr.go internal/catalog/sonarr_test.go docs/implementation-status.md
git commit -m "feat: classify scoped Arr media identity"
```

---

### Task 4: Replace custom history DTOs with arrapi entity reconciliation

**Files:**
- Replace: `internal/catalog/history.go`
- Modify: `internal/catalog/catalog.go`
- Modify: `internal/catalog/radarr.go`
- Modify: `internal/catalog/sonarr.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `test/e2e/e2e_test.go`
- Create: `internal/catalog/testdata/radarr_history.json`
- Create: `internal/catalog/testdata/sonarr_history.json`
- Replace: fabricated inline history payloads in `internal/catalog/radarr_test.go` and `internal/catalog/sonarr_test.go`
- Modify: `internal/catalog/radarr_test.go`
- Modify: `internal/catalog/sonarr_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Changes: `Catalog.ListChanges(ctx context.Context, since, through time.Time) ([]HistoryChange, error)` replaces `ListChangesSince`.
- Changes: `HistoryChange` carries `EntityID int64`, `State HistoryState`, and `Media domain.Media` (zero-valued unless state is present), with no file-based reduction key.
- Produces: `HistoryPresent`, `HistoryAbsent`, and `HistoryOutsideScope`.
- Consumes: Task 1 clients/error mapping and Task 3 scope-aware hydration.

- [ ] **Step 1: Replace fixtures with real API shapes and write failing contract tests**

Use sanitized payloads shaped like the live APIs. Radarr example:

```json
{
  "id": 5418,
  "movieId": 400,
  "sourceTitle": "Example.Movie.2026.1080p.WEB-DL-GROUP",
  "eventType": "downloadFolderImported",
  "date": "2026-09-05T05:12:18Z",
  "data": {"fileId": "1531", "importedPath": "/remote/example.mkv"}
}
```

Radarr delete retains `movieId` but omits both top-level file ID and `data.fileId`. Sonarr import uses `episodeId`, `seriesId`, and `data.fileId`; Sonarr delete retains `episodeId`/`seriesId` but no file ID. Include one unknown event token and one malformed relevant record per adapter.

Tests must prove:

- the request uses `/api/v3/history/since` and an RFC3339 second timestamp;
- records after `through` are ignored before collapse;
- import/delete/import for one entity resolves the current replacement once;
- two present episode history entities resolving to one multi-episode file are
  membership-checked and collapsed to its canonical earliest episode ID;
- current no-file state returns `HistoryAbsent` without detail hydration;
- a safe unmapped current file returns `HistoryOutsideScope` without detail hydration;
- missing history ID/entity ID/date fails;
- raw unknown events are ignored;
- no test payload contains `movieFileId` or `episodeFileId` at history top level.

- [ ] **Step 2: Run both adapter suites and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -run 'Test(Radarr|Sonarr).*History' -count=1
```

Expected: old reducers fail because real fixtures provide only stable entity IDs.

- [ ] **Step 3: Implement entity-based reduction**

Define the subsyncd-owned contract:

```go
type HistoryState string

const (
    HistoryPresent      HistoryState = "present"
    HistoryAbsent       HistoryState = "absent"
    HistoryOutsideScope HistoryState = "outside_scope"
)

type HistoryChange struct {
    HistoryID  int64
    EntityID   int64
    Type       EventType
    State      HistoryState
    Media      domain.Media
    OccurredAt time.Time
}
```

Implement a reducer accepting `[]arrapi.HistoryRecord`, media kind, `through`, and an entity-ID selector. Filter to arrapi download-import, rename, and file-delete types; validate relevant records; discard `Date.After(through)`; sort by date/ID; retain the latest record for each entity.

- [ ] **Step 4: Wire current-state resolution into each adapter**

`Radarr` owns `entity radarrEntityClient`; `Sonarr` owns `entity sonarrEntityClient`. Production constructors create both the existing detail client and the Task 1 arrapi client without making a request. Add an `events *observability.Emitter` argument to both catalog constructors, pass the application emitter from `buildCatalogs`, and pass `nil` from isolated catalog tests. Update every `Catalog` test fake to implement the new `ListChanges(ctx, since, through)` signature.

For each reduced entity:

1. Call `MovieByID` or `EpisodeByID` once.
2. Treat arrapi 404 or `HasFile=false`/nil file as `HistoryAbsent`.
3. Validate positive current file ID.
4. Call `MapPath` on arrapi's current file path. Convert only `ErrOutsideScope` to `HistoryOutsideScope`; propagate every other error.
5. Hydrate full metadata with `GetMedia(currentFileID)`. Radarr requires exact
   entity equality. Sonarr calls `hydrateMedia`, requires the triggering
   episode ID to occur in its returned attached-ID set, and uses the media's
   canonical earliest `EntityID`; a mismatch is a consistency error.
6. Collapse hydrated present changes again by canonical entity ID so one
   unsupported multi-episode file produces one mutation.
7. Return `HistoryPresent`; retain rename only when the latest canonical record
   was rename, otherwise normalize the mutation type to import. Absent and
   outside-scope states always normalize to delete.
8. Map every arrapi failure through `safeArrAPIError`.

Delete `arrHistoryRecord`, `sonarrHistoryEvent`, `radarrHistoryEvent`, and every `MovieFileID`/`EpisodeFileID` assumption.

- [ ] **Step 5: Verify catalog contracts and race safety**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog -race -count=1
rg -n '"(movieFileId|episodeFileId)"' internal/catalog/testdata/radarr_history.json internal/catalog/testdata/sonarr_history.json
git diff --check
```

Expected: tests pass; `rg` exits 1 because neither history fixture contains a fabricated top-level file ID. Existing webhook fixtures legitimately retain their file objects.

- [ ] **Step 6: Record and commit arrapi history**

```bash
git add internal/catalog internal/app/app.go internal/app/app_test.go test/e2e/e2e_test.go docs/implementation-status.md
git commit -m "fix: reconcile Arr history by stable entity"
```

---

### Task 5: Resolve entity deletes atomically with the reconciliation cursor

**Files:**
- Modify: `internal/catalog/reconcile.go`
- Modify: `internal/catalog/reconcile_test.go`
- Modify: `internal/store/repository.go`
- Modify: `internal/store/repository_test.go`
- Modify: `test/e2e/e2e_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Changes: `ReconciliationStore.CommitReconciliation` continues as one call, but mutations may address a delete by positive `EntityID` with `Ref.FileID==0`.
- Consumes: Task 4 `HistoryChange.State` and Task 2 `MediaEventMutation.EntityID`.
- Produces: atomically resolved entity delete/audit behavior.

- [ ] **Step 1: Write failing repository tests for known and unknown entity deletes**

Seed entity 101/file 1001. Commit this reconciliation page:

```go
[]MediaEventMutation{
    {
        EventID: "reconcile:sonarr-main:42",
        Type: "delete",
        EntityID: 101,
        Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode},
        At: now,
        Priority: SearchPriorityMissing,
    },
}
```

Assert the transaction fills the audit's `entity_id=101`, `file_id=1001`, links its media row, and completes its searches as deleted. Repeat with entity 999 and assert an idempotent audit with `file_id=0`, no linked media, and no other media change.

Add rollback and identity-conflict cases: a valid entity delete followed by a mutation for another instance must leave the media/search/audit/cursor untouched.

- [ ] **Step 2: Write failing reconciler conversion tests**

Use one present, one absent, and one outside-scope `HistoryChange`. Assert present becomes an import/rename with current file ref and media; absent/outside become delete mutations with positive entity IDs and zero file IDs. Assert an invalid state fails before commit and `OnCommitted` fires only after success.

- [ ] **Step 3: Run focused tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog ./internal/store -run 'Test(Reconciler|CommitReconciliation).*Entity' -count=1
```

Expected: current validation rejects zero file IDs and cannot resolve deletes by entity.

- [ ] **Step 4: Implement transactional entity resolution**

Update event insertion to persist `mutation.EntityID`. Validation rules are exact:

- import/rename: positive entity ID, positive file ID, and `Media.EntityID == mutation.EntityID`;
- file-addressed delete webhook: positive file ID, entity ID optional;
- entity-addressed reconciliation delete: positive entity ID, file ID may be zero;
- every other zero file ID is invalid.

For an entity-addressed delete, query media by `(instance, kind, entity_id)` inside `applyMediaMutationTx`. If found, use its current file ID for the audit, link the event, and apply existing search completion. If absent, leave audit file ID zero. Never infer deletion from a transport or hydration failure.

Update `Reconciler.Run` to call `ListChanges(ctx, cursor, pageEnd)`, convert all states, and invoke `CommitReconciliation` once.

- [ ] **Step 5: Add/update the tagged integration path**

Drive a real arrapi-backed Sonarr fake through catalog reconciliation and SQLite:

- existing entity/file is seeded;
- deletion history omits file ID;
- `EpisodeByID` reports no current file;
- commit completes the known row and advances the cursor;
- an unrelated outside-scope entity produces no media read or search work;
- replay changes neither audit count nor state.

- [ ] **Step 6: Verify atomic behavior**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/catalog ./internal/store -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -run TestReconciliation -race -count=1
git diff --check
```

- [ ] **Step 7: Record and commit atomic entity reconciliation**

```bash
git add internal/catalog/reconcile.go internal/catalog/reconcile_test.go internal/store/repository.go internal/store/repository_test.go test/e2e/e2e_test.go docs/implementation-status.md
git commit -m "fix: resolve Arr deletions transactionally"
```

---

### Task 6: Back off failed reconciliation without moving its cursor

**Files:**
- Modify: `internal/worker/worker.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Replaces: success-only `lastReconcile map[string]time.Time` with per-instance in-memory attempt state.
- Produces: failure delays of 5m, 15m, 1h, then 6h; success restores the normal configured interval.
- Consumes: existing `Clock`, `ReconcileInterval`, and structured worker events.

- [ ] **Step 1: Write failing clock-driven backoff tests**

Add a reconciler whose first four calls fail and fifth succeeds. Call `RunOnce` while advancing the fake clock just before and at each boundary. Assert calls occur only at:

```text
12:00 failure 1
12:05 failure 2
12:20 failure 3
13:20 failure 4
19:20 success
```

Then assert the next call waits the configured six hours. Add a second instance that succeeds throughout to prove one instance's failure does not delay another.

Capture logs and assert `reconcile.failed` includes bounded `attempt` and `retry_at`, with no raw Arr body/path; successful completion resets attempt state.

- [ ] **Step 2: Run worker tests and verify RED**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker -run 'Test.*Reconcile.*(Backoff|Independent)' -count=1
```

Expected: the failure is retried on every `RunOnce`/recovery poll.

- [ ] **Step 3: Implement per-instance attempt state**

Define:

```go
type reconcileAttempt struct {
    LastAttempt time.Time
    Failures    int
}

var reconcileFailureDelays = [...]time.Duration{
    5 * time.Minute,
    15 * time.Minute,
    time.Hour,
    6 * time.Hour,
}
```

Store one state per instance under the existing reconciliation mutex. A failed call records `LastAttempt=now` and increments failures up to the last delay. A success records `LastAttempt=now` and zero failures. Due calculation uses `ReconcileInterval` after success and the indexed failure delay after failure. Do not alter the durable database cursor on failure.

- [ ] **Step 4: Verify worker and catalog integration**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./internal/worker ./internal/catalog -race -count=1
git diff --check
```

- [ ] **Step 5: Record and commit failure backoff**

```bash
git add internal/worker/worker.go internal/worker/worker_test.go docs/implementation-status.md
git commit -m "fix: back off failed Arr reconciliation"
```

---

### Task 7: Complete documentation and repository-wide verification

**Files:**
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `docs/architecture.md`
- Modify: `docs/operations.md`
- Modify: `docs/release-notes.md`
- Modify: `docs/implementation-status.md`

**Interfaces:**
- Consumes: every previous task's final behavior.
- Produces: durable operator/agent documentation and a release-ready local tree.

- [ ] **Step 1: Update the durable behavior contract**

Document all of the following explicitly:

- stable entity ID versus replaceable file ID;
- arrapi v2.0.5 ownership of history/current-entity API calls and the temporary narrow detail enrichment client;
- entity-based import/rename/upgrade/delete behavior;
- second-resolution cursor overlap and event-ID replay safety;
- outside-scope skip/delete behavior for narrow canaries;
- legacy zero-ID lazy adoption and its one-time missed-deletion limitation;
- 5m/15m/1h/6h reconciliation failure backoff;
- Go 1.27.1 build requirement;
- no startup network calls and no full-library backfill.

Change the design status to `Implemented` only after every verification below passes. Add the final plan/commit ledger and next safe action to `docs/implementation-status.md`.

- [ ] **Step 2: Run formatting and focused contract scans**

Run:

```bash
gofmt -w internal/catalog/arrapi_client.go internal/catalog/arrapi_client_test.go internal/catalog/catalog.go internal/catalog/history.go internal/catalog/pathmap.go internal/catalog/pathmap_test.go internal/catalog/radarr.go internal/catalog/radarr_test.go internal/catalog/reconcile.go internal/catalog/reconcile_test.go internal/catalog/sonarr.go internal/catalog/sonarr_test.go internal/catalog/webhook.go internal/catalog/webhook_test.go internal/domain/media.go internal/store/repository.go internal/store/repository_test.go internal/worker/worker.go internal/worker/worker_test.go internal/app/app.go internal/app/app_test.go test/e2e/e2e_test.go
! rg -n 'type arrHistoryRecord|MovieFileID|EpisodeFileID' internal/catalog
rg -n 'github.com/cplieger/arrapi/v2 v2.0.5' go.mod go.sum
! rg -n '1\.27\.0' go.mod Dockerfile .github/workflows/container.yml docs/release-notes.md
git diff --check
```

Expected: obsolete history symbols and Go 1.27.0 references are absent; the exact arrapi version is present.

- [ ] **Step 3: Run the full verification matrix**

Run:

```bash
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./... -race -count=1
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache go test ./test/e2e -tags=e2e -race -count=1
docker compose -f compose.example.yml config --quiet
git diff --check
```

Expected: every command exits zero. If a failure appears, add the smallest failing regression before changing production code, then rerun the affected package and the complete matrix.

- [ ] **Step 4: Perform a native container build and offline smoke check**

Run:

```bash
docker build --build-arg VERSION=arrapi-local -t subsyncd:arrapi-local .
docker run --rm subsyncd:arrapi-local --version
```

Expected: the build uses Go 1.27.1 without downloading another toolchain, and the command prints `subsyncd arrapi-local`.

- [ ] **Step 5: Review the implementation against the design**

Read the final diff and verify every spec section has code/tests/docs evidence. Confirm there are no changes to provider routing, scoring, LAPSE, subtitle files, Silo, automatic webhooks, GitHub publishing, or Hades deployment.

- [ ] **Step 6: Commit final documentation and verification record**

```bash
git add README.md AGENTS.md docs go.mod go.sum Dockerfile .github/workflows/container.yml internal test
git commit -m "docs: document stable Arr reconciliation"
```

- [ ] **Step 7: Report the local result and request separate deployment authority**

Report commits, exact arrapi/Go versions, tests/build output, remaining legacy limitation, and whether the worktree is clean. Do not push, publish, or change Hades without a new explicit request.
