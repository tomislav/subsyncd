package catalog

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

type arrHistoryRecord struct {
	ID            int64     `json:"id"`
	EventType     string    `json:"eventType"`
	Date          time.Time `json:"date"`
	EpisodeFileID int64     `json:"episodeFileId"`
	MovieFileID   int64     `json:"movieFileId"`
}

type historyKey struct {
	kind   domain.MediaKind
	fileID int64
}

func reduceHistory(instance string, kind domain.MediaKind, records []arrHistoryRecord, fileID func(arrHistoryRecord) int64, eventType func(string) (EventType, bool)) ([]HistoryChange, error) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Date.Equal(records[j].Date) {
			return records[i].ID < records[j].ID
		}
		return records[i].Date.Before(records[j].Date)
	})
	latest := make(map[historyKey]HistoryChange)
	for _, record := range records {
		typeName, relevant := eventType(record.EventType)
		if !relevant {
			continue
		}
		id := fileID(record)
		if record.ID <= 0 || id <= 0 || record.Date.IsZero() {
			return nil, fmt.Errorf("%s history record has incomplete identity", kind)
		}
		ref := domain.MediaRef{Instance: instance, Kind: kind, FileID: id}
		latest[historyKey{kind: kind, fileID: id}] = HistoryChange{HistoryID: record.ID, Type: typeName, Ref: ref, OccurredAt: record.Date.UTC()}
	}
	changes := make([]HistoryChange, 0, len(latest))
	for _, change := range latest {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		if !changes[i].OccurredAt.Equal(changes[j].OccurredAt) {
			return changes[i].OccurredAt.Before(changes[j].OccurredAt)
		}
		if changes[i].HistoryID != changes[j].HistoryID {
			return changes[i].HistoryID < changes[j].HistoryID
		}
		if changes[i].Ref.Kind != changes[j].Ref.Kind {
			return changes[i].Ref.Kind < changes[j].Ref.Kind
		}
		return changes[i].Ref.FileID < changes[j].Ref.FileID
	})
	return changes, nil
}

func sonarrHistoryEvent(raw string) (EventType, bool) {
	switch strings.ToLower(raw) {
	case "downloadfolderimported":
		return EventImport, true
	case "episodefilerenamed":
		return EventRename, true
	case "episodefiledeleted":
		return EventDelete, true
	default:
		return "", false
	}
}

func radarrHistoryEvent(raw string) (EventType, bool) {
	switch strings.ToLower(raw) {
	case "downloadfolderimported":
		return EventImport, true
	case "moviefilerenamed":
		return EventRename, true
	case "moviefiledeleted":
		return EventDelete, true
	default:
		return "", false
	}
}
