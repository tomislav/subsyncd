package catalog

import (
	"context"
	"errors"
	"time"

	"subsyncd/internal/domain"
)

// ErrHistoryDeferred means a history page contained one or more current Arr
// entities whose media could not yet be resolved. Nondelayed changes may be
// committed, but the cursor must remain unchanged so the page is retried.
var ErrHistoryDeferred = errors.New("reconciliation history deferred")

type Catalog interface {
	GetMedia(context.Context, domain.MediaRef) (domain.Media, error)
	ListChanges(context.Context, time.Time, time.Time) ([]HistoryChange, error)
}

// LibraryCatalog is an optional capability implemented by catalogs that can
// enumerate their complete current library independently of retained history.
type LibraryCatalog interface {
	ListLibrary(context.Context) ([]domain.Media, error)
}

// CatalogIdentitySnapshot is a complete, validated set of current top-level
// Arr identities for one media kind. IDs are always positive and duplicates
// are collapsed.
type CatalogIdentitySnapshot struct {
	Kind domain.MediaKind
	IDs  map[int64]struct{}
}

// IdentitySnapshotCatalog is an optional capability implemented by catalogs
// that can enumerate current top-level Arr identities independently of
// retained history.
type IdentitySnapshotCatalog interface {
	ListIdentitySnapshot(context.Context) (CatalogIdentitySnapshot, error)
}

// ReconciliationCatalog is the complete catalog contract required by normal
// and explicit reconciliation. Production reconcilers must never silently
// omit authoritative top-level identity recovery.
type ReconciliationCatalog interface {
	Catalog
	IdentitySnapshotCatalog
}

type HistoryState string

const (
	HistoryPresent      HistoryState = "present"
	HistoryAbsent       HistoryState = "absent"
	HistoryOutsideScope HistoryState = "outside_scope"
)

type HistoryChange struct {
	HistoryID  int64
	EntityID   int64
	Kind       domain.MediaKind
	Type       EventType
	State      HistoryState
	Media      domain.Media
	OccurredAt time.Time
}

type EventType string

const (
	EventImport EventType = "import"
	EventRename EventType = "rename"
	EventDelete EventType = "delete"
)

type WebhookEvent struct {
	SeriesID         int64
	EntityID         int64
	EventID          string
	Type             EventType
	Ref              domain.MediaRef
	RemotePath       string
	PreviousPath     string
	IsUpgrade        bool
	OriginalFilename string
	Quality          string
	ReleaseGroup     string
}
