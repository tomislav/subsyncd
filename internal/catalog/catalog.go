package catalog

import (
	"context"
	"time"

	"subsyncd/internal/domain"
)

type Catalog interface {
	GetMedia(context.Context, domain.MediaRef) (domain.Media, error)
	ListChanges(context.Context, time.Time, time.Time) ([]HistoryChange, error)
}

// LibraryCatalog is an optional capability implemented by catalogs that can
// enumerate their complete current library independently of retained history.
type LibraryCatalog interface {
	ListLibrary(context.Context) ([]domain.Media, error)
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
