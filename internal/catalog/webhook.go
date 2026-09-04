package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

var (
	ErrIgnoredEvent   = errors.New("ignored Arr event")
	ErrInvalidWebhook = errors.New("invalid Arr webhook")
)

type webhookPayload struct {
	EventType           string        `json:"eventType"`
	IsUpgrade           bool          `json:"isUpgrade"`
	MovieFile           webhookFile   `json:"movieFile"`
	EpisodeFile         webhookFile   `json:"episodeFile"`
	EpisodeFiles        []webhookFile `json:"episodeFiles"`
	RenamedEpisodeFiles []webhookFile `json:"renamedEpisodeFiles"`
}

type webhookFile struct {
	ID           int64  `json:"id"`
	Size         int64  `json:"size"`
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	PreviousPath string `json:"previousPath"`
	SceneName    string `json:"sceneName"`
	Quality      string `json:"quality"`
	ReleaseGroup string `json:"releaseGroup"`
}

func NormalizeWebhook(instance, instanceType string, body []byte) ([]WebhookEvent, error) {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode %s payload: %v", ErrInvalidWebhook, instanceType, err)
	}
	if strings.EqualFold(payload.EventType, "test") {
		return nil, ErrIgnoredEvent
	}

	kind, files, eventType, err := webhookFiles(instanceType, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWebhook, err)
	}
	events := make([]WebhookEvent, 0, len(files))
	for _, file := range files {
		if file.ID <= 0 {
			continue
		}
		remotePath := file.Path
		if remotePath == "" {
			remotePath = file.RelativePath
		}
		event := WebhookEvent{
			Type:             eventType,
			Ref:              domain.MediaRef{Instance: instance, Kind: kind, FileID: file.ID},
			RemotePath:       remotePath,
			PreviousPath:     file.PreviousPath,
			IsUpgrade:        payload.IsUpgrade,
			OriginalFilename: file.SceneName,
			Quality:          file.Quality,
			ReleaseGroup:     file.ReleaseGroup,
		}
		event.EventID = stableEventID(instance, payload.EventType, kind, file, payload.IsUpgrade)
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: %s webhook %q has no file identity", ErrInvalidWebhook, instanceType, payload.EventType)
	}
	return events, nil
}

func webhookFiles(instanceType string, payload webhookPayload) (domain.MediaKind, []webhookFile, EventType, error) {
	event := strings.ToLower(payload.EventType)
	switch strings.ToLower(instanceType) {
	case "sonarr":
		switch event {
		case "download":
			return domain.MediaEpisode, payload.EpisodeFiles, EventImport, nil
		case "rename":
			return domain.MediaEpisode, payload.RenamedEpisodeFiles, EventRename, nil
		case "episodefiledelete":
			return domain.MediaEpisode, []webhookFile{payload.EpisodeFile}, EventDelete, nil
		}
	case "radarr":
		switch event {
		case "download":
			return domain.MediaMovie, []webhookFile{payload.MovieFile}, EventImport, nil
		case "rename":
			return domain.MediaMovie, []webhookFile{payload.MovieFile}, EventRename, nil
		case "moviefiledelete":
			return domain.MediaMovie, []webhookFile{payload.MovieFile}, EventDelete, nil
		}
	default:
		return "", nil, "", fmt.Errorf("unknown Arr instance type %q", instanceType)
	}
	return "", nil, "", fmt.Errorf("unsupported %s webhook event %q", instanceType, payload.EventType)
}

func stableEventID(instance, eventType string, kind domain.MediaKind, file webhookFile, upgrade bool) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%t", instance, strings.ToLower(eventType), kind, file.ID, file.Size, file.Path, file.RelativePath, file.PreviousPath, file.SceneName, file.ReleaseGroup, upgrade)))
	return hex.EncodeToString(sum[:16])
}

type MediaEventStore interface {
	ApplyMediaEvent(context.Context, store.MediaEventMutation) (bool, error)
}

// WebhookHandler normalizes an Arr payload, hydrates current media metadata for
// imports and renames, and atomically persists each resulting state change.
type WebhookHandler struct {
	Instance     string
	InstanceType string
	Catalog      Catalog
	Store        MediaEventStore
	Languages    []domain.Language
	Now          func() time.Time
	OnApplied    func()
}

func (h WebhookHandler) Handle(ctx context.Context, body []byte) error {
	events, err := NormalizeWebhook(h.Instance, h.InstanceType, body)
	if err != nil {
		return err
	}
	appliedAny := false
	defer func() {
		if appliedAny && h.OnApplied != nil {
			h.OnApplied()
		}
	}()
	now := h.Now().UTC()
	for _, event := range events {
		mutation := store.MediaEventMutation{
			EventID:   event.EventID,
			Type:      string(event.Type),
			Ref:       event.Ref,
			Languages: h.Languages,
			At:        now,
		}
		if event.Type != EventDelete {
			media, err := h.Catalog.GetMedia(ctx, event.Ref)
			if err != nil {
				return fmt.Errorf("hydrate %s event %s: %w", h.Instance, event.EventID, err)
			}
			mutation.Media = media
		}
		applied, err := h.Store.ApplyMediaEvent(ctx, mutation)
		if err != nil {
			return fmt.Errorf("apply %s event %s: %w", h.Instance, event.EventID, err)
		}
		appliedAny = appliedAny || applied
	}
	return nil
}
