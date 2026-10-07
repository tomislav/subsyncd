package catalog

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

// ErrHistoryDeferred means a history page contained one or more current Arr
// entities whose media could not yet be resolved. Nondelayed changes may be
// committed, but the cursor must remain unchanged so the page is retried.
var ErrHistoryDeferred = errors.New("reconciliation history deferred")

// HistoryDeferral identifies one current Arr entity whose media could not be
// read. Title is a log-safe display identity and may be empty.
type HistoryDeferral struct {
	Kind     domain.MediaKind
	EntityID int64
	Title    string
}

// DeferredHistoryError lists the entities one history read deferred. It
// matches ErrHistoryDeferred.
type DeferredHistoryError struct {
	Entities []HistoryDeferral
}

func (e *DeferredHistoryError) Error() string {
	parts := make([]string, 0, len(e.Entities))
	for _, entity := range e.Entities {
		parts = append(parts, entity.describe()+" has unavailable current media")
	}
	return ErrHistoryDeferred.Error() + ": " + strings.Join(parts, "; ")
}

func (e *DeferredHistoryError) Is(target error) bool {
	return target == ErrHistoryDeferred
}

// maximumLoggedDeferrals bounds the entity list carried in one log record.
const maximumLoggedDeferrals = 10

// DeferredMedia returns log-safe descriptions of at most ten deferred entities.
func (e *DeferredHistoryError) DeferredMedia() []string {
	shown := e.Entities[:min(len(e.Entities), maximumLoggedDeferrals)]
	described := make([]string, 0, len(shown))
	for _, entity := range shown {
		described = append(described, entity.describe())
	}
	return described
}

func (d HistoryDeferral) describe() string {
	label := "Sonarr episode"
	if d.Kind == domain.MediaMovie {
		label = "Radarr movie"
	}
	label += " " + strconv.FormatInt(d.EntityID, 10)
	if d.Title != "" {
		label += " (" + d.Title + ")"
	}
	return label
}

// addHistoryDeferral appends one entity, allocating the error on first use so
// a page without deferrals still returns a nil error.
func addHistoryDeferral(deferred *DeferredHistoryError, entity HistoryDeferral) *DeferredHistoryError {
	if deferred == nil {
		deferred = &DeferredHistoryError{}
	}
	deferred.Entities = append(deferred.Entities, entity)
	return deferred
}

// deferredHistoryResult keeps the error interface nil when nothing deferred.
func deferredHistoryResult(deferred *DeferredHistoryError) error {
	if deferred == nil {
		return nil
	}
	return deferred
}

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
