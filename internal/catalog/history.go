package catalog

import (
	"fmt"
	"sort"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/domain"
)

func reduceHistoryByEntity(records []arrapi.HistoryRecord, kind domain.MediaKind, through time.Time, entityID func(arrapi.HistoryRecord) int64) ([]HistoryChange, error) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Date.Equal(records[j].Date) {
			return records[i].ID < records[j].ID
		}
		return records[i].Date.Before(records[j].Date)
	})
	latest := make(map[int64]HistoryChange)
	for _, record := range records {
		typeName, relevant := historyEvent(record.EventType)
		if !relevant {
			continue
		}
		if record.Date.IsZero() {
			return nil, fmt.Errorf("%s history record has incomplete identity", kind)
		}
		if record.Date.After(through) {
			continue
		}
		entity := entityID(record)
		if record.ID <= 0 || entity <= 0 {
			return nil, fmt.Errorf("%s history record has incomplete identity", kind)
		}
		latest[entity] = HistoryChange{
			HistoryID:  int64(record.ID),
			EntityID:   entity,
			Kind:       kind,
			Type:       typeName,
			OccurredAt: record.Date.UTC(),
		}
	}
	changes := make([]HistoryChange, 0, len(latest))
	for _, change := range latest {
		changes = append(changes, change)
	}
	sortHistoryChanges(changes)
	return changes, nil
}

func historyEvent(event arrapi.EventType) (EventType, bool) {
	switch event {
	case arrapi.EventDownloadImported:
		return EventImport, true
	case arrapi.EventFileRenamed:
		return EventRename, true
	case arrapi.EventFileDeleted:
		return EventDelete, true
	default:
		return "", false
	}
}

func sortHistoryChanges(changes []HistoryChange) {
	sort.Slice(changes, func(i, j int) bool {
		if !changes[i].OccurredAt.Equal(changes[j].OccurredAt) {
			return changes[i].OccurredAt.Before(changes[j].OccurredAt)
		}
		if changes[i].HistoryID != changes[j].HistoryID {
			return changes[i].HistoryID < changes[j].HistoryID
		}
		return changes[i].EntityID < changes[j].EntityID
	})
}
