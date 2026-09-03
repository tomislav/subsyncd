package catalog

import (
	"context"
	"time"

	"subsyncd/internal/domain"
)

type Catalog interface {
	GetMedia(context.Context, domain.MediaRef) (domain.Media, error)
	ListMediaChangedSince(context.Context, time.Time) ([]domain.Media, error)
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
