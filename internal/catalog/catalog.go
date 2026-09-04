package catalog

import (
	"context"
	"time"

	"subsyncd/internal/domain"
)

type Catalog interface {
	GetMedia(context.Context, domain.MediaRef) (domain.Media, error)
	ListChangesSince(context.Context, time.Time) ([]HistoryChange, error)
}

type HistoryChange struct {
	HistoryID  int64
	Type       EventType
	Ref        domain.MediaRef
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
