package catalog

import (
	"context"
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

// readHistoryWindow walks descending, bounded pages. Fractional cursors overlap
// the complete second; transaction event IDs make that overlap idempotent.
func readHistoryWindow(ctx context.Context, client arrHistoryClient, since, through time.Time) ([]arrapi.HistoryRecord, error) {
	const pageSize = 100
	lower := since.UTC().Truncate(time.Second)
	seen := make(map[int]time.Time)
	var records []arrapi.HistoryRecord
	var oldest time.Time
	received := 0
	previousTotal := 0
	for pageNumber := 1; ; pageNumber++ {
		page, err := client.History(ctx, arrapi.HistoryOptions{Page: pageNumber, PageSize: pageSize})
		if err != nil {
			return nil, err
		}
		if page.Page != pageNumber || page.TotalRecords < previousTotal || page.PageSize != pageSize || len(page.Records) > pageSize || page.TotalRecords < len(page.Records) {
			return nil, fmt.Errorf("invalid Arr history page")
		}
		previousTotal = page.TotalRecords
		if len(page.Records) == 0 {
			if received < page.TotalRecords {
				return nil, fmt.Errorf("incomplete Arr history page")
			}
			return records, nil
		}
		progress := false
		reachedLower := false
		for _, record := range page.Records {
			if record.ID <= 0 || record.Date.IsZero() {
				return nil, fmt.Errorf("incomplete Arr history identity")
			}
			if date, duplicate := seen[record.ID]; duplicate {
				if !date.Equal(record.Date) {
					return nil, fmt.Errorf("inconsistent Arr history identity")
				}
				continue
			}
			seen[record.ID] = record.Date
			if !oldest.IsZero() && record.Date.After(oldest) {
				return nil, fmt.Errorf("unordered Arr history page")
			}
			oldest = record.Date
			progress = true
			if !since.IsZero() && record.Date.Before(lower) {
				reachedLower = true
				continue
			}
			if record.Date.After(through) {
				continue
			}
			if _, relevant := historyEvent(record.EventType); relevant {
				records = append(records, record)
			}
		}
		if !progress {
			return nil, fmt.Errorf("non-progressing Arr history page")
		}
		received += len(page.Records)
		if reachedLower || page.TotalRecords > 0 && received >= page.TotalRecords {
			return records, nil
		}
	}
}
