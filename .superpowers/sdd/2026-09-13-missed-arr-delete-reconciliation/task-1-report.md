# Task 1 Report: Complete Arr Identity Snapshot Contract

## Delivered behavior

- Added `catalog.CatalogIdentitySnapshot`, containing the media kind and a
  non-nil set of validated positive top-level Arr IDs.
- Added optional `catalog.IdentitySnapshotCatalog` with
  `ListIdentitySnapshot(context.Context)`.
- Sonarr enumerates `arrapi.Series`; Radarr enumerates `arrapi.Movies`.
  Both adapters reject nil/incomplete collections and non-positive IDs,
  collapse repeated positive IDs, preserve empty completed collections, honor
  cancellation, and wrap adapter failures through the existing bounded
  `safeArrAPIError` boundary.
- No reconciliation, storage, discovery, media, provider, filesystem, or
  network behavior changed.

## TDD evidence

The focused snapshot test was added before production code. Its RED run was:

```text
go test ./internal/catalog -run 'IdentitySnapshot' -count=1
# subsyncd/internal/catalog [subsyncd/internal/catalog.test]
internal/catalog/library_test.go:65:40: undefined: IdentitySnapshotCatalog
...
FAIL\tsubsyncd/internal/catalog [build failed]
```

After the minimal interface and adapter implementation, the required GREEN
command passed:

```text
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache \
  go test ./internal/catalog -run 'IdentitySnapshot' -race -count=1
ok  \tsubsyncd/internal/catalog
```

The focused tests cover complete positive enumeration and media kind,
deterministic duplicate collapse, valid empty snapshots, zero/negative-ID
rejection, and safe wrapping of adapter errors without retaining sensitive
upstream text.

## Verification

```text
GOCACHE=/tmp/subsyncd-gocache GOMODCACHE=/tmp/subsyncd-gomodcache \
  go test ./internal/catalog -race -count=1
ok  \tsubsyncd/internal/catalog

git diff --check
# no output
```

The full catalog package uses sanitized local fixtures. Its initial sandbox
run was unable to bind `httptest` loopback listeners; the approved rerun used
only those local listeners and made no external or production request.

## Next task

Task 2 can add the SQLite query for active tracked top-level identities and
consume this optional adapter capability in Task 3 reconciliation.
