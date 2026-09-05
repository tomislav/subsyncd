# Clean Schema Baseline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the pre-release ten-migration SQLite lineage with one strict current baseline, remove compatibility-only code, and prove a recoverable clean production canary rebuild that recreates the authorized Croatian sidecar through Titlovi and LAPSE.

**Architecture:** Keep the general embedded migration runner, but make `001_baseline.sql` the only accepted lineage and reject unknown applied migration names before executing SQL. Persisted media always has a positive stable Arr entity ID while physical `file_id`, event-local zero identities, fingerprints, queues, and audit semantics remain unchanged. production receives a fresh database and a clean three-file end-to-end replay; the old database and exact sidecar remain recoverable backups.

**Tech Stack:** Go 1.27.1, `modernc.org/sqlite`, embedded ordered SQL migrations, Docker BuildKit, GitHub Actions, Sonarr/Radarr v3 APIs, Titlovi, FFprobe, LAPSE v2.0.5.

**Spec:** `docs/superpowers/specs/2026-09-05-clean-schema-baseline-design.md`

## Global Constraints

- Old `001_initial.sql` through `010_media_entity_ids.sql` databases are unsupported; do not build a converter or lazy adapter.
- Retain `schema_migrations` and the embedded ordered migration runner for future schema evolution.
- `media.entity_id` must be positive; `file_id` remains the replaceable physical-file identity.
- `events.event_id` remains nullable and event identity columns may remain zero for current local/file-oriented audit cases.
- Startup, readiness, and schema migration remain offline and perform no Arr, provider, Silo, or media scan.
- Preserve fail-closed identity conflicts, transactional reconciliation, queue leases, fingerprints, rejection evidence, and notification behavior.
- Historical plans remain immutable records; update active README, architecture, operations, implementation-status, and `AGENTS.md` only.
- The production deployment scope is `/opt/subsyncd-daemon-canary` plus the exact S01E06 `.hr.srt`; no other service, media, subtitle, or database may be mutated.
- Back up the old canary database and exact sidecar before deleting either live target.
- Never print credentials, webhook tokens, provider bodies, raw subtitle text, or absolute outside-scope media paths.

---

### Task 1: Establish the single baseline and reject old lineages

**Files:**
- Create: `internal/store/migrations/001_baseline.sql`
- Delete: `internal/store/migrations/001_initial.sql`
- Delete: `internal/store/migrations/002_catalog_events.sql`
- Delete: `internal/store/migrations/003_media_hashes.sql`
- Delete: `internal/store/migrations/004_episode_titles.sql`
- Delete: `internal/store/migrations/005_installation_fingerprints.sql`
- Delete: `internal/store/migrations/006_notification_leases.sql`
- Delete: `internal/store/migrations/007_candidate_rejections.sql`
- Delete: `internal/store/migrations/008_search_priorities.sql`
- Delete: `internal/store/migrations/009_media_unsupported_reason.sql`
- Delete: `internal/store/migrations/010_media_entity_ids.sql`
- Modify: `internal/store/store.go:52-98`
- Modify: `internal/store/repository_test.go:14-65,1356-1389`

**Interfaces:**
- Consumes: the existing `Store.migrate(context.Context) error` entry point and embedded `migrationFiles`.
- Produces: accepted lineage `001_baseline.sql`; `validateAppliedMigrations(context.Context, *sql.DB, map[string]struct{}) error`; explicit error text `unsupported database migration %q; rebuild from an empty data directory`.

- [ ] **Step 1: Replace migration-upgrade tests with failing baseline/lineage/constraint tests**

Replace `TestOpenAppliesMigrationsIdempotently` and `TestMediaEntityIDMigrationPreservesLegacyRows` with these tests. Keep `TestMediaEntityIDRoundTrips` and later current-behavior tests.

```go
func TestOpenAppliesBaselineIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	for run := 0; run < 2; run++ {
		store, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open() run %d error = %v", run, err)
		}
		var version string
		if err := store.db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != "001_baseline.sql" {
			t.Fatalf("migration version = %q, want 001_baseline.sql", version)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRejectsUnknownMigrationLineage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP); INSERT INTO schema_migrations(version) VALUES ('001_initial.sql')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), `unsupported database migration "001_initial.sql"; rebuild from an empty data directory`) {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestBaselineRequiresPositiveMediaEntityID(t *testing.T) {
	repo := openTestRepository(t)
	_, err := repo.store.db.Exec(`INSERT INTO media(instance, kind, entity_id, file_id, path, size, mod_time_ns, title, updated_at_ns) VALUES ('sonarr-main', 'episode', 0, 42, '/media/show.mkv', 100, 1, 'Show', 1)`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Fatalf("zero entity insert error = %v", err)
	}
}

func TestBaselineHasCompleteCurrentSurface(t *testing.T) {
	repo := openTestRepository(t)
	wantTables := []string{"instances", "media", "tracks", "search_states", "provider_states", "provider_cache", "pack_cache", "pack_members", "candidates", "installations", "notifications", "events", "media_hashes", "candidate_rejections"}
	for _, name := range wantTables {
		var count int
		if err := repo.store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count/error = %d/%v", name, count, err)
		}
	}
	wantColumns := map[string][]string{
		"media": {"entity_id", "episode_title", "unsupported_reason"},
		"search_states": {"priority", "rerun_requested"},
		"installations": {"media_path", "media_file_id", "media_size", "media_mod_time_ns"},
		"notifications": {"dedupe_key", "lease_owner", "lease_until_ns"},
		"events": {"event_id", "instance", "kind", "file_id", "entity_id"},
	}
	for table, columns := range wantColumns {
		rows, err := repo.store.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			got[name] = true
		}
		_ = rows.Close()
		for _, column := range columns {
			if !got[column] {
				t.Errorf("%s missing column %s", table, column)
			}
		}
	}
	wantIndexes := []string{"tracks_media_language_idx", "search_due_idx", "provider_cache_expiry_idx", "pack_lookup_idx", "pack_lru_idx", "notifications_dedupe_idx", "notifications_due_idx", "events_event_id_idx", "events_created_idx", "candidate_rejections_lookup_idx", "media_entity_identity_idx"}
	for _, name := range wantIndexes {
		var count int
		if err := repo.store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s count/error = %d/%v", name, count, err)
		}
	}
	wantForeignKeys := map[string]string{"tracks": "media", "search_states": "media", "pack_members": "pack_cache", "candidates": "media", "installations": "media", "events": "media", "media_hashes": "media", "candidate_rejections": "media"}
	for table, parent := range wantForeignKeys {
		rows, err := repo.store.db.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, seq int
			var target, from, to, onUpdate, onDelete, match string
			if err := rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				t.Fatal(err)
			}
			found = found || target == parent
		}
		_ = rows.Close()
		if !found {
			t.Errorf("%s missing foreign key to %s", table, parent)
		}
	}
}
```

Delete `TestMediaUnsupportedReasonMigration`, `TestSearchPriorityMigrationDefaults`, and `openDatabaseThroughMigration`; no clean-baseline test may manufacture an intermediate historical schema. Current unsupported-reason and priority behavior remains covered by their round-trip/ordering tests.

- [ ] **Step 2: Run the new tests and verify the red state**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/store -run 'Test(OpenAppliesBaselineIdempotently|OpenRejectsUnknownMigrationLineage|BaselineRequiresPositiveMediaEntityID)$' -count=1
```

Expected: FAIL because the embedded lineage still contains ten migrations, unknown applied names are accepted, and zero `media.entity_id` is allowed.

- [ ] **Step 3: Build the exact final baseline SQL**

Create `001_baseline.sql` from the current final schema. Preserve every table, foreign key, unique constraint, and index from migrations 001–010, but fold later additions into their owning `CREATE TABLE` statements:

```sql
-- media additions folded into CREATE TABLE media
entity_id INTEGER NOT NULL CHECK (entity_id > 0),
episode_title TEXT NOT NULL DEFAULT '',
unsupported_reason TEXT NOT NULL DEFAULT '',
UNIQUE(instance, kind, file_id)

-- search_states additions folded into CREATE TABLE search_states
priority INTEGER NOT NULL DEFAULT 200,
rerun_requested INTEGER NOT NULL DEFAULT 0 CHECK (rerun_requested IN (0, 1)),

-- installations additions folded into CREATE TABLE installations
media_path TEXT NOT NULL DEFAULT '',
media_file_id INTEGER NOT NULL DEFAULT 0,
media_size INTEGER NOT NULL DEFAULT 0,
media_mod_time_ns INTEGER NOT NULL DEFAULT 0,

-- notifications additions folded into CREATE TABLE notifications
dedupe_key TEXT,

-- events additions folded into CREATE TABLE events
event_id TEXT,
instance TEXT NOT NULL DEFAULT '',
kind TEXT NOT NULL DEFAULT '',
file_id INTEGER NOT NULL DEFAULT 0,
entity_id INTEGER NOT NULL DEFAULT 0,
```

Include the complete `media_hashes` and `candidate_rejections` table definitions unchanged. Define these final indexes exactly once:

```sql
CREATE INDEX tracks_media_language_idx ON tracks(media_id, language);
CREATE INDEX search_due_idx ON search_states(state, priority DESC, next_attempt_at_ns, lease_until_ns);
CREATE INDEX provider_cache_expiry_idx ON provider_cache(expires_at_ns);
CREATE INDEX pack_lookup_idx ON pack_cache(series_key, season, language, expires_at_ns);
CREATE INDEX pack_lru_idx ON pack_cache(expires_at_ns, last_access_at_ns);
CREATE UNIQUE INDEX notifications_dedupe_idx ON notifications(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX notifications_due_idx ON notifications(next_attempt_at_ns, lease_until_ns);
CREATE UNIQUE INDEX events_event_id_idx ON events(event_id) WHERE event_id IS NOT NULL;
CREATE INDEX events_created_idx ON events(created_at_ns);
CREATE INDEX candidate_rejections_lookup_idx ON candidate_rejections(media_id, language, provider_id, result_id, candidate_signature, tool_signature, expires_at_ns);
CREATE UNIQUE INDEX media_entity_identity_idx ON media(instance, kind, entity_id);
```

Delete the old ten files only after every statement has been accounted for in the baseline.

- [ ] **Step 4: Add explicit embedded-lineage validation**

In `Store.migrate`, read and sort embedded migration entries immediately after ensuring `schema_migrations`. Build the allowed set from non-directory `.sql` entries, then validate applied names before applying missing files:

```go
func validateAppliedMigrations(ctx context.Context, db *sql.DB, allowed map[string]struct{}) error {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return fmt.Errorf("scan applied migration: %w", err)
		}
		if _, ok := allowed[version]; !ok {
			return fmt.Errorf("unsupported database migration %q; rebuild from an empty data directory", version)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}
	return nil
}
```

Do not contact any external service or inspect media from this function.

- [ ] **Step 5: Run store tests and verify green**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/store -race -count=1
```

Expected: PASS with one baseline migration, explicit old-lineage rejection, and positive media identity enforced by SQLite.

- [ ] **Step 6: Commit the schema baseline**

```bash
git add internal/store/store.go internal/store/repository_test.go internal/store/migrations
git commit -m "store: reset SQLite schema baseline"
```

---

### Task 2: Remove zero-entity runtime compatibility

**Files:**
- Modify: `internal/store/repository.go:1215-1235,1370-1425`
- Modify: `internal/store/repository_test.go:90-205,1200-1335`
- Modify: `internal/catalog/webhook_test.go`
- Modify: `internal/catalog/reconcile_test.go`

**Interfaces:**
- Consumes: positive `domain.Media.EntityID`, `MediaEventMutation.EntityID`, and the strict baseline from Task 1.
- Produces: `applyMediaMutationTx` that requires explicit event entity identity; `findMediaIdentityTx` with entity/file conflict detection but no zero-ID adoption.

- [ ] **Step 1: Add a failing test for explicit mutation identity**

Add beside the existing media-event identity tests:

```go
func TestImportMutationRequiresExplicitEntityID(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	media.EntityID = 101
	_, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{
		EventID: "missing-explicit-entity",
		Type: "import",
		Ref: media.Ref,
		Media: media,
		Languages: []domain.Language{"hr"},
		At: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid import media event identity") {
		t.Fatalf("missing entity error = %v", err)
	}
}
```

Use `rg -n 'MediaEventMutation{' internal --glob='*.go'` and ensure every valid import/rename fixture explicitly sets `EntityID: media.EntityID` (or its concrete positive entity value); do not weaken validation to keep an incomplete fixture passing. Delete/reconciliation mutations continue using the positive history entity where available and may retain file ID zero.

- [ ] **Step 2: Run the focused test and verify red**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/store -run TestImportMutationRequiresExplicitEntityID -count=1
```

Expected: FAIL because `applyMediaMutationTx` currently copies `mutation.Media.EntityID` into an omitted mutation field.

- [ ] **Step 3: Remove fallback and adoption branches**

Delete this block from `applyMediaMutationTx`:

```go
if mutation.EntityID == 0 && (mutation.Type == "import" || mutation.Type == "rename") {
	mutation.EntityID = mutation.Media.EntityID
}
```

Delete `TestLegacyEntityAdoptionUpdatesExistingFileRow`. Simplify the selection in `findMediaIdentityTx` to:

```go
if entityFound && fileFound && entityRow.id != fileRow.id {
	return 0, "", 0, 0, 0, false, fmt.Errorf("conflicting media identities")
}
if entityFound {
	return entityRow.id, entityRow.path, entityRow.fileID, entityRow.size, entityRow.modTime, true, nil
}
if fileFound {
	return 0, "", 0, 0, 0, false, fmt.Errorf("conflicting media identities")
}
return 0, "", 0, 0, 0, false, nil
```

The file lookup remains mandatory: it rejects reuse of one physical Arr file identity by a different stable entity.

- [ ] **Step 4: Run identity/catalog tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/store ./internal/catalog -race -count=1
```

Expected: PASS. Entity replacement still updates one row; conflicting entity/file rows still fail closed; known and unknown entity deletes retain their audit behavior; webhook and reconciliation fixtures provide explicit positive identities.

- [ ] **Step 5: Commit strict identity behavior**

```bash
git add internal/store/repository.go internal/store/repository_test.go internal/catalog
git commit -m "store: remove legacy entity adoption"
```

---

### Task 3: Remove compatibility-only API and test vocabulary

**Files:**
- Modify: `internal/store/repository.go:588-590`
- Modify: `internal/store/repository_test.go`
- Modify: `internal/worker/worker_test.go`
- Modify: `internal/provider/coordinator_test.go:130-150,220-235`
- Modify: `internal/domain/candidate_test.go:8-25`

**Interfaces:**
- Consumes: `Repository.UpsertSearchStateWithPriority(context.Context, int64, domain.Language, time.Time, SearchPriority) error`.
- Produces: no default-priority wrapper; tests name current serialization behavior without a legacy promise.

- [ ] **Step 1: Replace every test-only wrapper call explicitly**

Change every test call of:

```go
repo.UpsertSearchState(ctx, mediaID, language, next)
```

to:

```go
repo.UpsertSearchStateWithPriority(ctx, mediaID, language, next, SearchPriorityMissing)
```

In `internal/worker/worker_test.go`, qualify the constant as `store.SearchPriorityMissing`.

- [ ] **Step 2: Remove the unused wrapper and prove compilation**

Delete `Repository.UpsertSearchState`. Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/store ./internal/worker -race -count=1
```

Expected: PASS; `rg -n 'UpsertSearchState\(' --glob='*.go'` returns no matches.

- [ ] **Step 3: Remove obsolete cache-lineage fixtures**

Delete `TestCoordinatorIgnoresLegacyNormalizedCandidateCache` and `legacyProviderCacheKey`. Remove now-unused `crypto/sha256`, `encoding/hex`, or `fmt` imports only if `goimports`/the compiler reports them unused. Do not change `normalizedCandidateCacheVersion`, the current cache key, or download-reference stripping.

Rename:

```go
func TestCandidateJSONPreservesLegacyShapeUnlessForced(t *testing.T)
```

to:

```go
func TestCandidateJSONOmitsFalseForcedFlag(t *testing.T)
```

Keep both assertions unchanged: false omits the field and true persists it.

- [ ] **Step 4: Run provider/domain tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./internal/provider ./internal/domain -race -count=1
```

Expected: PASS with current cache safety and candidate JSON behavior intact.

- [ ] **Step 5: Commit compatibility cleanup**

```bash
git add internal/store/repository.go internal/store/repository_test.go internal/worker/worker_test.go internal/provider/coordinator_test.go internal/domain/candidate_test.go
git commit -m "refactor: remove pre-release compatibility helpers"
```

---

### Task 4: Replace active legacy guidance with the clean-baseline contract

**Files:**
- Modify: `README.md:90-97`
- Modify: `docs/architecture.md:7-15`
- Modify: `docs/operations.md:165-185`
- Modify: `docs/implementation-status.md`
- Modify: `AGENTS.md:50-56`

**Interfaces:**
- Consumes: the strict lineage/error behavior and positive entity contract from Tasks 1–3.
- Produces: one operator/agent contract that old databases require a fresh data directory; historical specs/plans remain untouched.

- [ ] **Step 1: Update active documentation**

Use this exact substance in active docs, adapting only surrounding grammar:

```text
subsyncd currently supports only databases created from 001_baseline.sql. A database containing migration names from the pre-release 001_initial.sql–010_media_entity_ids.sql lineage is rejected explicitly. Stop the service, preserve the old data directory if rollback matters, and start with an empty data directory. The migration runner remains in place for migrations added after the baseline.

Every persisted media row has a positive stable Arr entity ID. Physical file ID remains separate and replaceable. Startup performs no Arr request or identity backfill.
```

Remove statements about zero-entity rows, lazy adoption, missed deletion before adoption, and intermediate migration upgrades. Do not alter the valid reconciliation backoff, overlap, idempotency, outside-scope, or offline-startup contracts.

- [ ] **Step 2: Record implementation status without rewriting history**

Add a new implementation-status section naming the new baseline, explicit lineage rejection, removed compatibility paths, verification commands, and the still-pending production clean cutover. Leave dated historical plans/specs unchanged.

- [ ] **Step 3: Validate active documentation and formatting**

Run:

```bash
rg -n 'legacy|lazy adoption|zero.entity|001_initial|010_media_entity_ids' README.md AGENTS.md docs/architecture.md docs/operations.md
git diff --check
```

Expected: only deliberate clean-baseline/unsupported-lineage wording remains; no active instruction says old rows are adopted or upgraded.

- [ ] **Step 4: Commit active documentation**

```bash
git add README.md AGENTS.md docs/architecture.md docs/operations.md docs/implementation-status.md
git commit -m "docs: define clean database baseline"
```

---

### Task 5: Run the complete verification gate and produce the immutable image

**Files:**
- Modify only if verification exposes a defect in files already scoped by Tasks 1–4.

**Interfaces:**
- Consumes: the complete clean-baseline implementation.
- Produces: a clean tested commit SHA in shell variable `code_sha` and native arm64 image `ghcr.io/tomislav/subsyncd:sha-$code_sha` suitable for direct production loading and GitHub publication.

- [ ] **Step 1: Format and inspect the complete patch**

Run:

```bash
gofmt -w internal/store/store.go internal/store/repository.go internal/store/repository_test.go internal/worker/worker_test.go internal/provider/coordinator_test.go internal/domain/candidate_test.go
git diff --check
git status --short
```

Expected: no formatting error, no unplanned path, and no leftover compatibility symbol in active code.

- [ ] **Step 2: Run the complete race-enabled suite**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./... -race -count=1
```

Expected: PASS with zero failed packages.

- [ ] **Step 3: Run vet and tagged end-to-end tests**

Run:

```bash
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go vet ./...
GOCACHE=/tmp/subsyncd-baseline-gocache GOMODCACHE=/tmp/subsyncd-baseline-gomodcache go test ./test/e2e -tags=e2e -race -count=1
```

Expected: both commands exit 0.

- [ ] **Step 4: Build and inspect a clean native arm64 image**

Record `code_sha=$(git rev-parse --short HEAD)`, then run:

```bash
docker build --no-cache --platform linux/arm64 --build-arg VERSION=sha-$code_sha -t ghcr.io/tomislav/subsyncd:sha-$code_sha .
docker run --rm ghcr.io/tomislav/subsyncd:sha-$code_sha version
docker image inspect ghcr.io/tomislav/subsyncd:sha-$code_sha --format '{{.Architecture}} {{.Config.User}} {{.Id}}'
```

Expected: reported version matching `sha-$code_sha`, architecture `arm64`, user `1000:1000`, and a concrete image ID.

- [ ] **Step 5: Push the verified code to main and confirm CI**

```bash
git push origin main
run_id=$(gh run list --branch main --limit 1 --json databaseId --jq '.[0].databaseId')
gh run watch "$run_id" --exit-status
```

Expected: GitHub Verify and multi-architecture publication jobs succeed. The direct production cutover may use the locally built image without waiting for publication, but final evidence must record the workflow result.

---

## Deployment follow-up

Host-specific cutover instructions and deployment records are maintained in the ignored local production runbook, outside public documentation.
